package chsql_test

import (
	"context"
	"fmt"
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

	sql, _, err := chsql.EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 1, "")
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
			sql, _, err := chsql.EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 1, "")
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

// The range-query lowerings (RangeLWR / RangeBucketFanout / native
// resample / native rate) route their inner-scan pruning through the
// shared maybePushRangeScanTimeBound helper:
//
//	func maybePushRangeScanTimeBound(sb *QueryBuilder, tsCol string, start, end time.Time, offsetNS, spanNS int64) {
//		if start.IsZero() || end.IsZero() {  // <- gate
//			return
//		}
//		lo, hi := innerScanTsBoundsFrags(tsCol, start, end, offsetNS, spanNS)
//		sb.Where(lo, hi)
//	}
//
// The `||` short-circuit is load-bearing: when EITHER edge is zero the
// bound must be suppressed (the now64()/@-pinned/zero-grid fixture shapes
// rely on it to stay byte-stable). Two earlier tests
// (TestRangeLWRInnerScanTimeBound_BothSet / _ZeroGridSuppressed) pin the
// both-set and both-zero corners — but neither distinguishes the `||`
// gate from an `&&` gate, because both predicates agree when Start and
// End are EITHER both set OR both zero. The exactly-one-set cases below
// are the discriminating shapes: with `||` the bound is suppressed, with
// the `&&` mutant it leaks a half-zero `now64(9)` bound. They kill the
// INVERT_LOGICAL mutant on the gate (range_lwr.go) for both fan-out node
// types, and the offset/span edge assertions kill the ARITHMETIC /
// CONDITIONALS_BOUNDARY mutants on the inner-scan bound math.

// rangeScanOffsetShift is the offset interval (2m) the bound subtracts
// from both edges; rangeScanSpanShift is the lower-edge span widening
// (5m Lookback / Range). Both render as toIntervalNanosecond(<ns>) and
// are load-bearing — an ARITHMETIC mutant on either term changes the
// emitted ns literal, and a CONDITIONALS_BOUNDARY / sign flip on the
// offset shift moves the whole interval.
const (
	rangeScanOffsetShift = "toIntervalNanosecond(120000000000)" // 2m offset
	rangeScanSpanShift   = "toIntervalNanosecond(300000000000)" // 5m Lookback/Range
)

// TestRangeLWRInnerScanTimeBound_OnlyOneSet pins the gate's `||`
// short-circuit for the RangeLWR fan-out: with exactly one of Start/End
// set the inner-scan bound MUST be suppressed. Under the INVERT_LOGICAL
// mutant (`||` -> `&&`) a single-edge grid would NOT suppress and would
// leak a half-zero `now64(9)` bound, so this case kills it.
func TestRangeLWRInnerScanTimeBound_OnlyOneSet(t *testing.T) {
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
			plan := &chplan.RangeLWR{
				Input:         &chplan.Scan{Table: "otel_metrics_gauge"},
				Start:         c.start,
				End:           c.end,
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
				t.Errorf("RangeLWR inner-scan lower bound leaked with only %s set; SQL=%s", c.name, sql)
			}
			if strings.Contains(sql, lwrPushdownUpperSubstr) {
				t.Errorf("RangeLWR inner-scan upper bound leaked with only %s set; SQL=%s", c.name, sql)
			}
		})
	}
}

