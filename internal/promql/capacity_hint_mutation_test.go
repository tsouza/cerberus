// Capacity-hint adjudication for internal/promql.
//
// Every hint in this file sizes a slice that becomes an EXPORTED field of a
// value its builder returns — a `chplan.Project`'s `Projections` or a
// `chplan.FuncCall`'s `Args` — and none of them grows past its
// pre-allocation, so `cap` reads the hint's arithmetic straight back and a
// plain assertion kills the `ARITHMETIC_BASE` mutant gremlins emits on it.
// `docs/test-strategy.md`'s "When a capacity mutant is equivalent" states the
// rule; `internal/logql/detected_level_test.go` carries the worked instances
// it was derived from.
//
// Each test names the escape surface it reads the capacity back through.
// [capmutant.AssertKilled] then replays the substitution against the builder's
// own append sequence and requires it to move the finished capacity, so "the
// assertion discriminates" is re-run rather than asserted.
package promql

import (
	"fmt"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/capmutant"
)

// capHintProjections reads the `Projections` slice of the [chplan.Project] n
// is, failing the test when the node is some other shape — a navigation that
// silently answered an empty slice would report a capacity nobody allocated.
func capHintProjections(t *testing.T, n chplan.Node, what string) []chplan.Projection {
	t.Helper()

	p, ok := n.(*chplan.Project)
	if !ok {
		t.Fatalf("%s is %T; want *chplan.Project", what, n)
	}
	return p.Projections
}

// capHintMapArgs reads the `Args` of the `map(...)` FuncCall wrapped in the
// [chplan.MapWithoutEmptyValues] e is.
func capHintMapArgs(t *testing.T, e chplan.Expr, what string) []chplan.Expr {
	t.Helper()

	w, ok := e.(*chplan.MapWithoutEmptyValues)
	if !ok {
		t.Fatalf("%s is %T; want *chplan.MapWithoutEmptyValues", what, e)
	}
	fn, ok := w.Map.(*chplan.FuncCall)
	if !ok {
		t.Fatalf("%s wraps %T; want the map(...) *chplan.FuncCall", what, w.Map)
	}
	return fn.Args
}

// capHintProjectionExpr answers the expression the projection list projects
// under alias.
func capHintProjectionExpr(t *testing.T, projs []chplan.Projection, alias string) chplan.Expr {
	t.Helper()

	for _, p := range projs {
		if p.Alias == alias {
			return p.Expr
		}
	}
	t.Fatalf("no projection aliased %q in %d projections", alias, len(projs))
	return nil
}

// capHintAliases builds n distinct column names for a grouping key list.
func capHintAliases(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("cap_key_%d", i))
	}
	return out
}

// TestExpHistogramPairCountStage_CapHintMutantKilled kills the ARITHMETIC_BASE
// mutant gremlins reports on histogram_native_resets.go:`len(keyAliases)+1`.
//
// Escape surface: the slice IS the returned [chplan.Project]'s exported
// `Projections`, after the pair-count projection is appended onto it in the
// struct literal.
func TestExpHistogramPairCountStage_CapHintMutantKilled(t *testing.T) {
	t.Parallel()

	const keys = 5

	s := schema.DefaultOTelMetrics()

	capmutant.AssertKilled(t, capmutant.Hint{
		Construct: "histogram_native_resets.go:`len(keyAliases)+1`",
		Positions: []capmutant.Position{{Name: "the `+1`", Op: "+"}},
		Eval: func(t testing.TB, ops []string) (int, bool) {
			return capmutant.Eval(t, []int{keys, 1}, ops)
		},
		Observe: func(t *testing.T) (int, int) {
			node := expHistogramPairCountStage(&chplan.OneRow{}, rateWindowFn, capHintAliases(keys), s)
			projs := capHintProjections(t, node, "expHistogramPairCountStage result")
			return len(projs), cap(projs)
		},
		Build: func(hint int) (int, int) {
			projs := make([]chplan.Projection, 0, hint)
			for i := 0; i < keys; i++ {
				projs = append(projs, chplan.Projection{})
			}
			projs = append(projs, chplan.Projection{}) // the pair-count value
			return len(projs), cap(projs)
		},
	})
}

