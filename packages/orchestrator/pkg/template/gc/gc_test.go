package gc

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

const blockSize = 4096

// buildDir writes a build directory whose rootfs header maps one block per
// entry of refs (a nil entry becomes the empty-block sentinel) and whose
// memfile header maps a single self block, which is what a template final layer
// looks like under the leaf-only snapshot policy.
func buildDir(t *testing.T, store, buildID string, refs ...string) {
	t.Helper()

	dir := filepath.Join(store, buildID)
	require.NoError(t, os.MkdirAll(dir, 0o755))

	id := uuid.MustParse(buildID)

	mappings := make([]header.BuildMap, 0, len(refs)+1)
	mappings = append(mappings, header.BuildMap{Offset: 0, Length: blockSize, BuildId: id})

	for i, ref := range refs {
		refID := uuid.Nil
		if ref != "" {
			refID = uuid.MustParse(ref)
		}

		mappings = append(mappings, header.BuildMap{
			Offset:  uint64((i + 1) * blockSize),
			Length:  blockSize,
			BuildId: refID,
		})
	}

	size := uint64(len(mappings) * blockSize)

	writeHeader(t, filepath.Join(dir, storage.RootfsName+storage.HeaderSuffix), id, size, mappings)
	writeHeader(t, filepath.Join(dir, storage.MemfileName+storage.HeaderSuffix), id, blockSize,
		[]header.BuildMap{{Offset: 0, Length: blockSize, BuildId: id}})

	// Stand-ins for the payload, so the directory has a nonzero size.
	require.NoError(t, os.WriteFile(filepath.Join(dir, storage.RootfsName), make([]byte, 128), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, storage.MetadataName), []byte(`{"version":2}`), 0o644))
}

// layerDir writes an intermediate layer: rootfs only, no memfile at all, which
// is what leaf-only produces.
func layerDir(t *testing.T, store, buildID string, refs ...string) {
	t.Helper()

	buildDir(t, store, buildID, refs...)
	require.NoError(t, os.Remove(filepath.Join(store, buildID, storage.MemfileName+storage.HeaderSuffix)))
}

func writeHeader(t *testing.T, path string, id uuid.UUID, size uint64, mappings []header.BuildMap) {
	t.Helper()

	meta := &header.Metadata{
		Version:     3,
		BlockSize:   blockSize,
		Size:        size,
		Generation:  0,
		BuildId:     id,
		BaseBuildId: id,
	}

	data, err := header.Serialize(meta, mappings)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))
}

func age(t *testing.T, store, buildID string, d time.Duration) {
	t.Helper()

	dir := filepath.Join(store, buildID)
	when := time.Now().Add(-d)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	for _, e := range entries {
		require.NoError(t, os.Chtimes(filepath.Join(dir, e.Name()), when, when))
	}

	require.NoError(t, os.Chtimes(dir, when, when))
}

func ids(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = uuid.NewString()
	}

	return out
}

func exists(t *testing.T, path string) bool {
	t.Helper()

	_, err := os.Stat(path)

	return err == nil
}

func cfgFor(store string) Config {
	return Config{
		TemplateStorageDir: store,
		MinAge:             time.Hour,
		Reason:             "test",
	}
}

func TestCollectKeepsTheClosureAndTakesTheRest(t *testing.T) {
	store := t.TempDir()
	id := ids(5)
	root, layerA, layerB, orphan, orphanLayer := id[0], id[1], id[2], id[3], id[4]

	// root -> layerA -> layerB; orphan -> orphanLayer
	buildDir(t, store, root, layerA)
	layerDir(t, store, layerA, layerB)
	layerDir(t, store, layerB)
	buildDir(t, store, orphan, orphanLayer)
	layerDir(t, store, orphanLayer)

	for _, b := range id {
		age(t, store, b, 2*time.Hour)
	}

	res, err := Collect(t.Context(), cfgFor(store), []string{root})
	require.NoError(t, err)

	assert.Equal(t, 5, res.ScannedDirs)
	assert.Equal(t, 3, res.KeptDirs)
	assert.Equal(t, 2, res.CollectedDirs())
	assert.Positive(t, res.FreedBytes)

	// The layers a live build reaches through its header mappings survive even
	// though nothing but those mappings knows about them.
	assert.True(t, exists(t, filepath.Join(store, root)))
	assert.True(t, exists(t, filepath.Join(store, layerA)))
	assert.True(t, exists(t, filepath.Join(store, layerB)))
	assert.False(t, exists(t, filepath.Join(store, orphan)))
	assert.False(t, exists(t, filepath.Join(store, orphanLayer)))
}

