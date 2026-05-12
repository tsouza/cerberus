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
// can show "could not load trace abc123".
func TestTraceByID_CHFailure(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{err: errors.New("clickhouse: timeout")}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/traces/abc123")
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
	if er.TraceID != "abc123" {
		t.Errorf("traceID: got %q, want abc123", er.TraceID)
	}
}

// TestTraceByID_InvalidHex — non-hex IDs are passed through to CH as
// strings; with no rows matching, the response is the standard
// not-found envelope. Verify cerberus doesn't panic on weird input
// (single quote, dollar sign, etc.).
//
// NOTE: sub-tests deliberately run sequentially (no inner
// `t.Parallel()`) so they don't race on the shared `stubQuerier`'s
// `lastSQL` / `lastArgs` fields. The outer `t.Parallel()` still
// allows this test to run alongside other top-level tests in the
// package — each of which builds its own stubQuerier instance.
func TestTraceByID_InvalidHex(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{samples: nil}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	cases := []string{
		"not-hex-at-all",
		"123!", // shell-special
		"zzzz", // hex-shaped but not hex
	}
	for _, id := range cases {
		t.Run(id, func(t *testing.T) {
			// No t.Parallel() inside — see comment above.
			resp, err := http.Get(srv.URL + "/api/traces/" + id)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("status: got %d, want 404", resp.StatusCode)
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
