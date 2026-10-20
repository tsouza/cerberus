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

// TestStepsBetween_FastPathAgreesWithBig pins that the int64 path answers
// what the arbitrary-precision path answers, on both sides of the longest
// distance a time.Duration holds and for negative distances.
func TestStepsBetween_FastPathAgreesWithBig(t *testing.T) {
	t.Parallel()
	origin := time.Date(1900, 1, 1, 0, 0, 0, 123, time.UTC)
	longest := time.Duration(math.MaxInt64)
	steps := []time.Duration{1, time.Second, 7 * time.Minute, wideStep, longest}
	distances := []time.Duration{
		0, 1, -1, time.Second - 1, -time.Second + 1, 90 * 24 * time.Hour, -90 * 24 * time.Hour,
		longest - 1, longest, -longest, math.MinInt64,
	}
	for _, step := range steps {
		for _, d := range distances {
			end := origin.Add(d)
			gotSteps, gotRem := StepsBetween(origin, end, step)
			wantSteps, wantRem := stepsBetweenBig(origin, end, step)
			if gotSteps != wantSteps || gotRem != wantRem {
				t.Errorf("StepsBetween(+%v, step %v) = (%d, %v), big path (%d, %v)",
					d, step, gotSteps, gotRem, wantSteps, wantRem)
			}
		}
		// One nanosecond past the longest Duration, and the 328-year span,
		// take the big path; the answer is still the exact one.
		for _, end := range []time.Time{origin.Add(longest).Add(1), origin.Add(-longest).Add(-2), wideEnd} {
			start := origin
			if end.Equal(wideEnd) {
				start = wideStart
			}
			gotSteps, gotRem := StepsBetween(start, end, step)
			wantSteps, wantRem := stepsBetweenBig(start, end, step)
			if gotSteps != wantSteps || gotRem != wantRem {
				t.Errorf("StepsBetween(%v, %v, step %v) = (%d, %v), big path (%d, %v)",
					start, end, step, gotSteps, gotRem, wantSteps, wantRem)
			}
		}
	}
}

// TestGridInstant_FastPathAgreesWithBig pins that the int64 path answers
// what the arbitrary-precision path answers, for k up to and one past the
// largest multiple of step a time.Duration holds, forwards and backwards.
func TestGridInstant_FastPathAgreesWithBig(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 1, 0, 0, 0, 500, time.UTC)
	for _, step := range []time.Duration{time.Nanosecond, time.Second, wideStep, -time.Minute, -wideStep} {
		maxK := int64(time.Duration(math.MaxInt64) / max(step, -step))
		for _, k := range []int64{0, 1, -1, 1000, -1000, maxK - 1, maxK, -maxK, maxK + 1, -maxK - 1} {
			got := GridInstant(start, step, k)
			want := gridInstantBig(start, step, k)
			if !got.Equal(want) || got.Location() != want.Location() {
				t.Errorf("GridInstant(step %v, k %d) = %v, big path %v", step, k, got, want)
			}
		}
	}
	if got := GridInstant(start, 0, 5); !got.Equal(start) {
		t.Errorf("GridInstant(step 0) = %v, want start %v", got, start)
	}
}

// TestSpanHelpers_InDurationDoNotAllocate pins that a span or offset a
// time.Duration holds is computed without the big.Int path's allocations.
func TestSpanHelpers_InDurationDoNotAllocate(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(90 * 24 * time.Hour)
	if n := testing.AllocsPerRun(100, func() { StepsBetween(start, end, time.Minute) }); n != 0 {
		t.Errorf("StepsBetween within a Duration allocated %v times per call, want 0", n)
	}
	if n := testing.AllocsPerRun(100, func() { GridInstant(end, -time.Minute, 1000) }); n != 0 {
		t.Errorf("GridInstant within a Duration allocated %v times per call, want 0", n)
	}
}
