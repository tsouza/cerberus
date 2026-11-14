package tempo

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// These tests pin the metrics-path bound on the per-trace subqueries of a
// trace-scoped intrinsic (rootName / rootServiceName / traceDuration): a
// TraceLookupFilter lookup joined onto the spans, or a membership subquery in a
// Filter predicate or a compare() selection. The metrics emitters bound the
// metrics input only in the SELECT around it, so without
// chplan.BoundMetricsPerTraceSubqueries each of those GROUP BY TraceId
// subqueries reads the spans table across full retention. Every statement a
// metrics request sends — the matrix statement, the exemplar statement and the
// compare root leg — must carry the RangeWindow input bound
// (Start - Range, End] on each one.

// perTraceWindowStep is the step of the matrix RangeWindow these tests wrap.
const perTraceWindowStep = time.Minute

// Bounded-site counts of one render of a per-trace shape: a lookup or a
// membership subquery, plus the plain span rows it filters.
const (
	lookupSites           = 2 // the lookup and the span rows joined onto it
	membershipSites       = 2 // the membership subquery and the span rows it filters
	selectionSites        = 1 // a compare() selection's membership subquery
	noOwnBinds            = 0
	inputRendersHistogram = 2 // the sample arm and the zero-fill bucket discovery arm
	inputRendersGrouped   = 2 // the sample arm and the zero-fill group discovery arm
	inputRendersCompare   = 2 // the span leg and the root lookup's trace-id seed
	// compareRootSeedBinds counts the binds of the RangeWindow input bound's
	// instants the compare emitter makes for the root lookup's own seed
	// window.
	compareRootSeedBinds = 1
)

// windowedBoundPair is the SQL of chplan.RangeWindowInputBound: an exclusive
// lower and an inclusive upper conjunct, as the emitter's own
// `(anchor - range, anchor]` window is.
const windowedBoundPair = "(`Timestamp` > fromUnixTimestamp64Nano(?)) AND (`Timestamp` <= fromUnixTimestamp64Nano(?))"

// perTraceWindowStart / perTraceWindowEnd are the request window the cases
// wrap; the RangeWindow input span is (start - step, end].
var (
	perTraceWindowStart = time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	perTraceWindowEnd   = perTraceWindowStart.Add(time.Hour)
)

// perTraceWindowLowerNanos / perTraceWindowUpperNanos are the bound arguments
// every bounded per-trace subquery binds.
func perTraceWindowLowerNanos() int64 {
	return perTraceWindowStart.Add(-perTraceWindowStep).UnixNano()
}

func perTraceWindowUpperNanos() int64 { return perTraceWindowEnd.UnixNano() }

// countBoundArgs returns how many times the lower and the upper RangeWindow
// input bound nanos are bound in args.
func countBoundArgs(args []any) (lower, upper int) {
	for _, a := range args {
		v, ok := a.(int64)
		if !ok {
			continue
		}
		switch v {
		case perTraceWindowLowerNanos():
			lower++
		case perTraceWindowUpperNanos():
			upper++
		}
	}
	return lower, upper
}

// perTraceRangeWindow wraps a lowered metrics node the way the
// /api/metrics/query_range handler does.
func perTraceRangeWindow(input chplan.Node) *chplan.RangeWindow {
	return &chplan.RangeWindow{
		Input:           input,
		Range:           perTraceWindowStep,
		Step:            perTraceWindowStep,
		Start:           perTraceWindowStart,
		End:             perTraceWindowEnd,
		TimestampColumn: tracesSchema().TimestampColumn,
	}
}

// emitSpans emits plan the way the engine does for the Tempo head: with the
// spans table on the context, so the emit chokepoint's spans-scan checks run.
func emitSpans(t *testing.T, plan chplan.Node) (string, []any) {
	t.Helper()
	ctx := chsql.WithSpansTable(context.Background(), tracesSchema().SpansTable)
	sql, args, err := chsql.Emit(ctx, plan)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	return sql, args
}

// assertWindowedSites fails unless the statement renders the RangeWindow input
// bound exactly sites times — once per per-trace subquery and per plain row
// source it filters, each time the statement renders them — and binds the
// bound's instants sites + ownBinds times, ownBinds counting the binds of the
// same instants an emitter makes for a bound of its own.
func assertWindowedSites(t *testing.T, sql string, args []any, sites, ownBinds int) {
	t.Helper()
	if got := strings.Count(sql, windowedBoundPair); got != sites {
		t.Fatalf("RangeWindow input bound %q rendered %d times, want %d\n--- sql ---\n%s", windowedBoundPair, got, sites, sql)
	}
	lower, upper := countBoundArgs(args)
	if want := sites + ownBinds; lower != want || upper != want {
		t.Fatalf("RangeWindow input bound instants bound %d (lower) / %d (upper) times, want %d each\n--- sql ---\n%s", lower, upper, want, sql)
	}
}

