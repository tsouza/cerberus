package chsql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// The matrix-shape MetricsAggregate emitters
// (emitRangeWindowMetrics, emitRangeWindowMetricsQuantileBuckets,
// emitMetricsExemplars) pin the inner otel_traces scan to the
// (Start - range, End] window via maybePushInnerScanTimeBounds so
// ClickHouse can prune partitions / granules by the Timestamp key.
// The pushdown is gated on BOTH Start AND End being set — the PromQL
// subquery-internal RangeWindow shapes (only Range / Step / OuterRange,
// no explicit grid) rely on the bounds being absent to keep the
// emitter output byte-stable against pinned snapshots.
//
// The tests below pin both halves of the gate:
//
//   - Start AND End set → WHERE clause MUST carry the `<tsCol> >
//     toDateTime64(...)` / `<tsCol> <= toDateTime64(...)` pair.
//   - Either Start or End zero → WHERE clause MUST NOT carry that pair
//     (so OuterRange-only PromQL shapes stay byte-stable).
//
// These coverage shapes kill the INVERT_LOGICAL mutant on the
// `Start.IsZero() || End.IsZero()` short-circuit (flip the `||` to
// `&&` and Start=zero+End=set emits the WHERE clause that the original
// suppressed).

// pushdownLowerSubstr is the load-bearing substring the matrix-shape
// emitters render for the lower half of the inner-scan time pushdown.
// It is structurally distinct from the outer per-anchor WHERE
// (`ts > anchor_ts - ...`) because the inner pushdown references the
// scan's actual timestamp column (`Timestamp`) rather than the inner
// `ts` alias — so a substring hit uniquely identifies the pushdown.
const (
	pushdownLowerSubstr = "`Timestamp` > toDateTime64("
	pushdownUpperSubstr = "`Timestamp` <= toDateTime64("
)

func TestRangeWindowMetricsInnerScanPushdown_BothSet(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC)

	plan := &chplan.RangeWindow{
		Input: &chplan.MetricsAggregate{
			Op:         chplan.MetricsOpRate,
			ValueAlias: "Value",
			Inner:      &chplan.Scan{Table: "otel_traces"},
		},
		Step:            time.Minute,
		Range:           time.Minute,
		Start:           start,
		End:             end,
		TimestampColumn: "Timestamp",
	}

	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, pushdownLowerSubstr) {
		t.Errorf("expected inner-scan lower-bound pushdown %q in SQL=%s", pushdownLowerSubstr, sql)
	}
	if !strings.Contains(sql, pushdownUpperSubstr) {
		t.Errorf("expected inner-scan upper-bound pushdown %q in SQL=%s", pushdownUpperSubstr, sql)
	}
}

// TestRangeWindowMetricsInnerScanPushdown_OnlyOneSet pins the gate's
// short-circuit: with only one of Start/End set the inner-scan WHERE
// pushdown MUST be suppressed (so the PromQL subquery-internal shapes
// — OuterRange-only — stay byte-stable). Kills the INVERT_LOGICAL
// mutant on `Start.IsZero() || End.IsZero()` (flip to `&&` and the
// pushdown leaks into the SQL with a `now64(9)` half-bound).
func TestRangeWindowMetricsInnerScanPushdown_OnlyOneSet(t *testing.T) {
	t.Parallel()

	nonZero := time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC)

	cases := []struct {
		name  string
		start time.Time
		end   time.Time
	}{
		{name: "start_only", start: nonZero, end: time.Time{}},
		{name: "end_only", start: time.Time{}, end: nonZero},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			plan := &chplan.RangeWindow{
				Input: &chplan.MetricsAggregate{
					Op:         chplan.MetricsOpRate,
					ValueAlias: "Value",
					Inner:      &chplan.Scan{Table: "otel_traces"},
				},
				Step:            time.Minute,
				Range:           time.Minute,
				Start:           c.start,
				End:             c.end,
				OuterRange:      5 * time.Minute,
				TimestampColumn: "Timestamp",
			}
			sql, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			if strings.Contains(sql, pushdownLowerSubstr) {
				t.Errorf("inner-scan lower-bound pushdown leaked with only %s set; SQL=%s", c.name, sql)
			}
			if strings.Contains(sql, pushdownUpperSubstr) {
				t.Errorf("inner-scan upper-bound pushdown leaked with only %s set; SQL=%s", c.name, sql)
			}
		})
	}
}