// A superseded build's layers go, but only the ones the surviving build does
// not also reach. This is the shape of every base rebuild: a fresh push shares
// a cached prefix with its predecessor.
func TestCollectKeepsLayersSharedWithTheSurvivingBuild(t *testing.T) {
	store := t.TempDir()
	id := ids(4)
	shared, oldExclusive, oldBuild, newBuild := id[0], id[1], id[2], id[3]

	layerDir(t, store, shared)
	layerDir(t, store, oldExclusive, shared)
	buildDir(t, store, oldBuild, oldExclusive)
	buildDir(t, store, newBuild, shared)

	for _, b := range id {
		age(t, store, b, 2*time.Hour)
	}

	res, err := Collect(t.Context(), cfgFor(store), []string{newBuild})
	require.NoError(t, err)

	assert.Equal(t, 2, res.KeptDirs)
	assert.Equal(t, 2, res.CollectedDirs())
	assert.True(t, exists(t, filepath.Join(store, shared)))
	assert.False(t, exists(t, filepath.Join(store, oldBuild)))
	assert.False(t, exists(t, filepath.Join(store, oldExclusive)))
}

// A paused sandbox is a generation-2 snapshot whose header maps back into the
// template final it was spawned from. That template final may itself be
// superseded — the snapshot root has to hold it alive.
func TestCollectKeepsATemplateFinalAlivePurelyForAPausedSnapshot(t *testing.T) {
	store := t.TempDir()
	id := ids(4)
	baseLayer, supersededFinal, snapshot, currentFinal := id[0], id[1], id[2], id[3]

	layerDir(t, store, baseLayer)
	buildDir(t, store, supersededFinal, baseLayer)
	buildDir(t, store, snapshot, supersededFinal)
	buildDir(t, store, currentFinal, baseLayer)

	for _, b := range id {
		age(t, store, b, 2*time.Hour)
	}

	// Roots: the env's current build, and the paused sandbox's snapshot build.
	res, err := Collect(t.Context(), cfgFor(store), []string{currentFinal, snapshot})
	require.NoError(t, err)

	assert.Equal(t, 4, res.KeptDirs)
	assert.Equal(t, 0, res.CollectedDirs())
	assert.True(t, exists(t, filepath.Join(store, supersededFinal)))

	// The first pass took supersededFinal's memfile header (only its rootfs is
	// mapped to), which moved its directory's mtime; age it again so the next
	// pass is about reachability, not the floor.
	age(t, store, supersededFinal, 2*time.Hour)

	// Drop the paused sandbox from the root set (the sandbox was killed) and
	// the same store collects the snapshot and the final it pinned.
	res, err = Collect(t.Context(), cfgFor(store), []string{currentFinal})
	require.NoError(t, err)

	assert.Equal(t, 2, res.KeptDirs)
	assert.Equal(t, 2, res.CollectedDirs())
	assert.True(t, exists(t, filepath.Join(store, baseLayer)))
	assert.False(t, exists(t, filepath.Join(store, snapshot)))
}

func TestCollectRefusesAnEmptyRootSet(t *testing.T) {
	store := t.TempDir()
	orphan := uuid.NewString()
	buildDir(t, store, orphan)
	age(t, store, orphan, 2*time.Hour)

	_, err := Collect(t.Context(), cfgFor(store), nil)
	require.ErrorIs(t, err, ErrNoRoots)
	assert.True(t, exists(t, filepath.Join(store, orphan)))
}

func TestCollectProtectsDirectoriesYoungerThanMinAge(t *testing.T) {
	store := t.TempDir()
	id := ids(3)
	root, oldOrphan, freshOrphan := id[0], id[1], id[2]

	buildDir(t, store, root)
	buildDir(t, store, oldOrphan)
	buildDir(t, store, freshOrphan)

	age(t, store, root, 2*time.Hour)
	age(t, store, oldOrphan, 2*time.Hour)

	res, err := Collect(t.Context(), cfgFor(store), []string{root})
	require.NoError(t, err)

	assert.Equal(t, 1, res.SkippedRecentDirs)
	assert.Equal(t, 1, res.CollectedDirs())
	assert.True(t, exists(t, filepath.Join(store, freshOrphan)))
	assert.False(t, exists(t, filepath.Join(store, oldOrphan)))
}

