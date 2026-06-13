package promql

import (
	"fmt"
	"math"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// This file lowers PromQL's native-histogram value functions —
// histogram_count / histogram_sum / histogram_avg / histogram_stddev /
// histogram_stdvar / histogram_fraction — against the OTel-CH
// exponential-histogram table.
//
// Reference semantics (tsouza/prometheus@cerberus-parser
// promql/functions.go, simpleHistogramFunc + histogramVariance +
// funcHistogramFraction): each function maps native-histogram samples
// to a float and SKIPS float samples entirely, so a float-only input
// evaluates to an empty vector (never an error). In cerberus's OTel-CH
// model native histograms live exclusively in the exp-histogram table:
//
//   - a bare VectorSelector argument scans that table (matchers
//     applied verbatim — non-native metric names simply match no
//     rows, the reference's empty result);
//   - every other argument shape (aggregations, arithmetic, …) is a
//     float pipeline by construction and folds to a constant-false
//     Filter over its own lowering — the same posture as
//     histogram_quantile's non-bucket fold.
//
// Per-function value, computed per row from the exp-histogram columns
// (Count, Sum, Scale, ZeroCount, Positive/Negative Offset+BucketCounts;
// base = 2^(2^-Scale)):
//
//   - histogram_count → Count;  histogram_sum → Sum;
//     histogram_avg → Sum / Count.
//   - histogram_stddev / histogram_stdvar → the bucket-midpoint
//     variance estimate (histogramVariance): each standard
//     exponential bucket contributes count·(mid − mean)² with the
//     GEOMETRIC midpoint mid = ±√(lower·upper) = ±base^(k+0.5)
//     (absolute bucket index k), the zero bucket contributes at
//     mid = 0, and the total divides by Count (population variance).
//   - histogram_fraction(l, u, v) → (R(u) − R(l)) / Count, where R(v)
//     is the interpolated rank of v across the bucket walk
//     (promql/quantile.go::HistogramFraction): NaN when Count = 0 or
//     either bound is NaN; 0 when l >= u; exponential (log-scale)
//     interpolation inside the bucket containing a bound. The default
//     OTel-CH schema does not persist zero_threshold, so the zero
//     bucket is a point at 0 and no finite bound can fall inside it —
//     the zeroBucket linear-interpolation branch is unreachable here.
//
// Output rows follow the native histogram_quantile path's Sample
// contract: MetricName=” (derived samples drop __name__), Attributes
// passthrough, Value as above. Sample selection mirrors the native
// histogram_quantile scaffold exactly:
//
//   - Instant (`!histogramRangeApplies(ctx)`): the filtered exp-hist
//     scan is aggregated with `argMax(<col>, TimeUnix)` GROUP BY
//     Attributes, picking the newest sample per series before the
//     value math runs. TimeUnix surfaces as now64(9) (instant eval
//     anchor). Without this the bare scan emits every historical
//     sample per series instead of the latest.
//   - Range (`histogramRangeApplies(ctx)`): the scan cross-joins a
//     StepGrid, each (sample, anchor) pair is filtered to the
//     per-anchor lookback window (instantLookback = 5m), and
//     `argMax(<col>, TimeUnix)` GROUP BY [anchor_ts, Attributes]
//     selects the newest sample per (series, anchor). TimeUnix
//     surfaces as anchor_ts so the matrix pivot sees one row per
//     (series, step) rather than N rows all stamped now64(9).
func lowerHistogramValueFn(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	vecIdx := 0
	if c.Func.Name == "histogram_fraction" {
		if len(c.Args) != 3 {
			return nil, fmt.Errorf("promql: histogram_fraction expects 3 arguments, got %d", len(c.Args))
		}
		vecIdx = 2
	} else if len(c.Args) != 1 {
		return nil, fmt.Errorf("promql: %s expects 1 argument, got %d", c.Func.Name, len(c.Args))
	}

	arg := unwrapParens(c.Args[vecIdx])
	vs, ok := arg.(*parser.VectorSelector)
	if !ok {
		// Float pipelines (aggregations, arithmetic, vector(), …)
		// provably carry no native-histogram samples; the reference
		// skips float samples, so fold to the empty vector while
		// preserving the argument's own lowering errors.
		inner, err := lower(c.Args[vecIdx], s, ctx)
		if err != nil {
			return nil, err
		}
		return &chplan.Filter{
			Input:     inner,
			Predicate: &chplan.LitBool{V: false},
		}, nil
	}

	value, err := histogramValueExpr(c, s, ctx)
	if err != nil {
		return nil, err
	}

	// Range mode (ctx.step > 0): fan the per-series newest-sample
	// selection across the request's step grid so the matrix pivot
	// sees one row per (series, step) instead of N now64(9) rows.
	// Modifier-bearing selectors fall back to the instant path until
	// matrix-anchor handling lands (mirrors the native quantile path).
	if histogramRangeApplies(ctx) && !hasModifier(vs) {
		return lowerHistogramValueFnRange(vs, value, s, ctx), nil
	}

	// Instant mode: aggregate the filtered exp-hist scan with
	// argMax(<col>, TimeUnix) GROUP BY Attributes so the value math
	// reads the newest sample per series, then surface now64(9) as the
	// instant eval anchor. Without the aggregation the bare scan emits
	// every historical sample per series.
	scan := &chplan.Scan{Table: s.ExpHistogramTable}
	pred := buildPredicate(vs.LabelMatchers, s)
	if hasModifier(vs) {
		anchor, err := anchorFromSelector(vs, ctx)
		if err != nil {
			return nil, err
		}
		timeBound := timeBoundExpr(s.TimestampColumn, anchor)
		pred = andExpr(pred, timeBound)
	}
	var input chplan.Node = scan
	if pred != nil {
		input = &chplan.Filter{Input: scan, Predicate: pred}
	}

	agg := &chplan.Aggregate{
		Input:              input,
		GroupBy:            []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
		GroupByAliases:     []string{s.AttributesColumn},
		AggFuncs:           histogramValueLatestAggs(s),
		DropEmptyOnNoGroup: true,
	}

	return &chplan.Project{
		Input: agg,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: chplan.NowNano(), Alias: s.TimestampColumn},
			{Expr: value, Alias: s.ValueColumn},
		},
	}, nil
}

