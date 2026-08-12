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

// ExpHistogramFractionBoundPool is the pool each of
// histogram_fraction's two bounds is drawn from. Bounds land below,
// inside and above the bucket range the dataset generator populates,
// on both sides of zero, so the rank walk is exercised in its
// positive, zero and negative regions as well as its two saturation
// clamps. Drawing both bounds from one pool also produces the
// lower >= upper shape, which is specified to yield exactly 0.
var ExpHistogramFractionBoundPool = []float64{-8, -1.5, -0.25, 0, 0.25, 1.5, 8}

// expHistogramValueFns are the native-histogram functions whose only
// argument is the histogram vector. Each reduces a histogram sample to
// one float, so the result encodes on the Prom wire as an ordinary
// vector and the comparator can diff it directly.
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

// ExpHistogramQuery returns a rapid generator producing a random
// property.Query over the native (exponential) histogram dataset d
// (see [ExpHistogramDataset]).
//
// Accept-set — the seven PromQL functions that read a native
// histogram, each over a bare vector selector:
//
//   - histogram_count / histogram_sum / histogram_avg /
//     histogram_stddev / histogram_stdvar (metric{…})
//   - histogram_quantile(phi, metric{…})
//   - histogram_fraction(lower, upper, metric{…})
//
// The bare selector itself is deliberately NOT in the accept-set: it
// evaluates to a histogram-valued vector, which the Prom wire format
// cerberus speaks cannot encode, so it is not a shape a differential
// comparison of decoded float samples can be run over. Wrapping the
// selector in any of the seven reduces it to a float first.
//
// The argument is always a bare selector because that is exactly
// cerberus's own accept-set for these functions
// (internal/promql/histogram_value_fns.go lowers a *parser.VectorSelector
// argument and nothing else) as well as the oracle's.
//
// As in [PromQLQuery] the expression is built as a parser AST and
// rendered with String(), so every generated query is guaranteed to
// re-parse.
func ExpHistogramQuery(d property.Dataset) *rapid.Generator[property.Query] {
	return rapid.Custom(func(t *rapid.T) property.Query {
		names := d.Metrics.NamesPresent()
		if len(names) == 0 {
			// Run() skips datasets with no series before drawing a
			// query; guard anyway so the generator cannot panic.
			return property.Query{}
		}

		name := rapid.SampledFrom(names).Draw(t, "expHistMetric")
		matchers := drawMatchers(t, name, d.Metrics)

		return property.Query{
			String: drawExpHistogramExpr(t, name, matchers).String(),
			EvalTs: AnchorTime().Add(expHistogramEvalOffset).Unix(),
		}
	})
}

// drawExpHistogramExpr picks one of the three call shapes and fills in
// its scalar arguments.
func drawExpHistogramExpr(t *rapid.T, name string, matchers []*labels.Matcher) parser.Expr {
	sel := &parser.VectorSelector{Name: name, LabelMatchers: matchers}

	switch rapid.IntRange(0, 2).Draw(t, "expHistShape") {
	case 0:
		fn := rapid.SampledFrom(expHistogramValueFns).Draw(t, "expHistValueFn")
		return &parser.Call{Func: parser.Functions[fn], Args: []parser.Expr{sel}}
	case 1:
		phi := rapid.SampledFrom(ExpHistogramPhiPool).Draw(t, "expHistPhi")
		return &parser.Call{
			Func: parser.Functions["histogram_quantile"],
			Args: []parser.Expr{&parser.NumberLiteral{Val: phi}, sel},
		}
	default:
		lower := rapid.SampledFrom(ExpHistogramFractionBoundPool).Draw(t, "expHistFractionLower")
		upper := rapid.SampledFrom(ExpHistogramFractionBoundPool).Draw(t, "expHistFractionUpper")
		return &parser.Call{
			Func: parser.Functions["histogram_fraction"],
			Args: []parser.Expr{
				&parser.NumberLiteral{Val: lower},
				&parser.NumberLiteral{Val: upper},
				sel,
			},
		}
	}
}
