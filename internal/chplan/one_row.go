package chplan

// OneRow emits a no-FROM `SELECT 1` — ClickHouse evaluates a bare
// SELECT to a single all-literal row, giving callers a deterministic
// one-row relation without touching a table.
//
// Used by PromQL `time()` and `vector(scalar)`, which both lower to a
// synthetic 1-row vector with `MetricName=”`, no labels, the eval
// timestamp, and a single Value. The surrounding `Project` provides
// the column shape; OneRow's role is to supply the row count.
type OneRow struct{}

func (*OneRow) planNode() {}

func (*OneRow) Children() []Node { return nil }

func (*OneRow) Equal(other Node) bool {
	_, ok := other.(*OneRow)
	return ok
}