// TestRangeLWRInnerScanTimeBound_OffsetAndSpanEdges pins the exact
// offset-shift + lower-span-widening terms the RangeLWR bound subtracts.
// The lower bound is
//
//	`TimeUnix` > (<Start> - toIntervalNanosecond(<offset>)) - toIntervalNanosecond(<lookback>)
//
// and the upper bound is `(<End> - toIntervalNanosecond(<offset>))`. An
// ARITHMETIC mutant on the offset or span term changes the emitted ns
// literal; a sign flip / boundary mutant moves the interval. Asserting
// both literals appear (offset on BOTH edges, span ONLY on the lower)
// kills those mutants on the inner-scan math.
func TestRangeLWRInnerScanTimeBound_OffsetAndSpanEdges(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC)

	plan := &chplan.RangeLWR{
		Input:         &chplan.Scan{Table: "otel_metrics_gauge"},
		Start:         start,
		End:           end,
		Step:          30 * time.Second,
		Lookback:      5 * time.Minute,
		Offset:        2 * time.Minute,
		MetricNameCol: "MetricName",
		AttributesCol: "Attributes",
		TimestampCol:  "TimeUnix",
		ValueCol:      "Value",
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	// The full lower-bound expression: offset shift then span widening.
	wantLower := "`TimeUnix` > (toDateTime64('2026-05-13 12:00:00.000000000', 9) - " +
		rangeScanOffsetShift + ") - " + rangeScanSpanShift
	if !strings.Contains(sql, wantLower) {
		t.Errorf("expected RangeLWR offset+span lower bound %q in SQL=%s", wantLower, sql)
	}
	// The upper bound carries the offset shift but NO span term.
	wantUpper := "`TimeUnix` <= (toDateTime64('2026-05-13 12:05:00.000000000', 9) - " +
		rangeScanOffsetShift + ")"
	if !strings.Contains(sql, wantUpper) {
		t.Errorf("expected RangeLWR offset-only upper bound %q in SQL=%s", wantUpper, sql)
	}
}

// TestRangeLWRRejectsBadInput pins the RangeLWR emitter's pre-flight
// guards (Step <= 0, nil Input, the 4-way column-name OR, and Start >
// End). Under a mutated guard the bad-input plan would emit SQL instead
// of erroring, killing the CONDITIONALS_BOUNDARY / CONDITIONALS_NEGATION
// / INVERT_LOGICAL mutants on those guards and the span-sign boundary.
func TestRangeLWRRejectsBadInput(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC)
	mkBase := func() chplan.RangeLWR {
		return chplan.RangeLWR{
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
	}

	cases := []struct {
		name  string
		mutfn func(p *chplan.RangeLWR)
	}{
		{name: "zero_step", mutfn: func(p *chplan.RangeLWR) { p.Step = 0 }},
		{name: "neg_step", mutfn: func(p *chplan.RangeLWR) { p.Step = -time.Second }},
		{name: "nil_input", mutfn: func(p *chplan.RangeLWR) { p.Input = nil }},
		{name: "no_timestamp_col", mutfn: func(p *chplan.RangeLWR) { p.TimestampCol = "" }},
		{name: "no_value_col", mutfn: func(p *chplan.RangeLWR) { p.ValueCol = "" }},
		{name: "no_metric_name_col", mutfn: func(p *chplan.RangeLWR) { p.MetricNameCol = "" }},
		{name: "no_attributes_col", mutfn: func(p *chplan.RangeLWR) { p.AttributesCol = "" }},
		{name: "start_after_end", mutfn: func(p *chplan.RangeLWR) { p.Start, p.End = end, start }},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p := mkBase()
			c.mutfn(&p)
			if _, _, err := chsql.Emit(context.Background(), &p); err == nil {
				t.Errorf("expected RangeLWR to reject %s, got nil error", c.name)
			}
		})
	}
}

// TestRangeBucketFanoutRejectsBadAggExpr pins the per-AggFunc pre-flight
// that synchronously surfaces chplan Expr errors from BOTH the Params and
// the Args lists (the `(&Builder{}).Expr(...)` loops). A nil Expr element
// hits Builder.Expr's default branch (ErrUnsupported), so a fan-out whose
// AggFunc carries a nil Param / nil Arg must error — covering and killing
// the mutants on those two pre-flight loops.
func TestRangeBucketFanoutRejectsBadAggExpr(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC)
	mkBase := func() chplan.RangeBucketFanout {
		return chplan.RangeBucketFanout{
			Input:        &chplan.Scan{Table: "otel_metrics_exponential_histogram"},
			Start:        start,
			End:          end,
			Step:         30 * time.Second,
			Lookback:     5 * time.Minute,
			GroupBy:      []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
			AnchorAlias:  "anchor_ts",
			TimestampCol: "TimeUnix",
		}
	}

	cases := []struct {
		name string
		agg  chplan.AggFunc
	}{
		{
			name: "bad_param",
			agg: chplan.AggFunc{
				Name:   "quantilesTDigest",
				Alias:  "BucketCounts",
				Params: []chplan.Expr{nil},
				Args:   []chplan.Expr{&chplan.ColumnRef{Name: "BucketCounts"}},
			},
		},
		{
			name: "bad_arg",
			agg: chplan.AggFunc{
				Name:  "argMax",
				Alias: "BucketCounts",
				Args:  []chplan.Expr{nil},
			},
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p := mkBase()
			p.AggFuncs = []chplan.AggFunc{c.agg}
			if _, _, err := chsql.Emit(context.Background(), &p); err == nil {
				t.Errorf("expected RangeBucketFanout to reject %s, got nil error", c.name)
			}
		})
	}
}

