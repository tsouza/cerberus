// Tests in this file pin behaviour that the gremlins mutation suite had
// reported as LIVED on internal/promql/subquery.go — each one constructs
// an input that observably differentiates the original code from the
// mutated branch, so the test fails when the mutant is applied and the
// mutant is reported KILLED. See gremlins_kill_test.go's own header for
// the mutant-citation convention.
//
// Two CONDITIONALS_BOUNDARY mutants reported for this file are NOT
// addressed here (subquery.go:`if step < 0` and subquery.go:`if ns%stepNS != 0 && ns < 0`): both are
// mathematically equivalent mutants, not coverage gaps. At
// subquery.go:`if step < 0`, `step` is reassigned to the positive
// `defaultSubqueryStep` constant whenever it was zero (the
// `if step == 0` arm just above it), so
// `step` can never be exactly 0 by the time the boundary check runs —
// `<` and `<=` decide identically over every reachable value. At
// subquery.go:`if ns%stepNS != 0 && ns < 0`, the `<`/`<=`
// boundary sits at ns==0, but ns==0 forces `ns%stepNS == 0` for any
// nonzero stepNS, which makes the left `&&` operand false and
// short-circuits the whole expression regardless of the right operand's
// boundary — so no input can ever reach a state where the two operators
// disagree. No test can kill either mutant without the underlying code
// changing shape.
package promql

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLowerOuterRangeFnOverSubquery_PinnedRangeModeWidensByOffsetPlusRange
// kills the ARITHMETIC_BASE + INVERT_NEGATIVES pair on the `-rw.Offset`
// term of the `@`-pinned + range-mode arm's
// `widenSubquerySpine(inner, anchor.End.Add(-rw.Offset-sub.Range), anchor.End)`
// call, subquery.go:`case rangeMode && pinned:`. Both mutants flip the Offset term's sign (one by swapping the
// arithmetic operator, one by inverting the unary negation — the
// observable effect is identical: the inner spine gets widened starting
// `+rw.Offset` early instead of `-rw.Offset` late).
//
// TestLowerOuterRangeFnOverSubquery_WidensInnerByOffsetPlusRange
// (subquery_time_offset_test.go) already pins the neighbouring
// "range mode, not pinned" arm; this test is its `@`-pinned sibling,
// the one branch that actually reaches that call.
func TestLowerOuterRangeFnOverSubquery_PinnedRangeModeWidensByOffsetPlusRange(t *testing.T) {
	t.Parallel()

	const query = `max_over_time(rate(demo_cpu[5m])[1h:5m] offset 10m @ 1700010000)`
	offset := 10 * time.Minute
	subRange := time.Hour
	pinnedTS := time.Unix(1700010000, 0).UTC()
	s := schema.DefaultOTelMetrics()

	expr, err := parser.NewParser(parser.Options{}).ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}

	start := time.Unix(1_700_000_000, 0).UTC()
	end := start.Add(2 * time.Hour)
	plan, err := LowerAtRange(context.Background(), expr, s, start, end, time.Minute)
	if err != nil {
		t.Fatalf("LowerAtRange(%q): %v", query, err)
	}
	outerProj, ok := plan.(*chplan.Project)
	if !ok {
		t.Fatalf("plan = %T, want *chplan.Project (wrapRangeWindowAtBroadcast)", plan)
	}
	joined, ok := outerProj.Input.(*chplan.CrossJoin)
	if !ok {
		t.Fatalf("Project.Input = %T, want *chplan.CrossJoin", outerProj.Input)
	}
	rw, ok := joined.Right.(*chplan.RangeWindow)
	if !ok {
		t.Fatalf("CrossJoin.Right = %T, want *chplan.RangeWindow", joined.Right)
	}
	inner := firstRangeWindow(rw.Input)
	if inner == nil {
		t.Fatal("no inner RangeWindow found under the outer reducer's Input")
	}
	wantStart := pinnedTS.Add(-offset).Add(-subRange)
	if !inner.Start.Equal(wantStart) {
		t.Errorf("inner.Start = %s, want %s (pinnedTS - offset - subRange) — "+
			"mutants on subquery.go:`case rangeMode && pinned:`'s widenSubquerySpine call "+
			"would yield %s (pinnedTS + offset - subRange)",
			inner.Start, wantStart, pinnedTS.Add(offset).Add(-subRange))
	}
}

