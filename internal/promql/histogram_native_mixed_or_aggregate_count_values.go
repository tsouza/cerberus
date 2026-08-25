package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_mixed_or_aggregate_count_values.go lowers
// `count_values("label", ...)` [by/without] wrapping a mixed
// float/histogram `or` — cerberus issue #2595, the third and last of the
// sum/avg-wrapped composition's (histogram_native_mixed_or_aggregate.go,
// cerberus issue #2346) deliberately-unattempted sibling aggregation ops.
//
// Unlike every other aggregation in this family, `count_values` is
// VALUE-aware even for a native-histogram sample — reference Prometheus's
// aggregationCountValues stringifies EVERY sample (float or histogram)
// into the synthetic label's value rather than dropping either type:
//
//	if s.H == nil {
//	    enh.lb.Set(valueLabel, strconv.FormatFloat(s.F, 'f', -1, 64))
//	} else {
//	    enh.lb.Set(valueLabel, s.H.String())
//	}
//
// Cerberus already has both halves of this for the PURE (non-mixed)
// cases: [lowerCountValues] (lower.go) stringifies an ordinary float
// Value via `toString(Value)`, and [nativeHistogramStringExpr]
// (histogram_native_count_values.go, cerberus issue #2470) reproduces
// Go's `FloatHistogram.String()` layout byte-for-byte from the
// thirteen-column histogram row contract. Composing count_values() over a
// mixed `or` needs no new stringification machinery — only recombining
// the two existing consumers correctly.
//
// A histogram's stringified value can never collide with a float's:
// [nativeHistogramStringExpr] always opens with `{`
// (`histogramOpenBraceCode`), which `strconv.FormatFloat`'s output space
// (digits, `.`, `-`, `e`, `+`, `Inf`, `NaN`) never produces. So the two
// arms' (partition-key, stringified-value) pairs are DISJOINT by
// construction, at every group — there is no analogue of SUM/AVG's
// "group present on both branches" collision (histogram_native_mixed_or_
// aggregate.go's [combineMixedAggregateBranches]) to guard against here,
// and no analogue of PR #2592 / cerberus issue #2581's grouping-collision
// hazard either (that hazard is specific to a `by(...)` grouping key
// alone coinciding across arms; here the OUTPUT partition key includes
// the never-colliding value label, so two rows from different arms are
// never the same output group to begin with). The two independently
// count_values()-reduced branches can therefore simply be unioned:
//
//  1. [shadowResolveMixedExpHistogramOperands] (histogram_native_mixed_or_
//     aggregate.go) lowers the `or`'s two operands and resolves the
//     shadow rule for both, exactly as every sibling in this family does.
//  2. Each shadow-resolved arm is reduced independently by
//     [lowerCountValuesOverPlan] (lower.go) — the SAME shared grouping/
//     projection logic [lowerCountValues] already uses for an ordinary
//     float aggregand and [lowerExpHistogramCountValuesOverPlan]
//     (histogram_native_count_values.go) already uses for a PURE
//     native-histogram aggregand — with [nativeHistogramStringExpr] as
//     the histogram arm's value-key and the ordinary `toString(Value)`
//     as the float arm's.
//  3. The two branches are combined with a plain (non-Mixed,
//     non-Histogram) [chplan.VectorSetOp] OR — matching on the FULL
//     reconstructed Attributes (an empty [chplan.VectorMatch], mirroring
//     [combineMixedAggregateBranches]'s identical choice), never the
//     `or`'s own on()/ignoring() clause, which governed shadow resolution
//     one stage earlier. Both branches already publish ordinary
//     float-valued Sample rows (count_values's own output shape, never
//     histogram-valued), so the plain union's shadow-priority test is a
//     structural no-op given the arms are disjoint by construction (see
//     above) rather than something this step needs to reason about.
func countValuesOverMixedExpHistogramSetOp(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (*parser.AggregateExpr, *parser.BinaryExpr, bool) {
	agg, ok := unwrapAggregateExpr(expr)
	if !ok || agg.Op != parser.COUNT_VALUES {
		return nil, nil, false
	}
	b, ok := mixedExpHistogramSetOp(agg.Expr, s, ctx)
	if !ok {
		return nil, nil, false
	}
	return agg, b, true
}

// lowerCountValuesOverMixedExpHistogramSetOp lowers the shape
// [countValuesOverMixedExpHistogramSetOp] recognised. See this file's
// header for the three-stage reduction.
func lowerCountValuesOverMixedExpHistogramSetOp(agg *parser.AggregateExpr, b *parser.BinaryExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if b.ReturnBool {
		return nil, fmt.Errorf("promql: 'bool' modifier is only allowed on comparison binary ops")
	}
	label, ok := tryStringLiteral(agg.Param)
	if !ok {
		return nil, fmt.Errorf("promql: count_values requires a string-literal label name as the first arg")
	}
	if label == "" {
		return nil, fmt.Errorf("promql: count_values requires a non-empty label name")
	}

	histForAgg, floatForAgg, err := shadowResolveMixedExpHistogramOperands(b, s, ctx)
	if err != nil {
		return nil, err
	}

	histBranch := lowerCountValuesOverPlan(agg, label, histForAgg, nativeHistogramStringExpr(s), s, ctx)
	floatBranch := lowerCountValuesOverPlan(agg, label, floatForAgg, &chplan.FuncCall{
		Fn:   chplan.FnToString,
		Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ValueColumn}},
	}, s, ctx)

	return combineMixedCountValuesBranches(histBranch, floatBranch, s, ctx), nil
}

// combineMixedCountValuesBranches unions the two independently
// count_values()-reduced branches. See this file's header for why a
// plain OR — not [combineMixedAggregateBranches]'s drop-on-collision
// dance — is the correct combinator here: the two branches' (partition,
// stringified-value) output keys are disjoint by construction, so there
// is no collision to drop.
func combineMixedCountValuesBranches(histBranch, floatBranch chplan.Node, s schema.Metrics, ctx lowerCtx) chplan.Node {
	return &chplan.VectorSetOp{
		Left:             histBranch,
		Right:            floatBranch,
		Op:               chplan.VectorSetOr,
		Match:            chplan.VectorMatch{},
		StepAligned:      ctx.step > 0,
		MetricNameColumn: s.MetricNameColumn,
		AttributesColumn: s.AttributesColumn,
		TimestampColumn:  s.TimestampColumn,
		ValueColumn:      s.ValueColumn,
	}
}
