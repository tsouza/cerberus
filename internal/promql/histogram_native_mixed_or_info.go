package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_mixed_or_info.go composes `info()` over a mixed
// float/histogram `or` BASE argument (histogram_native_mixed_or.go's
// #2330 shape) — cerberus issue #2618, info()'s own sibling of
// histogram_native_mixed_or_value_fn.go's identical composition for the
// native-histogram value functions.
//
// Reference Prometheus's evalInfo (promql/info.go) never inspects a base
// sample's value type at all: addToSeries copies BOTH `sample.F` and
// `sample.H` through unchanged onto the enriched output (cerberus issue
// #2509 already established this for a wholly histogram-valued base) — so
// a MIXED base vector's result must stay mixed too, unlike the value
// functions (histogram_native_mixed_or_value_fn.go), which skip every
// non-histogram row instead. This file lowers each of
// [splitMixedExpHistogramSetOpByType]'s two partitions through info()'s
// OWN existing enrichment pipeline independently
// ([lowerInfoNonStaticBase], factored out of [lowerInfo] for exactly this
// reuse) and recombines the two results with the same [chplan.VectorSetOp]
// Mixed machinery [lowerMixedExpHistogramSetOp] itself uses to build the
// argument in the first place.
//
// That recombination's own shadow-resolution is provably a no-op, not
// merely assumed harmless: the two partitions are disjoint on their full
// label set BEFORE enrichment (that is exactly what the argument's own
// `or` union already guarantees — a row lands in one partition or the
// other, never both), and info()'s join only ADDS labels keyed off
// `{instance,job}` (chplan.InfoJoin never removes an existing label), so
// two rows that differed on some OTHER label before enrichment still
// differ on it afterwards. Reusing [chplan.VectorSetOp] here is therefore
// about reusing vetted, already-tested emitter code for the recombination,
// not about needing its anti-join semantics for correctness.
func infoArgOverMixedExpHistogramSetOp(v parser.Expr, s schema.Metrics, ctx lowerCtx) (*parser.BinaryExpr, bool) {
	return mixedExpHistogramSetOp(v, s, ctx)
}

// lowerInfoOverMixedExpHistogramSetOp lowers the shape
// [infoArgOverMixedExpHistogramSetOp] recognised: c's second argument (the
// label-selector matchers) is resolved once and applied to both of b's
// histogram/float partitions independently, then the two enriched results
// are recombined into a single Mixed-shaped node.
func lowerInfoOverMixedExpHistogramSetOp(c *parser.Call, b *parser.BinaryExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	histPart, floatPart, err := splitMixedExpHistogramSetOpByType(b, s, ctx)
	if err != nil {
		return nil, err
	}

	nameMatchers, dataMatchers, err := infoSecondArgMatchers(c)
	if err != nil {
		return nil, err
	}
	nameMatchers = effectiveInfoNameMatchers(nameMatchers)

	histResult, err := lowerInfoNonStaticBase(histPart, true, nameMatchers, dataMatchers, s, ctx)
	if err != nil {
		return nil, err
	}
	floatResult, err := lowerInfoNonStaticBase(floatPart, false, nameMatchers, dataMatchers, s, ctx)
	if err != nil {
		return nil, err
	}

	return &chplan.VectorSetOp{
		Left:                 histResult,
		Right:                floatResult,
		Op:                   chplan.VectorSetOr,
		StepAligned:          ctx.step > 0,
		Mixed:                true,
		MixedHistogramOnLeft: true,
		MetricNameColumn:     s.MetricNameColumn,
		AttributesColumn:     s.AttributesColumn,
		TimestampColumn:      s.TimestampColumn,
		ValueColumn:          s.ValueColumn,
	}, nil
}
