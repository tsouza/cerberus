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