// TestLowerAbsentOverTimeOverSubquery_RangeModeWidensByOffsetPlusRange
// kills the ARITHMETIC_BASE + INVERT_NEGATIVES pair on the `-a.Offset`
// term of lowerAbsentOverTimeOverSubquery's range-mode (not pinned) arm,
// subquery.go:`widenSubquerySpine(a.Input, ctx.start.Add(-a.Offset-sub.Range), ctx.end)`.
func TestLowerAbsentOverTimeOverSubquery_RangeModeWidensByOffsetPlusRange(t *testing.T) {
	t.Parallel()

	const query = `absent_over_time(demo_cpu[5m:1m] offset 10m)`
	offset := 10 * time.Minute
	subRange := 5 * time.Minute
	s := schema.DefaultOTelMetrics()

	expr, err := parser.NewParser(parser.Options{}).ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}

	start := time.Unix(1_700_000_000, 0).UTC()
	end := start.Add(2 * time.Hour)
	plan, err := LowerAtRange(context.Background(), expr, s, start, end, time.Minute)
	if err != nil {
		t.Fatalf("LowerAtRange(%q): %v", query, err)
	}
	a := firstAbsentOverTime(plan)
	if a == nil {
		t.Fatalf("no chplan.AbsentOverTime under the lowered %q", query)
	}
	inner := firstRangeWindow(a.Input)
	if inner == nil {
		t.Fatal("no RangeWindow found under AbsentOverTime.Input")
	}
	wantStart := start.Add(-offset).Add(-subRange)
	if !inner.Start.Equal(wantStart) {
		t.Errorf("inner.Start = %s, want %s (ctx.start - offset - subRange) — "+
			"mutants on subquery.go:`widenSubquerySpine(a.Input, ctx.start.Add(-a.Offset-sub.Range), ctx.end)` would yield %s (ctx.start + offset - subRange)",
			inner.Start, wantStart, start.Add(offset).Add(-subRange))
	}
}

// TestLowerAbsentOverTimeOverSubquery_InstantModeWidensByOffsetPlusRange
// kills the ARITHMETIC_BASE + INVERT_NEGATIVES pair on the `-a.Offset`
// term of the default (instant-mode) arm's widenSubquerySpine call,
// subquery.go:`widenSubquerySpine(a.Input, a.End.Add(-a.Offset-sub.Range), a.End)`.
func TestLowerAbsentOverTimeOverSubquery_InstantModeWidensByOffsetPlusRange(t *testing.T) {
	t.Parallel()

	const query = `absent_over_time(demo_cpu[5m:1m] offset 7m)`
	offset := 7 * time.Minute
	subRange := 5 * time.Minute
	evalTS := time.Unix(1_700_010_000, 0).UTC()
	s := schema.DefaultOTelMetrics()

	expr, err := parser.NewParser(parser.Options{}).ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}

	plan, err := LowerAt(context.Background(), expr, s, evalTS, evalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	a := firstAbsentOverTime(plan)
	if a == nil {
		t.Fatalf("no chplan.AbsentOverTime under the lowered %q", query)
	}
	inner := firstRangeWindow(a.Input)
	if inner == nil {
		t.Fatal("no RangeWindow found under AbsentOverTime.Input")
	}
	wantStart := evalTS.Add(-offset).Add(-subRange)
	if !inner.Start.Equal(wantStart) {
		t.Errorf("inner.Start = %s, want %s (a.End - offset - subRange) — "+
			"mutants on subquery.go:`widenSubquerySpine(a.Input, a.End.Add(-a.Offset-sub.Range), a.End)` would yield %s (a.End + offset - subRange)",
			inner.Start, wantStart, evalTS.Add(offset).Add(-subRange))
	}
}

