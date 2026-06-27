package loki

import "testing"

// TestParsePositiveInt31 pins the param contract the metadata-peek endpoints
// rely on: empty -> default; valid -> itself; above max -> clamped DOWN to max
// (never an error, mirroring parseLogLimit); invalid / zero / >2^31-1 ->
// rejected. The clamp is the bound that stops an absurd line_limit from
// emitting an unbounded SQL LIMIT (#1109-class metadata-drain OOM).
func TestParsePositiveInt31(t *testing.T) {
	t.Parallel()
	const def, max = 1000, 10_000
	cases := []struct {
		name    string
		raw     string
		want    int
		wantErr bool
	}{
		{"empty_returns_default", "", def, false},
		{"within_range_passthrough", "500", 500, false},
		{"at_max_passthrough", "10000", max, false},
		{"above_max_clamped", "2000000000", max, false},
		{"just_above_max_clamped", "10001", max, false},
		{"zero_rejected", "0", 0, true},
		{"negative_rejected", "-1", 0, true},
		{"overflow_int31_rejected", "2147483648", 0, true},
		{"non_numeric_rejected", "abc", 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parsePositiveInt31(c.raw, def, max)
			if (err != nil) != c.wantErr {
				t.Fatalf("parsePositiveInt31(%q) err=%v, wantErr=%v", c.raw, err, c.wantErr)
			}
			if !c.wantErr && got != c.want {
				t.Errorf("parsePositiveInt31(%q) = %d, want %d", c.raw, got, c.want)
			}
		})
	}
}
