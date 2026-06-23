package loki_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/chclient"
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

// TestQuery_POST — Grafana's Loki datasource POSTs queries as an
// application/x-www-form-urlencoded body, not URL query params. The
// handler reads via r.FormValue, which merges the POST body, so a
// body-only query returns 200 with the streams shape. Before the
// FormValue fix this returned 400 (empty query), which the assertions
// below would catch as a regression.
func TestQuery_POST(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		samples: []chclient.Sample{
			{MetricName: "request started", Labels: map[string]string{"job": "api"}},
		},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	form := url.Values{"query": {`{job="api"}`}}
	resp, err := http.Post(
		srv.URL+"/loki/api/v1/query",
		"application/x-www-form-urlencoded",
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (POST body query must be read via FormValue)", resp.StatusCode)
	}
	var parsed queryResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if parsed.Status != "success" {
		t.Fatalf("status field: got %q, want success", parsed.Status)
	}
}

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
