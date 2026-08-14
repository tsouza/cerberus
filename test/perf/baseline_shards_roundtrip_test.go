// Direct coverage for the shard store the two perf ratchet baselines are kept
// in. The ratchets themselves only ever regenerate a tree whose fixture set is
// unchanged, so on their own they exercise the happy path and nothing else —
// the prune pass and every refusal would be untested code guarding the two
// artefacts a merge is most likely to corrupt.

package perf

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tsouza/cerberus/test/spec"
)

// shardProbe stands in for the two real baseline record types: a key field plus
// one value, which is all [baselineShards] needs to see.
type shardProbe struct {
	Key   string `json:"key"`
	Value int    `json:"value"`
}

const probeRegen = "just update-probe-baseline"

func probeShards(dir string, depth int) baselineShards[shardProbe] {
	return baselineShards[shardProbe]{
		dir:   dir,
		depth: depth,
		keyOf: func(p shardProbe) string { return p.Key },
		regen: probeRegen,
	}
}

func probes(keys ...string) map[string]shardProbe {
	out := make(map[string]shardProbe, len(keys))
	for i, key := range keys {
		out[key] = shardProbe{Key: key, Value: i}
	}

	return out
}

// TestBaselineShardsRoundTrip pins that what write puts on disk is what load
// reads back, at both tree depths the real baselines use. A store that lost or
// reshaped a record here would leave the ratchet comparing against something
// other than the generator's own output.
func TestBaselineShardsRoundTrip(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		depth int
		keys  []string
	}{
		{"nested, one directory per head", 2, []string{"promql/rate_basic", "logql/json_parser", "traceql/span_attr"}},
		{"flat, one shard per query", 1, []string{"abs_metric", "sum_rate", "topk_5"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := probeShards(t.TempDir(), tc.depth)
			want := probes(tc.keys...)
			s.mustWrite(t, want)

			got := s.mustLoad(t)
			if len(got) != len(want) {
				t.Fatalf("loaded %d record(s), wrote %d", len(got), len(want))
			}
			for key, w := range want {
				g, ok := got[key]
				if !ok {
					t.Fatalf("record %q did not survive the round trip", key)
				}
				if g != w {
					t.Errorf("record %q round-tripped as %+v, wrote %+v", key, g, w)
				}
			}
		})
	}
}

// TestBaselineShardsWriteIsCanonicalJSON pins the on-disk encoding: two-space
// indent and a trailing newline, so a shard is byte-identical to `jq .` output
// and a regeneration produces no whitespace-only churn.
func TestBaselineShardsWriteIsCanonicalJSON(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	probeShards(dir, 1).mustWrite(t, probes("solo"))

	buf, err := os.ReadFile(filepath.Join(dir, "solo.json"))
	if err != nil {
		t.Fatalf("read shard: %v", err)
	}

	canonical, err := json.MarshalIndent(shardProbe{Key: "solo", Value: 0}, "", "  ")
	if err != nil {
		t.Fatalf("marshal reference: %v", err)
	}
	if want := string(canonical) + "\n"; string(buf) != want {
		t.Errorf("shard encoding is\n%q\nwant\n%q", buf, want)
	}
}

// TestBaselineShardsWritePrunesDroppedRecords is the reason write does a prune
// pass at all. A shard the generator no longer produces is not inert: load
// would keep serving its record to the ratchet as a fixture the corpus does not
// contain, and the regenerate-and-diff discipline would report no drift because
// nothing rewrote the file.
func TestBaselineShardsWritePrunesDroppedRecords(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := probeShards(dir, 2)
	s.mustWrite(t, probes("promql/kept", "promql/dropped", "logql/sole_member"))

	// logql/ loses its only record, so the directory itself must go too — an
	// empty directory left behind is a tree that no longer matches the corpus.
	s.mustWrite(t, probes("promql/kept"))

	if got := s.mustLoad(t); len(got) != 1 {
		t.Errorf("after pruning, tree holds %d record(s), want 1: %v", len(got), got)
	}
	for _, gone := range []string{"promql/dropped.json", "logql/sole_member.json", "logql"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Errorf("%s survived the prune (stat err: %v)", gone, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "promql", "kept.json")); err != nil {
		t.Errorf("prune removed a record the generator still produces: %v", err)
	}
}

