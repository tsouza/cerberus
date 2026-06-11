package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// lowerSort implements PromQL `sort(v)` / `sort_desc(v)`: return the
// input instant vector sorted by SAMPLE VALUE — ascending for `sort`,
// descending for `sort_desc`. Both take a single instant-vector
// argument; Prom rejects a range-vector argument at type-check time, so
// the inner expression is always instant-shaped here.
//
// PromQL semantics (prometheus/promql/functions.go::funcSort /
// funcSortDesc): a stable sort on the sample value alone — labels are
// preserved, only row order changes. We lower to an OrderBy on the
// Value column over the inner vector. NaN ordering follows CH's default
// (NaN sorts last in ASC, first in DESC); Prom places NaN last in both
// directions, but the wire renderer keys series by label set rather
// than positional order, so the residual NaN-ordering difference is not
// observable through cerberus's vector/matrix response shapes.
//
// `sort`/`sort_desc` preserve `__name__` — they don't derive a new
// sample, they reorder existing ones — so no MetricName rewrite applies
// (unlike the instant-math fns); the inner plan's columns flow through
// the OrderBy unchanged.
func lowerSort(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if len(c.Args) != 1 {
		return nil, fmt.Errorf("promql: %s expects 1 argument, got %d", c.Func.Name, len(c.Args))
	}
	inner, err := lower(c.Args[0], s, ctx)
	if err != nil {
		return nil, err
	}
	desc := c.Func.Name == "sort_desc"
	return &chplan.OrderBy{
		Input: inner,
		Keys: []chplan.OrderKey{
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Desc: desc},
		},
	}, nil
}
