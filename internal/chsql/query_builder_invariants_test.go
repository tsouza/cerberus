package chsql

import (
	"reflect"
	"testing"
)

// TestQueryBuilder_SlotOrdering_CallOrderIndependent verifies that the
// fluent-API call order does NOT influence the rendered SQL: assembling
// the same slots in different sequences must produce byte-identical
// SQL. The contract is that QueryBuilder owns the canonical clause
// order at render time; the caller is free to chain Select/From/Where/
// GroupBy/OrderBy/Limit in whatever order is readable at the call site.
func TestQueryBuilder_SlotOrdering_CallOrderIndependent(t *testing.T) {
	t.Parallel()

	build1, _ := NewQuery().
		Select(Col("Value")).
		From(Col("t")).
		Where(Eq(Col("MetricName"), Lit("x"))).
		GroupBy(Col("Attributes")).
		OrderBy(Col("Value"), true).
		Limit(10).
		Build()
	build2, _ := NewQuery().
		Limit(10).
		OrderBy(Col("Value"), true).
		GroupBy(Col("Attributes")).
		Where(Eq(Col("MetricName"), Lit("x"))).
		From(Col("t")).
		Select(Col("Value")).
		Build()
	build3, _ := NewQuery().
		From(Col("t")).
		Limit(10).
		Select(Col("Value")).
		OrderBy(Col("Value"), true).
		Where(Eq(Col("MetricName"), Lit("x"))).
		GroupBy(Col("Attributes")).
		Build()

	if build1 != build2 {
		t.Errorf("permutation 1 vs 2 differs:\n  build1 = %q\n  build2 = %q", build1, build2)
	}
	if build1 != build3 {
		t.Errorf("permutation 1 vs 3 differs:\n  build1 = %q\n  build3 = %q", build1, build3)
	}
}

// TestQueryBuilder_SlotOrdering_CanonicalClauseOrder pins the exact
// emission order against the ClickHouse SELECT grammar:
//
//	WITH RECURSIVE … SELECT … FROM … JOIN … PREWHERE … WHERE … GROUP BY … ORDER BY … LIMIT …
//
// Every slot is populated and the rendered string is asserted as one
// long literal. A regression in any slot's emission order surfaces here
// as a single-test break.
func TestQueryBuilder_SlotOrdering_CanonicalClauseOrder(t *testing.T) {
	t.Parallel()

	// Minimal recursive CTE: anchor + step.
	anchor := NewQuery().Select(Col("id")).From(Col("nodes"))
	step := NewQuery().Select(Col("id")).From(Col("nodes"))

	sql, args := NewQuery().
		WithRecursive("closure", anchor, step).
		Select(Col("id")).
		From(Col("closure")).
		Join(InnerJoin, As(Col("nodes"), "n"), Eq(Qual("closure", "id"), Qual("n", "id"))).
		Prewhere(Eq(Col("ServiceName"), Lit("api"))).
		Where(Gt(Col("Value"), Lit(0))).
		GroupBy(Col("Attributes")).
		OrderBy(Col("Value"), true).
		Limit(100).
		Build()

	wantSQL := "WITH RECURSIVE closure AS (" +
		"SELECT `id` FROM `nodes` UNION ALL SELECT `id` FROM `nodes`" +
		") SELECT `id` FROM `closure`" +
		" INNER JOIN `nodes` AS `n` ON `closure`.`id` = `n`.`id`" +
		" PREWHERE `ServiceName` = ?" +
		" WHERE `Value` > ?" +
		" GROUP BY `Attributes`" +
		" ORDER BY `Value` DESC" +
		" LIMIT 100"
	if sql != wantSQL {
		t.Errorf("SQL = %q\nwant %q", sql, wantSQL)
	}
	// PREWHERE arg comes before WHERE arg, matching emission order.
	if want := []any{"api", 0}; !reflect.DeepEqual(args, want) {
		t.Errorf("Args = %v; want %v", args, want)
	}
}

