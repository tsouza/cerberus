package chsql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

func TestDeltaAnchorFanoutUsesOneContiguousGridRange(t *testing.T) {
	t.Parallel()

	got := renderFragToSQL(deltaAnchorFanoutFrag(
		Col("query_end"),
		Col("sample_ts"),
		Col("temporality"),
		int64(time.Minute),
		int64(5*time.Minute),
		6,
	))

	for _, want := range []string{
		"arrayJoin(arrayMap(i ->",
		"range(if(`temporality` = 1, 0, greatest(0,",
		"least(6,",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("DELTA anchor fanout missing %q\nSQL: %s", want, got)
		}
	}
	for _, forbidden := range []string{"arrayConcat", "tuple("} {
		if strings.Contains(got, forbidden) {
			t.Errorf("DELTA anchor fanout contains cumulative-path overhead %q\nSQL: %s", forbidden, got)
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
		"arraySort(groupArray((`TimeUnix`, `Value`))) AS `delta_anchor_pairs`",
		"arrayFilter(p -> tupleElement(p, 1) <= `anchor_ts` - toIntervalNanosecond(300000000000)",
		"arrayFilter(p -> tupleElement(p, 1) > `anchor_ts` - toIntervalNanosecond(300000000000)",
		"if(temporality = 1, arraySum(",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("temporality-aware matrix SQL missing %q\nSQL: %s", want, sql)
		}
	}
	for _, forbidden := range []string{"groupArrayIf", "delta_running_level", "UNION ALL"} {
		if strings.Contains(sql, forbidden) {
			t.Errorf("temporality-aware matrix SQL contains obsolete split cost %q\nSQL: %s", forbidden, sql)
		}
	}
	if got := strings.Count(sql, "FROM `otel_metrics_sum`"); got != 1 {
		t.Errorf("temporality-aware matrix scans otel_metrics_sum %d times, want 1\nSQL: %s", got, sql)
	}
}
