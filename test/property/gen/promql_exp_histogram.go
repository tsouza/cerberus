package gen

import (
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
	"pgregory.net/rapid"

	"github.com/tsouza/cerberus/test/property"
)

// ExpHistogramPhiPool is the `phi` sweep for histogram_quantile over a
// native histogram. It spans the three regions the bucket walk treats
// separately — the phi < 0 / phi > 1 out-of-range clamps, the phi == 0
// and phi == 1 boundary shortcuts, and the interior ranks that
// interpolate inside a bucket.
var ExpHistogramPhiPool = []float64{-0.5, 0, 0.1, 0.5, 0.9, 0.99, 1, 1.5}

// ExpHistogramFractionBoundPool is the pool histogram_fraction's two
// bounds are drawn from. Bounds land below, inside and above the bucket
// range the dataset generator populates, on both sides of zero, so the
// rank walk is exercised in its positive, zero and negative regions as
// well as its two saturation clamps.
var ExpHistogramFractionBoundPool = []float64{-8, -1.5, -0.25, 0, 0.25, 1.5, 8}

// expHistogramDegenerateFractionRate is how often histogram_fraction's
// bounds are left in the lower >= upper order, which is specified to
// yield exactly 0 without consulting a single bucket.
//
// That shape needs covering, but it is a constant-answer query: both
// sides return 0 for free, so it exercises none of the rank walk.
// Drawing both bounds independently from a 7-value pool would leave
// 28/49 of fraction queries degenerate — enough dilution to matter — so
// the bounds are sorted into lower <= upper except one draw in this
// many. Equal bounds survive the sort and stay degenerate, which keeps
// the lower == upper boundary itself in the sweep.
const expHistogramDegenerateFractionRate = 8

// expHistogramValueFns is the exact scalar-function vocabulary. Stable shape
// IDs select these functions individually; this list remains the independent
// accept-set used by generator grammar tests.
var expHistogramValueFns = []string{
	"histogram_count",
	"histogram_sum",
	"histogram_avg",
	"histogram_stddev",
	"histogram_stdvar",
}

// expHistogramEvalOffset is how far past the dataset's last snapshot
// the generated eval timestamp sits. The dataset generator emits at
// most maxExpHistPoints snapshots at expHistogramStep spacing, so the
// last sample is at anchor+45s; anchor+200s leaves a clear margin past
// it while staying well inside Prometheus's 5-minute lookback, so the
// latest-snapshot rule always has exactly one row to answer from.
const expHistogramEvalOffset = 200 * time.Second

// expHistogramRangeWindow includes every generated snapshot at the fixed
// evaluation timestamp. A one-point series still exercises Prometheus's
// silent insufficient-samples drop; every longer series reaches the native
// histogram rate/increase fold.
const expHistogramRangeWindow = 5 * time.Minute

// ExpHistogramQuery returns a rapid generator producing a random
// property.Query over the native (exponential) histogram dataset d
// (see [ExpHistogramDataset]).
//
// Accept-set:
//
//   - histogram_count / histogram_sum / histogram_avg /
//     histogram_stddev / histogram_stdvar (metric{…})
//   - histogram_quantile(phi, metric{…})
//   - histogram_fraction(lower, upper, metric{…})
//   - histogram-valued bare selector, sum/sum-by, rate, and increase
//   - histogram_quantile over bare, sum/sum-by, rate/increase, and
//     sum/sum-by wrapped rate/increase histogram vectors
//
// The five value functions and histogram_fraction remain selector-only,
// matching cerberus's lowering boundary. The other shapes exercise the
// histogram-valued wire comparator and the oracle's merge/window support.
//
// As in [PromQLQuery] the expression is built as a parser AST and
// rendered with String(), so every generated query is guaranteed to
// re-parse.
func ExpHistogramQuery(d property.Dataset) *rapid.Generator[property.Query] {
	return rapid.Custom(func(t *rapid.T) property.Query {
		family := rapid.SampledFrom(expHistogramRandomShapeFamilies).Draw(t, "expHistShapeFamily")
		shapeID := rapid.SampledFrom(family).Draw(t, "expHistShapeID")
		return drawExpHistogramQuery(t, d, shapeID)
	})
}

// ExpHistogramQueryForShape fixes the native-histogram generator to one exact
// roster member for deterministic one-per-shape execution.
func ExpHistogramQueryForShape(
	d property.Dataset,
	shapeID ShapeID,
) *rapid.Generator[property.Query] {
	if !containsShapeID(expHistogramShapeRoster[:], shapeID) {
		panic("gen/promql-native-histogram: unknown shape " + string(shapeID))
	}
	return rapid.Custom(func(t *rapid.T) property.Query {
		return drawExpHistogramQuery(t, d, shapeID)
	})
}

func drawExpHistogramQuery(t *rapid.T, d property.Dataset, shapeID ShapeID) property.Query {
	names := d.Metrics.NamesPresent()
	if len(names) == 0 {
		// The framework rejects this generator defect before execution;
		// preserve a non-panicking diagnostic value for direct callers.
		return property.Query{ShapeID: property.ShapeID(shapeID)}
	}

	name := rapid.SampledFrom(names).Draw(t, "expHistMetric")
	matchers := drawMatchers(t, name, d.Metrics)
	labelNames := mapKeys(d.Metrics.LabelsPresentFor(name))
	groupLabel := ""
	if len(labelNames) > 0 {
		groupLabel = rapid.SampledFrom(labelNames).Draw(t, "expHistGroupLabel")
	}

	return property.Query{
		ShapeID: property.ShapeID(shapeID),
		String:  drawExpHistogramExpr(t, name, matchers, groupLabel, shapeID).String(),
		EvalTs:  AnchorTime().Add(expHistogramEvalOffset).Unix(),
	}
}