// probeShardCount is the number of legs the sharded-write tests partition their
// probe corpus across. Four rather than two so a leg has several non-neighbour
// siblings — a prune scoped by a bug to "my shard plus the next one" still looks
// correct at two legs.
const probeShardCount = 4

// probeCorpus is a fixture id set big enough for every leg of probeShardCount
// to own several records, spread over two head directories so directory-level
// effects are visible.
func probeCorpus(n int) []string {
	out := make([]string, 0, 2*n)
	for i := range n {
		out = append(out, fmt.Sprintf("promql/fixture_%02d", i), fmt.Sprintf("logql/fixture_%02d", i))
	}

	return out
}

// treeFiles reads a shard tree into a path→bytes map, so two trees can be
// compared as the ARTEFACT rather than as a decoded roster. Directories are not
// entries: git tracks files, so an empty directory is not part of the artefact.
func treeFiles(t *testing.T, dir string) map[string]string {
	t.Helper()

	out := make(map[string]string)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		buf, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return rerr
		}
		out[filepath.ToSlash(rel)] = string(buf)

		return nil
	})
	if err != nil {
		t.Fatalf("read tree %s: %v", dir, err)
	}

	return out
}

// TestBaselineShardsShardedWriteLeavesOtherShardsAlone is the regression test
// for #2122's core hazard, and the reason the ratchet's update path no longer
// refuses a partial corpus.
//
// One leg regenerating its slice used to walk the WHOLE tree and delete every
// row it was not handed — so a sharded regeneration deleted the other legs'
// rows, produced a tree that still parsed, and landed in review as a large,
// plausible-looking "removed N fixtures" diff. A leg must now be unable to touch
// a path outside its own slice at all, whether that path is stale or current.
func TestBaselineShardsShardedWriteLeavesOtherShardsAlone(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := probeShards(dir, 2)
	corpus := probeCorpus(12)
	s.mustWrite(t, probes(corpus...))
	before := treeFiles(t, dir)

	// Leg 1 regenerates. Every other leg's rows — including ones leg 1 would
	// have called stale, because leg 1 did not profile them — must survive
	// untouched, byte for byte.
	leg := spec.Shard{Index: 1, Count: probeShardCount}
	mine := spec.FilterShardMap(leg, probes(corpus...))
	if len(mine) == 0 || len(mine) == len(corpus) {
		t.Fatalf("the probe corpus does not partition: leg 1 of %d holds %d of %d records",
			probeShardCount, len(mine), len(corpus))
	}
	s.mustWriteShard(t, mine, leg)

	after := treeFiles(t, dir)
	for path, want := range before {
		got, ok := after[path]
		if !ok {
			t.Errorf("%s was deleted by a write of the %v, which does not own every record", path, leg)

			continue
		}
		if got != want {
			t.Errorf("%s changed content under a write of the %v", path, leg)
		}
	}
	if len(after) != len(before) {
		t.Errorf("the tree holds %d file(s) after the sharded write, held %d before", len(after), len(before))
	}
}

