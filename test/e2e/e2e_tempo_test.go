//go:build e2e

package e2e

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestTempoEcho verifies /api/echo returns the literal "echo" string
// that Grafana's Tempo datasource health-check expects.
func TestTempoEcho(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL()+"/api/echo", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/echo: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.TrimSpace(string(body)) != "echo" {
		t.Fatalf("body: got %q, want %q", body, "echo")
	}
}

// TestTempoVersion verifies /api/status/version returns a JSON body
// with both `version` and `goVersion` fields populated.
func TestTempoVersion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp := getJSON(ctx, t, "/api/status/version")
	var parsed struct {
		Version   string `json:"version"`
		GoVersion string `json:"goVersion"`
	}
	mustDecode(t, resp, &parsed)
	if parsed.Version == "" {
		t.Errorf("expected `version` set; got empty")
	}
	if parsed.GoVersion == "" {
		t.Errorf("expected `goVersion` set; got empty")
	}
}

// TestTempoSearch verifies /api/search returns trace summaries for a
// TraceQL filter. The seed inserts a trace with
// resource.service.name = "frontend", so the query must return ≥ 1.
func TestTempoSearch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	v := url.Values{}
	v.Set("q", `{ resource.service.name = "frontend" }`)

	resp := getJSON(ctx, t, "/api/search?"+v.Encode())
	var parsed struct {
		Traces []struct {
			TraceID         string `json:"traceID"`
			RootServiceName string `json:"rootServiceName"`
			RootTraceName   string `json:"rootTraceName"`
			DurationMs      int    `json:"durationMs"`
		} `json:"traces"`
	}
	mustDecode(t, resp, &parsed)

	if len(parsed.Traces) == 0 {
		t.Fatalf("expected at least one trace summary; got 0")
	}
	for _, tr := range parsed.Traces {
		if tr.RootServiceName == "" {
			t.Errorf("summary missing rootServiceName: %+v", tr)
		}
	}
}

// TestTempoTraceByID_Found verifies /api/traces/<id> returns batches
// for a seeded trace ID.
func TestTempoTraceByID_Found(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	traceID := "a0000000000000000000000000000001"
	resp := getJSON(ctx, t, "/api/traces/"+traceID)
	var parsed struct {
		Batches []struct {
			Spans []struct {
				Name string `json:"name"`
			} `json:"spans"`
		} `json:"batches"`
	}
	mustDecode(t, resp, &parsed)

	if len(parsed.Batches) == 0 {
		t.Fatalf("expected at least one batch; got 0")
	}
	totalSpans := 0
	for _, b := range parsed.Batches {
		totalSpans += len(b.Spans)
	}
	if totalSpans == 0 {
		t.Errorf("expected at least one span across all batches; got 0")
	}
}

// TestTempoTraceByID_NotFound verifies the Tempo error envelope
// shape on a miss — Grafana relies on this distinct shape to render
// the right "trace not found" UI.
func TestTempoTraceByID_NotFound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL()+"/api/traces/deadbeef00000000000000000000dead", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/traces/<missing>: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", resp.StatusCode)
	}
	var parsed struct {
		TraceID string `json:"traceID"`
		Error   bool   `json:"error"`
		Message string `json:"message"`
	}
	mustDecode(t, resp, &parsed)
	if !parsed.Error {
		t.Errorf("expected error=true in body; got %+v", parsed)
	}
	if parsed.TraceID == "" {
		t.Errorf("expected traceID set in body; got %+v", parsed)
	}
}
