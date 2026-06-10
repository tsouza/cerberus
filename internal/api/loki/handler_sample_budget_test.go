package loki_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
)

// TestQueryRange_SampleBudget400 — when the per-query sample budget
// (CERBERUS_QUERY_MAX_SAMPLES) aborts the result-set drain, the Loki
// head must answer with an upstream-Loki-style limit rejection: HTTP
// 400 bad_data with a "maximum ... reached for a single query"
// message carrying the configured limit — never a 5xx (ClickHouse is
// healthy; the query is over-broad).
func TestQueryRange_SampleBudget400(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{err: &chclient.TooManySamplesError{Limit: 7}}
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
	want := "maximum number of samples (7) reached for a single query; consider reducing the query range or resolution"
	if body.Error != want {
		t.Fatalf("error message: got %q, want %q", body.Error, want)
	}
}
