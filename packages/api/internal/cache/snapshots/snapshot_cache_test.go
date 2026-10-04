package snapshotcache

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/db/pkg/types"
	"github.com/e2b-dev/infra/packages/db/queries"
)

func TestSnapshotInfo_Resumable(t *testing.T) {
	t.Parallel()

	info := func(status types.BuildStatus, group types.BuildStatusGroup) *SnapshotInfo {
		return &SnapshotInfo{
			Snapshot: queries.Snapshot{SandboxID: "sbx-1"},
			EnvBuild: queries.EnvBuild{ID: uuid.New(), Status: status, StatusGroup: group},
		}
	}

	require.NoError(t, info(types.BuildStatusSuccess, types.BuildStatusGroupReady).Resumable())

	for _, unready := range []*SnapshotInfo{
		info(types.BuildStatusFailed, types.BuildStatusGroupFailed),
		info(types.BuildStatusSnapshotting, types.BuildStatusGroupInProgress),
	} {
		err := unready.Resumable()
		require.ErrorIs(t, err, ErrSnapshotNotReady)
		assert.Contains(t, err.Error(), "sbx-1")
		assert.Contains(t, err.Error(), unready.EnvBuild.ID.String())
	}
}