// TestLowerAbsentOverTimeOverSubquery_InstantModeKeepsExistingPin kills the
// INVERT_LOGICAL mutant on the `&&` of
// subquery.go:lowerAbsentOverTimeOverSubquery:`if a.End.IsZero() && !ctx.end.IsZero()`.
// That guard only fills a.End
// from ctx.end when a.End was NOT already set (i.e. the subquery has no
// `@` pin of its own). Flipping `&&` to `||` makes the guard fire whenever
// ctx.end is merely non-zero, unconditionally overwriting an existing `@`
// pin with the query's own eval timestamp.
//
// The query pins the subquery at a timestamp deliberately different from
// the instant query's own eval time, so an overwrite is observable.
func TestLowerAbsentOverTimeOverSubquery_InstantModeKeepsExistingPin(t *testing.T) {
	t.Parallel()

	const query = `absent_over_time(demo_cpu[5m:1m] @ 1700000000)`
	pinnedTS := time.Unix(1_700_000_000, 0).UTC()
	evalTS := time.Unix(1_700_010_000, 0).UTC() // deliberately different from the pin
	s := schema.DefaultOTelMetrics()

	expr, err := parser.NewParser(parser.Options{}).ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}

	plan, err := LowerAt(context.Background(), expr, s, evalTS, evalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	a := firstAbsentOverTime(plan)
	if a == nil {
		t.Fatalf("no chplan.AbsentOverTime under the lowered %q", query)
	}
	if !a.End.Equal(pinnedTS) {
		t.Errorf("AbsentOverTime.End = %s, want %s (the `@` pin) — mutant `&&`->`||` at "+
			"subquery.go:lowerAbsentOverTimeOverSubquery:`if a.End.IsZero() && !ctx.end.IsZero()` "+
			"would overwrite the pin with the eval timestamp %s", a.End, pinnedTS, evalTS)
	}
}

// TestLowerSubqueryOverCall_UnsafeInstantTransformSkipsIdentityWrap kills
// the INVERT_LOGICAL mutant at subquery.go:`if isInstantTransformCall(call) && subqueryInstantSafe(call)` — the `&&` in
// `if isInstantTransformCall(call) && subqueryInstantSafe(call)`. abs() is
// an instant-transform call, but its argument here is rate(...), which is
// NOT sample-preserving, so subqueryInstantSafe must be false and the
// conjunction must skip the Identity-wrap branch. Flipping `&&` to `||`
// would take the Identity-wrap branch anyway (since isInstantTransformCall
// alone is true), returning a *chplan.RangeWindow instead of the
// *chplan.Project the per-anchor fallback (subqueryAnchorShape) builds.
func TestLowerSubqueryOverCall_UnsafeInstantTransformSkipsIdentityWrap(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	expr := mustParse(t, `abs(rate(demo_cpu[1m]))[5m:1m]`)
	sub, ok := expr.(*parser.SubqueryExpr)
	if !ok {
		t.Fatalf("parsed %T, want *parser.SubqueryExpr", expr)
	}
	call, ok := sub.Expr.(*parser.Call)
	if !ok {
		t.Fatalf("sub.Expr = %T, want *parser.Call", sub.Expr)
	}

	evalTS := time.Date(2026, time.January, 1, 0, 10, 0, 0, time.UTC)
	ctx := lowerCtx{end: evalTS, lowerers: RangeLowerers{}.withDefaults()}
	plan, err := lowerSubqueryOverCall(sub, call, sub.Step, s, ctx)
	if err != nil {
		t.Fatalf("lowerSubqueryOverCall(%q): %v", `abs(rate(demo_cpu[1m]))[5m:1m]`, err)
	}
	if _, isRW := plan.(*chplan.RangeWindow); isRW {
		t.Fatalf("plan = *chplan.RangeWindow (the Identity wrap); want the per-anchor *chplan.Project " +
			"fallback (subqueryInstantSafe must be false for abs(rate(...)), so the && must skip the " +
			"Identity branch — mutant `&&`->`||` at " +
			"subquery.go:`if isInstantTransformCall(call) && subqueryInstantSafe(call)` would take it anyway)")
	}
	if _, isProj := plan.(*chplan.Project); !isProj {
		t.Fatalf("plan = %T, want *chplan.Project (subqueryAnchorShape's per-anchor fallback)", plan)
	}
}

