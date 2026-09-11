package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// The two established entry paths expose different range layouts. Keep that
// choice explicit while sharing grouping, reduction, and sample projection.
type plainAggregateLayout uint8

const (
	ordinaryPlainAggregateLayout plainAggregateLayout = iota
	mixedPlainAggregateLayout
	mixedAggregateBucketAlias = "mixed_agg_bucket_ts"
)

// lowerPlainAggregateOverInput is the post-load aggregation kernel. Mixed-arm
// callers retain timestamp canonicalization and a leading bucket key; ordinary
// callers retain their native-grid opportunity and trailing bucket key.
// Admission and quantile-domain guards belong to the respective callers.
func lowerPlainAggregateOverInput(a *parser.AggregateExpr, input chplan.Node, s schema.Metrics, ctx lowerCtx, layout plainAggregateLayout) (chplan.Node, error) {
	if layout == mixedPlainAggregateLayout {
		input = canonicalizeMixedFloatArmForAgg(input, s)
	}
	groupBy, err := aggregateGroupBy(a, s)
	if err != nil {
		return nil, err
	}
	aggFunc, err := buildAggFunc(a, s, ctx)
	if err != nil {
		return nil, err
	}
	labelAliases := groupKeyAliases(len(groupBy))
	rangeBucketed := ctx.step > 0
	// Only an eligible, already-finished per-series native grid can fold the
	// vector reduction before explosion. Pass user keys before bucket widening;
	// quantile is not eligible, so its caller-owned domain guard still runs.
	if layout == ordinaryPlainAggregateLayout && rangeBucketed && ctx.lowerers.VectorAgg {
		if node, ok := tryNativeGridVectorAgg(input, groupBy, labelAliases, aggFunc, s); ok {
			return wrapAggregateForSample(node, a, s, labelAliases, true, rangeBucketAlias), nil
		}
	}
	bucketAlias := rangeBucketAlias
	aliases := labelAliases
	if layout == mixedPlainAggregateLayout {
		bucketAlias = mixedAggregateBucketAlias
	}
	if rangeBucketed {
		bucket := &chplan.ColumnRef{Name: s.TimestampColumn}
		if layout == mixedPlainAggregateLayout {
			groupBy = append([]chplan.Expr{bucket}, groupBy...)
			aliases = append([]string{bucketAlias}, aliases...)
		} else {
			groupBy = append(groupBy, bucket)
			aliases = append(aliases, bucketAlias)
		}
	}
	agg := &chplan.Aggregate{
		Roles: metricRoles(s), Input: input,
		GroupBy: groupBy, GroupByAliases: aliases,
		AggFuncs: []chplan.AggFunc{aggFunc}, DropEmptyOnNoGroup: true,
	}
	return wrapAggregateForSample(agg, a, s, labelAliases, rangeBucketed, bucketAlias), nil
}