// TestRangeBucketFanoutInnerScanTimeBound_OnlyOneSet mirrors the LWR
// gate test for the histogram-over-range fan-out: with exactly one of
// Start/End set the bound MUST be suppressed (kills the INVERT_LOGICAL
// gate mutant on this node's call site too).
func TestRangeBucketFanoutInnerScanTimeBound_OnlyOneSet(t *testing.T) {
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
			plan := &chplan.RangeBucketFanout{
				Input:        &chplan.Scan{Table: "otel_metrics_exponential_histogram"},
				Start:        c.start,
				End:          c.end,
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
			if strings.Contains(sql, lwrPushdownLowerSubstr) {
				t.Errorf("RangeBucketFanout lower bound leaked with only %s set; SQL=%s", c.name, sql)
			}
			if strings.Contains(sql, lwrPushdownUpperSubstr) {
				t.Errorf("RangeBucketFanout upper bound leaked with only %s set; SQL=%s", c.name, sql)
			}
		})
	}
}

// TestRangeBucketFanoutInnerScanTimeBound_OffsetAndSpanEdges pins the
// exact offset-shift + span-widening terms for the histogram-over-range
// fan-out's inner-scan bound (same contract as RangeLWR above).
func TestRangeBucketFanoutInnerScanTimeBound_OffsetAndSpanEdges(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC)

	plan := &chplan.RangeBucketFanout{
		Input:        &chplan.Scan{Table: "otel_metrics_exponential_histogram"},
		Start:        start,
		End:          end,
		Step:         30 * time.Second,
		Lookback:     5 * time.Minute,
		Offset:       2 * time.Minute,
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
	wantLower := "`TimeUnix` > (toDateTime64('2026-05-13 12:00:00.000000000', 9) - " +
		rangeScanOffsetShift + ") - " + rangeScanSpanShift
	if !strings.Contains(sql, wantLower) {
		t.Errorf("expected RangeBucketFanout offset+span lower bound %q in SQL=%s", wantLower, sql)
	}
	wantUpper := "`TimeUnix` <= (toDateTime64('2026-05-13 12:05:00.000000000', 9) - " +
		rangeScanOffsetShift + ")"
	if !strings.Contains(sql, wantUpper) {
		t.Errorf("expected RangeBucketFanout offset-only upper bound %q in SQL=%s", wantUpper, sql)
	}
	// numAnchors = (End-Start)/Step + 1 = 5m/30s + 1 = 11. It renders as
	// the `least(11, …)` upper index cap of the anchor-fanout range. An
	// ARITHMETIC mutant on the `/Step` or `+1` term shifts the count
	// (e.g. `+1`->`-1` yields least(9,…)), so pinning the literal kills
	// both numAnchors arithmetic mutants. (The RangeLWR sibling is pinned
	// by range_lwr_test.go's TestEmitRangeLWR_SinglePassShape.)
	if !strings.Contains(sql, "least(11,") {
		t.Errorf("expected RangeBucketFanout numAnchors cap least(11, in SQL=%s", sql)
	}
}

