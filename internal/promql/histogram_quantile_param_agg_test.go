package promql

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// rungProbeColumn is the stand-in rung array handed to a fold under test.
// Finding it in the folded expression is how these tests tell a fold that
// READS its group's rungs from one that folds to a constant.
const rungProbeColumn = "_rung_probe"

// foldRungs applies a fold to the probe column and reports the result plus
// whether the probe survived into it.
func foldRungs(t *testing.T, fold classicBucketRungFold) (chplan.Expr, bool) {
	t.Helper()
	if fold == nil {
		t.Fatal("fold is nil")
	}
	out := fold(&chplan.ColumnRef{Name: rungProbeColumn})
	if out == nil {
		t.Fatal("fold returned a nil expression")
	}
	readsRungs := false
	chplan.InspectExpr(out, func(e chplan.Expr) bool {
		if c, ok := e.(*chplan.ColumnRef); ok && c.Name == rungProbeColumn {
			readsRungs = true
		}
		return true
	})
	return out, readsRungs
}

// TestClassicBucketLadderFold_OperatorTable pins WHICH PromQL
// aggregations have a per-`le` rung reduction over classic buckets.
//
// The table is the single gate: matchHistogramAggIdiom captures any
// aggregation wrapper, and a nil fold is what routes an operator to the
// empty result. So an operator silently dropping out of this table is
// indistinguishable, from the wire, from a query that matched no data —
// which is exactly how #1569 and #1590 presented.
//
// `quantile` belongs here because it reduces each group to one value like
// any other reducer. `topk` / `bottomk` / `count_values` do not: the first
// two SELECT a subset of the input series keeping their full label sets,
// and the third MINTS a label from sample values. They are tracked in
// #1626, and this test pins that they are absent BY OPERATOR rather than
// by the blanket "carries a parameter" rejection that also excluded
// `quantile`.
func TestClassicBucketLadderFold_OperatorTable(t *testing.T) {
	t.Parallel()

	// A parameterised op needs a Param to be well-formed; the
	// non-parameterised ones must have none.
	param := func(op parser.ItemType) parser.Expr {
		switch op {
		case parser.QUANTILE, parser.TOPK, parser.BOTTOMK:
			return &parser.NumberLiteral{Val: 0.5}
		case parser.COUNT_VALUES:
			return &parser.StringLiteral{Val: "v"}
		}
		return nil
	}

	reducers := []parser.ItemType{
		parser.SUM, parser.AVG, parser.MIN, parser.MAX,
		parser.COUNT, parser.GROUP, parser.STDDEV, parser.STDVAR,
		parser.QUANTILE,
	}
	shapers := []parser.ItemType{parser.TOPK, parser.BOTTOMK, parser.COUNT_VALUES}

	s := schema.DefaultOTelMetrics()

	for _, op := range reducers {
		op := op
		t.Run("reduces/"+op.String(), func(t *testing.T) {
			t.Parallel()
			agg := &parser.AggregateExpr{Op: op, Param: param(op)}
			fold, err := classicBucketLadderFold(agg, s, lowerCtx{})
			if err != nil {
				t.Fatalf("classicBucketLadderFold(%s): %v", op, err)
			}
			if fold == nil {
				t.Fatalf("classicBucketLadderFold(%s) = nil, want a rung reduction — a nil fold routes the whole query to an empty result", op)
			}
			out, readsRungs := foldRungs(t, fold)
			// `group` is the one reducer that legitimately ignores its
			// input: PromQL defines its sample value as a constant.
			if op == parser.GROUP {
				lit, ok := out.(*chplan.LitFloat)
				if !ok || lit.V != promGroupSampleValue {
					t.Fatalf("group fold = %#v, want the constant %v PromQL writes into every `group` sample", out, promGroupSampleValue)
				}
				return
			}
			if !readsRungs {
				t.Fatalf("%s fold does not reference its rung array; a reduction that ignores its input cannot depend on the data", op)
			}
		})
	}

	for _, op := range shapers {
		op := op
		t.Run("no_fold/"+op.String(), func(t *testing.T) {
			t.Parallel()
			agg := &parser.AggregateExpr{Op: op, Param: param(op)}
			fold, err := classicBucketLadderFold(agg, s, lowerCtx{})
			if err != nil {
				t.Fatalf("classicBucketLadderFold(%s): %v", op, err)
			}
			if fold != nil {
				t.Fatalf("classicBucketLadderFold(%s) returned a fold, but %s reshapes series identity rather than reducing a group to one value per rung (#1626) — a fold here would emit a plausible wrong number", op, op)
			}
		})
	}
}

