// Shared shard store for the two committed perf ratchet baselines.
//
// # Why the baselines are directories
//
// Both baselines are generated rosters keyed by a fixture id: the cardinality
// ratchet stores one record per profiled fixture, the routing-decision ratchet
// one record per corpus query. Stored as a single flat JSON array they are the
// #1422 corruption shape by construction — two branches that each add a fixture
// insert records at nearby offsets, git's line merge shifts the record
// boundaries without reporting a conflict, and the blended file still parses
// with a plausible length while every record in the blended region pairs one
// fixture's name with the next one's numbers. `-merge` in .gitattributes turns
// that silent blend into a refusal, which is correct but expensive: the only
// resolution is a full regeneration, and the cardinality regeneration is a
// multi-minute chDB profile pass over the whole corpus.
//
// One record per FILE removes the conflict class rather than converting it. A
// change that adds, drops or moves a fixture touches exactly that fixture's
// shard, so two branches adding different fixtures write disjoint paths and
// merge with no interaction at all. `-merge` still covers the residual case —
// two branches that move the SAME record — because sharding relocates the
// hazard to shard granularity instead of removing it outright.
//
// # The key IS the path
//
// A shard's file path is derived from its record key, one path segment per
// key segment: cardinality-baseline/promql/rate_basic.json holds the record
// whose `fixture` is "promql/rate_basic". [baselineShards.load] asserts that
// correspondence on every read, which is an invariant a flat array could not
// express at all — in an array a record's position carries no information, so
// nothing could contradict its key.
//
// [baselineShards.write] PRUNES a shard whose record the generator no longer
// produces. Without that a removed fixture leaves its shard behind forever,
// and a stale shard is not inert: load would keep feeding the ratchet a record
// for a fixture the corpus does not contain, with the regenerate-and-diff
// discipline reporting no drift.
//
// # Errors, not t.Fatal
//
// The store's operations return errors and the [baselineShards.mustLoad] /
// [baselineShards.mustWrite] wrappers turn those into test failures. The split
// exists so the store's own refusals — a key that would need escaping, a record
// filed under a name that is not its own, a foreign file in the tree — can be
// ASSERTED by baseline_shards_roundtrip_test.go. A refusal expressed only as
// t.Fatal is unassertable, which would leave the checks that make this store
// safe as the one part of it nothing tests.

package perf

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// shardExt is the suffix every shard file carries; a shard tree holds nothing
// else, and load refuses a tree that does.
const shardExt = ".json"

// shardDirPerm / shardFilePerm are the modes write creates the tree with —
// owner-only, matching how the rest of test/perf writes its generated files.
const (
	shardDirPerm  fs.FileMode = 0o750
	shardFilePerm fs.FileMode = 0o600
)

// shardSegmentPattern constrains ONE path segment of a record key. The leading
// character is restricted further than the rest so a key can never produce a
// dot segment ("." / ".."), a hidden file, or a name a shell or tool would read
// as a flag — every one of which would make the key→path mapping ambiguous or
// unsafe. Keys are checked against this rather than assumed to comply: today's
// corpus ids all satisfy it, and the point of the check is to fail loudly on
// the first one that would need escaping instead of silently writing a shard
// somewhere unexpected.
var shardSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

// baselineShards is a directory of one-record-per-file JSON shards, standing in
// for what a single flat JSON array would otherwise hold. See the file doc for
// why the baselines are stored this way.
type baselineShards[T any] struct {
	// dir is the shard tree root, relative to this package directory.
	dir string
	// depth is how many path segments a record key maps to, and therefore
	// how deep the tree is. Pinning it means a key that gained or lost a
	// segment fails rather than quietly reshaping the tree.
	depth int
	// keyOf reads the record's own key field — the value that must equal
	// the shard's path.
	keyOf func(T) string
	// regen is the recipe that rewrites this tree, quoted verbatim in
	// failure messages so whoever hits one can read the fix off it.
	regen string
}

// pathFor maps a record key to its shard path, refusing a key that would need
// escaping to become one.
func (s baselineShards[T]) pathFor(key string) (string, error) {
	segs := strings.Split(key, "/")
	if len(segs) != s.depth {
		return "", fmt.Errorf("%s: key %q has %d path segment(s), want %d. The shard tree's depth is "+
			"fixed so a record key maps to exactly one path; a key of a different shape needs the "+
			"tree's depth changed deliberately, not a shard written at the wrong level",
			s.dir, key, len(segs), s.depth)
	}
	for _, seg := range segs {
		if !shardSegmentPattern.MatchString(seg) {
			return "", fmt.Errorf("%s: key %q has segment %q, which does not match %s. Shard paths are "+
				"derived from record keys verbatim, with no escaping — a key needing escaping must "+
				"either be renamed at its source or the mapping made explicit here",
				s.dir, key, seg, shardSegmentPattern)
		}
	}

	return filepath.Join(append([]string{s.dir}, segs...)...) + shardExt, nil
}

