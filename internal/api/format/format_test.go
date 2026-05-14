package format_test

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/format"
)

func TestCanonicalKey(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]string
		want string
	}{
		{"empty", nil, ""},
		{"empty-map", map[string]string{}, ""},
		{
			"single",
			map[string]string{"job": "api"},
			"job=api\x00",
		},
		{
			"sorted",
			map[string]string{"z": "1", "a": "2", "m": "3"},
			"a=2\x00m=3\x00z=1\x00",
		},
		{
			"determinism-vs-insertion-order",
			map[string]string{"b": "2", "a": "1"},
			"a=1\x00b=2\x00",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := format.CanonicalKey(tc.in)
			if got != tc.want {
				t.Fatalf("CanonicalKey(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCanonicalKeyStable(t *testing.T) {
	// Two maps built with different insertion orders must produce the
	// same key — this is the property every handler relied on.
	a := map[string]string{"x": "1", "y": "2", "z": "3"}
	b := map[string]string{"z": "3", "x": "1", "y": "2"}
	if format.CanonicalKey(a) != format.CanonicalKey(b) {
		t.Fatalf("CanonicalKey not stable across insertion order")
	}
}

func TestWithMetricName(t *testing.T) {
	in := map[string]string{"job": "api"}
	out := format.WithMetricName(in, "http_requests_total")
	if out["__name__"] != "http_requests_total" {
		t.Fatalf("missing __name__: %v", out)
	}
	if out["job"] != "api" {
		t.Fatalf("missing job: %v", out)
	}
	// The original must not be mutated.
	if _, ok := in["__name__"]; ok {
		t.Fatalf("input mutated: %v", in)
	}
}

func TestWithMetricNameEmptyName(t *testing.T) {
	out := format.WithMetricName(map[string]string{"a": "b"}, "")
	if _, ok := out["__name__"]; ok {
		t.Fatalf("__name__ added on empty name: %v", out)
	}
	if out["a"] != "b" {
		t.Fatalf("missing copied label: %v", out)
	}
}

func TestParseDuration(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
		err  bool
	}{
		{"", 0, true},
		{"5m", 5 * time.Minute, false},
		{"30s", 30 * time.Second, false},
		{"60", 60 * time.Second, false},
		{"60.5", 60_500 * time.Millisecond, false},
		{"abc", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := format.ParseDuration(tc.in)
			if tc.err {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseTimeProm(t *testing.T) {
	def := time.Unix(1_000, 0).UTC()
	tests := []struct {
		name string
		raw  string
		want time.Time
		err  bool
	}{
		{"empty-defaults", "", def, false},
		{"unix-seconds", "1700000000", time.Unix(1_700_000_000, 0).UTC(), false},
		{"unix-fractional", "1700000000.5", time.Unix(1_700_000_000, 500_000_000).UTC(), false},
		{"rfc3339", "2024-01-02T03:04:05Z", time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC), false},
		{"bogus", "not-a-time", time.Time{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := format.ParseTimeProm(tc.raw, def)
			if tc.err {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !got.Equal(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseTimeLoki(t *testing.T) {
	def := time.Unix(1_000, 0).UTC()
	tests := []struct {
		name string
		raw  string
		want time.Time
		err  bool
	}{
		{"empty-defaults", "", def, false},
		{"unix-seconds", "1700000000", time.Unix(1_700_000_000, 0).UTC(), false},
		{"unix-nanos", "1700000000000000000", time.Unix(0, 1_700_000_000_000_000_000).UTC(), false},
		{"rfc3339", "2024-01-02T03:04:05Z", time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC), false},
		{"bogus", "not-a-time", time.Time{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := format.ParseTimeLoki(tc.raw, def)
			if tc.err {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !got.Equal(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCrossHandlerCanonicalKey verifies the shared CanonicalKey
// produces the same output that each handler's private copy did
// before the extraction. The expected values below are byte-identical
// to what prom / loki / tempo computed previously.
func TestCrossHandlerCanonicalKey(t *testing.T) {
	labels := map[string]string{
		"__name__": "http_requests_total",
		"job":      "api",
		"instance": "10.0.0.1:9090",
	}
	want := "__name__=http_requests_total\x00instance=10.0.0.1:9090\x00job=api\x00"
	if got := format.CanonicalKey(labels); got != want {
		t.Fatalf("CanonicalKey diverged from pre-extraction output:\n got: %q\nwant: %q", got, want)
	}
}
