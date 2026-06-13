package chplan_test

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

func newRangeLWR() *chplan.RangeLWR {
	return &chplan.RangeLWR{
		Input:         &chplan.Scan{Table: "otel_metrics_gauge"},
		Start:         time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		End:           time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC),
		Step:          30 * time.Second,
		Lookback:      5 * time.Minute,
		Offset:        0,
		MetricNameCol: "MetricName",
		AttributesCol: "Attributes",
		TimestampCol:  "TimeUnix",
		ValueCol:      "Value",
	}
}

func TestRangeLWR_Equal_Positive(t *testing.T) {
	t.Parallel()
	if !newRangeLWR().Equal(newRangeLWR()) {
		t.Fatalf("identical RangeLWR trees should be Equal")
	}
}

func TestRangeLWR_Equal_Negative_Step(t *testing.T) {
	t.Parallel()
	a := newRangeLWR()
	b := newRangeLWR()
	b.Step = time.Minute
	if a.Equal(b) {
		t.Errorf("different Step should not be Equal")
	}
}

func TestRangeLWR_Equal_Negative_Offset(t *testing.T) {
	t.Parallel()
	a := newRangeLWR()
	b := newRangeLWR()
	b.Offset = -5 * time.Minute
	if a.Equal(b) {
		t.Errorf("different Offset should not be Equal")
	}
}

func TestRangeLWR_Equal_Negative_Lookback(t *testing.T) {
	t.Parallel()
	a := newRangeLWR()
	b := newRangeLWR()
	b.Lookback = time.Minute
	if a.Equal(b) {
		t.Errorf("different Lookback should not be Equal")
	}
}

func TestRangeLWR_Equal_Negative_Cols(t *testing.T) {
	t.Parallel()
	a := newRangeLWR()
	b := newRangeLWR()
	b.ValueCol = "OtherValue"
	if a.Equal(b) {
		t.Errorf("different ValueCol should not be Equal")
	}
}

// TestRangeLWR_Equal_Negative_Start / _End pin the `!Start.Equal ||
// !End.Equal` disjunct: flipping the `||` to `&&` would only report a
// divergence when BOTH bounds differ, so a single-bound mismatch must
// still come back not-Equal. (The Step/Offset/Lookback cases above sit
// on a different line; this one is the time-bound clause.)
func TestRangeLWR_Equal_Negative_Start(t *testing.T) {
	t.Parallel()
	a := newRangeLWR()
	b := newRangeLWR()
	b.Start = b.Start.Add(time.Second) // End stays equal
	if a.Equal(b) {
		t.Errorf("different Start (End equal) should not be Equal")
	}
}

func TestRangeLWR_Equal_Negative_End(t *testing.T) {
	t.Parallel()
	a := newRangeLWR()
	b := newRangeLWR()
	b.End = b.End.Add(time.Second) // Start stays equal
	if a.Equal(b) {
		t.Errorf("different End (Start equal) should not be Equal")
	}
}

// TestRangeLWR_Equal_Negative_MetricNameCol / _AttributesCol pin the
// `MetricNameCol != || AttributesCol !=` disjunct (a different line
// from the ValueCol case above). `||` → `&&` would require both columns
// to differ before reporting not-Equal.
func TestRangeLWR_Equal_Negative_MetricNameCol(t *testing.T) {
	t.Parallel()
	a := newRangeLWR()
	b := newRangeLWR()
	b.MetricNameCol = "OtherMetricName" // AttributesCol stays equal
	if a.Equal(b) {
		t.Errorf("different MetricNameCol (AttributesCol equal) should not be Equal")
	}
}

func TestRangeLWR_Equal_Negative_AttributesCol(t *testing.T) {
	t.Parallel()
	a := newRangeLWR()
	b := newRangeLWR()
	b.AttributesCol = "OtherAttributes" // MetricNameCol stays equal
	if a.Equal(b) {
		t.Errorf("different AttributesCol (MetricNameCol equal) should not be Equal")
	}
}

// TestRangeLWR_Equal_Negative_InputOneNil pins the `Input == nil ||
// o.Input == nil` short-circuit: one side has a nil Input, the other a
// real one. `||` → `&&` would skip the early-out and walk into
// Input.Equal on a nil receiver (or mis-report equality), so the nodes
// must come back not-Equal both ways.
func TestRangeLWR_Equal_Negative_InputOneNil(t *testing.T) {
	t.Parallel()
	a := newRangeLWR()
	b := newRangeLWR()
	b.Input = nil
	if a.Equal(b) {
		t.Errorf("non-nil Input vs nil Input should not be Equal")
	}
	if b.Equal(a) {
		t.Errorf("nil Input vs non-nil Input should not be Equal (reverse)")
	}
}

func TestRangeLWR_Equal_Negative_Input(t *testing.T) {
	t.Parallel()
	a := newRangeLWR()
	b := newRangeLWR()
	b.Input = &chplan.Scan{Table: "otel_metrics_sum"}
	if a.Equal(b) {
		t.Errorf("different Input should not be Equal")
	}
}

func TestRangeLWR_Equal_Negative_Type(t *testing.T) {
	t.Parallel()
	if newRangeLWR().Equal(&chplan.Scan{Table: "otel_metrics_gauge"}) {
		t.Errorf("RangeLWR should not equal a Scan")
	}
}

func TestRangeLWR_Children_ReturnsExactlyInput(t *testing.T) {
	t.Parallel()
	input := &chplan.Scan{Table: "t"}
	r := &chplan.RangeLWR{Input: input}
	kids := r.Children()
	if len(kids) != 1 || kids[0] != input {
		t.Errorf("RangeLWR.Children() should return [Input], got %v", kids)
	}
}

func TestRangeLWR_Walk_VisitsInput(t *testing.T) {
	t.Parallel()
	input := &chplan.Scan{Table: "t"}
	root := &chplan.RangeLWR{Input: input}
	var sawInput bool
	chplan.Walk(root, func(n chplan.Node) bool {
		if n == input {
			sawInput = true
		}
		return true
	})
	if !sawInput {
		t.Errorf("Walk over RangeLWR should visit its Input")
	}
}
