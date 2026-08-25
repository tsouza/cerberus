package chsql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

func TestDeltaPrefixAnchorArrayEmitsAtMostOneConditionalEvent(t *testing.T) {
	t.Parallel()

	got := renderFragToSQL(deltaPrefixAnchorArrayFrag(
		Col("query_end"), Col("sample_ts"), Col("temporality"),
		int64(time.Minute), int64(5*time.Minute), 6,
	))

	for _, want := range []string{
		"arrayMap(i -> `query_end` - toIntervalNanosecond(i * 60000000000)",
		"+ if(`temporality` = 1 AND `sample_ts` <= `query_end` - toIntervalNanosecond(300000000000), 1, 0)",
		// prefixIndex's upper clamp is numAnchors-1 (5 for numAnchors=6): the
		// prefix bucket index space is [0, numAnchors), so the greatest valid
		// prefix-bucket index is one less than numAnchors. Pins both the
		// binary-minus shape (not numAnchors+1) and the operand order.
		"least(5, greatest(0,",
		// prefixIndex subtracts one more from the anchor-grid floor index than
		// the plain sample-fanout bound does — deltaPrefixAnchorArrayFrag keys
		// buckets by the anchor whose left edge is at or after ts, one anchor
		// earlier than the covering-window floor. Pins the trailing `- 1` (not
		// `+ 1`) that turns the floor index into the prefix-bucket index.
		"toInt64(60000000000)) < 0) + 1 - 1))",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("DELTA prefix anchor array missing %q\nSQL: %s", want, got)
		}
	}
	for _, forbidden := range []string{"arrayJoin", "arrayConcat", "tuple("} {
		if strings.Contains(got, forbidden) {
			t.Errorf("DELTA prefix anchor array contains suffix-fanout shape %q\nSQL: %s", forbidden, got)
		}
	}
}

// TestDeltaPrefixAggregateRawAnchorArrayRequiresDeltaTemporality pins the
// fix for a bug where deltaPrefixAggregateRawAnchorArrayFrag (the exact
// DELTA-prefix aggregate mechanism's matrix-path raw-remainder term)
// rendered with NO temporality gate at all — every CUMULATIVE sample on a
// mixed-temporality table produced an unused prefix-array entry, inflating
// the #2429 fanout row budget for no correctness benefit (only a genuinely
// DELTA series' reconstructed level is ever consumed, via
// deltaFirstValFrag). The sibling deltaPrefixAnchorArrayFrag has always
// carried this gate; this function must too.
func TestDeltaPrefixAggregateRawAnchorArrayRequiresDeltaTemporality(t *testing.T) {
	t.Parallel()

	got := renderFragToSQL(deltaPrefixAggregateRawAnchorArrayFrag(
		Col("query_end"), Col("sample_ts"), Col("temporality"),
		int64(time.Minute), int64(5*time.Minute), 6,
	))

	if !strings.Contains(got, "`temporality` = 1") {
		t.Errorf("aggregate-mechanism raw anchor array is missing the DELTA-temporality gate — every "+
			"CUMULATIVE sample would also fan into an unused prefix-array row\nSQL: %s", got)
	}
}

func TestDeltaPrefixSumDeduplicatesTimestampBeforeSumming(t *testing.T) {
	t.Parallel()

	got := renderFragToSQL(deltaPrefixSumFrag(Col("prefix_pairs")))
	for _, want := range []string{
		"arraySum(arrayMap(p -> tupleElement(p, 2)",
		"arrayReverse(arrayCompact(p -> tupleElement(p, 1), arrayReverse(`prefix_pairs`)))",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("DELTA prefix sum missing %q\nSQL: %s", want, got)
		}
	}
}

func TestExtrapolatedMatrixDeltaPrefixUsesOneScanAndWindowedLevels(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r := &chplan.RangeWindow{
		Input:             &chplan.Scan{Table: "otel_metrics_sum"},
		Func:              "rate",
		Range:             5 * time.Minute,
		Start:             start,
		End:               start.Add(10 * time.Minute),
		Step:              time.Minute,
		OuterRange:        10 * time.Minute,
		TimestampColumn:   "TimeUnix",
		ValueColumn:       "Value",
		TemporalityColumn: "AggregationTemporality",
		GroupBy:           []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}

	sql, _, err := Emit(context.Background(), r)
	if err != nil {
		t.Fatalf("Emit(rate matrix): %v", err)
	}
	for _, want := range []string{
		"arrayJoin(arrayConcat(arrayMap(i ->",
		"groupArrayIf((`TimeUnix`, `Value`), `TimeUnix` > `anchor_ts` - toIntervalNanosecond(300000000000))",
		"groupArrayIf((`TimeUnix`, `Value`), `TimeUnix` <= `anchor_ts` - toIntervalNanosecond(300000000000))",
		"sum(`delta_prefix_step`) OVER (PARTITION BY `Attributes` ORDER BY `anchor_ts`)",
		"if(temporality = 1, `delta_anchor_levels` + tupleElement(window_pairs[1], 2)",
		// The inner-scan pushdown widens the covering window by
		// (numAnchors-1)*stepNS — 10 anchors back at a 1-minute step, i.e.
		// 600000000000ns — before subtracting the range, so a DELTA prefix
		// bucket anchored at the earliest grid point still has its preceding
		// observation in view. Pins numAnchors-1 (not +1) and `*` (not `/`)
		// against stepNS.
		"toIntervalNanosecond(600000000000) - toIntervalNanosecond(300000000000))",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("temporality-aware matrix SQL missing %q\nSQL: %s", want, sql)
		}
	}
	for _, forbidden := range []string{
		"delta_running_level", "arrayCumSum", "delta_anchor_windows", "UNION ALL",
		"delta_event_anchors", "delta_event_kinds", "delta_anchor_kind",
	} {
		if strings.Contains(sql, forbidden) {
			t.Errorf("temporality-aware matrix SQL contains obsolete split cost %q\nSQL: %s", forbidden, sql)
		}
	}
	// #2429's rateWindowFanoutBoundedSourceFrag adds a SECOND, independently
	// LIMIT-bounded read of the same time-pushed-down scan to detect fanout
	// truncation cheaply (see its own doc comment for why a window-function
	// count on the FIRST read isn't a substitute — it forces full
	// materialisation of the whole LIMIT-bounded set, which alone can
	// exceed the memory cap for a real, wide-payload row before the guard
	// ever gets a chance to fire). This is NOT the retention-wide rescan
	// this test originally guarded against (#2240) — both reads share the
	// identical, already-bounded time window; neither reads the series'
	// full matched history. Two is the deliberate, minimum count for a
	// guard that must read a genuine second sample of the same bounded
	// data to stay both single-pass-safe and truncation-aware.
	if got := strings.Count(sql, "FROM `otel_metrics_sum`"); got != 2 {
		t.Errorf("temporality-aware matrix scans otel_metrics_sum %d times, want two (main + #2429 fanout-bound probe)\nSQL: %s", got, sql)
	}
}
