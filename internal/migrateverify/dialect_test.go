package migrateverify

import (
	"strconv"
	"testing"
	"time"
)

// upstreamIntegerInstantDigits is the digit count at or below which upstream Loki
// and upstream Tempo read an INTEGER start/end as Unix SECONDS; above it they read
// nanoseconds. Reimplemented here (rather than imported) so the test pins the
// external contract the encoder must survive, not cerberus's own reading of it.
const upstreamIntegerInstantDigits = 10

// TestPromDialect_Encode pins that the Prometheus lane's wire encoding is
// unchanged: the endpoint, the `query` parameter name, Unix-seconds instants, and
// float-seconds step. A drift here silently changes every existing invocation.
func TestPromDialect_Encode(t *testing.T) {
	d := PromDialect()
	if d.Name() != HeadProm {
		t.Errorf("Name() = %q, want %q", d.Name(), HeadProm)
	}
	if d.Path() != "/api/v1/query_range" {
		t.Errorf("Path() = %q, want /api/v1/query_range", d.Path())
	}
	p := testParams()
	got := d.Encode("up", p)
	if got.Get("query") != "up" {
		t.Errorf("query = %q, want up", got.Get("query"))
	}
	if want := strconv.FormatInt(p.Start.Unix(), 10); got.Get("start") != want {
		t.Errorf("start = %q, want Unix seconds %q", got.Get("start"), want)
	}
	if want := strconv.FormatInt(p.End.Unix(), 10); got.Get("end") != want {
		t.Errorf("end = %q, want Unix seconds %q", got.Get("end"), want)
	}
	if got.Get("step") != "60" {
		t.Errorf("step = %q, want 60", got.Get("step"))
	}
}

// TestLokiDialect_Encode pins the Loki lane's wire contract: the Loki endpoint
// path, the `query` parameter, RFC3339Nano instants that parse BACK to the exact
// requested window, and the step. The instants are asserted by re-parsing rather
// than by string comparison, so the test cannot pass by rebuilding the encoder's
// own formatting mistake.
func TestLokiDialect_Encode(t *testing.T) {
	d := LokiDialect()
	if d.Name() != HeadLoki {
		t.Errorf("Name() = %q, want %q", d.Name(), HeadLoki)
	}
	if d.Path() != "/loki/api/v1/query_range" {
		t.Errorf("Path() = %q, want /loki/api/v1/query_range", d.Path())
	}
	p := testParams()
	got := d.Encode(`sum(rate({job="api"}[5m]))`, p)
	if got.Get("query") != `sum(rate({job="api"}[5m]))` {
		t.Errorf("query = %q, want the LogQL expression verbatim", got.Get("query"))
	}
	assertRFC3339Instant(t, "start", got.Get("start"), p.Start)
	assertRFC3339Instant(t, "end", got.Get("end"), p.End)
	if got.Get("step") != "60" {
		t.Errorf("step = %q, want 60", got.Get("step"))
	}
	// limit/direction steer the log-stream branch this lane never reaches.
	if got.Has("limit") || got.Has("direction") {
		t.Errorf("loki metric lane must not send limit/direction, got %v", got)
	}
}

// TestTempoDialect_Encode pins the Tempo lane's wire contract. The query rides
// under `q`: cerberus's Tempo head reads ONLY `q` and 400s a request without it,
// so the test asserts both that `q` is set AND that `query` is absent — a request
// carrying `query` instead would look like a whole-lane rejection rather than an
// encoder bug.
func TestTempoDialect_Encode(t *testing.T) {
	d := TempoDialect()
	if d.Name() != HeadTempo {
		t.Errorf("Name() = %q, want %q", d.Name(), HeadTempo)
	}
	if d.Path() != "/api/metrics/query_range" {
		t.Errorf("Path() = %q, want /api/metrics/query_range", d.Path())
	}
	p := testParams()
	got := d.Encode("{} | rate()", p)
	if got.Get("q") != "{} | rate()" {
		t.Errorf("q = %q, want the TraceQL expression verbatim", got.Get("q"))
	}
	if got.Has("query") {
		t.Errorf("tempo lane must not send a `query` parameter (only `q` is read), got %v", got)
	}
	assertRFC3339Instant(t, "start", got.Get("start"), p.Start)
	assertRFC3339Instant(t, "end", got.Get("end"), p.End)
	if got.Get("step") != "60" {
		t.Errorf("step = %q, want 60", got.Get("step"))
	}
	if got.Has("exemplars") {
		t.Errorf("tempo lane must not request exemplars (they are not comparable), got %v", got)
	}
}

