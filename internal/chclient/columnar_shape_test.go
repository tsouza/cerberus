package chclient

import (
	"context"
	"testing"

	chproto "github.com/ClickHouse/ch-go/proto"
)

// matrixShapedResults builds a chproto.Results that structurally matches
// matrixColumns exactly — same names, same types, in order — without any
// ClickHouse connection. It stands in for a real query_range matrix result
// block AND for the historical Loki-collision scenario (#1429): a
// differently-intentioned projection that happens to reuse the same four
// names and types.
func matrixShapedResults() chproto.Results {
	return chproto.Results{
		{Name: "MetricName", Data: &chproto.ColStr{}},
		{Name: "Attributes", Data: &chproto.ColMap[string, string]{}},
		{Name: "TimeUnix", Data: &chproto.ColDateTime{}},
		{Name: "Value", Data: &chproto.ColFloat64{}},
	}
}

// TestBindColumns_StructuralMatchAlone proves the pre-#1429 behaviour is
// unchanged: a structurally matrix-shaped result binds when no ResponseShape
// was declared (responseShape == ""), i.e. a caller that hasn't opted into
// the new gate sees byte-identical behaviour to before it existed.
func TestBindColumns_StructuralMatchAlone(t *testing.T) {
	cur := &columnarCursor{results: matrixShapedResults()}
	if _, ok := cur.bindColumns(); !ok {
		t.Fatal("bindColumns declined a structurally matching, unshaped (responseShape==\"\") result — pre-#1429 behaviour regressed")
	}
}

// TestBindColumns_ResponseShapeMatch proves the happy defense-in-depth path:
// a structurally matching result with the correct declared ResponseShape
// still binds.
func TestBindColumns_ResponseShapeMatch(t *testing.T) {
	cur := &columnarCursor{results: matrixShapedResults(), responseShape: ResponseShapeMatrix}
	if _, ok := cur.bindColumns(); !ok {
		t.Fatal("bindColumns declined a structurally matching result whose declared ResponseShape agrees")
	}
}

// TestBindColumns_ResponseShapeMismatch is the #1429 regression pin: a
// result block that is STRUCTURALLY indistinguishable from the matrix shape
// (same four names, same four types) must still be declined when the caller
// declared a non-matrix ResponseShape — the scenario a future projection
// reusing those names would hit. This is the second, independent check
// ANDed on top of the untouched structural test; either one declining is
// enough to fall back to the row path.
func TestBindColumns_ResponseShapeMismatch(t *testing.T) {
	cur := &columnarCursor{results: matrixShapedResults(), responseShape: "loki-streams"}
	if _, ok := cur.bindColumns(); ok {
		t.Fatal("bindColumns accepted a structurally matching result despite a mismatched declared ResponseShape — #1429 defense-in-depth gate not engaging")
	}
}

// TestResponseShapeContext_RoundTrip pins WithResponseShape /
// ResponseShapeFromContext against drift: the value set is exactly the
// value read back, and an untouched ctx reads back "".
func TestResponseShapeContext_RoundTrip(t *testing.T) {
	if got := ResponseShapeFromContext(context.Background()); got != "" {
		t.Fatalf("ResponseShapeFromContext(bare ctx) = %q, want \"\"", got)
	}
	ctx := WithResponseShape(context.Background(), ResponseShapeMatrix)
	if got := ResponseShapeFromContext(ctx); got != ResponseShapeMatrix {
		t.Fatalf("ResponseShapeFromContext = %q, want %q", got, ResponseShapeMatrix)
	}
}
