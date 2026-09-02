// Tests in this file pin behaviour that a gremlins mutation run over
// internal/promql reported as LIVED against the exponential-histogram
// range-function family: histogram_native_range_fn.go,
// histogram_native_reset.go, histogram_native_count_present_over_time.go,
// histogram_native_dropping_shape.go,
// histogram_native_mixed_or_subquery_aggregate_range_fn.go and
// histogram_native_mixed_or_subquery_range_fn.go.
//
// See gremlins_kill_test.go for the shared convention this file follows:
// one Test... per mutant (or per tightly-related cluster of mutants), with
// the gremlins `file:line:col` id named in the doc comment and in the
// failure message.
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

// mustParseVectorSelector parses name as a bare selector and returns its
// *parser.VectorSelector, whose LabelMatchers (unlike a hand-built
// &parser.VectorSelector{Name: ...}) carry the `__name__` matcher every
// exp-histogram recognizer's IsExpHistogramMetric(metricNameFromMatchers(...))
// check needs to resolve the metric name at all.
func mustParseVectorSelector(t *testing.T, name string) *parser.VectorSelector {
	t.Helper()
	expr := mustParse(t, name)
	vs, ok := expr.(*parser.VectorSelector)
	if !ok {
		t.Fatalf("mustParse(%q) = %T, want *parser.VectorSelector", name, expr)
	}
	return vs
}

// gremlinsMixedOrPinnedQuery builds `<fn>((sum by (service) ((a) or
// (b)))[5m:1m] @ <pin>)` — the canonical shape
// sumOrAvgMixedOrSubqueryOuterFnRecognized admits, with sub.Timestamp set
// so subqueryPinned(sub) is true regardless of whether the OUTER query is
// itself instant or range mode.
func gremlinsMixedOrPinnedQuery(fn string) string {
	return fn + `((sum by (service) ((latency_exp_hist) or (other_metric)))[5m:1m] @ 1700000000)`
}

// assertNoCrossJoin fails the test if plan contains a *chplan.CrossJoin
// anywhere. A CrossJoin against a StepGrid is the range/broadcast fan-out
// shape; a plain instant query with no query_range context has no grid to
// fan across and must never build one.
func assertNoCrossJoin(t *testing.T, plan chplan.Node, label string) {
	t.Helper()
	found := false
	chplan.Walk(plan, func(n chplan.Node) bool {
		if _, ok := n.(*chplan.CrossJoin); ok {
			found = true
		}
		return true
	})
	if found {
		t.Fatalf("%s: plan unexpectedly contains a *chplan.CrossJoin for an instant (non-range) query", label)
	}
}

// TestSelectExpHistogramWindowSamples_ProjectionCapacityIsTight kills the
// ARITHMETIC_BASE mutant at histogram_native_range_fn.go:546:65 inside
// selectExpHistogramWindowSamples's slice-capacity hint:
//
//	projs := make([]chplan.Projection, 0, len(keyAliases)+len(aggs)+1)
//
// The function always appends exactly len(keyAliases) + 1 (the times
// alias) + len(aggs) projections — one per keyAlias, one for
// hqWindowAllTsListAlias, one per agg — so the original arithmetic gives
// an EXACT capacity: no append ever triggers a reallocation, and cap()
// stays at the requested value. Flipping the trailing `+1` to `-1`
// under-allocates by 2, forcing growslice to run during the last two
// appends; Go's growth rounding does not land back on the original exact
// value, so cap() differs from the original construction.
func TestSelectExpHistogramWindowSamples_ProjectionCapacityIsTight(t *testing.T) {
	t.Parallel()

	keyAliases := []string{"attrs_alias"}
	aggs := []chplan.AggFunc{{Fn: chplan.FnAny, Alias: "agg_alias"}}
	input := &chplan.Scan{Table: "otel_metrics_exponential_histogram"}

	// selection must NOT be histogramWindowAllSamples — that branch
	// returns input unchanged before ever reaching the mutated line.
	node := selectExpHistogramWindowSamples(input, aggs, keyAliases, histogramWindowEndpoints)

	proj, ok := node.(*chplan.Project)
	if !ok {
		t.Fatalf("selectExpHistogramWindowSamples returned %T, want *chplan.Project", node)
	}

	const want = 3 // len(keyAliases)=1 + len(aggs)=1 + 1 (the times alias)
	if got := len(proj.Projections); got != want {
		t.Fatalf("len(Projections) = %d, want %d", got, want)
	}
	if got := cap(proj.Projections); got != want {
		t.Fatalf("cap(Projections) = %d, want %d (mutant `+`->`-` at histogram_native_range_fn.go:546:65 under-allocates and forces a reallocation, leaving cap != %d)",
			got, want, want)
	}
}