// histogramValueLatestAggs renders the per-series newest-sample
// selection shared by the instant and range value-fn paths: one
// argMax(<col>, TimeUnix) per exp-histogram column the value math
// reads, each aliased back to its schema-canonical name so
// histogramValueExpr's column references resolve unchanged.
//
// The column set is the union read across all six value functions
// (Count/Sum for count/sum/avg, plus Scale/ZeroCount/Positive &
// Negative Offset+BucketCounts for the stddev/stdvar variance and the
// histogram_fraction rank walk). histogram_fraction's scalar bounds
// (args 0/1) are not exp-hist columns and stay inside
// histogramValueExpr.
func histogramValueLatestAggs(s schema.Metrics) []chplan.AggFunc {
	cols := []string{
		s.CountColumn,
		s.SumColumn,
		s.ScaleColumn,
		s.ZeroCountColumn,
		s.PositiveOffsetColumn,
		s.PositiveBucketCountsColumn,
		s.NegativeOffsetColumn,
		s.NegativeBucketCountsColumn,
	}
	aggs := make([]chplan.AggFunc, 0, len(cols))
	for _, col := range cols {
		aggs = append(aggs, chplan.AggFunc{
			Name: "argMax",
			Args: []chplan.Expr{
				&chplan.ColumnRef{Name: col},
				&chplan.ColumnRef{Name: s.TimestampColumn},
			},
			Alias: col,
		})
	}
	return aggs
}

