package logql

import (
	"fmt"

	"github.com/grafana/loki/v3/pkg/logql/syntax"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// lowerRangeAggregation handles LogQL's metric form:
// `rate({selector}[5m])`, `count_over_time({selector}[5m])`,
// `bytes_rate({selector}[5m])`, `bytes_over_time({selector}[5m])`.
//
// The shape is uniform: lower the inner LogSelectorExpr to a Scan +
// optional Filter, wrap with a Project that synthesises a numeric
// `Value` column (constant 1 for line-counting ops; `length(Body)`
// for byte-counting ops), then wrap with a RangeWindow that aggregates
// over the [end - range, end] window per stream.
//
// `unwrap` and the value-aggregation ops (`avg_over_time`,
// `quantile_over_time`, etc.) require parser-extracted numeric
// columns and defer until parsers land.
func lowerRangeAggregation(e *syntax.RangeAggregationExpr, s schema.Logs) (chplan.Node, error) {
	if e.Left == nil {
		return nil, fmt.Errorf("logql: range-aggregation has nil inner")
	}
	if e.Left.Unwrap != nil {
		return nil, fmt.Errorf("logql: `| unwrap` is not yet supported (parser-extracted numeric values land in M3.2 follow-ups)")
	}
	if e.Grouping != nil && (len(e.Grouping.Groups) > 0 || e.Grouping.Without) {
		return nil, fmt.Errorf("logql: range-aggregation with `by`/`without` grouping is not yet supported (M3.4)")
	}

	inner, err := lower(e.Left.Left, s)
	if err != nil {
		return nil, err
	}

	value, err := rangeValueExpr(e.Operation, s)
	if err != nil {
		return nil, err
	}

	const synthValue = "Value"
	projected := &chplan.Project{
		Input: inner,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.ResourceAttributesColumn}},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}},
			{Expr: value, Alias: synthValue},
		},
	}

	chFunc, err := rangeFuncName(e.Operation)
	if err != nil {
		return nil, err
	}

	return &chplan.RangeWindow{
		Input:           projected,
		Func:            chFunc,
		Range:           e.Left.Interval,
		Offset:          e.Left.Offset,
		TimestampColumn: s.TimestampColumn,
		ValueColumn:     synthValue,
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: s.ResourceAttributesColumn}},
	}, nil
}

// rangeValueExpr returns the per-row Value the RangeWindow aggregates.
// Line-counting ops use constant 1; byte-counting ops use length(Body).
func rangeValueExpr(op string, s schema.Logs) (chplan.Expr, error) {
	switch op {
	case syntax.OpRangeTypeRate, syntax.OpRangeTypeCount:
		return &chplan.LitInt{V: 1}, nil
	case syntax.OpRangeTypeBytesRate, syntax.OpRangeTypeBytes:
		return &chplan.FuncCall{
			Name: "length",
			Args: []chplan.Expr{&chplan.ColumnRef{Name: s.BodyColumn}},
		}, nil
	}
	return nil, fmt.Errorf("logql: range op %s is not yet supported (unwrap-based ops land with parser support)", op)
}

// rangeFuncName maps LogQL range ops to the chplan/chsql RangeWindow
// function name. `rate` and `bytes_rate` use the new "log_rate" func
// (sum/range_seconds — non-counter, vs PromQL's counter "rate").
// `count_over_time` reuses PromQL's identical-shape function name.
// `bytes_over_time` reuses `sum_over_time` since the per-row Value has
// already been projected to length(Body).
func rangeFuncName(op string) (string, error) {
	switch op {
	case syntax.OpRangeTypeRate, syntax.OpRangeTypeBytesRate:
		return "log_rate", nil
	case syntax.OpRangeTypeCount:
		return "count_over_time", nil
	case syntax.OpRangeTypeBytes:
		return "sum_over_time", nil
	}
	return "", fmt.Errorf("logql: range op %s is not yet supported", op)
}
