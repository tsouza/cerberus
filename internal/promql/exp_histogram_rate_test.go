package promql_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_ExpHistogram_RangeFunctionsAreHistogramValued pins issue #1967's
// rate/increase cut and issue #2224's delta/irate/idelta extension: all five
// native-histogram range functions are answerable, and the answer is a
// chplan.HistogramProjection publishing the same thirteen-column
// contract as the bare and `sum()` siblings.
//
// Like `sum()`, and unlike the bare selector, the result's first slot
// must be an EMPTY literal: reference PromQL drops `__name__` from every
// range-vector function result too.
func TestLower_ExpHistogram_RangeFunctionsAreHistogramValued(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	cases := []struct {
		name  string
		query string
		lower func(parser.Expr) (chplan.Node, error)
	}{
		{
			name:  "instant rate",
			query: `rate(latency_exp_hist[5m])`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "instant increase",
			query: `increase(latency_exp_hist[5m])`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "instant delta",
			query: `delta(latency_exp_hist[5m])`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "instant irate",
			query: `irate(latency_exp_hist[5m])`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "instant idelta",
			query: `idelta(latency_exp_hist[5m])`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "instant with matchers",
			query: `rate(latency_exp_hist{service="api"}[10m])`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "parenthesised",
			query: `(rate(latency_exp_hist[5m]))`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "offset",
			query: `rate(latency_exp_hist[5m] offset 10m)`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "delta offset",
			query: `delta(latency_exp_hist[5m] offset 10m)`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "irate with absolute @ pin",
			query: `irate(latency_exp_hist[5m] @ 1767225600)`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "idelta offset",
			query: `idelta(latency_exp_hist[5m] offset 10m)`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "range",
			query: `rate(latency_exp_hist[5m])`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, 30*time.Second)
			},
		},
		{
			name:  "range increase",
			query: `increase(latency_exp_hist[5m])`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, 30*time.Second)
			},
		},
		{
			name:  "range delta",
			query: `delta(latency_exp_hist[5m])`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, 30*time.Second)
			},
		},
		{
			name:  "range irate",
			query: `irate(latency_exp_hist[5m])`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, 30*time.Second)
			},
		},
		{
			name:  "range idelta",
			query: `idelta(latency_exp_hist[5m])`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, 30*time.Second)
			},
		},
		{
			name:  "range with absolute @ pin",
			query: `rate(latency_exp_hist[5m] @ 1767225600)`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, 30*time.Second)
			},
		},
		{
			name:  "range delta offset",
			query: `delta(latency_exp_hist[5m] offset 10m)`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, 30*time.Second)
			},
		},
		{
			name:  "range irate with absolute @ pin",
			query: `irate(latency_exp_hist[5m] @ 1767225600)`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, 30*time.Second)
			},
		},
		{
			name:  "range idelta offset",
			query: `idelta(latency_exp_hist[5m] offset 10m)`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, 30*time.Second)
			},
		},
	}

	wantAliases := []string{
		s.MetricNameColumn, s.AttributesColumn, s.TimestampColumn, s.ValueColumn,
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := tc.lower(expr)
			if err != nil {
				t.Fatalf("lower(%q): unexpected error: %v", tc.query, err)
			}
			if shape := chplan.RowShapeOf(plan); shape != chplan.HistogramRowShape {
				t.Fatalf("lower(%q): plan root publishes %s, want histogram", tc.query, shape)
			}
			hp, ok := plan.(*chplan.HistogramProjection)
			if !ok {
				t.Fatalf("lower(%q): plan root is %T, want *chplan.HistogramProjection", tc.query, plan)
			}
			if !slices.Equal(hp.GroupByAliases, wantAliases) {
				t.Fatalf("lower(%q): leading output aliases = %v, want %v", tc.query, hp.GroupByAliases, wantAliases)
			}
			name, ok := hp.GroupBy[0].(*chplan.LitString)
			if !ok || name.V != "" {
				t.Fatalf("lower(%q): __name__ projection is %#v, want an empty literal — "+
					"a range-vector function result carries no metric name", tc.query, hp.GroupBy[0])
			}
		})
	}
}