// TestBaselineShardsShardedWritePrunesItsOwnStaleRecords is the other half:
// scoping the prune must not disable it. A leg still has to drop the rows of ITS
// OWN fixtures that the corpus no longer holds, or a fanned-out regeneration
// only ever adds — and a stale row the ratchet keeps honouring is the whole
// reason prune exists.
func TestBaselineShardsShardedWritePrunesItsOwnStaleRecords(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := probeShards(dir, 2)
	corpus := probeCorpus(12)
	s.mustWrite(t, probes(corpus...))

	leg := spec.Shard{Index: 1, Count: probeShardCount}
	var dropped string
	for _, key := range corpus {
		if leg.Holds(key) {
			dropped = key

			break
		}
	}
	if dropped == "" {
		t.Fatalf("the %v holds none of the %d probe records", leg, len(corpus))
	}

	kept := make([]string, 0, len(corpus)-1)
	for _, key := range corpus {
		if key != dropped {
			kept = append(kept, key)
		}
	}
	s.mustWriteShard(t, spec.FilterShardMap(leg, probes(kept...)), leg)

	path, err := s.pathFor(dropped)
	if err != nil {
		t.Fatalf("pathFor(%q): %v", dropped, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s survived a write of the leg that OWNS it (stat err: %v) — scoping the prune "+
			"must narrow its blast radius, not switch it off", dropped, err)
	}
	if got := s.mustLoad(t); len(got) != len(kept) {
		t.Errorf("tree holds %d record(s) after the sharded prune, want %d", len(got), len(kept))
	}
}

// TestBaselineShardsShardedWriteRefusesForeignRecords pins the misconfiguration
// guard. A leg handed a record outside its own slice means the generator
// filtered its corpus by a different partition than the one it declares — two
// legs then write the same file and judge it stale by different rules, and both
// still exit 0.
func TestBaselineShardsShardedWriteRefusesForeignRecords(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := probeShards(dir, 2)
	corpus := probeCorpus(12)
	s.mustWrite(t, probes(corpus...))

	leg := spec.Shard{Index: 1, Count: probeShardCount}
	var foreign string
	for _, key := range corpus {
		if !leg.Holds(key) {
			foreign = key

			break
		}
	}
	if foreign == "" {
		t.Fatalf("the %v holds every probe record, so no foreign key exists to test with", leg)
	}

	err := s.writeShard(probes(foreign), leg)
	if err == nil {
		t.Fatal("writeShard accepted a record the shard does not own; it must refuse")
	}
	if !strings.Contains(err.Error(), foreign) {
		t.Errorf("refusal reads %q, want it to name the record %q it does not own", err, foreign)
	}
}

// TestBaselineShardsShardedWriteMatchesTheWholeCorpusWrite is the correctness
// proof the fan-out rests on: N legs at count N, running CONCURRENTLY against
// one tree, produce the same artefact as the single serial pass they replace.
//
// Both trees start from a stale roster, so the comparison covers pruning as well
// as writing — the half a "sharded regeneration" gets wrong is the half that
// deletes.
//
// The legs run as real goroutines rather than in sequence to exercise the
// FILESYSTEM interleaving they meet in production: concurrent MkdirAll, WriteFile,
// Remove and WalkDir against one tree. That is the part `-race` cannot speak to —
// the legs share no mutable Go state (the store is a value with no field written,
// and each leg owns its own index of errs), so the detector has nothing to
// instrument here. Sequencing them would leave the interleaving untested until
// the first real regeneration.
func TestBaselineShardsShardedWriteMatchesTheWholeCorpusWrite(t *testing.T) {
	t.Parallel()

	stale := probeCorpus(16)
	fresh := probeCorpus(12)

	serialDir := t.TempDir()
	serial := probeShards(serialDir, 2)
	serial.mustWrite(t, probes(stale...))
	serial.mustWrite(t, probes(fresh...))

	shardedDir := t.TempDir()
	sharded := probeShards(shardedDir, 2)
	sharded.mustWrite(t, probes(stale...))

	errs := make([]error, probeShardCount)
	var wg sync.WaitGroup
	for i := range probeShardCount {
		leg := spec.Shard{Index: i + 1, Count: probeShardCount}
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = sharded.writeShard(spec.FilterShardMap(leg, probes(fresh...)), leg)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("leg %d of %d failed: %v", i+1, probeShardCount, err)
		}
	}

	want, got := treeFiles(t, serialDir), treeFiles(t, shardedDir)
	for path, w := range want {
		g, ok := got[path]
		if !ok {
			t.Errorf("%s is in the serial tree and missing from the sharded one", path)

			continue
		}
		if g != w {
			t.Errorf("%s differs between the serial and sharded trees:\nserial:  %q\nsharded: %q", path, w, g)
		}
	}
	for path := range got {
		if _, ok := want[path]; !ok {
			t.Errorf("%s is in the sharded tree and not in the serial one — a leg failed to prune it", path)
		}
	}
}