// TestExpHistogramResetMaskStage_ProjectionCapacityIsTight is the sibling
// of the test above for the ARITHMETIC_BASE mutant at
// histogram_native_reset.go:`projs := make([]chplan.Projection, 0, len(keyAliases)+len(aggs)+1)` inside expHistogramResetMaskStage's
// identically-shaped capacity hint. The function appends exactly
// len(keyAliases) + len(aggs) + 1 (the reset-mask column) projections, so
// the same exact-capacity argument applies.
func TestExpHistogramResetMaskStage_ProjectionCapacityIsTight(t *testing.T) {
	t.Parallel()

	keyAliases := []string{"attrs_alias"}
	aggs := []chplan.AggFunc{{Fn: chplan.FnAny, Alias: "agg_alias"}}
	input := &chplan.Scan{Table: "otel_metrics_exponential_histogram"}

	node := expHistogramResetMaskStage(input, aggs, keyAliases)

	proj, ok := node.(*chplan.Project)
	if !ok {
		t.Fatalf("expHistogramResetMaskStage returned %T, want *chplan.Project", node)
	}

	const want = 3 // len(keyAliases)=1 + len(aggs)=1 + 1 (the reset-mask column)
	if got := len(proj.Projections); got != want {
		t.Fatalf("len(Projections) = %d, want %d", got, want)
	}
	if got := cap(proj.Projections); got != want {
		t.Fatalf("cap(Projections) = %d, want %d (mutant `+`->`-` at histogram_native_reset.go:`projs := make([]chplan.Projection, 0, len(keyAliases)+len(aggs)+1)` under-allocates and forces a reallocation, leaving cap != %d)",
			got, want, want)
	}
}

// TestCountPresentOverExpHistogram_MetadataFullRangeShortCircuits kills
// the INVERT_LOGICAL mutant at
// histogram_native_count_present_over_time.go:`if s.ExpHistogramTable == "" || ctx.metadataFullRange`, where
//
//	if s.ExpHistogramTable == "" || ctx.metadataFullRange {
//
// must reject on EITHER condition. countPresentOverExpHistogram has no
// downstream call that independently re-checks this same guard (unlike
// its subquery-composing siblings), so metadataFullRange alone —
// unaccompanied by an empty ExpHistogramTable — is a clean differentiator:
// flipping `||` to `&&` would let a metadata-driven lowering (which must
// never apply the instant-query LWR semantics this recognizer's lowering
// assumes) fall through to a "recognized" answer.
func TestCountPresentOverExpHistogram_MetadataFullRangeShortCircuits(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	expr := mustParse(t, `count_over_time(latency_exp_hist[5m])`)
	if _, _, _, ok := countPresentOverExpHistogram(expr, s, lowerCtx{metadataFullRange: true}); ok {
		t.Fatalf("expected metadataFullRange to reject recognition; got ok=true (mutant `||`->`&&` at histogram_native_count_present_over_time.go:65:31)")
	}
}

