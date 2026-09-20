package chsql

import "github.com/tsouza/cerberus/internal/chplan"

// materializeHistogramInput makes a folded histogram an execution boundary.
// A derived SELECT alone lets ClickHouse substitute the producing expressions
// into each consumer. In particular, a quantile can evaluate the rate/reset
// arrays repeatedly. ARRAY JOIN of a singleton tuple preserves exactly one
// row, including empty bucket arrays, while computing the state before its
// consumers. Carry the entire row together so labels and timestamps cannot
// become detached from the histogram they describe.
func materializeHistogramInput(input Frag, row chplan.Schema) Frag {
	const inputTupleAlias = "_cerb_histogram_input"
	values := make([]Frag, len(row.Columns))
	for i, column := range row.Columns {
		values[i] = Col(column.Name)
	}
	materialized := NewQuery().Select(As(Call("arrayJoin", Array(Tuple(values...))), inputTupleAlias)).From(input)
	projected := NewQuery().From(Subquery(materialized))
	for i, column := range row.Columns {
		projected.Select(As(TupleIndex(Col(inputTupleAlias), i+1), column.Name))
	}
	return Subquery(projected)
}