// TestRangeBucketFanoutZeroSpanGridAccepted pins the `span < 0` reject
// boundary: a single-point grid (Start == End, span == 0) is VALID and
// must emit one anchor (least(1, …)), not error. The earlier
// start_after_end case only exercises span < 0 (rejected by both `< 0`
// and the `<= 0` boundary mutant); this span == 0 success case is the
// discriminating shape that kills the CONDITIONALS_BOUNDARY mutant on the
// `if span < 0` guard.
func TestRangeBucketFanoutZeroSpanGridAccepted(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)

	plan := &chplan.RangeBucketFanout{
		Input:        &chplan.Scan{Table: "otel_metrics_exponential_histogram"},
		Start:        at,
		End:          at, // span == 0
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
		t.Fatalf("expected zero-span grid to be accepted, got error: %v", err)
	}
	if !strings.Contains(sql, "least(1,") {
		t.Errorf("expected zero-span grid to emit a single anchor (least(1,) in SQL=%s", sql)
	}
}

// TestRangeWindowNativeInnerScanTimeBound pins the inner-scan prune the
// ClickHouse-native rate lowering (RangeWindowNative, the
// timeSeriesRateToGrid path) pushes onto the per-series SELECT BEFORE the
// GROUP BY. This shape was previously only exercised through the
// test/spec TXTAR goldens, which the package-scoped mutation lane does
// NOT run — so every guard + the maybePushRangeScanTimeBound call here
// was an uncovered mutation surface. The exact-bound assertions cover and
// kill the CONDITIONALS_NEGATION / CONDITIONALS_BOUNDARY mutants on the
// emitter's guards and the inner-scan bound math.
func TestRangeWindowNativeInnerScanTimeBound(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC)

	plan := &chplan.RangeWindowNative{
		Input:           &chplan.Scan{Table: "otel_metrics_sum"},
		Func:            "rate",
		Start:           start,
		End:             end,
		Step:            30 * time.Second,
		Range:           5 * time.Minute,
		Offset:          2 * time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	// Lower bound: offset shift then Range (5m) widening; upper bound:
	// offset shift only. The Range term (300000000000) reuses the same
	// 5m literal as Lookback above.
	wantLower := "`TimeUnix` > (toDateTime64('2026-05-13 12:00:00.000000000', 9) - " +
		rangeScanOffsetShift + ") - " + rangeScanSpanShift
	if !strings.Contains(sql, wantLower) {
		t.Errorf("expected RangeWindowNative offset+Range lower bound %q in SQL=%s", wantLower, sql)
	}
	wantUpper := "`TimeUnix` <= (toDateTime64('2026-05-13 12:05:00.000000000', 9) - " +
		rangeScanOffsetShift + ")"
	if !strings.Contains(sql, wantUpper) {
		t.Errorf("expected RangeWindowNative offset-only upper bound %q in SQL=%s", wantUpper, sql)
	}
	// When the schema TimestampColumn differs from the bare anchor alias
	// (the common case — here "TimeUnix" != "anchor_ts"), the outer SELECT
	// surfaces the anchor BOTH bare and re-aliased to the schema column so
	// the wrapping Aggregate's per-step GROUP BY(ColumnRef{TimestampColumn})
	// resolves. The `r.TimestampColumn != RangeWindowAnchorAlias` guard
	// gates that extra projection; a CONDITIONALS_NEGATION mutant (`!=` ->
	// `==`) drops the re-alias for a differently-named column, so pinning
	// the `anchor_ts` -> `TimeUnix` projection kills it.
	if !strings.Contains(sql, "`anchor_ts` AS `TimeUnix`") {
		t.Errorf("expected RangeWindowNative anchor re-alias `anchor_ts` AS `TimeUnix` in SQL=%s", sql)
	}
}

