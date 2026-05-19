package score

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCompute_FullyPassing(t *testing.T) {
	t.Parallel()
	s := Compute("tempo compat", 10, 10)
	if s.Color != "brightgreen" {
		t.Fatalf("100%% should be brightgreen, got %q", s.Color)
	}
	if s.Percent != 100.00 {
		t.Fatalf("100%% should be 100.00, got %v", s.Percent)
	}
	if s.Message != "100.00%" {
		t.Fatalf("message = %q, want %q", s.Message, "100.00%")
	}
	if s.SchemaVersion != 1 {
		t.Fatalf("schemaVersion = %d, want 1", s.SchemaVersion)
	}
	if s.Label != "tempo compat" {
		t.Fatalf("label = %q", s.Label)
	}
	if s.Passed != 10 || s.Total != 10 {
		t.Fatalf("passed/total = %d/%d", s.Passed, s.Total)
	}
}

func TestCompute_ColorBands(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		passed, tot int
		wantColor   string
		wantPercent float64
	}{
		{"100%", 10, 10, "brightgreen", 100.00},
		{"99%", 99, 100, "green", 99.00},
		{"95%", 95, 100, "green", 95.00},
		{"94.99%", 9499, 10000, "yellowgreen", 94.99},
		{"80%", 80, 100, "yellowgreen", 80.00},
		{"79.99%", 7999, 10000, "yellow", 79.99},
		{"60%", 60, 100, "yellow", 60.00},
		{"59.99%", 5999, 10000, "orange", 59.99},
		{"40%", 40, 100, "orange", 40.00},
		{"39.99%", 3999, 10000, "red", 39.99},
		{"0%", 0, 10, "red", 0.00},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := Compute("x", tc.passed, tc.tot)
			if s.Color != tc.wantColor {
				t.Fatalf("color = %q, want %q (passed=%d total=%d)", s.Color, tc.wantColor, tc.passed, tc.tot)
			}
			if s.Percent != tc.wantPercent {
				t.Fatalf("percent = %v, want %v", s.Percent, tc.wantPercent)
			}
		})
	}
}

func TestCompute_EmptyCorpusForcesRed(t *testing.T) {
	t.Parallel()
	// Total == 0 must not divide-by-zero AND must not flash brightgreen
	// (which "100% of nothing" would compute to under naïve math).
	s := Compute("x", 0, 0)
	if s.Color != "red" {
		t.Fatalf("empty corpus should be red, got %q", s.Color)
	}
	if s.Percent != 0 {
		t.Fatalf("empty corpus percent should be 0, got %v", s.Percent)
	}
	if s.Total != 0 {
		t.Fatalf("total = %d, want 0", s.Total)
	}
}

func TestCompute_RoundsToTwoDecimals(t *testing.T) {
	t.Parallel()
	// 2/3 = 66.6666...; we want 66.67 (round half-up).
	s := Compute("x", 2, 3)
	if s.Percent != 66.67 {
		t.Fatalf("percent = %v, want 66.67", s.Percent)
	}
	if s.Message != "66.67%" {
		t.Fatalf("message = %q, want 66.67%%", s.Message)
	}
}

func TestWrite_RoundTripsThroughJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "compat-score.json")

	in := Compute("loki compat", 17, 20)
	if err := Write(path, in); err != nil {
		t.Fatalf("Write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	// Trailing newline check — required for clean diffs.
	if len(data) == 0 || data[len(data)-1] != '\n' {
		t.Fatalf("score JSON should end with newline; got tail %q", string(data[max(0, len(data)-4):]))
	}

	var out Score
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Fatalf("round trip mismatch:\n  in  = %+v\n  out = %+v", in, out)
	}

	// Verify the shields.io contract keys are present at the top level.
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	for _, k := range []string{"schemaVersion", "label", "message", "color"} {
		if _, ok := raw[k]; !ok {
			t.Fatalf("missing shields key %q in JSON output", k)
		}
	}
}

func TestWrite_OverwritesExisting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "compat-score.json")

	if err := Write(path, Compute("x", 1, 10)); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := Write(path, Compute("x", 10, 10)); err != nil {
		t.Fatalf("second write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var out Score
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Passed != 10 || out.Total != 10 || out.Color != "brightgreen" {
		t.Fatalf("second write didn't overwrite; got %+v", out)
	}
}

func TestColorFor_BoundaryExactness(t *testing.T) {
	t.Parallel()
	// Exact threshold percents land on the lower-band side
	// (inclusive lower, exclusive upper).
	if got := colorFor(95.0, 100); got != "green" {
		t.Fatalf("95.0 -> %q, want green", got)
	}
	if got := colorFor(94.999999, 100); got != "yellowgreen" {
		// Strictly less than 95 by float; we accept that a "drift" near
		// the band edge would round visually to 95 in the badge message
		// but still classify yellowgreen on the underlying value.
		t.Fatalf("94.999999 -> %q, want yellowgreen", got)
	}
	if got := colorFor(100.0, 100); got != "brightgreen" {
		t.Fatalf("100.0 -> %q, want brightgreen", got)
	}
}