func TestCollectKeepsBrokenRootsAndReportsThem(t *testing.T) {
	store := t.TempDir()
	id := ids(3)
	brokenRoot, goodRoot, orphan := id[0], id[1], id[2]
	gone := uuid.NewString()

	buildDir(t, store, brokenRoot, gone)
	buildDir(t, store, goodRoot)
	buildDir(t, store, orphan)

	for _, b := range []string{brokenRoot, goodRoot, orphan} {
		age(t, store, b, 2*time.Hour)
	}

	res, err := Collect(t.Context(), cfgFor(store), []string{brokenRoot, goodRoot})
	require.NoError(t, err)

	assert.Equal(t, []string{brokenRoot}, res.BrokenRoots)
	assert.Equal(t, 1, res.DanglingRefs)
	assert.True(t, exists(t, filepath.Join(store, brokenRoot)), "a broken root is still a root")
	assert.Equal(t, 1, res.CollectedDirs())
}

func TestCollectReportsMissingRootsWithoutFailing(t *testing.T) {
	store := t.TempDir()
	root := uuid.NewString()
	absent := uuid.NewString()

	buildDir(t, store, root)
	age(t, store, root, 2*time.Hour)

	res, err := Collect(t.Context(), cfgFor(store), []string{root, absent})
	require.NoError(t, err)

	assert.Equal(t, []string{absent}, res.MissingRoots)
	assert.Equal(t, 1, res.KeptDirs)
}

func TestCollectDryRunDeletesNothing(t *testing.T) {
	store := t.TempDir()
	id := ids(2)
	root, orphan := id[0], id[1]

	buildDir(t, store, root)
	buildDir(t, store, orphan)
	age(t, store, root, 2*time.Hour)
	age(t, store, orphan, 2*time.Hour)

	cfg := cfgFor(store)
	cfg.DryRun = true

	res, err := Collect(t.Context(), cfg, []string{root})
	require.NoError(t, err)

	assert.Equal(t, 1, res.CollectedDirs())
	assert.Positive(t, res.FreedBytes)
	assert.True(t, exists(t, filepath.Join(store, orphan)))
}

func TestCollectIsIdempotent(t *testing.T) {
	store := t.TempDir()
	id := ids(2)
	root, orphan := id[0], id[1]

	buildDir(t, store, root)
	buildDir(t, store, orphan)
	age(t, store, root, 2*time.Hour)
	age(t, store, orphan, 2*time.Hour)

	first, err := Collect(t.Context(), cfgFor(store), []string{root})
	require.NoError(t, err)
	assert.Equal(t, 1, first.CollectedDirs())

	second, err := Collect(t.Context(), cfgFor(store), []string{root})
	require.NoError(t, err)
	assert.Equal(t, 0, second.CollectedDirs())
	assert.Equal(t, uint64(0), second.FreedBytes)
}

// Collect commits by renaming aside; the bytes go away in PurgeTrash, which
// the caller runs after dropping its build lock.
func TestCollectStagesAndPurgeTrashRemoves(t *testing.T) {
	store := t.TempDir()
	id := ids(2)
	root, orphan := id[0], id[1]

	buildDir(t, store, root)
	buildDir(t, store, orphan)
	age(t, store, root, 2*time.Hour)
	age(t, store, orphan, 2*time.Hour)

	res, err := Collect(t.Context(), cfgFor(store), []string{root})
	require.NoError(t, err)
	require.Equal(t, 1, res.CollectedDirs())

	// Gone from the store's view, still on disk under the trash.
	assert.False(t, exists(t, filepath.Join(store, orphan)))
	assert.True(t, exists(t, filepath.Join(store, trashDirName, orphan)))

	require.NoError(t, PurgeTrash(store))
	assert.False(t, exists(t, filepath.Join(store, trashDirName)))
	assert.True(t, exists(t, filepath.Join(store, root)))
}

func TestPurgeTrashIsSafeWithNothingStaged(t *testing.T) {
	require.NoError(t, PurgeTrash(t.TempDir()))
}

func TestCollectClearsAStaleTrashDirectory(t *testing.T) {
	store := t.TempDir()
	root := uuid.NewString()
	buildDir(t, store, root)
	age(t, store, root, 2*time.Hour)

	stale := filepath.Join(store, trashDirName, uuid.NewString())
	require.NoError(t, os.MkdirAll(stale, 0o755))

	_, err := Collect(t.Context(), cfgFor(store), []string{root})
	require.NoError(t, err)

	assert.False(t, exists(t, filepath.Join(store, trashDirName)))
}

func TestCollectIgnoresNonBuildDirectories(t *testing.T) {
	store := t.TempDir()
	root := uuid.NewString()
	buildDir(t, store, root)
	age(t, store, root, 2*time.Hour)

	require.NoError(t, os.MkdirAll(filepath.Join(store, "operator-scratch"), 0o755))

	res, err := Collect(t.Context(), cfgFor(store), []string{root})
	require.NoError(t, err)

	assert.Equal(t, 1, res.ScannedDirs)
	assert.True(t, exists(t, filepath.Join(store, "operator-scratch")))
}