// keyFor is pathFor's inverse: the shard path relative to the tree root, minus
// the extension, read back as a record key.
func keyFor(rel string) string {
	return strings.TrimSuffix(filepath.ToSlash(rel), shardExt)
}

// load reads every shard in the tree into a key-indexed map, asserting that
// each record's own key field matches the path it was read from. Per-shard
// problems are accumulated rather than returned at the first one, so a tree
// that drifted in several places reports every site in one pass.
func (s baselineShards[T]) load() (map[string]T, error) {
	out := make(map[string]T)
	var bad []error

	err := filepath.WalkDir(s.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		rel, rerr := filepath.Rel(s.dir, path)
		if rerr != nil {
			return rerr
		}
		if !strings.HasSuffix(path, shardExt) {
			bad = append(bad, fmt.Errorf("%s: %s is not a %s shard. The tree is written entirely by %q "+
				"and holds nothing else; a stray file here is either a leftover or a hand edit that "+
				"the next regeneration would drop", s.dir, rel, shardExt, s.regen))

			return nil
		}

		buf, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		var entry T
		if uerr := json.Unmarshal(buf, &entry); uerr != nil {
			bad = append(bad, fmt.Errorf("%s: parse shard %s: %w", s.dir, rel, uerr))

			return nil
		}

		key := keyFor(rel)
		if got := s.keyOf(entry); got != key {
			bad = append(bad, fmt.Errorf("%s: shard %s holds a record keyed %q. A shard's path IS its "+
				"key, so this record is filed under a name that is not its own — regenerate with %q "+
				"rather than editing either half by hand", s.dir, rel, got, s.regen))

			return nil
		}
		if _, dup := out[key]; dup {
			bad = append(bad, fmt.Errorf("%s: key %q read twice from the tree", s.dir, key))

			return nil
		}
		out[key] = entry

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read shard tree %s: %w — run %q to create it", s.dir, err, s.regen)
	}
	if len(bad) > 0 {
		return nil, errors.Join(bad...)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s holds no %s shard. An empty baseline tree would make the ratchet "+
			"vacuous: every fixture would read as new rather than as measured against a committed "+
			"record", s.dir, shardExt)
	}

	return out, nil
}

// write rewrites the shard tree from the current generator output: one file per
// record, plus a prune pass that deletes the shards of records that no longer
// exist and the directories those leave empty.
func (s baselineShards[T]) write(entries map[string]T) error {
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	want := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		path, err := s.pathFor(key)
		if err != nil {
			return err
		}
		want[path] = struct{}{}

		if err := os.MkdirAll(filepath.Dir(path), shardDirPerm); err != nil {
			return fmt.Errorf("create shard directory for %s: %w", path, err)
		}
		buf, err := json.MarshalIndent(entries[key], "", "  ")
		if err != nil {
			return fmt.Errorf("marshal shard %s: %w", path, err)
		}
		buf = append(buf, '\n')
		if err := os.WriteFile(path, buf, shardFilePerm); err != nil {
			return fmt.Errorf("write shard %s: %w", path, err)
		}
	}

	return s.prune(want)
}

// prune deletes every shard the current generator output does not claim, then
// removes the directories that leaves empty. A stale shard is not inert — load
// would keep serving its record to the ratchet as if the fixture were still in
// the corpus — so a regeneration that only ADDS files is not a regeneration.
func (s baselineShards[T]) prune(want map[string]struct{}) error {
	var stale []string
	var dirs []string

	err := filepath.WalkDir(s.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != s.dir {
				dirs = append(dirs, path)
			}

			return nil
		}
		if _, ok := want[path]; ok {
			return nil
		}
		if !strings.HasSuffix(path, shardExt) {
			return fmt.Errorf("%s: refusing to prune %s — it is not a %s shard, so it was not written "+
				"by %q and deleting it is not this generator's call", s.dir, path, shardExt, s.regen)
		}
		stale = append(stale, path)

		return nil
	})
	if err != nil {
		return fmt.Errorf("walk shard tree %s: %w", s.dir, err)
	}

	for _, path := range stale {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("prune stale shard %s: %w", path, err)
		}
	}

	// Deepest first, so a directory emptied by pruning its children is itself
	// empty by the time this pass reaches it.
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, dir := range dirs {
		des, err := os.ReadDir(dir)
		if err != nil {
			return fmt.Errorf("read shard directory %s: %w", dir, err)
		}
		if len(des) > 0 {
			continue
		}
		if err := os.Remove(dir); err != nil {
			return fmt.Errorf("prune empty shard directory %s: %w", dir, err)
		}
	}

	return nil
}

// mustLoad is load for the ratchets, which have no recovery from a tree they
// cannot read.
func (s baselineShards[T]) mustLoad(t *testing.T) map[string]T {
	t.Helper()

	entries, err := s.load()
	if err != nil {
		t.Fatalf("%v", err)
	}

	return entries
}

// mustWrite is write for the ratchets' UPDATE_* path.
func (s baselineShards[T]) mustWrite(t *testing.T, entries map[string]T) {
	t.Helper()

	if err := s.write(entries); err != nil {
		t.Fatalf("%v", err)
	}
}