// assertRFC3339Instant checks an encoded instant parses back to want exactly.
func assertRFC3339Instant(t *testing.T, name, encoded string, want time.Time) {
	t.Helper()
	got, err := time.Parse(time.RFC3339Nano, encoded)
	if err != nil {
		t.Fatalf("%s = %q: must be RFC3339Nano-parseable: %v", name, encoded, err)
	}
	if !got.Equal(want) {
		t.Errorf("%s = %q parses to %s, want %s", name, encoded, got.UTC(), want.UTC())
	}
}

// TestDialects_TimeEncodingIsUnambiguous is the load-bearing guard against a
// silently-green gate.
//
// Upstream Loki and upstream Tempo disambiguate an INTEGER instant by digit count
// (<= 10 digits means seconds, otherwise nanoseconds), while cerberus's own heads
// use a magnitude heuristic with an extra millisecond branch. A window misread by
// six to nine orders of magnitude returns EMPTY on both sides — and Compare scores
// two empty results as a MATCH, so a mis-encoded window produces a PASSING gate
// that compared nothing.
//
// The test asserts the two new lanes encode an instant that is unambiguous under
// BOTH rules: RFC3339Nano is not an integer at all, so the digit-count branch can
// never see it, and every parser resolves it to the same instant.
func TestDialects_TimeEncodingIsUnambiguous(t *testing.T) {
	p := testParams()
	for _, d := range []Dialect{LokiDialect(), TempoDialect()} {
		vals := d.Encode("x", p)
		for _, name := range []string{"start", "end"} {
			raw := vals.Get(name)
			if _, err := strconv.ParseInt(raw, 10, 64); err == nil {
				t.Errorf("%s dialect encodes %s=%q as a bare integer: upstream then reads it as seconds-or-nanoseconds by digit count (<= %d digits = seconds), and a misread window returns empty on BOTH backends, which Compare scores as a match",
					d.Name(), name, raw, upstreamIntegerInstantDigits)
			}
			if _, err := time.Parse(time.RFC3339Nano, raw); err != nil {
				t.Errorf("%s dialect encodes %s=%q, which upstream's RFC3339Nano branch cannot parse: %v", d.Name(), name, raw, err)
			}
		}
	}

	// Guard the reasoning itself: the digit-count rule really is ambiguous for the
	// integer forms, which is why RFC3339Nano is used instead of picking one.
	secs := strconv.FormatInt(p.Start.Unix(), 10)
	nanos := strconv.FormatInt(p.Start.UnixNano(), 10)
	if len(secs) > upstreamIntegerInstantDigits {
		t.Errorf("Unix seconds %q has %d digits: the upstream seconds branch is keyed on <= %d", secs, len(secs), upstreamIntegerInstantDigits)
	}
	if len(nanos) <= upstreamIntegerInstantDigits {
		t.Errorf("Unix nanos %q has %d digits: the upstream nanosecond branch is keyed on > %d", nanos, len(nanos), upstreamIntegerInstantDigits)
	}
}

// TestNewHTTPBackend_DefaultsToPromDialect pins that an option-free backend is
// behaviour-identical to the pre-multi-head client, so every existing PromQL-only
// invocation keeps hitting the Prometheus endpoint with Prometheus encoding.
func TestNewHTTPBackend_DefaultsToPromDialect(t *testing.T) {
	b := NewHTTPBackend("http://example.invalid")
	if b.dialect.Name() != HeadProm {
		t.Errorf("default dialect = %q, want %q", b.dialect.Name(), HeadProm)
	}
}
