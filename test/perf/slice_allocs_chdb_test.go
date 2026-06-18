//go:build chdb

// Perf guard for the copy-on-write off-spine sharing in plan slicing
// (`perf(solver): share immutable off-spine in plan slicing`). The slicing
// hot path -- (*Planner).slice() -> 1 unpinSpine + K chplan.ReanchorRange --
// used to re-anchor every shard by CloneNode-ing the ENTIRE off-spine subtree
// K+1 times, even though that subtree is byte-identical across all K shards
// (it does not move in time). The COW rewrite shares the immutable off-spine
// verbatim and clones only the O(spine-depth) re-gridded spine nodes, so the
// per-shard allocation count collapses from O(K x plan-size) toward
// O(K x spine-depth).
//
// `internal/solver/slicer_bench_test.go:BenchmarkSlice` measures the WALL-CLOCK
// win, but it lives in the WEEKLY informational `perf-benchmark.yml` lane,
// which "never gates" (its own header). So a future PR could revert the COW
// `default` arm in internal/chplan/reanchor.go back to a per-shard CloneNode --
// re-inflating the allocs to hundreds -- and NO required check would fail. This
// file closes that gap: a DETERMINISTIC allocation assertion-pin that runs in
// the `perf-guards` GATING job (`just perf-chdb` -> `go test -tags chdb
// ./test/perf/...`, a required check) and FAILS the PR if slice()'s allocations
// regress upward.
//
// # Why allocations, not wall-clock
//
// `testing.AllocsPerRun` is deterministic to the allocation (an exact integer,
// reproducible run-to-run -- verified across repeated runs and both K values),
// so it pins on a gating lane without the runner-variance flake that keeps the
// wall-clock benchmark informational. The slice() path is reached through the
// production entry `(*Planner).Plan` (slice() is unexported; Plan is the only
// caller and the only public seam), exactly as `solver_decision_ratchet_test.go`
// drives the Planner through its public API from this package.
//
// # The fixed plan + grid (a pure function of shape)
//
// The representative plan mirrors `sum by (job)(rate(http_requests_total[5m]))`
// -- the same shape `BenchmarkSlice` builds: a matrix RangeWindow spine over a
// Filter-over-Scan off-spine, wrapped in Aggregate + Project. It is classified
// on a fixed deterministic grid (start fixed wall-clock, range 6h, step 15s) so
// the routing decision -- and therefore the slice() call -- is a pure function
// of the shape. The produced shard count K is pinned EXACTLY by setting
// Config.MaxK (the grid is wide enough that K clamps to MaxK), so the two arms
// measure slice() at K=4 and K=16 -- the same K values BenchmarkSlice exercises
// at its top and bottom.
//
// # The assertion (don't-regress-upward, like TestSeriesFanout_ChDB)
//
// Each arm asserts `allocs <= PINNED_BOUND`, where the bound is the measured
// COW allocs/op plus a few allocs of slack -- tight enough that reverting the
// COW `default` arm in reanchor.go back to `CloneNode(n)` (the pre-COW per-shard
// deep copy) pushes the allocs PAST the bound and trips this guard, but loose
// enough that trivial unrelated churn a few allocs either way does not flake it.
// This is the same shape as the /series fan-in pin (TestSeriesFanout_ChDB):
// a deterministic ceiling that a regression re-inflating the count trips, not an
// exact-equal lockstep. Load-bearing proof (measured on this exact plan/grid):
//
//	          COW (shared off-spine)   reverted (per-shard CloneNode)   bound
//	K=4               38                          46                     44
//	K=16              88                         120                    100
//
// Reverting the COW arm fails BOTH arms (46 > 44, 120 > 100); the COW path
// passes both with headroom.
//
// The pin carries the `chdb` build tag only so it is COMPILED and RUN by the
// `perf-guards` lane's `go test -tags chdb ./test/perf/...` selection -- it is a
// pure-Go allocation pin and never touches chDB.
package perf

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/solver"
)

// sliceAllocsGrid is the fixed deterministic classification grid. The 6h range
// at a 15s step gives enough anchors that the produced K clamps to Config.MaxK,
// so each arm pins slice() at an EXACT K rather than at a grid-derived count.
var (
	sliceAllocsStart = time.Unix(1_700_000_000, 0).UTC()
	sliceAllocsStep  = 15 * time.Second
	sliceAllocsEnd   = sliceAllocsStart.Add(6 * time.Hour)
	sliceAllocsRange = 5 * time.Minute
)

