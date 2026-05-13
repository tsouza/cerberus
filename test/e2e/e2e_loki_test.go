//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"
)

// TestLokiQueryStreamSelector verifies /loki/api/v1/query (M3.5)
// returns the `streams` result type for a bare stream selector.
// The seed (test/e2e/seed/cmd/seed/main.go) inserts 60 log records
// across 3 services in the last minute, so {service_name="api"}
// must return at least one stream with values.
func TestLokiQueryStreamSelector(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	v := url.Values{}
	v.Set("query", `{service_name="api"}`)

	resp := getJSON(ctx, t, "/loki/api/v1/query?"+v.Encode())
	var parsed struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Stream map[string]string `json:"stream"`
				Values [][]string        `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	mustDecode(t, resp, &parsed)

	if parsed.Status != "success" {
		t.Fatalf("status: got %q, want success", parsed.Status)
	}
	if parsed.Data.ResultType != "streams" {
		t.Fatalf("resultType: got %q, want streams", parsed.Data.ResultType)
	}
	if len(parsed.Data.Result) == 0 {
		t.Fatalf("expected at least one stream; got 0")
	}
	for _, s := range parsed.Data.Result {
		if len(s.Stream) == 0 {
			t.Errorf("stream missing labels: %+v", s)
		}
	}
}

// TestLokiQueryRangeCountOverTime verifies the LogQL metric path
// (M3.3): count_over_time({selector}[5m]) returns a matrix.
func TestLokiQueryRangeCountOverTime(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	now := time.Now().Unix()
	start := now - 5*60
	v := url.Values{}
	v.Set("query", `count_over_time({service_name=~".+"}[5m])`)
	v.Set("start", fmt.Sprintf("%d", start))
	v.Set("end", fmt.Sprintf("%d", now))
	v.Set("step", "30")

	resp := getJSON(ctx, t, "/loki/api/v1/query_range?"+v.Encode())
	var parsed struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Values [][]any           `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
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