// TestScaleHistogramProjection_CapHintMutantsKilled kills the two
// ARITHMETIC_BASE mutants gremlins reports on
// histogram_native_scalar_binop.go:`len(passthroughCols)+len(scalarCols)+len(ladderCols)`.
//
// Escape surface: the slice becomes the `Projections` of the [chplan.Project]
// the returned [chplan.HistogramProjection] wraps as its `Input`.
//
// The three operand lists are fixed by the schema rather than by this test, so
// their sizes are read back from the builder's own inputs instead of written
// down: a schema whose histogram columns changed shape would otherwise silently
// adjudicate a hint with different operands.
func TestScaleHistogramProjection_CapHintMutantsKilled(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	histSchema := histogramProjectionSchema(s)
	passthrough := len([]string{
		s.AttributesColumn,
		s.TimestampColumn,
		histSchema.ScaleColumn,
		histSchema.ZeroThresholdColumn,
		histSchema.PositiveOffsetColumn,
		histSchema.NegativeOffsetColumn,
	})
	scalars := len([]string{histSchema.CountColumn, histSchema.SumColumn, histSchema.ZeroCountColumn})
	ladders := len([]string{histSchema.PositiveBucketCountsColumn, histSchema.NegativeBucketCountsColumn})

	capmutant.AssertKilled(t, capmutant.Hint{
		Construct: "histogram_native_scalar_binop.go:`len(passthroughCols)+len(scalarCols)+len(ladderCols)`",
		Positions: []capmutant.Position{
			{Name: "the `+len(scalarCols)`", Op: "+"},
			{Name: "the `+len(ladderCols)`", Op: "+"},
		},
		Eval: func(t testing.TB, ops []string) (int, bool) {
			return capmutant.Eval(t, []int{passthrough, scalars, ladders}, ops)
		},
		Observe: func(t *testing.T) (int, int) {
			hp := scaleHistogramProjection(&chplan.OneRow{}, chplan.OpMul, &chplan.LitFloat{V: 2}, s)
			projs := capHintProjections(t, hp.Input, "scaleHistogramProjection's inner node")
			return len(projs), cap(projs)
		},
		Build: func(hint int) (int, int) {
			projs := make([]chplan.Projection, 0, hint)
			for i := 0; i < passthrough+scalars+ladders; i++ {
				projs = append(projs, chplan.Projection{})
			}
			return len(projs), cap(projs)
		},
	})
}

// TestExpHistogramWindowFactorStage_CapHintMutantsKilled kills the three
// ARITHMETIC_BASE mutants gremlins reports on
// histogram_quantile_native_window.go:`len(keyAliases)+len(aggs)+len(extraAliases)+1`.
//
// Escape surface: the slice IS the returned [chplan.Project]'s exported
// `Projections`.
//
// The stage only builds the slice on its hoistable branch, which needs a
// `rate`/`increase` window with a series-wide count series — see
// [histogramWindowInvariantFactorExpr] — so the inputs below set exactly that.
func TestExpHistogramWindowFactorStage_CapHintMutantsKilled(t *testing.T) {
	t.Parallel()

	const (
		keys   = 4
		aggs   = 3
		extras = 2
	)

	in := histogramWindowInputs{
		rangeStart:  &chplan.LitInt{V: 0},
		rangeEnd:    &chplan.LitInt{V: 1},
		countValues: &chplan.ColumnRef{Name: "counts"},
	}
	aggFuncs := make([]chplan.AggFunc, 0, aggs)
	for i := 0; i < aggs; i++ {
		aggFuncs = append(aggFuncs, chplan.AggFunc{Fn: chplan.FnCount, Alias: fmt.Sprintf("agg_%d", i)})
	}

	capmutant.AssertKilled(t, capmutant.Hint{
		Construct: "histogram_quantile_native_window.go:`len(keyAliases)+len(aggs)+len(extraAliases)+1`",
		Positions: []capmutant.Position{
			{Name: "the `+len(aggs)`", Op: "+"},
			{Name: "the `+len(extraAliases)`", Op: "+"},
			{Name: "the trailing `+1`", Op: "+"},
		},
		Eval: func(t testing.TB, ops []string) (int, bool) {
			return capmutant.Eval(t, []int{keys, aggs, extras, 1}, ops)
		},
		Observe: func(t *testing.T) (int, int) {
			node, _ := expHistogramWindowFactorStage(
				&chplan.OneRow{}, aggFuncs, capHintAliases(keys), capHintAliases(extras),
				rateWindowFn, in, histogramWindowFold(rateWindowFn, in),
			)
			projs := capHintProjections(t, node, "expHistogramWindowFactorStage result")
			return len(projs), cap(projs)
		},
		Build: func(hint int) (int, int) {
			projs := make([]chplan.Projection, 0, hint)
			for i := 0; i < keys+aggs+extras; i++ {
				projs = append(projs, chplan.Projection{})
			}
			projs = append(projs, chplan.Projection{}) // the hoisted factor
			return len(projs), cap(projs)
		},
	})
}

