package format_test

import (
	"math"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/format"
)

// minStorable / maxStorable are the edges of the window a timestamp can
// survive end to end: an int64 nanosecond count is what
// time.Time.UnixNano() (every scan bound goes through it) and a
// ClickHouse DateTime64(9) column both hold. Spelled here from
// math.MinInt64 / math.MaxInt64 rather than imported from the package
// under test, so the assertions below pin the contract rather than
// whatever the implementation currently computes.
var (
	minStorable = time.Unix(0, math.MinInt64).UTC()
	maxStorable = time.Unix(0, math.MaxInt64).UTC()
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
		// All three references reject a float-seconds duration whose
		// nanosecond product overflows int64 rather than wrapping it:
		// Prometheus web/api/v1/api.go:2273-2279, Loki
		// pkg/loghttp/params.go:202-209, Tempo pkg/api/http.go:661-668.
		// Unguarded, `step=1e30` became an implementation-defined
		// (negative) Duration and the step grid was built from it.
		{"1e30", 0, true},
		{"-1e30", 0, true},
		{"NaN", 0, true},
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
		// Grafana's Prom datasource sends ms over `resources/` proxy.
		// Routing >= 1e12 to the ms branch protects every Prom handler
		// from the toDateTime64('58353-12-31', 9) overflow.
		{"unix-millis-13digit", "1700000000000", time.Unix(1_700_000_000, 0).UTC(), false},
		{"unix-millis-with-frac", "1700000000123", time.Unix(1_700_000_000, 123_000_000).UTC(), false},
		// Scale routing is unchanged — < 1e12 is still seconds — but a
		// seconds value that lands past the storable window is pinned to
		// the window's edge. 999999999999s is year 33658, which reached
		// ClickHouse as toDateTime64('33658-…', 9) and came back a 502.
		// Prometheus parses the same input (parseTime calls ParseFloat
		// first and unconditionally, web/api/v1/api.go:2249-2254) and
		// answers 200 with no samples; the clamp is what reproduces that.
		{"boundary-1e12-minus-1-clamps", "999999999999", maxStorable, false},
		{"largest-unclamped-seconds", "9223372036", time.Unix(9_223_372_036, 0).UTC(), false},
		{"boundary-1e12-exact", "1000000000000", time.UnixMilli(1_000_000_000_000).UTC(), false},
		// Prometheus does NOT reject these: ParseFloat accepts both, so
		// the reference answers 200-empty rather than 400. Cerberus must
		// too, which means clamping rather than overflowing the
		// float64 → int64 conversion.
		{"exponent-float-overflows-int64", "1e19", maxStorable, false},
		{"integer-too-large-for-int64", "10000000000000000000", maxStorable, false},
		{"negative-exponent-float", "-1e19", minStorable, false},
		{"nan", "NaN", minStorable, false},
		{"rfc3339-past-storable-window", "9999-01-01T00:00:00Z", maxStorable, false},
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

func TestParseTimeUnixScaled(t *testing.T) {
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
		// #194: Grafana 11.x emits ms over the Loki `resources/` proxy
		// (just like Prom). A 13-digit ms value used to land in the ns
		// branch and overflow ClickHouse's toDateTime64; the parser
		// now routes 1e12..1e15 to time.UnixMilli.
		{"unix-millis-13digit-boundary", "1000000000000", time.UnixMilli(1_000_000_000_000).UTC(), false},
		{"unix-millis-grafana-shape", "1737000000000", time.UnixMilli(1_737_000_000_000).UTC(), false},
		{"unix-millis-with-frac", "1700000000123", time.UnixMilli(1_700_000_000_123).UTC(), false},
		{"unix-millis-1_5e12", "1500000000000", time.UnixMilli(1_500_000_000_000).UTC(), false},
		// Scale routing still says "seconds" below 1e12; the value is
		// then pinned to the storable window's edge (see maxStorable).
		{"boundary-1e12-minus-1-clamps", "999999999999", maxStorable, false},
		{"largest-unclamped-seconds", "9223372036", time.Unix(9_223_372_036, 0).UTC(), false},
		// `logcli` and other ns-native Loki clients keep working: the
		// >=1e15 branch still routes to time.Unix(0, n).
		{"unix-nanos-1e15-boundary", "1000000000000000", time.Unix(0, 1_000_000_000_000_000).UTC(), false},
		{"unix-nanos-logcli-shape", "1700000000000000000", time.Unix(0, 1_700_000_000_000_000_000).UTC(), false},
		{"unix-nanos-2e18", "2000000000000000000", time.Unix(0, 2_000_000_000_000_000_000).UTC(), false},
		// Reference Loki (pkg/loghttp/params.go:168-174) and reference
		// Tempo (pkg/api/http.go:631-637) reach ParseFloat only when the
		// value contains a decimal point. A bare integer too large for
		// int64 therefore falls to strconv.ParseInt (which rejects it)
		// and then to RFC3339 (which rejects it) — a 400 on both. Before
		// the gate, cerberus took the ungated float branch and answered
		// 200 over a window the client never asked for.
		{"integer-too-large-for-int64-rejected", "10000000000000000000", time.Time{}, true},
		{"exponent-without-decimal-point-rejected", "1e19", time.Time{}, true},
		{"exponent-in-range-without-point-rejected", "1e9", time.Time{}, true},
		// With a decimal point the float branch is live on both
		// references, so cerberus keeps it — clamped, not overflowed.
		{"huge-float-with-decimal-point-clamps", "10000000000000000000.0", maxStorable, false},
		{"negative-huge-float-with-decimal-point-clamps", "-10000000000000000000.0", minStorable, false},
		{"rfc3339-past-storable-window", "9999-01-01T00:00:00Z", maxStorable, false},
		{"bogus", "not-a-time", time.Time{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := format.ParseTimeUnixScaled(tc.raw, def)
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
