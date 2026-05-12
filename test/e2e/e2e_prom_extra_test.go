//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestPromHeaders_Success — successful /api/v1/query returns both
// `X-Prometheus-API-Version: v1` (so Grafana's Prom datasource is
// happy) and a `X-Cerberus-CH-Millis` numeric value (the timing
// header set by promHeadersMiddleware).
func TestPromHeaders_Success(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp := getJSON(ctx, t, "/api/v1/query?query=up")
	defer func() { _ = resp.Body.Close() }()

	if v := resp.Header.Get("X-Prometheus-API-Version"); v != "v1" {
		t.Errorf("X-Prometheus-API-Version: got %q, want v1", v)
	}
	if v := resp.Header.Get("X-Cerberus-CH-Millis"); v == "" {
		t.Errorf("X-Cerberus-CH-Millis: missing")
	} else if _, err := strconv.Atoi(v); err != nil {
		t.Errorf("X-Cerberus-CH-Millis: got %q, want a numeric value", v)
	}
}

// TestPromInvalidQuery — a syntactically broken PromQL returns 400
// with the documented Prom error envelope shape:
//
//	{"status":"error","errorType":"bad_data","error":"..."}
//
// Grafana renders this specifically.
func TestPromInvalidQuery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	v := url.Values{}
	v.Set("query", "*broken")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL()+"/api/v1/query?"+v.Encode(), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type: got %q, want application/json", ct)
	}
	var env struct {
		Status    string `json:"status"`
		ErrorType string `json:"errorType"`
		Error     string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Status != "error" || env.ErrorType != "bad_data" {
		t.Errorf("envelope: got status=%q errorType=%q, want error/bad_data", env.Status, env.ErrorType)
	}
	if env.Error == "" {
		t.Errorf("error message: empty (Grafana renders this)")
	}
}

// TestPromSeries — /api/v1/series?match[]={__name__="up"} returns
// label-set objects for each matching series in the gauge table.
func TestPromSeries(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	v := url.Values{}
	v.Set("match[]", `{__name__="up"}`)
	resp := getJSON(ctx, t, "/api/v1/series?"+v.Encode())
	var parsed struct {
		Status string              `json:"status"`
		Data   []map[string]string `json:"data"`
	}
	mustDecode(t, resp, &parsed)
	if parsed.Status != "success" {
		t.Fatalf("status: got %q, want success", parsed.Status)
	}
	if len(parsed.Data) == 0 {
		t.Fatalf("expected at least one series label-set; got 0")
	}
	for _, ls := range parsed.Data {
		if ls["__name__"] != "up" {
			t.Errorf("series missing __name__=up: %+v", ls)
		}
	}
}