// TestCountPresentOverExpHistogram_ZeroRangeRejected kills the
// CONDITIONALS_BOUNDARY mutant at
// histogram_native_count_present_over_time.go:`ms.Range <= 0` (`ms.Range <= 0` ->
// `ms.Range < 0`). A zero-duration matrix selector is built by hand — the
// PromQL grammar refuses a literal `[0s]` — and must still be rejected.
func TestCountPresentOverExpHistogram_ZeroRangeRejected(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	call := &parser.Call{
		Func: parser.MustGetFunction("count_over_time"),
		Args: parser.Expressions{
			&parser.MatrixSelector{Range: 0, VectorSelector: mustParseVectorSelector(t, "latency_exp_hist")},
		},
	}
	if _, _, _, ok := countPresentOverExpHistogram(call, s, lowerCtx{}); ok {
		t.Fatalf("expected zero-range matrix selector to be rejected; got ok=true (mutant `<=`->`<` at histogram_native_count_present_over_time.go:`if !ok || ms.Range <= 0`)")
	}
}

// TestRangeFnOverExpHistogram_MetadataFullRangeShortCircuits kills the
// INVERT_LOGICAL mutant at histogram_native_range_fn.go:157:31. Like
// countPresentOverExpHistogram above, rangeFnOverExpHistogram has no
// downstream call re-checking the same guard, so this is a clean,
// unmasked differentiator.
func TestRangeFnOverExpHistogram_MetadataFullRangeShortCircuits(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	expr := mustParse(t, `rate(latency_exp_hist[5m])`)
	if _, ok := rangeFnOverExpHistogram(expr, s, lowerCtx{metadataFullRange: true}); ok {
		t.Fatalf("expected metadataFullRange to reject recognition; got ok=true (mutant `||`->`&&` at histogram_native_range_fn.go:157:31)")
	}
}

// TestRangeFnOverExpHistogram_ZeroRangeRejected kills the
// CONDITIONALS_BOUNDARY mutant at histogram_native_range_fn.go:`ms.Range <= 0`
// (`ms.Range <= 0` -> `ms.Range < 0`).
func TestRangeFnOverExpHistogram_ZeroRangeRejected(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	call := &parser.Call{
		Func: parser.MustGetFunction("rate"),
		Args: parser.Expressions{
			&parser.MatrixSelector{Range: 0, VectorSelector: mustParseVectorSelector(t, "latency_exp_hist")},
		},
	}
	if _, ok := rangeFnOverExpHistogram(call, s, lowerCtx{}); ok {
		t.Fatalf("expected zero-range matrix selector to be rejected; got ok=true (mutant `<=`->`<` at histogram_native_range_fn.go:170:21)")
	}
}

// TestRangeFnOverExpHistogramSubquery_ZeroRangeRejected kills the
// CONDITIONALS_BOUNDARY mutant at histogram_native_range_fn.go:`sub.Range <= 0`
// (`sub.Range <= 0` -> `sub.Range < 0`). Unlike the guard-clause
// INVERT_LOGICAL mutant on this same function's first line (masked by
// isExpHistogramValuedShape's own identical guard — see this file's
// header note in the final report), the Range boundary is examined only
// here, so a zero-range subquery with an otherwise valid histogram-native
// inner cleanly differentiates.
func TestRangeFnOverExpHistogramSubquery_ZeroRangeRejected(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sub := &parser.SubqueryExpr{
		Expr:  mustParseVectorSelector(t, "latency_exp_hist"),
		Range: 0,
		Step:  time.Minute,
	}
	call := &parser.Call{
		Func: parser.MustGetFunction("sum_over_time"),
		Args: parser.Expressions{sub},
	}
	ctx := lowerCtx{start: at, end: at}
	if _, ok := rangeFnOverExpHistogramSubquery(call, s, ctx); ok {
		t.Fatalf("expected zero-range subquery to be rejected; got ok=true (mutant `<=`->`<` at histogram_native_range_fn.go:223:22)")
	}
}

