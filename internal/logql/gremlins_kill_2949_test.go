package logql

import (
	"context"
	"testing"
	"time"

	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// Tests in this file kill LIVED gremlins mutants reported on the
// phase4-logql-aggregation and phase4-logql-other-a legs (cerberus issue
// #2949).

// TestAbsentSynthLabels_DroppedNameSkipsNotStops kills the INVERT_LOOPCTRL
// mutant on the `continue` taken for a dropped name in [absentSynthLabels]'s
// emit loop, guarded by range_aggregation.go:`if dropped[name] {`.
//
// The citation names that neighbouring `if` rather than the mutated statement
// itself: the mutant is a bare `continue`, which no substring singles out.
//
// The function mirrors reference Loki's `absentLabels`: an equality matcher
// pins its (name, value) on first sight, and ANY second occurrence of that
// name deletes the label from the output entirely. Deletion is recorded in a
// `dropped` set during the first pass, so the emit loop has to walk PAST a
// dropped name to reach the surviving ones behind it. `break` abandons them,
// and which labels `absent_over_time` synthesises then depends on where in
// the selector the user happened to write the duplicated matcher.
//
// The selector below puts the duplicated name FIRST for exactly that reason:
// with `job` duplicated ahead of `app`, the original emits `app` and the
// mutant emits nothing.
func TestAbsentSynthLabels_DroppedNameSkipsNotStops(t *testing.T) {
	t.Parallel()

	const q = `absent_over_time({job="j", job="k", app="a"}[5m])`
	expr, err := ParseExprPermissive(q)
	if err != nil {
		t.Fatalf("ParseExprPermissive(%q): %v", q, err)
	}
	rng, ok := expr.(*syntax.RangeAggregationExpr)
	if !ok {
		t.Fatalf("ParseExprPermissive(%q) = %T, want *syntax.RangeAggregationExpr", q, expr)
	}

	got := absentSynthLabels(rng.Left.Left)
	if len(got) != 1 || got[0].Key != "app" || got[0].Value != "a" {
		t.Fatalf("absentSynthLabels(%q) = %#v; want the single synthesised label app=\"a\" — "+
			"`job` is duplicated and so is deleted, but the labels behind it in the selector "+
			"survive (mutant `continue`->`break` under "+
			"range_aggregation.go:`if dropped[name] {` stops at the "+
			"dropped name and emits nothing)", q, got)
	}
}

// TestLowerVectorSetOp_MatchingClauseReachesTheSetOp kills the
// CONDITIONALS_NEGATION mutant at binary.go:`if vm != nil {` — rewritten to
// `vm == nil` inside [lowerVectorSetOp].
//
// The guard decides whether the query's own `on(...)` / `ignoring(...)`
// clause is copied into the emitted [chplan.VectorSetOp]'s Match. Negated,
// the copy happens only when there is no clause to copy, so an explicit
// `on(app)` is parsed and then silently discarded and the set operation
// matches on the full series identity instead — a different query, and one
// that answers with different series rather than with an error.
func TestLowerVectorSetOp_MatchingClauseReachesTheSetOp(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	const q = `sum(rate({app="a"}[5m])) and on(app) sum(rate({job="b"}[5m]))`
	expr, err := ParseExprPermissive(q)
	if err != nil {
		t.Fatalf("ParseExprPermissive(%q): %v", q, err)
	}
	plan, err := LowerAtRange(context.Background(), expr, s, start, end, time.Minute)
	if err != nil {
		t.Fatalf("LowerAtRange(%q): %v", q, err)
	}

	setOp := findVectorSetOp(plan)
	if setOp == nil {
		t.Fatalf("LowerAtRange(%q) produced no *chplan.VectorSetOp; the test would then assert "+
			"nothing about the matching clause", q)
	}
	if !setOp.Match.On || len(setOp.Match.Labels) != 1 || setOp.Match.Labels[0] != "app" {
		t.Fatalf("LowerAtRange(%q) produced Match %#v; want On=true with labels [app] — the "+
			"query's own `on(app)` clause (mutant `!=`->`==` at "+
			"binary.go:`if vm != nil {` copies it only "+
			"when there is nothing to copy, leaving the zero Match)", q, setOp.Match)
	}
}

// findVectorSetOp returns the first *chplan.VectorSetOp in the plan, or nil.
func findVectorSetOp(n chplan.Node) *chplan.VectorSetOp {
	if n == nil {
		return nil
	}
	if v, ok := n.(*chplan.VectorSetOp); ok {
		return v
	}
	for _, kid := range n.Children() {
		if hit := findVectorSetOp(kid); hit != nil {
			return hit
		}
	}
	return nil
}

// NOT KILLABLE — documented, not defended by a test. With the two
// outer-by threading mutants killed in vector_aggregation_mutation_test.go
// these are the LIVED mutants phase4-logql-aggregation has left, and no
// input distinguishes any of them.
//
// lang.go:`err == nil && parsed != nil` — INVERT_LOGICAL (`&&` -> `||`).
// The two operands are not independent: they are the same fact. The guard
// runs only under `HasLabelMutatingStage(expr)`, and BOTH of that
// predicate's true-returning paths require expr to be a
// *syntax.PipelineExpr. PipelineLabelsExpr on a *syntax.PipelineExpr
// returns `(nil, err)` on failure and otherwise a labelsExpr that starts
// non-nil and is only ever replaced by a non-nil merge — so `err == nil`
// holds exactly when `parsed != nil`. The `||` form therefore selects the
// same iterations as the `&&` form. (The two CONDITIONALS_NEGATION mutants
// on the same line ARE killed: negating either operand breaks the
// correlation instead of preserving it.) The redundancy itself is a
// finding rather than a defence, and is tracked as cerberus issue #2973.
//
// range_aggregation.go:`len(e.Left.Unwrap.PostFilters) > 0` —
// CONDITIONALS_BOUNDARY (`>` -> `>=`), so the mutant also enters the body
// with an EMPTY post-filter list. applyUnwrapPostFilters is the identity
// on an empty list: it peels the top-level Filter, folds no predicate onto
// it, wraps the labels with an empty mark set (wrapLabelsWithMarks returns
// its argument unchanged for zero marks) and rebuilds the same Filter over
// the same Input with the same Predicate. Its third result is
// `len(marks) > 0` — false — so the caller's `hasErrorMarks` is unchanged
// too. Every field of every returned value is identical.
//
// range_aggregation.go:`len(e.Grouping.Groups)*2` — ARITHMETIC_BASE on a
// make() CAPACITY hint, equivalent for the reason spelled out in
// vector_aggregation_mutation_test.go's header for the sibling site.
