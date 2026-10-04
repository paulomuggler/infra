// Package gc reclaims template-storage build directories that are no longer
// reachable from any live root.
//
// Upstream e2b-infra never collects template storage: template deletion is
// metadata-only by design ("build artifacts are intentionally NOT deleted from
// storage here because builds are layered diffs that may be referenced by other
// builds' header mappings. [ENG-3477] a future GC mechanism will handle
// orphaned storage"). With STORAGE_PROVIDER=Local that makes disk exhaustion
// the default trajectory — ordinary build cadence alone writes 75-165 GiB a day
// and nothing ever takes it back. This package is that GC.
//
// # Reachability, not registry membership
//
// A build's rootfs and memfile are layered diffs. The block mappings in
// rootfs.ext4.header / memfile.header name the build IDs that actually hold
// each range of blocks, and those IDs are routinely *unregistered* — they are
// intermediate layer artifacts that never get an env_builds row yet are read at
// page-fetch time for the whole life of the build. Classifying a directory by
// whether the registry knows it deletes live layers: doing exactly that on
// 2026-08-01 broke 408 registered builds on this host, silently, surfacing only
// as Input/output error inside guests. The only sound criterion is transitive
// reachability from a live root through parsed header mappings, which is what
// Collect computes.
//
// # Per file, not per directory
//
// A build's two layered files are reached independently: a rootfs header maps
// blocks to other builds' rootfs data, a memfile header to other builds' memfile
// data, never across. Reachability is therefore tracked per (build, file), and a
// directory that is still reachable through one file loses the other once
// nothing reaches it. This is what bounds a paused sandbox: each snapshot's
// rootfs diff stays as a layer under the next, while the memfile of every
// snapshot but the newest — a guest-sized file — goes as soon as a newer
// self-contained one exists.
package gc

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

// trashDirName is the staging directory collected build dirs are renamed into
// before they are removed. It lives inside the template store so the rename is
// a same-filesystem operation, and it is not a valid build ID so the scanner
// skips it.
const trashDirName = ".gc-trash"

// ledgerDirName is where run ledgers are written under the ledger root.
const ledgerDirName = "template-gc"

// ErrNoRoots is returned when Collect is called with an empty root set. An
// empty root set is indistinguishable from a failed registry read, and acting
// on one is how the 2026-08-01 outage happened.
var ErrNoRoots = errors.New("refusing to collect with an empty root set")

// Config parameterises a collection run.
type Config struct {
	// TemplateStorageDir is the local template store — one directory per build
	// ID. Required.
	TemplateStorageDir string
	// BuildCacheDir is the local build cache root, holding the layer hash index
	// (<cacheScope>/index/<hash>). Empty disables index pruning.
	BuildCacheDir string
	// LedgerDir is where the run ledger is written. Empty disables the ledger.
	LedgerDir string
	// MinAge protects directories modified more recently than this. It covers
	// intermediate layers minted by a build the orchestrator has since
	// forgotten (a crash or restart mid-build).
	MinAge time.Duration
	// DryRun computes and reports everything but deletes nothing.
	DryRun bool
	// Reason is recorded in the ledger and the log line: "supersede:<buildID>",
	// "periodic", "manual".
	Reason string
	// Now defaults to time.Now.
	Now func() time.Time
}

// CollectedDir is one reclaimed directory.
type CollectedDir struct {
	BuildID string `json:"buildID"`
	Bytes   uint64 `json:"bytes"`
}

