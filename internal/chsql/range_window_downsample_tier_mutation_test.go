package chsql

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// This file pins range_window_downsample_tier.go's emission against the
// gremlins mutation lane (phase2-other, scope ./internal/chsql). Before it
// the file's only tests were `//go:build chdb` and
// `//go:build integration` roundtrips plus
// chplan_ir_discriminates_test.go, which asserts only that the tier plan and
// the raw plan emit DIFFERENT SQL. That combination makes the emitter's lines
// COVERED without pinning any token in them, which is exactly the state that
// leaves a mutant alive: nine survivors sat here, and thirteen further mutants
// in downsampleTierValueExprFrag's own arms were never executed at all because
// no untagged test called it. Every test below names the construct it defends.

// downsampleTierMutationPlan builds an emittable DownsampleTier RangeWindow.
// The grid is deliberately ten steps wide so the anchor count
// (End-Start)/Step + 1 is 11 — a value no arithmetic mutation of that
// expression reproduces.
func downsampleTierMutationPlan(fn string) *chplan.RangeWindow {
	const (
		windowRange = 10 * time.Minute
		gridStep    = time.Minute
	)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &chplan.RangeWindow{
		Input:               &chplan.Scan{Table: "otel_metrics_gauge"},
		DownsampleTierInput: &chplan.Scan{Table: "otel_metrics_gauge_downsampled"},
		DownsampleTier:      true,
		Func:                fn,
		Range:               windowRange,
		Step:                gridStep,
		Start:               start,
		End:                 start.Add(windowRange),
		TimestampColumn:     "TimeUnix",
		ValueColumn:         "Value",
		GroupBy:             []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
}

// TestDownsampleTierValueExprFrag_PerFunc pins the rendered value expression
// of every Func downsampleTierMinSamples accepts, byte for byte.
//
// Kills the CONDITIONALS_NEGATION mutant of
// range_window_downsample_tier.go:`if fn == "last_over_time"` and of
// range_window_downsample_tier.go:`if fn == "idelta"`: each negation routes a
// Func to a different arm, so at least two of the three cases below render the
// wrong expression.
//
// Kills the INVERT_NEGATIVES and ARITHMETIC_BASE mutants of every negative
// trailing-pair subscript the three arms pass to downsampleTierValFrag and
// downsampleTierTsFrag. Each is rendered literally, so `[-1]` becoming `[1]`
// (or `[-2]` becoming `[2]`) reads the array's FRONT instead of its trailing
// pair, and the expected strings below spell the sign. The subscripts are
// named in prose rather than cited because the same call repeats across the
// arms and no construct citation can pick one out.
//
// The irate case additionally pins downsampleTierIntervalSecondsFrag's two
// `toUnixTimestamp64Nano` subscripts and the nanosecond divisor, whose mutants
// were NOT COVERED before this test: nothing untagged called the irate arm.
func TestDownsampleTierValueExprFrag_PerFunc(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		fn   string
		want string
	}{
		{
			fn:   "last_over_time",
			want: "`downsample_tier_sorted`[-1].2",
		},
		{
			fn:   "idelta",
			want: "`downsample_tier_sorted`[-1].2 - `downsample_tier_sorted`[-2].2",
		},
		{
			fn: "irate",
			want: "if(`downsample_tier_temporality` = 1, `downsample_tier_sorted`[-1].2, " +
				"if(`downsample_tier_sorted`[-1].2 < `downsample_tier_sorted`[-2].2, " +
				"`downsample_tier_sorted`[-1].2, " +
				"`downsample_tier_sorted`[-1].2 - `downsample_tier_sorted`[-2].2)) / " +
				"((toUnixTimestamp64Nano(`downsample_tier_sorted`[-1].1) - " +
				"toUnixTimestamp64Nano(`downsample_tier_sorted`[-2].1)) / 1000000000)",
		},
	} {
		t.Run(tc.fn, func(t *testing.T) {
			t.Parallel()
			if got := renderFragToSQL(downsampleTierValueExprFrag(tc.fn)); got != tc.want {
				t.Errorf("downsampleTierValueExprFrag(%q) rendered\n  %s\nwant\n  %s", tc.fn, got, tc.want)
			}
		})
	}
}

