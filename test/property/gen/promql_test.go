package gen

import (
	"testing"

	"github.com/prometheus/prometheus/promql/parser"
	"pgregory.net/rapid"
)

// TestDrawLabelReplaceWrap_CopyNeverPairedWithLabelStrippingInner is a
// regression test for a property-generator degeneracy: labelReplaceWrapCopy
// (the "(.+)" rename-and-copy branch) can only ever produce a genuine copy
// when the inner expression it wraps still carries the src label. Before
// promQLLabelStrippingShapes existed, pairing it with a fully
// label-stripping inner shape (a bare sum()/sum(rate()), which drops every
// label including __name__) silently collapsed the wrap into a no-op
// indistinguishable from labelReplaceWrapAbsent's own dedicated case,
// degrading the effective test rate of the "rename and copy" semantics for
// 2 of drawExpr's 5 base shapes.
//
// This exercises drawLabelReplaceWrap directly across many draws and
// confirms the copy variant (dst == "endpoint") is never selected for a
// label-stripping inner shape, while a label-preserving inner shape
// (a bare selector, which keeps every label) still gets a real chance to
// draw it — confirming the guard is load-bearing, not just vacuously true.
func TestDrawLabelReplaceWrap_CopyNeverPairedWithLabelStrippingInner(t *testing.T) {
	const copyDst = "endpoint" // labelReplaceWrapCopy's dst sentinel
	groupLabels := []string{"uri", "region"}
	sel := &parser.VectorSelector{Name: "http_requests_total"}

	drawnDst := func(rt *rapid.T, innerShape ShapeID) string {
		call, ok := drawLabelReplaceWrap(rt, sel, groupLabels, innerShape).(*parser.Call)
		if !ok || len(call.Args) != 5 {
			t.Fatalf("drawLabelReplaceWrap did not return a 5-arg label_replace call: %#v", call)
		}
		dst, ok := call.Args[1].(*parser.StringLiteral)
		if !ok {
			t.Fatalf("label_replace dst arg is %T, want *parser.StringLiteral", call.Args[1])
		}
		return dst.Val
	}

	for _, shape := range []ShapeID{promQLSumShape, promQLSumRateShape} {
		t.Run(string(shape), func(t *testing.T) {
			sawCopy := false
			rapid.Check(t, func(rt *rapid.T) {
				if drawnDst(rt, shape) == copyDst {
					sawCopy = true
				}
			})
			if sawCopy {
				t.Errorf("labelReplaceWrapCopy (dst=%q) was drawn for label-stripping shape %s — "+
					"src is guaranteed absent for this inner shape, so this degrades to a no-op",
					copyDst, shape)
			}
		})
	}

	t.Run("selector inner still draws the copy variant", func(t *testing.T) {
		sawCopy := false
		rapid.Check(t, func(rt *rapid.T) {
			if drawnDst(rt, promQLSelectorShape) == copyDst {
				sawCopy = true
			}
		})
		if !sawCopy {
			t.Error("labelReplaceWrapCopy was never drawn for a label-preserving inner shape across many " +
				"samples — the label-stripping guard may be over-broad")
		}
	})
}
