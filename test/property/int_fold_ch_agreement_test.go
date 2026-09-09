//go:build chdb

// Layer 4 — the ClickHouse-agreement half of the int64 constant-fold property.
//
// # The gap this closes (#3188)
//
// internal/optimizer's TestFoldIntInt_Arithmetic asserts foldIntInt reproduces
// two's-complement wraparound. It can only ever assert that against an oracle
// living in the same process — originally Go's own operator (a restatement of
// the implementation), now an arbitrary-precision reduction that at least
// states the contract explicitly.
//
// Neither answers the question the fold actually owes an answer to. Folding
// `LitInt op LitInt` at plan time REPLACES an arithmetic ClickHouse would
// otherwise have performed itself. The fold is correct only if it computes
// what ClickHouse would have computed. Whether Go's int64 semantics and
// ClickHouse's Int64 semantics agree across the overflow region is a claim
// about ClickHouse, and no in-process oracle can settle it — a suite that
// compares Go against Go passes identically whether they agree or not.
//
// So this asks ClickHouse. The same operand pairs the unit property drives
// through foldIntInt are evaluated by chDB as Int64 arithmetic, and the two
// must match. If ClickHouse ever saturated, promoted, or threw where cerberus
// wraps, the fold would be silently rewriting query results and this is the
// only layer that would notice.
//
// Build-tagged chdb: it needs libchdb.so (`just chdb-install`).

package property

import (
	"database/sql"
	"fmt"
	"math"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/optimizer"
)

// intFoldOps pairs each folded operator with its ClickHouse spelling. The
// optimizer folds exactly these three for int64 (OpDiv declines, and OpMod /
// OpPow are deliberately unfolded because Go and ClickHouse disagree on them).
var intFoldOps = []struct {
	op chplan.BinaryOp
	ch string
}{
	{chplan.OpAdd, "+"},
	{chplan.OpSub, "-"},
	{chplan.OpMul, "*"},
}

// intFoldOperands are the pairs the agreement is checked over: the int64
// boundaries where wrap-vs-saturate-vs-throw actually diverge, plus ordinary
// values so a reader can see the check is not boundary-only.
var intFoldOperands = [][2]int64{
	{0, 0},
	{1, -1},
	{7, 6},
	{-7, 6},
	{math.MaxInt32, math.MaxInt32},
	{math.MinInt32, math.MinInt32},
	{math.MaxInt64, 1},
	{math.MaxInt64, math.MaxInt64},
	{math.MinInt64, 1},
	{math.MinInt64, -1},
	{math.MinInt64, math.MinInt64},
	{math.MaxInt64, -1},
	{math.MaxInt64, 2},
	{math.MinInt64, 2},
}

// chInt64Arith evaluates `a op b` in ClickHouse with both operands pinned to
// Int64, and reports whether ClickHouse produced a value at all.
//
// The casts are load-bearing. An unqualified integer literal is typed by its
// magnitude, so ClickHouse would widen the expression to Int128 and compute a
// non-wrapping result that says nothing about the Int64 arithmetic the fold
// replaces.
func chInt64Arith(t *testing.T, db *sql.DB, op string, a, b int64) (int64, bool) {
	t.Helper()
	q := fmt.Sprintf("SELECT toInt64(%d) %s toInt64(%d)", a, op, b)
	var got int64
	if err := db.QueryRow(q).Scan(&got); err != nil {
		t.Logf("chDB refused %q: %v", q, err)
		return 0, false
	}
	return got, true
}

// TestIntFoldAgreesWithClickHouse is the cross-engine half of the fold
// property: for every folded operator and operand pair, ClickHouse's own Int64
// arithmetic must equal what the optimizer folds to.
func TestIntFoldAgreesWithClickHouse(t *testing.T) {
	db := openChDB(t)

	for _, o := range intFoldOps {
		for _, pair := range intFoldOperands {
			a, b := pair[0], pair[1]
			name := fmt.Sprintf("%s/%d_%s_%d", o.op, a, o.ch, b)
			t.Run(name, func(t *testing.T) {
				chVal, ok := chInt64Arith(t, db, o.ch, a, b)
				if !ok {
					t.Fatalf("ClickHouse refused toInt64(%d) %s toInt64(%d), but the optimizer "+
						"folds it to a literal — folding an expression the engine rejects "+
						"turns a query error into a wrong answer", a, o.ch, b)
				}

				folded, ok := foldLiteralInts(o.op, a, b)
				if !ok {
					t.Fatalf("the optimizer declined to fold %d %s %d, which ClickHouse evaluates "+
						"to %d", a, o.ch, b, chVal)
				}
				if folded != chVal {
					t.Fatalf("fold(%d %s %d) = %d, ClickHouse says %d — the constant fold replaces "+
						"arithmetic ClickHouse would otherwise do, so a disagreement here is a "+
						"silently wrong query result", a, o.ch, b, folded, chVal)
				}
			})
		}
	}
}

// foldLiteralInts runs the SHIPPED ConstantFoldSemantic rule over a plan whose
// projection is `a op b` with both operands int literals, and returns the
// folded value.
//
// It drives the real rule rather than reaching for the private helper, so what
// this test compares against ClickHouse is the same rewrite a production query
// gets — including the walk that decides an expression is foldable at all.
func foldLiteralInts(op chplan.BinaryOp, a, b int64) (int64, bool) {
	plan := &chplan.Project{
		Input: &chplan.Scan{Table: "t", Columns: []string{"v"}},
		Projections: []chplan.Projection{{
			Alias: "folded",
			Expr: &chplan.Binary{
				Op:    op,
				Left:  &chplan.LitInt{V: a},
				Right: &chplan.LitInt{V: b},
			},
		}},
	}

	out, _ := optimizer.ConstantFoldSemantic{}.Apply(plan)
	proj, ok := out.(*chplan.Project)
	if !ok || len(proj.Projections) != 1 {
		return 0, false
	}
	lit, ok := proj.Projections[0].Expr.(*chplan.LitInt)
	if !ok {
		return 0, false
	}
	return lit.V, true
}
