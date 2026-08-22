package promql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// histogram_shape_guard.go — the structural assertion that keeps the two
// canonical-column forwarders from silently mis-projecting a
// histogram-valued row.
//
// [projectValueOverInner] and [projectAttributesOverInner] both build a
// projection list that references `Value` by name. That is correct for
// every row shape they can actually receive, and WRONG — silently, not
// loudly — for [chplan.HistogramRowShape]: a [chplan.HistogramProjection]
// does publish a `Value` column, but it is the meaningless placeholder
// the histogram lowerings bind alongside the nine real Histogram*Column
// outputs (mirroring reference Prometheus's own `Sample{F, H}`, where F
// is meaningless once H is set). Forwarding it does not fail in
// ClickHouse; it answers 0 and drops the histogram, which is the worst
// possible failure mode for a numeric gateway.
//
// Today that cannot happen, and the reason is structural rather than
// defensive. A HistogramProjection CAN now sit at a non-root position —
// composing `sum`/`avg` over an already histogram-valued result
// (`sum(rate(m_exp_hist[5m]))`) or rewriting attributes for `label_replace`
// / `label_join` both feed a nested HistogramProjection into another node
// — but the node directly above it is never one of these two forwarders.
// It is always another histogram-aware consumer: [lowerExpHistogramValuedShape]
// recognises the composed shape recursively (via mergeableExpHistogramAggregate
// for the aggregation case, labelCallOverExpHistogram for the label-rewrite
// case) and hands the nested HistogramProjection to a dedicated lowering
// (histogram_native_sum.go's lowerExpHistogramSumOrAvgOverPlan,
// histogram_native_label_replace.go's rewriteHistogramProjectionAttributes)
// that reads its nine columns by name instead of going through
// [projectValueOverInner] / [projectAttributesOverInner]. Every remaining
// consumer either recognises its histogram-valued operand directly
// (scalar/histogram and histogram/histogram binops) or drops it
// ([dropExpHistogramSamples], for float-only functions). Every nested
// exp-histogram selector that reaches none of those recognisers is refused
// by [expHistogramSelectorRouting] before a forwarder ever sees it.
//
// So this is an assertion about an impossible state, not a user-facing
// rejection, and it panics rather than returning an error for exactly
// that reason — the same contract [scanFromTables]'s "called with no
// candidate tables" panic keeps. What it buys is the failure mode issue
// #1967 first asked for and issue #2296 confirmed still held once
// `sum`/`avg` composition and label rewrites grew their own histogram-aware
// lowerings (#2245): the day a FUTURE consumer routes a histogram-valued
// node through one of these two generic forwarders — instead of through the
// shared recognizer set above, or a dedicated lowering of its own — the
// first test that exercises it dies with a message naming the forwarder and
// the fix, instead of quietly returning zeros.
//
// [chplan.MixedRowShape] (cerberus issue #2330, histogram_native_mixed_or.go)
// earns the identical structural guarantee the same way: its lowering
// (lowerMixedExpHistogramSetOp) is registered only at the query ROOT, never
// inside [lowerExpHistogramValuedShape]'s recursive dispatch table, so
// nothing composes a further GENERIC wrapper around a Mixed node — a query
// that tries `abs(a or b)` for a mixed `a`/`b` keeps falling through to
// [expHistogramSelectorRouting]'s existing rejection, exactly as it did
// before this file learned the Mixed shape.
//
// `sum(a or b)` / `avg(a or b)` for a mixed `a`/`b` is the one exception,
// and a deliberate one (cerberus issue #2346,
// histogram_native_mixed_or_aggregate.go): its own root-only recognizer
// ([sumOrAvgOverMixedExpHistogramSetOp]) builds a dedicated reduction —
// two independent SUM/AVG branches (one through the SAME histogram-valued
// machinery this guard protects, one through the ordinary float aggregate
// path) recombined with reference's own "drop a mixed-type output group"
// rule — instead of ever routing a Mixed node through a generic forwarder.
// The panic below still holds for every OTHER wrapper.

// assertValueShapedInput panics when inner publishes
// [chplan.HistogramRowShape] or [chplan.MixedRowShape] — the two shapes
// whose `Value` column is a placeholder on at least some rows rather than
// every row's actual magnitude. forwarder names the calling helper so the
// panic points at the projection that needs teaching. See this file's
// comment for why the state is unreachable and why an unreachable state is
// still worth asserting.
//
// MixedRowShape (cerberus issue #2330) widens the same hazard rather than
// sidestepping it: a [chplan.VectorSetOp] with Mixed set publishes Value
// for every row, but on the rows whose discriminator marks them
// histogram-shaped that Value is the identical
// histogramSampleValuePlaceholder a pure [chplan.HistogramProjection]
// carries. internal/promql's lowerMixedExpHistogramSetOp only ever builds
// a Mixed node at the query ROOT (see that function's doc comment for
// why), so — like the pure-histogram case — no generic forwarder should
// ever actually receive one; this assertion is the same "impossible state,
// asserted anyway" contract as the HistogramRowShape case below.
func assertValueShapedInput(inner chplan.Node, forwarder string) {
	shape := chplan.RowShapeOf(inner)
	if shape != chplan.HistogramRowShape && shape != chplan.MixedRowShape {
		return
	}
	panic(fmt.Sprintf(
		"promql: %s received a %s-shaped input: its projection forwards %q, "+
			"which is only a placeholder on at least some rows rather than every "+
			"row's actual magnitude (a chplan.HistogramProjection publishes it "+
			"alongside the nine Histogram*Column outputs; a Mixed chplan.VectorSetOp "+
			"publishes it as the placeholder on its histogram-shaped rows only). "+
			"Projecting it would answer 0 and drop the histogram on those rows. "+
			"Either route this node through the shared histogram-valued recognizer "+
			"set (see this file's doc comment) instead of %s, or teach %s the "+
			"%s shape directly.",
		forwarder, shape, "Value", forwarder, forwarder, shape,
	))
}
