package promql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// mixedDiscriminatorColumn is internal/promql's own name for
// [chplan.MixedDiscriminatorColumn] — internal/promql may depend on
// chplan (.go-arch-lint.yml), so this aliases the single definition
// rather than duplicating the literal the way internal/chclient's
// decode-side probe has to. [projectAttributesOverInner] (label_fns.go)
// and [guardLabelRewriteCollision] (duplicate_labelset_guard.go) both
// reference it by name to forward the column unchanged through a
// `label_replace`/`label_join` rewrite over a mixed `or` (cerberus issue
// #2449); [chplan.RowShapeOf]'s own [*chplan.Project] case reads it back
// to recognise the result as still [chplan.MixedRowShape].
const mixedDiscriminatorColumn = chplan.MixedDiscriminatorColumn

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
// inside [lowerExpHistogramValuedShape]'s recursive dispatch table, so no
// wrapper OTHER than the two exceptions below ever reaches a generic
// forwarder with a Mixed node — a query that tries `abs(a or b)` for a
// mixed `a`/`b` keeps falling through to [expHistogramSelectorRouting]'s
// existing rejection, exactly as it did before this file learned the
// Mixed shape (cerberus issue #2449 tracks the remaining wrapper
// families as an open divergence in test/rejection-parity/catalogue).
//
// `sum(a or b)` / `avg(a or b)` for a mixed `a`/`b` is one exception,
// and a deliberate one (cerberus issue #2346,
// histogram_native_mixed_or_aggregate.go): its own root-only recognizer
// ([sumOrAvgOverMixedExpHistogramSetOp]) builds a dedicated reduction —
// two independent SUM/AVG branches (one through the SAME histogram-valued
// machinery this guard protects, one through the ordinary float aggregate
// path) recombined with reference's own "drop a mixed-type output group"
// rule — instead of ever routing a Mixed node through a generic forwarder.
//
// `label_replace(a or b, ...)` / `label_join(a or b, ...)` for a mixed
// `a`/`b` are the SECOND exception, since cerberus issue #2449:
// histogram_native_mixed_or_label.go's own root-only recognizer
// ([labelCallOverMixedExpHistogramSetOp]) routes the SAME Mixed node the
// leaf case builds through [projectAttributesOverInner] itself — unlike
// the sum/avg case, this one DOES hand a Mixed node to a generic
// forwarder, because a label rewrite touches only Attributes and needs
// no per-payload branching: [projectAttributesOverInner] carries its own
// [chplan.MixedRowShape] branch (see that function's doc comment,
// label_fns.go) instead of taking the panic below.
// [projectValueOverInner] has grown no such branch — a value transform
// genuinely differs by payload and would need a discriminator-keyed
// `chplan.Case`/CH `if()` this issue's PR did not build — so the panic
// below still holds for it, and for every wrapper family neither
// exception recognises.

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
// why), so this assertion is the same "impossible state, asserted
// anyway" contract as the HistogramRowShape case below for
// [projectValueOverInner] — Value genuinely IS unsafe to forward
// unconditionally for that caller. [projectAttributesOverInner]
// (cerberus issue #2449, label_fns.go) is the one caller this is no
// longer true for: it checks [chplan.RowShapeOf] itself and takes a
// dedicated MixedRowShape branch BEFORE ever reaching this function, so
// this panic still fires only when some future caller of
// [projectAttributesOverInner] finds a way to bypass that branch, not on
// the ordinary `label_replace`/`label_join`-over-mixed-`or` path.
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
