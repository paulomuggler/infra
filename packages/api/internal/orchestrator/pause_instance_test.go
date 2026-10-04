package orchestrator

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/api/internal/orchestrator/nodemanager"
	"github.com/e2b-dev/infra/packages/api/internal/sandbox"
	"github.com/e2b-dev/infra/packages/db/pkg/testutils"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

type pauseSandboxClient struct {
	orchestrator.SandboxServiceClient

	err error
}

func (c *pauseSandboxClient) Pause(_ context.Context, _ *orchestrator.SandboxPauseRequest, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	if c.err != nil {
		return nil, c.err
	}

	return &emptypb.Empty{}, nil
}

type countingInvalidator struct {
	calls atomic.Int32
}

func (c *countingInvalidator) Invalidate(context.Context, string) {
	c.calls.Add(1)
}

func newPauseTestOrchestrator(t *testing.T, db *testutils.Database, pauseErr error) (*Orchestrator, *nodemanager.Node, *countingInvalidator) {
	t.Helper()

	sem, err := utils.NewAdjustableSemaphore(1)
	require.NoError(t, err)

	invalidator := &countingInvalidator{}
	node := nodemanager.NewTestNode("node-1", api.NodeStatusReady, 0, 8)
	node.SetSandboxClient(&pauseSandboxClient{err: pauseErr})

	return &Orchestrator{
		sqlcDB:            db.SqlcClient,
		snapshotCache:     invalidator,
		snapshotUpsertSem: sem,
	}, node, invalidator
}

func pauseTestSandbox(t *testing.T, db *testutils.Database) sandbox.Sandbox {
	t.Helper()

	teamID := testutils.CreateTestTeam(t, db)

	return sandbox.Sandbox{
		SandboxID:          "sbx-" + uuid.NewString()[:8],
		TeamID:             teamID,
		BaseTemplateID:     testutils.CreateTestTemplate(t, db, teamID),
		StartTime:          time.Now(),
		VCpu:               2,
		RamMB:              512,
		TotalDiskSizeMB:    1024,
		KernelVersion:      "6.1.0",
		FirecrackerVersion: "1.4.0",
		EnvdVersion:        "0.1.0",
	}
}

// A pause that fails must leave its build 'failed', never 'snapshotting': the
// orchestrator stopped the VM anyway, and resume must refuse that build rather
// than fall back to an older one.
func TestPauseSandbox_FailedPauseMarksBuildFailed(t *testing.T) {
	t.Parallel()

	for name, pauseErr := range map[string]error{
		"deadline":        status.Error(codes.DeadlineExceeded, "context deadline exceeded"),
		"queue exhausted": status.Error(codes.ResourceExhausted, "pause queue full"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			db := testutils.SetupDatabase(t)
			o, node, invalidator := newPauseTestOrchestrator(t, db, pauseErr)
			sbx := pauseTestSandbox(t, db)

			err := o.pauseSandbox(t.Context(), node, sbx)
			require.Error(t, err)

			last, err := db.SqlcClient.GetLastSnapshot(t.Context(), sbx.SandboxID)
			require.NoError(t, err)
			assert.Equal(t, "failed", string(last.EnvBuild.Status))
			assert.NotNil(t, last.EnvBuild.FinishedAt)
			assert.NotEmpty(t, last.EnvBuild.Reason.Message)
			assert.EqualValues(t, 1, invalidator.calls.Load(), "the cached last snapshot is stale after a failed pause too")
		})
	}
}

type cancellingPauseClient struct {
	orchestrator.SandboxServiceClient

	cancel context.CancelFunc
}

// Pause fails the way an expiring pause deadline does: by the time the RPC
// returns, the caller's ctx is done.
func (c *cancellingPauseClient) Pause(_ context.Context, _ *orchestrator.SandboxPauseRequest, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	c.cancel()

	return nil, status.Error(codes.DeadlineExceeded, "context deadline exceeded")
}

// The failed marking must still land when the pause used up its ctx.
func TestPauseSandbox_FailedPauseMarksBuildFailedPastDeadline(t *testing.T) {
	t.Parallel()

	db := testutils.SetupDatabase(t)
	o, node, _ := newPauseTestOrchestrator(t, db, nil)
	sbx := pauseTestSandbox(t, db)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	node.SetSandboxClient(&cancellingPauseClient{cancel: cancel})

	require.Error(t, o.pauseSandbox(ctx, node, sbx))

	last, err := db.SqlcClient.GetLastSnapshot(t.Context(), sbx.SandboxID)
	require.NoError(t, err)
	assert.Equal(t, "failed", string(last.EnvBuild.Status))
}

func TestPauseSandbox_SuccessMarksBuildSuccess(t *testing.T) {
	t.Parallel()

	db := testutils.SetupDatabase(t)
	o, node, invalidator := newPauseTestOrchestrator(t, db, nil)
	sbx := pauseTestSandbox(t, db)

	require.NoError(t, o.pauseSandbox(t.Context(), node, sbx))

	last, err := db.SqlcClient.GetLastSnapshot(t.Context(), sbx.SandboxID)
	require.NoError(t, err)
	assert.Equal(t, "success", string(last.EnvBuild.Status))
	assert.EqualValues(t, 1, invalidator.calls.Load())
}
