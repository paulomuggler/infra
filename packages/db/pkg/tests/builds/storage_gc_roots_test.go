package builds

import (
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/db/pkg/testutils"
	"github.com/e2b-dev/infra/packages/db/pkg/types"
)

func storageGCRoots(t *testing.T, db *testutils.Database) []uuid.UUID {
	t.Helper()

	roots, err := db.SqlcClient.GetStorageGCRoots(t.Context())
	require.NoError(t, err)

	return roots
}

func ageBuild(t *testing.T, db *testutils.Database, buildID uuid.UUID, age string) {
	t.Helper()

	err := db.SqlcClient.TestsRawSQL(t.Context(),
		`UPDATE public.env_builds SET created_at = now() - $2::interval WHERE id = $1`,
		buildID, age,
	)
	require.NoError(t, err)
}

// An in-flight build that only its own env references (no ready assignment,
// no paused sandbox) is kept by clause 4 alone.
func TestGetStorageGCRoots_FreshPauseBuildIsInFlight(t *testing.T) {
	t.Parallel()

	db := testutils.SetupDatabase(t)
	teamID := testutils.CreateTestTeam(t, db)
	envID := testutils.CreateTestTemplate(t, db, teamID)

	buildID := testutils.CreateTestBuild(t, t.Context(), db, envID, "snapshotting")
	testutils.CreateTestBuildAssignment(t, t.Context(), db, envID, buildID, "default")

	assert.True(t, slices.Contains(storageGCRoots(t, db), buildID))
}

// A pause build still 'snapshotting' past the API's pause timeout plus margin
// was abandoned; it must not stay a root forever.
func TestGetStorageGCRoots_StalePauseBuildIsNotInFlight(t *testing.T) {
	t.Parallel()

	db := testutils.SetupDatabase(t)
	teamID := testutils.CreateTestTeam(t, db)
	envID := testutils.CreateTestTemplate(t, db, teamID)

	buildID := testutils.CreateTestBuild(t, t.Context(), db, envID, "snapshotting")
	testutils.CreateTestBuildAssignment(t, t.Context(), db, envID, buildID, "default")
	ageBuild(t, db, buildID, "21 minutes")

	assert.False(t, slices.Contains(storageGCRoots(t, db), buildID))
}

// A pause build whose env is gone (the sandbox was killed, its assignments
// cascaded away) is dead however young it is.
func TestGetStorageGCRoots_PauseBuildOfDeletedEnvIsNotInFlight(t *testing.T) {
	t.Parallel()

	db := testutils.SetupDatabase(t)
	teamID := testutils.CreateTestTeam(t, db)
	envID := testutils.CreateTestTemplate(t, db, teamID)

	buildID := testutils.CreateTestBuild(t, t.Context(), db, envID, "snapshotting")
	testutils.CreateTestBuildAssignment(t, t.Context(), db, envID, buildID, "default")

	err := db.SqlcClient.TestsRawSQL(t.Context(), `DELETE FROM public.envs WHERE id = $1`, envID)
	require.NoError(t, err)

	assert.False(t, slices.Contains(storageGCRoots(t, db), buildID))
}

// The pause-specific bounds do not apply to template builds: a long-running
// build stays in flight.
func TestGetStorageGCRoots_OldTemplateBuildStaysInFlight(t *testing.T) {
	t.Parallel()

	db := testutils.SetupDatabase(t)
	teamID := testutils.CreateTestTeam(t, db)
	envID := testutils.CreateTestTemplate(t, db, teamID)

	buildID := testutils.CreateTestBuild(t, t.Context(), db, envID, "building")
	ageBuild(t, db, buildID, "3 hours")

	assert.True(t, slices.Contains(storageGCRoots(t, db), buildID))
}

// pausedSandbox is one sandbox and the snapshot builds its pauses recorded,
// oldest first.
type pausedSandbox struct {
	teamID         uuid.UUID
	baseTemplateID string
	snapshotEnvID  string
	sandboxID      string
	builds         []uuid.UUID
}

// pauseNTimes records n successful pauses of one sandbox.
func pauseNTimes(t *testing.T, db *testutils.Database, n int) pausedSandbox {
	t.Helper()

	teamID := testutils.CreateTestTeam(t, db)
	p := pausedSandbox{
		teamID:         teamID,
		baseTemplateID: testutils.CreateTestTemplate(t, db, teamID),
		snapshotEnvID:  "snapshot-template-" + uuid.NewString(),
		sandboxID:      "sandbox-" + uuid.NewString(),
	}

	for range n {
		res := testutils.UpsertTestSnapshot(t, t.Context(), db, p.snapshotEnvID, p.sandboxID, p.teamID, p.baseTemplateID)
		p.builds = append(p.builds, res.BuildID)

		// Assignment order is by created_at.
		time.Sleep(10 * time.Millisecond)
	}

	return p
}

// A paused sandbox roots its newest snapshot and none of the earlier ones: the
// newest holds the whole RAM image, and storage GC keeps the earlier rootfs
// diffs it maps to through its header.
func TestGetStorageGCRoots_PausedSandboxRootsOnlyItsNewestSnapshot(t *testing.T) {
	t.Parallel()

	db := testutils.SetupDatabase(t)
	builds := pauseNTimes(t, db, 3).builds

	roots := storageGCRoots(t, db)

	assert.True(t, slices.Contains(roots, builds[2]), "the newest snapshot is a root")
	assert.False(t, slices.Contains(roots, builds[0]), "a superseded snapshot is not a root")
	assert.False(t, slices.Contains(roots, builds[1]), "a superseded snapshot is not a root")
}

// While the next pause is in flight, and after it failed, the newest ready
// snapshot is still what the sandbox resumes from (or what an operator would
// recover), so it stays a root.
func TestGetStorageGCRoots_PreviousSnapshotStaysARootWhileThePauseIsNotReady(t *testing.T) {
	t.Parallel()

	for _, status := range []types.BuildStatus{types.BuildStatusSnapshotting, types.BuildStatusFailed} {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()

			db := testutils.SetupDatabase(t)
			p := pauseNTimes(t, db, 2)
			builds := p.builds

			unready := testutils.UpsertTestSnapshotWithStatus(t, t.Context(), db, p.snapshotEnvID, p.sandboxID, p.teamID, p.baseTemplateID, status)

			roots := storageGCRoots(t, db)

			assert.True(t, slices.Contains(roots, builds[1]), "the newest ready snapshot stays a root")
			assert.False(t, slices.Contains(roots, builds[0]))
			assert.Equal(t, status == types.BuildStatusSnapshotting, slices.Contains(roots, unready.BuildID),
				"an in-flight pause is a root, a failed one is not")
		})
	}
}