// lowerHistogramValueFnRange builds the range-mode plan tree for a
// native-histogram value function over a bare exp-hist VectorSelector.
//
// It reuses the single-pass bounded sample-side fan-out
// (buildHistogramBucketFanout) shared with the histogram-quantile range
// paths, collapsing the newest exp-hist sample per (series, anchor) via
// argMax(<col>, TimeUnix) GROUP BY [anchor_ts, Attributes], then
// projects the value math with anchor_ts surfaced as TimeUnix.
//
// Plan shape (in chsql output order):
//
//	Project [MetricName='', Attributes, anchor_ts AS TimeUnix, Value]
//	  RangeBucketFanout groupBy=[Attributes] funcs=[argMax(<col>, TimeUnix)…]
//	    Filter(Scan, <matchers>)
func lowerHistogramValueFnRange(
	vs *parser.VectorSelector,
	value chplan.Expr,
	s schema.Metrics,
	ctx lowerCtx,
) chplan.Node {
	scan := &chplan.Scan{Table: s.ExpHistogramTable}
	pred := buildPredicate(vs.LabelMatchers, s)
	anchorRef := &chplan.ColumnRef{Name: histogramAnchorCol}

	agg := buildHistogramBucketFanout(
		scan, pred, instantLookback,
		[]chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
		[]string{s.AttributesColumn},
		histogramValueLatestAggs(s), s, ctx,
	)

	return &chplan.Project{
		Input: agg,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: anchorRef, Alias: s.TimestampColumn},
			{Expr: value, Alias: s.ValueColumn},
		},
	}
}

// histogramValueExpr builds the per-row Value expression for one of
// the six native-histogram value functions.
func histogramValueExpr(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Expr, error) {
	countF := hvFloat(hvCol(s.CountColumn))
	sum := hvCol(s.SumColumn)
	mean := hvDiv(sum, countF)

	switch c.Func.Name {
	case "histogram_count":
		return countF, nil
	case "histogram_sum":
		return sum, nil
	case "histogram_avg":
		return mean, nil
	case "histogram_stddev":
		return hvCall("sqrt", histogramVarianceExpr(s, mean, countF)), nil
	case "histogram_stdvar":
		return histogramVarianceExpr(s, mean, countF), nil
	case "histogram_fraction":
		lowerE, err := histogramFractionBound(c.Args[0], s, ctx)
		if err != nil {
			return nil, err
		}
		upperE, err := histogramFractionBound(c.Args[1], s, ctx)
		if err != nil {
			return nil, err
		}
		return histogramFractionExpr(s, lowerE, upperE, countF), nil
	}
	return nil, fmt.Errorf("promql: %s is not a native-histogram value function", c.Func.Name)
}

// histogramFractionBound lowers one of histogram_fraction's scalar
// bounds: literal trees fold; computed scalars bind via lowerScalarArg.
func histogramFractionBound(e parser.Expr, s schema.Metrics, ctx lowerCtx) (chplan.Expr, error) {
	if v, ok := tryScalarLiteral(e); ok {
		return &chplan.LitFloat{V: v}, nil
	}
	return lowerScalarArg(e, s, ctx)
}

// histogramVarianceExpr renders the bucket-midpoint variance estimate
// (reference histogramVariance, promql/functions.go):
//
//	( Σ_pos c_i·(base^(PO+i-1+0.5) − mean)²
//	+ Σ_neg c_i·(−base^(NO+i-1+0.5) − mean)²
//	+ ZeroCount·(0 − mean)² ) / Count
//
// i is arrayEnumerate's 1-based index, so the absolute bucket index of
// element i is Offset + (i−1) and the geometric midpoint of the
// standard exponential bucket (base^k, base^(k+1)] is base^(k+0.5).
func histogramVarianceExpr(s schema.Metrics, mean, countF chplan.Expr) chplan.Expr {
	sumSide := func(bucketsCol, offsetCol string, negate bool) chplan.Expr {
		// mid = ±pow(base, Offset + (i - 1) + 0.5)
		mid := hvPow(hvBase(s), hvAdd(hvAdd(hvCol(offsetCol), hvSub(hvBare("i"), hvLit(1))), hvLit(0.5)))
		if negate {
			mid = hvSub(hvLit(0), mid)
		}
		delta := hvSub(mid, mean)
		lambda := &chplan.Lambda{
			Params: []string{"c", "i"},
			Body:   hvMul(hvFloat(hvBare("c")), hvMul(delta, delta)),
		}
		return hvCall("arraySum", hvCall("arrayMap", lambda, hvCol(bucketsCol), hvCall("arrayEnumerate", hvCol(bucketsCol))))
	}
	zero := hvMul(hvFloat(hvCol(s.ZeroCountColumn)), hvMul(mean, mean))
	total := hvAdd(hvAdd(sumSide(s.PositiveBucketCountsColumn, s.PositiveOffsetColumn, false),
		sumSide(s.NegativeBucketCountsColumn, s.NegativeOffsetColumn, true)), zero)
	return hvDiv(total, countF)
}

