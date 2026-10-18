package chsql

import (
	"errors"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestRangeWindowGridAnchorCount pins the anchor count every windowed
// fan-out shares: a span needs both of its ends, so a window with only one
// of them set is the single instant anchor, not a span reaching back to the
// zero time or forward from it.
func TestRangeWindowGridAnchorCount(t *testing.T) {
	t.Parallel()
	start := time.Unix(1_700_000_000, 0)
	const step = 15 * time.Second
	const stepsInSpan = 4
	end := start.Add(stepsInSpan * step)
	for _, tc := range []struct {
		name string
		rw   chplan.RangeWindow
		want int64
	}{
		{"outer range", chplan.RangeWindow{OuterRange: stepsInSpan * step, Start: start}, stepsInSpan + 1},
		{"start and end", chplan.RangeWindow{Start: start, End: end}, stepsInSpan + 1},
		{"start only", chplan.RangeWindow{Start: start}, 1},
		{"end only", chplan.RangeWindow{End: end}, 1},
		{"neither", chplan.RangeWindow{}, 1},
	} {
		got, err := rangeWindowGridAnchorCount(&tc.rw, step.Nanoseconds())
		if err != nil || got != tc.want {
			t.Errorf("%s: rangeWindowGridAnchorCount = %d, %v; want %d, nil", tc.name, got, err, tc.want)
		}
	}
	reversed := chplan.RangeWindow{Start: end, End: start}
	if _, err := rangeWindowGridAnchorCount(&reversed, step.Nanoseconds()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Start > End: err = %v; want ErrUnsupported", err)
	}
}