func TestCollectWritesALedger(t *testing.T) {
	store := t.TempDir()
	ledgerRoot := t.TempDir()
	id := ids(2)
	root, orphan := id[0], id[1]

	buildDir(t, store, root)
	buildDir(t, store, orphan)
	age(t, store, root, 2*time.Hour)
	age(t, store, orphan, 2*time.Hour)

	cfg := cfgFor(store)
	cfg.LedgerDir = ledgerRoot
	cfg.Reason = "supersede:" + root

	res, err := Collect(t.Context(), cfg, []string{root})
	require.NoError(t, err)
	require.NotEmpty(t, res.LedgerPath)

	data, err := os.ReadFile(res.LedgerPath)
	require.NoError(t, err)

	var ledger Result
	require.NoError(t, json.Unmarshal(data, &ledger))

	assert.Equal(t, res.RunID, ledger.RunID)
	assert.Equal(t, cfg.Reason, ledger.Reason)
	require.Len(t, ledger.Collected, 1)
	assert.Equal(t, orphan, ledger.Collected[0].BuildID)
}

func TestReadRefsIgnoresTheEmptyBlockSentinel(t *testing.T) {
	store := t.TempDir()
	id := uuid.NewString()

	// "" becomes uuid.Nil, the sentinel CreateMapping uses for empty blocks.
	buildDir(t, store, id, "")

	refs, err := readRefs(filepath.Join(store, id), rootfsArtifact)
	require.NoError(t, err)
	assert.Empty(t, refs)
}

func TestReadRefsFailsOnAnUnparseableHeader(t *testing.T) {
	store := t.TempDir()
	id := uuid.NewString()
	buildDir(t, store, id)

	require.NoError(t, os.WriteFile(
		filepath.Join(store, id, storage.RootfsName+storage.HeaderSuffix),
		[]byte("not a header"), 0o644))

	_, err := readRefs(filepath.Join(store, id), rootfsArtifact)
	require.Error(t, err)

	// And it takes the whole run with it rather than collecting on a partial
	// view of the references.
	_, err = Collect(t.Context(), cfgFor(store), []string{id})
	require.Error(t, err)
}

func TestVerifyDisjointRefusesWhenAKeptBuildReferencesACollectedOne(t *testing.T) {
	store := t.TempDir()
	id := ids(2)
	kept, referenced := id[0], id[1]

	buildDir(t, store, kept, referenced)
	buildDir(t, store, referenced)

	err := verifyDisjoint(
		t.Context(), store,
		map[node]struct{}{{id: kept, artifact: rootfsArtifact}: {}},
		map[string]struct{}{referenced: {}},
		nil,
	)
	require.ErrorContains(t, err, "gc integrity check failed")
}

func TestVerifyDisjointRefusesABuildThatIsBothKeptAndCollected(t *testing.T) {
	store := t.TempDir()
	id := uuid.NewString()
	buildDir(t, store, id)

	err := verifyDisjoint(
		t.Context(), store,
		map[node]struct{}{{id: id, artifact: rootfsArtifact}: {}},
		map[string]struct{}{id: {}},
		nil,
	)
	require.ErrorContains(t, err, "both kept and collected")
}

func TestClosureTerminatesOnAReferenceCycle(t *testing.T) {
	store := t.TempDir()
	id := ids(2)
	a, b := id[0], id[1]

	buildDir(t, store, a, b)
	buildDir(t, store, b, a)

	scanned, err := scan(store)
	require.NoError(t, err)

	keep, dangling := closure(scanned, []string{a})
	assert.Len(t, dirsOf(keep), 2)
	assert.Empty(t, dangling)
	assert.Empty(t, brokenDirs(scanned, nil, nil))
}