// TestExpHistogramWindowReshape_CapHintMutantsKilled kills the ARITHMETIC_BASE
// mutants gremlins reports on
// histogram_quantile_native_window.go:`len(keyAliases)+len(scalars)+7`.
//
// Escape surface: the slice IS the returned [chplan.Project]'s exported
// `Projections`, after the four merge/bucket projections are appended onto it.
//
// The `7` covers a projection the schema's optional zero-threshold column
// gates, so on a schema that leaves that column unset the builder appends one
// slot fewer than it reserved. Capacity is still the hint either way — an
// under-filled slice never grows, so it keeps the capacity it was made with —
// which is why the adjudication turns on the finished capacity rather than on
// the finished length.
func TestExpHistogramWindowReshape_CapHintMutantsKilled(t *testing.T) {
	t.Parallel()

	const (
		keys    = 4
		scalars = 3
		// The projections the reshape appends beyond the keys and the
		// scalars: the merged scale and the zero-count fold, then the two
		// signed offset/bucket pairs in the closing append.
		fixedProjections = 6
	)

	s := schema.DefaultOTelMetrics()
	in := histogramWindowInputs{
		rangeStart:  &chplan.LitInt{V: 0},
		rangeEnd:    &chplan.LitInt{V: 1},
		countValues: &chplan.ColumnRef{Name: "counts"},
	}
	scalarProjs := make([]chplan.Projection, 0, scalars)
	for i := 0; i < scalars; i++ {
		scalarProjs = append(scalarProjs, chplan.Projection{
			Expr:  &chplan.ColumnRef{Name: fmt.Sprintf("scalar_%d", i)},
			Alias: fmt.Sprintf("scalar_%d", i),
		})
	}

	if s.ZeroThresholdColumn != "" {
		t.Fatalf("the default metrics schema now configures a zero-threshold column (%q), "+
			"so the reshape appends one projection more than this test's fixedProjections "+
			"assumes", s.ZeroThresholdColumn)
	}

	capmutant.AssertKilled(t, capmutant.Hint{
		Construct: "histogram_quantile_native_window.go:`len(keyAliases)+len(scalars)+7`",
		Positions: []capmutant.Position{
			{Name: "the `+len(scalars)`", Op: "+"},
			{Name: "the trailing `+7`", Op: "+"},
		},
		Eval: func(t testing.TB, ops []string) (int, bool) {
			return capmutant.Eval(t, []int{keys, scalars, 7}, ops)
		},
		Observe: func(t *testing.T) (int, int) {
			node := expHistogramWindowReshape(
				&chplan.OneRow{}, nil, capHintAliases(keys), nil,
				histogramWindowFold(rateWindowFn, in), rateWindowFn, in, scalarProjs, s,
			)
			projs := capHintProjections(t, node, "expHistogramWindowReshape result")
			return len(projs), cap(projs)
		},
		Build: func(hint int) (int, int) {
			projs := make([]chplan.Projection, 0, hint)
			for i := 0; i < keys; i++ {
				projs = append(projs, chplan.Projection{})
			}
			projs = append(projs, scalarProjs...)
			projs = append(projs, chplan.Projection{}, chplan.Projection{})
			projs = append(projs, make([]chplan.Projection, fixedProjections-2)...)
			return len(projs), cap(projs)
		},
	})
}

