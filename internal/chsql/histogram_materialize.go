package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// histogramInputTupleAlias names the single tuple column the histogram input
// boundary carries each row in.
const histogramInputTupleAlias = "_cerb_histogram_input"

// histogramOutputTupleAlias names the single tuple column the classic
// quantile's output boundary carries each answer row in.
const histogramOutputTupleAlias = "_cerb_histogram_output"

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
	return histogramRowBoundary(input, row, histogramInputTupleAlias)
}

// histogramInputField reads the row column at 0-based position i of the
// schema [histogramInputBoundary] was built from.
func histogramInputField(i int) Frag {
	return TupleIndex(Col(histogramInputTupleAlias), i+1)
}

// histogramRowBoundary selects every row of input through a singleton-array
// ARRAY JOIN, the whole row carried as the single tuple column alias.
func histogramRowBoundary(input Frag, row chplan.Schema, alias string) Frag {
	values := make([]Frag, len(row.Columns))
	for i, column := range row.Columns {
		values[i] = Col(column.Name)
	}
	return Subquery(NewQuery().Select(As(Call("arrayJoin", Array(Tuple(values...))), alias)).From(input))
}

// materializeHistogramOutput makes a classic quantile's answer an execution
// boundary on its way out, as materializeHistogramInput does on the way in:
// each answer row passes through a singleton-array ARRAY JOIN before anything
// above reads it.
//
// A result's series-order sort sits above the quantile. At the server default
// (query_plan_execute_functions_after_sorting=1) ClickHouse moves every
// expression of the step beneath a sort that the sort keys do not read to
// after the sort. The sort then buffers the kernel's bucket arrays instead
// of its scalar answer, and the kernel runs over the sort's merged output
// blocks instead of the blocks the stages beneath it hand up. An ARRAY JOIN
// changes the row count, so the server keeps it, and the kernel it reads,
// beneath the sort.
//
// Every column of row needs a name, because the boundary reads each column
// by its name.
func materializeHistogramOutput(answer *QueryBuilder, row chplan.Schema) (*QueryBuilder, error) {
	for i, column := range row.Columns {
		if column.Name == "" {
			return nil, fmt.Errorf("%w: the quantile's output column %d has no name", ErrUnsupported, i)
		}
	}
	projected := NewQuery().From(histogramRowBoundary(Subquery(answer), row, histogramOutputTupleAlias))
	for i, column := range row.Columns {
		projected.Select(As(TupleIndex(Col(histogramOutputTupleAlias), i+1), column.Name))
	}
	return projected, nil
}
