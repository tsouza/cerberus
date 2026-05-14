package chplan

// TopK selects the top (or bottom) K rows of its input by the given
// SortExpr, optionally partitioned by the `By` expressions so the
// limit fires per partition (one K-sized window per group).
//
// This is PromQL's `topk(K, expr)` and `bottomk(K, expr)` shape:
// per-group, return the K rows whose `expr` is largest (topk) or
// smallest (bottomk). Unlike a regular Aggregate the output preserves
// every input column — `by(g1, g2)` only partitions, it does not drop
// labels.
//
// SQL form:
//
//	SELECT <Columns> FROM (<input>) ORDER BY <SortExpr> [DESC] LIMIT K [BY <By>...]
//
// `Desc=true` renders `ORDER BY ... DESC` (topk). `Desc=false` renders
// `ORDER BY ... ASC` (bottomk).
//
// `By` empty means "no partitioning" — the limit applies to the whole
// result, equivalent to bare `LIMIT K`. Non-empty `By` emits CH's
// `LIMIT K BY <exprs>` extension so each partition gets its own
// K-row window.
//
// `Columns` names the explicit projection list for the outer SELECT.
// Empty renders `SELECT *` (preserves the inner subquery's column
// order verbatim). Non-empty renders `SELECT col1, col2, ...` —
// PromQL lowering populates it with the canonical Sample names
// (MetricName / Attributes / TimeUnix / Value) so downstream
// consumers (chDB roundtrip runner, handler projection) see a
// fixed-arity column list rather than the opaque-arity `*`.
type TopK struct {
	Input    Node
	K        int64
	By       []Expr
	SortExpr Expr     // value column reference used as the ORDER BY key
	Desc     bool     // true = topk (DESC), false = bottomk (ASC)
	Columns  []string // explicit outer SELECT column list; empty = `SELECT *`
}

func (*TopK) planNode() {}

func (t *TopK) Children() []Node { return []Node{t.Input} }

func (t *TopK) Equal(other Node) bool {
	o, ok := other.(*TopK)
	if !ok || t.K != o.K || t.Desc != o.Desc ||
		len(t.By) != len(o.By) || len(t.Columns) != len(o.Columns) {
		return false
	}
	if t.SortExpr == nil || o.SortExpr == nil {
		if t.SortExpr != o.SortExpr {
			return false
		}
	} else if !t.SortExpr.Equal(o.SortExpr) {
		return false
	}
	for i := range t.By {
		if !t.By[i].Equal(o.By[i]) {
			return false
		}
	}
	for i := range t.Columns {
		if t.Columns[i] != o.Columns[i] {
			return false
		}
	}
	return t.Input.Equal(o.Input)
}
