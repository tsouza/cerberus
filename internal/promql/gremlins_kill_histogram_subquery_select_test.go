// Tests in this file kill the LIVED gremlins mutants assigned to the
// histogram_native_subquery_select.go / histogram_native_bare.go cluster
// from a phase4-promql-i mutation run (mutation.yml, cerberus issue #2636 —
// the leg whose original run crashed on a network flake before any mutant
// ran). See gremlins_kill_test.go for the shared file-header convention
// this file follows.
//
// Three mutants are NOT addressed with a dedicated test here — all three
// are provably equivalent, not coverage gaps:
//
//   - histogram_native_subquery_select.go:102:31 (INVERT_LOGICAL,
//     `s.ExpHistogramTable == "" || ctx.metadataFullRange` -> `&&` inside
//     selectFnOverExpHistogramSubquery). This function re-validates the
//     identical condition one level down at line 118's
//     `!isExpHistogramValuedShape(sub.Expr, s, ctx)` — and EVERY leaf
//     recognizer isExpHistogramValuedShape dispatches to carries the same
//     guard, so whenever the top guard's condition holds,
//     isExpHistogramValuedShape is unconditionally false regardless of
//     which branch of the mutation let control reach it. Unlike
//     countOrGroupOverExpHistogramValue (gremlins_kill_histogram_binop_
//     count_test.go), no "did it even try to parse" tuple survives here:
//     EVERY rejection path in this function returns the literal zero-value
//     `histogramSubquerySelectShape{}, false` — never a partially built
//     one — so the two branches converge on the exact same return value on
//     every input.
//   - histogram_native_subquery_select.go:170:10 (CONDITIONALS_BOUNDARY,
//     `step < 0` -> `<= 0`). Two lines above, `if step == 0 { step =
//     defaultSubqueryStep }` (defaultSubqueryStep = time.Minute, a positive
//     constant) already eliminates step == 0 as a reachable value by the
//     time line 170 runs — step is either the caller's own non-zero
//     sub.Step or the positive default. `<` and `<=` decide identically
//     over every value except exactly 0, so no reachable input can make
//     the two operators disagree (the same reasoning
//     gremlins_kill_subquery_test.go's header gives for subquery.go:37:10).
//   - histogram_native_subquery_select.go:185:14 (INVERT_LOGICAL, `!matched
//     || chplan.RowShapeOf(input) != chplan.HistogramRowShape` -> `&&`).
//     lowerExpHistogramValuedShape's own contract (histogram_native_float_
//     fn.go) guarantees `matched == false` if and only if its very last
//     fallback ran, which always returns `input == nil` — and
//     chplan.RowShapeOf(nil) (no `case nil` in its type switch) always
//     falls through to its non-Histogram default. So `!matched` and
//     `RowShapeOf(input) != Histogram` are always EQUAL on every reachable
//     input: both true together (an unmatched sub-expression) or both false
//     together (every histogram-preserving recognizer this dispatch reaches
//     is documented to publish HistogramRowShape whenever it returns a nil
//     error, and a non-nil error already returns two lines above this
//     guard). OR and AND compute the same boolean whenever their two
//     operands are always equal.
//
// All three are confirmed by manually applying the mutation and running
// `go test ./internal/promql/...`: green.
package promql

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestSelectFnOverExpHistogramSubquery_ZeroRangeRejected kills the
// CONDITIONALS_BOUNDARY mutant at histogram_native_subquery_select.go:
// 118:22 (`sub.Range <= 0` -> `< 0`). A zero-duration subquery with an
// otherwise valid histogram-native inner and a resolvable eval anchor
// isolates the Range check alone: subqueryGridCtx tolerates a zero Range
// via its own sub-step-window clamp, so subqueryHasEvalAnchor genuinely
// returns true here — the same isolation strategy
// TestRangeFnOverExpHistogramSubquery_ZeroRangeRejected
// (histogram_native_range_family_gremlins_test.go) uses for its own
// sibling guard.
func TestSelectFnOverExpHistogramSubquery_ZeroRangeRejected(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sub := &parser.SubqueryExpr{
		Expr:  mustParseVectorSelector(t, "latency_exp_hist"),
		Range: 0,
		Step:  time.Minute,
	}
	call := &parser.Call{
		Func: parser.MustGetFunction("count_over_time"),
		Args: parser.Expressions{sub},
	}
	ctx := lowerCtx{start: at, end: at}
	if _, ok := selectFnOverExpHistogramSubquery(call, s, ctx); ok {
		t.Fatalf("expected zero-range subquery to be rejected; got ok=true " +
			"(mutant `<=`->`<` at histogram_native_subquery_select.go:118:22)")
	}
}

