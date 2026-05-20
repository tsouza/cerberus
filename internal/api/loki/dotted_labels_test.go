package loki_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestDottedLabels_QueryHandler is the handler-layer conformance leg
// for the LogQL OTel-dotted-label rewrite. Without
// `logql.NormalizeDottedLabels` wired in front of the parser, every
// shape here 400-parse-errors with `parse error … syntax error:
// unexpected '.'`. With the rewrite, the handler returns HTTP 200 and
// the stub query path runs to completion.
//
// The stub returns no samples, so the test asserts only:
//   - HTTP 200 (the rewrite + parse + lower succeed),
//   - the response envelope decodes as `{"status":"success", …}`.
//
// Wire-format semantics (streams vs matrix, label-set shape, …) are
// covered by TestConformance_LokiQueryWire; this test is exclusively
// the parser-acceptance gate.
func TestDottedLabels_QueryHandler(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		query string
	}{
		{"bare_dotted", `{service.name="api"}`},
		{"multi_dotted", `{service.name="api", http.method="GET"}`},
		{"with_pipeline", `{service.name="api"} | json`},
		{"regex_matcher", `{service.name=~"api|web"}`},
		{"k8s_multisegment", `{k8s.pod.name="cerberus-0"}`},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{})
			t.Cleanup(srv.Close)
			resp, err := http.Get(srv.URL + "/loki/api/v1/query?query=" + url.QueryEscape(c.query))
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
			}
			var env struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal(body, &env); err != nil {
				t.Fatalf("decode: %v\nbody=%s", err, body)
			}
			if env.Status != "success" {
				t.Errorf("status: got %q, want success (body=%s)", env.Status, body)
			}
		})
	}
}

// TestDottedLabels_RangeHandler mirrors TestDottedLabels_QueryHandler
// against /loki/api/v1/query_range — the range-form path goes through
// the same Lang.Parse hook but exercises a different handler wrapper.
func TestDottedLabels_RangeHandler(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	end := start.Add(10 * time.Minute)
	srv := newServer(&stubQuerier{})
	t.Cleanup(srv.Close)

	q := url.Values{}
	q.Set("query", `rate({service.name="api"}[1m])`)
	q.Set("start", start.Format(time.RFC3339Nano))
	q.Set("end", end.Format(time.RFC3339Nano))
	q.Set("step", "60s")
	resp, err := http.Get(srv.URL + "/loki/api/v1/query_range?" + q.Encode())
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"status":"success"`) {
		t.Errorf("body missing success status: %s", body)
	}
}

// TestDottedLabels_SeriesHandler exercises the /loki/api/v1/series
// path, which routes through selectorMatchers (not Lang.Parse). The
// rewrite is wired at the matcher entry too, so a dotted match[]
// argument must round-trip the same way.
func TestDottedLabels_SeriesHandler(t *testing.T) {
	t.Parallel()

	stub := &stubQuerier{
		labelSets: []map[string]string{
			{"service_name": "api"},
		},
	}
	srv := newServer(stub)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/loki/api/v1/series?match[]=" + url.QueryEscape(`{service.name="api"}`))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"status":"success"`) {
		t.Errorf("body missing success status: %s", body)
	}
}

// TestDottedLabels_LabelsHandler exercises /loki/api/v1/labels with a
// dotted `query=` filter, same matchers entry point.
func TestDottedLabels_LabelsHandler(t *testing.T) {
	t.Parallel()

	stub := &stubQuerier{stringRows: []string{"service_name", "level"}}
	srv := newServer(stub)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/loki/api/v1/labels?query=" + url.QueryEscape(`{service.name="api"}`))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"status":"success"`) {
		t.Errorf("body missing success status: %s", body)
	}
}

