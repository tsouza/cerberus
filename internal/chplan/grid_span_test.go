package chplan

import (
	"math"
	"testing"
	"time"
)

// Instants about 328 years apart, inside DateTime64(9)'s range but further
// apart than a time.Duration holds, and the 100-day step that lays a grid
// across them.
var (
	wideStart = time.Date(1698, 1, 1, 0, 0, 0, 0, time.UTC)
	wideEnd   = time.Date(2025, 11, 8, 0, 0, 0, 0, time.UTC)
)

const wideStep = 100 * 24 * time.Hour

// wideStepsAndRem returns the whole steps from wideStart to wideEnd and
// the remainder, counted in days: no time.Duration is involved.
func wideStepsAndRem(t *testing.T) (int64, time.Duration) {
	t.Helper()
	const secondsPerDay = 24 * 60 * 60
	days := (wideEnd.Unix() - wideStart.Unix()) / secondsPerDay
	return days / 100, time.Duration(days%100) * 24 * time.Hour
}

func TestStepsBetween_BeyondDuration(t *testing.T) {
	t.Parallel()
	if d := wideEnd.Sub(wideStart); d != time.Duration(math.MaxInt64) {
		t.Fatalf("fixture: End.Sub(Start) = %s, want it saturated", d)
	}
	wantSteps, wantRem := wideStepsAndRem(t)
	steps, rem := StepsBetween(wideStart, wideEnd, wideStep)
	if steps != wantSteps || rem != wantRem {
		t.Fatalf("StepsBetween = %d, %s; want %d, %s", steps, rem, wantSteps, wantRem)
	}
}

func TestStepsBetween_NegativeDistanceFloors(t *testing.T) {
	t.Parallel()
	start := time.Unix(100, 0)
	steps, rem := StepsBetween(start, start.Add(-90*time.Second), time.Minute)
	if steps != -2 || rem != 30*time.Second {
		t.Fatalf("StepsBetween(-90s, 1m) = %d, %s; want -2, 30s", steps, rem)
	}
}

func TestStepsBetween_SubSecondParts(t *testing.T) {
	t.Parallel()
	start := time.Unix(10, 900_000_000)
	steps, rem := StepsBetween(start, time.Unix(12, 100_000_000), 500*time.Millisecond)
	if steps != 2 || rem != 200*time.Millisecond {
		t.Fatalf("StepsBetween(1.2s, 500ms) = %d, %s; want 2, 200ms", steps, rem)
	}
}

func TestGridAnchors(t *testing.T) {
	t.Parallel()
	wantSteps, _ := wideStepsAndRem(t)
	for _, tc := range []struct {
		name       string
		start, end time.Time
		step       time.Duration
		want       int64
	}{
		{"beyond a duration", wideStart, wideEnd, wideStep, wantSteps + 1},
		{"one step", time.Unix(0, 0), time.Unix(60, 0), time.Minute, 2},
		{"start unset", time.Time{}, wideEnd, wideStep, 0},
		{"end unset", wideStart, time.Time{}, wideStep, 0},
		{"no step", wideStart, wideEnd, 0, 0},
	} {
		if got := GridAnchors(tc.start, tc.end, tc.step); got != tc.want {
			t.Errorf("%s: GridAnchors = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestSpanSteps(t *testing.T) {
	t.Parallel()
	wantSteps, wantRem := wideStepsAndRem(t)
	saturated := wideEnd.Sub(wideStart)
	if steps, rem := SpanSteps(saturated, wideStart, wideEnd, wideStep); steps != wantSteps || rem != wantRem {
		t.Errorf("saturated span: SpanSteps = %d, %s; want %d, %s", steps, rem, wantSteps, wantRem)
	}
	// Unset bounds leave the outer range the only measure, as for a
	// subquery-inner window before re-anchoring.
	if steps, rem := SpanSteps(150*time.Second, time.Time{}, time.Time{}, time.Minute); steps != 2 || rem != 30*time.Second {
		t.Errorf("unset bounds: SpanSteps = %d, %s; want 2, 30s", steps, rem)
	}
	// An outer range that fits is the span even where it differs from
	// End − Start: a step-aligned subquery's window is not its bounds.
	start := time.Unix(0, 0)
	if steps, _ := SpanSteps(5*time.Minute, start, start.Add(time.Hour), time.Minute); steps != 5 {
		t.Errorf("fitting outer range: SpanSteps = %d, want 5", steps)
	}
}

func TestGridInstant_BeyondDuration(t *testing.T) {
	t.Parallel()
	steps, rem := wideStepsAndRem(t)
	if got := GridInstant(wideStart, wideStep, steps); !got.Equal(wideEnd.Add(-rem)) {
		t.Errorf("forward: GridInstant = %s, want %s", got, wideEnd.Add(-rem))
	}
	if got := GridInstant(wideEnd, -wideStep, steps); !got.Equal(wideStart.Add(rem)) {
		t.Errorf("backward: GridInstant = %s, want %s", got, wideStart.Add(rem))
	}
	if got := GridInstant(time.Unix(10, 900_000_000), -700*time.Millisecond, 3); !got.Equal(time.Unix(8, 800_000_000)) {
		t.Errorf("sub-second backward: GridInstant = %s, want 8.8s", got)
	}
}

func TestNumAnchors_BeyondDuration(t *testing.T) {
	t.Parallel()
	wantSteps, wantRem := wideStepsAndRem(t)
	outer := wideEnd.Sub(wideStart)
	rw := &RangeWindow{Start: wideStart, End: wideEnd, Step: wideStep, OuterRange: outer}
	if got := rw.NumAnchors(); got != wantSteps+1 {
		t.Errorf("RangeWindow.NumAnchors = %d, want %d", got, wantSteps+1)
	}
	if steps, rem := rw.OuterSteps(); steps != wantSteps || rem != wantRem {
		t.Errorf("RangeWindow.OuterSteps = %d, %s; want %d, %s", steps, rem, wantSteps, wantRem)
	}
	outerMode := &RangeBucketFanout{Start: wideStart, End: wideEnd, Step: wideStep, OuterRange: outer}
	if got := outerMode.NumAnchors(); got != wantSteps+1 {
		t.Errorf("RangeBucketFanout.NumAnchors (outer range) = %d, want %d", got, wantSteps+1)
	}
	gridMode := &RangeBucketFanout{Start: wideStart, End: wideEnd, Step: wideStep}
	if got := gridMode.NumAnchors(); got != wantSteps+1 {
		t.Errorf("RangeBucketFanout.NumAnchors (grid) = %d, want %d", got, wantSteps+1)
	}
	native := &RangeBucketGridNative{Start: wideStart, End: wideEnd, Step: wideStep}
	if got := native.NumAnchors(); got != wantSteps+1 {
		t.Errorf("RangeBucketGridNative.NumAnchors = %d, want %d", got, wantSteps+1)
	}
	lwr := &RangeLWR{Start: wideStart, End: wideEnd, Step: wideStep}
	if got := lwr.NumAnchors(); got != wantSteps+1 {
		t.Errorf("RangeLWR.NumAnchors = %d, want %d", got, wantSteps+1)
	}
}
