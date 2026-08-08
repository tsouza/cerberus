package chplan_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// rowShapeVerdicts is the expected chplan.RowShapeOf answer for one
// populated instance of every Node kind — the instances in allNodeKinds(),
// whose RangeWindow carries `OuterRange: time.Hour` and is therefore the
// MATRIX form.
//
// The verdict is a statement about the columns that kind's own SELECT
// exposes to the projection directly above it, and both PromQL forwarders
// (projectAttributesOverInner, projectValueOverInner) build their column
// list from this one answer. A wrong verdict is not a stylistic
// disagreement: it is either a ClickHouse code 47 for a column the scope
// does not carry, or a column silently missing from the wire.
//
// TestRowShapeOf_CoversEveryNodeKind derives the key set from the package's
// planNode() implementers, so a new Node kind cannot be added without
// deciding its row shape here.
var rowShapeVerdicts = map[string]chplan.RowShape{
	// The two windows that publish a query_range grid: one row per
	// (series, anchor), the anchor under both its own name and the
	// timestamp column, and no MetricName.
	"RangeWindow":       chplan.GridWindowRowShape,
	"RangeWindowNative": chplan.GridWindowRowShape,

	// Everything else republishes the canonical four names, whether by
	// passing its input's through or by projecting them itself.
	"AbsentOverTime":           chplan.SampleRowShape,
	"Aggregate":                chplan.SampleRowShape,
	"CrossJoin":                chplan.SampleRowShape,
	"Filter":                   chplan.SampleRowShape,
	"HistogramQuantile":        chplan.SampleRowShape,
	"HistogramQuantileNative":  chplan.SampleRowShape,
	"HistogramProjection":      chplan.HistogramRowShape,
	"InfoJoin":                 chplan.SampleRowShape,
	"Limit":                    chplan.SampleRowShape,
	"MetricsAggregate":         chplan.SampleRowShape,
	"MetricsCompare":           chplan.SampleRowShape,
	"MetricsHistogramOverTime": chplan.SampleRowShape,
	"MetricsSecondStage":       chplan.SampleRowShape,
	"NaryVectorSetOp":          chplan.SampleRowShape,
	"NestedSetAnnotate":        chplan.SampleRowShape,
	"OneRow":                   chplan.SampleRowShape,
	"OrderBy":                  chplan.SampleRowShape,
	"Project":                  chplan.SampleRowShape,
	"RangeBucketFanout":        chplan.SampleRowShape,
	"RangeLWR":                 chplan.SampleRowShape,
	"RangeWindowResample":      chplan.SampleRowShape,
	"Scan":                     chplan.SampleRowShape,
	"SearchTraceLimit":         chplan.SampleRowShape,
	"SetOperation":             chplan.SampleRowShape,
	"StepGrid":                 chplan.SampleRowShape,
	"StructuralJoin":           chplan.SampleRowShape,
	"TopK":                     chplan.SampleRowShape,
	"UnionAll":                 chplan.SampleRowShape,
	"VectorJoin":               chplan.SampleRowShape,
	"VectorSetOp":              chplan.SampleRowShape,
}

// TestRowShapeOf_CoversEveryNodeKind is the ratchet: a Node kind added to
// the IR without a recorded row shape fails here, because a forwarder
// placed over it would silently take the canonical branch by default.
func TestRowShapeOf_CoversEveryNodeKind(t *testing.T) {
	t.Parallel()

	covered := make(map[string]bool, len(rowShapeVerdicts))
	for kind := range rowShapeVerdicts {
		covered[kind] = true
	}
	assertCoversEveryNodeKind(t, covered, "rowShapeVerdicts",
		"record the row shape its emitter publishes and, if it is not the canonical "+
			"sample row, add an arm to chplan.RowShapeOf")
}

// TestRowShapeOf_ClassifiesEveryNodeKind runs the classifier over one
// populated instance of every Node kind and compares against the recorded
// verdict, naming the kind on a mismatch.
func TestRowShapeOf_ClassifiesEveryNodeKind(t *testing.T) {
	t.Parallel()

	for _, node := range allNodeKinds() {
		kind := reflect.TypeOf(node).Elem().Name()
		want, recorded := rowShapeVerdicts[kind]
		if !recorded {
			// Reported by TestRowShapeOf_CoversEveryNodeKind; skipping the
			// comparison keeps this failure from duplicating that one.
			continue
		}
		if got := chplan.RowShapeOf(node); got != want {
			t.Errorf("RowShapeOf(*%s) = %s, want %s — a projection forwarding over %s "+
				"builds its column list from this answer, so the wrong one is a live "+
				"code 47 or a column dropped from the wire",
				kind, got, want, kind)
		}
	}
}

// TestRowShapeOf_RangeWindowSplitsOnOuterRange pins the one discrimination
// allNodeKinds() cannot express, because it holds a single RangeWindow
// instance: the SAME node kind answers differently depending on whether it
// materialises a query_range grid.
//
// A matrix window (OuterRange > 0) emits one row per (series, anchor) and
// has both timestamp names to forward; an instant one has already collapsed
// each series to a single row and has no timestamp at all. Collapsing the
// two would make an instant `rate(m[5m])` under a label rewrite forward
// columns its scope does not expose.
func TestRowShapeOf_RangeWindowSplitsOnOuterRange(t *testing.T) {
	t.Parallel()

	base := chplan.RangeWindow{
		Input: &chplan.Scan{Table: "otel_metrics_sum"},
		Func:  "rate", Range: 5 * time.Minute, Step: time.Minute,
		Start: time.Unix(1000, 0).UTC(), End: time.Unix(4600, 0).UTC(),
	}

	instant := base
	if got := chplan.RowShapeOf(&instant); got != chplan.ReducedWindowRowShape {
		t.Errorf("RowShapeOf(instant RangeWindow) = %s, want %s", got, chplan.ReducedWindowRowShape)
	}

	matrix := base
	matrix.OuterRange = time.Hour
	if got := chplan.RowShapeOf(&matrix); got != chplan.GridWindowRowShape {
		t.Errorf("RowShapeOf(matrix RangeWindow) = %s, want %s", got, chplan.GridWindowRowShape)
	}
}

// TestRowShapeString pins the names the failure messages above are written
// against, including the answer for a value outside the declared set — a
// forwarder reading a corrupt shape should say so rather than print an
// integer.
func TestRowShapeString(t *testing.T) {
	t.Parallel()

	const outsideDeclaredSet = chplan.HistogramRowShape + 1

	for _, tc := range []struct {
		shape chplan.RowShape
		want  string
	}{
		{chplan.SampleRowShape, "sample"},
		{chplan.GridWindowRowShape, "grid-window"},
		{chplan.ReducedWindowRowShape, "reduced-window"},
		{chplan.HistogramRowShape, "histogram"},
		{outsideDeclaredSet, "unknown"},
	} {
		if got := tc.shape.String(); got != tc.want {
			t.Errorf("RowShape(%d).String() = %q, want %q", int(tc.shape), got, tc.want)
		}
	}
}