// A directory the age floor spares may reference one this pass is taking, which
// makes it broken the moment the pass commits. Its index entry has to go with
// it: leaving it is a cache hit onto a chain that faults, the exact state the
// prune exists to prevent.
func TestCollectPrunesTheIndexOfADirThisPassIsAboutToBreak(t *testing.T) {
	store := t.TempDir()
	cacheDir := t.TempDir()

	id := ids(3)
	root, doomedLayer, sparedByAge := id[0], id[1], id[2]

	buildDir(t, store, root)
	layerDir(t, store, doomedLayer)
	// References the layer that is about to be collected, but is too new to be
	// collected itself.
	buildDir(t, store, sparedByAge, doomedLayer)

	age(t, store, root, 2*time.Hour)
	age(t, store, doomedLayer, 2*time.Hour)

	indexDir := filepath.Join(cacheDir, "scope-1", "index")
	require.NoError(t, os.MkdirAll(indexDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(indexDir, "spared"),
		[]byte(`{"template":{"build_id":"`+sparedByAge+`"}}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(indexDir, "root"),
		[]byte(`{"template":{"build_id":"`+root+`"}}`), 0o644))

	cfg := cfgFor(store)
	cfg.BuildCacheDir = cacheDir

	res, err := Collect(t.Context(), cfg, []string{root})
	require.NoError(t, err)

	require.Equal(t, 1, res.CollectedDirs())
	require.Equal(t, 1, res.SkippedRecentDirs)

	assert.True(t, exists(t, filepath.Join(store, sparedByAge)), "the age floor keeps the dir")
	assert.False(t, exists(t, filepath.Join(indexDir, "spared")),
		"but its index entry now resolves to a chain that faults, so it must be pruned")
	assert.True(t, exists(t, filepath.Join(indexDir, "root")))
	assert.Equal(t, 1, res.PrunedIndexBlobs)
}

// The index is pruned BEFORE the directories move, so a run that dies between
// the two leaves "entry gone, directory present" — a cache miss — and never
// "entry live, directory gone", which is a cache hit onto nothing.
//
// Pinned by making the prune fail and checking nothing has been staged. Reverse
// the two steps in Collect and this goes red.
func TestCollectPrunesTheIndexBeforeItMovesAnything(t *testing.T) {
	store := t.TempDir()
	cacheDir := t.TempDir()

	id := ids(2)
	root, orphan := id[0], id[1]

	buildDir(t, store, root)
	buildDir(t, store, orphan)
	age(t, store, root, 2*time.Hour)
	age(t, store, orphan, 2*time.Hour)

	// A scope whose "index" is a regular file: ReadDir fails with ENOTDIR,
	// which no amount of privilege turns into success.
	require.NoError(t, os.MkdirAll(filepath.Join(cacheDir, "scope-1"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir, "scope-1", "index"), []byte("x"), 0o644))

	cfg := cfgFor(store)
	cfg.BuildCacheDir = cacheDir

	_, err := Collect(t.Context(), cfg, []string{root})
	require.Error(t, err)

	assert.True(t, exists(t, filepath.Join(store, orphan)),
		"a prune that fails must abort before any directory is staged")
	assert.False(t, exists(t, filepath.Join(store, trashDirName)))
	assert.True(t, exists(t, filepath.Join(store, root)))
}

func TestPruneIndexRemovesEntriesThatCannotResolveToAUsableLayer(t *testing.T) {
	store := t.TempDir()
	cacheDir := t.TempDir()

	id := ids(4)
	live, collected, broken, absent := id[0], id[1], id[2], id[3]
	gone := uuid.NewString()

	buildDir(t, store, live)
	buildDir(t, store, collected)
	buildDir(t, store, broken, gone)

	scanned, err := scan(store)
	require.NoError(t, err)

	indexDir := filepath.Join(cacheDir, "scope-1", "index")
	require.NoError(t, os.MkdirAll(indexDir, 0o755))

	write := func(name, buildID string) {
		require.NoError(t, os.WriteFile(filepath.Join(indexDir, name),
			[]byte(`{"template":{"build_id":"`+buildID+`"}}`), 0o644))
	}

	write("live", live)
	write("collected", collected)
	write("broken", broken)
	write("absent", absent)
	require.NoError(t, os.WriteFile(filepath.Join(indexDir, "garbage"), []byte("{"), 0o644))

	pruned, err := pruneIndex(cacheDir, scanned, map[string]struct{}{collected: {}}, nil, false)
	require.NoError(t, err)

	assert.Equal(t, 4, pruned)
	assert.True(t, exists(t, filepath.Join(indexDir, "live")))
	assert.False(t, exists(t, filepath.Join(indexDir, "collected")))
	assert.False(t, exists(t, filepath.Join(indexDir, "broken")))
	assert.False(t, exists(t, filepath.Join(indexDir, "absent")))
	assert.False(t, exists(t, filepath.Join(indexDir, "garbage")))

	_ = gone
}

func TestPruneIndexHonoursDryRun(t *testing.T) {
	store := t.TempDir()
	cacheDir := t.TempDir()

	orphan := uuid.NewString()
	buildDir(t, store, orphan)

	scanned, err := scan(store)
	require.NoError(t, err)

	indexDir := filepath.Join(cacheDir, "scope-1", "index")
	require.NoError(t, os.MkdirAll(indexDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(indexDir, "entry"),
		[]byte(`{"template":{"build_id":"`+orphan+`"}}`), 0o644))

	pruned, err := pruneIndex(cacheDir, scanned, map[string]struct{}{orphan: {}}, nil, true)
	require.NoError(t, err)

	assert.Equal(t, 1, pruned)
	assert.True(t, exists(t, filepath.Join(indexDir, "entry")))
}

func TestPruneIndexIsANoOpWithoutACacheDir(t *testing.T) {
	pruned, err := pruneIndex("", nil, nil, nil, false)
	require.NoError(t, err)
	assert.Equal(t, 0, pruned)

	pruned, err = pruneIndex(filepath.Join(t.TempDir(), "missing"), nil, nil, nil, false)
	require.NoError(t, err)
	assert.Equal(t, 0, pruned)
}

func TestCollectPrunesTheIndexAlongsideTheDirectories(t *testing.T) {
	store := t.TempDir()
	cacheDir := t.TempDir()

	id := ids(2)
	root, orphan := id[0], id[1]

	buildDir(t, store, root)
	buildDir(t, store, orphan)
	age(t, store, root, 2*time.Hour)
	age(t, store, orphan, 2*time.Hour)

	indexDir := filepath.Join(cacheDir, "scope-1", "index")
	require.NoError(t, os.MkdirAll(indexDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(indexDir, "root"),
		[]byte(`{"template":{"build_id":"`+root+`"}}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(indexDir, "orphan"),
		[]byte(`{"template":{"build_id":"`+orphan+`"}}`), 0o644))

	cfg := cfgFor(store)
	cfg.BuildCacheDir = cacheDir

	res, err := Collect(t.Context(), cfg, []string{root})
	require.NoError(t, err)

	assert.Equal(t, 1, res.PrunedIndexBlobs)
	assert.True(t, exists(t, filepath.Join(indexDir, "root")))
	assert.False(t, exists(t, filepath.Join(indexDir, "orphan")))
}

func TestCollectRequiresATemplateStorageDir(t *testing.T) {
	_, err := Collect(context.Background(), Config{}, []string{uuid.NewString()})
	require.ErrorContains(t, err, "template storage dir is required")
}

// mappings builds a header mapping of one block for self, then one per ref
// ("" is the empty-block sentinel).
func mappings(self string, refs []string) ([]header.BuildMap, uint64) {
	out := []header.BuildMap{{Offset: 0, Length: blockSize, BuildId: uuid.MustParse(self)}}

	for i, ref := range refs {
		refID := uuid.Nil
		if ref != "" {
			refID = uuid.MustParse(ref)
		}

		out = append(out, header.BuildMap{Offset: uint64((i + 1) * blockSize), Length: blockSize, BuildId: refID})
	}

	return out, uint64(len(out) * blockSize)
}

// snapshotDir writes a paused sandbox's snapshot: rootfs and memfile, each a
// data file plus a header mapping to the given builds, and the snapfile and
// metadata that only matter while the build is a root.
func snapshotDir(t *testing.T, store, buildID string, rootfsRefs, memfileRefs []string) {
	t.Helper()

	dir := filepath.Join(store, buildID)
	require.NoError(t, os.MkdirAll(dir, 0o755))

	id := uuid.MustParse(buildID)

	m, size := mappings(buildID, rootfsRefs)
	writeHeader(t, filepath.Join(dir, storage.RootfsName+storage.HeaderSuffix), id, size, m)
	m, size = mappings(buildID, memfileRefs)
	writeHeader(t, filepath.Join(dir, storage.MemfileName+storage.HeaderSuffix), id, size, m)

	require.NoError(t, os.WriteFile(filepath.Join(dir, storage.RootfsName), make([]byte, 64), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, storage.MemfileName), make([]byte, 4096), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, storage.SnapfileName), make([]byte, 16), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, storage.MetadataName), []byte(`{"version":2}`), 0o644))
}