// TestLower_ExpHistogram_RangeFunctionsTemporalityMembership pins the
// gauge-versus-counter split for the three functions added in issue #2224.
// irate reconstructs a counter increase, so a DELTA-temporality OTel row uses
// the current observation while a CUMULATIVE row uses reset-aware subtraction.
// delta and idelta are gauge functions: their arithmetic never consults the
// counter temporality column, and carrying it would be dead plan state that can
// accidentally invite counter-reset behavior into a gauge result.
func TestLower_ExpHistogram_RangeFunctionsTemporalityMembership(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	if s.AggregationTemporalityColumn == "" {
		t.Fatal("default exponential-histogram schema has no AggregationTemporality column")
	}
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		fn   string
		want bool
	}{
		{fn: "delta", want: false},
		{fn: "irate", want: true},
		{fn: "idelta", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.fn, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.fn + `(latency_exp_hist[5m])`)
			if err != nil {
				t.Fatalf("ParseExpr(%s): %v", tc.fn, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
			if err != nil {
				t.Fatalf("LowerAt(%s): %v", tc.fn, err)
			}

			var got bool
			chplan.Walk(plan, func(node chplan.Node) bool {
				agg, ok := node.(*chplan.Aggregate)
				if !ok {
					return true
				}
				for _, fn := range agg.AggFuncs {
					for _, arg := range fn.Args {
						if arg.Equal(&chplan.ColumnRef{Name: s.AggregationTemporalityColumn}) {
							got = true
						}
					}
				}
				return true
			})
			if got != tc.want {
				t.Fatalf("%s temporality aggregate present = %v, want %v", tc.fn, got, tc.want)
			}
		})
	}
}

