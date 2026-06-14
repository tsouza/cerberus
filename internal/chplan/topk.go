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
// SQL form (literal-K, KExpr == nil, Unordered == false):
//
//	SELECT <Columns> FROM (<input>) ORDER BY <SortExpr> [DESC] LIMIT K [BY <By>...]
//
// SQL form (literal-K, Unordered == true — limitk):
//
//	SELECT <Columns> FROM (<input>) LIMIT K [BY <By>...]
//
// SQL form (computed-K, KExpr != nil):
//
//	SELECT <Columns> FROM (
//	  SELECT *, row_number() OVER (PARTITION BY <By> ORDER BY <SortExpr> [DESC]) AS _rn
//	  FROM (<input>)
//	) WHERE _rn <= (SELECT toUInt64(`Value`) FROM (<KExpr>) LIMIT 1)
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
//
// `KExpr` is the computed-K variant: a chplan subtree whose evaluation
// yields the K integer at execution time. When non-nil, the emitter
// uses the row_number() window pattern instead of `LIMIT K [BY …]` —
// CH does not accept a subquery directly in a LIMIT clause, so the
// "per-partition top-K with computed K" semantics flow through a
// rank-based filter. The subtree is expected to produce a single-row
// vector shape (PromQL `scalar(<vector>)`) whose `Value` column is the
// K integer; the emitter wraps it as `(SELECT toUInt64(Value) FROM
// <KExpr> LIMIT 1)`. `K` and `KExpr` are mutually exclusive — set one
// or the other, never both.
//
// `Unordered` models PromQL's experimental `limitk(K, v)` aggregator:
// per group, return up to K *arbitrary* series — no ranking, no value
// ordering, the surviving series keep their original samples unchanged.
// When `Unordered` is true the emitter omits the ORDER BY entirely and
// renders a bare `LIMIT K BY <By>` (or `LIMIT K` when `By` is empty),
// and `SortExpr` is nil. `Unordered` only composes with the literal-K
// path (`K > 0`, `KExpr == nil`); the ranking variants (topk/bottomk,
// computed-K) keep `Unordered == false`.
type TopK struct {
	Input     Node
	K         int64
	KExpr     Node // computed-K subtree (mutually exclusive with K > 0)
	By        []Expr
	SortExpr  Expr     // value column reference used as the ORDER BY key; nil when Unordered
	Desc      bool     // true = topk (DESC), false = bottomk (ASC)
	Unordered bool     // true = limitk (no ORDER BY, arbitrary K-per-group)
	Columns   []string // explicit outer SELECT column list; empty = `SELECT *`
}

func (*TopK) planNode() {}

func (t *TopK) Children() []Node {
	if t.KExpr != nil {
		return []Node{t.Input, t.KExpr}
	}
	return []Node{t.Input}
}

func (t *TopK) Equal(other Node) bool {
	o, ok := other.(*TopK)
	if !ok || t.K != o.K || t.Desc != o.Desc || t.Unordered != o.Unordered ||
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
	if t.KExpr == nil || o.KExpr == nil {
		if t.KExpr != o.KExpr {
			return false
		}
	} else if !t.KExpr.Equal(o.KExpr) {
		return false
	}
	return t.Input.Equal(o.Input)
}
