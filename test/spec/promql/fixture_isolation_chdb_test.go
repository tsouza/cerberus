//go:build chdb

// fixture_isolation_chdb_test.go — regression pin for issue #1987.
package promql_spec_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/test/spec"
)

// isolationSample names a handful of round-trip fixtures whose only path
// to a green result, before #1987's per-fixture chDB isolation
// (test/spec/runner_chdb.go's OpenChDB), was reading a SIBLING
// fixture's leftover table in the process-shared chDB session:
//
//   - regex_name_matcher_underscored_matches_dotted and
//     info_regex_name_matcher_unions_info_metrics are the two fixtures
//     issue #1987 itself named — their seed only creates
//     otel_metrics_gauge, but the regex `__name__` matcher's lowering
//     unions every metrics table, so a bare, unseeded
//     otel_metrics_histogram / otel_metrics_exponential_histogram scan
//     failed with UNKNOWN_TABLE the moment nothing SIBLING had already
//     created those tables.
//   - histogram_count_basic, histogram_sum_basic,
//     scan_unions_histogram_sum_for_suffixed_metric round out the same
//     "queries a metrics table this fixture's own seed never declares"
//     class from the other direction (histogram/sum-only seeds whose
//     `_count`/`_sum`-suffixed selector also fans out onto gauge/sum).
//   - subquery_absent_missing_metric exercises the OTHER bug #1987's
//     isolation fix surfaced: the parity guard in parity_chdb.go treated
//     a genuinely empty seed as an error, which — before session
//     isolation — a sibling's leftover rows silently avoided tripping.
//   - pi_constant is a pure scalar fixture (no selector at all) that
//     depends on the SAME parity guard correctly special-casing a
//     selector-free query.
//
// Not exhaustive over the corpus (800+ fixtures would make this slow —
// see the issue's own suggested acceptance shape), but every entry here
// independently reproduced #1987 prior to the fix, which is what makes
// this sample a real regression pin rather than an arbitrary spot check.
var isolationSample = []string{
	"regex_name_matcher_underscored_matches_dotted",
	"info_regex_name_matcher_unions_info_metrics",
	"histogram_count_basic",
	"histogram_sum_basic",
	"scan_unions_histogram_sum_for_suffixed_metric",
	"subquery_absent_missing_metric",
	"pi_constant",
}

// TestFixtureIsolation_SampleRunsAlone pins #1987's own acceptance bar:
// "every fixture passes when run alone under
// `-run '^TestName$/^<name>$'`". It shells out to a NESTED `go test`
// invocation per sampled fixture — narrowed exactly the way CLAUDE.md
// invariant 5 mandates reproducing a red check — rather than just calling
// spec.RunRoundTrip in-process, because the whole point is to prove each
// fixture is self-sufficient in a process where NO sibling fixture has
// run first and left tables behind. Once each fixture is genuinely
// isolated (this repo's fix: spec.OpenChDB gives every fixture its own
// DATABASE on the shared session), running a fixture alone and running it
// as part of the full TestRoundTripChDB walk mean the same thing; a
// regression that reintroduces cross-fixture leakage (dropping that
// per-fixture database, or a fixture's seed drifting out of
// self-sufficiency) breaks exactly the isolated invocation this test
// drives, even though the whole-package walk would stay green.
func TestFixtureIsolation_SampleRunsAlone(t *testing.T) {
	for _, name := range isolationSample {
		name := name
		t.Run(name, func(t *testing.T) {
			pattern := "^TestRoundTripChDB$/^" + name + "$"
			cmd := exec.Command("go", "test", "-tags", "chdb", "-count=1", "-v", "-run", pattern, ".")
			cmd.Env = withoutCorpusShard(os.Environ())
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf(
					"fixture %s failed when run ALONE via -run %q — its seed is not "+
						"self-sufficient for its own query, so it was only ever passing "+
						"by reading a sibling fixture's leftover chDB table (#1987):\n%s",
					name, pattern, out,
				)
			}
			// A typo'd fixture name matches zero subtests and go test
			// still exits 0 ("ok", nothing run), which err == nil alone
			// would wave through as a pass that tested nothing. -v makes
			// Go print "=== RUN   TestRoundTripChDB/<name>" for the one
			// subtest that actually matched, so require it named this
			// exact fixture.
			if !strings.Contains(string(out), "=== RUN   TestRoundTripChDB/"+name) {
				t.Fatalf("fixture %s: -run %q matched no subtest (typo'd name?), output:\n%s", name, pattern, out)
			}
		})
	}
}

// withoutCorpusShard strips SPEC_SHARD_INDEX / SPEC_SHARD_COUNT from an
// environment.
//
// The nested `go test` above names ONE fixture and needs the whole corpus
// available to find it. Inherited unchanged, a parent that owns a corpus SLICE
// — CI's fanned-out `roundtrip (promql)` leg
// (.github/scripts/chdb-roundtrip.mjs) — hands the child its own partition,
// spec.WalkShard filters the named fixture out for two legs in three, and the
// child reports "matched no subtest" for a fixture that is not missing at all.
// The partition belongs to the walk that declared it, not to a subprocess
// asking a different question.
func withoutCorpusShard(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, spec.ShardIndexEnv+"=") || strings.HasPrefix(kv, spec.ShardCountEnv+"=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// TestWithoutCorpusShard pins the strip itself, because its failure mode is
// the quiet one: with the pair still in the child's environment the nested
// run reports a MISSING FIXTURE, which reads as a corpus problem rather than
// as an inherited partition, and the real message is lost.
func TestWithoutCorpusShard(t *testing.T) {
	t.Parallel()

	got := withoutCorpusShard([]string{
		"PATH=/usr/bin",
		spec.ShardIndexEnv + "=2",
		"GODEBUG=asyncpreemptoff=1",
		spec.ShardCountEnv + "=3",
	})
	want := []string{"PATH=/usr/bin", "GODEBUG=asyncpreemptoff=1"}
	if len(got) != len(want) {
		t.Fatalf("withoutCorpusShard() = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("withoutCorpusShard() = %q, want %q", got, want)
		}
	}
}
