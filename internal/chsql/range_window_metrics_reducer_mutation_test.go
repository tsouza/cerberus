package chsql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// metricsReducerRouteStep is the matrix grid step these fixtures use; the
// rate reducer divides by the window seconds, so a whole-minute step keeps
// the expected divisor a readable literal.
const metricsReducerRouteStep = time.Minute

// metricsReducerRouteZeroFillReducer is the reducer emitRangeWindowMetrics
// renders for the ops that zero-fill empty buckets: the sample arm tags every
// real row `1 AS in_window` and the generator arm tags every (group, anchor)
// `0`, so summing the tag counts observed samples and answers 0 for an anchor
// with none. metricsReducerRouteRateDivisor is the extra `/ <window seconds>`
// rate applies on top.
const (
	metricsReducerRouteZeroFillReducer = "toFloat64(sum(in_window))"
	metricsReducerRouteRateDivisor     = " / 60"
	metricsReducerRouteOperandReducer  = "toFloat64(sum(`metric_arg`))"
)

// metricsReducerRoutePlan builds the matrix-path RangeWindow the emitter walks
// for op, over a spans inner with a bounded request window.
func metricsReducerRoutePlan(op chplan.MetricsOp) *chplan.RangeWindow {
	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	m := &chplan.MetricsAggregate{
		Op:             op,
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "resource.service.name"}},
		GroupByAliases: []string{"resource.service.name"},
		ValueAlias:     "Value",
		Inner:          &chplan.Scan{Table: "otel_traces"},
	}
	if op != chplan.MetricsOpRate && op != chplan.MetricsOpCountOverTime {
		m.Attr = &chplan.ColumnRef{Name: "Duration"}
	}
	return &chplan.RangeWindow{
		Input:           m,
		Step:            metricsReducerRouteStep,
		Range:           metricsReducerRouteStep,
		Start:           start,
		End:             start.Add(5 * metricsReducerRouteStep),
		TimestampColumn: "Timestamp",
	}
}

