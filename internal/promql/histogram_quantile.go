package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// lowerHistogramQuantile handles `histogram_quantile(phi, X)`. X must
// be a *parser.VectorSelector naming a histogram metric — classic
// (target table `otel_metrics_histogram` per the OTel-CH schema) or
// exponential / native (target table `otel_metrics_exp_histogram`).
//
// Routing decision: the metric-name suffix configured on
// schema.Metrics.ExpHistogramSuffix (default `"_exp_hist"`) selects
// the native path; everything else falls through to the classic
// path. PromQL itself has no naming convention for exp histograms;
// this is a cerberus-side heuristic, configurable per deployment.
//
// Lowering produces either a chplan.HistogramQuantile (classic) or
// chplan.HistogramQuantileNative (exp). The chsql emitter renders the
// quantile arithmetic in two flavours: linear interpolation across
// ExplicitBounds × BucketCounts for the classic case, log-scale
// midpoint estimation across PositiveBucketCounts for the native case.
//
// The result is wrapped in a Project to match the Sample contract
// downstream — `MetricName=”` (Prom quantile drops __name__),
// `Attributes` reconstructed from the per-row Attributes column,
// `TimeUnix = now64(9)` (instant eval anchor; M2.1 plumbs real eval
// time), `Value` from the interpolated quantile.
func lowerHistogramQuantile(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if len(c.Args) != 2 {
		return nil, fmt.Errorf("promql: histogram_quantile expects 2 arguments, got %d", len(c.Args))
	}
	phi, ok := tryScalarLiteral(c.Args[0])
	if !ok {
		return nil, fmt.Errorf("promql: histogram_quantile requires a scalar-literal phi (computed phi defers to RC3)")
	}
	vs, ok := unwrapVectorSelector(c.Args[1])
	if !ok {
		return nil, fmt.Errorf("promql: histogram_quantile second argument must be a histogram VectorSelector (aggregated forms land in RC3)")
	}

	if s.IsExpHistogramMetric(vs.Name) {
		return lowerHistogramQuantileNative(vs, phi, s, ctx)
	}

	// Target the classic-histogram table directly — the metric name is
	// not required to carry a `_bucket` suffix (OTel-CH classic
	// histograms are one row per series with parallel BucketCounts +
	// ExplicitBounds arrays; there is no `le` label).
	scan := &chplan.Scan{Table: s.HistogramTable}
	pred := buildPredicate(vs.LabelMatchers, s)
	if hasModifier(vs) {
		anchor, err := anchorFromSelector(vs, ctx)
		if err != nil {
			return nil, err
		}
		timeBound := timeBoundExpr(s.TimestampColumn, anchor)
		if pred == nil {
			pred = timeBound
		} else {
			pred = &chplan.Binary{Op: chplan.OpAnd, Left: pred, Right: timeBound}
		}
	}
	var input chplan.Node = scan
	if pred != nil {
		input = &chplan.Filter{Input: scan, Predicate: pred}
	}

	hq := &chplan.HistogramQuantile{
		Input:                input,
		Phi:                  phi,
		BucketCountsColumn:   s.BucketCountsColumn,
		ExplicitBoundsColumn: s.ExplicitBoundsColumn,
		GroupBy: []chplan.Expr{
			&chplan.ColumnRef{Name: s.AttributesColumn},
		},
		GroupByAliases:   []string{s.AttributesColumn},
		MetricNameColumn: s.MetricNameColumn,
		AttributesColumn: s.AttributesColumn,
		TimestampColumn:  s.TimestampColumn,
	}

	// Wrap in a Project to match the Sample-row contract downstream
	// (MetricName='', Attributes=<gkey>, TimeUnix=now64(9), Value=value).
	// Mirrors wrapAggregateForSample in lower.go.
	return &chplan.Project{
		Input: hq,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: &chplan.FuncCall{Name: "now64", Args: []chplan.Expr{&chplan.LitInt{V: 9}}}, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
		},
	}, nil
}

// lowerHistogramQuantileNative builds the chplan.HistogramQuantileNative
// IR for the exp-histogram path. Mirrors the classic-path scaffold:
// Scan or Filter against the exp-histogram table, then wrap in a
// Project to satisfy the Sample-row contract downstream.
func lowerHistogramQuantileNative(vs *parser.VectorSelector, phi float64, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	scan := &chplan.Scan{Table: s.ExpHistogramTable}
	pred := buildPredicate(vs.LabelMatchers, s)
	if hasModifier(vs) {
		anchor, err := anchorFromSelector(vs, ctx)
		if err != nil {
			return nil, err
		}
		timeBound := timeBoundExpr(s.TimestampColumn, anchor)
		if pred == nil {
			pred = timeBound
		} else {
			pred = &chplan.Binary{Op: chplan.OpAnd, Left: pred, Right: timeBound}
		}
	}
	var input chplan.Node = scan
	if pred != nil {
		input = &chplan.Filter{Input: scan, Predicate: pred}
	}

	hq := &chplan.HistogramQuantileNative{
		Input:                      input,
		Phi:                        phi,
		ScaleColumn:                s.ScaleColumn,
		ZeroCountColumn:            s.ZeroCountColumn,
		ZeroThresholdColumn:        s.ZeroThresholdColumn,
		PositiveOffsetColumn:       s.PositiveOffsetColumn,
		PositiveBucketCountsColumn: s.PositiveBucketCountsColumn,
		NegativeOffsetColumn:       s.NegativeOffsetColumn,
		NegativeBucketCountsColumn: s.NegativeBucketCountsColumn,
		GroupBy: []chplan.Expr{
			&chplan.ColumnRef{Name: s.AttributesColumn},
		},
		GroupByAliases:   []string{s.AttributesColumn},
		MetricNameColumn: s.MetricNameColumn,
		AttributesColumn: s.AttributesColumn,
		TimestampColumn:  s.TimestampColumn,
	}

	return &chplan.Project{
		Input: hq,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: &chplan.FuncCall{Name: "now64", Args: []chplan.Expr{&chplan.LitInt{V: 9}}}, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
		},
	}, nil
}

// unwrapVectorSelector peels ParenExpr / StepInvariantExpr wrappers off
// the argument and returns the bare VectorSelector if any. Mirrors what
// tryScalarLiteral does for NumberLiteral, but for the vector arg.
func unwrapVectorSelector(e parser.Expr) (*parser.VectorSelector, bool) {
	for {
		switch v := e.(type) {
		case *parser.VectorSelector:
			return v, true
		case *parser.ParenExpr:
			e = v.Expr
		case *parser.StepInvariantExpr:
			e = v.Expr
		default:
			return nil, false
		}
	}
}