func memfilePaths(store, buildID string) []string {
	return []string{
		filepath.Join(store, buildID, storage.MemfileName),
		filepath.Join(store, buildID, storage.MemfileName+storage.HeaderSuffix),
	}
}

// A sandbox paused three times with self-contained memfiles: every rootfs diff
// stays, because the newest rootfs header still maps blocks to each of them,
// but only the newest memfile does. This is the bound the per-file split buys.
func TestCollectKeepsOnlyTheNewestSelfContainedMemfileOfAChain(t *testing.T) {
	t.Parallel()

	store := t.TempDir()
	id := ids(4)
	template, first, second, newest := id[0], id[1], id[2], id[3]

	buildDir(t, store, template)
	snapshotDir(t, store, first, []string{template}, nil)
	snapshotDir(t, store, second, []string{template, first}, nil)
	snapshotDir(t, store, newest, []string{template, first, second}, nil)

	for _, b := range id {
		age(t, store, b, 2*time.Hour)
	}

	res, err := Collect(t.Context(), cfgFor(store), []string{template, newest})
	require.NoError(t, err)

	assert.Equal(t, 4, res.KeptDirs)
	assert.Equal(t, 0, res.CollectedDirs())
	require.Equal(t, 2, res.TrimmedDirs())

	trimmed := map[string][]string{}
	for _, tr := range res.Trimmed {
		trimmed[tr.BuildID] = tr.Files
		assert.Positive(t, tr.Bytes)
	}

	assert.ElementsMatch(t, []string{storage.MemfileName, storage.MemfileName + storage.HeaderSuffix}, trimmed[first])
	assert.ElementsMatch(t, []string{storage.MemfileName, storage.MemfileName + storage.HeaderSuffix}, trimmed[second])

	for _, b := range []string{first, second} {
		for _, p := range memfilePaths(store, b) {
			assert.False(t, exists(t, p), "superseded memfile %s should be gone", p)
		}

		assert.True(t, exists(t, filepath.Join(store, b, storage.RootfsName)), "a rootfs diff the newest snapshot maps to stays")
		assert.True(t, exists(t, filepath.Join(store, b, storage.RootfsName+storage.HeaderSuffix)))
		assert.True(t, exists(t, filepath.Join(store, trashDirName, b+"."+storage.MemfileName, storage.MemfileName)))
	}

	for _, p := range memfilePaths(store, newest) {
		assert.True(t, exists(t, p), "the newest snapshot is a root and keeps its memfile")
	}

	require.NoError(t, PurgeTrash(store))
	assert.False(t, exists(t, filepath.Join(store, trashDirName)))

	// A second pass over the trimmed store finds nothing more to take and
	// nothing broken.
	again, err := Collect(t.Context(), cfgFor(store), []string{template, newest})
	require.NoError(t, err)
	assert.Equal(t, 0, again.CollectedDirs())
	assert.Equal(t, 0, again.TrimmedDirs())
	assert.Equal(t, 0, again.DanglingRefs)
	assert.Empty(t, again.BrokenRoots)
}