// TestClassicBucketLadderFold_NilAggIsSum pins the bare-`rate(...)` case:
// with no aggregation wrapper the window collapse still folds every
// in-window row of a series into one ladder, and summing is what that
// collapse means.
func TestClassicBucketLadderFold_NilAggIsSum(t *testing.T) {
	t.Parallel()

	fold, err := classicBucketLadderFold(nil, schema.DefaultOTelMetrics(), lowerCtx{})
	if err != nil {
		t.Fatalf("classicBucketLadderFold(nil): %v", err)
	}
	out, readsRungs := foldRungs(t, fold)
	if !readsRungs {
		t.Fatal("bare-rate fold does not reference its rung array")
	}
	call, ok := out.(*chplan.FuncCall)
	if !ok || call.Name != "arraySum" {
		t.Fatalf("bare-rate fold = %#v, want arraySum", out)
	}
}

// TestPromQuantileRungFold_PhiDomain pins PromQL's phi-domain rule for a
// LITERAL phi (prometheus/promql/quantile.go): an out-of-domain phi is
// not interpolated at all — the aggregation's value is the NaN / ±Inf
// constant, so every rung of the ladder carries it. Folding it at
// lowering time also keeps ClickHouse's quantile domain check out of the
// picture entirely.
func TestPromQuantileRungFold_PhiDomain(t *testing.T) {
	t.Parallel()

	outOfDomain := []struct {
		phi  float64
		want float64
	}{
		{phi: -0.5, want: math.Inf(-1)},
		{phi: 1.5, want: math.Inf(1)},
		{phi: math.NaN(), want: math.NaN()},
	}
	for _, tc := range outOfDomain {
		tc := tc
		t.Run(fmt.Sprintf("out_of_domain/phi=%v", tc.phi), func(t *testing.T) {
			t.Parallel()
			out, readsRungs := foldRungs(t, promQuantileRungFold(&chplan.LitFloat{V: tc.phi}))
			if readsRungs {
				t.Fatalf("phi=%v folded to an expression reading the rungs; PromQL replaces the value outright", tc.phi)
			}
			lit, ok := out.(*chplan.LitFloat)
			if !ok {
				t.Fatalf("phi=%v folded to %#v, want a constant", tc.phi, out)
			}
			if math.IsNaN(tc.want) {
				if !math.IsNaN(lit.V) {
					t.Fatalf("phi=%v folded to %v, want NaN", tc.phi, lit.V)
				}
				return
			}
			if lit.V != tc.want {
				t.Fatalf("phi=%v folded to %v, want %v", tc.phi, lit.V, tc.want)
			}
		})
	}

	for _, phi := range []float64{0, 0.5, 1} {
		phi := phi
		t.Run(fmt.Sprintf("in_domain/phi=%v", phi), func(t *testing.T) {
			t.Parallel()
			out, readsRungs := foldRungs(t, promQuantileRungFold(&chplan.LitFloat{V: phi}))
			if !readsRungs {
				t.Fatalf("phi=%v folded to %#v without reading the rungs; an in-domain quantile must interpolate the group's values", phi, out)
			}
			// The rung array must be evaluated ONCE: it is an arrayFilter
			// over an arrayMap, so a fold that inlined it per reference
			// would re-run the filter for every term of the interpolation.
			refs := 0
			chplan.InspectExpr(out, func(e chplan.Expr) bool {
				if c, ok := e.(*chplan.ColumnRef); ok && c.Name == rungProbeColumn {
					refs++
				}
				return true
			})
			if refs != 1 {
				t.Fatalf("phi=%v fold references the rung array %d times, want exactly 1 — see bindOnce", phi, refs)
			}
		})
	}
}