// TestLowerExpHistogramRangeFnOverSubquery_ZeroStepDefaults kills the
// CONDITIONALS_NEGATION mutant at histogram_native_range_fn.go:lowerExpHistogramRangeFnOverSubquery:`if step == 0`
// (`step == 0` -> `step != 0`) inside lowerExpHistogramRangeFnOverSubquery.
//
// A subquery with no explicit step (`[5m:]`) must fall back to
// defaultSubqueryStep. With the negation, step stays 0 and flows into
// subqueryGridCtx -> epochFloor, which divides by the step duration —
// a genuine divide-by-zero panic, not merely a wrong answer. The original
// code lowers this query cleanly; the mutant panics.
func TestLowerExpHistogramRangeFnOverSubquery_ZeroStepDefaults(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expr := mustParse(t, `sum_over_time((latency_exp_hist)[5m:])`)
	if _, err := LowerAt(context.Background(), expr, s, at, at); err != nil {
		t.Fatalf("LowerAt: %v", err)
	}
}

// TestExpHistogramRangeFnWindowed_PinnedAtNotOverwrittenByQueryEnd kills
// the INVERT_LOGICAL mutant at histogram_native_range_fn.go:427:25
// (`anchor.End.IsZero() && !ctx.end.IsZero()` -> `||`) inside
// expHistogramRangeFnWindowed.
//
// The original AND only back-fills anchor.End from ctx.end when the
// selector carries NO pin of its own. An `@`-pinned selector already has
// a non-zero anchor.End; with the flip to OR, a non-zero ctx.end (any
// instant query reached via LowerAt) alone satisfies the condition and
// OVERWRITES the pin with the query's own eval time — silently answering
// the wrong window.
func TestExpHistogramRangeFnWindowed_PinnedAtNotOverwrittenByQueryEnd(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	// 1767225600 == 2026-01-01T00:00:00Z.
	expr := mustParse(t, `rate(latency_exp_hist[5m] @ 1767225600)`)
	queryEnd := time.Date(2030, 5, 5, 0, 0, 0, 0, time.UTC)

	plan, err := LowerAt(context.Background(), expr, s, queryEnd, queryEnd)
	if err != nil {
		t.Fatalf("LowerAt: %v", err)
	}
	sql, params, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	// Values are emitted as `?` placeholders (invariant 10 — typed Frags
	// only), so the anchor's rendered date lands in the parameter list,
	// not in the SQL text itself.
	var paramsText strings.Builder
	for _, p := range params {
		fmt.Fprintf(&paramsText, "%v;", p)
	}
	got := paramsText.String()
	if !strings.Contains(got, "2026-01-01") {
		t.Fatalf("expected the pinned @ timestamp (2026-01-01) among the emitted parameters, got:\n%s\nSQL:\n%s", got, sql)
	}
	if strings.Contains(got, "2030-05-05") {
		t.Fatalf("emitted parameters leaked the query end (2030-05-05) instead of the pinned @ timestamp (mutant `&&`->`||` at histogram_native_range_fn.go:427:25 overwrites the pin with ctx.end):\n%s", got)
	}
}

// TestLabelCallOverExpHistogramDroppingShape_LabelReplaceExactArity kills
// two mutants at once, both on histogram_native_dropping_shape.go:`len(call.Args) != 5`:
//   - CONDITIONALS_NEGATION at :204:25 (`s.ExpHistogramTable == ""` ->
//     `!= ""`): with the negation, ANY non-empty ExpHistogramTable — the
//     normal, default schema — makes the guard bail, so this test's
//     ordinary DefaultOTelMetrics() schema alone differentiates.
//   - CONDITIONALS_NEGATION at :213:21 (`len(call.Args) != 5` -> `== 5`):
//     with the negation, exactly-5-args (the only valid label_replace
//     shape) is rejected instead of accepted.
//
// The original code recognises the shape under the standard schema with a
// well-formed 5-arg label_replace call; either mutant makes it reject.
func TestLabelCallOverExpHistogramDroppingShape_LabelReplaceExactArity(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	expr := mustParse(t, `label_replace(latency_exp_hist + 1, "copy", "$1", "service", "(.*)")`)
	call, ok := labelCallOverExpHistogramDroppingShape(expr, s, lowerCtx{})
	if !ok {
		t.Fatalf("expected 5-arg label_replace wrapping a drop-family binop to be recognised under the default schema; got ok=false (mutants at histogram_native_dropping_shape.go:204:25 and :213:21)")
	}
	if call.Func.Name != fnLabelReplace {
		t.Fatalf("call.Func.Name = %q, want %q", call.Func.Name, fnLabelReplace)
	}
}

