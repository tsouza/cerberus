package loki_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/chclient"
)

// TestQueryRange_MemoryLimit400 — when ClickHouse aborts a data-plane
// query with MEMORY_LIMIT_EXCEEDED (code 241; per-query
// CERBERUS_CH_QUERY_MAX_MEMORY cap or a server-side limit), the Loki
// head must answer with an upstream-Loki-style limit rejection: HTTP
// 400 bad_data with a "maximum ... reached for a single query"
// message carrying the configured cap — never a 5xx (ClickHouse is
// healthy when it enforces a cap; code 241 is breaker-neutral).
func TestQueryRange_MemoryLimit400(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{err: &chclient.MemoryLimitError{
		Limit: 1 << 30,
		Cause: &clickhouse.Exception{
			Code:    241,
			Name:    "MEMORY_LIMIT_EXCEEDED",
			Message: "Memory limit (total) exceeded: would use 2.12 GiB, maximum: 1.80 GiB",
		},
	}}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	end := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	reqURL := fmt.Sprintf(
		"%s/loki/api/v1/query_range?query=%s&start=%d&end=%d",
		srv.URL,
		url.QueryEscape(`{job="api"}`),
		end.Add(-time.Hour).UnixNano(),
		end.UnixNano(),
	)
	resp, err := http.Get(reqURL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", resp.StatusCode)
	}
	var body struct {
		Status    string `json:"status"`
		ErrorType string `json:"errorType"`
		Error     string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Status != "error" {
		t.Fatalf("status field: got %q, want \"error\"", body.Status)
	}
	if body.ErrorType != "bad_data" {
		t.Fatalf("errorType: got %q, want \"bad_data\"", body.ErrorType)
	}
	want := "maximum memory usage (1073741824 bytes) reached for a single query; consider reducing the query range or resolution"
	if body.Error != want {
		t.Fatalf("error message: got %q, want %q", body.Error, want)
	}
}