// TestBaselineShardsShardedWriteOwnsFilesAtUnEXPECTEDDepths pins the totality
// the fan-out's completeness rests on.
//
// A leg prunes a stale file only when it OWNS the file's key, so the argument
// that N legs together clear every stale file needs the partition to be total
// over whatever keys are on disk — not merely over the keys a generator would
// produce. `spec.ShardOf` hashes an arbitrary string, so a file at the wrong
// depth (a leftover from a tree whose shape changed, or a hand-dropped file) is
// still owned by exactly one leg. Were it owned by NONE, it would survive every
// sharded regeneration while a whole-corpus one removed it — a stale row the
// ratchet keeps honouring, visible only when somebody happened to run unsharded.
func TestBaselineShardsShardedWriteOwnsFilesAtUnexpectedDepths(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := probeShards(dir, 2)
	corpus := probeCorpus(8)
	s.mustWrite(t, probes(corpus...))

	// Depth 1 in a depth-2 tree: no key the generator emits maps here.
	stray := filepath.Join(dir, "stray.json")
	if err := os.WriteFile(stray, []byte("{}\n"), shardFilePerm); err != nil {
		t.Fatalf("plant stray shard: %v", err)
	}

	removedBy := 0
	for i := range probeShardCount {
		leg := spec.Shard{Index: i + 1, Count: probeShardCount}
		if err := s.writeShard(spec.FilterShardMap(leg, probes(corpus...)), leg); err != nil {
			t.Fatalf("leg %d of %d failed: %v", i+1, probeShardCount, err)
		}
		if _, err := os.Stat(stray); os.IsNotExist(err) && removedBy == 0 {
			removedBy = i + 1
		}
	}

	if removedBy == 0 {
		t.Errorf("a stray shard at an unexpected depth survived all %d legs — it is owned by no leg, "+
			"so no sharded regeneration can ever remove it", probeShardCount)
	}
	if want := spec.ShardOf(keyFor("stray.json"), probeShardCount); removedBy != want {
		t.Errorf("the stray was removed by leg %d, but its key is owned by leg %d — prune is not "+
			"scoping by the same partition the corpus is split on", removedBy, want)
	}
}

