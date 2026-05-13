package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// lowerHistogramQuantile handles `histogram_quantile(phi, X)`. For PR G,
// X must be a *parser.VectorSelector naming a classic histogram metric
// (target table `otel_metrics_histogram` per the OTel-CH schema). Native
// (exp) histograms route in via PR H — the dispatch shape here is
// deliberately minimal so adding the exp variant later is a small
// addition: check the target table, fork to the exp path.
//
// Lowering produces a chplan.HistogramQuantile node whose Input is a
// scan/filter against the histogram table, surfacing the per-row
// `BucketCounts` (Array(UInt64)) and `ExplicitBounds` (Array(Float64))
// columns. The chsql emitter renders the linear-interpolation arithmetic
// (cumulative-sum + bucket lookup + bound interpolation) on those arrays.
//
// The result is wrapped in a Project to match the Sample contract
// downstream — `MetricName=''` (Prom quantile drops __name__),
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
		return nil, fmt.Errorf("promql: histogram_quantile second argument must be a classic-histogram VectorSelector (native + aggregated forms land in PR H / RC3)")
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