// TestPromQLMatrixInnerScanPushdown_OffsetAware pins #93: the PromQL
// matrix emitters (here the extrapolated rate path via a bare
// RangeWindow with a Scan input) push the offset-shifted
// (Start - Offset - range, End - Offset] inner-scan bound. Offset
// enters with its sign — a positive offset subtracts a positive
// interval from both edges; a negative offset subtracts a negative
// interval (CH folds `End - toIntervalNanosecond(-N)` to `End + N`),
// widening the upper bound to the RIGHT past End. Offset == 0 reduces
// to the bare `> toDateTime64(...)` / `<= toDateTime64(...)` pair the
// Tempo-path tests above already pin.
func TestPromQLMatrixInnerScanPushdown_OffsetAware(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC)

	cases := []struct {
		name      string
		offset    time.Duration
		wantShift string // the offset interval the bound must subtract
	}{
		{name: "positive_offset", offset: 2 * time.Minute, wantShift: "toIntervalNanosecond(120000000000)"},
		{name: "negative_offset", offset: -3 * time.Minute, wantShift: "toIntervalNanosecond(-180000000000)"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			plan := &chplan.RangeWindow{
				Input:           &chplan.Scan{Table: "otel_metrics_sum"},
				Func:            "rate",
				Step:            30 * time.Second,
				Range:           5 * time.Minute,
				OuterRange:      5 * time.Minute,
				Offset:          c.offset,
				Start:           start,
				End:             end,
				TimestampColumn: "TimeUnix",
				ValueColumn:     "Value",
			}
			sql, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			// The offset-shifted edges render as
			// `(toDateTime64(...) - toIntervalNanosecond(<offsetNS>))`,
			// so the offset interval must appear in the pushdown WHERE.
			if !strings.Contains(sql, c.wantShift) {
				t.Errorf("expected offset-shifted bound carrying %q in SQL=%s", c.wantShift, sql)
			}
			// The lower bound still subtracts the range (300000000000ns)
			// from the offset-shifted Start; the upper bound carries no
			// range term. Both reference the scan's TimeUnix column.
			if !strings.Contains(sql, "`TimeUnix` > ") {
				t.Errorf("expected lower-bound pushdown on TimeUnix in SQL=%s", sql)
			}
			if !strings.Contains(sql, "`TimeUnix` <= ") {
				t.Errorf("expected upper-bound pushdown on TimeUnix in SQL=%s", sql)
			}
		})
	}
}

// TestRangeWindowMetricsQuantileBucketsInnerScanPushdown_BothSet pins
// the matrix-quantile path's pushdown: with Start AND End set the
// inner SELECT carries the (Start - range, End] WHERE.
func TestRangeWindowMetricsQuantileBucketsInnerScanPushdown_BothSet(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC)

	plan := &chplan.RangeWindow{
		Input: &chplan.MetricsAggregate{
			Op:         chplan.MetricsOpQuantileOverTime,
			Attr:       &chplan.ColumnRef{Name: "Duration"},
			Quantiles:  []float64{0.95},
			ValueAlias: "Value",
			Inner:      &chplan.Scan{Table: "otel_traces"},
		},
		Step:            time.Minute,
		Range:           time.Minute,
		Start:           start,
		End:             end,
		TimestampColumn: "Timestamp",
	}

	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, pushdownLowerSubstr) {
		t.Errorf("expected lower-bound pushdown %q in SQL=%s", pushdownLowerSubstr, sql)
	}
	if !strings.Contains(sql, pushdownUpperSubstr) {
		t.Errorf("expected upper-bound pushdown %q in SQL=%s", pushdownUpperSubstr, sql)
	}
}