// Result is the outcome of a run, and the body of its ledger.
type Result struct {
	RunID      string    `json:"runID"`
	Reason     string    `json:"reason"`
	DryRun     bool      `json:"dryRun"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`

	MinAgeSeconds int64 `json:"minAgeSeconds"`

	// ScannedDirs is every build directory found in the store.
	ScannedDirs int `json:"scannedDirs"`
	// Roots is the size of the union root set handed to the run.
	Roots int `json:"roots"`
	// MissingRoots are roots with no directory on disk. Reported, never fatal:
	// a root whose storage is already gone changes nothing about what is safe
	// to collect.
	MissingRoots []string `json:"missingRoots"`
	// KeptDirs is the number of directories the reference closure over the
	// roots reaches through at least one of their files.
	KeptDirs int `json:"keptDirs"`
	// SkippedRecentDirs were unreachable but younger than MinAge.
	SkippedRecentDirs int `json:"skippedRecentDirs"`
	// SkippedRecentFiles counts files of kept directories (one build's rootfs
	// or memfile, data and header) that were unreachable but younger than
	// MinAge.
	SkippedRecentFiles int `json:"skippedRecentFiles"`
	// DanglingRefs counts distinct referenced (build, file) pairs whose data
	// file is not in the store, reachable from the roots — i.e. broken chains
	// inside the live set.
	DanglingRefs int `json:"danglingRefs"`
	// BrokenRoots are roots whose closure contains a dangling reference. They
	// are kept (a broken root is still a root); this is the inventory the
	// deliberate repair sweep consumes.
	BrokenRoots []string `json:"brokenRoots"`

	Collected []CollectedDir `json:"collected"`
	// Trimmed are files collected out of directories that stay, because the
	// build is still reachable through its other file.
	Trimmed []TrimmedFiles `json:"trimmed"`
	// FreedBytes, for whole directories and trimmed files alike, is apparent size — the sum of stat sizes — which reads about
	// 1.8% above what df gives back, because rootfs.ext4 is sparse. It is what
	// the store charges you for on paper, not what the filesystem returns.
	FreedBytes       uint64 `json:"freedBytes"`
	PrunedIndexBlobs int    `json:"prunedIndexBlobs"`

	LedgerPath string `json:"-"`
}

// TrimmedFiles is one file of a kept build — its data and its header —
// reclaimed because nothing reaches it any more.
type TrimmedFiles struct {
	BuildID string   `json:"buildID"`
	Files   []string `json:"files"`
	Bytes   uint64   `json:"bytes"`
}

// CollectedDirs is the number of directories collected.
func (r *Result) CollectedDirs() int { return len(r.Collected) }

// TrimmedDirs is the number of kept directories that lost a file.
func (r *Result) TrimmedDirs() int { return len(r.Trimmed) }

