package chaudit

import "testing"

// TestCostUnits_MatchesTheEmittedGuardModel pins this package's copy of the
// density-guard cost model against the emitter's.
//
// internal/chsql emits `(groups x anchors) + (rawRows x width^2)` as SQL for
// ClickHouse to evaluate at query time; this package evaluates the same
// formula in Go, because an audit has no query to attach a throwIf to. That
// duplication is deliberate and bounded, but it can drift silently — an audit
// reporting healthy headroom under a model the engine no longer uses is worse
// than no audit, because it is believed.
//
// THE EXPECTED VALUES ARE DECIMAL LITERALS, not expressions. An earlier version
// wrote them as `271*361 + 83_997*68*68` — costUnits' own body with the
// operands substituted — so the test asserted the function against itself and
// passed while the model was WRONG: it read the guard's `groups` factor as
// `series`, when the emitter groups by the query's keys AND the `le` rung
// (range_bucket_grid_native_bound.go's probeGroups.GroupBy(keyCols...,
// Col(bucketGridLeAlias))). A literal cannot be rewritten by the same edit that
// rewrites the code.
//
// The rung sensitivity is asserted directly below, because that is the specific
// drift this package already suffered.
func TestCostUnits_MatchesTheEmittedGuardModel(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name                            string
		series, anchors, rawRows, width int64
		want                            int64
	}{
		// 10 series x 4 rungs x 100 anchors = 4,000; 50 rows x 4^2 = 800.
		{"both terms contribute", 10, 100, 50, 4, 4_800},
		// 1 x 100 x 1 = 100; 1 x 100^2 = 10,000.
		{"width drives both terms", 1, 1, 1, 100, 10_100},
		// 253 x 68 x 360 = 6,193,440; no rows, so no second term.
		{"no rows leaves only the grid term", 253, 360, 0, 68, 6_193_440},
		// The real production shape from #2677: 271 series, width 68, ~84k
		// rows, a 6h grid at 60s. 271 x 68 x 361 = 6,652,508;
		// 83,997 x 68^2 = 388,402,128.
		{"production shape from #2677", 271, 361, 83_997, 68, 395_054_636},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := costUnits(tc.series, tc.anchors, tc.rawRows, tc.width); got != tc.want {
				t.Errorf("costUnits(%d, %d, %d, %d) = %d, want %d",
					tc.series, tc.anchors, tc.rawRows, tc.width, got, tc.want)
			}
		})
	}
}

// TestCostUnits_GridTermScalesWithRungCount is the regression this package
// actually had: the grid term read `groups` as `series`, dropping the `le`-rung
// factor entirely and understating cost by the rung count (12-16 on the
// calibrated production shapes).
//
// With rawRows at zero the second term vanishes, so the grid term is observed
// alone. Under the old model it was `series x anchors` — independent of width —
// and both cases below would have returned the same number. Any future edit
// that drops the rung factor fails here rather than silently making the audit
// optimistic again.
func TestCostUnits_GridTermScalesWithRungCount(t *testing.T) {
	t.Parallel()

	const series, anchors = 100, 360
	narrow := costUnits(series, anchors, 0, 8)
	wide := costUnits(series, anchors, 0, 16)

	if narrow == wide {
		t.Fatalf("the grid term ignored the rung count: width 8 and width 16 both cost %d.\n"+
			"The emitter groups by (keys, le), so `groups` is series x rungs — reading it as\n"+
			"series alone reports headroom on metrics the engine rejects", narrow)
	}
	if wide != narrow*2 {
		t.Errorf("doubling the rungs must double the grid term: width 8 = %d, width 16 = %d", narrow, wide)
	}
}
