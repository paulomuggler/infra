package header

import (
	"context"
	"fmt"
	"io"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
)

var ignoreBuildID = uuid.Nil

type DiffMetadata struct {
	Dirty *roaring.Bitmap
	Empty *roaring.Bitmap

	BlockSize int64
}

func NewDiffMetadata(blockSize int64, dirty *roaring.Bitmap) *DiffMetadata {
	return &DiffMetadata{
		Dirty:     dirty,
		Empty:     roaring.New(),
		BlockSize: blockSize,
	}
}

func (d *DiffMetadata) toDiffMapping(
	ctx context.Context,
	buildID uuid.UUID,
) (mapping []BuildMap) {
	dirtyMappings := CreateMapping(
		&buildID,
		d.Dirty,
		d.BlockSize,
	)
	telemetry.ReportEvent(ctx, "created dirty mapping")

	emptyMappings := CreateMapping(
		// This buildID is intentionally ignored for nil blocks
		&ignoreBuildID,
		d.Empty,
		d.BlockSize,
	)
	telemetry.ReportEvent(ctx, "created empty mapping")

	mappings := MergeMappings(dirtyMappings, emptyMappings)
	telemetry.ReportEvent(ctx, "merge mappings")

	return mappings
}

func (d *DiffMetadata) ToDiffHeader(
	ctx context.Context,
	originalHeader *Header,
	buildID uuid.UUID,
) (h *Header, e error) {
	ctx, span := tracer.Start(ctx, "to diff-header")
	defer span.End()
	defer func() {
		if e != nil {
			span.RecordError(e)
			span.SetStatus(codes.Error, e.Error())
		}
	}()

	diffMapping := d.toDiffMapping(ctx, buildID)

	m := MergeMappings(
		originalHeader.Mapping,
		diffMapping,
	)
	telemetry.ReportEvent(ctx, "merged mappings")

	// TODO: We can run normalization only when empty mappings are not empty for this snapshot
	m = NormalizeMappings(m)
	telemetry.ReportEvent(ctx, "normalized mappings")

	metadata := originalHeader.Metadata.NextGeneration(buildID)

	telemetry.SetAttributes(ctx,
		attribute.Int64("snapshot.header.mappings.length", int64(len(m))),
		attribute.Int64("snapshot.diff.size", int64(d.Dirty.GetCardinality())*int64(originalHeader.Metadata.BlockSize)),
		attribute.Int64("snapshot.mapped_size", int64(metadata.Size)),
		attribute.Int64("snapshot.block_size", int64(metadata.BlockSize)),
		attribute.Int64("snapshot.metadata.version", int64(metadata.Version)),
		attribute.Int64("snapshot.metadata.generation", int64(metadata.Generation)),
		attribute.String("snapshot.metadata.build_id", metadata.BuildId.String()),
		attribute.String("snapshot.metadata.base_build_id", metadata.BaseBuildId.String()),
	)

	header, err := NewHeader(metadata, m)
	if err != nil {
		return nil, fmt.Errorf("failed to create header: %w", err)
	}

	err = ValidateMappings(header.Mapping, header.Metadata.Size, header.Metadata.BlockSize)
	if err != nil {
		if header.IsNormalizeFixApplied() {
			return nil, fmt.Errorf("invalid header mappings: %w", err)
		}

		logger.L().Warn(ctx, "header mappings are invalid, but normalize fix is not applied", zap.Error(err), logger.WithBuildID(header.Metadata.BuildId.String()))
	}

	return header, nil
}

// SelfContained widens d so that the header it builds over original names no
// build but the new one.
//
// A diff header maps every block d leaves untouched to whatever original maps
// it to, so a chain of pauses keeps every earlier snapshot's memfile alive for
// as long as any of its blocks is still unwritten — and with 2 MiB hugepages a
// handful of blocks per ancestor is enough to pin a whole guest-sized file. The
// widened metadata takes those inherited blocks into the diff too: every block
// original maps to a build becomes data of the new build, unless d already
// says it is empty. Everything else is empty.
//
// The returned Dirty is the full data set, in the block order the memfile has
// to be written in. Which of those blocks come from the guest and which are
// carried over from original is the caller's to tell apart: d.Dirty minus
// d.Empty is the guest's part, Inherited is the rest.
func (d *DiffMetadata) SelfContained(original *Header) (*DiffMetadata, error) {
	inherited, err := d.Inherited(original)
	if err != nil {
		return nil, err
	}

	data := d.Dirty.Clone()
	data.AndNot(d.Empty)
	data.Or(inherited)

	total := uint64(TotalBlocks(int64(original.Metadata.Size), d.BlockSize))

	empty := roaring.Flip(data, 0, total)
	empty.RemoveRange(total, uint64(1)<<32)

	return &DiffMetadata{
		Dirty:     data,
		Empty:     empty,
		BlockSize: d.BlockSize,
	}, nil
}

// Inherited returns the blocks a diff header built from d over original would
// keep reading from original's builds: mapped to a build (not to the empty
// sentinel) in original, and neither dirty nor empty in d.
func (d *DiffMetadata) Inherited(original *Header) (*roaring.Bitmap, error) {
	if int64(original.Metadata.BlockSize) != d.BlockSize {
		return nil, fmt.Errorf("block size mismatch: original header has %d, diff has %d", original.Metadata.BlockSize, d.BlockSize)
	}

	inherited := roaring.New()

	for _, m := range original.Mapping {
		if m.BuildId == uuid.Nil || m.Length == 0 {
			continue
		}

		if m.Offset%uint64(d.BlockSize) != 0 || m.Length%uint64(d.BlockSize) != 0 {
			return nil, fmt.Errorf("mapping at offset %d (length %d) is not aligned to block size %d", m.Offset, m.Length, d.BlockSize)
		}

		inherited.AddRange(
			uint64(BlockIdx(int64(m.Offset), d.BlockSize)),
			uint64(BlockIdx(int64(m.Offset+m.Length), d.BlockSize)),
		)
	}

	inherited.AndNot(d.Dirty)
	inherited.AndNot(d.Empty)

	return inherited, nil
}

type DiffMetadataBuilder struct {
	dirty *roaring.Bitmap
	empty *roaring.Bitmap

	blockSize int64
}

func NewDiffMetadataBuilder(blockSize int64) *DiffMetadataBuilder {
	return &DiffMetadataBuilder{
		dirty: roaring.New(),
		empty: roaring.New(),

		blockSize: blockSize,
	}
}

func (b *DiffMetadataBuilder) Process(ctx context.Context, block []byte, out io.Writer, offset int64) error {
	blockIdx := BlockIdx(offset, b.blockSize)

	isEmpty, err := IsEmptyBlock(block, b.blockSize)
	if err != nil {
		return fmt.Errorf("error checking empty block: %w", err)
	}
	if isEmpty {
		b.empty.Add(uint32(blockIdx))

		return nil
	}

	b.dirty.Add(uint32(blockIdx))
	n, err := out.Write(block)
	if err != nil {
		logger.L().Error(ctx, "error writing to out", zap.Error(err))

		return err
	}

	if int64(n) != b.blockSize {
		return fmt.Errorf("short write: %d != %d", int64(n), b.blockSize)
	}

	return nil
}

func (b *DiffMetadataBuilder) Build() *DiffMetadata {
	return &DiffMetadata{
		Dirty:     b.dirty,
		Empty:     b.empty,
		BlockSize: b.blockSize,
	}
}