// TestEmitRangeWindowDownsampleTier_AnchorCountAndProjection pins the two
// numbers the emitter computes itself and the conditional timestamp
// projection.
//
// Kills both ARITHMETIC_BASE mutants of
// range_window_downsample_tier.go:`numAnchors := r.End.Sub(r.Start).Nanoseconds()/stepNS + 1`.
// The plan spans ten one-minute steps, so the end-inclusive anchor count is
// 11 and reaches the SQL as sampleAnchorFanoutFrag's `least(11, …)` upper
// bound; `+ 1` becoming `- 1` renders `least(9, …)` and `/` becoming `*`
// renders a number in the 10^22 range that wraps.
//
// Kills the CONDITIONALS_NEGATION mutant of
// range_window_downsample_tier.go:`if r.TimestampColumn != RangeWindowAnchorAlias`:
// this plan's TimestampColumn is `TimeUnix`, which is not the anchor alias, so
// the anchor must be re-projected under it. The negation drops that projection
// and the request's own timestamp column disappears from the result shape.
//
// Kills the three CONDITIONALS_NEGATION mutants of the `if err != nil` guards
// that follow downsampleTierMinSamples, collectGroupByFrags and subqueryFrag.
// Each negation returns a nil error on the SUCCESS path, before emitSelect
// ever runs, so the emitter produces no statement at all — which is why this
// test asserts the four SQL levels are present rather than only that Emit
// returned no error.
func TestEmitRangeWindowDownsampleTier_AnchorCountAndProjection(t *testing.T) {
	t.Parallel()
	sql, _, err := Emit(context.Background(), downsampleTierMutationPlan("last_over_time"))
	if err != nil {
		t.Fatalf("Emit(downsample tier): %v", err)
	}
	for _, want := range []string{
		// The end-inclusive anchor count, as sampleAnchorFanoutFrag's bound.
		"least(11, ",
		// The conditional re-projection of the anchor under TimestampColumn.
		"`anchor_ts` AS `TimeUnix`",
		// The four levels: fan, merged, sorted, outer. Their presence is what
		// distinguishes "emitted the statement" from "returned early with a
		// nil error and emitted nothing".
		"arrayJoin(arrayMap(",
		"timeSeriesLastTwoSamplesMerge(`LastTwoSamples`)",
		"arraySort(x -> x.1, arrayFilter(",
		"WHERE length(`downsample_tier_sorted`) >= 1",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("emitted SQL is missing %q\nSQL: %s", want, sql)
		}
	}
}

// TestEmitRangeWindowDownsampleTier_RejectsUnsupportedFunc pins that a Func
// outside downsampleTierMinSamples' accepted set is rejected rather than
// emitted with a zero minimum-sample guard.
//
// Kills the CONDITIONALS_NEGATION mutant of the `if err != nil` guard below
// range_window_downsample_tier.go:`minSamples, err := downsampleTierMinSamples(r.Func)`
// from the other side: with the guard negated the ERROR path falls through,
// minSamples stays 0, and the emitter answers a statement plus a nil error for
// a Func it cannot compute. Both directions of that one mutant are pinned —
// this test for the error path, the projection test above for the success
// path — because either alone leaves the other half of the branch unasserted.
func TestEmitRangeWindowDownsampleTier_RejectsUnsupportedFunc(t *testing.T) {
	t.Parallel()
	sql, _, err := Emit(context.Background(), downsampleTierMutationPlan("sum_over_time"))
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Emit(downsample tier, sum_over_time) err = %v, want ErrUnsupported (SQL: %s)", err, sql)
	}
}