// Collect commits a collection pass: it prunes build-cache index entries that
// can no longer resolve to a usable layer, and renames every build directory
// not transitively reachable from roots into the trash staging directory — and,
// out of a directory that is reachable, the files of the one layered file
// (rootfs or memfile) nothing reaches. Both
// are O(1)-per-entry, so a caller holding a lock against concurrent builds can
// hold it across exactly this call.
//
// The bulk removal is deliberately NOT here — call PurgeTrash after releasing
// that lock. Deleting a couple of hundred gigabytes takes minutes; deciding
// what to delete takes a second, and only the deciding has to be exclusive.
//
// roots must be non-empty: see ErrNoRoots.
func Collect(ctx context.Context, cfg Config, roots []string) (*Result, error) {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	if cfg.TemplateStorageDir == "" {
		return nil, errors.New("template storage dir is required")
	}

	if len(roots) == 0 {
		return nil, ErrNoRoots
	}

	res := &Result{
		RunID:         uuid.NewString(),
		Reason:        cfg.Reason,
		DryRun:        cfg.DryRun,
		StartedAt:     now(),
		MinAgeSeconds: int64(cfg.MinAge.Seconds()),
		Roots:         len(roots),
		MissingRoots:  []string{},
		BrokenRoots:   []string{},
		Collected:     []CollectedDir{},
		Trimmed:       []TrimmedFiles{},
	}

	// A trash directory left by a run that died between the rename and the
	// removal. Its contents are already committed as collected.
	if err := os.RemoveAll(filepath.Join(cfg.TemplateStorageDir, trashDirName)); err != nil {
		return nil, fmt.Errorf("failed to clear stale gc trash: %w", err)
	}

	store, err := scan(cfg.TemplateStorageDir)
	if err != nil {
		return nil, err
	}

	res.ScannedDirs = len(store)

	keep, dangling := closure(store, roots)
	keptDirs := dirsOf(keep)
	res.KeptDirs = len(keptDirs)
	res.DanglingRefs = len(dangling)

	for _, r := range roots {
		if _, ok := store[r]; !ok {
			res.MissingRoots = append(res.MissingRoots, r)

			continue
		}

		if _, missing := closure(store, []string{r}); len(missing) > 0 {
			res.BrokenRoots = append(res.BrokenRoots, r)
		}
	}

	sort.Strings(res.MissingRoots)
	sort.Strings(res.BrokenRoots)

	cutoff := now().Add(-cfg.MinAge)
	collect := make(map[string]struct{})
	trim := make(map[node]struct{})

	for id, d := range store {
		if _, kept := keptDirs[id]; !kept {
			if d.modTime.After(cutoff) {
				res.SkippedRecentDirs++

				continue
			}

			collect[id] = struct{}{}

			continue
		}

		for _, a := range artifacts {
			f := d.files[a]
			if len(f.names) == 0 {
				continue
			}

			if _, kept := keep[node{id: id, artifact: a}]; kept {
				continue
			}

			// The same floor as for directories: a file written moments ago
			// may belong to a build whose root this pass could not see yet.
			if f.modTime.After(cutoff) {
				res.SkippedRecentFiles++

				continue
			}

			trim[node{id: id, artifact: a}] = struct{}{}
		}
	}

	// Independent re-verification: re-read every kept file's header from disk
	// and re-check each reference against what this pass takes. This does not
	// trust the closure computed above — it is the check that turns "the
	// closure should not overlap the collect set" into something the run
	// proves before it deletes anything.
	if err := verifyDisjoint(ctx, cfg.TemplateStorageDir, keep, collect, trim); err != nil {
		return nil, err
	}

	for id := range collect {
		res.Collected = append(res.Collected, CollectedDir{BuildID: id, Bytes: store[id].bytes})
		res.FreedBytes += store[id].bytes
	}

	sort.Slice(res.Collected, func(i, j int) bool { return res.Collected[i].BuildID < res.Collected[j].BuildID })

	for n := range trim {
		f := store[n.id].files[n.artifact]
		res.Trimmed = append(res.Trimmed, TrimmedFiles{BuildID: n.id, Files: f.names, Bytes: f.bytes})
		res.FreedBytes += f.bytes
	}

	slices.SortFunc(res.Trimmed, func(a, b TrimmedFiles) int {
		return cmp.Or(cmp.Compare(a.BuildID, b.BuildID), cmp.Compare(a.Files[0], b.Files[0]))
	})

	// Index blobs are pruned BEFORE the directories move. A crash between the
	// two can then only leave "index entry gone, directory present" — a cache
	// miss, costing a rebuild. The reverse order leaves "index entry live,
	// directory gone", which is a cache *hit* onto nothing and reintroduces
	// the silent-corruption failure this package exists to end.
	pruned, err := pruneIndex(cfg.BuildCacheDir, store, collect, trim, cfg.DryRun)
	if err != nil {
		return nil, err
	}

	res.PrunedIndexBlobs = pruned

	if !cfg.DryRun {
		if err := stage(cfg.TemplateStorageDir, store, collect, trim); err != nil {
			return nil, err
		}
	}

	res.FinishedAt = now()

	if err := writeLedger(cfg.LedgerDir, res); err != nil {
		return nil, err
	}

	return res, nil
}

// artifact is one of a build's two layered files. Each is a data file plus a
// header mapping its blocks to the builds that hold them, and a header only
// ever maps to the same artifact of other builds.
type artifact int

const (
	rootfsArtifact artifact = iota
	memfileArtifact
)

var artifacts = [...]artifact{rootfsArtifact, memfileArtifact}

// dataName is the artifact's data file, which is also how it is named in
// errors and the trash.
func (a artifact) dataName() string {
	if a == memfileArtifact {
		return storage.MemfileName
	}

	return storage.RootfsName
}

func (a artifact) headerName() string { return a.dataName() + storage.HeaderSuffix }

// node is one artifact of one build: the unit reachability is computed over.
type node struct {
	id       string
	artifact artifact
}

func (n node) String() string { return n.id + "/" + n.artifact.dataName() }

// artifactFiles is what a build directory holds of one artifact.
type artifactFiles struct {
	// refs are the other builds the header maps blocks to; the same artifact
	// of each is what this one reads.
	refs map[string]struct{}
	// names are the artifact's files present: data, header, or both.
	names   []string
	hasData bool
	bytes   uint64
	modTime time.Time
}

// dirInfo is one build directory as found on disk.
type dirInfo struct {
	files   [len(artifacts)]artifactFiles
	bytes   uint64
	modTime time.Time
}

// scan reads every build directory in the store, parsing both headers for the
// build IDs their block mappings name.
func scan(storeDir string) (map[string]*dirInfo, error) {
	entries, err := os.ReadDir(storeDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read template storage dir %q: %w", storeDir, err)
	}

	store := make(map[string]*dirInfo, len(entries))

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}

		// Build directories are named by build UUID. Anything else (the trash
		// staging dir, an operator's scratch) is not ours to reason about.
		if _, err := uuid.Parse(e.Name()); err != nil {
			continue
		}

		d, err := readDir(storeDir, e.Name())
		if err != nil {
			return nil, err
		}

		store[e.Name()] = d
	}

	return store, nil
}