// A snapshot paused before memfiles were self-contained still pages from its
// ancestors' memfiles, and those stay for as long as it is a root. Only the
// ancestor reached through rootfs alone loses its memfile.
func TestCollectKeepsTheMemfilesADiffSnapshotStillReads(t *testing.T) {
	t.Parallel()

	store := t.TempDir()
	id := ids(3)
	rootfsOnly, memfileAncestor, newest := id[0], id[1], id[2]

	snapshotDir(t, store, rootfsOnly, nil, nil)
	snapshotDir(t, store, memfileAncestor, []string{rootfsOnly}, nil)
	snapshotDir(t, store, newest, []string{rootfsOnly, memfileAncestor}, []string{memfileAncestor})

	for _, b := range id {
		age(t, store, b, 2*time.Hour)
	}

	res, err := Collect(t.Context(), cfgFor(store), []string{newest})
	require.NoError(t, err)

	require.Equal(t, 1, res.TrimmedDirs())
	assert.Equal(t, rootfsOnly, res.Trimmed[0].BuildID)

	for _, p := range memfilePaths(store, memfileAncestor) {
		assert.True(t, exists(t, p))
	}
}

// Once a pause's memfile is the only one left, the snapshot it superseded is a
// rootfs layer and nothing else; killing the sandbox then takes the whole
// chain, directories and all.
func TestCollectTakesATrimmedChainWholeWhenItsRootGoes(t *testing.T) {
	t.Parallel()

	store := t.TempDir()
	id := ids(3)
	template, first, newest := id[0], id[1], id[2]

	buildDir(t, store, template)
	snapshotDir(t, store, first, []string{template}, nil)
	snapshotDir(t, store, newest, []string{template, first}, nil)

	for _, b := range id {
		age(t, store, b, 2*time.Hour)
	}

	_, err := Collect(t.Context(), cfgFor(store), []string{template, newest})
	require.NoError(t, err)
	require.NoError(t, PurgeTrash(store))

	// The trim renamed files out of first's directory, which moves its mtime;
	// age it again so this pass is about reachability, not the floor.
	age(t, store, first, 2*time.Hour)

	res, err := Collect(t.Context(), cfgFor(store), []string{template})
	require.NoError(t, err)

	assert.Equal(t, 2, res.CollectedDirs())
	assert.False(t, exists(t, filepath.Join(store, first)))
	assert.False(t, exists(t, filepath.Join(store, newest)))
	assert.True(t, exists(t, filepath.Join(store, template)))
}