// TestLabelCallOverExpHistogramDroppingShape_LabelJoinMinArity kills the
// CONDITIONALS_BOUNDARY mutant at histogram_native_dropping_shape.go:`len(call.Args) < 3`
// (`len(call.Args) < 3` -> `<= 3`). The minimum valid label_join call has
// exactly 3 args (vector, dst, separator, zero src labels) — the parser
// itself refuses this degenerate shape, so the Call is built by hand.
func TestLabelCallOverExpHistogramDroppingShape_LabelJoinMinArity(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	binExpr := mustParse(t, `latency_exp_hist + 1`)
	call := &parser.Call{
		Func: parser.MustGetFunction("label_join"),
		Args: parser.Expressions{binExpr, &parser.StringLiteral{Val: "copy"}, &parser.StringLiteral{Val: "-"}},
	}
	if _, ok := labelCallOverExpHistogramDroppingShape(call, s, lowerCtx{}); !ok {
		t.Fatalf("expected minimum-arity (3-arg) label_join wrapping a drop-family binop to be recognised; got ok=false (mutant `<`->`<=` at histogram_native_dropping_shape.go:217:21)")
	}
}

// TestSumOrAvgMixedOrSubqueryOuterFnRecognized_ZeroRangeRejected kills two
// mutants on histogram_native_mixed_or_subquery_aggregate_range_fn.go's
// line 124 at once:
//   - CONDITIONALS_BOUNDARY at :124:22 (`sub.Range <= 0` -> `< 0`).
//   - INVERT_LOGICAL at :124:27 (the second `||`, between the Range check
//     and `!subqueryHasEvalAnchor(sub, ctx)`, flipped to `&&`).
//
// sub.Range = 0 with everything else valid (a real mixed-or inner,
// eval-anchor-resolvable ctx) makes the ORIGINAL code reject via the
// Range check alone (short-circuiting subqueryHasEvalAnchor away
// entirely). subqueryGridCtx tolerates a zero Range via its own
// sub-step-window clamp, so subqueryHasEvalAnchor(sub, ctx) genuinely
// returns true here — which is exactly what lets the :124:27 AND-mutant
// (needing BOTH operands true) fall through to a false "not rejected"
// verdict, and what lets the :124:22 boundary-mutant do the same once the
// Range check alone no longer rejects.
func TestSumOrAvgMixedOrSubqueryOuterFnRecognized_ZeroRangeRejected(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sub := &parser.SubqueryExpr{
		Expr:  mustParse(t, `sum by (service) ((latency_exp_hist) or (other_metric))`),
		Range: 0,
		Step:  time.Minute,
	}
	call := &parser.Call{
		Func: parser.MustGetFunction("count_over_time"),
		Args: parser.Expressions{sub},
	}
	ctx := lowerCtx{start: at, end: at}
	if _, ok := sumOrAvgMixedOrSubqueryOuterFnRecognized(call, s, ctx); ok {
		t.Fatalf("expected zero-range subquery to be rejected; got ok=true (mutants at histogram_native_mixed_or_subquery_aggregate_range_fn.go:124:22 and :124:27)")
	}
}

