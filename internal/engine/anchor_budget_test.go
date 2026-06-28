package engine

import (
	"errors"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
)

func subqueryRW(outer, step time.Duration, input chplan.Node) *chplan.RangeWindow {
	return &chplan.RangeWindow{
		Func: "rate", Range: time.Minute, OuterRange: outer, Step: step, Input: input,
	}
}

func TestRequireSubquerySampleBudget(t *testing.T) {
	t.Parallel()
	scan := &chplan.Scan{Table: "otel_metrics_sum"}
	const day = 24 * time.Hour

	cases := []struct {
		name       string
		plan       chplan.Node
		maxSamples int64
		wantReject bool
	}{
		// 90d/1s = 7,776,001 anchors > 1M budget → reject like Prometheus.
		{"giant grid over budget rejects", subqueryRW(90*day, time.Second, scan), 1_000_000, true},
		// 5m/30s = 11 anchors, trivially under budget.
		{"normal subquery passes", subqueryRW(5*time.Minute, 30*time.Second, scan), 1_000_000, false},
		// 0 disables the budget — even the giant grid is allowed through.
		{"budget disabled passes giant", subqueryRW(90*day, time.Second, scan), 0, false},
		// A non-subquery instant leaf (OuterRange 0) has NumAnchors 0.
		{"non-subquery leaf passes", &chplan.RangeWindow{Func: "rate", Range: time.Minute, Input: scan}, 1_000_000, false},
		// The walk finds the worst grid nested under a wrapper.
		{"nested giant grid rejects", &chplan.Aggregate{Input: subqueryRW(90*day, time.Second, scan)}, 1_000_000, true},
		{"nil plan passes", nil, 1_000_000, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := requireSubquerySampleBudget(tc.plan, tc.maxSamples)
			switch {
			case tc.wantReject && err == nil:
				t.Fatal("want rejection, got nil")
			case tc.wantReject && !errors.Is(err, chclient.ErrTooManySamples):
				t.Fatalf("want ErrTooManySamples, got %v", err)
			case !tc.wantReject && err != nil:
				t.Fatalf("want pass, got %v", err)
			}
		})
	}
}

func TestRangeWindowNumAnchors(t *testing.T) {
	t.Parallel()
	const day = 24 * time.Hour
	cases := []struct {
		outer, step time.Duration
		want        int64
	}{
		{90 * day, time.Second, 7_776_001},
		{5 * time.Minute, 30 * time.Second, 11},
		{0, time.Second, 0}, // not a subquery
		{time.Hour, 0, 0},   // unstepped
	}
	for _, c := range cases {
		got := (&chplan.RangeWindow{OuterRange: c.outer, Step: c.step}).NumAnchors()
		if got != c.want {
			t.Errorf("NumAnchors(outer=%s step=%s): got %d, want %d", c.outer, c.step, got, c.want)
		}
	}
}