// TestRangeWindowNativeRejectsBadInput pins the emitter's pre-flight
// guards (TimestampColumn / ValueColumn unset, Step <= 0, unknown Func)
// so the CONDITIONALS_NEGATION / CONDITIONALS_BOUNDARY mutants on each
// guard are killed: under a mutated guard the bad-input plan would emit
// SQL instead of erroring.
func TestRangeWindowNativeRejectsBadInput(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC)
	base := chplan.RangeWindowNative{
		Input:           &chplan.Scan{Table: "otel_metrics_sum"},
		Func:            "rate",
		Start:           start,
		End:             end,
		Step:            30 * time.Second,
		Range:           5 * time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
	}

	cases := []struct {
		name  string
		mutfn func(p *chplan.RangeWindowNative)
	}{
		{name: "no_timestamp_col", mutfn: func(p *chplan.RangeWindowNative) { p.TimestampColumn = "" }},
		{name: "no_value_col", mutfn: func(p *chplan.RangeWindowNative) { p.ValueColumn = "" }},
		{name: "zero_step", mutfn: func(p *chplan.RangeWindowNative) { p.Step = 0 }},
		{name: "neg_step", mutfn: func(p *chplan.RangeWindowNative) { p.Step = -time.Second }},
		{name: "unknown_func", mutfn: func(p *chplan.RangeWindowNative) { p.Func = "nope" }},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p := base
			c.mutfn(&p)
			if _, _, err := chsql.Emit(context.Background(), &p); err == nil {
				t.Errorf("expected RangeWindowNative to reject %s, got nil error", c.name)
			}
		})
	}
}

// TestRangeWindowResampleInnerScanTimeBound pins the inner-scan prune the
// native-staleness resample lowering (RangeWindowResample,
// timeSeriesResampleToGridWithStaleness) pushes onto the per-series
// SELECT. Same coverage rationale as the native-rate test above — this
// shape only reached the spec goldens before, so its guards + bound were
// an uncovered mutation surface.
func TestRangeWindowResampleInnerScanTimeBound(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC)

	plan := &chplan.RangeWindowResample{
		Input:         &chplan.Scan{Table: "otel_metrics_gauge"},
		Start:         start,
		End:           end,
		Step:          30 * time.Second,
		Lookback:      5 * time.Minute,
		Offset:        2 * time.Minute,
		MetricNameCol: "MetricName",
		AttributesCol: "Attributes",
		TimestampCol:  "TimeUnix",
		ValueCol:      "Value",
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	wantLower := "`TimeUnix` > (toDateTime64('2026-05-13 12:00:00.000000000', 9) - " +
		rangeScanOffsetShift + ") - " + rangeScanSpanShift
	if !strings.Contains(sql, wantLower) {
		t.Errorf("expected RangeWindowResample offset+Lookback lower bound %q in SQL=%s", wantLower, sql)
	}
	wantUpper := "`TimeUnix` <= (toDateTime64('2026-05-13 12:05:00.000000000', 9) - " +
		rangeScanOffsetShift + ")"
	if !strings.Contains(sql, wantUpper) {
		t.Errorf("expected RangeWindowResample offset-only upper bound %q in SQL=%s", wantUpper, sql)
	}
}