// sliceAllocsPlan builds the representative single-spine plan -- the same
// `sum by (job)(rate(http_requests_total[5m]))` shape BenchmarkSlice builds: a
// matrix RangeWindow spine over a Filter-over-Scan off-spine, wrapped in an
// Aggregate + Project. The off-spine (Filter -> Scan) is what the COW `default`
// arm shares instead of cloning K+1 times.
func sliceAllocsPlan() chplan.Node {
	scan := &chplan.Scan{
		Table:   "otel_metrics_sum",
		Columns: []string{"MetricName", "Attributes", "TimeUnix", "Value", "ResourceAttributes", "ScopeName"},
	}
	filter := &chplan.Filter{
		Input: scan,
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "MetricName"},
			Right: &chplan.LitString{V: "http_requests_total"},
		},
	}
	rw := &chplan.RangeWindow{
		Input:           filter,
		Func:            "rate",
		Range:           sliceAllocsRange,
		Step:            sliceAllocsStep,
		OuterRange:      sliceAllocsEnd.Sub(sliceAllocsStart),
		Start:           sliceAllocsStart,
		End:             sliceAllocsEnd,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	agg := &chplan.Aggregate{
		Input:   rw,
		GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "job"}},
		AggFuncs: []chplan.AggFunc{
			{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "sum_value"},
		},
	}
	return &chplan.Project{
		Input: agg,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "job"}},
			{Expr: &chplan.ColumnRef{Name: "sum_value"}, Alias: "result"},
		},
	}
}

// TestSliceAllocs_ChDB pins the slice() allocation count at K=4 and K=16. It
// fails a PR if the copy-on-write off-spine sharing regresses back toward the
// pre-COW per-shard CloneNode (allocs jump past the pinned bound). See the file
// header for the load-bearing pass-vs-reverted numbers.
func TestSliceAllocs_ChDB(t *testing.T) {
	plan := sliceAllocsPlan()
	meta := solver.RequestMeta{
		Lang:  solver.LangPromQL,
		Start: sliceAllocsStart,
		End:   sliceAllocsEnd,
		Step:  sliceAllocsStep,
	}

	cases := []struct {
		k     int // produced shard count (== Config.MaxK on this wide grid).
		bound float64
	}{
		// COW measures 38 allocs/op (reverted-to-CloneNode measures 46); a
		// bound of 44 leaves 6 allocs of slack above COW yet trips the revert.
		{k: 4, bound: 44},
		// COW measures 88 allocs/op (reverted-to-CloneNode measures 120); a
		// bound of 100 leaves 12 allocs of slack above COW yet trips the revert.
		{k: 16, bound: 100},
	}

	for _, tc := range cases {
		cfg := solver.DefaultConfig()
		cfg.Mode = solver.ModeAuto
		cfg.MaxK = tc.k
		p := &solver.Planner{Cfg: cfg}

		// Sanity: the fixed plan/grid must actually route at the pinned K, or
		// the arm would be measuring the cheap not-routed path and silently
		// stop guarding slice(). A construction break (the plan stopped
		// routing, or the grid stopped clamping K to MaxK) fails loudly here.
		d, routed := p.Plan(plan, meta)
		if !routed {
			t.Fatalf("K=%d: plan no longer routes (reason=%q) -- the slice() guard "+
				"is measuring the not-routed path and no longer pins the COW win; "+
				"fix sliceAllocsPlan/grid so it routes at this K", tc.k, d.Reason)
		}
		if d.K != tc.k {
			t.Fatalf("K=%d: produced shard count is %d, want exactly %d -- the grid "+
				"no longer clamps K to Config.MaxK, so this arm is not pinning slice() "+
				"at the intended K; widen the grid or adjust MaxK", tc.k, d.K, tc.k)
		}

		allocs := testing.AllocsPerRun(100, func() {
			p.Plan(plan, meta)
		})

		// Don't-regress-upward (mirrors TestSeriesFanout_ChDB's n!=1 ceiling):
		// a regression that reverted the COW off-spine sharing back to a
		// per-shard CloneNode re-inflates slice()'s allocs/op past this bound
		// and trips here.
		if allocs > tc.bound {
			t.Fatalf("slice() allocation regression at K=%d: Plan->slice() allocated "+
				"%.0f allocs/op, want <= %.0f. The copy-on-write off-spine sharing "+
				"(internal/chplan/reanchor.go) shares the immutable off-spine subtree "+
				"across the K shards instead of CloneNode-ing it K+1 times; an allocs/op "+
				"above this bound means that sharing regressed (e.g. the `default` arm of "+
				"ReanchorRange went back to CloneNode(n)) and slice() is paying the pre-COW "+
				"per-shard deep copy again.", tc.k, allocs, tc.bound)
		}
		t.Logf("K=%d: slice() allocs/op=%.0f (bound %.0f)", tc.k, allocs, tc.bound)
	}
}