// histogramFractionExpr renders HistogramFraction's
// (R(upper) − R(lower)) / Count with the reference's guard order:
// Count = 0 or NaN bound → NaN; lower >= upper → 0. Both ranks clamp
// to Count (the reference's trailing `rank > count → count` clamp).
func histogramFractionExpr(s schema.Metrics, lowerE, upperE, countF chplan.Expr) chplan.Expr {
	rUpper := hvCall("least", countF, histogramRankExpr(s, upperE))
	rLower := hvCall("least", countF, histogramRankExpr(s, lowerE))
	frac := hvDiv(hvSub(rUpper, rLower), countF)
	return &chplan.FuncCall{
		Name: "multiIf",
		Args: []chplan.Expr{
			hvOr(hvBin(chplan.OpEq, countF, hvLit(0)), hvOr(isNaNExpr(lowerE), isNaNExpr(upperE))),
			&chplan.LitFloat{V: math.NaN()},
			hvBin(chplan.OpGe, lowerE, upperE),
			hvLit(0),
			frac,
		},
	}
}

// histogramRankExpr renders R(v): the interpolated count of
// observations the bucket walk places before value v —
// HistogramFraction's lowerRank/upperRank closed form for standard
// exponential buckets with a point zero bucket:
//
//	v > 0: TN + Z + S_pos(p),  p = log2(v)·2^Scale − PositiveOffset
//	v = 0: TN
//	v < 0: S_neg(q),           q = log2(−v)·2^Scale − NegativeOffset
//
// where S_pos walks the positive buckets with exponential
// interpolation inside the covering bucket (p ∈ (j, j+1] ⇒ full
// counts of buckets ≤ j plus a (p−j) share of bucket j+1) and S_neg
// mirrors it from the most-negative end (the walk iterates negative
// buckets most-negative first, so the rank before bucket index j is
// the count of buckets with HIGHER index, and the in-bucket share is
// (ceil(q) − q)).
func histogramRankExpr(s schema.Metrics, v chplan.Expr) chplan.Expr {
	// arraySum over Array(UInt64) is UInt64; every consumer below sits
	// in a multiIf whose other branches are Float64, and CH refuses a
	// UInt64/Float64 supertype — pin Float64 at the source.
	tn := hvFloat(hvCall("arraySum", hvCol(s.NegativeBucketCountsColumn)))
	z := hvFloat(hvCol(s.ZeroCountColumn))
	tp := hvFloat(hvCall("arraySum", hvCol(s.PositiveBucketCountsColumn)))

	// log-index of |v| in bucket units: log2(|v|) · 2^Scale.
	logIdx := hvMul(hvCall("log2", hvCall("abs", v)), hvPow(hvLit(2), hvFloat(hvCol(s.ScaleColumn))))

	// Positive side: p = logIdx − PositiveOffset.
	p := hvSub(logIdx, hvFloat(hvCol(s.PositiveOffsetColumn)))
	pbcLen := hvFloat(hvCall("length", hvCol(s.PositiveBucketCountsColumn)))
	cumP := hvCall("arrayCumSum", hvCol(s.PositiveBucketCountsColumn))
	posFull := hvFloat(hvSubscript(cumP, hvCall("toUInt32", hvCall("floor", p))))
	posPartial := hvMul(
		hvFloat(hvSubscript(hvCol(s.PositiveBucketCountsColumn), hvAdd(hvCall("toUInt32", hvCall("floor", p)), hvInt(1)))),
		hvSub(p, hvCall("floor", p)),
	)
	sPos := &chplan.FuncCall{
		Name: "multiIf",
		Args: []chplan.Expr{
			hvBin(chplan.OpLe, p, hvLit(0)), hvLit(0),
			hvBin(chplan.OpGe, p, pbcLen), tp,
			hvAdd(posFull, posPartial),
		},
	}

	// Negative side: q = logIdx − NegativeOffset; covering bucket index
	// j = ceil(q) − 1 (0-based); rank = (TN − cum[ceil(q)]) +
	// NBC[ceil(q)]·(ceil(q) − q).
	q := hvSub(logIdx, hvFloat(hvCol(s.NegativeOffsetColumn)))
	nbcLen := hvFloat(hvCall("length", hvCol(s.NegativeBucketCountsColumn)))
	cumN := hvCall("arrayCumSum", hvCol(s.NegativeBucketCountsColumn))
	ceilQ := hvCall("ceil", q)
	negRank := hvAdd(
		hvSub(tn, hvFloat(hvSubscript(cumN, hvCall("toUInt32", ceilQ)))),
		hvMul(
			hvFloat(hvSubscript(hvCol(s.NegativeBucketCountsColumn), hvCall("toUInt32", ceilQ))),
			hvSub(ceilQ, q),
		),
	)
	sNeg := &chplan.FuncCall{
		Name: "multiIf",
		Args: []chplan.Expr{
			hvBin(chplan.OpGe, q, nbcLen), hvLit(0),
			hvBin(chplan.OpLe, q, hvLit(0)), tn,
			negRank,
		},
	}

	return &chplan.FuncCall{
		Name: "multiIf",
		Args: []chplan.Expr{
			hvBin(chplan.OpGt, v, hvLit(0)), hvAdd(hvAdd(tn, z), sPos),
			hvBin(chplan.OpLt, v, hvLit(0)), sNeg,
			tn,
		},
	}
}

