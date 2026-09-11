package chplan_test

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

func TestRowShapeOfNil(t *testing.T) {
	if got := chplan.RowShapeOf(nil); got != chplan.SampleRowShape {
		t.Fatalf("nil shape = %s; want sample default", got)
	}
}

// allNodeKinds is source-derived from the sealed Node implementations. The
// fold therefore acquires no second node-kind inventory as the IR grows.
func TestRowShapeOfFoldsEveryNodeSchema(t *testing.T) {
	for _, node := range allNodeKinds() {
		want := chplan.RowShapeFromSchema(node.RowType())
		if got := chplan.RowShapeOf(node); got != want {
			t.Errorf("RowShapeOf(%T) = %s, schema fold = %s", node, got, want)
		}
	}
}

func TestRowShapeString(t *testing.T) {
	t.Parallel()

	const outsideDeclaredSet = chplan.MixedRowShape + 1

	for _, tc := range []struct {
		shape chplan.RowShape
		want  string
	}{
		{chplan.SampleRowShape, "sample"},
		{chplan.GridWindowRowShape, "grid-window"},
		{chplan.ReducedWindowRowShape, "reduced-window"},
		{chplan.HistogramRowShape, "histogram"},
		{chplan.MixedRowShape, "mixed"},
		{outsideDeclaredSet, "unknown"},
	} {
		if got := tc.shape.String(); got != tc.want {
			t.Errorf("RowShape(%d).String() = %q, want %q", int(tc.shape), got, tc.want)
		}
	}
}