// TestQueryBuilder_Select_AppendsNotReplaces — successive Select calls
// accumulate into a single comma-separated list. The fluent API is
// additive on every slot except From and Limit; Select MUST append so
// the lower-level emitter can do `sb.Select(g)` then `sb.Select(agg)`
// to build the GROUP BY-style select list piecewise.
func TestQueryBuilder_Select_AppendsNotReplaces(t *testing.T) {
	t.Parallel()

	sql, _ := NewQuery().
		Select(Col("a")).
		Select(Col("b"), Col("c")).
		From(Col("t")).
		Build()
	if want := "SELECT `a`, `b`, `c` FROM `t`"; sql != want {
		t.Errorf("multi-call Select = %q; want %q", sql, want)
	}
}

// TestQueryBuilder_Where_AppendsNotReplaces — Where appends; multiple
// calls produce `cond1 AND cond2 AND …`. Same shape as a single
// call passing all conds as variadics.
func TestQueryBuilder_Where_AppendsNotReplaces(t *testing.T) {
	t.Parallel()

	sql1, _ := NewQuery().
		From(Col("t")).
		Where(Eq(Col("a"), Lit(1))).
		Where(Eq(Col("b"), Lit(2))).
		Build()
	sql2, _ := NewQuery().
		From(Col("t")).
		Where(Eq(Col("a"), Lit(1)), Eq(Col("b"), Lit(2))).
		Build()
	if sql1 != sql2 {
		t.Errorf("multi-Where vs variadic-Where differ:\n  multi   = %q\n  variadic = %q", sql1, sql2)
	}
	if want := "SELECT * FROM `t` WHERE `a` = ? AND `b` = ?"; sql1 != want {
		t.Errorf("multi-Where = %q; want %q", sql1, want)
	}
}

// TestQueryBuilder_From_ReplacesNotAppends — From has a single slot;
// calling From twice retains only the last value. This is the
// asymmetry vs Select / Where / GroupBy: there's only one FROM source
// per SELECT, and the emitter relies on the "last write wins" shape so
// rewrites can update it without explicit reset plumbing.
func TestQueryBuilder_From_ReplacesNotAppends(t *testing.T) {
	t.Parallel()

	sql, _ := NewQuery().
		From(Col("first")).
		From(Col("second")).
		Build()
	if want := "SELECT * FROM `second`"; sql != want {
		t.Errorf("repeated From = %q; want %q (last wins)", sql, want)
	}
}

// TestQueryBuilder_Limit_ReplacesNotAppends — Limit has a single
// scalar slot; the last call wins. n <= 0 turns the slot off (no LIMIT
// clause emitted).
func TestQueryBuilder_Limit_ReplacesNotAppends(t *testing.T) {
	t.Parallel()

	sql, _ := NewQuery().From(Col("t")).Limit(10).Limit(100).Build()
	if want := "SELECT * FROM `t` LIMIT 100"; sql != want {
		t.Errorf("repeated Limit = %q; want %q (last wins)", sql, want)
	}

	// Zero (or negative) Limit disables the slot.
	sql0, _ := NewQuery().From(Col("t")).Limit(100).Limit(0).Build()
	if want := "SELECT * FROM `t`"; sql0 != want {
		t.Errorf("Limit(0) after Limit(100) = %q; want %q (slot disabled)", sql0, want)
	}

	// Negative Limit also disables.
	sqlNeg, _ := NewQuery().From(Col("t")).Limit(-1).Build()
	if want := "SELECT * FROM `t`"; sqlNeg != want {
		t.Errorf("Limit(-1) = %q; want %q (no LIMIT emitted)", sqlNeg, want)
	}
}

// TestQueryBuilder_GroupBy_Appends — GroupBy appends. The emitter
// composes piecewise (one expression per call) when lowering Aggregate.
func TestQueryBuilder_GroupBy_Appends(t *testing.T) {
	t.Parallel()

	sql, _ := NewQuery().
		Select(Col("ServiceName"), Col("MetricName"), Call("sum", Col("Value"))).
		From(Col("t")).
		GroupBy(Col("ServiceName")).
		GroupBy(Col("MetricName")).
		Build()
	want := "SELECT `ServiceName`, `MetricName`, sum(`Value`) FROM `t` GROUP BY `ServiceName`, `MetricName`"
	if sql != want {
		t.Errorf("multi-call GroupBy = %q; want %q", sql, want)
	}
}

