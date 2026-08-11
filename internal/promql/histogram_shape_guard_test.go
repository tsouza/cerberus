package promql

import (
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// The two canonical-column forwarders reference `Value` by name. A
// chplan.HistogramProjection publishes a `Value` column too — but only as
// the placeholder it binds alongside the nine Histogram*Column outputs —
// so forwarding it emits VALID ClickHouse that answers 0 and drops the
// histogram. That is the silent mis-projection issue #1967 asked to be
// made loud.
//
// These tests are the ratchet on the assertion itself. Deleting
// assertValueShapedInput, or widening chplan.RowShapeOf so a
// HistogramProjection stops answering HistogramRowShape, fails here
// rather than in a numeric diff nobody attributes to this change.

// histogramShapedInput returns a minimal plan node that publishes
// chplan.HistogramRowShape.
func histogramShapedInput() chplan.Node {
	return &chplan.HistogramProjection{
		Input:                      &chplan.Scan{Table: "otel_metrics_exponential_histogram"},
		CountColumn:                "Count",
		SumColumn:                  "Sum",
		ScaleColumn:                "Scale",
		ZeroCountColumn:            "ZeroCount",
		PositiveOffsetColumn:       "PositiveOffset",
		PositiveBucketCountsColumn: "PositiveBucketCounts",
		NegativeOffsetColumn:       "NegativeOffset",
		NegativeBucketCountsColumn: "NegativeBucketCounts",
	}
}

func TestProjectForwarders_PanicOnHistogramShapedInput(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	cases := []struct {
		name string
		call func()
	}{
		{
			name: "projectValueOverInner",
			call: func() {
				projectValueOverInner(histogramShapedInput(), s, &chplan.LitFloat{V: 1})
			},
		},
		{
			name: "projectAttributesOverInner",
			call: func() {
				projectAttributesOverInner(histogramShapedInput(), s, &chplan.ColumnRef{Name: s.AttributesColumn})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := capturePanic(t, tc.call)
			// The message must name the forwarder that needs teaching
			// and the shape it refused, so the failure is actionable
			// without reading this test.
			if !strings.Contains(msg, tc.name) {
				t.Errorf("panic message does not name the forwarder %q: %s", tc.name, msg)
			}
			if !strings.Contains(msg, chplan.HistogramRowShape.String()) {
				t.Errorf("panic message does not name the %s shape: %s", chplan.HistogramRowShape, msg)
			}
			if !strings.Contains(msg, "#1967") {
				t.Errorf("panic message does not cite the tracking issue: %s", msg)
			}
		})
	}
}

// The assertion must be narrow: a blanket panic would be caught by every
// other test, but a guard that fires on the SampleRowShape / windowed
// shapes these forwarders exist to serve would be a regression the moment
// someone widened the condition by accident.
func TestProjectForwarders_AcceptEveryNonHistogramShape(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	scan := &chplan.Scan{Table: "otel_metrics_gauge"}
	inputs := []struct {
		name  string
		node  chplan.Node
		shape chplan.RowShape
	}{
		{name: "sample", node: scan, shape: chplan.SampleRowShape},
		{
			name:  "grid-window",
			node:  &chplan.RangeWindow{Input: scan, OuterRange: 1},
			shape: chplan.GridWindowRowShape,
		},
		{
			name:  "reduced-window",
			node:  &chplan.RangeWindow{Input: scan},
			shape: chplan.ReducedWindowRowShape,
		},
	}
	for _, in := range inputs {
		t.Run(in.name, func(t *testing.T) {
			if got := chplan.RowShapeOf(in.node); got != in.shape {
				t.Fatalf("fixture publishes %s, want %s", got, in.shape)
			}
			// Both forwarders must return a projection rather than panic.
			if got := projectValueOverInner(in.node, s, &chplan.LitFloat{V: 1}); got == nil {
				t.Error("projectValueOverInner returned nil")
			}
			if got := projectAttributesOverInner(in.node, s, &chplan.ColumnRef{Name: s.AttributesColumn}); got == nil {
				t.Error("projectAttributesOverInner returned nil")
			}
		})
	}
}

// capturePanic runs fn and returns the panic value formatted as a string,
// failing the test if fn returned normally.
func capturePanic(t *testing.T, fn func()) (msg string) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected a panic, got a normal return")
		}
		s, ok := r.(string)
		if !ok {
			t.Fatalf("panic value is %T, want string: %v", r, r)
		}
		msg = s
	}()
	fn()
	return ""
}