// hv* are terse constructors for the deeply-nested chplan.Expr trees
// the histogram value functions build. Local to this file by design —
// the rest of the package writes its (much shallower) trees longhand.
func hvCol(name string) chplan.Expr  { return &chplan.ColumnRef{Name: name} }
func hvBare(name string) chplan.Expr { return &chplan.BareIdent{Name: name} }
func hvLit(v float64) chplan.Expr    { return &chplan.LitFloat{V: v} }
func hvInt(v int64) chplan.Expr      { return &chplan.LitInt{V: v} }

func hvCall(name string, args ...chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{Name: name, Args: args}
}

func hvFloat(e chplan.Expr) chplan.Expr { return hvCall("toFloat64", e) }

func hvBin(op chplan.BinaryOp, l, r chplan.Expr) chplan.Expr {
	return &chplan.Binary{Op: op, Left: l, Right: r}
}

func hvAdd(l, r chplan.Expr) chplan.Expr { return hvBin(chplan.OpAdd, l, r) }
func hvSub(l, r chplan.Expr) chplan.Expr { return hvBin(chplan.OpSub, l, r) }
func hvMul(l, r chplan.Expr) chplan.Expr { return hvBin(chplan.OpMul, l, r) }
func hvDiv(l, r chplan.Expr) chplan.Expr { return hvBin(chplan.OpDiv, l, r) }
func hvOr(l, r chplan.Expr) chplan.Expr  { return hvBin(chplan.OpOr, l, r) }

func hvPow(l, r chplan.Expr) chplan.Expr { return hvCall("pow", l, r) }

func hvSubscript(container, key chplan.Expr) chplan.Expr {
	return &chplan.Subscript{Container: container, Key: key}
}

// hvBase renders base = pow(2, pow(2, -Scale)).
func hvBase(s schema.Metrics) chplan.Expr {
	return hvPow(hvLit(2), hvPow(hvLit(2), hvSub(hvLit(0), hvFloat(hvCol(s.ScaleColumn)))))
}
