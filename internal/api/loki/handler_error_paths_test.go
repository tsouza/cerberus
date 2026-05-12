package loki_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/loki"
)

// TestQuery_MissingParam — /loki/api/v1/query with no `query` form
// value → 400 with the Loki error envelope (status=error, errorType,
// error message). Grafana renders this specifically.
func TestQuery_MissingParam(t *testing.T) {
	t.Parallel()

	srv := newServer(&stubQuerier{})
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/loki/api/v1/query")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", resp.StatusCode)
	}
	var env loki.Response
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Status != "error" || env.ErrorType != loki.ErrBadData {
		t.Errorf("envelope: got status=%q errorType=%q, want error/%s",
			env.Status, env.ErrorType, loki.ErrBadData)
	}
}

// TestQuery_InvalidLogQL — syntactically broken LogQL → 400 bad_data.
func TestQuery_InvalidLogQL(t *testing.T) {
	t.Parallel()

	srv := newServer(&stubQuerier{})
	t.Cleanup(srv.Close)

	// `{` without close brace = parser rejects.
	resp, err := http.Get(srv.URL + "/loki/api/v1/query?query=%7B")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", resp.StatusCode)
	}
}

// TestQuery_ParserStageRejection — `{...} | json` is parsed by the
// LogQL syntax library but cerberus's lowering rejects parser stages
// (M3.2 deferral). Regression test: the deferral error message has to
// stay stable until the feature actually ships in RC2.
func TestQuery_ParserStageRejection(t *testing.T) {
	t.Parallel()

	srv := newServer(&stubQuerier{})
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + `/loki/api/v1/query?query=%7Bjob%3D%22api%22%7D%20%7C%20json`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422 (UnprocessableEntity)", resp.StatusCode)
	}
	var env loki.Response
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Status != "error" {
		t.Errorf("status: got %q, want error", env.Status)
	}
	// Message should mention "json" or "parser" so an operator looking
	// at Grafana's error UI can see what was rejected.
	if !strings.Contains(strings.ToLower(env.Error), "json") &&
		!strings.Contains(strings.ToLower(env.Error), "parser") {
		t.Errorf("error message %q should mention `json` or `parser`", env.Error)
	}
}

// TestQuery_UpstreamError — stub Querier returns a CH error → 502
// with Loki error envelope.
func TestQuery_UpstreamError(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{err: errors.New("clickhouse: refused")}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + `/loki/api/v1/query?query=%7Bjob%3D%22api%22%7D`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status: got %d, want 502", resp.StatusCode)
	}
}

// POST-form test for /loki/api/v1/query is intentionally absent
// until the handler reads from r.FormValue() instead of
// r.URL.Query() (same gap as the prom side). When fixed, re-add
// TestQuery_POST here.

// TestErrorResponse_ContentType — every error path returns
// application/json (Grafana parses errors as JSON).
func TestErrorResponse_ContentType(t *testing.T) {
	t.Parallel()

	srv := newServer(&stubQuerier{})
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/loki/api/v1/query")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type on 400: got %q, want application/json", ct)
	}
}
