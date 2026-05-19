package logql

import (
	"fmt"

	"github.com/grafana/loki/v3/pkg/logql/syntax"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// lowerVectorAggregation handles `sum(rate(...))`, `avg by (job) (...)`,
// `count without (instance) (...)`, and friends. Mirrors the PromQL
// aggregation lowering: GROUP BY the requested labels (or all-but-the-
// excluded ones for `without`), apply the aggregator on the inner
// stream's `value` column.
//
// `topk` / `bottomk` change output shape (K rows per group) and are
// unsupported; `quantile` requires a parameterised CH aggregate that
// we already support for PromQL.
func lowerVectorAggregation(e *syntax.VectorAggregationExpr, s schema.Logs, lc lowerCtx) (chplan.Node, error) {
	if e.Left == nil {
		return nil, fmt.Errorf("logql: vector-aggregation has nil inner")
	}

	innerExpr, ok := e.Left.(syntax.Expr)
	if !ok {
		return nil, fmt.Errorf("logql: vector-aggregation inner is not an Expr (%T)", e.Left)
	}
	input, err := lower(innerExpr, s, lc)
	if err != nil {
		return nil, err
	}

	groupBy, aliases := vectorAggregationGroupBy(e, s)
	aggFunc, err := buildVectorAggFunc(e, s)
	if err != nil {
		return nil, err
	}

	// In range mode the inner plan is a matrix-shape RangeWindow that
	// exposes a per-row `anchor_ts` column (the eval anchor for that
	// row). The aggregation MUST include that anchor in its GROUP BY
	// keys — otherwise CH collapses every per-step row of a series-set
	// into one aggregate and the wire matrix has a single value per
	// series instead of one per step. Mirrors the PromQL aggregate
	// lowering's `rangeBucketed` path in internal/promql/lower.go.
	const bucketAlias = "bucket_ts"
	rangeBucketed := lc.rangeMode() && isMatrixRangeWindow(input)
	if rangeBucketed {
		groupBy = append(groupBy, &chplan.ColumnRef{Name: "anchor_ts"})
		aliases = append(aliases, bucketAlias)
	}

	agg := &chplan.Aggregate{
		Input:              input,
		GroupBy:            groupBy,
		GroupByAliases:     aliases,
		AggFuncs:           []chplan.AggFunc{aggFunc},
		DropEmptyOnNoGroup: true,
	}
	userAliases := aliases
	if rangeBucketed {
		userAliases = aliases[:len(aliases)-1]
	}
	return wrapVectorAggregateForSample(agg, e, s, userAliases, rangeBucketed, bucketAlias), nil
}

// vectorAggregationGroupBy mirrors PromQL aggregateGroupBy, but Loki's
// LogQL groups against the carrier-stream attributes (ResourceAttributes
// in OTel-CH). The output shape exposes group-key columns as
// `gkey_0`, `gkey_1`, ... so the wrapping Project can build a
// Map(String, String) Attributes column from them.
func vectorAggregationGroupBy(e *syntax.VectorAggregationExpr, s schema.Logs) ([]chplan.Expr, []string) {
	if e.Grouping == nil {
		return nil, nil
	}
	if e.Grouping.Without {
		// `without (k1, k2)`: group by the full ResourceAttributes map
		// minus the excluded keys.
		return []chplan.Expr{
				&chplan.MapWithoutKeys{
					Map:  &chplan.ColumnRef{Name: s.ResourceAttributesColumn},
					Keys: append([]string(nil), e.Grouping.Groups...),
				},
			},
			[]string{"gkey_0"}
	}
	if len(e.Grouping.Groups) == 0 {
		return nil, nil
	}
	out := make([]chplan.Expr, 0, len(e.Grouping.Groups))
	aliases := make([]string, 0, len(e.Grouping.Groups))
	for i, label := range e.Grouping.Groups {
		out = append(out, &chplan.MapAccess{
			Map: &chplan.ColumnRef{Name: s.ResourceAttributesColumn},
			Key: &chplan.LitString{V: label},
		})
		aliases = append(aliases, fmt.Sprintf("gkey_%d", i))
	}
	return out, aliases
}

// buildVectorAggFunc produces the AggFunc for the LogQL operator.
// Output-shape-changing ops (topk, bottomk, sort, sort_desc, quantile)
// are unsupported — CH support exists for quantile but the LogQL
// semantic for K-row-per-group ops needs result shaping.
func buildVectorAggFunc(e *syntax.VectorAggregationExpr, _ schema.Logs) (chplan.AggFunc, error) {
	const valueAlias = "Value"
	valueArg := &chplan.ColumnRef{Name: valueAlias}

	switch e.Operation {
	case syntax.OpTypeSum:
		return chplan.AggFunc{Name: "sum", Args: []chplan.Expr{valueArg}, Alias: valueAlias}, nil
	case syntax.OpTypeAvg:
		return chplan.AggFunc{Name: "avg", Args: []chplan.Expr{valueArg}, Alias: valueAlias}, nil
	case syntax.OpTypeMin:
		return chplan.AggFunc{Name: "min", Args: []chplan.Expr{valueArg}, Alias: valueAlias}, nil
	case syntax.OpTypeMax:
		return chplan.AggFunc{Name: "max", Args: []chplan.Expr{valueArg}, Alias: valueAlias}, nil
	case syntax.OpTypeCount:
		return chplan.AggFunc{Name: "count", Args: []chplan.Expr{valueArg}, Alias: valueAlias}, nil
	case syntax.OpTypeStddev:
		return chplan.AggFunc{Name: "stddevPop", Args: []chplan.Expr{valueArg}, Alias: valueAlias}, nil
	case syntax.OpTypeStdvar:
		return chplan.AggFunc{Name: "varPop", Args: []chplan.Expr{valueArg}, Alias: valueAlias}, nil

	case syntax.OpTypeTopK, syntax.OpTypeBottomK:
		return chplan.AggFunc{}, fmt.Errorf("logql: %s changes output shape and is unsupported", e.Operation)
	case syntax.OpTypeSort, syntax.OpTypeSortDesc:
		return chplan.AggFunc{}, fmt.Errorf("logql: %s requires output ordering rather than aggregation", e.Operation)
	}
	return chplan.AggFunc{}, fmt.Errorf("logql: aggregation operation %q is unsupported", e.Operation)
}

// wrapVectorAggregateForSample mirrors PromQL's wrapAggregateForSample:
// re-shape the aggregate output into the Sample contract so the API
// layer can stream rows through chclient.Sample. LogQL aggregations
// drop `__name__`-equivalent stream identity except for the requested
// grouping labels.
//
//	MetricName  = ''
//	Attributes  = map('lbl0', gkey_0, ...)    for `by (...)`
//	            | gkey_0                        for `without (...)` (mapFilter output)
//	            | empty Map(String,String)      for unaggregated
//	TimeUnix    = now64(9)                      (instant mode)
//	            | <bucketAlias>                  (range mode: per-anchor anchor_ts)
//	Value       = <agg alias>
//
// In range mode (rangeBucketed=true) the inner Aggregate carries the
// per-anchor timestamp under bucketAlias; the outer Project re-aliases
// it to TimeUnix so the canonical Sample shape exposes one row per step
// instead of collapsing to one row per series at `now64(9)`.
func wrapVectorAggregateForSample(agg *chplan.Aggregate, e *syntax.VectorAggregationExpr, s schema.Logs, aliases []string, rangeBucketed bool, bucketAlias string) chplan.Node {
	var attrs chplan.Expr
	switch {
	case len(aliases) == 0:
		attrs = emptyAttrsMap()
	case e.Grouping != nil && e.Grouping.Without:
		attrs = &chplan.ColumnRef{Name: aliases[0]}
	default:
		args := make([]chplan.Expr, 0, len(e.Grouping.Groups)*2)
		for i, label := range e.Grouping.Groups {
			args = append(args, &chplan.LitString{V: label}, &chplan.ColumnRef{Name: aliases[i]})
		}
		attrs = &chplan.FuncCall{Name: "map", Args: args}
	}

	// Use the metrics-schema column names for the Sample contract; the
	// API layer reads MetricName/Attributes/TimeUnix/Value regardless of
	// which head produced the row.
	const (
		metricNameCol = "MetricName"
		attrsCol      = "Attributes"
		tsCol         = "TimeUnix"
		valueCol      = "Value"
	)

	tsExpr := chplan.Expr(&chplan.FuncCall{Name: "now64", Args: []chplan.Expr{&chplan.LitInt{V: 9}}})
	if rangeBucketed {
		tsExpr = &chplan.ColumnRef{Name: bucketAlias}
	}

	return &chplan.Project{
		Input: agg,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: metricNameCol},
			{Expr: attrs, Alias: attrsCol},
			{Expr: tsExpr, Alias: tsCol},
			{Expr: &chplan.ColumnRef{Name: "Value"}, Alias: valueCol},
		},
	}
}

// emptyAttrsMap returns CH `CAST(map(), 'Map(String,String)')` for the
// no-grouping aggregation case. Same helper shape as the PromQL side.
func emptyAttrsMap() chplan.Expr {
	return &chplan.FuncCall{
		Name: "CAST",
		Args: []chplan.Expr{
			&chplan.FuncCall{Name: "map", Args: nil},
			&chplan.LitString{V: "Map(String,String)"},
		},
	}
}
