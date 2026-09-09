package tempo

import (
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
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

		// Below 1e12 still routes to the seconds branch, but the
		// resulting time is pinned to the last instant a scan bound can
		// represent: tsScanBound renders t.UnixNano(), and 999999999999
		// seconds is year 33658, far past the int64-nanosecond ceiling
		// that both UnixNano() and the DateTime64(9) Timestamp column
		// top out at. Unpinned, UnixNano() wrapped and the search ran
		// over a window nobody asked for.
		{"boundary-1e12-minus-1-clamps", "999999999999", time.Unix(0, math.MaxInt64).UTC(), false},
		{"largest-unclamped-seconds", "9223372036", time.Unix(9_223_372_036, 0).UTC(), false},
		{"rfc3339-past-storable-window", "9999-01-01T00:00:00Z", time.Unix(0, math.MaxInt64).UTC(), false},

		// Reference Tempo reaches strconv.ParseFloat only for a value
		// containing a decimal point (its `parseTimestamp` in
		// pkg/api/http.go); a bare
		// integer wider than int64 falls to ParseInt, then RFC3339, and
		// is rejected — `start=10000000000000000000` is a 400 there.
		{"integer-too-large-for-int64-rejected", "10000000000000000000", time.Time{}, true},
		{"exponent-without-decimal-point-rejected", "1e19", time.Time{}, true},

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

// TestTempoScanBoundNeverWraps is the reason the parser clamps rather
// than passing an out-of-range time through. tsScanBound renders
// `Timestamp <op> fromUnixTimestamp64Nano(<t.UnixNano()>)`, and
// time.Time.UnixNano() is documented as undefined outside roughly
// 1678–2262: past that edge it silently WRAPS, so a request for "up to
// year 33658" became a scan bound in the distant past (or, as an
// unsigned nanosecond count, the far future) and the handler answered
// 200 over a window the client never asked for — a wrong answer, not a
// wrong status code.
//
// The invariant asserted here is round-tripping: whatever time the
// parser hands back, re-reading the emitted nanosecond literal must
// reproduce it exactly. That holds for every in-range timestamp and can
// only hold at the edges if the parser pinned them there.
func TestTempoScanBoundNeverWraps(t *testing.T) {
	t.Parallel()

	// Inputs a client can actually put on the wire that decode to a
	// timestamp at or beyond the representable edge.
	raws := []string{
		"999999999999",         // seconds → year 33658
		"9999-01-01T00:00:00Z", // RFC3339 → year 9999
		"1700000000",           // control: an ordinary in-range seconds value
		"1700000000000000000",  // control: an ordinary in-range ns value
	}
	for _, raw := range raws {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			parsed, err := parseTempoTime(raw)
			if err != nil {
				t.Fatalf("parseTempoTime(%q): %v", raw, err)
			}
			bound, ok := tsScanBound("Timestamp", chplan.OpLe, parsed).(*chplan.Binary)
			if !ok {
				t.Fatalf("tsScanBound did not return a *chplan.Binary")
			}
			call, ok := bound.Right.(*chplan.FuncCall)
			if !ok || len(call.Args) != 1 {
				t.Fatalf("tsScanBound rhs = %#v, want a 1-arg FuncCall", bound.Right)
			}
			lit, ok := call.Args[0].(*chplan.LitInt)
			if !ok {
				t.Fatalf("tsScanBound nanosecond arg = %#v, want *chplan.LitInt", call.Args[0])
			}
			if got := time.Unix(0, lit.V).UTC(); !got.Equal(parsed) {
				t.Fatalf("scan bound wrapped: parsed %v rendered as %d ns, which reads back as %v",
					parsed, lit.V, got)
			}
		})
	}
}