// TestClassicBucketReshape_CapHintMutantKilled kills the ARITHMETIC_BASE
// mutant gremlins reports on
// histogram_quantile.go:`projections := make([]chplan.Projection, 0, len(passthrough)+2)`.
//
// The citation names the whole assignment rather than the hint alone because
// the sibling `merged := …` a few lines below spells an identical hint, and a
// citation that resolved to both would register this verdict on a mutant it is
// not adjudicating.
//
// Escape surface: the slice IS the returned [chplan.Project]'s exported
// `Projections`.
func TestClassicBucketReshape_CapHintMutantKilled(t *testing.T) {
	t.Parallel()

	const passthrough = 5

	s := schema.DefaultOTelMetrics()
	pass := make([]chplan.Projection, 0, passthrough)
	for i := 0; i < passthrough; i++ {
		pass = append(pass, chplan.Projection{
			Expr:  &chplan.ColumnRef{Name: fmt.Sprintf("pass_%d", i)},
			Alias: fmt.Sprintf("pass_%d", i),
		})
	}

	capmutant.AssertKilled(t, capmutant.Hint{
		Construct: "histogram_quantile.go:`projections := make([]chplan.Projection, 0, len(passthrough)+2)`",
		Positions: []capmutant.Position{{Name: "the `+2`", Op: "+"}},
		Eval: func(t testing.TB, ops []string) (int, bool) {
			return capmutant.Eval(t, []int{passthrough, 2}, ops)
		},
		Observe: func(t *testing.T) (int, int) {
			// A nil fold selects the branch this hint sizes — the argMax
			// newest-row path, which has no layouts to merge.
			node, _ := classicBucketShaping{}.reshape(&chplan.OneRow{}, pass, s)
			projs := capHintProjections(t, node, "classicBucketShaping.reshape result")
			return len(projs), cap(projs)
		},
		Build: func(hint int) (int, int) {
			projs := make([]chplan.Projection, 0, hint)
			projs = append(projs, pass...)
			projs = append(projs, chplan.Projection{}, chplan.Projection{})
			return len(projs), cap(projs)
		},
	})
}

// TestHistogramAggGroupBy_CapHintMutantKilled kills the ARITHMETIC_BASE mutant
// gremlins reports on histogram_quantile.go:`len(labels)*2`.
//
// Escape surface: the slice is the `map(...)` FuncCall's exported `Args` inside
// the [chplan.MapWithoutEmptyValues] the builder returns as its third result.
func TestHistogramAggGroupBy_CapHintMutantKilled(t *testing.T) {
	t.Parallel()

	const (
		query  = `sum by (job, instance, region, zone, shard) (latency_bucket)`
		labels = 5
	)

	s := schema.DefaultOTelMetrics()
	agg := mustAggregateExpr(t, query, labels)

	capmutant.AssertKilled(t, capmutant.Hint{
		Construct: "histogram_quantile.go:`len(labels)*2`",
		Positions: []capmutant.Position{{Name: "the `*2`", Op: "*"}},
		Eval: func(t testing.TB, ops []string) (int, bool) {
			return capmutant.Eval(t, []int{labels, 2}, ops)
		},
		Observe: func(t *testing.T) (int, int) {
			_, _, attrs := histogramAggGroupBy(agg, &chplan.ColumnRef{Name: s.AttributesColumn}, s)
			args := capHintMapArgs(t, attrs, "histogramAggGroupBy's Attributes map")
			return len(args), cap(args)
		},
		Build: func(hint int) (int, int) {
			args := make([]chplan.Expr, 0, hint)
			for i := 0; i < labels; i++ {
				args = append(args, nil, nil) // (label literal, gkey column)
			}
			return len(args), cap(args)
		},
	})
}