func readDir(storeDir, buildID string) (*dirInfo, error) {
	path := filepath.Join(storeDir, buildID)

	files, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read build dir %q: %w", path, err)
	}

	d := &dirInfo{}

	for _, f := range files {
		info, err := f.Info()
		if err != nil {
			return nil, fmt.Errorf("failed to stat %q: %w", filepath.Join(path, f.Name()), err)
		}

		size := uint64(info.Size())
		d.bytes += size

		if info.ModTime().After(d.modTime) {
			d.modTime = info.ModTime()
		}

		for _, a := range artifacts {
			if f.Name() != a.dataName() && f.Name() != a.headerName() {
				continue
			}

			af := &d.files[a]
			af.names = append(af.names, f.Name())
			af.bytes += size
			af.hasData = af.hasData || f.Name() == a.dataName()

			if info.ModTime().After(af.modTime) {
				af.modTime = info.ModTime()
			}
		}
	}

	if info, err := os.Stat(path); err == nil && info.ModTime().After(d.modTime) {
		d.modTime = info.ModTime()
	}

	for _, a := range artifacts {
		refs, err := readRefs(path, a)
		if err != nil {
			return nil, err
		}

		d.files[a].refs = refs
	}

	return d, nil
}

// readRefs returns every *external* build ID named by the block mappings of
// one of a build's headers — the builds whose same artifact it pages from. A
// missing header contributes nothing: an intermediate layer under the leaf-only
// snapshot policy has no memfile header at all. A header that exists but cannot
// be parsed is fatal — a closure computed over a header we could not read is a
// guess, and guessing is what this package refuses to do.
func readRefs(dir string, a artifact) (map[string]struct{}, error) {
	self := filepath.Base(dir)
	refs := make(map[string]struct{})

	path := filepath.Join(dir, a.headerName())

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return refs, nil
	}

	if err != nil {
		return nil, fmt.Errorf("failed to read header %q: %w", path, err)
	}

	h, err := header.DeserializeBytes(data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse header %q: %w", path, err)
	}

	for _, m := range h.Mapping {
		if m.BuildId == uuid.Nil {
			// The sentinel for empty blocks, not a reference.
			continue
		}

		id := m.BuildId.String()
		if id == self {
			continue
		}

		refs[id] = struct{}{}
	}

	return refs, nil
}

// closure returns the (build, artifact) pairs reachable from the seed builds —
// every artifact of a seed, and transitively the same artifact of each build
// its header maps to — and the referenced pairs whose data file is not in the
// store (dangling references — broken chains, which page-fault as Input/output
// error inside a guest). A dangling pair whose directory exists is kept all the
// same.
func closure(store map[string]*dirInfo, seeds []string) (keep map[node]struct{}, dangling map[node]struct{}) {
	keep = make(map[node]struct{}, len(seeds)*len(artifacts))
	dangling = make(map[node]struct{})

	var frontier []node

	for _, s := range seeds {
		if _, ok := store[s]; !ok {
			continue
		}

		for _, a := range artifacts {
			n := node{id: s, artifact: a}
			if _, seen := keep[n]; seen {
				continue
			}

			keep[n] = struct{}{}
			frontier = append(frontier, n)
		}
	}

	for len(frontier) > 0 {
		n := frontier[len(frontier)-1]
		frontier = frontier[:len(frontier)-1]

		for ref := range store[n.id].files[n.artifact].refs {
			r := node{id: ref, artifact: n.artifact}

			d, ok := store[ref]
			if !ok {
				dangling[r] = struct{}{}

				continue
			}

			// A directory that lacks the data file is a broken chain too, but
			// what it does hold stays: a pass never makes a broken chain worse.
			if !d.files[n.artifact].hasData {
				dangling[r] = struct{}{}
			}

			if _, seen := keep[r]; seen {
				continue
			}

			keep[r] = struct{}{}
			frontier = append(frontier, r)
		}
	}

	return keep, dangling
}

// dirsOf returns the builds that have at least one artifact in nodes.
func dirsOf(nodes map[node]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(nodes))
	for n := range nodes {
		out[n.id] = struct{}{}
	}

	return out
}

