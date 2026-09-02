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
// mutant at range_aggregation.go:361:4 — the `continue` taken for a dropped
// name in [absentSynthLabels]'s emit loop.
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
			"survive (mutant `continue`->`break` at range_aggregation.go:361:4 stops at the "+
			"dropped name and emits nothing)", q, got)
	}
}

// TestLowerVectorSetOp_MatchingClauseReachesTheSetOp kills the
// CONDITIONALS_NEGATION mutant at binary.go:250:8 — `vm != nil` ->
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
			"query's own `on(app)` clause (mutant `!=`->`==` at binary.go:250:8 copies it only "+
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
