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
// compare root leg — must carry, on each per-trace subquery, the RangeWindow
// input span (Start - Range, End] widened by the maximum trace duration on
// both sides, and on the plain span rows each one filters, the input span
// itself.

// perTraceWindowStep is the step of the matrix RangeWindow these tests wrap.
const perTraceWindowStep = time.Minute

// boundSites counts the renders of a per-trace shape's bounds in one
// statement: perTrace counts the per-trace subqueries (a lookup or a
// membership subquery), each bound by the widened input span; rows counts the
// plain span rows they filter, each bound by the input span itself.
type boundSites struct {
	perTrace, rows int
}

// plus returns the site counts of rendering both a and b.
func (a boundSites) plus(b boundSites) boundSites {
	return boundSites{perTrace: a.perTrace + b.perTrace, rows: a.rows + b.rows}
}

// times returns the site counts of rendering a n times.
func (a boundSites) times(n int) boundSites {
	return boundSites{perTrace: a.perTrace * n, rows: a.rows * n}
}

// Bounded sites of one render of a per-trace shape, and the renders of the
// metrics input each statement makes.
var (
	lookupSites     = boundSites{perTrace: 1, rows: 1} // the lookup and the span rows joined onto it
	membershipSites = boundSites{perTrace: 1, rows: 1} // the membership subquery and the span rows it filters
	selectionSites  = boundSites{perTrace: 1}          // a compare() selection's membership subquery
	noSites         = boundSites{}
)

const (
	inputRendersHistogram = 2 // the sample arm and the zero-fill bucket discovery arm
	inputRendersGrouped   = 2 // the sample arm and the zero-fill group discovery arm
	inputRendersCompare   = 2 // the span leg and the root lookup's trace-id seed
	structuralLeftRenders = 2 // a structural join's left operand: the closure seed and the rooted-trace anchor
)

// ownBinds counts the binds of the bound instants an emitter makes for a
// bound of its own, outside the per-trace subqueries and their span rows.
type ownBinds struct {
	perTrace, rows int
}

// noOwnBinds is a statement whose emitter binds neither bound for itself.
var noOwnBinds = ownBinds{}

// compareRootSites is the compare emitter's own bound site on the root
// lookup: a non-root selection's root scan reads the widened input span, since
// its root may precede the window.
var compareRootSites = boundSites{perTrace: 1}

// compareRootBinds counts the compare emitter's own bind for the root lookup
// outside a bound site: the trace-id seed reads the input span itself.
var compareRootBinds = ownBinds{rows: 1}

// windowedBoundPair is the SQL both bounds render as: an exclusive lower and
// an inclusive upper conjunct, as the emitter's own `(anchor - range, anchor]`
// window is.
const windowedBoundPair = "(`Timestamp` > fromUnixTimestamp64Nano(?)) AND (`Timestamp` <= fromUnixTimestamp64Nano(?))"

// perTraceWindowStart / perTraceWindowEnd are the request window the cases
// wrap; the RangeWindow input span is (start - step, end].
var (
	perTraceWindowStart = time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	perTraceWindowEnd   = perTraceWindowStart.Add(time.Hour)
)

// perTraceLookback is the maximum trace duration the lowered plans carry.
func perTraceLookback() time.Duration { return tracesSchema().MaxTraceDuration }

// inputSpanNanos returns the bound instants of the RangeWindow input span
// widened by lookback on both sides.
func inputSpanNanos(lookback time.Duration) (lower, upper int64) {
	return perTraceWindowStart.Add(-perTraceWindowStep - lookback).UnixNano(), perTraceWindowEnd.Add(lookback).UnixNano()
}

// countBoundArgs returns how many times the lower and the upper instant of the
// input span widened by lookback are bound in args.
func countBoundArgs(args []any, lookback time.Duration) (lower, upper int) {
	lo, hi := inputSpanNanos(lookback)
	for _, a := range args {
		v, ok := a.(int64)
		if !ok {
			continue
		}
		switch v {
		case lo:
			lower++
		case hi:
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

// assertWindowedSites fails unless the statement renders a bound once per
// per-trace subquery and per plain row source it filters, each time the
// statement renders them; binds the widened input span's instants once per
// per-trace subquery plus own.perTrace; and binds the input span's own
// instants once per plain row source plus own.rows — so each per-trace
// subquery carries the widened bound and each row source the unwidened one.
func assertWindowedSites(t *testing.T, sql string, args []any, sites boundSites, own ownBinds) {
	t.Helper()
	if lookback := perTraceLookback(); lookback <= 0 {
		t.Fatalf("maximum trace duration %s: the widened and the unwidened bound cannot be told apart", lookback)
	}
	if got, want := strings.Count(sql, windowedBoundPair), sites.perTrace+sites.rows; got != want {
		t.Fatalf("bound %q rendered %d times, want %d\n--- sql ---\n%s", windowedBoundPair, got, want, sql)
	}
	for _, tc := range []struct {
		name     string
		lookback time.Duration
		want     int
	}{
		{"per-trace (widened)", perTraceLookback(), sites.perTrace + own.perTrace},
		{"row source (input span)", 0, sites.rows + own.rows},
	} {
		lower, upper := countBoundArgs(args, tc.lookback)
		if lower != tc.want || upper != tc.want {
			t.Fatalf("%s bound instants bound %d (lower) / %d (upper) times, want %d each\n--- sql ---\n%s",
				tc.name, lower, upper, tc.want, sql)
		}
	}
}

// TestMetricsPerTraceSubqueriesWindowed pins the bound on the matrix
// statement of each metrics function family over each per-trace shape.
func TestMetricsPerTraceSubqueriesWindowed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		query string
		// sites counts the bounded per-trace subqueries and the plain row
		// sources they filter, times the renders of the metrics input.
		sites boundSites
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
		{"lookup_and_membership", `{ rootName = name && traceDuration > 1s } | rate()`, boundSites{perTrace: 2, rows: 1}},
		{"quantile", `{ rootName = name } | quantile_over_time(duration, .9)`, lookupSites},
		{"grouped", `{ rootName = name } | rate() by (resource.service.name)`, lookupSites.times(inputRendersGrouped)},
		// A per-trace shape on one operand of a structural join, beside an
		// operand that carries none.
		{"structural_membership_left", `{ rootName = "x" } >> { kind = server } | rate()`, membershipSites.times(structuralLeftRenders)},
		{"structural_lookup_left", `{ rootName = name } ~ { kind = server } | rate()`, lookupSites.times(structuralLeftRenders)},
		{"structural_duration_left", `{ traceDuration > 1s } > { kind = server } | count_over_time()`, membershipSites.times(structuralLeftRenders)},
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
	assertWindowedSites(t, sql, args, lookupSites.times(inputRendersHistogram), noOwnBinds)
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
	assertWindowedSites(t, sql, args, selectionSites.plus(lookupSites.times(inputRendersCompare)).plus(compareRootSites), compareRootBinds)

	ctx := chsql.WithSpansTable(context.Background(), tracesSchema().SpansTable)
	legSQL, legArgs, _, err := chsql.EmitCompareRootLeg(ctx, rw)
	if err != nil {
		t.Fatalf("EmitCompareRootLeg: %v", err)
	}
	// The root leg seeds itself from Inner: the lookup and the span rows.
	assertWindowedSites(t, legSQL, legArgs, lookupSites.plus(compareRootSites), compareRootBinds)
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
	assertWindowedSites(t, sql, args, noSites, noOwnBinds)
}