// TestLowerSelectFnOverExpHistogramSubquery_ZeroStepDefaults kills the
// CONDITIONALS_NEGATION mutant at histogram_native_subquery_select.go:
// 167:10 (`step == 0` -> `step != 0`). A subquery with no explicit step
// (`[5m:]`) must fall back to defaultSubqueryStep. With the negation, step
// stays 0 and flows into subqueryGridCtx -> epochFloor, which divides by
// the step duration — a genuine divide-by-zero panic, not merely a wrong
// answer (the same signature
// TestLowerExpHistogramRangeFnOverSubquery_ZeroStepDefaults pins for its
// own sibling in histogram_native_range_fn.go). The original code lowers
// this query cleanly; the mutant panics.
func TestLowerSelectFnOverExpHistogramSubquery_ZeroStepDefaults(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expr := mustParse(t, `count_over_time((latency_exp_hist)[5m:])`)
	if _, err := LowerAt(context.Background(), expr, s, at, at); err != nil {
		t.Fatalf("LowerAt: %v", err)
	}
}

// TestLowerSelectFnOverExpHistogramSubquery_InstantPinDoesNotBroadcast
// kills the INVERT_LOGICAL mutant at histogram_native_subquery_select.go:
// 200:21 (`ctx.rangeMode() && subqueryPinned(sub)` -> `||`). An
// `@`-pinned histogram-preserving subquery (last_over_time) reached via a
// plain instant LowerAt (ctx.rangeMode() == false) must take the ordinary
// single-window branch. With OR, subqueryPinned(sub) alone satisfies the
// condition and routes through the CrossJoin-over-StepGrid broadcast
// branch with ctx.step == 0 — a shape that must never appear outside real
// query_range lowering (mirrors
// TestLowerSumOrAvgMixedOrSubquerySelectFn_InstantPinDoesNotBroadcast's
// identical strategy for its own sibling guard).
func TestLowerSelectFnOverExpHistogramSubquery_InstantPinDoesNotBroadcast(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expr := mustParse(t, `last_over_time((latency_exp_hist)[5m:1m] @ 1700000000)`)
	plan, err := LowerAt(context.Background(), expr, s, at, at)
	if err != nil {
		t.Fatalf("LowerAt: %v", err)
	}
	assertNoCrossJoin(t, plan, "last_over_time instant query over an @-pinned histogram-native subquery")
}

// TestBareExpHistogramMatrixSelector_MetadataFullRangeShortCircuits kills
// the INVERT_LOGICAL mutant at histogram_native_bare.go:225:31, where
//
//	if s.ExpHistogramTable == "" || ctx.metadataFullRange {
//
// must reject on EITHER condition. bareExpHistogramMatrixSelector has no
// downstream call that independently re-checks this same guard (a plain
// type assertion plus IsExpHistogramMetric, both guard-free), so
// metadataFullRange alone is a clean, unmasked differentiator — mirrors
// TestRangeFnOverExpHistogram_MetadataFullRangeShortCircuits
// (histogram_native_range_family_gremlins_test.go).
func TestBareExpHistogramMatrixSelector_MetadataFullRangeShortCircuits(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	expr := mustParse(t, `latency_exp_hist[5m]`)
	if _, _, ok := bareExpHistogramMatrixSelector(expr, s, lowerCtx{metadataFullRange: true}); ok {
		t.Fatalf("expected metadataFullRange to reject recognition; got ok=true " +
			"(mutant `||`->`&&` at histogram_native_bare.go:225:31)")
	}
}

