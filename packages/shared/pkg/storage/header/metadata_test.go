package header

import (
	"testing"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chainedOriginal is the memfile header of a sandbox resumed from a snapshot
// that itself sits on two earlier builds: blocks 0-1 empty, 2-5 in baseID,
// 6-7 in olderID.
func chainedOriginal(t *testing.T, olderID uuid.UUID) *Header {
	t.Helper()

	h, err := NewHeader(&Metadata{
		Version:     NormalizeFixVersion,
		BlockSize:   blockSize,
		Size:        size,
		Generation:  2,
		BuildId:     baseID,
		BaseBuildId: olderID,
	}, []BuildMap{
		{Offset: 0, Length: 2 * blockSize, BuildId: uuid.Nil},
		{Offset: 2 * blockSize, Length: 4 * blockSize, BuildId: baseID, BuildStorageOffset: 0},
		{Offset: 6 * blockSize, Length: 2 * blockSize, BuildId: olderID, BuildStorageOffset: 4 * blockSize},
	})
	require.NoError(t, err)

	return h
}

func referencedBuilds(h *Header) map[uuid.UUID]struct{} {
	out := make(map[uuid.UUID]struct{})

	for _, m := range h.Mapping {
		if m.BuildId != uuid.Nil {
			out[m.BuildId] = struct{}{}
		}
	}

	return out
}

// The plain diff header of a pause keeps reading every untouched block from the
// builds the sandbox was resumed from: that is the chain this change exists to
// cut, so the baseline is pinned here.
func TestToDiffHeader_DiffReferencesAncestors(t *testing.T) {
	t.Parallel()

	olderID := uuid.New()
	newID := uuid.New()

	d := NewDiffMetadata(int64(blockSize), roaring.BitmapOf(3, 6))

	h, err := d.ToDiffHeader(t.Context(), chainedOriginal(t, olderID), newID)
	require.NoError(t, err)

	assert.Equal(t, map[uuid.UUID]struct{}{newID: {}, baseID: {}, olderID: {}}, referencedBuilds(h))
}

func TestSelfContained_HeaderReferencesOnlyItself(t *testing.T) {
	t.Parallel()

	olderID := uuid.New()
	newID := uuid.New()
	original := chainedOriginal(t, olderID)

	d := NewDiffMetadata(int64(blockSize), roaring.BitmapOf(3, 6))

	inherited, err := d.Inherited(original)
	require.NoError(t, err)
	assert.Equal(t, []uint32{2, 4, 5, 7}, inherited.ToArray())

	sc, err := d.SelfContained(original)
	require.NoError(t, err)

	assert.Equal(t, []uint32{2, 3, 4, 5, 6, 7}, sc.Dirty.ToArray())
	assert.Equal(t, []uint32{0, 1}, sc.Empty.ToArray())

	h, err := sc.ToDiffHeader(t.Context(), original, newID)
	require.NoError(t, err)
	require.NoError(t, ValidateMappings(h.Mapping, h.Metadata.Size, h.Metadata.BlockSize))

	assert.Equal(t, map[uuid.UUID]struct{}{newID: {}}, referencedBuilds(h))

	// The data file is packed in block order: block 2 is the file's first
	// block, block 7 its sixth.
	assert.Equal(t, []BuildMap{
		{Offset: 0, Length: 2 * blockSize, BuildId: uuid.Nil},
		{Offset: 2 * blockSize, Length: 6 * blockSize, BuildId: newID, BuildStorageOffset: 0},
	}, h.Mapping)

	// The caller's own metadata is left as it was.
	assert.Equal(t, []uint32{3, 6}, d.Dirty.ToArray())
	assert.True(t, d.Empty.IsEmpty())
}

// A block the guest left zero (NoopMemory reports it both resident and empty)
// stays empty rather than becoming data, and an inherited block is never
// re-read when the diff says it is empty.
func TestSelfContained_EmptyBlocksStayEmpty(t *testing.T) {
	t.Parallel()

	olderID := uuid.New()
	newID := uuid.New()
	original := chainedOriginal(t, olderID)

	d := &DiffMetadata{
		Dirty:     roaring.BitmapOf(0, 3),
		Empty:     roaring.BitmapOf(3, 4),
		BlockSize: int64(blockSize),
	}

	sc, err := d.SelfContained(original)
	require.NoError(t, err)

	assert.Equal(t, []uint32{0, 2, 5, 6, 7}, sc.Dirty.ToArray())
	assert.Equal(t, []uint32{1, 3, 4}, sc.Empty.ToArray())

	h, err := sc.ToDiffHeader(t.Context(), original, newID)
	require.NoError(t, err)
	require.NoError(t, ValidateMappings(h.Mapping, h.Metadata.Size, h.Metadata.BlockSize))
	assert.Equal(t, map[uuid.UUID]struct{}{newID: {}}, referencedBuilds(h))
}

// A sandbox that wrote nothing still gets a memfile of its own.
func TestSelfContained_NothingDirty(t *testing.T) {
	t.Parallel()

	original := chainedOriginal(t, uuid.New())
	newID := uuid.New()

	sc, err := NewDiffMetadata(int64(blockSize), roaring.New()).SelfContained(original)
	require.NoError(t, err)

	assert.Equal(t, []uint32{2, 3, 4, 5, 6, 7}, sc.Dirty.ToArray())

	h, err := sc.ToDiffHeader(t.Context(), original, newID)
	require.NoError(t, err)
	assert.Equal(t, map[uuid.UUID]struct{}{newID: {}}, referencedBuilds(h))
}

func TestSelfContained_BlockSizeMismatch(t *testing.T) {
	t.Parallel()

	_, err := NewDiffMetadata(PageSize, roaring.BitmapOf(1)).SelfContained(chainedOriginal(t, uuid.New()))
	require.ErrorContains(t, err, "block size mismatch")
}
