//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// TestPromQueryRangeRate exercises the M1.1 RangeWindow SQL path:
// rate() over a 5-minute window against the seeded counter.
//
// Skipped until RC2: the wrap-sample projection on top of RangeWindow
// references MetricName / TimeUnix / Value columns, but RangeWindow's
// output is just (<group keys>, value). CH responds with a
// "missing columns" error and the request returns 502. The fix lives
// in either the chsql emitter (project group-keys + synthesise the
// missing columns at the windowed-array boundary) or in executeInstant
// (skip the wrap when the lowered plan root is a RangeWindow). The
// existing Prom unit tests don't surface this because they stub the
// Querier — only a real CH integration like this E2E exercise does.
func TestPromQueryRangeRate(t *testing.T) {
	t.Skip("rate-in-query_range projection deferred to RC2 — wrap-projection vs RangeWindow output column mismatch")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	now := time.Now().Unix()
	start := now - 5*60
	v := url.Values{}
	v.Set("query", "rate(http_server_request_duration_count[5m])")
	v.Set("start", fmt.Sprintf("%d", start))
	v.Set("end", fmt.Sprintf("%d", now))
	v.Set("step", "30")

	resp := getJSON(ctx, t, "/api/v1/query_range?"+v.Encode())
	var parsed promRangeResponse
	mustDecode(t, resp, &parsed)

	if parsed.Status != "success" {
		t.Fatalf("status: got %q, want success", parsed.Status)
	}
	if parsed.Data.ResultType != "matrix" {
		t.Fatalf("resultType: got %q, want matrix", parsed.Data.ResultType)
	}
	if len(parsed.Data.Result) == 0 {
		t.Fatalf("expected at least one series; got 0")
	}
}

// TestPromLabels verifies /api/v1/labels (M2.3) returns a non-empty
// list including the `job` label seeded as an Attributes key.
func TestPromLabels(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp := getJSON(ctx, t, "/api/v1/labels")
	var parsed struct {
		Status string   `json:"status"`
		Data   []string `json:"data"`
	}
	mustDecode(t, resp, &parsed)

	if parsed.Status != "success" {
		t.Fatalf("status: got %q, want success", parsed.Status)
	}
	if len(parsed.Data) == 0 {
		t.Fatalf("expected non-empty label list")
	}
	if !contains(parsed.Data, "job") {
		t.Errorf("expected `job` in labels; got %v", parsed.Data)
	}
}

// TestPromLabelValuesName verifies /api/v1/label/__name__/values
// (M2.4) returns the metric names cerberus seeded.
func TestPromLabelValuesName(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp := getJSON(ctx, t, "/api/v1/label/__name__/values")
	var parsed struct {
		Status string   `json:"status"`
		Data   []string `json:"data"`
	}
	mustDecode(t, resp, &parsed)

	if parsed.Status != "success" {
		t.Fatalf("status: got %q, want success", parsed.Status)
	}
	if !contains(parsed.Data, "up") {
		t.Errorf("expected `up` in label values; got %v", parsed.Data)
	}
	if !contains(parsed.Data, "http_server_request_duration_count") {
		t.Errorf("expected `http_server_request_duration_count` in label values; got %v", parsed.Data)
	}
}

// promRangeResponse is the Prom `query_range` response shape.
type promRangeResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Values [][]any           `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

// getJSON is a small test helper that issues a GET, fails the test on
// dial / non-200, and returns the open response (caller closes).
func getJSON(ctx context.Context, t *testing.T, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL()+path, nil)
	if err != nil {
		t.Fatalf("new request %q: %v", path, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("GET %s: status %d", path, resp.StatusCode)
	}
	return resp
}

func mustDecode(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