// TestNodeCarriesMetricName_VectorSetOp_OrChecksBothSidesAndUnlessDoesNot
// kills the CONDITIONALS_NEGATION mutant on the `==` of
// subquery.go:`if v.Op == chplan.VectorSetOr && !nodeCarriesMetricName(v.Right, s)`.
// `or` must check BOTH arms (it unions rows from both); `and`/`unless`
// must check ONLY the left arm (they only ever emit LHS rows). Flipping
// `==` to `!=` swaps which operator gets the Right-arm check.
func TestNodeCarriesMetricName_VectorSetOp_OrChecksBothSidesAndUnlessDoesNot(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	named := &chplan.Scan{}
	unnamed := &chplan.StepGrid{}

	orOp := &chplan.VectorSetOp{Op: chplan.VectorSetOr, Left: named, Right: unnamed}
	if nodeCarriesMetricName(orOp, s) {
		t.Fatalf("or: Left carries a name but Right does not; `or` must check BOTH sides and answer " +
			"false (mutant `==`->`!=` at " +
			"subquery.go:`if v.Op == chplan.VectorSetOr && !nodeCarriesMetricName(v.Right, s)` " +
			"would only check Left here)")
	}

	andOp := &chplan.VectorSetOp{Op: chplan.VectorSetAnd, Left: named, Right: unnamed}
	if !nodeCarriesMetricName(andOp, s) {
		t.Fatalf("and: Left carries a name; `and`/`unless` must ignore Right and answer true " +
			"(mutant `==`->`!=` at " +
			"subquery.go:`if v.Op == chplan.VectorSetOr && !nodeCarriesMetricName(v.Right, s)` " +
			"would incorrectly also check Right here)")
	}
}

// TestSubqueryInstantSafe_PiExceptionNotRejected kills the
// CONDITIONALS_NEGATION mutant on the `!=` of
// subquery.go:`if !isInstantTransformCall(v) && v.Func.Name != "pi"`. pi() is the
// documented parse-time-constant exception: a zero-arg call that is not
// itself an instant-transform call, but must still be treated as safe.
// Flipping `!=` to `==` rejects any call subtree containing pi().
func TestSubqueryInstantSafe_PiExceptionNotRejected(t *testing.T) {
	t.Parallel()

	call, ok := mustParse(t, `clamp_min(demo_cpu, pi())`).(*parser.Call)
	if !ok {
		t.Fatalf("expected *parser.Call")
	}
	if !subqueryInstantSafe(call) {
		t.Fatalf("subqueryInstantSafe(clamp_min(demo_cpu, pi())) = false, want true — pi() is the " +
			"documented exception (mutant `!=`->`==` at " +
			"subquery.go:`if !isInstantTransformCall(v) && v.Func.Name != \"pi\"` would reject any call " +
			"tree containing pi())")
	}
}

// TestLowerSubqueryOverBinary_PlainShapeSucceedsWithNoEvalAnchor kills the
// CONDITIONALS_NEGATION mutant at subquery.go:lowerSubqueryOverBinary:`if shape := chplan.RowShapeOf(inner); shape == chplan.HistogramRowShape || shape == chplan.MixedRowShape` — the second `==` in
// `if shape := chplan.RowShapeOf(inner); shape == chplan.HistogramRowShape || shape == chplan.MixedRowShape`.
// A plain (non-histogram, non-mixed) `or` composition must lower
// successfully even with no query eval-time context threaded through
// (the same !ok fallback TestSubqueryOverBinary_HistogramSetOp_NoEvalAnchorRejects,
// subquery_and_unless_mixed_histogram_outer_test.go, pins the REJECT half
// of). Flipping `==` to `!=` on the Mixed comparison turns the guard into
// `shape == Histogram || shape != Mixed`, which is true for every ordinary
// shape and wrongly rejects this query.
func TestLowerSubqueryOverBinary_PlainShapeSucceedsWithNoEvalAnchor(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	expr := mustParse(t, `(demo_cpu or demo_mem)[5m:1m]`)
	sub, ok := expr.(*parser.SubqueryExpr)
	if !ok {
		t.Fatalf("parsed %T, want *parser.SubqueryExpr", expr)
	}
	plan, err := lowerSubquery(sub, s, lowerCtx{lowerers: RangeLowerers{}.withDefaults()})
	if err != nil {
		t.Fatalf("bare lowerSubquery(%q) with no eval anchor: %v (want success — the shape is plain, "+
			"not histogram/mixed)", `(demo_cpu or demo_mem)[5m:1m]`, err)
	}
	if plan == nil {
		t.Fatal("lowerSubquery returned nil plan with nil error")
	}
}