// TestSumOrAvgMixedOrSubqueryOuterFnRecognized_NonSubqueryArgNoPanic kills
// the INVERT_LOGICAL mutant at
// histogram_native_mixed_or_subquery_aggregate_range_fn.go:124:9 — the
// FIRST `||` on that line (`!ok || sub.Range <= 0 ...`), flipped to `&&`.
//
// When c.Args[0] is not a *parser.SubqueryExpr, the failed type assertion
// leaves `sub` nil. The original `||` short-circuits on `!ok` and never
// evaluates `sub.Range`. `&&`, given precedence (`&&` binds tighter than
// `||`), regroups the expression as `(!ok && sub.Range <= 0) ||
// !subqueryHasEvalAnchor(...)`, and `&&` evaluates its second operand
// whenever the first is true — dereferencing the nil `sub` and panicking.
// The original code returns ok=false cleanly; the mutant crashes the test.
func TestSumOrAvgMixedOrSubqueryOuterFnRecognized_NonSubqueryArgNoPanic(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	call := &parser.Call{
		Func: parser.MustGetFunction("count_over_time"),
		Args: parser.Expressions{&parser.VectorSelector{Name: "latency_exp_hist"}},
	}
	if _, ok := sumOrAvgMixedOrSubqueryOuterFnRecognized(call, s, lowerCtx{}); ok {
		t.Fatalf("expected non-subquery argument to be rejected; got ok=true")
	}
}

// TestLowerSumOrAvgMixedOrSubqueryOuterFn_ZeroStepDefaults kills the
// CONDITIONALS_NEGATION mutant at
// histogram_native_mixed_or_subquery_aggregate_range_fn.go:168:10
// (`step == 0` -> `step != 0`), the sibling of the range_fn.go:266:10
// kill above but inside lowerSumOrAvgMixedOrSubqueryOuterFn. Same
// divide-by-zero-in-epochFloor signature: the original lowers cleanly,
// the mutant panics.
func TestLowerSumOrAvgMixedOrSubqueryOuterFn_ZeroStepDefaults(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expr := mustParse(t, `count_over_time((sum by (service) ((latency_exp_hist) or (other_metric)))[5m:])`)
	if _, err := LowerAt(context.Background(), expr, s, at, at); err != nil {
		t.Fatalf("LowerAt: %v", err)
	}
}

// TestLowerSumOrAvgMixedOrSubquerySelectFn_InstantPinDoesNotBroadcast
// kills the INVERT_LOGICAL mutant at
// histogram_native_mixed_or_subquery_aggregate_range_fn.go:223:21
// (`ctx.rangeMode() && subqueryPinned(sub)` -> `||`) inside
// lowerSumOrAvgMixedOrSubquerySelectFn.
//
// An `@`-pinned mixed-or subquery reached via a plain instant LowerAt
// (ctx.rangeMode() == false) must take the ordinary single-window branch.
// With OR, subqueryPinned(sub) alone satisfies the condition and routes
// through the CrossJoin-over-StepGrid broadcast branch with ctx.step == 0
// — a shape that must never appear outside real query_range lowering.
func TestLowerSumOrAvgMixedOrSubquerySelectFn_InstantPinDoesNotBroadcast(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expr := mustParse(t, gremlinsMixedOrPinnedQuery("count_over_time"))
	plan, err := LowerAt(context.Background(), expr, s, at, at)
	if err != nil {
		t.Fatalf("LowerAt: %v", err)
	}
	assertNoCrossJoin(t, plan, "count_over_time instant query over an @-pinned mixed-or subquery")
}

// TestLowerSumOrAvgMixedOrSubqueryFoldFn_InstantPinDoesNotBroadcast kills
// two mutants at once, both `ctx.rangeMode() && subqueryPinned(sub)` ->
// `||` guards reached from lowerSumOrAvgMixedOrSubqueryFoldFn for the
// SAME query:
//   - :335:21 inside lowerHistFoldOverPureSubqueryBranch (the histogram
//     arm).
//   - :374:21 inside lowerFloatFoldOverPureSubqueryBranch (the float arm).
//
// sum_over_time reaches the FOLD family (as opposed to the SELECT family
// count_over_time exercises above), which runs BOTH arms and recombines
// them — so a single instant, `@`-pinned query exercises both sites in
// one lowering, and a CrossJoin surfacing from EITHER arm's own mutated
// guard is caught by the same assertion.
func TestLowerSumOrAvgMixedOrSubqueryFoldFn_InstantPinDoesNotBroadcast(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expr := mustParse(t, gremlinsMixedOrPinnedQuery("sum_over_time"))
	plan, err := LowerAt(context.Background(), expr, s, at, at)
	if err != nil {
		t.Fatalf("LowerAt: %v", err)
	}
	assertNoCrossJoin(t, plan, "sum_over_time instant query over an @-pinned mixed-or subquery")
}

