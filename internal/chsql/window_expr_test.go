package chsql

import (
	"errors"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestExprWindow_RendersFnArgsAndPartitionBy pins the core shape:
// `<fn>(<args>) OVER (PARTITION BY <keys>)`. This is the single-group-key
// case a multi-group exp-histogram merge would use (issue #2865, PR 1 of
// 3): PartitionBy carries the same group-key expression the consuming
// Aggregate later GROUP BYs on.
func TestExprWindow_RendersFnArgsAndPartitionBy(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	err := b.Expr(&chplan.WindowExpr{
		Fn:          chplan.FnMin,
		Args:        []chplan.Expr{&chplan.ColumnRef{Name: "Scale"}},
		PartitionBy: []chplan.Expr{&chplan.ColumnRef{Name: "route"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql, _, buildErr := b.Build()
	if buildErr != nil {
		t.Fatalf("unexpected build error: %v", buildErr)
	}
	const want = "min(`Scale`) OVER (PARTITION BY `route`)"
	if sql != want {
		t.Fatalf("got %q, want %q", sql, want)
	}
}

// TestExprWindow_EmptyPartitionByRendersOverEmptyParens pins the
// degenerate no-group-key shape: `OVER ()`, ClickHouse's whole-result-set
// partition. This is the shape that reproduces chplan.ScalarSubquery's
// current single-group behavior without a subquery (issue #2865's
// empirically-verified degenerate case).
func TestExprWindow_EmptyPartitionByRendersOverEmptyParens(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	err := b.Expr(&chplan.WindowExpr{
		Fn:   chplan.FnMin,
		Args: []chplan.Expr{&chplan.ColumnRef{Name: "Scale"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql, _, buildErr := b.Build()
	if buildErr != nil {
		t.Fatalf("unexpected build error: %v", buildErr)
	}
	const want = "min(`Scale`) OVER ()"
	if sql != want {
		t.Fatalf("got %q, want %q", sql, want)
	}
}

// TestExprWindow_MultiplePartitionByKeys pins the multi-group-key shape:
// range mode (issue #2865 item 2) partitions by `[anchor, ...by/without
// labels]`, so PartitionBy must render every key, comma-separated, in
// order.
func TestExprWindow_MultiplePartitionByKeys(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	err := b.Expr(&chplan.WindowExpr{
		Fn:   chplan.FnMin,
		Args: []chplan.Expr{&chplan.ColumnRef{Name: "Scale"}},
		PartitionBy: []chplan.Expr{
			&chplan.ColumnRef{Name: "anchor"},
			&chplan.ColumnRef{Name: "route"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql, _, buildErr := b.Build()
	if buildErr != nil {
		t.Fatalf("unexpected build error: %v", buildErr)
	}
	const want = "min(`Scale`) OVER (PARTITION BY `anchor`, `route`)"
	if sql != want {
		t.Fatalf("got %q, want %q", sql, want)
	}
}

// TestExprWindow_MultipleArgs pins that Args renders every positional
// argument, comma-separated, inside the aggregate call — the same
// positional-args contract chplan.FuncCall has.
func TestExprWindow_MultipleArgs(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	err := b.Expr(&chplan.WindowExpr{
		Fn: chplan.FnMax,
		Args: []chplan.Expr{
			&chplan.ColumnRef{Name: "A"},
			&chplan.ColumnRef{Name: "B"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql, _, buildErr := b.Build()
	if buildErr != nil {
		t.Fatalf("unexpected build error: %v", buildErr)
	}
	const want = "max(`A`, `B`) OVER ()"
	if sql != want {
		t.Fatalf("got %q, want %q", sql, want)
	}
}

// TestExprWindow_ArgErrorPropagates pins that a render error in an Args
// slot reaches the caller instead of silently truncating the SQL. exprWindow
// renders Args inside a Frag closure passed to Window (which cannot itself
// return an error), so the error must be captured and returned explicitly.
func TestExprWindow_ArgErrorPropagates(t *testing.T) {
	t.Parallel()

	err := NewBuilder().Expr(&chplan.WindowExpr{
		Fn:   chplan.FnMin,
		Args: []chplan.Expr{errExpr()},
	})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("WindowExpr with an unrenderable Args entry must return ErrUnsupported, got %v", err)
	}
}

// TestExprWindow_PartitionByErrorPropagates is the PartitionBy sibling of
// TestExprWindow_ArgErrorPropagates: PartitionBy renders through its own
// Frag closures and must surface a render error the same way.
func TestExprWindow_PartitionByErrorPropagates(t *testing.T) {
	t.Parallel()

	err := NewBuilder().Expr(&chplan.WindowExpr{
		Fn:          chplan.FnMin,
		Args:        []chplan.Expr{&chplan.ColumnRef{Name: "Scale"}},
		PartitionBy: []chplan.Expr{errExpr()},
	})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("WindowExpr with an unrenderable PartitionBy entry must return ErrUnsupported, got %v", err)
	}
}
