package promql

import (
	"sort"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestNativeGridVectorAggUnionFns_TracksTheGridSet pins the equality
// nativeGridVectorAggUnionFns's doc promises: the union shape admits exactly
// the aggregates the plain native grid folds, so an aggregate added to one
// set and not the other fails here instead of silently splitting the
// vocabulary (one lowering folding it natively while the other falls back to
// the exploded Aggregate).
func TestNativeGridVectorAggUnionFns_TracksTheGridSet(t *testing.T) {
	t.Parallel()

	names := func(set map[chplan.Fn]struct{}) []string {
		out := make([]string, 0, len(set))
		for fn := range set {
			out = append(out, string(fn))
		}
		sort.Strings(out)
		return out
	}
	grid, union := names(nativeGridVectorAggFns), names(nativeGridVectorAggUnionFns)
	if len(grid) == 0 {
		t.Fatal("nativeGridVectorAggFns is empty — the native grid vector-agg fold would never fire")
	}
	if !equalStrings(grid, union) {
		t.Fatalf("nativeGridVectorAggUnionFns %v != nativeGridVectorAggFns %v — the two sets must stay identical (see nativeGridVectorAggUnionFns's doc)", union, grid)
	}
}

// TestDistinctSampleRowsFuncs_IsEveryPromQLRangeFuncButTheRateFamily derives
// distinctSampleRowsFuncs from chplan's range-function vocabulary instead of
// restating it: the set must be every PromQL-reachable RangeWindow.Func
// except the extrapolating rate family (rate / increase / delta), which the
// set's own doc excludes by name because the stronger per-timestamp collapse
// of cerberus issue #1092 already governs them. A range function added to
// chplan without a decision here fails, as does one dropped from chplan but
// still listed.
func TestDistinctSampleRowsFuncs_IsEveryPromQLRangeFuncButTheRateFamily(t *testing.T) {
	t.Parallel()

	extrapolatingRateFamily := map[string]bool{"rate": true, "increase": true, "delta": true}
	var want []string
	for _, name := range chplan.RangeWindowFuncNames() {
		if chplan.IsPromQLRangeWindowFunc(name) && !extrapolatingRateFamily[name] {
			want = append(want, name)
		}
	}
	got := make([]string, 0, len(distinctSampleRowsFuncs))
	for name, declared := range distinctSampleRowsFuncs {
		if !declared {
			t.Fatalf("distinctSampleRowsFuncs[%q] = false — a member is either declared or absent, never listed as false", name)
		}
		got = append(got, name)
	}
	sort.Strings(got)
	if !equalStrings(got, want) {
		t.Fatalf("distinctSampleRowsFuncs = %v\nwant every PromQL range function but rate/increase/delta = %v", got, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