// verifyDisjoint re-reads the header of every kept (build, artifact) and fails
// if it is itself being taken, or references a directory in the collect set or
// an artifact being trimmed.
func verifyDisjoint(ctx context.Context, storeDir string, keep map[node]struct{}, collect map[string]struct{}, trim map[node]struct{}) error {
	for n := range keep {
		if err := ctx.Err(); err != nil {
			return err
		}

		if _, bad := collect[n.id]; bad {
			return fmt.Errorf("gc integrity check failed: build %s is both kept and collected", n.id)
		}

		if _, bad := trim[n]; bad {
			return fmt.Errorf("gc integrity check failed: %s is both kept and collected", n)
		}

		refs, err := readRefs(filepath.Join(storeDir, n.id), n.artifact)
		if err != nil {
			return err
		}

		for ref := range refs {
			if _, bad := collect[ref]; bad {
				return fmt.Errorf("gc integrity check failed: kept %s references collect-set build %s", n, ref)
			}

			r := node{id: ref, artifact: n.artifact}
			if _, bad := trim[r]; bad {
				return fmt.Errorf("gc integrity check failed: kept %s references %s, which this pass collects", n, r)
			}
		}
	}

	return nil
}

// stage renames every collected directory, and the files of every trimmed
// artifact, into the trash staging directory. The rename is O(1) per entry and
// same-filesystem, which is what lets the caller hold its build lock across the
// commit and release it before the much slower removal.
func stage(storeDir string, store map[string]*dirInfo, collect map[string]struct{}, trim map[node]struct{}) error {
	if len(collect) == 0 && len(trim) == 0 {
		return nil
	}

	trash := filepath.Join(storeDir, trashDirName)
	if err := os.MkdirAll(trash, 0o700); err != nil {
		return fmt.Errorf("failed to create gc trash dir: %w", err)
	}

	for id := range collect {
		src := filepath.Join(storeDir, id)

		if err := os.Rename(src, filepath.Join(trash, id)); err != nil {
			return fmt.Errorf("failed to stage %q for collection: %w", src, err)
		}
	}

	for n := range trim {
		dst := filepath.Join(trash, n.id+"."+n.artifact.dataName())
		if err := os.MkdirAll(dst, 0o700); err != nil {
			return fmt.Errorf("failed to create gc trash dir %q: %w", dst, err)
		}

		for _, name := range store[n.id].files[n.artifact].names {
			src := filepath.Join(storeDir, n.id, name)

			if err := os.Rename(src, filepath.Join(dst, name)); err != nil {
				return fmt.Errorf("failed to stage %q for collection: %w", src, err)
			}
		}
	}

	return nil
}

// PurgeTrash removes what the last Collect staged. It is safe to call at any
// time, from any state: the trash holds only directories a completed pass has
// already committed as unreachable. A run that dies before this is called
// leaves the space held but nothing broken, and the next Collect clears it.
func PurgeTrash(storeDir string) error {
	if err := os.RemoveAll(filepath.Join(storeDir, trashDirName)); err != nil {
		return fmt.Errorf("failed to remove gc trash dir: %w", err)
	}

	return nil
}

// indexEntry is the layer-hash index blob written by
// pkg/template/build/storage/cache.HashIndex.
type indexEntry struct {
	Template struct {
		BuildID string `json:"build_id"`
	} `json:"template"`
}