// TestMixedOrSubqueryOuterFn_ArgCountVsNameGuard kills the INVERT_LOGICAL
// mutant at histogram_native_mixed_or_subquery_range_fn.go:116:22
// (`len(c.Args) != 1 || !isHistogramSubqueryOuterFnName(c.Func.Name)` ->
// `&&`).
//
// A 2-arg call to a recognised name (count_over_time takes exactly 1
// argument) must reject on arity alone. With AND, a recognised name
// (`!isHistogramSubqueryOuterFnName(...)` == false) makes the whole
// conjunction false regardless of arity, so the mutant proceeds to index
// c.Args[0] as if it were the sole argument and can recognise the shape
// anyway.
func TestMixedOrSubqueryOuterFn_ArgCountVsNameGuard(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sub := &parser.SubqueryExpr{
		Expr:  mustParse(t, `(latency_exp_hist) or (other_metric)`),
		Range: 5 * time.Minute,
		Step:  time.Minute,
	}
	call := &parser.Call{
		Func: parser.MustGetFunction("count_over_time"),
		Args: parser.Expressions{sub, &parser.NumberLiteral{Val: 1}},
	}
	ctx := lowerCtx{start: at, end: at}
	if _, _, _, ok := mixedOrSubqueryOuterFn(call, s, ctx); ok {
		t.Fatalf("expected a 2-arg call to a 1-arg function to be rejected; got ok=true (mutant `||`->`&&` at histogram_native_mixed_or_subquery_range_fn.go:116:22)")
	}
}

// TestMixedOrSubqueryOuterFn_ZeroRangeRejected is the
// histogram_native_mixed_or_subquery_range_fn.go sibling of
// TestSumOrAvgMixedOrSubqueryOuterFnRecognized_ZeroRangeRejected above,
// killing:
//   - CONDITIONALS_BOUNDARY at :120:22 (`sub.Range <= 0` -> `< 0`).
//   - INVERT_LOGICAL at :120:27 (the second `||` -> `&&`).
func TestMixedOrSubqueryOuterFn_ZeroRangeRejected(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sub := &parser.SubqueryExpr{
		Expr:  mustParse(t, `(latency_exp_hist) or (other_metric)`),
		Range: 0,
		Step:  time.Minute,
	}
	call := &parser.Call{
		Func: parser.MustGetFunction("count_over_time"),
		Args: parser.Expressions{sub},
	}
	ctx := lowerCtx{start: at, end: at}
	if _, _, _, ok := mixedOrSubqueryOuterFn(call, s, ctx); ok {
		t.Fatalf("expected zero-range subquery to be rejected; got ok=true (mutants at histogram_native_mixed_or_subquery_range_fn.go:120:22 and :120:27)")
	}
}

// TestMixedOrSubqueryOuterFn_NonSubqueryArgNoPanic is the
// histogram_native_mixed_or_subquery_range_fn.go sibling of
// TestSumOrAvgMixedOrSubqueryOuterFnRecognized_NonSubqueryArgNoPanic
// above, killing the INVERT_LOGICAL mutant at :120:9 (the first `||` on
// that line -> `&&`, causing a nil-pointer panic on `sub.Range` when
// c.Args[0] is not a subquery).
func TestMixedOrSubqueryOuterFn_NonSubqueryArgNoPanic(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	call := &parser.Call{
		Func: parser.MustGetFunction("count_over_time"),
		Args: parser.Expressions{&parser.VectorSelector{Name: "latency_exp_hist"}},
	}
	if _, _, _, ok := mixedOrSubqueryOuterFn(call, s, lowerCtx{}); ok {
		t.Fatalf("expected non-subquery argument to be rejected; got ok=true")
	}
}
