package traceql

import (
	"testing"

	tempoql "github.com/tsouza/cerberus/internal/traceql/ast"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// Mutation-coverage tests for the five `CONDITIONALS_NEGATION` survivors
// tracked by #1524: gremlins-phase4-traceql-lower run 30695961440 recorded
// nine LIVED mutants on lower.go, two of which (lower.go:651 / :655, inside
// the now-deleted spansetArmExposesSpanID) no longer exist — #1413 removed
// that function when it folded span-identity tracking into mapSetOp's own
// alignUnionArms call. The remaining five all still exist, unchanged in
// substance, just renumbered by intervening edits:
//
//	lower.go:613  isNarrowSpanProjection  Replacements guard
//	lower.go:620  isNarrowSpanProjection  Projections-length guard
//	lower.go:1323 lowerInOperation        absentAttributePredicate guard
//	lower.go:1394 attributeHasNoBacking   instrumentation-scope guard
//	lower.go:1469 coerceBoolFieldAccess   op guard
//
// Each test below calls the mutated function/call-site directly and asserts
// the CONCRETE result on both sides of the guard, so a CONDITIONALS_NEGATION
// mutant (which wraps the whole guard condition in `!(...)`) flips at least
// one assertion's expected outcome.

// TestIsNarrowSpanProjection_ReplacementsGuard pins the `len(p.Replacements)
// != 0` guard (lower.go:613). The mutant flips it to `== 0`, which:
//   - on the canonical envelope (Replacements empty) short-circuits false
//     instead of falling through to the (matching) shape check;
//   - on a Project WITH Replacements set skips the short-circuit and falls
//     through to the shape check, wrongly accepting it.
func TestIsNarrowSpanProjection_ReplacementsGuard(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()

	canonical := narrowSpanProjection(nil, s).(*chplan.Project)
	if !isNarrowSpanProjection(canonical, s) {
		t.Fatalf("isNarrowSpanProjection rejected the canonical narrow envelope (empty Replacements)")
	}

	withReplacements := narrowSpanProjection(nil, s).(*chplan.Project)
	withReplacements.Replacements = []chplan.Projection{{Expr: &chplan.ColumnRef{Name: "SpanName"}}}
	if isNarrowSpanProjection(withReplacements, s) {
		t.Errorf("isNarrowSpanProjection accepted a Project with non-empty Replacements; want rejected")
	}
}

// TestIsNarrowSpanProjection_ProjectionCountGuard pins the
// `len(p.Projections) != len(want)` guard (lower.go:620). The mutant flips it
// to `==`, which:
//   - on the exact-length canonical envelope short-circuits false instead of
//     running the per-column check that confirms it;
//   - on a Project with an EXTRA projection skips the short-circuit and lets
//     the per-column loop index past `want`'s bounds.
func TestIsNarrowSpanProjection_ProjectionCountGuard(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()

	canonical := narrowSpanProjection(nil, s).(*chplan.Project)
	if !isNarrowSpanProjection(canonical, s) {
		t.Fatalf("isNarrowSpanProjection rejected an exact-length narrow envelope")
	}

	extra := narrowSpanProjection(nil, s).(*chplan.Project)
	extra.Projections = append(extra.Projections, chplan.Projection{Expr: &chplan.ColumnRef{Name: "Extra"}})
	if isNarrowSpanProjection(extra, s) {
		t.Errorf("isNarrowSpanProjection accepted a Project with an extra projection column; want rejected (length mismatch)")
	}
}

// TestIsNarrowSpanProjection_PerColumnGuard pins the per-column rejection
// guard `!ok || projection.Alias != "" || column.Name != want[i]`
// (lower.go:625). This guard was previously NOT_COVERED — nothing exercised
// isNarrowSpanProjection's loop body at all — so the two length guards above
// were the only ones gremlins could even attempt to kill. Reaching the loop
// (as the two tests above now do) makes this three-way OR live too; each arm
// gets its own discriminating negative case here so none of them regress to
// a fresh LIVED survivor.
func TestIsNarrowSpanProjection_PerColumnGuard(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()

	// Not a *chplan.ColumnRef: `!ok` must reject.
	notColumnRef := narrowSpanProjection(nil, s).(*chplan.Project)
	notColumnRef.Projections[0].Expr = &chplan.LitString{V: "not-a-column"}
	if isNarrowSpanProjection(notColumnRef, s) {
		t.Errorf("isNarrowSpanProjection accepted a non-ColumnRef projection; want rejected")
	}

	// A non-empty Alias: `projection.Alias != \"\"` must reject.
	aliased := narrowSpanProjection(nil, s).(*chplan.Project)
	aliased.Projections[0].Alias = "aliased"
	if isNarrowSpanProjection(aliased, s) {
		t.Errorf("isNarrowSpanProjection accepted an aliased projection; want rejected")
	}

	// Right position, wrong column name: `column.Name != want[i]` must reject.
	wrongName := narrowSpanProjection(nil, s).(*chplan.Project)
	wrongName.Projections[0].Expr = &chplan.ColumnRef{Name: "NotTheKeyColumn"}
	if isNarrowSpanProjection(wrongName, s) {
		t.Errorf("isNarrowSpanProjection accepted a projection with the wrong column name; want rejected")
	}
}

// TestLowerInOperation_AbsentAttributeGuard pins the `; absent {` guard at
// lower.go:1323 (`if pred, absent := absentAttributePredicate(...); absent`).
// The mutant flips it to `; !absent {`, which:
//   - on a backed attribute (absent == false) wrongly fires the early
//     return, yielding the (nil, nil) zero value instead of an *InList;
//   - on an unbacked attribute (absent == true) wrongly SKIPS the early
//     return and falls through to generic lowering instead of the constant
//     predicate.
func TestLowerInOperation_AbsentAttributeGuard(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()

	backed := &tempoql.BinaryOperation{
		Op:  tempoql.OpIn,
		LHS: tempoql.NewScopedAttribute(tempoql.AttributeScopeSpan, false, "http.status_code"),
		RHS: tempoql.NewStaticStringArray([]string{"200", "201"}),
	}
	got, err := lowerInOperation(backed, s)
	if err != nil {
		t.Fatalf("lowerInOperation(backed attribute IN [...]): unexpected error: %v", err)
	}
	if _, ok := got.(*chplan.InList); !ok {
		t.Fatalf("lowerInOperation(backed attribute IN [...]) = %#v; want *chplan.InList", got)
	}

	unbacked := &tempoql.BinaryOperation{
		// IntrinsicChildCount has no per-span OTel-CH column (see
		// attributeHasNoBacking); reference Tempo resolves it to StaticNil, so
		// `childCount IN [...]` is constant-false.
		Op:  tempoql.OpIn,
		LHS: tempoql.NewIntrinsic(tempoql.IntrinsicChildCount),
		RHS: tempoql.NewStaticIntArray([]int{1, 2}),
	}
	got, err = lowerInOperation(unbacked, s)
	if err != nil {
		t.Fatalf("lowerInOperation(unbacked attribute IN [...]): unexpected error: %v", err)
	}
	lit, ok := got.(*chplan.LitBool)
	if !ok || lit.V {
		t.Errorf("lowerInOperation(unbacked attribute IN [...]) = %#v; want LitBool{V:false}", got)
	}
}

// TestAttributeHasNoBacking_InstrumentationScopeGuard pins the
// `attr.Scope == AttributeScopeInstrumentation && s.ScopeAttributesColumn ==
// ""` predicate (lower.go:1394). The mutant negates the whole expression,
// which flips every one of the three cases below.
func TestAttributeHasNoBacking_InstrumentationScopeGuard(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces() // ScopeAttributesColumn == "" by default

	instrAttr := tempoql.NewScopedAttribute(tempoql.AttributeScopeInstrumentation, false, "some.attr")
	if !attributeHasNoBacking(instrAttr, s) {
		t.Errorf("attributeHasNoBacking(instrumentation-scoped, no ScopeAttributesColumn) = false; want true")
	}

	spanAttr := tempoql.NewScopedAttribute(tempoql.AttributeScopeSpan, false, "http.status_code")
	if attributeHasNoBacking(spanAttr, s) {
		t.Errorf("attributeHasNoBacking(span-scoped) = true; want false (span attributes have real backing)")
	}

	// Same instrumentation scope, but the schema now configures a real
	// ScopeAttributesColumn — the guard must read the schema, not just the
	// scope, so this now HAS backing.
	withScopeCol := s
	withScopeCol.ScopeAttributesColumn = "ScopeAttributes"
	if attributeHasNoBacking(instrAttr, withScopeCol) {
		t.Errorf("attributeHasNoBacking(instrumentation-scoped, WITH ScopeAttributesColumn) = true; want false")
	}
}

// TestCoerceBoolFieldAccess_OpGuard pins the `op != chplan.OpEq && op !=
// chplan.OpNe` guard (lower.go:1469). The mutant flips it to `op == OpEq ||
// op == OpNe`, which:
//   - on OpEq wrongly fires the early return, leaving the LitBool operand
//     un-coerced instead of rewritten to its OTel-CH string encoding;
//   - on a non-equality op (OpAnd) wrongly SKIPS the early return and
//     coerces an operand that must be left alone.
func TestCoerceBoolFieldAccess_OpGuard(t *testing.T) {
	t.Parallel()

	fa := &chplan.FieldAccess{Source: &chplan.ColumnRef{Name: "SpanAttributes"}, Path: "cache.hit"}
	tru := &chplan.LitBool{V: true}

	lhs, rhs := coerceBoolFieldAccess(chplan.OpEq, fa, tru)
	if lhs != chplan.Expr(fa) {
		t.Errorf("coerceBoolFieldAccess(OpEq) lhs = %#v; want the FieldAccess unchanged", lhs)
	}
	rstr, ok := rhs.(*chplan.LitString)
	if !ok || rstr.V != "true" {
		t.Errorf("coerceBoolFieldAccess(OpEq) rhs = %#v; want LitString{V:\"true\"}", rhs)
	}

	lhs2, rhs2 := coerceBoolFieldAccess(chplan.OpAnd, fa, tru)
	if lhs2 != chplan.Expr(fa) || rhs2 != chplan.Expr(tru) {
		t.Errorf("coerceBoolFieldAccess(OpAnd) = (%#v, %#v); want both operands unchanged (non-equality op must not coerce)", lhs2, rhs2)
	}
}