// TestSubqueryOffsetCtx_NonZeroOffsetShiftsBothBounds kills the
// CONDITIONALS_NEGATION mutant on the `==` of
// subquery.go:`if sub.OriginalOffset == 0`. With a non-zero offset and
// non-zero start/end bounds, the function must shift both bounds by
// -offset. Flipping `==` to `!=` returns ctx UNSHIFTED whenever the
// offset is non-zero — the one case where a shift is actually required.
func TestSubqueryOffsetCtx_NonZeroOffsetShiftsBothBounds(t *testing.T) {
	t.Parallel()

	sub := &parser.SubqueryExpr{OriginalOffset: 10 * time.Minute}
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	got := subqueryOffsetCtx(sub, lowerCtx{start: start, end: end})

	wantStart := start.Add(-10 * time.Minute)
	wantEnd := end.Add(-10 * time.Minute)
	if !got.start.Equal(wantStart) {
		t.Errorf("start = %s, want %s (mutant `==`->`!=` at subquery.go:`if sub.OriginalOffset == 0` would leave it at %s)",
			got.start, wantStart, start)
	}
	if !got.end.Equal(wantEnd) {
		t.Errorf("end = %s, want %s (mutant `==`->`!=` at subquery.go:`if sub.OriginalOffset == 0` would leave it at %s)",
			got.end, wantEnd, end)
	}
}

// TestProjectCarriesMetricName_ContinuesPastNonMatchingProjections kills
// the INVERT_LOOPCTRL mutant at subquery.go:projectCarriesMetricName:`continue`
// — the `continue` inside its loop over p.Projections. The MetricName
// projection is deliberately placed SECOND so the loop must skip past a
// non-matching first entry to find it. Flipping `continue` to `break`
// stops at the first non-matching projection and falls through to
// `return false`, regardless of what a later projection carries.
func TestProjectCarriesMetricName_ContinuesPastNonMatchingProjections(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := &chplan.Project{
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: &chplan.LitString{V: "http_requests_total"}, Alias: s.MetricNameColumn},
		},
	}
	if !projectCarriesMetricName(p, s) {
		t.Fatalf("projectCarriesMetricName = false, want true — the MetricName projection is the " +
			"SECOND entry and carries a real name; mutant `continue`->`break` at " +
			"subquery.go:projectCarriesMetricName:`continue` " +
			"would stop at the first (non-matching) entry and answer false")
	}
}

// TestLowerSubqueryOverCountValues_MapArgsCapacityIsTight kills the
// ARITHMETIC_BASE mutant on lowerSubqueryOverCountValues's `by(...)`
// capacity hint,
// subquery.go:`mapArgs := make([]chplan.Expr, 0, (len(agg.Grouping)+1)*2)`.
//
// With 2 grouping labels, the original pre-allocates exactly enough room
// for all 6 appends (2 labels * 2 + the synthetic value-as-label pair),
// so no reallocation ever happens and cap stays at 6. The `*` -> `/`
// mutant starts at cap (2+1)/2 = 1, forcing append's growth path to
// reallocate — empirically verified (via a standalone simulation) to
// settle at cap 8, not 6, for this specific grouping count.
func TestLowerSubqueryOverCountValues_MapArgsCapacityIsTight(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	expr := mustParse(t, `count_values("val", demo_cpu) by (job, instance)[5m:1m]`)
	sub, ok := expr.(*parser.SubqueryExpr)
	if !ok {
		t.Fatalf("parsed %T, want *parser.SubqueryExpr", expr)
	}
	plan, err := lowerSubquery(sub, s, lowerCtx{lowerers: RangeLowerers{}.withDefaults()})
	if err != nil {
		t.Fatalf("lowerSubquery: %v", err)
	}
	proj, ok := plan.(*chplan.Project)
	if !ok {
		t.Fatalf("plan = %T, want *chplan.Project", plan)
	}
	if len(proj.Projections) == 0 {
		t.Fatal("no projections")
	}
	mw, ok := proj.Projections[0].Expr.(*chplan.MapWithoutEmptyValues)
	if !ok {
		t.Fatalf("Projections[0].Expr = %T, want *chplan.MapWithoutEmptyValues", proj.Projections[0].Expr)
	}
	fc, ok := mw.Map.(*chplan.FuncCall)
	if !ok || fc.Fn != chplan.FnMap {
		t.Fatalf("MapWithoutEmptyValues.Map = %#v, want a Map FuncCall", mw.Map)
	}
	const wantArgs = 6 // 2 labels * 2 + (value literal, cv_val column ref)
	if got := len(fc.Args); got != wantArgs {
		t.Fatalf("len(Args) = %d, want %d", got, wantArgs)
	}
	if got := cap(fc.Args); got != wantArgs {
		t.Fatalf("cap(Args) = %d, want %d (mutant `*`->`/` at "+
			"subquery.go:`mapArgs := make([]chplan.Expr, 0, (len(agg.Grouping)+1)*2)` would yield a "+
			"different cap via append's reallocation growth path)", got, wantArgs)
	}
}

