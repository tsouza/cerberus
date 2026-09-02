// Tests in this file kill LIVED gremlins mutants reported on the
// phase4-promql-c / -f / -other / -quantile legs (cerberus issue #2949),
// across the exp-histogram recognizers' shape guards and the classic/native
// histogram_quantile window guards. See gremlins_kill_test.go for the shared
// file-header convention this file follows.
package promql

import (
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestMixedOrSubqueryOuterFn_RequiresAConfiguredTable kills the
// CONDITIONALS_NEGATION mutant on
//
//	histogram_native_mixed_or_subquery_range_fn.go:mixedOrSubqueryOuterFn:`s.ExpHistogramTable == ""`
//
// which rewrites it to `s.ExpHistogramTable != ""` inside
// [mixedOrSubqueryOuterFn].
//
// The guard reads "bail when the schema declares NO exp-histogram table".
// Negated it reads "bail when the schema DOES declare one", which is every
// real deployment: the recognizer then answers false for every input it
// exists to accept, and the mixed-or subquery shape silently stops being
// recognised.
//
// The mutant is therefore killed by a POSITIVE recognition — the negative
// direction is what the original already does. This is the only mutant on
// this leg that a test can reach; the file's NOT KILLABLE footer adjudicates
// the rest.
func TestMixedOrSubqueryOuterFn_RequiresAConfiguredTable(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	if s.ExpHistogramTable == "" {
		t.Fatal("positive control: the default schema declares no ExpHistogramTable, so the guard " +
			"under test would bail for the original reason and the assertion below would prove " +
			"nothing")
	}

	// The subquery inner is the bare mixed set-op [wrapMixedOrSubqueryInner]
	// admits, and `@` pins it so subqueryHasEvalAnchor resolves without an
	// ambient query time.
	const q = `last_over_time(((latency_exp_hist) or (other_metric))[5m:1m] @ 1700000000)`
	call, ok := mustParse(t, q).(*parser.Call)
	if !ok {
		t.Fatalf("mustParse(%q) = %T, want *parser.Call", q, mustParse(t, q))
	}

	sub, b, rebuild, ok := mixedOrSubqueryOuterFn(call, s, lowerCtx{})
	if !ok {
		t.Fatalf("mixedOrSubqueryOuterFn(%q) = ok false; want true — the schema DOES declare an "+
			"exp-histogram table, so the table guard must not fire (mutant `==`->`!=` at "+
			"histogram_native_mixed_or_subquery_range_fn.go:mixedOrSubqueryOuterFn:`s.ExpHistogramTable == \"\"` makes it fire on every "+
			"configured deployment)", q)
	}
	if sub == nil || b == nil || rebuild == nil {
		t.Fatalf("mixedOrSubqueryOuterFn(%q) returned ok=true with sub=%v b=%v rebuild==nil:%v; a "+
			"recognised shape must carry all three", q, sub, b, rebuild == nil)
	}
}

// TestIsSyntheticScalarPlan_RejectsANonEmptyMetricName kills the
// INVERT_LOGICAL mutant on the `||` of
//
//	synthetic.go:`lit, ok := mn.Expr.(*chplan.LitString); !ok || lit.V != ""`
//
// inside [isSyntheticScalarPlan].
//
// The projection must be the EMPTY string literal, because a synthetic scalar
// plan carries no identifying labels — the function's own comment says a
// non-empty literal "means the plan carries identifying labels the fold would
// erase". Under `&&` the rejection needs the expression to be both a
// non-LitString AND a non-empty one, which nothing is: a `LitString("x")`
// satisfies only the second half, so the mutant reports a plan that names a
// metric as a scalar plan and the fold erases the name.
func TestIsSyntheticScalarPlan_RejectsANonEmptyMetricName(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()

	if !isSyntheticScalarPlan(syntheticScalarPlan(s, &chplan.LitString{V: ""}), s) {
		t.Fatal("positive control: a plan whose MetricName projection is LitString(\"\") is not " +
			"recognised as synthetic; the negative assertion below would then hold for the " +
			"wrong reason")
	}
	if isSyntheticScalarPlan(syntheticScalarPlan(s, &chplan.LitString{V: "some_metric"}), s) {
		t.Fatal("isSyntheticScalarPlan reported a plan whose MetricName projection is " +
			"LitString(\"some_metric\") as synthetic; a synthetic scalar plan carries no " +
			"identifying labels (the `||`->`&&` mutant on " +
			"synthetic.go:`lit, ok := mn.Expr.(*chplan.LitString); !ok || lit.V != \"\"` stops the non-empty " +
			"literal disqualifying on its own)")
	}
}

// syntheticScalarPlan builds the four-projection Project over a OneRow that
// [isSyntheticScalarPlan] accepts, with the MetricName slot's expression left
// to the caller so a test can vary the one projection under examination.
func syntheticScalarPlan(s schema.Metrics, metricName chplan.Expr) chplan.Node {
	return &chplan.Project{
		Input: &chplan.OneRow{},
		Projections: []chplan.Projection{
			{Expr: metricName, Alias: s.MetricNameColumn},
			{Expr: emptyAttrsMap(), Alias: s.AttributesColumn},
			{Expr: chplan.NowNano(), Alias: s.TimestampColumn},
			{Expr: &chplan.LitFloat{V: 1}, Alias: s.ValueColumn},
		},
	}
}

// TestSortOverMixedExpHistogramSetOp_TakesExactlyOneArgument kills the
// CONDITIONALS_NEGATION mutant on
//
//	histogram_native_mixed_or_sort.go:`len(c.Args) != 1`
//
// rewritten to `len(c.Args) == 1` inside [sortOverMixedExpHistogramSetOp].
//
// `sort` / `sort_desc` take exactly one argument, so the guard rejects
// everything else. Negated, it rejects exactly the arity the function exists
// to serve and admits every other, so the one-argument sort over a mixed
// set-op stops being recognised.
func TestSortOverMixedExpHistogramSetOp_TakesExactlyOneArgument(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	const q = `sort((latency_exp_hist) or (other_metric))`
	call, ok := mustParse(t, q).(*parser.Call)
	if !ok {
		t.Fatalf("mustParse(%q) = %T, want *parser.Call", q, mustParse(t, q))
	}

	b, ok := sortOverMixedExpHistogramSetOp(call, s, lowerCtx{})
	if !ok || b == nil {
		t.Fatalf("sortOverMixedExpHistogramSetOp(%q) = ok %v; want true — `sort` takes exactly "+
			"one argument and this call has one (mutant `!=`->`==` at "+
			"histogram_native_mixed_or_sort.go:`len(c.Args) != 1` rejects precisely the admitted arity)",
			q, ok)
	}
}

// TestExpHistogramWindowTemporalityExpr_BothConditionsDisqualify kills the
// INVERT_LOGICAL mutant on the `||` of
//
//	histogram_quantile_native_window.go:`!needsTemporalityAgg(windowFn) || s.AggregationTemporalityColumn == ""`
//
// inside [expHistogramWindowTemporalityExpr].
//
// There is no temporality column reference to emit when EITHER the window
// function does not need a temporality aggregate, or the schema configures no
// such column. Under `&&` a window function that needs no temporality still
// gets the column reference back as long as the schema happens to configure
// one — a reference to an alias the window stage never projected.
//
// `last_over_time` is the disqualifying case: it reads a single sample rather
// than folding a counter, so it needs no temporality aggregate, while the
// default schema does configure the column.
func TestExpHistogramWindowTemporalityExpr_BothConditionsDisqualify(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	if s.AggregationTemporalityColumn == "" {
		t.Fatal("positive control: the default schema configures no AggregationTemporalityColumn, " +
			"so the second disjunct already disqualifies and the assertion below proves nothing")
	}
	if !needsTemporalityAgg(rateWindowFn) {
		t.Fatalf("positive control: %q is expected to need a temporality aggregate; without a "+
			"window function that does, the guard's first disjunct is never the deciding one",
			rateWindowFn)
	}

	if got := expHistogramWindowTemporalityExpr(s, rateWindowFn); got == nil {
		t.Fatalf("expHistogramWindowTemporalityExpr(%q) = nil; want the temporality column "+
			"reference — the window function needs the aggregate and the schema configures the "+
			"column", rateWindowFn)
	}
	if got := expHistogramWindowTemporalityExpr(s, lastOverTimeWindowFn); got != nil {
		t.Fatalf("expHistogramWindowTemporalityExpr(%q) = %#v; want nil — a window function that "+
			"needs no temporality aggregate must not reference the column even when the schema "+
			"configures one (the `||`->`&&` mutant on "+
			"histogram_quantile_native_window.go:`!needsTemporalityAgg(windowFn) || s.AggregationTemporalityColumn == \"\"` "+
			"stops that disqualifying on its own)", lastOverTimeWindowFn, got)
	}
}

// TestLabelCallOverExpHistogram_LabelJoinAcceptsThreeArguments kills the
// CONDITIONALS_BOUNDARY mutant on
//
//	histogram_native_label_replace.go:`len(call.Args) < 3`
//
// rewritten to `len(call.Args) <= 3` inside [labelCallOverExpHistogram].
//
// `label_join(v, dst, sep)` with no source labels is a well-formed call: the
// separator-joined result of zero sources is the empty string, and PromQL
// admits it. Three is therefore the MINIMUM arity, not a rejected one, and
// the boundary belongs below it. `<= 3` rejects the minimal call, so
// `label_join` over an exp-histogram stops being recognised at exactly the
// arity the guard was written to admit.
func TestLabelCallOverExpHistogram_LabelJoinAcceptsThreeArguments(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	const q = `label_join(latency_exp_hist, "dst", ",")`
	expr := mustParse(t, q)

	call, ok := labelCallOverExpHistogram(expr, s, lowerCtx{})
	if !ok || call == nil {
		t.Fatalf("labelCallOverExpHistogram(%q) = ok %v; want true — three arguments is "+
			"`label_join`'s minimum, not a rejected arity (mutant `<`->`<=` at "+
			"histogram_native_label_replace.go:`len(call.Args) < 3` rejects it)", q, ok)
	}
	if len(call.Args) != 3 {
		t.Fatalf("the query under test parsed to %d arguments, not the 3 the boundary is about; "+
			"the test no longer exercises the guard", len(call.Args))
	}
}

// TestIsExpHistogramValuedShape_ScalarLiteralOnTheLeftIsMulOnly kills the
// CONDITIONALS_NEGATION mutant on
//
//	histogram_native_scalar_binop.go:isExpHistogramValuedShape:`if b.Op == parser.MUL {`
//
// rewritten to `b.Op != parser.MUL` inside [isExpHistogramValuedShape]'s
// binary-operand arm.
//
// A scalar literal on the LEFT composes with a histogram-valued right operand
// for MUL only: multiplication is commutative, so `2 * hist` scales the
// histogram, while `2 / hist` has no histogram-valued reading (a histogram
// can only be a numerator — see [expHistogramFloatVectorScalingBinop]'s own
// DIV comment). Negating the operator check swaps exactly those two
// judgements, and `2 * latency_exp_hist` — the shape the arm exists for —
// stops being histogram-valued.
func TestIsExpHistogramValuedShape_ScalarLiteralOnTheLeftIsMulOnly(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()

	const mul = `2 * latency_exp_hist`
	if !isExpHistogramValuedShape(mustParse(t, mul), s, lowerCtx{}) {
		t.Fatalf("isExpHistogramValuedShape(%q) = false; want true — a scalar literal times a "+
			"histogram-valued operand scales it (mutant `==`->`!=` at "+
			"histogram_native_scalar_binop.go:isExpHistogramValuedShape:`if b.Op == parser.MUL {` rejects MUL and admits DIV instead)", mul)
	}
	const div = `2 / latency_exp_hist`
	if isExpHistogramValuedShape(mustParse(t, div), s, lowerCtx{}) {
		t.Fatalf("isExpHistogramValuedShape(%q) = true; want false — a histogram can only be a "+
			"numerator, so a scalar literal DIVIDED by one has no histogram-valued reading", div)
	}
}

// TestLowerExpHistogramSetOp_OnClauseReachesTheVectorMatch kills the
// CONDITIONALS_NEGATION mutant on
//
//	histogram_native_set_op.go:`if b.VectorMatching != nil {`
//
// rewritten to `b.VectorMatching == nil`.
//
// The guard decides whether the parser's matching clause is copied into the
// emitted [chplan.VectorSetOp]'s Match. Negated, the copy happens only when
// there is nothing to copy, so an explicit `on(...)` is read from the query
// and then silently discarded: the plan joins on the full series identity
// instead of on the named label, which is a different query.
//
// The `on(service)` spelling is what makes the two distinguishable. Written
// without a matching clause the parser still allocates a VectorMatching for a
// set operator — it carries CardManyToMany and no labels — so both the
// original and the mutant produce the same zero Match there and the guard's
// direction cannot be observed.
func TestLowerExpHistogramSetOp_OnClauseReachesTheVectorMatch(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	const q = `latency_exp_hist and on(service) other_exp_hist`
	b, ok := mustParse(t, q).(*parser.BinaryExpr)
	if !ok {
		t.Fatalf("mustParse(%q) = %T, want *parser.BinaryExpr", q, mustParse(t, q))
	}
	if b.VectorMatching == nil || len(b.VectorMatching.MatchingLabels) != 1 {
		t.Fatalf("positive control: %q parsed with VectorMatching %#v; the test needs exactly the "+
			"one `on` label to observe whether it is carried through", q, b.VectorMatching)
	}

	plan, err := lowerExpHistogramSetOp(b, s, lowerCtx{})
	if err != nil {
		t.Fatalf("lowerExpHistogramSetOp(%q) = error %v; want a plan", q, err)
	}
	setOp, ok := plan.(*chplan.VectorSetOp)
	if !ok {
		t.Fatalf("lowerExpHistogramSetOp(%q) = %T; want *chplan.VectorSetOp", q, plan)
	}
	if len(setOp.Match.Labels) != 1 || setOp.Match.Labels[0] != "service" || !setOp.Match.On {
		t.Fatalf("lowerExpHistogramSetOp(%q) produced Match %#v; want On=true with labels "+
			"[service] — the query's own `on(service)` clause (mutant `!=`->`==` at "+
			"histogram_native_set_op.go:`if b.VectorMatching != nil {` copies it only when there is nothing to copy, "+
			"leaving the zero Match)", q, setOp.Match)
	}
}
