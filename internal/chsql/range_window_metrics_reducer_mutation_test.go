package chsql

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// metricsReducerRouteStep is the matrix grid step these fixtures use; the
// rate reducer divides by the window seconds, so a whole-minute step keeps
// the expected divisor a readable literal.
const metricsReducerRouteStep = time.Minute

// metricsReducerRouteSpanSteps is the grid span in steps, so the fixture
// covers several anchors rather than the instant-fallback single one.
const metricsReducerRouteSpanSteps = 5

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

// metricsOpZeroFillExpectation records, for EVERY chplan.MetricsOp, whether
// the matrix path zero-fills its empty buckets. rate and count_over_time do
// because Tempo's aggregators emit 0 for an empty bucket; quantile_over_time
// does too (and is additionally routed to the bucket-shape emitter before the
// reducer choice is made); the observed-only ops do not, because Tempo
// initialises their aggregators to NaN and skips empty buckets on the wire.
// MetricsOpInvalid is the zero value and reaches no reducer at all.
var metricsOpZeroFillExpectation = map[chplan.MetricsOp]bool{
	chplan.MetricsOpInvalid:           false,
	chplan.MetricsOpRate:              true,
	chplan.MetricsOpCountOverTime:     true,
	chplan.MetricsOpQuantileOverTime:  true,
	chplan.MetricsOpSumOverTime:       false,
	chplan.MetricsOpAvgOverTime:       false,
	chplan.MetricsOpMinOverTime:       false,
	chplan.MetricsOpMaxOverTime:       false,
	chplan.MetricsOpHistogramOverTime: false,
}

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
		End:             start.Add(metricsReducerRouteSpanSteps * metricsReducerRouteStep),
		TimestampColumn: "Timestamp",
	}
}

// TestMetricsMatrixCountingOpsTakeTheZeroFillReducer pins the routing fact
// metricsReducerFrag's guard depends on: in the matrix path, rate and
// count_over_time are answered by the zero-fill reducer
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
	// metricsReducerFrag, asserted directly and over the WHOLE enum rather
	// than a hand-picked sample: an op added to chplan.MetricsOp later must
	// have its zero-fill answer recorded here, or this fails. That is what
	// keeps a new op from quietly joining the observed-only branch without
	// anyone deciding whether its empty buckets need filling.
	t.Run("zero-fill op set", func(t *testing.T) {
		t.Parallel()
		for op := chplan.MetricsOpInvalid; op <= chplan.MetricsOpHistogramOverTime; op++ {
			want, recorded := metricsOpZeroFillExpectation[op]
			if !recorded {
				t.Errorf("chplan.MetricsOp %v (%d) has no recorded zero-fill expectation — record one, "+
					"or the new op ships with no coverage of the predicate this file's equivalence note depends on", op, op)
				continue
			}
			if got := metricsOpZeroFillsEmptyBuckets(op); got != want {
				t.Errorf("metricsOpZeroFillsEmptyBuckets(%v) = %v, want %v", op, got, want)
			}
		}
	})
}

// metricsReducerRowCountingOps are the MetricsAggregate ops whose value is the
// NUMBER of matching rows rather than a reduction over an operand column.
// metricsReducerFrag refuses them; emitRangeWindowMetrics never offers them.
var metricsReducerRowCountingOps = []chplan.MetricsOp{
	chplan.MetricsOpRate,
	chplan.MetricsOpCountOverTime,
}

// metricsReducerOperandOps are the ops that DO reach metricsReducerFrag from
// emitRangeWindowMetrics: the observed-only ops, which reduce over the operand
// the sample SELECT projects as `metric_arg`. quantile_over_time is absent
// because it leaves emitRangeWindowMetrics for the bucket-shape emitter before
// any reducer is chosen; MetricsOpInvalid and histogram_over_time are absent
// because metricsAggregateCH refuses them first.
var metricsReducerOperandOps = []chplan.MetricsOp{
	chplan.MetricsOpSumOverTime,
	chplan.MetricsOpAvgOverTime,
	chplan.MetricsOpMinOverTime,
	chplan.MetricsOpMaxOverTime,
}

// TestMetricsReducerFragRefusesRowCountingOps reaches metricsReducerFrag's
// guard by calling it directly with each row-counting op — the call
// emitRangeWindowMetrics cannot make, since its `if zeroFill` branch sends both
// ops to metricsSumWeightReducerFrag instead.
//
// The guard replaced two arms that handled exactly these ops and that no input
// could reach (cerberus issue #2945). It exists so the impossibility is
// ENFORCED rather than merely commented: a later edit that widened the
// observed-only branch to a row-counting op would otherwise emit
// `count(metric_arg)` over a sample arm that projects `in_window` and no
// `metric_arg` at all — SQL naming a column its own FROM does not expose.
//
// Calling the unexported function directly is the only way to reach that
// guard, and reaching it is the point: an error branch nothing ever executes
// would be the same unkillable dead code this change removed.
//
// The operand-op half keeps the assertion discriminating — a guard widened to
// refuse everything fails there rather than passing vacuously.
func TestMetricsReducerFragRefusesRowCountingOps(t *testing.T) {
	t.Parallel()

	// The aggregate call metricsAggregateCH hands the reducer, per op — so
	// the guard is exercised against the real (fn, params, args) triple
	// rather than a hand-made one that might not be what the emitter passes.
	chFor := func(t *testing.T, op chplan.MetricsOp) (chplan.Fn, []chplan.Expr, []chplan.Expr) {
		t.Helper()
		fn, params, args, err := metricsAggregateCH(&chplan.MetricsAggregate{
			Op:   op,
			Attr: &chplan.ColumnRef{Name: "Duration"},
		})
		if err != nil {
			t.Fatalf("metricsAggregateCH(%v): %v", op, err)
		}
		return fn, params, args
	}

	for _, op := range metricsReducerRowCountingOps {
		t.Run("refuses "+op.String(), func(t *testing.T) {
			t.Parallel()
			fn, params, args := chFor(t, op)
			frag, err := metricsReducerFrag(op, fn, params, args)
			if err == nil {
				t.Fatalf("metricsReducerFrag(%v) returned a reducer; %v counts rows and must be "+
					"refused so it can never reduce over the unprojected metric_arg column", op, op)
			}
			if !errors.Is(err, ErrUnsupported) {
				t.Errorf("metricsReducerFrag(%v) error = %v, want it to wrap ErrUnsupported", op, err)
			}
			if frag != nil {
				t.Errorf("metricsReducerFrag(%v) returned a non-nil Frag alongside its refusal", op)
			}
		})
	}

	for _, op := range metricsReducerOperandOps {
		t.Run("renders "+op.String(), func(t *testing.T) {
			t.Parallel()
			fn, params, args := chFor(t, op)
			frag, err := metricsReducerFrag(op, fn, params, args)
			if err != nil {
				t.Fatalf("metricsReducerFrag(%v): %v — this op reaches the reducer from "+
					"emitRangeWindowMetrics and must render", op, err)
			}
			if frag == nil {
				t.Fatalf("metricsReducerFrag(%v) returned a nil Frag and no error", op)
			}
			b := NewBuilder()
			frag(b)
			sql, _, err := b.Build()
			if err != nil {
				t.Fatalf("Build(%v): %v", op, err)
			}
			if !strings.Contains(sql, "metric_arg") {
				t.Errorf("metricsReducerFrag(%v) = %s, want it to reduce over the projected metric_arg", op, sql)
			}
		})
	}
}