// TestBuildAttributesFromAggregate_ArgsCapacityIsTight kills the
// ARITHMETIC_BASE mutant on buildAttributesFromAggregate's `by(...)`
// capacity hint,
// subquery.go:`args := make([]chplan.Expr, 0, len(agg.Grouping)*2)`.
//
// With 3 grouping labels, the original pre-allocates exactly enough room
// for all 6 appends, so cap stays at 6. The `*` -> `/` mutant starts at
// cap 3/2 = 1, forcing append's growth path to reallocate — empirically
// verified to settle at cap 8, not 6, for this specific grouping count.
func TestBuildAttributesFromAggregate_ArgsCapacityIsTight(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	expr := mustParse(t, `(sum by (job, instance, region) (demo_cpu))[5m:1m]`)
	sub, ok := expr.(*parser.SubqueryExpr)
	if !ok {
		t.Fatalf("parsed %T, want *parser.SubqueryExpr", expr)
	}
	plan, err := lowerSubquery(sub, s, lowerCtx{lowerers: RangeLowerers{}.withDefaults()})
	if err != nil {
		t.Fatalf("lowerSubquery: %v", err)
	}
	proj, ok := plan.(*chplan.Project)
	if !ok {
		t.Fatalf("plan = %T, want *chplan.Project", plan)
	}
	if len(proj.Projections) == 0 {
		t.Fatal("no projections")
	}
	fc, ok := proj.Projections[0].Expr.(*chplan.FuncCall)
	if !ok || fc.Fn != chplan.FnMap {
		t.Fatalf("Projections[0].Expr = %#v, want a Map FuncCall (buildAttributesFromAggregate's "+
			"by-clause branch)", proj.Projections[0].Expr)
	}
	const wantArgs = 6 // 3 labels * 2
	if got := len(fc.Args); got != wantArgs {
		t.Fatalf("len(Args) = %d, want %d", got, wantArgs)
	}
	if got := cap(fc.Args); got != wantArgs {
		t.Fatalf("cap(Args) = %d, want %d (mutant `*`->`/` at "+
			"subquery.go:`args := make([]chplan.Expr, 0, len(agg.Grouping)*2)` would yield a "+
			"different cap via append's reallocation growth path)", got, wantArgs)
	}
}

// TestLowerSubqueryOverCallSubquery_WidensToSumOfBothRanges kills the
// ARITHMETIC_BASE mutant on the `+` of
// subquery.go:lowerSubqueryOverCallSubquery:`widened.Range = sub.Range + innerSub.Range`.
// The nested-subquery shape
// `max_over_time(rate(m[1m])[5m:30s])[1h:5m]` must widen the inner
// subquery's own matrix to cover the OUTER range PLUS the inner range,
// which surfaces as the widened RangeWindow's OuterRange field. Flipping
// `+` to `-` would shrink it instead (1h - 5m = 55m rather than 65m).
func TestLowerSubqueryOverCallSubquery_WidensToSumOfBothRanges(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	expr := mustParse(t, `max_over_time(rate(demo_cpu[1m])[5m:30s])[1h:5m]`)
	topSub, ok := expr.(*parser.SubqueryExpr)
	if !ok {
		t.Fatalf("parsed %T, want *parser.SubqueryExpr", expr)
	}

	plan, err := lowerSubquery(topSub, s, lowerCtx{lowerers: RangeLowerers{}.withDefaults()})
	if err != nil {
		t.Fatalf("lowerSubquery: %v", err)
	}
	rw, ok := plan.(*chplan.RangeWindow)
	if !ok {
		t.Fatalf("plan = %T, want *chplan.RangeWindow", plan)
	}
	wideInner, ok := rw.Input.(*chplan.RangeWindow)
	if !ok {
		t.Fatalf("rw.Input = %T, want *chplan.RangeWindow (the widened nested subquery)", rw.Input)
	}
	const want = time.Hour + 5*time.Minute
	if wideInner.OuterRange != want {
		t.Fatalf("wideInner.OuterRange = %s, want %s (sub.Range + innerSub.Range) — mutant `+`->`-` "+
			"at subquery.go:lowerSubqueryOverCallSubquery:`widened.Range = sub.Range + innerSub.Range` "+
			"would yield %s", wideInner.OuterRange, want, time.Hour-5*time.Minute)
	}
}