// TestNativeTSGridFamilyBoundsAreWholeSecondDateTime pins the
// start_timestamp/end_timestamp argument TYPE the timeSeries*ToGrid native
// aggregate family (RangeWindowNative's timeSeriesRateToGrid /
// timeSeriesChangesToGrid / timeSeriesResetsToGrid, and RangeWindowResample's
// timeSeriesResampleToGridWithStaleness) and their companion timeSeriesRange
// receive: a whole-second `toDateTime(<unix seconds>, 'UTC')`, never a
// nanosecond-precision `toDateTime64(..., 9)`.
//
// ClickHouse's own docs type these functions' start_timestamp/end_timestamp
// parameters as "UInt32 or DateTime" — never DateTime64. Emitting
// DateTime64(9) there (as this emitter did before this test was added)
// forces ClickHouse's argument-coercion machinery through an internal
// Decimal(18, 9) representation with room for only 9 INTEGER digits; any
// Unix-second count past 2001-09-09 already needs 10. On ClickHouse server
// versions where that coercion path is exercised (reproduced against a live
// deployment; NOT reproducible on this repo's pinned chDB 25.8.2.1
// substrate — see TestNativeTSGridRate_DualEmitParity /
// TestNativeTSGridResample_DualEmitParity, which exercise the identical
// literal shape and pass there regardless) this 502s every real-world
// query with "DB::Exception: Decimal value is too big: 10 digits were
// read: '<epoch seconds>'e0. Expected to read decimal with scale 9 and
// precision 18: while parsing aggregate function '<fn>'".
//
// The fixed Start/End below are deliberately in the "needs 10 digits"
// range (>= 1_000_000_000, i.e. after 2001-09-09) with a sub-second
// component (mirroring a real Grafana millisecond-epoch request) — picking
// an easy small/round timestamp here would silently stop catching a
// regression back to toDateTime64.
func TestNativeTSGridFamilyBoundsAreWholeSecondDateTime(t *testing.T) {
	t.Parallel()

	start := time.UnixMilli(1782889603653).UTC()
	end := time.UnixMilli(1783062403653).UTC()
	offset := 2 * time.Minute

	if start.Unix() < 1_000_000_000 || end.Unix() < 1_000_000_000 {
		t.Fatalf("fixture bug: start/end must need 10 Unix-second digits to exercise the Decimal(18,9) overflow this test guards against (start=%d end=%d)", start.Unix(), end.Unix())
	}

	wantStartShifted := fmt.Sprintf("toDateTime(%d, 'UTC')", start.Add(-offset).Unix())
	wantEndShifted := fmt.Sprintf("toDateTime(%d, 'UTC')", end.Add(-offset).Unix())
	wantStartUnshifted := fmt.Sprintf("toDateTime(%d, 'UTC')", start.Unix())
	wantEndUnshifted := fmt.Sprintf("toDateTime(%d, 'UTC')", end.Unix())
	wantGridTS := "timeSeriesRange(" + wantStartUnshifted + ", " + wantEndUnshifted + ", 120)"

	t.Run("RangeWindowNative_rate", func(t *testing.T) {
		t.Parallel()
		plan := &chplan.RangeWindowNative{
			Input:           &chplan.Scan{Table: "otel_metrics_sum"},
			Func:            "rate",
			Start:           start,
			End:             end,
			Step:            120 * time.Second,
			Range:           5 * time.Minute,
			Offset:          offset,
			TimestampColumn: "TimeUnix",
			ValueColumn:     "Value",
		}
		sql, _, err := chsql.Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		wantAgg := "timeSeriesRateToGrid(" + wantStartShifted + ", " + wantEndShifted + ", 120, 300)"
		if !strings.Contains(sql, wantAgg) {
			t.Errorf("expected offset-shifted whole-second aggregate bounds %q in SQL=%s", wantAgg, sql)
		}
		if !strings.Contains(sql, wantGridTS) {
			t.Errorf("expected unshifted whole-second timeSeriesRange bounds %q in SQL=%s", wantGridTS, sql)
		}
	})

	t.Run("RangeWindowResample", func(t *testing.T) {
		t.Parallel()
		plan := &chplan.RangeWindowResample{
			Input:         &chplan.Scan{Table: "otel_metrics_gauge"},
			Start:         start,
			End:           end,
			Step:          120 * time.Second,
			Lookback:      5 * time.Minute,
			Offset:        offset,
			MetricNameCol: "MetricName",
			AttributesCol: "Attributes",
			TimestampCol:  "TimeUnix",
			ValueCol:      "Value",
		}
		sql, _, err := chsql.Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		wantAgg := "timeSeriesResampleToGridWithStaleness(" + wantStartShifted + ", " + wantEndShifted + ", 120, 300)"
		if !strings.Contains(sql, wantAgg) {
			t.Errorf("expected offset-shifted whole-second aggregate bounds %q in SQL=%s", wantAgg, sql)
		}
		if !strings.Contains(sql, wantGridTS) {
			t.Errorf("expected unshifted whole-second timeSeriesRange bounds %q in SQL=%s", wantGridTS, sql)
		}
	})
}