// TestMetricsPerTraceSubqueriesWindowed pins the bound on the matrix
// statement of each metrics function family over each per-trace shape.
func TestMetricsPerTraceSubqueriesWindowed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		query string
		// sites counts the bounded per-trace subqueries plus the plain row
		// sources they filter, times the renders of the metrics input.
		sites int
	}{
		// The lookup and the span rows it is joined onto.
		{"root_name_span_operand", `{ rootName = name } | rate()`, lookupSites},
		{"root_service_span_operand", `{ rootServiceName = resource.service.name } | count_over_time()`, lookupSites},
		{"trace_duration_span_operand", `{ traceDuration > duration } | rate()`, lookupSites},
		// The membership subquery and the span rows it filters.
		{"trace_duration_literal", `{ traceDuration > 1s } | rate()`, membershipSites},
		{"root_name_literal_negated", `{ rootName != "x" } | count_over_time()`, membershipSites},
		// The lookup, the membership subquery in the conjunct below the join,
		// and the span rows both of them filter.
		{"lookup_and_membership", `{ rootName = name && traceDuration > 1s } | rate()`, lookupSites + membershipSites - 1},
		{"quantile", `{ rootName = name } | quantile_over_time(duration, .9)`, lookupSites},
		{"grouped", `{ rootName = name } | rate() by (resource.service.name)`, inputRendersGrouped * lookupSites},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan := lowerMetricsWithWindow(t, tc.query, perTraceWindowStart, perTraceWindowEnd)
			sql, args := emitSpans(t, wrapMetricsMatrix(t, plan, perTraceWindowStart, perTraceWindowEnd))
			assertWindowedSites(t, sql, args, tc.sites, noOwnBinds)
		})
	}
}

// TestMetricsPerTraceSubqueriesWindowedHistogram pins the bound on the
// histogram_over_time matrix statement.
func TestMetricsPerTraceSubqueriesWindowedHistogram(t *testing.T) {
	t.Parallel()
	plan := lowerMetricsWithWindow(t, `{ rootName = name } | histogram_over_time(duration)`, perTraceWindowStart, perTraceWindowEnd)
	hist, ok := unwrapMetricsRoot[*chplan.MetricsHistogramOverTime](plan)
	if !ok {
		t.Fatalf("plan is not a histogram: %T", plan)
	}
	sql, args := emitSpans(t, wrapHistogramForSample(perTraceRangeWindow(plan), hist))
	assertWindowedSites(t, sql, args, inputRendersHistogram*lookupSites, noOwnBinds)
}

// TestMetricsPerTraceSubqueriesWindowedCompare pins the bound on the compare()
// matrix statement and its root leg: the lookup in Inner and the membership
// subquery in the selection, which sits beside Inner.
func TestMetricsPerTraceSubqueriesWindowedCompare(t *testing.T) {
	t.Parallel()
	plan := lowerMetricsWithWindow(t, `{ rootName = name } | compare({ traceDuration > 1s })`, perTraceWindowStart, perTraceWindowEnd)
	cmp, ok := unwrapMetricsRoot[*chplan.MetricsCompare](plan)
	if !ok {
		t.Fatalf("plan is not a compare: %T", plan)
	}
	rw := perTraceRangeWindow(plan)
	sql, args := emitSpans(t, wrapCompareForSample(rw, cmp))
	assertWindowedSites(t, sql, args, selectionSites+inputRendersCompare*lookupSites, compareRootSeedBinds)

	ctx := chsql.WithSpansTable(context.Background(), tracesSchema().SpansTable)
	legSQL, legArgs, _, err := chsql.EmitCompareRootLeg(ctx, rw)
	if err != nil {
		t.Fatalf("EmitCompareRootLeg: %v", err)
	}
	// The root leg seeds itself from Inner: the lookup and the span rows.
	assertWindowedSites(t, legSQL, legArgs, lookupSites, compareRootSeedBinds)
}

// TestMetricsPerTraceSubqueriesWindowedExemplars pins the bound on the
// exemplar statement, which renders the metrics input outside chsql.Emit.
func TestMetricsPerTraceSubqueriesWindowedExemplars(t *testing.T) {
	t.Parallel()
	plan := lowerMetricsWithWindow(t, `{ rootName = name } | rate()`, perTraceWindowStart, perTraceWindowEnd)
	m, ok := unwrapMetricsRoot[*chplan.MetricsAggregate](plan)
	if !ok {
		t.Fatalf("plan is not a metrics aggregate: %T", plan)
	}
	s := tracesSchema()
	const exemplarsPerSeriesUnderTest = 1
	sql, args, _, err := chsql.EmitMetricsExemplars(context.Background(), perTraceRangeWindow(plan), m,
		s.TraceIDColumn, s.SpanIDColumn, exemplarsPerSeriesUnderTest, s.SpansTable)
	if err != nil {
		t.Fatalf("EmitMetricsExemplars: %v", err)
	}
	assertWindowedSites(t, sql, args, lookupSites, noOwnBinds)
}

// TestMetricsPerTraceWindowLeavesPlainMetricsUnchanged pins the no-op: a
// metrics pipeline with no per-trace subquery keeps the statement the
// RangeWindow emitter alone bounds.
func TestMetricsPerTraceWindowLeavesPlainMetricsUnchanged(t *testing.T) {
	t.Parallel()
	plan := lowerMetricsWithWindow(t, `{ span.foo = "x" } | rate()`, perTraceWindowStart, perTraceWindowEnd)
	rw := perTraceRangeWindow(plan)
	if got := chplan.BoundMetricsPerTraceSubqueries(rw); got != chplan.Node(rw) {
		t.Fatalf("BoundMetricsPerTraceSubqueries rewrote a plan with no per-trace subquery: %T", got)
	}
	sql, args := emitSpans(t, wrapMetricsMatrix(t, plan, perTraceWindowStart, perTraceWindowEnd))
	const noSites = 0
	assertWindowedSites(t, sql, args, noSites, noOwnBinds)
}
