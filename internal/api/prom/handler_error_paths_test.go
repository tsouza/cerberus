package prom_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/prom"
)

// TestQueryPOSTForm + TestQueryRangePOSTForm pin the handler's POST-form
// behaviour. Background: prior to this fix, the handlers read `query` /
// `start` / `end` / `step` / `time` only from `r.URL.Query()`, so a POST
// request with an `application/x-www-form-urlencoded` body had its params
// silently ignored and the handler returned `400 missing query parameter`.
// This is the request shape that the prometheus/client_golang library
// (and therefore the upstream `promql-compliance-tester` driving the
// compatibility harness) uses by default — DoGetFallback in
// api/prometheus/v1/api.go POSTs form-encoded by default and only falls
// back to GET on a 405/501. The handlers now use `r.FormValue` which
// merges URL query + parsed POST form, matching upstream Prometheus.
func TestQueryPOSTForm(t *testing.T) {
	t.Parallel()

	srv := newServer(&stubQuerier{})
	t.Cleanup(srv.Close)

	// `1+1` is a scalar PromQL — the scalar-fold path short-circuits
	// before the stub is exercised, so we don't have to load any data.
	form := url.Values{}
	form.Set("query", "1+1")
	form.Set("time", "1700000000")
	resp, err := http.PostForm(srv.URL+"/api/v1/query", form)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", resp.StatusCode, body)
	}
	var env prom.Response
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("body not JSON: %v; body=%s", err, body)
	}
	if env.Status != "success" {
		t.Errorf("body.status: got %q, want success; body=%s", env.Status, body)
	}
}

func TestQueryRangePOSTForm(t *testing.T) {
	t.Parallel()

	srv := newServer(&stubQuerier{})
	t.Cleanup(srv.Close)

	form := url.Values{}
	form.Set("query", "1+1")
	form.Set("start", "1700000000")
	form.Set("end", "1700003600")
	form.Set("step", "60s")
	resp, err := http.PostForm(srv.URL+"/api/v1/query_range", form)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", resp.StatusCode, body)
	}
	var env prom.Response
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("body not JSON: %v; body=%s", err, body)
	}
	if env.Status != "success" {
		t.Errorf("body.status: got %q, want success; body=%s", env.Status, body)
	}
}

// TestErrorResponse_ShapeAndContentType verifies that every error
// response from cerberus carries:
//   - Content-Type: application/json
//   - the Prom error envelope shape (status=error, errorType=..., error="...")
//
// Grafana renders these specifically; a regression in shape breaks
// the Prom datasource error UI silently.
func TestErrorResponse_ShapeAndContentType(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		url       string
		stub      *stubQuerier
		wantCode  int
		wantKind  string
		bodySnips []string
	}{
		{
			name:     "missing query → 400 bad_data",
			url:      "/api/v1/query",
			stub:     &stubQuerier{},
			wantCode: http.StatusBadRequest,
			wantKind: prom.ErrBadData,
		},
		{
			name:     "invalid promql → 400 bad_data",
			url:      "/api/v1/query?query=*broken",
			stub:     &stubQuerier{},
			wantCode: http.StatusBadRequest,
			wantKind: prom.ErrBadData,
		},
		{
			name:     "upstream CH error → 502 internal",
			url:      "/api/v1/query?query=up",
			stub:     &stubQuerier{err: errors.New("clickhouse: timeout")},
			wantCode: http.StatusBadGateway,
			wantKind: prom.ErrInternal,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(tc.stub)
			t.Cleanup(srv.Close)
			resp, err := http.Get(srv.URL + tc.url)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			body := readBody(t, resp)

			if resp.StatusCode != tc.wantCode {
				t.Fatalf("status: got %d, want %d; body=%s", resp.StatusCode, tc.wantCode, body)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Errorf("Content-Type: got %q, want application/json", ct)
			}
			var env prom.Response
			if err := json.Unmarshal([]byte(body), &env); err != nil {
				t.Fatalf("error body not parseable JSON: %v; body=%s", err, body)
			}
			if env.Status != "error" {
				t.Errorf("body.status: got %q, want \"error\"", env.Status)
			}
			if env.ErrorType != tc.wantKind {
				t.Errorf("body.errorType: got %q, want %q", env.ErrorType, tc.wantKind)
			}
			if env.Error == "" {
				t.Errorf("body.error: empty (Grafana renders this string)")
			}
			for _, snip := range tc.bodySnips {
				if !strings.Contains(body, snip) {
					t.Errorf("body missing %q: got %s", snip, body)
				}
			}
		})
	}
}

// TestErrorResponse_HeadersStamped — even on a 4xx / 5xx, the headers
// the Prom datasource expects should still be present. Regression test
// for promHeadersMiddleware ordering bugs.
func TestErrorResponse_HeadersStamped(t *testing.T) {
	t.Parallel()

	srv := newServer(&stubQuerier{})
	t.Cleanup(srv.Close)

	// Missing-query → 400; headers should still be there.
	resp, err := http.Get(srv.URL + "/api/v1/query")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", resp.StatusCode)
	}

	if got := resp.Header.Get("X-Prometheus-API-Version"); got != "v1" {
		t.Errorf("X-Prometheus-API-Version on 400: got %q, want v1", got)
	}
	// X-Cerberus-CH-Millis is best-effort on error paths (no CH call
	// was made for a 400). Accept missing or zero.
	if got := resp.Header.Get("X-Cerberus-CH-Millis"); got != "" {
		if _, err := strconv.Atoi(got); err != nil {
			t.Errorf("X-Cerberus-CH-Millis on 400: got %q, want numeric or empty", got)
		}
	}
}
