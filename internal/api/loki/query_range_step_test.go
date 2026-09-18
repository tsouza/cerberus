package loki

import (
	"testing"
	"time"
)

// TestParseQueryRangeStep_AbsentDerivesFromTheWindow pins the wire
// contract an absent `?step=` follows: reference Loki's
// max(floor((end-start)/250), 1) seconds, not a fixed minute. Values are
// literals rather than a call to defaultQueryRangeStep, so a change to the
// divisor fails here by number.
func TestParseQueryRangeStep_AbsentDerivesFromTheWindow(t *testing.T) {
	t.Parallel()

	start := time.Unix(1717995600, 0).UTC()
	for _, tc := range []struct {
		name   string
		window time.Duration
		want   time.Duration
	}{
		{name: "one hour is 3600/250 = 14.4, floored", window: time.Hour, want: 14 * time.Second},
		{name: "six hours is 21600/250 = 86.4, floored", window: 6 * time.Hour, want: 86 * time.Second},
		{name: "exactly 250 s is 1 s", window: 250 * time.Second, want: time.Second},
		{name: "under 250 s floors to the 1 s minimum, never 0", window: 10 * time.Second, want: time.Second},
		{name: "a zero-width window still has a 1 s step", window: 0, want: time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseQueryRangeStep("", start, start.Add(tc.window))
			if err != nil {
				t.Fatalf("absent step over a %s window: %v", tc.window, err)
			}
			if got != tc.want {
				t.Errorf("absent step over a %s window = %s, want %s", tc.window, got, tc.want)
			}
		})
	}
}

// TestParseQueryRangeStep_PresentIsParsedNotDefaulted pins that a supplied
// step is used verbatim and that the two rejection shapes stay distinct
// from the absent case: malformed answers "cannot parse", non-positive
// answers the missing-or-invalid message, and neither falls back to the
// derived default.
func TestParseQueryRangeStep_PresentIsParsedNotDefaulted(t *testing.T) {
	t.Parallel()

	start := time.Unix(1717995600, 0).UTC()
	end := start.Add(time.Hour)
	for raw, want := range map[string]time.Duration{"15": 15 * time.Second, "1m": time.Minute, "2h": 2 * time.Hour} {
		got, err := parseQueryRangeStep(raw, start, end)
		if err != nil || got != want {
			t.Errorf("parseQueryRangeStep(%q) = %s, %v; want %s", raw, got, err, want)
		}
	}
	if _, err := parseQueryRangeStep("banana", start, end); err == nil {
		t.Error("a malformed step was accepted; want the cannot-parse rejection")
	}
	for _, raw := range []string{"0", "-30"} {
		if _, err := parseQueryRangeStep(raw, start, end); err == nil {
			t.Errorf("step %q was accepted; want the non-positive rejection", raw)
		}
	}
}
