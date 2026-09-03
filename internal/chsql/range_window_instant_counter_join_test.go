package chsql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestInstantCounterJoinAgreesWithHasJoin pins cerberus issue #3014 at the
// emission level: chplan.HasJoin's *RangeWindow arm must agree with
// emitWindowedArrayExtrapolated's OWN needsDeltaFirstLevel branch — not
// merely a plausible-looking proxy for it — for the instant shape
// (OuterRange == 0) default fallback (instantDeltaPrefixSource, taken
// whenever DeltaPrefixAggregateInput is nil or the operator has not opted
// into WithDeltaPrefixReadEnabled).
//
// Each case asserts BOTH sides of the agreement against the SAME plan:
// the SQL chsql.Emit actually renders (LEFT JOIN when the plan carries a
// GroupBy key, CROSS JOIN when it does not — instantDeltaPrefixSource's own
// branch), and chplan.HasJoin's verdict. A predicate that merely LOOKED
// right (e.g. checked TemporalityColumn alone, without the counter-Func
// gate, or fired for the matrix shape too) would diverge from one of these
// rows exactly the way #3014 itself describes three OTHER things
// diverging.
func TestInstantCounterJoinAgreesWithHasJoin(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(10 * time.Minute)

	newInstantRangeWindow := func(fn string, temporality, groupBy bool, outerRange time.Duration) *chplan.RangeWindow {
		r := &chplan.RangeWindow{
			Input:           &chplan.Scan{Table: "otel_metrics_sum"},
			Func:            fn,
			Range:           5 * time.Minute,
			End:             end,
			TimestampColumn: "TimeUnix",
			ValueColumn:     "Value",
		}
		if temporality {
			r.TemporalityColumn = "AggregationTemporality"
		}
		if groupBy {
			r.GroupBy = []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}}
		}
		if outerRange > 0 {
			r.OuterRange = outerRange
			r.Step = time.Minute
			r.Start = start
		}
		return r
	}

	cases := []struct {
		name string
		r    *chplan.RangeWindow
		// wantJoinToken is "" when the emitted SQL must contain NO join at
		// all; otherwise the exact SQL keyword instantDeltaPrefixSource's
		// GroupBy-vs-no-GroupBy branch must render.
		wantJoinToken string
		wantHasJoin   bool
	}{
		{
			name:          "instant rate() over temporality-projected counter, grouped: LEFT JOIN",
			r:             newInstantRangeWindow("rate", true, true, 0),
			wantJoinToken: "LEFT JOIN",
			wantHasJoin:   true,
		},
		{
			name:          "instant rate() over temporality-projected counter, ungrouped: CROSS JOIN",
			r:             newInstantRangeWindow("rate", true, false, 0),
			wantJoinToken: "CROSS JOIN",
			wantHasJoin:   true,
		},
		{
			name:          "instant increase() over temporality-projected counter, grouped: LEFT JOIN",
			r:             newInstantRangeWindow("increase", true, true, 0),
			wantJoinToken: "LEFT JOIN",
			wantHasJoin:   true,
		},
		{
			name:          "instant delta() over temporality-projected column: delta is not a counter, no JOIN",
			r:             newInstantRangeWindow("delta", true, true, 0),
			wantJoinToken: "",
			wantHasJoin:   false,
		},
		{
			name:          "instant rate() with no TemporalityColumn: no JOIN",
			r:             newInstantRangeWindow("rate", false, true, 0),
			wantJoinToken: "",
			wantHasJoin:   false,
		},
		{
			name: "matrix rate() (OuterRange > 0) over temporality-projected counter, no DeltaPrefixAggregateInput: " +
				"deltaMatrixLevelSource is a window function, not a JOIN",
			r:             newInstantRangeWindow("rate", true, true, 10*time.Minute),
			wantJoinToken: "",
			wantHasJoin:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sql, _, err := Emit(context.Background(), tc.r)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}

			gotJoin := strings.Contains(sql, "LEFT JOIN") || strings.Contains(sql, "CROSS JOIN")
			wantJoin := tc.wantJoinToken != ""
			if gotJoin != wantJoin {
				t.Errorf("emitted SQL join-presence = %v, want %v\nSQL: %s", gotJoin, wantJoin, sql)
			}
			if tc.wantJoinToken != "" && !strings.Contains(sql, tc.wantJoinToken) {
				t.Errorf("emitted SQL missing %q\nSQL: %s", tc.wantJoinToken, sql)
			}

			if got := chplan.HasJoin(tc.r); got != tc.wantHasJoin {
				t.Errorf("chplan.HasJoin = %v, want %v (disagrees with the emitted SQL's actual join-presence "+
					"of %v — this IS the #3014 gap if it reopens)", got, tc.wantHasJoin, gotJoin)
			}

			// The two assertions above must never disagree with EACH OTHER
			// either: HasJoin's whole job is predicting gotJoin.
			if tc.wantHasJoin != wantJoin {
				t.Fatalf("test bug: wantHasJoin (%v) and wantJoinToken-implied wantJoin (%v) disagree in the "+
					"table itself", tc.wantHasJoin, wantJoin)
			}
		})
	}
}