// drawExpHistogramExpr picks one of the supported native-histogram shapes.
func drawExpHistogramExpr(
	t *rapid.T,
	name string,
	matchers []*labels.Matcher,
	groupLabel string,
	shapeID ShapeID,
) parser.Expr {
	selectorMatchers := func(grouped bool) []*labels.Matcher {
		out := append([]*labels.Matcher(nil), matchers...)
		if grouped && groupLabel != "" {
			// Prometheus omits an absent grouping label, while cerberus's
			// native-histogram merge materializes it as "" (#2163). Keep this
			// generator on rows where both label shapes are identical.
			out = append(out, labels.MustNewMatcher(labels.MatchNotEqual, groupLabel, ""))
		}
		return out
	}
	sel := func(grouped bool) parser.Expr {
		return &parser.VectorSelector{Name: name, LabelMatchers: selectorMatchers(grouped)}
	}
	window := func(fn string, grouped bool) parser.Expr {
		return &parser.Call{
			Func: parser.Functions[fn],
			Args: []parser.Expr{&parser.MatrixSelector{
				VectorSelector: &parser.VectorSelector{Name: name, LabelMatchers: selectorMatchers(grouped)},
				Range:          expHistogramRangeWindow,
			}},
		}
	}
	sum := func(expr parser.Expr, grouped bool) parser.Expr {
		agg := &parser.AggregateExpr{Op: parser.SUM, Expr: expr}
		if grouped {
			if groupLabel == "" {
				panic("gen/promql-native-histogram: grouped shape has no observed label")
			}
			agg.Grouping = []string{groupLabel}
		}
		return agg
	}
	quantile := func(expr parser.Expr) parser.Expr {
		phi := rapid.SampledFrom(ExpHistogramPhiPool).Draw(t, "expHistPhi")
		return &parser.Call{
			Func: parser.Functions["histogram_quantile"],
			Args: []parser.Expr{&parser.NumberLiteral{Val: phi}, expr},
		}
	}

	switch shapeID {
	case expHistogramCountFunctionShape:
		return &parser.Call{Func: parser.Functions["histogram_count"], Args: []parser.Expr{sel(false)}}
	case expHistogramSumFunctionShape:
		return &parser.Call{Func: parser.Functions["histogram_sum"], Args: []parser.Expr{sel(false)}}
	case expHistogramAverageFunctionShape:
		return &parser.Call{Func: parser.Functions["histogram_avg"], Args: []parser.Expr{sel(false)}}
	case expHistogramStddevFunctionShape:
		return &parser.Call{Func: parser.Functions["histogram_stddev"], Args: []parser.Expr{sel(false)}}
	case expHistogramStdvarFunctionShape:
		return &parser.Call{Func: parser.Functions["histogram_stdvar"], Args: []parser.Expr{sel(false)}}
	case expHistogramFractionShape:
		lower := rapid.SampledFrom(ExpHistogramFractionBoundPool).Draw(t, "expHistFractionLower")
		upper := rapid.SampledFrom(ExpHistogramFractionBoundPool).Draw(t, "expHistFractionUpper")
		degenerate := rapid.IntRange(0, expHistogramDegenerateFractionRate-1).
			Draw(t, "expHistFractionDegenerate") == 0
		if !degenerate && lower > upper {
			lower, upper = upper, lower
		}
		return &parser.Call{
			Func: parser.Functions["histogram_fraction"],
			Args: []parser.Expr{
				&parser.NumberLiteral{Val: lower},
				&parser.NumberLiteral{Val: upper},
				sel(false),
			},
		}
	case expHistogramSelectorShape:
		return sel(false)
	case expHistogramSumShape:
		return sum(sel(false), false)
	case expHistogramSumByShape:
		return sum(sel(true), true)
	case expHistogramRateShape:
		return window("rate", false)
	case expHistogramIncreaseShape:
		return window("increase", false)
	case expHistogramQuantileSelectorShape:
		return quantile(sel(false))
	case expHistogramQuantileSumShape:
		return quantile(sum(sel(false), false))
	case expHistogramQuantileSumByShape:
		return quantile(sum(sel(true), true))
	case expHistogramQuantileRateShape:
		return quantile(window("rate", false))
	case expHistogramQuantileIncreaseShape:
		return quantile(window("increase", false))
	case expHistogramQuantileSumRateShape:
		return quantile(sum(window("rate", false), false))
	case expHistogramQuantileSumByRateShape:
		return quantile(sum(window("rate", true), true))
	case expHistogramQuantileSumIncreaseShape:
		return quantile(sum(window("increase", false), false))
	case expHistogramQuantileSumByIncreaseShape:
		return quantile(sum(window("increase", true), true))
	}
	panic("gen/promql-native-histogram: unhandled shape " + string(shapeID))
}
