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

func TestExtrapolatedMatrixDeltaPrefixUsesSingleAnchorAggregate(t *testing.T) {
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
		"AS `delta_window_anchors`",
		"AS `delta_prefix_anchors`",
		"ARRAY JOIN `delta_event_anchors` AS `anchor_ts`, `delta_event_kinds` AS `delta_anchor_kind`",
		"groupArrayIf((`TimeUnix`, `Value`), `delta_anchor_kind` = 0)",
		"groupArrayIf((`TimeUnix`, `Value`), `delta_anchor_kind` = 1)",
		"arrayCumSum(arrayMap(r -> tupleElement(r, 3), `delta_anchor_windows`))",
		"if(temporality = 1, `delta_anchor_levels` + tupleElement(window_pairs[1], 2)",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("temporality-aware matrix SQL missing %q\nSQL: %s", want, sql)
		}
	}
	for _, forbidden := range []string{"delta_running_level", " OVER (", "UNION ALL"} {
		if strings.Contains(sql, forbidden) {
			t.Errorf("temporality-aware matrix SQL contains obsolete split cost %q\nSQL: %s", forbidden, sql)
		}
	}
	if got := strings.Count(sql, "FROM `otel_metrics_sum`"); got != 1 {
		t.Errorf("temporality-aware matrix scans otel_metrics_sum %d times, want one scan\nSQL: %s", got, sql)
	}
}
