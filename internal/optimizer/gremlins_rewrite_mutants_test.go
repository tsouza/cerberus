// Internal-package (`package optimizer`) companions to
// gremlins_mutants_test.go. These pin mutants on the unexported
// rewriteChildren helpers (rewriteLeftRight / rewriteIrregularNode),
// which the external optimizer_test package can't reach directly. Each
// test drives rewriteChildren with a recursion fn that rewrites a
// single planted sentinel child, constructing the input so the original
// code and the mutated branch produce observably different `changed`
// flags or output trees.
//
// Mutant IDs use gremlins's `file:line:col` notation from the
// phase3-optimizer workflow logs.
package optimizer

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestRewriteLeftRight_RebuildsWhenOnlyLeftChanges pins the
// `if !lch && !rch` early-return guard at rule.go:251 inside
// rewriteLeftRight. The condition means "neither child changed — hand
// back the original node". Flipping `&&` to `||` (gremlins
// INVERT_LOGICAL) would early-return whenever EITHER child is unchanged,
// so a binary node whose Left child rewrote but whose Right did not (the
// common case — only one arm contains the rewrite target) would be
// returned UNCHANGED, silently dropping the Left-side rewrite.
//
// Input: CrossJoin{Left: sentinel, Right: plain Scan}. The recursion fn
// rewrites only the sentinel, so lch=true, rch=false. Original: rebuild
// (changed=true, Left=rewritten). Mutant: `!true || !false` = true →
// returns the original unchanged.
func TestRewriteLeftRight_RebuildsWhenOnlyLeftChanges(t *testing.T) {
	t.Parallel()

	node := &chplan.CrossJoin{
		Left:  sentinelChild(),
		Right: &chplan.Scan{Table: "right_unchanged"},
	}

	out, changed := rewriteChildren(node, rewriteChildrenFn)
	if !changed {
		t.Fatalf("rewriteLeftRight must report changed=true when only Left rewrote (INVERT_LOGICAL `&&`→`||` would return unchanged)")
	}
	cj, ok := out.(*chplan.CrossJoin)
	if !ok {
		t.Fatalf("expected *CrossJoin, got %T", out)
	}
	ls, ok := cj.Left.(*chplan.Scan)
	if !ok || ls.Table != "REWRITTEN_CHILD" {
		t.Fatalf("expected Left rewritten to REWRITTEN_CHILD, got %#v", cj.Left)
	}
	rs, ok := cj.Right.(*chplan.Scan)
	if !ok || rs.Table != "right_unchanged" {
		t.Fatalf("expected Right preserved as right_unchanged, got %#v", cj.Right)
	}
}

// TestRewriteIrregular_TopK_RecursesIntoKExprWhenInputUnchanged pins the
// `if v.KExpr != nil` guard at rule.go:463 inside rewriteIrregularNode's
// TopK case. The guard means "only recurse into KExpr when it exists".
// Flipping `!= nil` to `== nil` (gremlins CONDITIONALS_NEGATION) inverts
// it: a non-nil KExpr would be skipped (and a nil KExpr would be fed to
// fn). So a TopK whose KExpr carries the only rewrite target — and whose
// Input is unchanged — would be returned UNCHANGED, dropping the KExpr
// rewrite.
//
// Input: TopK{Input: plain Scan (unchanged), KExpr: sentinel}. Original:
// kCh=true → rebuild with rewritten KExpr. Mutant (`== nil`): KExpr is
// non-nil so the branch is skipped, kCh=false, Input unchanged → returns
// the original unchanged.
func TestRewriteIrregular_TopK_RecursesIntoKExprWhenInputUnchanged(t *testing.T) {
	t.Parallel()

	node := &chplan.TopK{
		Input: &chplan.Scan{Table: "input_unchanged"},
		KExpr: sentinelChild(),
		K:     1,
	}

	out, changed := rewriteChildren(node, rewriteChildrenFn)
	if !changed {
		t.Fatalf("rewriteIrregularNode must recurse into a non-nil TopK.KExpr (CONDITIONALS_NEGATION `!= nil`→`== nil` would skip it)")
	}
	tk, ok := out.(*chplan.TopK)
	if !ok {
		t.Fatalf("expected *TopK, got %T", out)
	}
	ks, ok := tk.KExpr.(*chplan.Scan)
	if !ok || ks.Table != "REWRITTEN_CHILD" {
		t.Fatalf("expected KExpr rewritten to REWRITTEN_CHILD, got %#v", tk.KExpr)
	}
	in, ok := tk.Input.(*chplan.Scan)
	if !ok || in.Table != "input_unchanged" {
		t.Fatalf("expected Input preserved as input_unchanged, got %#v", tk.Input)
	}
}

// TestRewriteIrregular_MetricsCompare_RebuildsWhenOnlyRootLookupChanges
// pins the `if !innerCh && !rootCh` early-return guard at rule.go:507
// inside rewriteIrregularNode's MetricsCompare case. The condition means
// "neither child changed — hand back the original". Flipping `&&` to
// `||` (gremlins INVERT_LOGICAL) would early-return whenever EITHER child
// is unchanged, so a MetricsCompare whose RootLookup rewrote but whose
// Inner did not would be returned UNCHANGED, dropping the RootLookup
// rewrite.
//
// Input: MetricsCompare{Inner: plain Scan (unchanged), RootLookup:
// sentinel}. innerCh=false, rootCh=true. Original: `!false && !true` =
// false → rebuild. Mutant: `!false || !true` = true → return unchanged.
func TestRewriteIrregular_MetricsCompare_RebuildsWhenOnlyRootLookupChanges(t *testing.T) {
	t.Parallel()

	node := &chplan.MetricsCompare{
		Inner:      &chplan.Scan{Table: "inner_unchanged"},
		RootLookup: sentinelChild(),
	}

	out, changed := rewriteChildren(node, rewriteChildrenFn)
	if !changed {
		t.Fatalf("rewriteIrregularNode must report changed=true when only MetricsCompare.RootLookup rewrote (INVERT_LOGICAL `&&`→`||` would return unchanged)")
	}
	mc, ok := out.(*chplan.MetricsCompare)
	if !ok {
		t.Fatalf("expected *MetricsCompare, got %T", out)
	}
	rl, ok := mc.RootLookup.(*chplan.Scan)
	if !ok || rl.Table != "REWRITTEN_CHILD" {
		t.Fatalf("expected RootLookup rewritten to REWRITTEN_CHILD, got %#v", mc.RootLookup)
	}
	in, ok := mc.Inner.(*chplan.Scan)
	if !ok || in.Table != "inner_unchanged" {
		t.Fatalf("expected Inner preserved as inner_unchanged, got %#v", mc.Inner)
	}
}