// The age floor protects a file the same way it protects a directory.
func TestCollectProtectsAFreshUnreachableMemfile(t *testing.T) {
	t.Parallel()

	store := t.TempDir()
	id := ids(2)
	first, newest := id[0], id[1]

	snapshotDir(t, store, first, nil, nil)
	snapshotDir(t, store, newest, []string{first}, nil)
	age(t, store, newest, 2*time.Hour)

	res, err := Collect(t.Context(), cfgFor(store), []string{newest})
	require.NoError(t, err)

	assert.Equal(t, 0, res.TrimmedDirs())
	assert.Equal(t, 1, res.SkippedRecentFiles)

	for _, p := range memfilePaths(store, first) {
		assert.True(t, exists(t, p))
	}
}

// A header that maps blocks to a build whose directory is there but whose
// data file for that artifact is not is a broken chain, the same as a missing
// directory.
func TestCollectReportsAReferenceToAMissingDataFileAsDangling(t *testing.T) {
	t.Parallel()

	store := t.TempDir()
	id := ids(2)
	ancestor, root := id[0], id[1]

	snapshotDir(t, store, ancestor, nil, nil)
	snapshotDir(t, store, root, nil, []string{ancestor})
	require.NoError(t, os.Remove(filepath.Join(store, ancestor, storage.MemfileName)))

	for _, b := range id {
		age(t, store, b, 2*time.Hour)
	}

	res, err := Collect(t.Context(), cfgFor(store), []string{root})
	require.NoError(t, err)

	assert.Equal(t, 1, res.DanglingRefs)
	assert.Equal(t, []string{root}, res.BrokenRoots)

	// What the broken link still holds is kept, not collected out from under
	// the root that maps to it.
	assert.Equal(t, 0, res.CollectedDirs())
	assert.True(t, exists(t, filepath.Join(store, ancestor, storage.MemfileName+storage.HeaderSuffix)))
}

func TestVerifyDisjointRefusesWhenAKeptFileReferencesATrimmedOne(t *testing.T) {
	t.Parallel()

	store := t.TempDir()
	id := ids(2)
	kept, referenced := id[0], id[1]

	snapshotDir(t, store, referenced, nil, nil)
	snapshotDir(t, store, kept, nil, []string{referenced})

	err := verifyDisjoint(
		t.Context(), store,
		map[node]struct{}{{id: kept, artifact: memfileArtifact}: {}},
		nil,
		map[node]struct{}{{id: referenced, artifact: memfileArtifact}: {}},
	)
	require.ErrorContains(t, err, "which this pass collects")

	// The other artifact of the same build being trimmed is no conflict.
	err = verifyDisjoint(
		t.Context(), store,
		map[node]struct{}{{id: kept, artifact: memfileArtifact}: {}},
		nil,
		map[node]struct{}{{id: referenced, artifact: rootfsArtifact}: {}},
	)
	require.NoError(t, err)
}

// An index entry is a promise that its target is a whole, usable build; one
// losing a file is no longer that.
func TestPruneIndexRemovesEntriesForATrimmedBuild(t *testing.T) {
	t.Parallel()

	store := t.TempDir()
	cacheDir := t.TempDir()

	target := uuid.NewString()
	snapshotDir(t, store, target, nil, nil)

	scanned, err := scan(store)
	require.NoError(t, err)

	indexDir := filepath.Join(cacheDir, "scope-1", "index")
	require.NoError(t, os.MkdirAll(indexDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(indexDir, "entry"),
		[]byte(`{"template":{"build_id":"`+target+`"}}`), 0o644))

	pruned, err := pruneIndex(cacheDir, scanned, nil, map[node]struct{}{{id: target, artifact: memfileArtifact}: {}}, false)
	require.NoError(t, err)

	assert.Equal(t, 1, pruned)
	assert.False(t, exists(t, filepath.Join(indexDir, "entry")))
}

// Reachability never crosses artifacts: a rootfs header naming a build keeps
// that build's rootfs, not its memfile.
func TestClosureFollowsEachArtifactSeparately(t *testing.T) {
	t.Parallel()

	store := t.TempDir()
	id := ids(3)
	viaRootfs, viaMemfile, root := id[0], id[1], id[2]

	snapshotDir(t, store, viaRootfs, nil, nil)
	snapshotDir(t, store, viaMemfile, nil, nil)
	snapshotDir(t, store, root, []string{viaRootfs}, []string{viaMemfile})

	scanned, err := scan(store)
	require.NoError(t, err)

	keep, dangling := closure(scanned, []string{root})
	assert.Empty(t, dangling)
	assert.Equal(t, map[node]struct{}{
		{id: root, artifact: rootfsArtifact}:        {},
		{id: root, artifact: memfileArtifact}:       {},
		{id: viaRootfs, artifact: rootfsArtifact}:   {},
		{id: viaMemfile, artifact: memfileArtifact}: {},
	}, keep)
}
