package engine

import (
	"errors"
	"math"
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

// TestSubqueryAnchorLoad_NestedProduct pins GAP-C: nested subquery grids
// multiply (each outer anchor re-evaluates the inner grid), siblings don't, and
// the product saturates rather than overflowing.
func TestSubqueryAnchorLoad_NestedProduct(t *testing.T) {
	t.Parallel()
	scan := &chplan.Scan{Table: "otel_metrics_sum"}
	// 1h/1m + 1 = 61 anchors per grid.
	grid := func(in chplan.Node) *chplan.RangeWindow {
		return &chplan.RangeWindow{Func: "rate", Range: time.Minute, OuterRange: time.Hour, Step: time.Minute, Input: in}
	}
	single := grid(scan)
	nested := &chplan.RangeWindow{Func: "max_over_time", Range: time.Hour, OuterRange: time.Hour, Step: time.Minute, Input: grid(scan)}

	if got := subqueryAnchorLoad(single); got != 61 {
		t.Fatalf("single: got %d, want 61", got)
	}
	// Nested multiplies: 61 * 61 = 3721 — the max-only count would have seen 61.
	if got := subqueryAnchorLoad(nested); got != 61*61 {
		t.Fatalf("nested product: got %d, want %d", got, 61*61)
	}
	// Siblings (two arms of a join) do NOT multiply — only the deepest chain.
	siblings := &chplan.CrossJoin{Left: grid(scan), Right: grid(scan)}
	if got := subqueryAnchorLoad(siblings); got != 61 {
		t.Fatalf("siblings: got %d, want 61 (max, not product)", got)
	}
	// The product propagates THROUGH a wrapper (Project) between the two grids —
	// the real lowered shape, where the outer reducer sits over a Project over
	// the inner subquery matrix.
	throughWrapper := &chplan.RangeWindow{
		Func: "max_over_time", Range: time.Hour, OuterRange: time.Hour, Step: time.Minute,
		Input: &chplan.Project{Input: grid(scan)},
	}
	if got := subqueryAnchorLoad(throughWrapper); got != 61*61 {
		t.Fatalf("product through wrapper: got %d, want %d", got, 61*61)
	}
	// The budget rejects the nested product but passes either level alone.
	if err := requireSubquerySampleBudget(nested, 1000); !errors.Is(err, chclient.ErrTooManySamples) {
		t.Fatalf("nested over budget 1000: want reject, got %v", err)
	}
	if err := requireSubquerySampleBudget(single, 1000); err != nil {
		t.Fatalf("single under budget 1000: want pass, got %v", err)
	}
}

func TestSatMulInt64_Saturates(t *testing.T) {
	t.Parallel()
	if got := satMulInt64(0, 1<<40); got != 0 {
		t.Fatalf("0*x: got %d, want 0", got)
	}
	if got := satMulInt64(3, 4); got != 12 {
		t.Fatalf("3*4: got %d, want 12", got)
	}
	// 7.78M ^ 3 overflows int64 — must saturate, never wrap negative.
	big := int64(7_776_001)
	if got := satMulInt64(satMulInt64(big, big), big); got != math.MaxInt64 {
		t.Fatalf("triple-nested product: got %d, want MaxInt64", got)
	}
}
