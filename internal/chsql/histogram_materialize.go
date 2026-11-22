package chsql

import "github.com/tsouza/cerberus/internal/chplan"

// histogramInputTupleAlias names the single tuple column the histogram input
// boundary carries each row in.
const histogramInputTupleAlias = "_cerb_histogram_input"

// materializeHistogramInput makes a folded histogram an execution boundary.
// A derived SELECT alone lets ClickHouse substitute the producing expressions
// into each consumer. In particular, a quantile can evaluate the rate/reset
// arrays repeatedly. ARRAY JOIN of a singleton tuple preserves exactly one
// row, including empty bucket arrays, while computing the state before its
// consumers. Carry the entire row together so labels and timestamps cannot
// become detached from the histogram they describe.
func materializeHistogramInput(input Frag, row chplan.Schema) Frag {
	projected := NewQuery().From(histogramInputBoundary(input, row))
	for i, column := range row.Columns {
		projected.Select(As(histogramInputField(i), column.Name))
	}
	return Subquery(projected)
}

// histogramInputBoundary is the ARRAY JOIN half of
// [materializeHistogramInput]: one output row per input row, the whole row
// carried as the single tuple column [histogramInputTupleAlias]. A consumer
// that reads the fields through [histogramInputField] in its own SELECT
// avoids the unpacking derived query materializeHistogramInput adds.
func histogramInputBoundary(input Frag, row chplan.Schema) Frag {
	values := make([]Frag, len(row.Columns))
	for i, column := range row.Columns {
		values[i] = Col(column.Name)
	}
	return Subquery(NewQuery().Select(As(Call("arrayJoin", Array(Tuple(values...))), histogramInputTupleAlias)).From(input))
}

// histogramInputField reads the row column at 0-based position i of the
// schema [histogramInputBoundary] was built from.
func histogramInputField(i int) Frag {
	return TupleIndex(Col(histogramInputTupleAlias), i+1)
}
