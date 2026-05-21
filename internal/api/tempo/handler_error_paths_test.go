package tempo_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/tempo"
)

// TestSearch_InvalidTraceQL — `/api/search?q=<broken>` returns 400
// with the Tempo error envelope shape:
//
//	{"traceID":"","spanID":"","error":true,"message":"..."}
//
// Grafana's Tempo datasource specifically renders this shape — a
// regression to the generic JSON error shape (status=error,
// errorType=..., error=...) silently breaks the error UI.
func TestSearch_InvalidTraceQL(t *testing.T) {
	t.Parallel()

	srv := newServer(&stubQuerier{}, "v1.0.0-test")
	t.Cleanup(srv.Close)

	// `wharblgarbl` is the upstream Tempo parser's canonical bogus-input
	// example — it fails to parse.
	resp, err := http.Get(srv.URL + "/api/search?q=wharblgarbl")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type: got %q, want application/json", ct)
	}
	var er tempo.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !er.Error {
		t.Errorf("error: got %v, want true", er.Error)
	}
	if er.Message == "" {
		t.Errorf("message: empty (Grafana renders this)")
	}
}

// TestSearch_CHFailure — stub CH returns error → 502 with the Tempo
// envelope.
func TestSearch_CHFailure(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{err: errors.New("clickhouse: connection refused")}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search?q=%7B%20resource.service.name%20%3D%20%22api%22%20%7D")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status: got %d, want 502", resp.StatusCode)
	}
	var er tempo.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !er.Error {
		t.Errorf("error: got %v, want true", er.Error)
	}
}

// TestTraceByID_CHFailure — same shape on the trace-by-ID endpoint.
// The error envelope must include the requested traceID so Grafana
// can show "could not load trace <id>". Uses a valid 16-hex ID so the
// up-front 16-/32-hex grammar gate (which returns 400 before any CH
// lookup) doesn't pre-empt the CH-failure path under test.
func TestTraceByID_CHFailure(t *testing.T) {
	t.Parallel()

	const traceID = "0123456789abcdef"
	q := &stubQuerier{err: errors.New("clickhouse: timeout")}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/traces/" + traceID)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status: got %d, want 502", resp.StatusCode)
	}
	var er tempo.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if er.TraceID != traceID {
		t.Errorf("traceID: got %q, want %q", er.TraceID, traceID)
	}
}

// TestTraceByID_GrammarGate pins reference Tempo's trace-id grammar
// on `/api/traces/{id}` (and the v2 alias from #208): only 16- or
// 32-char lowercase hex is accepted; anything else is 400 ("invalid
// trace id") BEFORE the CH lookup, valid IDs that don't match fall
// through to 404 ("trace not found: <id>"), and mixed-case input is
// lower-cased for the lookup (Grafana sometimes emits upper-case).
//
// Both `/api/traces/{id}` and `/api/v2/traces/{id}` share the handler
// and thus the same gate; the v2 sub-runs guarantee the alias doesn't
// silently drift.
//
// NOTE: sub-tests deliberately run sequentially (no inner
// `t.Parallel()`) so they don't race on the shared `stubQuerier`'s
// `lastSQL` / `lastArgs` fields. The outer `t.Parallel()` still
// allows this test to run alongside other top-level tests in the
// package — each of which builds its own stubQuerier instance.
func TestTraceByID_GrammarGate(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{samples: nil}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	// 40-hex string used by the "too long" case: length is hex-shaped
	// but neither 16 nor 32, so the gate must reject it.
	const tooLong40Hex = "0123456789abcdef0123456789abcdef01234567"

	cases := []struct {
		name       string
		id         string
		wantStatus int
		wantMsg    string // substring assertion on the JSON `message`
	}{
		// Reference Tempo: 400 "invalid trace id".
		{name: "non_hex_ZZZZ", id: "ZZZZ", wantStatus: http.StatusBadRequest, wantMsg: "invalid trace id"},
		{name: "too_short_abc", id: "abc", wantStatus: http.StatusBadRequest, wantMsg: "invalid trace id"},
		{name: "too_long_40hex", id: tooLong40Hex, wantStatus: http.StatusBadRequest, wantMsg: "invalid trace id"},
		// Length-15 (off-by-one against the 16-char form).
		{name: "off_by_one_15hex", id: "0123456789abcde", wantStatus: http.StatusBadRequest, wantMsg: "invalid trace id"},
		// Hex-shaped but non-hex bytes (`g`).
		{name: "non_hex_16chars", id: "g123456789abcdef", wantStatus: http.StatusBadRequest, wantMsg: "invalid trace id"},
		// Valid grammar, no rows → 404 (existing behaviour).
		{name: "valid_16hex_not_found", id: "0123456789abcdef", wantStatus: http.StatusNotFound, wantMsg: "trace not found"},
		{name: "valid_32hex_not_found", id: "0123456789abcdef0123456789abcdef", wantStatus: http.StatusNotFound, wantMsg: "trace not found"},
		// Upper-case input is accepted (lower-cased before lookup);
		// no rows → 404 with the lower-cased id in the message.
		{name: "mixed_case_valid_16", id: "0123456789ABCDEF", wantStatus: http.StatusNotFound, wantMsg: "0123456789abcdef"},
		{name: "mixed_case_valid_32", id: "0123456789ABCDEF0123456789abcdef", wantStatus: http.StatusNotFound, wantMsg: "0123456789abcdef0123456789abcdef"},
	}
	versions := []struct {
		name string
		path string
	}{
		{name: "v1", path: "/api/traces/"},
		{name: "v2", path: "/api/v2/traces/"},
	}
	for _, ver := range versions {
		ver := ver
		t.Run(ver.name, func(t *testing.T) {
			// No t.Parallel() — sub-tests share the stubQuerier.
			for _, c := range cases {
				c := c
				t.Run(c.name, func(t *testing.T) {
					resp, err := http.Get(srv.URL + ver.path + c.id)
					if err != nil {
						t.Fatalf("GET: %v", err)
					}
					defer resp.Body.Close()
					if resp.StatusCode != c.wantStatus {
						t.Fatalf("status: got %d, want %d", resp.StatusCode, c.wantStatus)
					}
					var er tempo.ErrorResponse
					if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
						t.Fatalf("decode: %v", err)
					}
					if !er.Error {
						t.Errorf("error: got %v, want true", er.Error)
					}
					if c.wantMsg != "" && !strings.Contains(er.Message, c.wantMsg) {
						t.Errorf("message: got %q, want substring %q", er.Message, c.wantMsg)
					}
				})
			}
		})
	}
}

// TestVersion_HasGoVersion — regression: the goVersion field is filled
// from runtime.Version() at request time. If anything ever moves it
// to a build-time variable that's missed in goreleaser config, the
// field would silently go empty.
func TestVersion_HasGoVersion(t *testing.T) {
	t.Parallel()

	srv := newServer(&stubQuerier{}, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/status/version")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var v tempo.VersionResponse
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(v.GoVersion, "go") {
		t.Errorf("goVersion: got %q, want a string starting with `go`", v.GoVersion)
	}
}