// TestLower_ExpHistogram_RateReachesTheWindowFold pins that `rate()`
// reaches the SHARED exponential-histogram WINDOW fold — the
// counter-reset differencing plus Prometheus's boundary extrapolation —
// rather than reducing the histogram columns some other way.
//
// This is the assertion that would catch the worst plausible bug in this
// lowering: a plan that took each series' NEWEST in-window sample (the
// bare selector's own `argMax(<col>, TimeUnix)` collapse, the easiest
// thing to copy from the sibling file) would still produce a
// HistogramProjection, still emit thirteen columns in contract order, and
// still pass every structural check above — while answering `rate()` with
// a raw cumulative counter reading.
//
// Each marker below is load-bearing:
//
//   - `arrayPopBack` / `arrayPopFront` are counterIncreaseFold's
//     consecutive-pair map, i.e. the counter-reset differencing itself.
//   - `dateDiff` is secondsBetweenTsExpr, which only the
//     boundary-extrapolation factor calls — its absence would mean the
//     window increase ships unextrapolated (the defect #1958 fixed on the
//     classic path).
//   - `groupArray(<Sum>)` is the whole-histogram Sum series this lowering
//     adds; without it the published Sum could only be a single sample's.
//   - `uniqExact` is the two-sample floor, which reference applies per
//     series before emitting anything at all.
//   - the absence of `argMax(<Count>` is the negative half: the bare
//     path's latest-sample collapse must NOT appear on this one.
func TestLower_ExpHistogram_RateReachesTheWindowFold(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(`rate(latency_exp_hist[5m])`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
	if err != nil {
		t.Fatalf("LowerAt: %v", err)
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	for _, want := range []string{
		"arrayPopBack(",
		"arrayPopFront(",
		"dateDiff(",
		"groupArray(`" + s.SumColumn + "`)",
		"groupArray(`" + s.CountColumn + "`)",
		"uniqExact(",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("emitted SQL does not reach the shared native-histogram window fold: missing %q\n%s", want, sql)
		}
	}
	if strings.Contains(sql, "argMax(`"+s.CountColumn+"`") {
		t.Fatalf("emitted SQL collapses to the newest sample per series — that is the BARE selector's "+
			"reduction, not a window rate:\n%s", sql)
	}
}

// TestLower_ExpHistogram_RateDividesByRangeAndIncreaseDoesNot pins the
// one arithmetic difference between the two functions this file answers.
//
// Reference PromQL folds the per-second division into the very scalar the
// boundary extrapolation produces — `factor /= ms.Range.Seconds()` before
// a single `Mul(factor)` over the whole histogram — so `rate` is
// `increase` divided by the range, field for field. The quantile path
// leaves that division out on purpose (a per-series constant cancels out
// of every bucket RATIO), which makes "forgot to divide" the single most
// likely way to ship a plausible-looking wrong answer here: it is
// invisible to every structural assertion and to `histogram_quantile`
// itself, and shows up only as a result 300x too large.
//
// It also pins WHERE the division lands, which is a separate fact from
// whether it happens. Reference divides the factor and then multiplies
// once; dividing the product is the same value in exact arithmetic and a
// different one in float64, and cerberus shipped the latter until the
// histogram-valued compat cases compared bucket counts against reference
// with no epsilon and caught the ulp. Asserting only "a division by 300
// exists somewhere" would have passed on the wrong shape, so the
// assertions below read the multiplication and its factor separately.
//
// The assertion reads the plan rather than the SQL because the divisor is
// a numeric literal, and the emitter parameterises those — `/ ?` in the
// SQL text tells a reader nothing about which number.
func TestLower_ExpHistogram_RateDividesByRangeAndIncreaseDoesNot(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// countProjection returns the window-reduced Count expression, i.e. the
	// projection the histogram row publishes as its total observation count.
	countProjection := func(t *testing.T, query string) chplan.Expr {
		t.Helper()
		expr, err := p.ParseExpr(query)
		if err != nil {
			t.Fatalf("ParseExpr(%q): %v", query, err)
		}
		plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
		if err != nil {
			t.Fatalf("LowerAt(%q): %v", query, err)
		}
		hp, ok := plan.(*chplan.HistogramProjection)
		if !ok {
			t.Fatalf("lower(%q): plan root is %T, want *chplan.HistogramProjection", query, plan)
		}
		reshape, ok := hp.Input.(*chplan.Project)
		if !ok {
			t.Fatalf("lower(%q): projection input is %T, want *chplan.Project", query, hp.Input)
		}
		for _, proj := range reshape.Projections {
			if proj.Alias == s.CountColumn {
				return proj.Expr
			}
		}
		t.Fatalf("lower(%q): reshape publishes no %q projection", query, s.CountColumn)
		return nil
	}

	const fiveMinutesInSeconds = 300.0

	// dividesByRange reports whether e is a division whose divisor is the
	// range literal.
	dividesByRange := func(e chplan.Expr) bool {
		div, ok := e.(*chplan.Binary)
		if !ok || div.Op != chplan.OpDiv {
			return false
		}
		secs, ok := div.Right.(*chplan.LitFloat)
		return ok && secs.V == fiveMinutesInSeconds
	}
	// countShapes walks a Count projection and reports the two shapes that
	// tell the correct placement from the one that shipped. The walk is
	// needed because the fold binds its arrays through hqLet, so the
	// arithmetic sits inside an arrayMap lambda rather than at the root.
	countShapes := func(e chplan.Expr) (factorDivided, productDivided bool) {
		chplan.InspectExpr(e, func(sub chplan.Expr) bool {
			b, ok := sub.(*chplan.Binary)
			if !ok {
				return true
			}
			// factor divided: Mul(increase, Div(factor, range)) — one
			// multiplication by an already-divided scalar, as reference does.
			if b.Op == chplan.OpMul && dividesByRange(b.Right) {
				factorDivided = true
			}
			// product divided: Div(Mul(...), range) — the inverted order.
			if dividesByRange(b) {
				if _, isMul := b.Left.(*chplan.Binary); isMul && b.Left.(*chplan.Binary).Op == chplan.OpMul {
					productDivided = true
				}
			}
			return true
		})
		return factorDivided, productDivided
	}

	rateFactorDivided, rateProductDivided := countShapes(countProjection(t, `rate(latency_exp_hist[5m])`))
	if !rateFactorDivided {
		t.Fatalf("rate's Count projection never divides the extrapolation FACTOR by %v — "+
			"reference computes `factor /= ms.Range.Seconds()` and then scales the histogram once",
			fiveMinutesInSeconds)
	}
	// Dividing the product instead is the same value in exact arithmetic
	// and a different one in float64 — (a*b)/c != a*(b/c) — which is not
	// cosmetic here: a histogram-valued answer is compared against
	// reference bucket by bucket with NO epsilon, and this exact inversion
	// shipped a divergence the compat corpus caught on
	// rate(demo_shifting_latency_exp_hist[1m]).
	if rateProductDivided {
		t.Fatal("rate's Count projection divides the PRODUCT of the increase and the factor — " +
			"the per-second division belongs on the factor, or the answer drifts an ulp off reference")
	}

	increaseFactorDivided, increaseProductDivided := countShapes(countProjection(t, `increase(latency_exp_hist[5m])`))
	if increaseFactorDivided || increaseProductDivided {
		t.Fatalf("increase's Count projection divides by the range (factor=%v product=%v) — "+
			"increase is the window total, NOT a per-second figure",
			increaseFactorDivided, increaseProductDivided)
	}
}