// TestHistogramQuantile_QuantileAgg_ReachesHistogramPath pins the routing
// half of #1590 at the plan level: `histogram_quantile(phi, quantile
// by(le)(q, rate(<classic>[r])))` must reach the classic-histogram
// lowering in BOTH evaluation modes rather than degenerating to the
// empty-result Filter the unrecognised-shape fallback produces.
//
// The value the plan computes is pinned separately, and executably, by
// test/spec/promql/histogram_quantile_quantile_by_le.txtar — a plan-shape
// assertion alone cannot tell a correct quantile from a wrong one.
func TestHistogramQuantile_QuantileAgg_ReachesHistogramPath(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})
	const query = `histogram_quantile(0.5, quantile by (le) (0.9, rate(http_server_request_duration_bucket[5m])))`

	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}

	rangeStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	modes := []struct {
		name string
		ctx  lowerCtx
	}{
		{name: "instant", ctx: lowerCtx{}},
		{name: "range", ctx: lowerCtx{
			start: rangeStart,
			end:   rangeStart.Add(5 * time.Minute),
			step:  time.Minute,
		}},
	}
	for _, mode := range modes {
		mode := mode
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			plan, err := lower(expr, s, mode.ctx)
			if err != nil {
				t.Fatalf("lower(%q): %v", query, err)
			}
			shape := inspectHistogramPlan(plan)
			if !shape.hasHistogramQuantile {
				t.Fatalf("plan for %q has no HistogramQuantile node — the shape fell through to the empty-result fallback (#1590)", query)
			}
			emptyFolds := 0
			chplan.Walk(plan, func(n chplan.Node) bool {
				if f, ok := n.(*chplan.Filter); ok {
					if b, ok := f.Predicate.(*chplan.LitBool); ok && !b.V {
						emptyFolds++
					}
				}
				return true
			})
			if emptyFolds != 0 {
				t.Fatalf("plan for %q contains %d always-false Filter(s); the query returns rows in reference Prometheus", query, emptyFolds)
			}
		})
	}
}

// TestHistogramQuantile_QuantileAgg_ComputedPhi pins that a COMPUTED
// quantile parameter (`quantile(scalar(x), ...)`) lowers rather than
// falling through to the empty result — the parameter resolves through
// the same scalar-subquery path histogram_quantile's own phi uses.
func TestHistogramQuantile_QuantileAgg_ComputedPhi(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})
	const query = `histogram_quantile(0.5, quantile by (le) (scalar(cpu_ratio), rate(http_server_request_duration_bucket[5m])))`

	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower(%q): %v", query, err)
	}
	if shape := inspectHistogramPlan(plan); !shape.hasHistogramQuantile {
		t.Fatalf("plan for %q has no HistogramQuantile node — a computed parameter must not route the query to an empty result", query)
	}

	// The parameter must actually reach the fold: resolve the same
	// aggregation the query carries and assert the folded rung reads the
	// scalar subquery rather than a silently substituted constant.
	agg, ok := peelWrappers(expr.(*parser.Call).Args[1]).(*parser.AggregateExpr)
	if !ok {
		t.Fatalf("test setup: %q's inner expression is not an aggregation", query)
	}
	fold, err := classicBucketLadderFold(agg, s, lowerCtx{})
	if err != nil {
		t.Fatalf("classicBucketLadderFold(quantile with computed param): %v", err)
	}
	out, readsRungs := foldRungs(t, fold)
	if !readsRungs {
		t.Fatal("computed-parameter quantile fold does not read its rung array")
	}
	found := false
	chplan.InspectExpr(out, func(x chplan.Expr) bool {
		if _, ok := x.(*chplan.ScalarSubquery); ok {
			found = true
		}
		return true
	})
	if !found {
		t.Fatalf("computed-parameter quantile fold carries no ScalarSubquery; the parameter was not threaded into the interpolation\nfold: %#v", out)
	}
}
