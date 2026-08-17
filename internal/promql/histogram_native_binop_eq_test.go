package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_ExpHistogram_HistogramCompareBinopIsHistogramValued pins
// cerberus issue #2273 (gap 2): `<exp-hist shape> (==|!=) <exp-hist
// shape>` with default matching and no `bool` modifier lowers to a
// chplan.HistogramProjection, same as `+`/`-` (#2263) — but, unlike
// the arithmetic merge, the metric-name slot is NOT an empty literal:
// reference's resultMetric keeps LHS's own full label set (including
// __name__) for a non-bool comparison, so the projection must read it
// off a real column rather than hard-coding "".
func TestLower_ExpHistogram_HistogramCompareBinopIsHistogramValued(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	cases := []struct {
		name          string
		query         string
		lower         func(parser.Expr) (chplan.Node, error)
		wantEmptyName bool // true only when an outer aggregation legitimately empties __name__ itself
	}{
		{name: "equality, two bare selectors, same metric", query: `latency_exp_hist == latency_exp_hist`},
		{name: "equality, two bare selectors, different metrics", query: `latency_exp_hist == other_exp_hist`},
		{name: "inequality", query: `latency_exp_hist != other_exp_hist`},
		{name: "sum() plus bare selector", query: `sum(latency_exp_hist) == other_exp_hist`},
		{name: "parenthesised", query: `(latency_exp_hist == other_exp_hist)`},
		// sum() ALWAYS empties __name__ regardless of its argument (see
		// aggregatedHistogramProjection) — this is sum()'s own rule, not
		// the compare-binop's, so it is the one case that expects the
		// empty literal rather than a preserved LHS column.
		{name: "composes with sum()", query: `sum(latency_exp_hist == other_exp_hist)`, wantEmptyName: true},
		{
			name:  "range query",
			query: `latency_exp_hist == other_exp_hist`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, 30*time.Second)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			lower := tc.lower
			if lower == nil {
				lower = func(e parser.Expr) (chplan.Node, error) {
					return promql.LowerAt(context.Background(), e, s, end, end)
				}
			}
			plan, err := lower(expr)
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
			_, isLit := hp.GroupBy[0].(*chplan.LitString)
			if isLit != tc.wantEmptyName {
				t.Fatalf("lower(%q): metric-name projection is literal=%v, want literal=%v", tc.query, isLit, tc.wantEmptyName)
			}
		})
	}
}

// TestLower_ExpHistogram_HistogramCompareBinopSupportsGroupLeftRight pins
// that group_left()/group_right() (many-to-one broadcast) lowers
// successfully for `==`/`!=` between two histograms too — cerberus issue
// #2328, the same gap [TestLower_ExpHistogram_HistogramBinopSupportsGroupLeftRight]
// pins for `+`/`-`. The bool-modifier variant answers a FLOAT-valued
// result (see [expHistogramHistogramCompareBoolBinop]'s doc), so it's
// asserted separately from the non-bool structural-filter cases.
func TestLower_ExpHistogram_HistogramCompareBinopSupportsGroupLeftRight(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	queries := []string{
		`latency_exp_hist != on(service) group_left() other_exp_hist`,
		`latency_exp_hist != on(service) group_right() other_exp_hist`,
	}

	for _, query := range queries {
		t.Run(query, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
			if err != nil {
				t.Fatalf("lower(%q): unexpected error: %v", query, err)
			}
			hp, ok := plan.(*chplan.HistogramProjection)
			if !ok {
				t.Fatalf("lower(%q): plan root is %T, want *chplan.HistogramProjection", query, plan)
			}
			findHistogramVectorJoin(t, hp)
		})
	}
}

// TestLower_ExpHistogram_HistogramCompareBoolBinopSupportsGroupLeftRight
// pins the bool-modifier variant of the same gap: `== bool` between two
// histograms under group_left()/group_right() lowers to a FLOAT-valued
// plan (not *chplan.HistogramProjection — see
// [expHistogramHistogramCompareBoolBinop]'s doc) whose tree still
// contains the chplan.HistogramVectorJoin broadcast.
func TestLower_ExpHistogram_HistogramCompareBoolBinopSupportsGroupLeftRight(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	query := `latency_exp_hist == bool on(service) group_left() other_exp_hist`
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
	if err != nil {
		t.Fatalf("lower(%q): unexpected error: %v", query, err)
	}
	if _, ok := plan.(*chplan.HistogramProjection); ok {
		t.Fatalf("lower(%q): plan root is *chplan.HistogramProjection, want a FLOAT-valued shape (bool modifier)", query)
	}
	findHistogramVectorJoin(t, plan)
}

// TestLower_ExpHistogram_HistogramCompareBinopSupportsOnIgnoringMatching
// pins cerberus issue #2273's on()/ignoring() gap being closed for
// `==`/`!=` too: the query lowers successfully to the same
// histogram-valued contract the default-matching case publishes.
func TestLower_ExpHistogram_HistogramCompareBinopSupportsOnIgnoringMatching(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	queries := []string{
		`latency_exp_hist == on(service) other_exp_hist`,
		`latency_exp_hist == ignoring(service) other_exp_hist`,
		`latency_exp_hist != on(service) other_exp_hist`,
	}

	for _, query := range queries {
		t.Run(query, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
			if err != nil {
				t.Fatalf("lower(%q): unexpected error: %v", query, err)
			}
			if shape := chplan.RowShapeOf(plan); shape != chplan.HistogramRowShape {
				t.Fatalf("lower(%q): plan root publishes %s, want histogram", query, shape)
			}
			if _, ok := plan.(*chplan.HistogramProjection); !ok {
				t.Fatalf("lower(%q): plan root is %T, want *chplan.HistogramProjection", query, plan)
			}
		})
	}
}