// TestRangeWindowResampleRejectsBadInput pins the resample emitter's
// pre-flight guards: the 4-way column-name OR, Step <= 0, and the
// pinned-Start/End requirement. Under a mutated guard the bad-input plan
// would emit SQL instead of erroring, so each case kills the
// CONDITIONALS_NEGATION / INVERT_LOGICAL / CONDITIONALS_BOUNDARY mutants
// on those guards.
func TestRangeWindowResampleRejectsBadInput(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC)
	base := chplan.RangeWindowResample{
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

	cases := []struct {
		name  string
		mutfn func(p *chplan.RangeWindowResample)
	}{
		{name: "no_timestamp_col", mutfn: func(p *chplan.RangeWindowResample) { p.TimestampCol = "" }},
		{name: "no_value_col", mutfn: func(p *chplan.RangeWindowResample) { p.ValueCol = "" }},
		{name: "no_metric_name_col", mutfn: func(p *chplan.RangeWindowResample) { p.MetricNameCol = "" }},
		{name: "no_attributes_col", mutfn: func(p *chplan.RangeWindowResample) { p.AttributesCol = "" }},
		{name: "zero_step", mutfn: func(p *chplan.RangeWindowResample) { p.Step = 0 }},
		{name: "neg_step", mutfn: func(p *chplan.RangeWindowResample) { p.Step = -time.Second }},
		{name: "zero_start", mutfn: func(p *chplan.RangeWindowResample) { p.Start = time.Time{} }},
		{name: "zero_end", mutfn: func(p *chplan.RangeWindowResample) { p.End = time.Time{} }},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p := base
			c.mutfn(&p)
			if _, _, err := chsql.Emit(context.Background(), &p); err == nil {
				t.Errorf("expected RangeWindowResample to reject %s, got nil error", c.name)
			}
		})
	}
}

// TestRangeBucketFanoutRejectsBadInput pins the fan-out emitter's
// pre-flight guards (Step <= 0, nil Input, empty TimestampCol/AnchorAlias,
// no AggFunc, and Start > End). The Step <= 0 case in particular kills the
// CONDITIONALS_BOUNDARY mutant at the `r.Step <= 0` guard; the Start > End
// case kills the span-sign boundary mutant.
func TestRangeBucketFanoutRejectsBadInput(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC)
	mkAgg := func() []chplan.AggFunc {
		return []chplan.AggFunc{
			{
				Name:  "argMax",
				Alias: "BucketCounts",
				Args: []chplan.Expr{
					&chplan.ColumnRef{Name: "BucketCounts"},
					&chplan.ColumnRef{Name: "TimeUnix"},
				},
			},
		}
	}
	mkBase := func() chplan.RangeBucketFanout {
		return chplan.RangeBucketFanout{
			Input:        &chplan.Scan{Table: "otel_metrics_exponential_histogram"},
			Start:        start,
			End:          end,
			Step:         30 * time.Second,
			Lookback:     5 * time.Minute,
			GroupBy:      []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
			AnchorAlias:  "anchor_ts",
			TimestampCol: "TimeUnix",
			AggFuncs:     mkAgg(),
		}
	}

	cases := []struct {
		name  string
		mutfn func(p *chplan.RangeBucketFanout)
	}{
		{name: "zero_step", mutfn: func(p *chplan.RangeBucketFanout) { p.Step = 0 }},
		{name: "neg_step", mutfn: func(p *chplan.RangeBucketFanout) { p.Step = -time.Second }},
		{name: "nil_input", mutfn: func(p *chplan.RangeBucketFanout) { p.Input = nil }},
		{name: "no_timestamp_col", mutfn: func(p *chplan.RangeBucketFanout) { p.TimestampCol = "" }},
		{name: "no_anchor_alias", mutfn: func(p *chplan.RangeBucketFanout) { p.AnchorAlias = "" }},
		{name: "no_agg_func", mutfn: func(p *chplan.RangeBucketFanout) { p.AggFuncs = nil }},
		{name: "start_after_end", mutfn: func(p *chplan.RangeBucketFanout) { p.Start, p.End = end, start }},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p := mkBase()
			c.mutfn(&p)
			if _, _, err := chsql.Emit(context.Background(), &p); err == nil {
				t.Errorf("expected RangeBucketFanout to reject %s, got nil error", c.name)
			}
		})
	}
}
