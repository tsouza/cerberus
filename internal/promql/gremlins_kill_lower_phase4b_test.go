// Tests in this file close LIVED and NOT-COVERED gremlins mutants reported
// on the phase4-promql-lower leg against internal/promql/lower.go (cerberus
// issue #3735). See gremlins_kill_test.go for the shared file-header
// convention this file follows, and gremlins_kill_native_grid_guard_test.go
// for the sibling native-grid guard tests this file mirrors.
package promql

import (
	"context"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestBareSubqueryInstantWiden_RequiresBothAnchorConditions kills the
// INVERT_LOGICAL mutant on lower.go's SubqueryExpr case (`aerr == nil &&
// !a.End.IsZero()`, `&&` -> `||`): the bare top-level subquery instant-
// widening branch must widen the inner RangeWindow spine only when the
// anchor resolved WITHOUT error AND carries a non-zero End. Under an
// instant Lower() call with no `@`/offset pin, subqueryAnchor succeeds
// (aerr == nil) but leaves End zero — the exact "one true, one false" case
// that distinguishes && from ||. The mutant fires anyway (aerr == nil is
// true on its own) and widens the RangeWindow's Start using the zero End as
// the anchor, shifting Start away from the zero value the correct guard
// leaves untouched.
func TestBareSubqueryInstantWiden_RequiresBothAnchorConditions(t *testing.T) {
	t.Parallel()
	expr := mustParse(t, `up[10m:1m]`)
	plan, err := Lower(context.Background(), expr, schema.DefaultOTelMetrics())
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	var found *chplan.RangeWindow
	chplan.Walk(plan, func(n chplan.Node) bool {
		if rw, ok := n.(*chplan.RangeWindow); ok && found == nil {
			found = rw
		}
		return true
	})
	if found == nil {
		t.Fatal("no RangeWindow node in the bare-subquery plan")
	}
	if !found.Start.IsZero() {
		t.Errorf("RangeWindow.Start = %v, want the zero value: an unresolved (zero-End) anchor must not widen the spine", found.Start)
	}
}

// TestLowerMatrixSelector_AnchorFillRequiresBothConditions kills the
// INVERT_LOGICAL mutant on lowerMatrixSelector's `anchor.End.IsZero() &&
// !ctx.end.IsZero()` (`&&` -> `||`): the ctx.end back-fill must apply only
// when the selector's own anchor left End unresolved AND the surrounding
// query supplies one. A `@ <literal>` modifier resolves anchor.End on its
// own (first condition false) while LowerAt also supplies a non-zero
// ctx.end (second condition true) — the one-true-one-false case where &&
// keeps the literal anchor and || would overwrite it with ctx.end.
func TestLowerMatrixSelector_AnchorFillRequiresBothConditions(t *testing.T) {
	t.Parallel()
	expr := mustParse(t, `up[5m] @ 1000`)
	start := time.UnixMilli(500_000).UTC()
	end := time.UnixMilli(2_000_000).UTC()
	plan, err := LowerAt(context.Background(), expr, schema.DefaultOTelMetrics(), start, end)
	if err != nil {
		t.Fatalf("LowerAt: %v", err)
	}
	lit := matrixSelectorUpperBoundLiteral(t, plan)
	const wantSeconds = 1000 // the `@ 1000` literal, in seconds
	want := time.Unix(wantSeconds, 0).UTC()
	if !lit.Equal(want) {
		t.Errorf("matrix selector upper bound = %v, want %v (the `@ 1000` literal anchor, not ctx.end)", lit, want)
	}
}

// matrixSelectorUpperBoundLiteral digs out the literal DateTime64 upper
// bound [timeBoundExpr] renders for a matrix selector's window predicate:
// Filter{Predicate: (Timestamp <= toDateTime64(<lit>, ...)) AND ...}.
func matrixSelectorUpperBoundLiteral(t *testing.T, plan chplan.Node) time.Time {
	t.Helper()
	var lit *chplan.LitString
	chplan.Walk(plan, func(n chplan.Node) bool {
		f, ok := n.(*chplan.Filter)
		if !ok || lit != nil {
			return true
		}
		and, ok := f.Predicate.(*chplan.Binary)
		if !ok || and.Op != chplan.OpAnd {
			return true
		}
		le, ok := and.Left.(*chplan.Binary)
		if !ok {
			return true
		}
		fc, ok := le.Right.(*chplan.FuncCall)
		if !ok || fc.Fn != "toDateTime64" || len(fc.Args) == 0 {
			return true
		}
		if s, ok := fc.Args[0].(*chplan.LitString); ok {
			lit = s
		}
		return true
	})
	if lit == nil {
		t.Fatal("no matrix-selector window-bound literal found in the plan")
	}
	parsed, err := time.Parse("2006-01-02 15:04:05.999999999", lit.V)
	if err != nil {
		t.Fatalf("parsing literal %q: %v", lit.V, err)
	}
	return parsed.UTC()
}

// nativeGridInstantAcceptedWindow is a RangeWindow
// [nativeTSGridInstantNode] accepts for `rate` in instant mode: Step == 0,
// a resolved (non-zero) End anchor, no Identity flag, whole-second Range /
// Offset, a plain-scan input, and no inherited temporality column.
func nativeGridInstantAcceptedWindow(s schema.Metrics) *chplan.RangeWindow {
	return &chplan.RangeWindow{
		Input:           nativeGuardInput(s),
		Func:            "rate",
		Range:           nativeGuardRange,
		End:             nativeGuardEnd(),
		TimestampColumn: s.TimestampColumn,
		ValueColumn:     s.ValueColumn,
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
	}
}

// TestNativeTSGridInstantNode_RejectsEachDisqualifyingClause kills both
// INVERT_LOGICAL mutants on nativeTSGridInstantNode's `rw.Identity ||
// rw.Step > 0 || rw.End.IsZero()` guard (lower.go:`nativeTSGridInstantNode`):
// each disjunct is
// independently sufficient to fall back to the fan-out path, so collapsing
// any one `||` into `&&` would require the OTHER two clauses to also hold
// before the reject fires. Each case below perturbs exactly one clause off
// the accepted baseline.
func TestNativeTSGridInstantNode_RejectsEachDisqualifyingClause(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()

	if got := nativeTSGridInstantNode(nativeGridInstantAcceptedWindow(s), "rate", s); got == nil {
		t.Fatal("baseline pinned instant rate window was refused; the rejection cases below would be vacuous")
	}

	for _, tc := range []struct {
		name    string
		perturb func(*chplan.RangeWindow)
	}{
		{"identity window", func(rw *chplan.RangeWindow) { rw.Identity = true }},
		{"non-instant step", func(rw *chplan.RangeWindow) { rw.Step = nativeGuardStep }},
		{"unresolved end", func(rw *chplan.RangeWindow) { rw.End = time.Time{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rw := nativeGridInstantAcceptedWindow(s)
			tc.perturb(rw)
			if got := nativeTSGridInstantNode(rw, "rate", s); got != nil {
				t.Errorf("nativeTSGridInstantNode accepted a %s: %#v; want nil so the fan-out path emits it", tc.name, got)
			}
		})
	}
}

// TestNativeTSGridInstantNode_WholeSecondsConjunction kills the
// INVERT_LOGICAL mutant on `!wholeSeconds(rw.Range) || !wholeSeconds(rw.Offset)`
// (lower.go:`nativeTSGridInstantNode`): a sub-second Range or a sub-second Offset must each
// independently fall back to the fan-out path (issue #3068's truncation
// hazard — see gremlins_kill_native_grid_guard_test.go's sibling tests for
// the full rationale), so collapsing this `||` into `&&` would require BOTH
// to be sub-second before either one's rejection fired.
func TestNativeTSGridInstantNode_WholeSecondsConjunction(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()

	for _, tc := range []struct {
		name string
		mut  func(*chplan.RangeWindow)
	}{
		{"range_sub_second", func(rw *chplan.RangeWindow) { rw.Range = 5*time.Minute + 500*time.Millisecond }},
		{"offset_sub_second", func(rw *chplan.RangeWindow) { rw.Offset = 500 * time.Millisecond }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rw := nativeGridInstantAcceptedWindow(s)
			tc.mut(rw)
			if got := nativeTSGridInstantNode(rw, "rate", s); got != nil {
				t.Errorf("nativeTSGridInstantNode accepted a %s window: %#v; want nil so the fan-out path answers it at full precision", tc.name, got)
			}
		})
	}
}