// TestLower_ExpHistogram_HistogramCompareBoolBinopIsFloatValued pins
// cerberus issue #2273's `bool`-modifier gap being closed: reference
// answers `hist1 == bool hist2` FLOAT-valued (every matched pair emits
// 1.0/0.0, discarding the histogram payload entirely) — [lowerVectorVector]
// (binary.go) dispatches to [lowerExpHistogramHistogramCompareBoolBinop],
// which builds a plain 4-column MetricName/Attributes/Timestamp/Value
// *chplan.Project rather than a *chplan.HistogramProjection.
func TestLower_ExpHistogram_HistogramCompareBoolBinopIsFloatValued(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	queries := []string{
		`latency_exp_hist == bool other_exp_hist`,
		`latency_exp_hist != bool other_exp_hist`,
		`latency_exp_hist == bool on(service) other_exp_hist`,
		`(latency_exp_hist == bool other_exp_hist) + 1`,
	}

	for _, query := range queries {
		t.Run(query, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
			if err != nil {
				t.Fatalf("lower(%q): unexpected error: %v", query, err)
			}
			if _, ok := plan.(*chplan.HistogramProjection); ok {
				t.Fatalf("lower(%q): plan root is *chplan.HistogramProjection, want a plain float Project", query)
			}
		})
	}
}

// TestLower_ExpHistogram_HistogramCompareBinopBuildsUnionAllAndCountGuard
// pins the plan SHAPE the join builds: a chplan.UnionAll of the two
// operands feeding a chplan.Aggregate grouped by Attributes with a
// Having guard that ANDs the shared `count() = 2` match guard
// (histogram_native_binop.go's [histogramBinopBothSidesMatchedGuard])
// with the field-by-field structural comparison — AND-combined for
// `==`, OR-combined (De Morgan) for `!=`.
func TestLower_ExpHistogram_HistogramCompareBinopBuildsUnionAllAndCountGuard(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name       string
		query      string
		fieldCmpOp chplan.BinaryOp
		combineOp  chplan.BinaryOp
	}{
		{name: "equality", query: `latency_exp_hist == other_exp_hist`, fieldCmpOp: chplan.OpEq, combineOp: chplan.OpAnd},
		{name: "inequality", query: `latency_exp_hist != other_exp_hist`, fieldCmpOp: chplan.OpNe, combineOp: chplan.OpOr},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
			if err != nil {
				t.Fatalf("LowerAt(%q): %v", tc.query, err)
			}
			hp, ok := plan.(*chplan.HistogramProjection)
			if !ok {
				t.Fatalf("lower(%q): plan root is %T, want *chplan.HistogramProjection", tc.query, plan)
			}
			reshape, ok := hp.Input.(*chplan.Project)
			if !ok {
				t.Fatalf("lower(%q): HistogramProjection.Input is %T, want *chplan.Project", tc.query, hp.Input)
			}
			agg, ok := reshape.Input.(*chplan.Aggregate)
			if !ok {
				t.Fatalf("lower(%q): reshape.Input is %T, want *chplan.Aggregate", tc.query, reshape.Input)
			}
			if agg.Having == nil {
				t.Fatalf("lower(%q): Aggregate.Having is nil, want the count() = 2 + field-equality guard", tc.query)
			}
			having, ok := agg.Having.(*chplan.Binary)
			if !ok || having.Op != chplan.OpAnd {
				t.Fatalf("lower(%q): Aggregate.Having = %#v, want a top-level AND Binary", tc.query, agg.Having)
			}
			matchGuard, ok := having.Left.(*chplan.Binary)
			if !ok || matchGuard.Op != chplan.OpEq {
				t.Fatalf("lower(%q): Aggregate.Having.Left = %#v, want the count() = 2 guard", tc.query, having.Left)
			}
			if _, ok := matchGuard.Left.(*chplan.FuncCall); !ok {
				t.Fatalf("lower(%q): Aggregate.Having.Left.Left = %#v, want a count() FuncCall", tc.query, matchGuard.Left)
			}

			fieldCmp := having.Right
			// The field comparison chain nests as a left-leaning tree of
			// tc.combineOp Binary nodes bottoming out in tc.fieldCmpOp
			// leaves; walk down the rightmost leaf to check the op without
			// pinning the exact field count/order.
			for {
				b, ok := fieldCmp.(*chplan.Binary)
				if !ok {
					t.Fatalf("lower(%q): Having.Right chain node = %#v, want a Binary", tc.query, fieldCmp)
				}
				if b.Op == tc.fieldCmpOp {
					break
				}
				if b.Op != tc.combineOp {
					t.Fatalf("lower(%q): Having.Right chain op = %s, want %s or %s", tc.query, b.Op, tc.combineOp, tc.fieldCmpOp)
				}
				fieldCmp = b.Right
			}

			union, ok := agg.Input.(*chplan.UnionAll)
			if !ok || len(union.Inputs) != 2 {
				t.Fatalf("lower(%q): Aggregate.Input = %#v, want a two-arm UnionAll", tc.query, agg.Input)
			}
			for i, arm := range union.Inputs {
				if _, ok := arm.(*chplan.Project); !ok {
					t.Fatalf("lower(%q): UnionAll.Inputs[%d] is %T, want *chplan.Project (the side-tagged wrap)", tc.query, i, arm)
				}
			}
		})
	}
}
