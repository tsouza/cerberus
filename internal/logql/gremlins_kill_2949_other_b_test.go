package logql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"
)

// This file kills a LIVED gremlins mutant reported on the
// phase4-logql-other-b leg (cerberus issue #2949) in drop_keep.go: the
// INVERT_LOOPCTRL rewrite of the `continue` inside
// [projectSyntheticLabelValue]'s MATCHER loop, where `break` abandons the
// rest of the matcher list and silently returns the synthetic label
// untouched — the plan then keeps a `detected_level` the query asked to
// drop.
//
// The citation names the mutated statement's neighbouring `if` rather
// than the statement itself: the mutant rewrites a bare `continue`, which
// no substring singles out.
//
// NOT KILLABLE — the other two INVERT_LOOPCTRL mutants on this function,
// on the `continue`s guarded by drop_keep.go:`if len(entries) == 0 {` and
// drop_keep.go:`if unconditional {`, cannot be killed by any input.
// Both of those `continue`s sit directly in a `case` body of the stage
// TYPE SWITCH, so Go binds the mutant's `break` to the SWITCH rather than
// to the `for`: it leaves the case, the loop body ends there anyway
// (the switch is its last statement), and the walk proceeds to the next
// stage exactly as `continue` would. Verified by hand-applying both
// mutants — the keep-then-drop and empty-keep-then-drop projections come
// out byte-identical. The matcher-loop `continue` killed below is
// different precisely because its innermost breakable statement is the
// inner `for _, e := range st.Matchers()`.

// requireConditionalRetention asserts that the projection came out as the
// per-row `if(<survives>, <value>, ”)` form rather than the untouched
// value, which is what every one of these mutants degrades it to.
func requireConditionalRetention(t *testing.T, got chplan.Expr, what string) {
	t.Helper()
	fc, ok := got.(*chplan.FuncCall)
	if !ok || fc.Fn != chplan.FnIf {
		t.Fatalf("%s: projectSyntheticLabelValue = %#v, want a *chplan.FuncCall(%q) row predicate — the stage walk stopped early and returned the synthetic label untouched, keeping a value the pipeline drops", what, got, chplan.FnIf)
	}
}

// syntheticValue is the per-use-site value constructor
// [projectSyntheticLabelValue] takes. The literal stands in for the
// synthesised `detected_level` expression; only its identity as a
// non-nil expr matters here.
func syntheticValue() chplan.Expr { return &chplan.LitString{V: "info"} }

// TestDropMatchersScanPastOtherLabels kills the INVERT_LOOPCTRL mutant on
// the `continue` that skips a value-matcher naming a DIFFERENT label,
// guarded by drop_keep.go:projectSyntheticLabelValue:`if e.Matcher.Name != key {`.
//
// `| drop` takes a comma-separated matcher list and the synthetic key can
// sit anywhere in it, so a matcher for another label has to be stepped
// over rather than end the scan. The selector below puts the unrelated
// `foo="x"` FIRST for exactly that reason.
//
//   - ORIGINAL `continue`: `foo="x"` is skipped, the
//     `detected_level="info"` matcher behind it is folded into the
//     survival predicate, and the projection becomes conditional.
//   - MUTANT `break`: the matcher scan ends at `foo="x"`, no predicate is
//     built, and `detected_level` is retained on every row.
func TestDropMatchersScanPastOtherLabels(t *testing.T) {
	t.Parallel()

	const query = `{a="b"} | drop foo="x", detected_level="info"`
	expr, err := syntax.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	got := projectSyntheticLabelValue(expr, detectedLevelLabel, syntheticValue)
	requireConditionalRetention(t, got, query)
}

// NOT KILLABLE — documented, not defended by a test. These are the LIVED
// mutants phase4-logql-other-b reports outside the logpattern subpackage
// (whose own survivors are adjudicated in
// logpattern/gremlins_kill_2949_test.go) that the kill above does not
// reach.
//
// duration.go:`len(marks)*2+1` — ARITHMETIC_BASE on a make() CAPACITY
// hint, carrying two mutants (one per operator). The slice is built with
// append from length 0, so contents and ordering do not depend on the cap,
// and the cap is not reachable from the returned expression. Neither
// mutated form can go negative and panic: the function returns early for
// an empty mark list, so len(marks) >= 1 at the make().
