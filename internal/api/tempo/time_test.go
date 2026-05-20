package tempo

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestParseTempoTime covers the three integer magnitudes plus float
// seconds, RFC3339, and the empty / bogus paths. The 1e12..1e15 ms
// branch is the #194 fix: Grafana 11.x's Tempo datasource sends ms
// timestamps over `/api/datasources/uid/<ds>/resources/...`, and the
// old `>1e12 → ns` heuristic decoded them as ns → year ~58353 → CH
// `toDateTime64` overflow → 500 response → empty Grafana panels.
func TestParseTempoTime(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want time.Time
		err  bool
	}{
		{"empty-zero", "", time.Time{}, false},
		{"unix-seconds", "1700000000", time.Unix(1_700_000_000, 0).UTC(), false},
		{"unix-fractional", "1700000000.5", time.Unix(1_700_000_000, 500_000_000).UTC(), false},
		{"rfc3339", "2024-01-02T03:04:05Z", time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC), false},

		// #194: 1e12..1e15 → ms (Grafana resources proxy).
		{"unix-millis-13digit-boundary", "1000000000000", time.UnixMilli(1_000_000_000_000).UTC(), false},
		{"unix-millis-grafana-shape", "1737000000000", time.UnixMilli(1_737_000_000_000).UTC(), false},
		{"unix-millis-with-frac", "1700000000123", time.UnixMilli(1_700_000_000_123).UTC(), false},
		{"unix-millis-1_5e12", "1500000000000", time.UnixMilli(1_500_000_000_000).UTC(), false},

		// Below 1e12 stays in the seconds branch (year ~33658 in
		// seconds would be absurd; clients never send that).
		{"boundary-1e12-minus-1-stays-seconds", "999999999999", time.Unix(999_999_999_999, 0).UTC(), false},

		// >=1e15 → ns (tempo-vulture / ns-native plugin shape).
		{"unix-nanos-1e15-boundary", "1000000000000000", time.Unix(0, 1_000_000_000_000_000).UTC(), false},
		{"unix-nanos-vulture-shape", "1700000000000000000", time.Unix(0, 1_700_000_000_000_000_000).UTC(), false},
		{"unix-nanos-2e18", "2000000000000000000", time.Unix(0, 2_000_000_000_000_000_000).UTC(), false},

		{"bogus", "not-a-time", time.Time{}, true},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseTempoTime(tc.raw)
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

// TestParseTempoStartEnd_GrafanaMs is the request-level companion to
// TestParseTempoTime: an HTTP request with 13-digit ms `start` / `end`
// (the Grafana 11.x resources-proxy wire shape) must decode to the
// expected UTC times — not the year-58353 garbage the old `>1e12 → ns`
// heuristic produced.
func TestParseTempoStartEnd_GrafanaMs(t *testing.T) {
	t.Parallel()

	// Pick a recent-ish point: 2025-01-26 ≈ 1_737_864_000_000 ms.
	const startMs = 1_737_000_000_000
	const endMs = 1_737_864_000_000

	r := httptest.NewRequest(http.MethodGet,
		"/api/search?start=1737000000000&end=1737864000000", nil)

	start, end, err := parseTempoStartEnd(r)
	if err != nil {
		t.Fatalf("parseTempoStartEnd: %v", err)
	}
	wantStart := time.UnixMilli(startMs).UTC()
	wantEnd := time.UnixMilli(endMs).UTC()
	if !start.Equal(wantStart) {
		t.Fatalf("start: got %v, want %v", start, wantStart)
	}
	if !end.Equal(wantEnd) {
		t.Fatalf("end: got %v, want %v", end, wantEnd)
	}
	// And the cardinal: the decoded year must be 2025, not 58353
	// (which is what 1.737e12 interpreted as ns gives).
	if y := start.Year(); y != 2025 {
		t.Fatalf("start year: got %d, want 2025 — ms→ns misroute regression", y)
	}
}

// TestParseTempoStartEnd_LogcliNanos pins the ns branch. The 19-digit
// shape must still decode to its original ns instant (this is the
// `tempo-vulture` / ns-native plugin wire).
func TestParseTempoStartEnd_LogcliNanos(t *testing.T) {
	t.Parallel()
	const startNs = 1_700_000_000_000_000_000
	const endNs = 1_700_000_060_000_000_000

	r := httptest.NewRequest(http.MethodGet,
		"/api/search?start=1700000000000000000&end=1700000060000000000", nil)

	start, end, err := parseTempoStartEnd(r)
	if err != nil {
		t.Fatalf("parseTempoStartEnd: %v", err)
	}
	if got := start.UnixNano(); got != startNs {
		t.Fatalf("start ns: got %d, want %d", got, startNs)
	}
	if got := end.UnixNano(); got != endNs {
		t.Fatalf("end ns: got %d, want %d", got, endNs)
	}
}