// TestRangeWindowMetricsQuantileBucketsInnerScanPushdown_OnlyOneSet
// mirrors the rate path's gate-short-circuit test for the
// quantile-bucket emitter.
func TestRangeWindowMetricsQuantileBucketsInnerScanPushdown_OnlyOneSet(t *testing.T) {
	t.Parallel()

	nonZero := time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC)

	cases := []struct {
		name  string
		start time.Time
		end   time.Time
	}{
		{name: "start_only", start: nonZero, end: time.Time{}},
		{name: "end_only", start: time.Time{}, end: nonZero},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			plan := &chplan.RangeWindow{
				Input: &chplan.MetricsAggregate{
					Op:         chplan.MetricsOpQuantileOverTime,
					Attr:       &chplan.ColumnRef{Name: "Duration"},
					Quantiles:  []float64{0.95},
					ValueAlias: "Value",
					Inner:      &chplan.Scan{Table: "otel_traces"},
				},
				Step:            time.Minute,
				Range:           time.Minute,
				Start:           c.start,
				End:             c.end,
				OuterRange:      5 * time.Minute,
				TimestampColumn: "Timestamp",
			}
			sql, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			if strings.Contains(sql, pushdownLowerSubstr) {
				t.Errorf("lower-bound pushdown leaked with only %s set; SQL=%s", c.name, sql)
			}
			if strings.Contains(sql, pushdownUpperSubstr) {
				t.Errorf("upper-bound pushdown leaked with only %s set; SQL=%s", c.name, sql)
			}
		})
	}
}

// TestEmitMetricsExemplarsInnerScanPushdown_BothSet pins the same
// (Start - range, End] pushdown contract for the exemplar emitter.
func TestEmitMetricsExemplarsInnerScanPushdown_BothSet(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC)

	m := &chplan.MetricsAggregate{
		Op:         chplan.MetricsOpRate,
		ValueAlias: "Value",
		Inner:      &chplan.Scan{Table: "otel_traces"},
	}
	rw := &chplan.RangeWindow{
		Input:           m,
		Step:            time.Minute,
		Range:           time.Minute,
		Start:           start,
		End:             end,
		TimestampColumn: "Timestamp",
	}

	sql, _, err := chsql.EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 1)
	if err != nil {
		t.Fatalf("EmitMetricsExemplars: %v", err)
	}
	if !strings.Contains(sql, pushdownLowerSubstr) {
		t.Errorf("expected exemplar lower-bound pushdown %q in SQL=%s", pushdownLowerSubstr, sql)
	}
	if !strings.Contains(sql, pushdownUpperSubstr) {
		t.Errorf("expected exemplar upper-bound pushdown %q in SQL=%s", pushdownUpperSubstr, sql)
	}
}

// TestEmitMetricsExemplarsInnerScanPushdown_OnlyOneSet mirrors the
// matrix-path gate test for the exemplar emitter. With only one of
// Start/End set the WHERE pushdown MUST be suppressed — kills the
// INVERT_LOGICAL mutant on the gate.
//
// Note: EmitMetricsExemplars rejects a missing Range upfront via
// `rw.Range == 0 → defaults to Step`, so the gate is reached even when
// one bound is zero (numAnchors falls through to 1 in that branch).
func TestEmitMetricsExemplarsInnerScanPushdown_OnlyOneSet(t *testing.T) {
	t.Parallel()

	nonZero := time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC)

	cases := []struct {
		name  string
		start time.Time
		end   time.Time
	}{
		{name: "start_only", start: nonZero, end: time.Time{}},
		{name: "end_only", start: time.Time{}, end: nonZero},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			m := &chplan.MetricsAggregate{
				Op:         chplan.MetricsOpRate,
				ValueAlias: "Value",
				Inner:      &chplan.Scan{Table: "otel_traces"},
			}
			rw := &chplan.RangeWindow{
				Input:           m,
				Step:            time.Minute,
				Range:           time.Minute,
				Start:           c.start,
				End:             c.end,
				TimestampColumn: "Timestamp",
			}
			sql, _, err := chsql.EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 1)
			if err != nil {
				t.Fatalf("EmitMetricsExemplars: %v", err)
			}
			if strings.Contains(sql, pushdownLowerSubstr) {
				t.Errorf("exemplar lower-bound pushdown leaked with only %s set; SQL=%s", c.name, sql)
			}
			if strings.Contains(sql, pushdownUpperSubstr) {
				t.Errorf("exemplar upper-bound pushdown leaked with only %s set; SQL=%s", c.name, sql)
			}
		})
	}
}

// lwrPushdownLowerSubstr / lwrPushdownUpperSubstr are the load-bearing
// substrings the range-query lowerings (RangeLWR / RangeBucketFanout /
// native resample / native rate) render for the inner-scan time bound.
// They reference the per-sample timestamp column `TimeUnix` (the
// canonical OTel-CH column the bare-selector range shapes scan), so a
// substring hit uniquely identifies the new inner-scan prune — distinct
// from the per-anchor distance math (`dateDiff('nanosecond', \`TimeUnix\`,
// …)`) which never renders the bare `\`TimeUnix\` > ` / `\`TimeUnix\` <= `
// comparison.
const (
	lwrPushdownLowerSubstr = "`TimeUnix` > "
	lwrPushdownUpperSubstr = "`TimeUnix` <= "
)

