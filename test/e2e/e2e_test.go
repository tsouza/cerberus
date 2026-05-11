//go:build e2e

// Package e2e holds HTTP-level end-to-end tests run against a deployed
// cerberus stack (k3d + ClickHouse + Grafana + cerberus, brought up by
// `just e2e-up`). Gated by the `e2e` build tag so regular `go test ./...`
// doesn't try to dial localhost:8080.
//
// Run via: just e2e-run  (after just e2e-up && just e2e-seed)
package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

// baseURL is the cerberus HTTP endpoint. Defaults to the port-forwarded
// localhost address that `just e2e-up` exposes; override via CERBERUS_URL
// to point at a different deployment.
func baseURL() string {
	if v := os.Getenv("CERBERUS_URL"); v != "" {
		return v
	}
	return "http://localhost:8080"
}

func TestHealthz(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL()+"/healthz", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Fatalf("body: got %q, want %q", body, "ok")
	}
}

func TestPromQueryUp(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL()+"/api/v1/query?query=up", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/v1/query?query=up: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want 200; body=%s", resp.StatusCode, body)
	}

	var parsed struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  []any             `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if parsed.Status != "success" {
		t.Fatalf("status: got %q, want success", parsed.Status)
	}
	if parsed.Data.ResultType != "vector" {
		t.Fatalf("resultType: got %q, want vector", parsed.Data.ResultType)
	}
	if len(parsed.Data.Result) < 1 {
		t.Fatalf("expected at least one series; got %d", len(parsed.Data.Result))
	}
	for _, s := range parsed.Data.Result {
		if s.Metric["__name__"] != "up" {
			t.Errorf("series missing __name__=up; got %+v", s.Metric)
		}
		if _, ok := s.Metric["job"]; !ok {
			t.Errorf("series missing job label; got %+v", s.Metric)
		}
	}
}
