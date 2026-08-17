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
// defensive: a HistogramProjection is built ONLY by [lowerRoot], which is
// reached only from the two public entry points ([Lower] and [LowerRange])
// and never recursively, so the node is only ever the query ROOT. A
// forwarder, by construction, wraps something — it is only ever called on
// a non-root position. Every nested exp-histogram selector is refused
// earlier still, by [expHistogramSelectorRouting].
//
// So this is an assertion about an impossible state, not a user-facing
// rejection, and it panics rather than returning an error for exactly
// that reason — the same contract [scanFromTables]'s "called with no
// candidate tables" panic keeps. What it buys is the failure mode issue
// #1967 asked for: the day someone teaches a nested shape to carry a
// histogram (lifting the routing guard for `label_join`, `abs`, or
// arithmetic — see issue #2296, since `label_replace` itself now has its
// own dedicated lowering) without first teaching these forwarders the
// shape, the first test that exercises it dies with a message naming the
// forwarder and the fix, instead of quietly returning zeros.

// assertValueShapedInput panics when inner publishes
// [chplan.HistogramRowShape], the one shape whose `Value` column is a
// placeholder rather than the sample's actual magnitude. forwarder names
// the calling helper so the panic points at the projection that needs
// teaching. See this file's comment for why the state is unreachable and
// why an unreachable state is still worth asserting.
func assertValueShapedInput(inner chplan.Node, forwarder string) {
	if chplan.RowShapeOf(inner) != chplan.HistogramRowShape {
		return
	}
	panic(fmt.Sprintf(
		"promql: %s received a %s-shaped input: its projection forwards %q, "+
			"which a chplan.HistogramProjection publishes only as a placeholder "+
			"alongside the nine Histogram*Column outputs. Projecting it would "+
			"answer 0 and drop the histogram. Teach %s the histogram shape "+
			"before routing a histogram-valued node through it (issue #2296).",
		forwarder, chplan.HistogramRowShape, "Value", forwarder,
	))
}