// TestRangeLWRInnerScanTimeBound_BothSet pins the P1 fix for the
// query_range O(rows × anchors) re-scan class: a bare instant-vector
// selector lowered over a pinned [Start, End] grid (the `query_range up`
// shape) MUST carry BOTH a lower AND an upper `TimeUnix` bound on the
// inner scan so ClickHouse prunes granules outside the eval window
// instead of arrayJoin-fanning every retained sample over every anchor.
func TestRangeLWRInnerScanTimeBound_BothSet(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC)

	plan := &chplan.RangeLWR{
		Input:         &chplan.Scan{Table: "otel_metrics_gauge"},
		Start:         start,
		End:           end,
		Step:          30 * time.Second,
		Lookback:      5 * time.Minute,
		MetricNameCol: "MetricName",
		AttributesCol: "Attributes",
		TimestampCol:  "TimeUnix",
		ValueCol:      "Value",
	}

	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, lwrPushdownLowerSubstr) {
		t.Errorf("expected RangeLWR inner-scan lower bound %q in SQL=%s", lwrPushdownLowerSubstr, sql)
	}
	if !strings.Contains(sql, lwrPushdownUpperSubstr) {
		t.Errorf("expected RangeLWR inner-scan upper bound %q in SQL=%s", lwrPushdownUpperSubstr, sql)
	}
}

// TestRangeLWRInnerScanTimeBound_ZeroGridSuppressed pins the gate: the
// now64()/@-pinned fixture shape leaves Start/End zero and relies on the
// bound being suppressed to stay byte-stable. Kills the INVERT_LOGICAL
// mutant on `start.IsZero() || end.IsZero()`.
func TestRangeLWRInnerScanTimeBound_ZeroGridSuppressed(t *testing.T) {
	t.Parallel()

	plan := &chplan.RangeLWR{
		Input:         &chplan.Scan{Table: "otel_metrics_gauge"},
		Step:          30 * time.Second,
		Lookback:      5 * time.Minute,
		MetricNameCol: "MetricName",
		AttributesCol: "Attributes",
		TimestampCol:  "TimeUnix",
		ValueCol:      "Value",
	}

	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if strings.Contains(sql, lwrPushdownLowerSubstr) {
		t.Errorf("RangeLWR inner-scan lower bound leaked on zero grid; SQL=%s", sql)
	}
	if strings.Contains(sql, lwrPushdownUpperSubstr) {
		t.Errorf("RangeLWR inner-scan upper bound leaked on zero grid; SQL=%s", sql)
	}
}

// TestRangeBucketFanoutInnerScanTimeBound_BothSet pins the same prune for
// the array-aggregate histogram-over-range shape
// (`histogram_quantile(…[range])` lowered as RangeBucketFanout): a pinned
// [Start, End] grid MUST carry BOTH a lower AND an upper `TimeUnix` bound
// on the inner scan before the SELECT-list arrayJoin fans each source row.
func TestRangeBucketFanoutInnerScanTimeBound_BothSet(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC)

	plan := &chplan.RangeBucketFanout{
		Input:        &chplan.Scan{Table: "otel_metrics_exponential_histogram"},
		Start:        start,
		End:          end,
		Step:         30 * time.Second,
		Lookback:     5 * time.Minute,
		GroupBy:      []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		AnchorAlias:  "anchor_ts",
		TimestampCol: "TimeUnix",
		AggFuncs: []chplan.AggFunc{
			{
				Name:  "argMax",
				Alias: "BucketCounts",
				Args: []chplan.Expr{
					&chplan.ColumnRef{Name: "BucketCounts"},
					&chplan.ColumnRef{Name: "TimeUnix"},
				},
			},
		},
	}

	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, lwrPushdownLowerSubstr) {
		t.Errorf("expected RangeBucketFanout inner-scan lower bound %q in SQL=%s", lwrPushdownLowerSubstr, sql)
	}
	if !strings.Contains(sql, lwrPushdownUpperSubstr) {
		t.Errorf("expected RangeBucketFanout inner-scan upper bound %q in SQL=%s", lwrPushdownUpperSubstr, sql)
	}
}