// TestMetricsMatrixCountingOpsTakeTheZeroFillReducer pins the routing fact the
// NOT KILLABLE note at the foot of this file depends on: in the matrix path,
// rate and count_over_time are answered by the zero-fill reducer
// (metricsSumWeightReducerFrag) and never reach metricsReducerFrag at all.
//
// The two are not interchangeable. metricsSumWeightReducerFrag sums the
// per-row `in_window` weight, which is what makes an anchor with no samples
// answer 0 — Tempo's rate/count_over_time aggregators emit 0 for an empty
// bucket, and dropping the row instead would answer with a hole. So this is a
// wire-visible contract in its own right, not merely bookkeeping about which
// helper runs.
//
// The observed-only counter-case keeps the assertion discriminating: sum_over_time
// takes the OTHER branch and reduces over the projected operand, so a change
// that routed everything through one reducer fails here.
func TestMetricsMatrixCountingOpsTakeTheZeroFillReducer(t *testing.T) {
	t.Parallel()

	emit := func(t *testing.T, op chplan.MetricsOp) string {
		t.Helper()
		sql, _, err := Emit(WithSpansTable(context.Background(), "otel_traces"), metricsReducerRoutePlan(op))
		if err != nil {
			t.Fatalf("Emit(op=%v): %v", op, err)
		}
		return sql
	}

	t.Run("rate", func(t *testing.T) {
		t.Parallel()
		sql := emit(t, chplan.MetricsOpRate)
		if !strings.Contains(sql, metricsReducerRouteZeroFillReducer+metricsReducerRouteRateDivisor) {
			t.Errorf("rate must reduce with %s%s so an empty anchor answers 0:\n%s",
				metricsReducerRouteZeroFillReducer, metricsReducerRouteRateDivisor, sql)
		}
		if strings.Contains(sql, metricsReducerRouteOperandReducer) {
			t.Errorf("rate must not reduce over metric_arg:\n%s", sql)
		}
	})

	t.Run("count_over_time", func(t *testing.T) {
		t.Parallel()
		sql := emit(t, chplan.MetricsOpCountOverTime)
		if !strings.Contains(sql, metricsReducerRouteZeroFillReducer) {
			t.Errorf("count_over_time must reduce with %s so an empty anchor answers 0:\n%s",
				metricsReducerRouteZeroFillReducer, sql)
		}
		if strings.Contains(sql, metricsReducerRouteZeroFillReducer+metricsReducerRouteRateDivisor) {
			t.Errorf("count_over_time must NOT divide by the window seconds — that is rate's normalisation:\n%s", sql)
		}
	})

	t.Run("sum_over_time stays observed-only", func(t *testing.T) {
		t.Parallel()
		sql := emit(t, chplan.MetricsOpSumOverTime)
		if !strings.Contains(sql, metricsReducerRouteOperandReducer) {
			t.Errorf("sum_over_time must reduce over the projected metric_arg:\n%s", sql)
		}
		if strings.Contains(sql, metricsReducerRouteZeroFillReducer) {
			t.Errorf("sum_over_time must not take the zero-fill reducer — Tempo skips its empty buckets:\n%s", sql)
		}
	})

	// The predicate that routes the two counting ops away from
	// metricsReducerFrag, asserted directly. quantile_over_time is listed
	// because it zero-fills too, and is additionally routed to the
	// bucket-shape emitter before the reducer choice is even made.
	t.Run("zero-fill op set", func(t *testing.T) {
		t.Parallel()
		zeroFilling := []chplan.MetricsOp{
			chplan.MetricsOpRate,
			chplan.MetricsOpCountOverTime,
			chplan.MetricsOpQuantileOverTime,
		}
		for _, op := range zeroFilling {
			if !metricsOpZeroFillsEmptyBuckets(op) {
				t.Errorf("metricsOpZeroFillsEmptyBuckets(%v) = false, want true", op)
			}
		}
		observedOnly := []chplan.MetricsOp{
			chplan.MetricsOpSumOverTime,
			chplan.MetricsOpAvgOverTime,
			chplan.MetricsOpMinOverTime,
			chplan.MetricsOpMaxOverTime,
			chplan.MetricsOpHistogramOverTime,
		}
		for _, op := range observedOnly {
			if metricsOpZeroFillsEmptyBuckets(op) {
				t.Errorf("metricsOpZeroFillsEmptyBuckets(%v) = true, want false", op)
			}
		}
	})
}

// NOT KILLABLE — documented, not defended by a test.
//
// range_window.go:2646:32 (INVERT_LOGICAL, `if op == chplan.MetricsOpRate ||
// op == chplan.MetricsOpCountOverTime` -> `&&`, inside metricsReducerFrag).
// The mutated conjunction is unsatisfiable, so the block never runs — but
// neither does the original disjunction, because metricsReducerFrag is never
// called with either of those two ops.
//
// metricsReducerFrag has exactly ONE caller in the tree
// (emitRangeWindowMetrics, range_window.go:1280), and that call sits in the
// `else` of `if zeroFill`, where `zeroFill :=
// metricsOpZeroFillsEmptyBuckets(m.Op)` was computed from the SAME m.Op a few
// lines above. metricsOpZeroFillsEmptyBuckets returns true for exactly
// count_over_time, rate and quantile_over_time, so the reachable op set at
// line 1280 excludes rate and count_over_time outright. (quantile_over_time is
// excluded twice over: emitRangeWindowMetrics routes it to
// emitRangeWindowMetricsQuantileBuckets before the reducer choice is made.)
// TestMetricsMatrixCountingOpsTakeTheZeroFillReducer above pins both halves of
// that routing, so this equivalence claim fails loudly if it ever stops
// holding — and the mutant becomes killable again at the same moment.
//
// Verified empirically as well as by construction: emitting every one of the
// nine chplan.MetricsOp values through the matrix path, crossed with
// Attr-set/Attr-nil and grouped/ungrouped (36 statements), produces
// byte-identical SQL and identical argument slices with the mutation applied
// and reverted.
//
// The same reachability makes the `case chplan.MetricsOpRate` arm of
// metricsReducerFrag's own switch, and the rate half of its doc comment,
// stale — tracked as a separate cleanup in cerberus issue #2945, because
// deleting reachable-looking production code belongs in a change that is about
// that code rather than in a mutation-coverage one.
