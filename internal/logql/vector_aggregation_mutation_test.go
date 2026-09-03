package logql

import (
	"testing"

	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// This file pins the mutation-surviving lines in vector_aggregation.go
// that the `mutation` CI lane (phase4-logql-aggregation) flagged as
// LIVED. Each test below targets one (or more) gremlins mutant(s) and
// is written so the mutated line produces a DIFFERENT lowered plan than
// the original, making the assertion fail under mutation.
//
// NOT KILLABLE — vector_aggregation.go:wrapVectorAggregateForSample:`len(e.Grouping.Groups)*2`
// carries an ARITHMETIC_BASE mutant (`*2` -> `/2` and friends) that no
// test can kill: it is a make() CAPACITY hint. The slice is built with
// append from length 0, so its final contents and ordering are identical
// whatever the pre-sized cap, and a cap is not reachable from the
// returned plan. `len(...)` is never negative and the mutated forms stay
// non-negative, so make() cannot panic either.
//
// The empty-`Grouping.Groups` CONDITIONALS_BOUNDARY mutants on the
// outer-by threading guard were ADJUDICATED EQUIVALENT HERE AND THAT WAS
// WRONG. The old argument was that the only distinguishing input is
// `by ()` and that `withOuterByLabels([]) ≡ withOuterByLabels(nil)`. Both
// halves fail. The parser materialises a non-nil EMPTY Grouping for a
// plainly ungrouped `sum(v)` / `topk(K, v)`, so the distinguishing input
// needs no `by ()` at all; and `withOuterByLabels` is an OVERWRITE, so
// when `lc.OuterByLabels` is already non-empty — which is exactly what an
// enclosing by-grouped aggregation makes it — threading an empty slice
// ERASES the outer labels rather than reproducing them.
// [TestUngroupedInnerAggregationKeepsOuterByLabels] and
// [TestUngroupedInnerTopKKeepsOuterByLabels] below kill both mutants.

// surfacedIdentityHasKey walks the lowered topk/sort plan
// (TopK|OrderBy → Project(sampleShape) → RangeWindow → Project(identity))
// down to the inner range aggregation's identity projection and reports
// whether the synthesised augmented-identity map carries `target` as a
// surfaced key. A top-level outer-by column (e.g. ServiceName) is
// surfaced into that map ONLY when sortableShapedInner threads it down
// via withOuterByLabels — which is exactly the behaviour the outer-by
// threading guard
// vector_aggregation.go:sortableShapedInner:`e.Grouping != nil && !e.Grouping.Without && len(e.Grouping.Groups) > 0`
// controls.
//
// The identity projection shape is
//
//	mapConcat(<base>, mapFilter((k,v)->v!='', map('detected_level', ..., '<col>', toString(<col>))))
//
// so we descend mapConcat.Args[1] → mapFilter.Args[1] → map and scan the
// even (key) positions for `target`.
func surfacedIdentityHasKey(t *testing.T, identityExpr chplan.Expr, target string) bool {
	t.Helper()
	mc, ok := identityExpr.(*chplan.FuncCall)
	if !ok || mc.Fn != chplan.FnMapMerge || len(mc.Args) < 2 {
		t.Fatalf("identity expr = %T (%q), want *chplan.FuncCall(mapConcat) with >=2 args", identityExpr, funcName(identityExpr))
	}
	mf, ok := mc.Args[1].(*chplan.FuncCall)
	if !ok || mf.Fn != chplan.FnMapFilter || len(mf.Args) < 2 {
		t.Fatalf("mapConcat.Args[1] = %T (%q), want *chplan.FuncCall(mapFilter)", mc.Args[1], funcName(mc.Args[1]))
	}
	sm, ok := mf.Args[1].(*chplan.FuncCall)
	if !ok || sm.Fn != chplan.FnMap {
		t.Fatalf("mapFilter.Args[1] = %T (%q), want *chplan.FuncCall(map)", mf.Args[1], funcName(mf.Args[1]))
	}
	for i := 0; i+1 < len(sm.Args); i += 2 {
		if k, ok := sm.Args[i].(*chplan.LitString); ok && k.V == target {
			return true
		}
	}
	return false
}

// lowerTopKIdentityExpr lowers `query` (a topk/bottomk) in instant mode
// and returns the inner range aggregation's identity projection expr as
// lowered — including the [canonicalIdentityExpr] key-order wrap, which
// callers peel with [requireCanonicalIdentity].
func lowerTopKIdentityExpr(t *testing.T, query string, s schema.Logs) chplan.Expr {
	t.Helper()
	expr, err := syntax.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := lower(expr, s, lowerCtx{})
	if err != nil {
		t.Fatalf("lower(%q): %v", query, err)
	}
	topk, ok := plan.(*chplan.TopK)
	if !ok {
		t.Fatalf("lower(%q) -> %T, want *chplan.TopK", query, plan)
	}
	sampleProj, ok := topk.Input.(*chplan.Project)
	if !ok {
		t.Fatalf("TopK.Input = %T, want *chplan.Project (sample-shape wrap)", topk.Input)
	}
	rw, ok := sampleProj.Input.(*chplan.RangeWindow)
	if !ok {
		t.Fatalf("sample Project.Input = %T, want *chplan.RangeWindow", sampleProj.Input)
	}
	idProj, ok := rw.Input.(*chplan.Project)
	if !ok {
		t.Fatalf("RangeWindow.Input = %T, want *chplan.Project (identity wrap)", rw.Input)
	}
	if len(idProj.Projections) == 0 {
		t.Fatalf("identity Project has no projections")
	}
	return idProj.Projections[0].Expr
}

// TestSortableShapedInnerThreadsOuterByColumn kills the mutants on the
// outer-by threading guard
// vector_aggregation.go:sortableShapedInner:`e.Grouping != nil && !e.Grouping.Without && len(e.Grouping.Groups) > 0`,
// which guards `innerLc = lc.withOuterByLabels(e.Grouping.Groups)`,
// in sortableShapedInner (the shared topk/sort front half). For
// `topk(K, rate(...)) by (ServiceName)` all three sub-conditions are
// true, so the outer-by label `ServiceName` (a top-level OTel-CH column)
// is threaded into the inner range aggregation's identity map and
// surfaced as a synthesised `ServiceName` key.
//
// Mutants this pins:
//   - CONDITIONALS_NEGATION on `e.Grouping != nil` (`!= nil` → `== nil`):
//     Grouping is non-nil here, so the mutant's first operand becomes false ⇒
//     guard skipped ⇒ ServiceName NOT surfaced. Assertion (present) fails.
//   - CONDITIONALS_NEGATION on `len(e.Grouping.Groups) > 0` (`> 0` → `<= 0`):
//     len(Groups)==1, so `<= 0` is false ⇒ guard skipped ⇒ ServiceName NOT
//     surfaced. Assertion (present) fails.
//
// (The two INVERT_LOGICAL mutants on the guard's two `&&` operators still
// surface the key for this `by`-input — they're killed by the `without` test
// below.)
func TestSortableShapedInnerThreadsOuterByColumn(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	const query = `topk(2, rate({app="api"}[5m])) by (ServiceName)`

	identity := requireCanonicalIdentity(t, lowerTopKIdentityExpr(t, query, s))
	if !surfacedIdentityHasKey(t, identity, s.ServiceNameColumn) {
		t.Fatalf("topk by (ServiceName): inner identity map is missing the surfaced %q key — outer-by threading guard leaked: a NEGATION mutant skipped withOuterByLabels", s.ServiceNameColumn)
	}
}

// TestSortableShapedInnerSkipsThreadingForWithout kills the
// INVERT_LOGICAL mutants on
// vector_aggregation.go:sortableShapedInner:`e.Grouping != nil && !e.Grouping.Without && len(e.Grouping.Groups) > 0`.
//
// For `topk(K, rate(...)) without (ServiceName)` the `without` clause
// makes !e.Grouping.Without false, so the ORIGINAL guard is false and
// ServiceName is NOT threaded into the inner identity map.
//
// Mutants this pins:
//   - INVERT_LOGICAL on the first `&&` (→ `||`): Grouping != nil is true,
//     so `nil!=… || …` short-circuits true ⇒ withOuterByLabels(Groups)
//     runs ⇒ ServiceName surfaced. Assertion (absent) fails.
//   - INVERT_LOGICAL on the second `&&` (→ `||`): becomes
//     `Grouping!=nil && (!Without || len>0)` = `true && (false || true)`
//     = true ⇒ withOuterByLabels runs ⇒ ServiceName surfaced. Assertion
//     (absent) fails.
func TestSortableShapedInnerSkipsThreadingForWithout(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	const query = `topk(2, rate({app="api"}[5m])) without (ServiceName)`

	identity := requireCanonicalIdentity(t, lowerTopKIdentityExpr(t, query, s))
	if surfacedIdentityHasKey(t, identity, s.ServiceNameColumn) {
		t.Fatalf("topk without (ServiceName): inner identity map unexpectedly surfaced the %q key — the without-clause must NOT thread outer-by labels (an INVERT_LOGICAL mutant flipped && to ||)", s.ServiceNameColumn)
	}
}

// TestTopKPartitionNilForUngrouped kills the INVERT_LOGICAL mutant on the
// early-return guard
// vector_aggregation.go:`g == nil || (!g.Without && len(g.Groups) == 0)`
// in topKPartition. The LogQL parser materialises a non-nil empty
// Grouping for an ungrouped `topk(K, v)` (mustNewVectorAggregationExpr
// defaults gr = &Grouping{}), so for `topk(2, rate(...))`:
//
//	g != nil, g.Without == false, len(g.Groups) == 0
//
// ORIGINAL `||`: `false || (true && true)` = true ⇒ returns nil (one
// global K-window, matching reference Loki's single empty grouping key).
//
// MUTANT `&&`: `false && (…)` = false ⇒ falls through to the by-branch,
// building an empty-but-non-nil partition slice. The assertion that the
// result is nil then fails.
func TestTopKPartitionNilForUngrouped(t *testing.T) {
	t.Parallel()

	const query = `topk(2, rate({app="api"}[5m]))`
	expr, err := syntax.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	vae, ok := expr.(*syntax.VectorAggregationExpr)
	if !ok {
		t.Fatalf("ParseExpr(%q) -> %T, want *syntax.VectorAggregationExpr", query, expr)
	}

	got := topKPartition(vae)
	if got != nil {
		t.Fatalf("topKPartition(ungrouped topk) = %#v (len %d), want nil — the no-meaningful-grouping guard was inverted, building a spurious partition slice", got, len(got))
	}
}

// innermostRangeWindowIdentity descends a lowered plan to the DEEPEST
// [chplan.RangeWindow] and returns its identity projection expr. A nested
// vector aggregation (`sum by (X) (sum(rate(...)))`) lowers to
// Project → Aggregate → …outer shape… → RangeWindow → Project(identity),
// and it is that innermost identity projection that records whether the
// enclosing by-clause labels reached the range aggregation.
func innermostRangeWindowIdentity(t *testing.T, plan chplan.Node) chplan.Expr {
	t.Helper()
	var found *chplan.RangeWindow
	var walk func(chplan.Node)
	walk = func(n chplan.Node) {
		switch v := n.(type) {
		case nil:
			return
		case *chplan.RangeWindow:
			found = v
			walk(v.Input)
		case *chplan.Project:
			walk(v.Input)
		case *chplan.Aggregate:
			walk(v.Input)
		case *chplan.TopK:
			walk(v.Input)
		case *chplan.Filter:
			walk(v.Input)
		}
	}
	walk(plan)
	if found == nil {
		t.Fatalf("lowered plan has no *chplan.RangeWindow: %T", plan)
	}
	idProj, ok := found.Input.(*chplan.Project)
	if !ok {
		t.Fatalf("RangeWindow.Input = %T, want *chplan.Project (identity wrap)", found.Input)
	}
	if len(idProj.Projections) == 0 {
		t.Fatalf("identity Project has no projections")
	}
	return idProj.Projections[0].Expr
}

// lowerNestedIdentity parses and lowers `query` in instant mode and
// returns the innermost range aggregation's identity projection expr,
// with the [canonicalIdentityExpr] key-order wrap already peeled.
func lowerNestedIdentity(t *testing.T, query string, s schema.Logs) chplan.Expr {
	t.Helper()
	expr, err := syntax.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := lower(expr, s, lowerCtx{})
	if err != nil {
		t.Fatalf("lower(%q): %v", query, err)
	}
	return requireCanonicalIdentity(t, innermostRangeWindowIdentity(t, plan))
}

// TestUngroupedInnerAggregationKeepsOuterByLabels kills the
// CONDITIONALS_BOUNDARY mutant on the outer-by threading guard
// vector_aggregation.go:lowerVectorAggregation:`e.Grouping != nil && !e.Grouping.Without && len(e.Grouping.Groups) > 0`
// (`> 0` -> `>= 0`).
//
// The distinguishing input is an UNGROUPED inner vector aggregation
// nested inside a by-grouped outer one. The parser materialises a
// non-nil empty Grouping for `sum(v)` (see
// [TestTopKPartitionNilForUngrouped]), so at the inner call
// `e.Grouping != nil` and `!e.Grouping.Without` both hold and only
// `len(e.Grouping.Groups) > 0` is false. Crucially, `lc` is NOT empty
// there: the outer `sum by (ServiceName)` already threaded
// `[ServiceName]` through this very guard, so the inner call decides
// whether that value SURVIVES.
//
//   - ORIGINAL: guard false => innerLc = lc, the outer `[ServiceName]`
//     is preserved and the innermost range aggregation surfaces
//     `ServiceName` into its identity map.
//   - MUTANT `>= 0`: guard true => innerLc = lc.withOuterByLabels(nil),
//     OVERWRITING the outer labels with the inner aggregation's empty
//     ones. `ServiceName` is no longer surfaced and the assertion fails.
func TestUngroupedInnerAggregationKeepsOuterByLabels(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	const query = `sum by (ServiceName) (sum(rate({app="api"}[5m])))`

	identity := lowerNestedIdentity(t, query, s)
	if !surfacedIdentityHasKey(t, identity, s.ServiceNameColumn) {
		t.Fatalf("%s: innermost identity map is missing the surfaced %q key — the ungrouped INNER aggregation overwrote the outer by-clause labels instead of leaving them alone (the outer-by threading guard's `len(Groups) > 0` was widened to `>= 0`)", query, s.ServiceNameColumn)
	}
}

// TestUngroupedInnerTopKKeepsOuterByLabels kills the
// CONDITIONALS_BOUNDARY mutant on the same guard in the topk/sort front
// half,
// vector_aggregation.go:sortableShapedInner:`e.Grouping != nil && !e.Grouping.Without && len(e.Grouping.Groups) > 0`
// (`> 0` -> `>= 0`).
//
// Same shape as [TestUngroupedInnerAggregationKeepsOuterByLabels], with
// the ungrouped inner aggregation being a `topk` so the inner lowering
// routes through [sortableShapedInner] rather than the plain
// vector-aggregation body. The outer `sum by (ServiceName)` supplies the
// non-empty `lc.OuterByLabels` the mutant erases.
func TestUngroupedInnerTopKKeepsOuterByLabels(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	const query = `sum by (ServiceName) (topk(2, rate({app="api"}[5m])))`

	identity := lowerNestedIdentity(t, query, s)
	if !surfacedIdentityHasKey(t, identity, s.ServiceNameColumn) {
		t.Fatalf("%s: innermost identity map is missing the surfaced %q key — the ungrouped INNER topk overwrote the outer by-clause labels instead of leaving them alone (sortableShapedInner's `len(Groups) > 0` was widened to `>= 0`)", query, s.ServiceNameColumn)
	}
}