// TestQueryBuilder_OrderBy_Appends — OrderBy appends; each call adds
// one sort key in the slot order it arrived. (Note: OrderBy takes one
// key per call, unlike GroupBy / Where which take variadics; the per-
// key direction is part of the call so a single variadic shape would
// be awkward.)
func TestQueryBuilder_OrderBy_Appends(t *testing.T) {
	t.Parallel()

	sql, _ := NewQuery().
		From(Col("t")).
		OrderBy(Col("ServiceName"), false).
		OrderBy(Col("Timestamp"), true).
		Build()
	want := "SELECT * FROM `t` ORDER BY `ServiceName`, `Timestamp` DESC"
	if sql != want {
		t.Errorf("multi-OrderBy = %q; want %q", sql, want)
	}
}

// TestQueryBuilder_Prewhere_Appends — Prewhere appends in the same
// shape as Where. The two slots are independent: predicates added to
// Prewhere never bleed into Where and vice-versa.
func TestQueryBuilder_Prewhere_Appends(t *testing.T) {
	t.Parallel()

	sql, _ := NewQuery().
		From(Col("t")).
		Prewhere(Eq(Col("ServiceName"), Lit("api"))).
		Prewhere(Gt(Col("SeverityNumber"), Lit(5))).
		Build()
	want := "SELECT * FROM `t` PREWHERE `ServiceName` = ? AND `SeverityNumber` > ?"
	if sql != want {
		t.Errorf("multi-Prewhere = %q; want %q", sql, want)
	}
}

// TestQueryBuilder_WithRecursive_Multiple — multiple WithRecursive
// calls chain into a single `WITH RECURSIVE n1 AS (...), n2 AS (...)`
// head. Each CTE renders independently with its own anchor + recursive
// arms.
func TestQueryBuilder_WithRecursive_Multiple(t *testing.T) {
	t.Parallel()

	a1 := NewQuery().Select(Col("id")).From(Col("t1"))
	r1 := NewQuery().Select(Col("id")).From(Col("t1"))
	a2 := NewQuery().Select(Col("id")).From(Col("t2"))
	r2 := NewQuery().Select(Col("id")).From(Col("t2"))

	sql, _ := NewQuery().
		WithRecursive("c1", a1, r1).
		WithRecursive("c2", a2, r2).
		Select(Col("id")).
		From(Col("c1")).
		Build()
	want := "WITH RECURSIVE c1 AS (" +
		"SELECT `id` FROM `t1` UNION ALL SELECT `id` FROM `t1`" +
		"), c2 AS (" +
		"SELECT `id` FROM `t2` UNION ALL SELECT `id` FROM `t2`" +
		") SELECT `id` FROM `c1`"
	if sql != want {
		t.Errorf("multi-CTE = %q; want %q", sql, want)
	}
}

// TestQueryBuilder_EmptySelectList_RendersStar — leaving Select
// unset renders `SELECT *`. The lowering pass relies on this default
// when a Scan has no Columns slice.
func TestQueryBuilder_EmptySelectList_RendersStar(t *testing.T) {
	t.Parallel()

	sql, _ := NewQuery().From(Col("t")).Build()
	if want := "SELECT * FROM `t`"; sql != want {
		t.Errorf("empty select list = %q; want %q", sql, want)
	}
}

// TestQueryBuilder_NoFrom_RendersWithoutFromKeyword — leaving From
// unset omits the FROM clause entirely (no dangling "FROM" keyword).
// CH accepts `SELECT 1` style.
func TestQueryBuilder_NoFrom_RendersWithoutFromKeyword(t *testing.T) {
	t.Parallel()

	sql, _ := NewQuery().Select(InlineLit(int64(1))).Build()
	if want := "SELECT 1"; sql != want {
		t.Errorf("no-FROM = %q; want %q", sql, want)
	}
}

// TestQueryBuilder_Frag_WrapsWithParens — QueryBuilder.Frag() emits the
// rendered SELECT wrapped in `(...)` and threads the inner args into the
// outer Builder's args slice at the position the Frag is written. This
// is the contract Subquery / From-as-subquery depend on.
func TestQueryBuilder_Frag_WrapsWithParens(t *testing.T) {
	t.Parallel()

	inner := NewQuery().
		Select(Col("Value")).
		From(Col("t")).
		Where(Eq(Col("k"), Lit("v")))

	outer := NewBuilder()
	inner.Frag()(outer)
	sql, args := outer.Build()
	if want := "(SELECT `Value` FROM `t` WHERE `k` = ?)"; sql != want {
		t.Errorf("Frag wrap = %q; want %q", sql, want)
	}
	if w := []any{"v"}; !reflect.DeepEqual(args, w) {
		t.Errorf("Args = %v; want %v", args, w)
	}
}

