package server

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/gc"
	templatemanager "github.com/e2b-dev/infra/packages/shared/pkg/grpc/template-manager"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
)

// TemplateStorageCollect reclaims template-storage build directories that are
// no longer reachable from any live root.
//
// The caller supplies the registry's roots — it is the only side that can see
// Postgres. This side unions its own live state on top (running sandboxes,
// cached templates, in-flight builds) before anything is deleted, so the
// safety property does not rest on the caller having got its query right.
func (s *ServerStore) TemplateStorageCollect(
	ctx context.Context,
	in *templatemanager.TemplateStorageCollectRequest,
) (*templatemanager.TemplateStorageCollectResponse, error) {
	ctx, childSpan := tracer.Start(ctx, "template-storage-collect", trace.WithAttributes())
	defer childSpan.End()

	// The store has to be enumerable and directly removable, which the
	// StorageProvider interface cannot express — it has no list operation, and
	// object stores reclaim through bucket lifecycle rules instead. Collecting
	// a cloud bucket is upstream's ENG-3477 to design; this is the local one.
	if !storage.IsLocal() {
		return nil, status.Error(codes.FailedPrecondition,
			"template storage GC requires STORAGE_PROVIDER=Local")
	}

	roots := in.GetRootBuildIDs()
	if len(roots) == 0 {
		return nil, status.Error(codes.InvalidArgument, gc.ErrNoRoots.Error())
	}

	minAge := s.builderConfig.TemplateGCMinAge
	if in.MinAgeSeconds != nil {
		minAge = time.Duration(in.GetMinAgeSeconds()) * time.Second
	}

	cfg := gc.Config{
		TemplateStorageDir: storage.TemplateStorageConfig.GetLocalBasePath(),
		BuildCacheDir:      storage.BuildCacheStorageConfig.GetLocalBasePath(),
		LedgerDir:          s.builderConfig.OrchestratorBaseDir,
		MinAge:             minAge,
		DryRun:             in.GetDryRun(),
		Reason:             in.GetReason(),
	}

	roots = append(roots, s.liveRoots()...)

	// The write side of the lock builds hold for read: no build, upload or
	// explicit build delete runs while a pass is deciding and committing. The
	// commit is a rename per directory and takes about a second on a
	// thousand-directory store; the removal of the staged bytes happens after
	// the lock is back down, where it blocks nothing.
	s.buildLock.Lock()
	res, err := gc.Collect(ctx, cfg, roots)
	s.buildLock.Unlock()

	if err != nil {
		if errors.Is(err, gc.ErrNoRoots) {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}

		s.logger.Error(ctx, "template storage GC failed", zap.Error(err), zap.String("reason", cfg.Reason))

		return nil, status.Errorf(codes.Internal, "template storage GC failed: %s", err)
	}

	if err := gc.PurgeTrash(cfg.TemplateStorageDir); err != nil {
		// The pass itself succeeded and the store is already consistent; the
		// space is simply still held. The next pass clears it.
		s.logger.Warn(ctx, "template storage GC could not remove staged directories",
			zap.Error(err), zap.String("runID", res.RunID))
	}

	s.logger.Info(ctx, "Template storage GC",
		zap.String("runID", res.RunID),
		zap.String("reason", res.Reason),
		zap.Bool("dryRun", res.DryRun),
		zap.Int("scannedDirs", res.ScannedDirs),
		zap.Int("keptDirs", res.KeptDirs),
		zap.Int("collectedDirs", res.CollectedDirs()),
		zap.Uint64("freedBytes", res.FreedBytes),
		zap.Int("skippedRecentDirs", res.SkippedRecentDirs),
		zap.Int("prunedIndexBlobs", res.PrunedIndexBlobs),
		zap.Int("danglingRefs", res.DanglingRefs),
		zap.Int("brokenRoots", len(res.BrokenRoots)),
		zap.Int("missingRoots", len(res.MissingRoots)),
		zap.String("ledger", res.LedgerPath),
	)

	return &templatemanager.TemplateStorageCollectResponse{
		RunID:             res.RunID,
		ScannedDirs:       uint64(res.ScannedDirs),
		RootBuilds:        uint64(res.Roots),
		KeptDirs:          uint64(res.KeptDirs),
		CollectedDirs:     uint64(res.CollectedDirs()),
		FreedBytes:        res.FreedBytes,
		SkippedRecentDirs: uint64(res.SkippedRecentDirs),
		DanglingRefs:      uint64(res.DanglingRefs),
		PrunedIndexBlobs:  uint64(res.PrunedIndexBlobs),
		MissingRoots:      res.MissingRoots,
		BrokenRoots:       res.BrokenRoots,
		LedgerPath:        res.LedgerPath,
	}, nil
}

// liveRoots are the builds this orchestrator knows are in use right now,
// independent of anything the registry says.
//
//   - Every sandbox in the map, whatever its status: a sandbox pages its rootfs
//     and memfile lazily from its build's layers for its whole life, so a
//     supersede that happens mid-run must not take those layers away.
//   - Every template held in the sandbox template cache (25 h TTL) — a template
//     opened for a spawn.
//   - Every build the template build cache still reports as building.
func (s *ServerStore) liveRoots() []string {
	var roots []string

	if s.sandboxes != nil {
		roots = append(roots, s.sandboxes.BuildIDs()...)
	}

	if s.templateCache != nil {
		for buildID := range s.templateCache.Items() {
			roots = append(roots, buildID)
		}
	}

	roots = append(roots, s.buildCache.RunningBuildIDs()...)

	return roots
}
