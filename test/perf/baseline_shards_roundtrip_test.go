// Direct coverage for the shard store the two perf ratchet baselines are kept
// in. The ratchets themselves only ever regenerate a tree whose fixture set is
// unchanged, so on their own they exercise the happy path and nothing else —
// the prune pass and every refusal would be untested code guarding the two
// artefacts a merge is most likely to corrupt.

package perf

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