// TestQueryBuilder_JoinChain_PreservesOrder — multiple Join calls
// chain in call order, rendered after FROM and before PREWHERE /
// WHERE. Each ON predicate's args interleave at its position.
func TestQueryBuilder_JoinChain_PreservesOrder(t *testing.T) {
	t.Parallel()

	sql, args := NewQuery().
		Select(Qual("L", "id"), Qual("M", "id"), Qual("R", "id")).
		From(As(Col("a"), "L")).
		Join(InnerJoin, As(Col("b"), "M"), Eq(Qual("L", "k"), Lit("m_key"))).
		Join(LeftJoin, As(Col("c"), "R"), Eq(Qual("M", "k"), Lit("r_key"))).
		Where(Eq(Qual("L", "active"), Lit(true))).
		Build()
	wantSQL := "SELECT `L`.`id`, `M`.`id`, `R`.`id` FROM `a` AS `L`" +
		" INNER JOIN `b` AS `M` ON `L`.`k` = ?" +
		" LEFT JOIN `c` AS `R` ON `M`.`k` = ?" +
		" WHERE `L`.`active` = ?"
	if sql != wantSQL {
		t.Errorf("SQL = %q\nwant %q", sql, wantSQL)
	}
	// L→M ON, M→R ON, WHERE args, in that emission order.
	if w := []any{"m_key", "r_key", true}; !reflect.DeepEqual(args, w) {
		t.Errorf("Args = %v; want %v", args, w)
	}
}

// TestQueryBuilder_FluentReturnsSelf — every slot setter returns the
// receiver so the fluent chain is unbroken. A regression to a
// pass-by-value receiver would surface as a "lost" slot in a chain.
func TestQueryBuilder_FluentReturnsSelf(t *testing.T) {
	t.Parallel()

	q := NewQuery()
	if got := q.Select(Col("x")); got != q {
		t.Errorf("Select returned %p; want receiver %p", got, q)
	}
	if got := q.SelectAs(Col("y"), "y"); got != q {
		t.Errorf("SelectAs returned %p; want receiver %p", got, q)
	}
	if got := q.From(Col("t")); got != q {
		t.Errorf("From returned %p; want receiver %p", got, q)
	}
	if got := q.Where(Eq(Col("k"), Lit(1))); got != q {
		t.Errorf("Where returned %p; want receiver %p", got, q)
	}
	if got := q.Prewhere(Eq(Col("k"), Lit(1))); got != q {
		t.Errorf("Prewhere returned %p; want receiver %p", got, q)
	}
	if got := q.GroupBy(Col("k")); got != q {
		t.Errorf("GroupBy returned %p; want receiver %p", got, q)
	}
	if got := q.OrderBy(Col("k"), true); got != q {
		t.Errorf("OrderBy returned %p; want receiver %p", got, q)
	}
	if got := q.Limit(1); got != q {
		t.Errorf("Limit returned %p; want receiver %p", got, q)
	}
	if got := q.Join(CrossJoin, Col("t2"), nil); got != q {
		t.Errorf("Join returned %p; want receiver %p", got, q)
	}
	a := NewQuery()
	r := NewQuery()
	if got := q.WithRecursive("c", a, r); got != q {
		t.Errorf("WithRecursive returned %p; want receiver %p", got, q)
	}
}

// TestQueryBuilder_Build_IsIdempotent — calling Build twice on the
// same QueryBuilder yields byte-identical SQL and matching args. The
// builder must not consume / mutate its slot state on Build, so a
// caller can render twice without surprises.
func TestQueryBuilder_Build_IsIdempotent(t *testing.T) {
	t.Parallel()

	q := NewQuery().
		Select(Col("Value")).
		From(Col("t")).
		Where(Eq(Col("k"), Lit("v")))
	sql1, args1 := q.Build()
	sql2, args2 := q.Build()
	if sql1 != sql2 {
		t.Errorf("Build #1 = %q; #2 = %q (should be identical)", sql1, sql2)
	}
	if !reflect.DeepEqual(args1, args2) {
		t.Errorf("Args differ: %v vs %v", args1, args2)
	}
}
