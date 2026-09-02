// Internal-package (`package chplan`) companions to
// gremlins_mutants_test.go. These pin mutants on the unexported
// RewriteChildren helpers (rewriteLeftRight / rewriteIrregularNode),
// which the external chplan_test package can't reach directly. Each
// test drives RewriteChildren with a recursion fn that rewrites a
// single planted sentinel child, constructing the input so the original
// code and the mutated branch produce observably different `changed`
// flags or output trees.
//
// Mutants are named by the construct they rewrite rather than by the
// `file:line:col` address the phase3-optimizer workflow logs report.
package chplan

import (
	"testing"
)

// TestRewriteLeftRight_RebuildsWhenOnlyLeftChanges pins the
// rewrite_children.go:`!lch && !rch` early-return guard inside
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

	node := &CrossJoin{
		Left:  sentinelChild(),
		Right: &Scan{Table: "right_unchanged"},
	}

	out, changed := RewriteChildren(node, rewriteChildrenFn)
	if !changed {
		t.Fatalf("rewriteLeftRight must report changed=true when only Left rewrote (INVERT_LOGICAL `&&`→`||` would return unchanged)")
	}
	cj, ok := out.(*CrossJoin)
	if !ok {
		t.Fatalf("expected *CrossJoin, got %T", out)
	}
	ls, ok := cj.Left.(*Scan)
	if !ok || ls.Table != "REWRITTEN_CHILD" {
		t.Fatalf("expected Left rewritten to REWRITTEN_CHILD, got %#v", cj.Left)
	}
	rs, ok := cj.Right.(*Scan)
	if !ok || rs.Table != "right_unchanged" {
		t.Fatalf("expected Right preserved as right_unchanged, got %#v", cj.Right)
	}
}

// TestRewriteIrregular_TopK_RecursesIntoKExprWhenInputUnchanged pins the
// rewrite_children.go:`optional != nil` guard in rewriteOptionalPair,
// which rewriteIrregularNode's TopK case reaches with KExpr as the
// optional child. The guard means "only recurse into KExpr when it
// exists".
// Flipping `!= nil` to `== nil` (gremlins CONDITIONALS_NEGATION) inverts
// it: a non-nil KExpr would be bypassed (and a nil KExpr would be fed to
// fn). So a TopK whose KExpr carries the only rewrite target — and whose
// Input is unchanged — would be returned UNCHANGED, dropping the KExpr
// rewrite.
//
// Input: TopK{Input: plain Scan (unchanged), KExpr: sentinel}. Original:
// optCh=true → rebuild with rewritten KExpr. Mutant (`== nil`): KExpr is
// non-nil so the branch is bypassed, optCh=false, Input unchanged →
// returns the original unchanged.
func TestRewriteIrregular_TopK_RecursesIntoKExprWhenInputUnchanged(t *testing.T) {
	t.Parallel()

	node := &TopK{
		Input: &Scan{Table: "input_unchanged"},
		KExpr: sentinelChild(),
		K:     1,
	}

	out, changed := RewriteChildren(node, rewriteChildrenFn)
	if !changed {
		t.Fatalf("rewriteIrregularNode must recurse into a non-nil TopK.KExpr (CONDITIONALS_NEGATION `!= nil`→`== nil` would skip it)")
	}
	tk, ok := out.(*TopK)
	if !ok {
		t.Fatalf("expected *TopK, got %T", out)
	}
	ks, ok := tk.KExpr.(*Scan)
	if !ok || ks.Table != "REWRITTEN_CHILD" {
		t.Fatalf("expected KExpr rewritten to REWRITTEN_CHILD, got %#v", tk.KExpr)
	}
	in, ok := tk.Input.(*Scan)
	if !ok || in.Table != "input_unchanged" {
		t.Fatalf("expected Input preserved as input_unchanged, got %#v", tk.Input)
	}
}

// TestRewriteIrregular_MetricsCompare_RebuildsWhenOnlyRootLookupChanges
// pins the rewrite_children.go:`primaryCh || optCh` changed-flag
// disjunction in rewriteOptionalPair, which rewriteIrregularNode's
// MetricsCompare case reaches with RootLookup as the optional child. The
// disjunction means "either child changed — rebuild". Flipping `||` to
// `&&` (gremlins INVERT_LOGICAL) would report unchanged whenever EITHER
// child is unchanged, so a MetricsCompare whose RootLookup rewrote but
// whose Inner did not would be returned UNCHANGED, dropping the
// RootLookup rewrite.
//
// Input: MetricsCompare{Inner: plain Scan (unchanged), RootLookup:
// sentinel}. primaryCh=false, optCh=true. Original: `false || true` =
// true → rebuild. Mutant: `false && true` = false → return unchanged.
func TestRewriteIrregular_MetricsCompare_RebuildsWhenOnlyRootLookupChanges(t *testing.T) {
	t.Parallel()

	node := &MetricsCompare{
		Inner:      &Scan{Table: "inner_unchanged"},
		RootLookup: sentinelChild(),
	}

	out, changed := RewriteChildren(node, rewriteChildrenFn)
	if !changed {
		t.Fatalf("rewriteIrregularNode must report changed=true when only MetricsCompare.RootLookup rewrote (INVERT_LOGICAL `&&`→`||` would return unchanged)")
	}
	mc, ok := out.(*MetricsCompare)
	if !ok {
		t.Fatalf("expected *MetricsCompare, got %T", out)
	}
	rl, ok := mc.RootLookup.(*Scan)
	if !ok || rl.Table != "REWRITTEN_CHILD" {
		t.Fatalf("expected RootLookup rewritten to REWRITTEN_CHILD, got %#v", mc.RootLookup)
	}
	in, ok := mc.Inner.(*Scan)
	if !ok || in.Table != "inner_unchanged" {
		t.Fatalf("expected Inner preserved as inner_unchanged, got %#v", mc.Inner)
	}
}