// TestBaselineShardsShardedWriteLeavesEmptyDirectories pins a DELIBERATE
// difference between the two write paths, so it cannot quietly become a racy
// directory removal instead.
//
// A whole-corpus write removes the directory a dropped record leaves empty. A
// leg running beside its siblings does not: the emptiness check and the removal
// cannot be made atomic against a sibling that is between MkdirAll and WriteFile
// in that directory. Leaving it is free — git tracks files, so an empty
// directory never reaches the committed artefact.
func TestBaselineShardsShardedWriteLeavesEmptyDirectories(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := probeShards(dir, 2)
	sole := "logql/sole_member"
	leg := spec.Shard{Index: spec.ShardOf(sole, probeShardCount), Count: probeShardCount}
	s.mustWrite(t, probes("promql/kept", sole))

	s.mustWriteShard(t, spec.FilterShardMap(leg, probes("promql/kept")), leg)

	if _, err := os.Stat(filepath.Join(dir, "logql", "sole_member.json")); !os.IsNotExist(err) {
		t.Errorf("the owning leg did not prune its own stale record (stat err: %v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "logql")); err != nil {
		t.Errorf("a sharded write removed the emptied directory: %v — removal races a sibling "+
			"writing into it, and an empty directory is not part of the committed artefact", err)
	}

	// The next whole-corpus write is what clears it.
	s.mustWrite(t, probes("promql/kept"))
	if _, err := os.Stat(filepath.Join(dir, "logql")); !os.IsNotExist(err) {
		t.Errorf("a whole-corpus write left the emptied directory behind (stat err: %v)", err)
	}
}

// TestBaselineShardsPruneRefusesForeignFiles pins that prune's blast radius is
// the shards it wrote. A tree holding a file from somewhere else is a situation
// to report, not one to clean up silently.
func TestBaselineShardsPruneRefusesForeignFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := probeShards(dir, 1)
	s.mustWrite(t, probes("kept"))

	foreign := filepath.Join(dir, "NOTES.md")
	if err := os.WriteFile(foreign, []byte("not a shard\n"), shardFilePerm); err != nil {
		t.Fatalf("plant foreign file: %v", err)
	}

	err := s.write(probes("kept"))
	if err == nil {
		t.Fatal("write accepted a non-shard file in the tree; prune must refuse rather than delete it")
	}
	if !strings.Contains(err.Error(), "refusing to prune") {
		t.Errorf("refusal reads %q, want it to name the prune refusal", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("prune deleted a file it did not write: %v", err)
	}
}

// TestBaselineShardsRejectKeysThatWouldNeedEscaping pins pathFor's charset and
// depth checks. Each of these keys would otherwise map to a path outside the
// tree, to a hidden or dot entry, or to the wrong level of it — the failure
// modes that make "the key IS the path" worth asserting rather than assuming.
func TestBaselineShardsRejectKeysThatWouldNeedEscaping(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		key  string
	}{
		{"parent traversal", "promql/.."},
		{"current directory", "promql/."},
		{"hidden entry", "promql/.hidden"},
		{"leading dash reads as a flag", "promql/-rf"},
		{"whitespace inside a segment", "promql/a b"},
		{"too few segments", "rate_basic"},
		{"too many segments", "promql/sub/rate_basic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := probeShards(t.TempDir(), 2)
			if _, err := s.pathFor(tc.key); err == nil {
				t.Fatalf("pathFor(%q) produced a path; a key that needs escaping must be refused", tc.key)
			}
			if err := s.write(probes(tc.key)); err == nil {
				t.Fatalf("write accepted key %q", tc.key)
			}
		})
	}
}

// TestBaselineShardsLoadRejectsMisfiledRecords is the invariant a flat array
// could not state: in an array a record's position carries no information, so
// nothing could contradict its key. Here the path is a second, independent
// statement of the same fact, and the two disagreeing means the tree drifted.
func TestBaselineShardsLoadRejectsMisfiledRecords(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := probeShards(dir, 2)
	s.mustWrite(t, probes("promql/rate_basic"))

	misfiled, err := json.MarshalIndent(shardProbe{Key: "promql/something_else", Value: 0}, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "promql", "rate_basic.json"), append(misfiled, '\n'), shardFilePerm); err != nil {
		t.Fatalf("write misfiled shard: %v", err)
	}

	loadErr := errFromLoad(t, s)
	if !strings.Contains(loadErr.Error(), "promql/something_else") {
		t.Errorf("refusal reads %q, want it to name the record's own key", loadErr)
	}
	if !strings.Contains(loadErr.Error(), probeRegen) {
		t.Errorf("refusal reads %q, want it to name the regeneration recipe", loadErr)
	}
}

// TestBaselineShardsLoadRejectsEmptyTree pins that an empty tree is a failure
// rather than an empty result. A ratchet handed zero records compares every
// fixture against nothing and passes, which is the exact shape of a gate that
// reports green while measuring nothing.
func TestBaselineShardsLoadRejectsEmptyTree(t *testing.T) {
	t.Parallel()

	s := probeShards(t.TempDir(), 1)
	if err := errFromLoad(t, s); !strings.Contains(err.Error(), "holds no") {
		t.Errorf("refusal reads %q, want it to name the tree as empty", err)
	}
}

// errFromLoad returns the error load refused with, failing the test if it did
// not refuse at all.
func errFromLoad(t *testing.T, s baselineShards[shardProbe]) error {
	t.Helper()

	got, err := s.load()
	if err == nil {
		t.Fatalf("load accepted the tree, returning %d record(s); it must refuse", len(got))
	}

	return err
}
