package solver

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// rwReach builds a matrix RangeWindow on the canonical grid with a caller-chosen
// Range and Offset over input, for exercising spineReach in isolation.
func rwReach(rang, offset time.Duration, input chplan.Node) *chplan.RangeWindow {
	return &chplan.RangeWindow{
		Input:           input,
		Func:            "rate",
		Range:           rang,
		Offset:          offset,
		Step:            gridStep,
		OuterRange:      time.Hour,
		Start:           gridStart,
		End:             gridEnd,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
	}
}

// joinArms is a minimal StepAligned VectorJoin over two arms — only the spine
// arms matter to spineReach.
func joinArms(left, right chplan.Node) *chplan.VectorJoin {
	return &chplan.VectorJoin{
		Left:            left,
		Right:           right,
		StepAligned:     true,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
	}
}

// TestSpineReach pins the scan-floor depth spineReach derives for each spine
// shape: Range/Lookback and Offset compound along a nested path, and parallel
// join arms take the deepest (max), so an asymmetric per-arm offset can never
// under-scan the deeper arm.
func TestSpineReach(t *testing.T) {
	t.Parallel()
	const (
		fiveMin = 5 * time.Minute
		oneHour = time.Hour
	)
	tests := []struct {
		name string
		plan chplan.Node
		want time.Duration
	}{
		{
			name: "single window, no offset",
			plan: rwReach(fiveMin, 0, leafScan()),
			want: fiveMin,
		},
		{
			name: "single window offset compounds with range",
			plan: rwReach(fiveMin, oneHour, leafScan()),
			want: fiveMin + oneHour,
		},
		{
			name: "aggregate passes through to its spine child",
			plan: &chplan.Aggregate{Input: rwReach(fiveMin, oneHour, leafScan())},
			want: fiveMin + oneHour,
		},
		{
			name: "lwr uses lookback plus offset",
			plan: lwrSpine(oneHour), // Lookback 5m + offset 1h
			want: fiveMin + oneHour,
		},
		{
			name: "nested subquery compounds range and offset along the path",
			// outer(Range=1h, off=2h) over inner(Range=5m, off=1h)
			plan: rwReach(oneHour, 2*oneHour, rwReach(fiveMin, oneHour, leafScan())),
			want: oneHour + 2*oneHour + fiveMin + oneHour,
		},
		{
			name: "join takes the deeper (right) arm, not last-wins",
			// left 5m+0 vs right 5m+1h — a single last-offset-wins scalar would
			// pick the left arm's 0 offset and under-scan the right arm.
			plan: joinArms(rwReach(fiveMin, 0, leafScan()), rwReach(fiveMin, oneHour, leafScan())),
			want: fiveMin + oneHour,
		},
		{
			name: "join takes the deeper (left) arm",
			plan: joinArms(rwReach(fiveMin, 2*oneHour, leafScan()), rwReach(fiveMin, oneHour, leafScan())),
			want: fiveMin + 2*oneHour,
		},
		{
			name: "join with a future-looking (negative offset) arm keeps the past arm",
			// left 5m+(-1h) = -55m (reaches into the future) vs right 5m+0
			plan: joinArms(rwReach(fiveMin, -oneHour, leafScan()), rwReach(fiveMin, 0, leafScan())),
			want: fiveMin,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := spineReach(tc.plan); got != tc.want {
				t.Fatalf("spineReach = %v, want %v", got, tc.want)
			}
		})
	}
}