// TestLowerCountValuesOverPlan_CapHintMutantsKilled kills the two
// ARITHMETIC_BASE mutants gremlins reports on
// lower.go:`(len(a.Grouping)+1)*2`.
//
// Escape surface: the slice is the `map(...)` FuncCall's exported `Args`,
// inside the [chplan.MapWithoutEmptyValues] the returned [chplan.Project]
// projects as its Attributes column.
func TestLowerCountValuesOverPlan_CapHintMutantsKilled(t *testing.T) {
	t.Parallel()

	const (
		query  = `count_values by (job, instance, region, zone) ("v", up)`
		groups = 4
	)

	s := schema.DefaultOTelMetrics()
	agg := mustAggregateExpr(t, query, groups)

	capmutant.AssertKilled(t, capmutant.Hint{
		Construct: "lower.go:`(len(a.Grouping)+1)*2`",
		Positions: []capmutant.Position{
			{Name: "the inner `+1`", Op: "+"},
			{Name: "the `*2`", Op: "*"},
		},
		Eval: func(t testing.TB, ops []string) (int, bool) {
			// The parentheses fix a grouping no substitution can move, so the
			// inner sum is evaluated on its own and fed to the outer factor.
			inner, ok := capmutant.Eval(t, []int{groups, 1}, ops[:1])
			if !ok {
				return 0, false
			}
			return capmutant.Eval(t, []int{inner, 2}, ops[1:])
		},
		Observe: func(t *testing.T) (int, int) {
			node := lowerCountValuesOverPlan(agg, "v", &chplan.OneRow{},
				&chplan.ColumnRef{Name: s.ValueColumn}, s, lowerCtx{})
			projs := capHintProjections(t, node, "lowerCountValuesOverPlan result")
			attrs := capHintProjectionExpr(t, projs, s.AttributesColumn)
			args := capHintMapArgs(t, attrs, "the count_values Attributes map")
			return len(args), cap(args)
		},
		Build: func(hint int) (int, int) {
			args := make([]chplan.Expr, 0, hint)
			for i := 0; i < groups; i++ {
				args = append(args, nil, nil) // (label literal, gkey column)
			}
			args = append(args, nil, nil) // the synthetic value-as-label pair
			return len(args), cap(args)
		},
	})
}

// TestPromAggregateAttributesExpr_CapHintMutantKilled kills the
// ARITHMETIC_BASE mutant gremlins reports on lower.go:`len(a.Grouping)*2`.
//
// Escape surface: the slice is the `map(...)` FuncCall's exported `Args` inside
// the [chplan.MapWithoutEmptyValues] the builder returns.
func TestPromAggregateAttributesExpr_CapHintMutantKilled(t *testing.T) {
	t.Parallel()

	const (
		query  = `sum by (job, instance, region, zone, shard) (up)`
		groups = 5
	)

	agg := mustAggregateExpr(t, query, groups)

	capmutant.AssertKilled(t, capmutant.Hint{
		Construct: "lower.go:`len(a.Grouping)*2`",
		Positions: []capmutant.Position{{Name: "the `*2`", Op: "*"}},
		Eval: func(t testing.TB, ops []string) (int, bool) {
			return capmutant.Eval(t, []int{groups, 2}, ops)
		},
		Observe: func(t *testing.T) (int, int) {
			attrs := promAggregateAttributesExpr(agg, capHintAliases(groups))
			args := capHintMapArgs(t, attrs, "promAggregateAttributesExpr result")
			return len(args), cap(args)
		},
		Build: func(hint int) (int, int) {
			args := make([]chplan.Expr, 0, hint)
			for i := 0; i < groups; i++ {
				args = append(args, nil, nil) // (label literal, alias column)
			}
			return len(args), cap(args)
		},
	})
}

// mustAggregateExpr parses query and asserts it groups on exactly `groups`
// labels, so a fixture edit that changed the grouping is reported instead of
// quietly adjudicating a hint with different operands.
func mustAggregateExpr(t *testing.T, query string, groups int) *parser.AggregateExpr {
	t.Helper()

	expr := mustParse(t, query)
	agg, ok := expr.(*parser.AggregateExpr)
	if !ok {
		t.Fatalf("parsing %q gave %T; want *parser.AggregateExpr", query, expr)
	}
	if len(agg.Grouping) != groups {
		t.Fatalf("fixture must group on exactly %d labels; got %v", groups, agg.Grouping)
	}
	return agg
}
