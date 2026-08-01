package template_manager

import (
	"context"
	"fmt"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/api/internal/utils"
	sqlcdb "github.com/e2b-dev/infra/packages/db/client"
	templatemanagergrpc "github.com/e2b-dev/infra/packages/shared/pkg/grpc/template-manager"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

// storageGCTimeout bounds one collection pass. A pass renames its collect set
// aside under the builder's build lock and removes it afterwards, so the slow
// part is the removal, not anything a build waits on.
const storageGCTimeout = 30 * time.Minute

// StorageGCRoots reads the build IDs the registry considers live.
//
// This is the only implementation of the root set. The reactive trigger inside
// this service and the periodic sweep binary both call it; two root-set
// implementations that could drift apart is the shape of the bug that took this
// host down on 2026-08-01.
func StorageGCRoots(ctx context.Context, db *sqlcdb.Client) ([]string, error) {
	rootIDs, err := db.GetStorageGCRoots(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read storage GC roots: %w", err)
	}

	if len(rootIDs) == 0 {
		// Not merely unusual — a registry with no live builds at all is
		// indistinguishable from a query that failed to see them, and the
		// builder refuses an empty root set for exactly that reason. Fail here
		// too, with the clearer message.
		return nil, fmt.Errorf("registry reported no live builds; refusing to run storage GC")
	}

	roots := make([]string, 0, len(rootIDs))
	for _, id := range rootIDs {
		roots = append(roots, id.String())
	}

	return roots, nil
}

// CollectStorageWithClient runs one collection pass against an already-dialled
// template service. Shared by this service's reactive trigger and the sweep
// binary, which reaches the builder directly rather than through a cluster
// pool.
func CollectStorageWithClient(
	ctx context.Context,
	db *sqlcdb.Client,
	client templatemanagergrpc.TemplateServiceClient,
	reason string,
	dryRun bool,
	minAgeSeconds *uint64,
) (*templatemanagergrpc.TemplateStorageCollectResponse, error) {
	roots, err := StorageGCRoots(ctx, db)
	if err != nil {
		return nil, err
	}

	res, err := client.TemplateStorageCollect(ctx, &templatemanagergrpc.TemplateStorageCollectRequest{
		RootBuildIDs:  roots,
		DryRun:        dryRun,
		Reason:        &reason,
		MinAgeSeconds: minAgeSeconds,
	})

	err = utils.UnwrapGRPCError(err)
	if err != nil {
		return nil, fmt.Errorf("template storage collect failed: %w", err)
	}

	return res, nil
}

// CollectStorage runs one template-storage collection pass on a build node.
func (tm *TemplateManager) CollectStorage(
	ctx context.Context,
	clusterID uuid.UUID,
	nodeID string,
	reason string,
	dryRun bool,
) (*templatemanagergrpc.TemplateStorageCollectResponse, error) {
	ctx, span := tracer.Start(ctx, "collect-template-storage")
	defer span.End()

	client, err := tm.GetClusterBuildClient(clusterID, nodeID)
	if err != nil {
		return nil, fmt.Errorf("failed to get builder client: %w", err)
	}

	res, err := CollectStorageWithClient(ctx, tm.sqlcDB, client.Template, reason, dryRun, nil)
	if err != nil {
		return nil, fmt.Errorf("on node '%s': %w", nodeID, err)
	}

	return res, nil
}

// collectStorageAfterBuild is the reactive trigger: a finished build is the
// moment its predecessor stops being reachable, so it is the moment its
// exclusive layers become collectable. Detached and best-effort — a build that
// succeeded must not be reported as failed because the collection after it
// could not run.
func (tm *TemplateManager) collectStorageAfterBuild(
	ctx context.Context,
	buildID uuid.UUID,
	clusterID uuid.UUID,
	nodeID string,
) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storageGCTimeout)
	defer cancel()

	res, err := tm.CollectStorage(ctx, clusterID, nodeID, "supersede:"+buildID.String(), false)
	if err != nil {
		logger.L().Warn(ctx, "Template storage GC after build failed",
			zap.Error(err), logger.WithBuildID(buildID.String()))

		return
	}

	logger.L().Info(ctx, "Template storage GC after build",
		logger.WithBuildID(buildID.String()),
		zap.String("gcRunID", res.GetRunID()),
		zap.Uint64("collectedDirs", res.GetCollectedDirs()),
		zap.String("freed", humanize.IBytes(res.GetFreedBytes())),
		zap.Uint64("keptDirs", res.GetKeptDirs()),
		zap.Uint64("prunedIndexBlobs", res.GetPrunedIndexBlobs()),
		zap.Int("brokenRoots", len(res.GetBrokenRoots())),
	)
}