// TestLowerExpHistogramBareMatrix_PinnedAtNotOverwrittenByQueryEnd kills
// the INVERT_LOGICAL mutant at histogram_native_bare.go:265:25
// (`anchor.End.IsZero() && !ctx.end.IsZero()` -> `||`).
//
// The original AND only back-fills anchor.End from ctx.end when the
// selector carries NO pin of its own. An `@`-pinned top-level matrix
// selector already has a non-zero anchor.End; with the flip to OR, a
// non-zero ctx.end (any instant query reached via LowerAt) alone satisfies
// the condition and OVERWRITES the pin with the query's own eval time —
// silently answering the wrong window. Mirrors
// TestExpHistogramRangeFnWindowed_PinnedAtNotOverwrittenByQueryEnd's
// identical technique for its own sibling guard
// (histogram_native_range_family_gremlins_test.go).
func TestLowerExpHistogramBareMatrix_PinnedAtNotOverwrittenByQueryEnd(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	// 1767225600 == 2026-01-01T00:00:00Z.
	expr := mustParse(t, `latency_exp_hist[5m] @ 1767225600`)
	queryEnd := time.Date(2030, 5, 5, 0, 0, 0, 0, time.UTC)

	plan, err := LowerAt(context.Background(), expr, s, queryEnd, queryEnd)
	if err != nil {
		t.Fatalf("LowerAt: %v", err)
	}
	sql, params, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	var paramsText strings.Builder
	for _, p := range params {
		fmt.Fprintf(&paramsText, "%v;", p)
	}
	got := paramsText.String()
	if !strings.Contains(got, "2026-01-01") {
		t.Fatalf("expected the pinned @ timestamp (2026-01-01) among the emitted parameters, got:\n%s\nSQL:\n%s", got, sql)
	}
	if strings.Contains(got, "2030-05-05") {
		t.Fatalf("emitted parameters leaked the query end (2030-05-05) instead of the pinned @ "+
			"timestamp (mutant `&&`->`||` at histogram_native_bare.go:265:25 overwrites the pin "+
			"with ctx.end):\n%s", got)
	}
}

// TestLowerExpHistogramBareMatrix_PredicateAppliesFilter kills the
// CONDITIONALS_NEGATION mutant at histogram_native_bare.go:277:10 (`pred
// != nil` -> `== nil`) inside lowerExpHistogramBareMatrix:
//
//	if pred != nil {
//		input = &chplan.Filter{Input: scan, Predicate: pred}
//	}
//
// pred is built from buildPredicate(vs.LabelMatchers, s) andExpr'd with
// two time-bound predicates — for any real selector (at least the
// `__name__` matcher, plus the window bounds) pred is always non-nil, so
// the original code always wraps the scan in a Filter. The mutant flips
// the condition, wrapping in a Filter only when pred IS nil — leaving a
// real query's scan completely UNFILTERED, reading the whole
// exp-histogram table.
func TestLowerExpHistogramBareMatrix_PredicateAppliesFilter(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	vs := mustParseVectorSelector(t, "latency_exp_hist")
	ms := &parser.MatrixSelector{VectorSelector: vs, Range: 5 * time.Minute}

	plan, err := lowerExpHistogramBareMatrix(ms, vs, s, lowerCtx{})
	if err != nil {
		t.Fatalf("lowerExpHistogramBareMatrix: %v", err)
	}
	hp, ok := plan.(*chplan.HistogramProjection)
	if !ok {
		t.Fatalf("plan = %T, want *chplan.HistogramProjection", plan)
	}
	if _, ok := hp.Input.(*chplan.Filter); !ok {
		t.Fatalf("HistogramProjection.Input = %T, want *chplan.Filter (mutant `!=`->`==` at "+
			"histogram_native_bare.go:277:10 would leave the scan unfiltered whenever a real "+
			"predicate exists)", hp.Input)
	}
}
