package chaudit

import "testing"

// TestCostUnits_MatchesTheEmittedGuardModel pins this package's copy of the
// density-guard cost model against the emitter's.
//
// internal/chsql emits `(series x anchors) + (rawRows x width^2)` as SQL for
// ClickHouse to evaluate at query time; this package evaluates the same
// formula in Go, because an audit has no query to attach a throwIf to. That
// duplication is deliberate and bounded, but it can drift silently — an audit
// reporting healthy headroom under a model the engine no longer uses is worse
// than no audit, because it is believed.
//
// The cases below are hand-computed rather than derived from the function, so
// a change to costUnits fails here instead of quietly redefining truth. The
// last row is the real production shape from #2677 (271 series, width 68,
// ~84k rows, a 6h grid at 60s), whose value must stay recognisable against the
// ~422M figure that incident was diagnosed with.
func TestCostUnits_MatchesTheEmittedGuardModel(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name                            string
		series, anchors, rawRows, width int64
		want                            int64
	}{
		{"both terms contribute", 10, 100, 50, 4, 10*100 + 50*16},
		{"width dominates quadratically", 1, 1, 1, 100, 1 + 10_000},
		{"no rows leaves only the grid term", 253, 360, 0, 68, 253 * 360},
		{"production shape from #2677", 271, 361, 83_997, 68, 271*361 + 83_997*68*68},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := costUnits(tc.series, tc.anchors, tc.rawRows, tc.width); got != tc.want {
				t.Errorf("costUnits(%d, %d, %d, %d) = %d, want %d — this package's model has drifted\n"+
					"from internal/chsql's emitted guard, so the audit is measuring against a\n"+
					"ceiling the engine does not enforce",
					tc.series, tc.anchors, tc.rawRows, tc.width, got, tc.want)
			}
		})
	}
}

// TestHeadroomPct_GoesNegativeExactlyWhenRejected pins the number an operator
// would alert on against the predicate the engine would apply, so a metric
// cannot read as having headroom while being over budget.
func TestHeadroomPct_GoesNegativeExactlyWhenRejected(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ cost, budget int64 }{
		{99, 100}, {100, 100}, {101, 100}, {1, 1_000_000}, {2_000_000, 1_000_000},
	} {
		m := MetricAudit{CostUnits: tc.cost, Budget: tc.budget, HeadroomPct: headroomPct(tc.cost, tc.budget)}
		if got, want := m.HeadroomPct < 0, m.OverBudget(); got != want {
			t.Errorf("cost=%d budget=%d: headroom<0 is %v but OverBudget is %v — the reported\n"+
				"number and the verdict disagree", tc.cost, tc.budget, got, want)
		}
	}
}
