//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// TestPromQueryRangeRate exercises the RangeWindow SQL path:
// rate() over a 5-minute window against the seeded counter.
func TestPromQueryRangeRate(t *testing.T) {
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

// TestPromQuerySubqueryBareVector — P0 4.5: `up[1m:30s]` lowers to a
// matrix RangeWindow with Identity=true, producing 3 anchors per
// series across [now-1m, now]. Validates the bare-vector subquery
// lowering against real CH data.
func TestPromQuerySubqueryBareVector(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	resp := getJSON(ctx, t, "/api/v1/query?query=up%5B1m%3A30s%5D")
	var parsed struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []any  `json:"result"`
		} `json:"data"`
	}
	mustDecode(t, resp, &parsed)

	if parsed.Status != "success" {
		t.Fatalf("status: got %q, want success", parsed.Status)
	}
	if len(parsed.Data.Result) == 0 {
		t.Fatalf("expected at least one series; got 0")
	}
}

// TestPromQuerySubqueryMaxOverTimeRate — P0 4.7: the canonical
// Grafana subquery shape `max_over_time(rate(m[5m])[5m:1m])`. The
// inner matrix produces 6 rate evaluations per series; the outer
// max_over_time reduces to the peak rate.
func TestPromQuerySubqueryMaxOverTimeRate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	q := url.QueryEscape("max_over_time(rate(http_server_request_duration_count[5m])[5m:1m])")
	resp := getJSON(ctx, t, "/api/v1/query?query="+q)
	var parsed struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []any  `json:"result"`
		} `json:"data"`
	}
	mustDecode(t, resp, &parsed)

	if parsed.Status != "success" {
		t.Fatalf("status: got %q, want success", parsed.Status)
	}
	if len(parsed.Data.Result) == 0 {
		t.Fatalf("expected at least one series for max_over_time(rate(...))[5m:1m]; got 0")
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
// dial / non-200, and returns the open response (caller closes). On a
// non-200 the response body is included in the failure message so the
// upstream error (CH parse failure, type mismatch, etc.) shows up in
// CI without needing a cluster-state dump.
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
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("GET %s: status %d; body=%s", path, resp.StatusCode, string(body))
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
