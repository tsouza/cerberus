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
// Equivalence verdicts (no test can kill these — see reasoning in the
// task return notes):
//   - 46:72  CONDITIONALS_BOUNDARY (`> 0` → `>= 0`): the only
//     distinguishing input is `by ()` (empty groups), and
//     withOuterByLabels([]) ≡ withOuterByLabels(nil) because both
//     topLevelColumnsReferencedBy and structuredOuterByKeys collapse a
//     len-0 slice to nil. Byte-identical lowering ⇒ equivalent.
//   - 390:72 CONDITIONALS_BOUNDARY (`> 0` → `>= 0`): same reasoning in
//     sortableShapedInner ⇒ equivalent.
//   - 454:56 ARITHMETIC_BASE (`len(...)*2` → `/2`/`+`/...): a make()
//     capacity hint. The slice is built with append from len 0, so its
//     final contents are identical regardless of the pre-sized cap; the
//     cap is never observable in the returned plan ⇒ equivalent.

// surfacedIdentityHasKey walks the lowered topk/sort plan
// (TopK|OrderBy → Project(sampleShape) → RangeWindow → Project(identity))
// down to the inner range aggregation's identity projection and reports
// whether the synthesised augmented-identity map carries `target` as a
// surfaced key. A top-level outer-by column (e.g. ServiceName) is
// surfaced into that map ONLY when sortableShapedInner threads it down
// via withOuterByLabels — which is exactly the behaviour line 390
// guards.
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
	if !ok || mc.Name != "mapConcat" || len(mc.Args) < 2 {
		t.Fatalf("identity expr = %T (%q), want *chplan.FuncCall(mapConcat) with >=2 args", identityExpr, funcName(identityExpr))
	}
	mf, ok := mc.Args[1].(*chplan.FuncCall)
	if !ok || mf.Name != "mapFilter" || len(mf.Args) < 2 {
		t.Fatalf("mapConcat.Args[1] = %T (%q), want *chplan.FuncCall(mapFilter)", mc.Args[1], funcName(mc.Args[1]))
	}
	sm, ok := mf.Args[1].(*chplan.FuncCall)
	if !ok || sm.Name != "map" {
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
// and returns the inner range aggregation's identity projection expr.
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

// TestSortableShapedInnerThreadsOuterByColumn kills the mutants on
// vector_aggregation.go:390
//
//	if e.Grouping != nil && !e.Grouping.Without && len(e.Grouping.Groups) > 0 {
//		innerLc = lc.withOuterByLabels(e.Grouping.Groups)
//	}
//
// in sortableShapedInner (the shared topk/sort front half). For
// `topk(K, rate(...)) by (ServiceName)` all three sub-conditions are
// true, so the outer-by label `ServiceName` (a top-level OTel-CH column)
// is threaded into the inner range aggregation's identity map and
// surfaced as a synthesised `ServiceName` key.
//
// Mutants this pins:
//   - 390:16 CONDITIONALS_NEGATION (`!= nil` → `== nil`): Grouping is
//     non-nil here, so the mutant's first operand becomes false ⇒ guard
//     skipped ⇒ ServiceName NOT surfaced. Assertion (present) fails.
//   - 390:72 CONDITIONALS_NEGATION (`> 0` → `<= 0`): len(Groups)==1, so
//     `<= 0` is false ⇒ guard skipped ⇒ ServiceName NOT surfaced.
//     Assertion (present) fails.
//
// (The two INVERT_LOGICAL mutants at 390:23 / 390:46 still surface the
// key for this `by`-input — they're killed by the `without` test below.)
func TestSortableShapedInnerThreadsOuterByColumn(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	const query = `topk(2, rate({app="api"}[5m])) by (ServiceName)`

	identity := lowerTopKIdentityExpr(t, query, s)
	if !surfacedIdentityHasKey(t, identity, s.ServiceNameColumn) {
		t.Fatalf("topk by (ServiceName): inner identity map is missing the surfaced %q key — outer-by threading guard (line 390) leaked: a NEGATION mutant skipped withOuterByLabels", s.ServiceNameColumn)
	}
}

// TestSortableShapedInnerSkipsThreadingForWithout kills the
// INVERT_LOGICAL mutants on vector_aggregation.go:390.
//
// For `topk(K, rate(...)) without (ServiceName)` the `without` clause
// makes !e.Grouping.Without false, so the ORIGINAL guard is false and
// ServiceName is NOT threaded into the inner identity map.
//
// Mutants this pins:
//   - 390:23 INVERT_LOGICAL (first `&&` → `||`): Grouping != nil is true,
//     so `nil!=… || …` short-circuits true ⇒ withOuterByLabels(Groups)
//     runs ⇒ ServiceName surfaced. Assertion (absent) fails.
//   - 390:46 INVERT_LOGICAL (second `&&` → `||`): becomes
//     `Grouping!=nil && (!Without || len>0)` = `true && (false || true)`
//     = true ⇒ withOuterByLabels runs ⇒ ServiceName surfaced. Assertion
//     (absent) fails.
func TestSortableShapedInnerSkipsThreadingForWithout(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	const query = `topk(2, rate({app="api"}[5m])) without (ServiceName)`

	identity := lowerTopKIdentityExpr(t, query, s)
	if surfacedIdentityHasKey(t, identity, s.ServiceNameColumn) {
		t.Fatalf("topk without (ServiceName): inner identity map unexpectedly surfaced the %q key — the without-clause must NOT thread outer-by labels (line 390 INVERT_LOGICAL mutant flipped && to ||)", s.ServiceNameColumn)
	}
}

// TestTopKPartitionNilForUngrouped kills the INVERT_LOGICAL mutant on
// vector_aggregation.go:337
//
//	if g == nil || (!g.Without && len(g.Groups) == 0) {
//		return nil
//	}
//
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
		t.Fatalf("topKPartition(ungrouped topk) = %#v (len %d), want nil — the no-meaningful-grouping guard (line 337) was inverted, building a spurious partition slice", got, len(got))
	}
}
