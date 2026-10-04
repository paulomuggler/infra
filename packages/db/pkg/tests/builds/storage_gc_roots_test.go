package builds

import (
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/db/pkg/testutils"
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