// pruneIndex removes build-cache index blobs that can no longer resolve to a
// usable cached layer: the directory is gone, is being collected, is losing one
// of its files, or is present but has a dangling reference somewhere in its own
// chain.
//
// The third case is an inference, and it is safe *here* in a way it would never
// be for deletion: the worst outcome of pruning a good entry is a cache miss
// and one rebuilt layer. It is also the repair path for a broken chain — the
// rebuild mints fresh dirs, the new build supersedes the broken one, and
// ordinary collection takes the broken one away. Leaving such an entry in place
// is the state the 2026-08-01 write-up called the worst of both: a cache hit
// onto a directory that faults.
func pruneIndex(buildCacheDir string, store map[string]*dirInfo, collect map[string]struct{}, trim map[node]struct{}, dryRun bool) (int, error) {
	if buildCacheDir == "" {
		return 0, nil
	}

	scopes, err := os.ReadDir(buildCacheDir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}

	if err != nil {
		return 0, fmt.Errorf("failed to read build cache dir %q: %w", buildCacheDir, err)
	}

	// Brokenness is evaluated against the store as it will be *after* this pass,
	// not as it is now: a directory the age floor spared may reference one that
	// is about to go, and would be broken the moment the pass commits. Judging
	// it on the pre-collection view leaves its index entry in place — a cache
	// hit onto a chain that faults, which is the exact state this prune exists
	// to prevent.
	broken := brokenDirs(store, collect, trim)
	trimmed := dirsOf(trim)
	pruned := 0

	for _, scope := range scopes {
		if !scope.IsDir() {
			continue
		}

		indexDir := filepath.Join(buildCacheDir, scope.Name(), "index")

		blobs, err := os.ReadDir(indexDir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}

		if err != nil {
			return pruned, fmt.Errorf("failed to read index dir %q: %w", indexDir, err)
		}

		for _, blob := range blobs {
			if blob.IsDir() {
				continue
			}

			path := filepath.Join(indexDir, blob.Name())

			target, err := indexTarget(path)
			if err != nil {
				return pruned, err
			}

			_, present := store[target]
			_, collected := collect[target]
			_, isTrimmed := trimmed[target]
			_, isBroken := broken[target]

			if present && !collected && !isTrimmed && !isBroken {
				continue
			}

			pruned++

			if dryRun {
				continue
			}

			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return pruned, fmt.Errorf("failed to prune index blob %q: %w", path, err)
			}
		}
	}

	return pruned, nil
}

// indexTarget reads the build ID an index blob resolves to. An unreadable or
// malformed blob resolves to "", which is never present in the store and is
// therefore pruned.
func indexTarget(path string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}

	if err != nil {
		return "", fmt.Errorf("failed to read index blob %q: %w", path, err)
	}

	var entry indexEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return "", nil
	}

	return entry.Template.BuildID, nil
}

// brokenDirs returns every directory with an artifact whose own transitive
// closure contains a dangling reference, evaluated against the store as it will
// be once the directories in collect and the artifacts in trim are gone. Callers
// judging the store after a pass want that view; passing nil for both gives the
// present one.
func brokenDirs(store map[string]*dirInfo, collect map[string]struct{}, trim map[node]struct{}) map[string]struct{} {
	// present reports whether a referenced artifact's data will still be there
	// when the pass has committed.
	present := func(n node) bool {
		d, ok := store[n.id]
		if !ok || !d.files[n.artifact].hasData {
			return false
		}

		if _, collected := collect[n.id]; collected {
			return false
		}

		_, trimmed := trim[n]

		return !trimmed
	}

	const (
		unknown = iota
		visiting
		intact
		broken
	)

	state := make(map[node]int, len(store)*len(artifacts))

	var walk func(n node) bool

	walk = func(n node) bool {
		switch state[n] {
		case broken:
			return true
		case intact, visiting:
			// A cycle is not evidence of breakage; treat the back-edge as
			// contributing nothing.
			return false
		}

		state[n] = visiting

		result := false

		for ref := range store[n.id].files[n.artifact].refs {
			r := node{id: ref, artifact: n.artifact}

			if !present(r) {
				result = true

				break
			}

			if walk(r) {
				result = true

				break
			}
		}

		if result {
			state[n] = broken
		} else {
			state[n] = intact
		}

		return result
	}

	out := make(map[string]struct{})

	for id := range store {
		for _, a := range artifacts {
			if walk(node{id: id, artifact: a}) {
				out[id] = struct{}{}
			}
		}
	}

	return out
}

// writeLedger records the run. Every run writes one, dry or not — the ledger is
// how an operator sees what a run would do before letting it run for real, and
// how they see what it did afterwards.
func writeLedger(ledgerRoot string, res *Result) error {
	if ledgerRoot == "" {
		return nil
	}

	dir := filepath.Join(ledgerRoot, ledgerDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("failed to create ledger dir %q: %w", dir, err)
	}

	name := fmt.Sprintf("%s-%s.json", res.StartedAt.UTC().Format("20060102T150405Z"), sanitize(res.Reason))
	if res.DryRun {
		name = "dryrun-" + name
	}

	path := filepath.Join(dir, name)

	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal ledger: %w", err)
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("failed to write ledger %q: %w", path, err)
	}

	res.LedgerPath = path

	return nil
}

func sanitize(s string) string {
	if s == "" {
		return "unspecified"
	}

	out := []rune(s)
	for i, r := range out {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			out[i] = '-'
		}
	}

	return string(out)
}
