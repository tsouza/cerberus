package loki_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/loki"
)

// canceledQueryRangeURL builds a well-formed /query_range request whose
// only interesting property is that the stub fails it — the parameters
// just have to survive validation so the error reaches the classifier.
func canceledQueryRangeURL(base string) string {
	end := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	return fmt.Sprintf(
		`%s/loki/api/v1/query_range?query=%%7Bjob%%3D%%22api%%22%%7D&start=%d&end=%d&step=60`,
		base, end.Add(-time.Hour).Unix(), end.Unix(),
	)
}

// getCanceled issues the request and returns the status code and body.
func getCanceled(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(body)
}

// TestLokiQueryRange_Canceled503 — the client closed the connection
// mid-query, so the query unwinds with context.Canceled wrapped in the
// engine's stage prefix. The Loki head must answer 503
// errorType=canceled rather than booking a caller hang-up as an
// `engine: execute:` 502 server fault.
func TestLokiQueryRange_Canceled503(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{err: fmt.Errorf("engine: execute: %w", context.Canceled)}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	status, body := getCanceled(t, canceledQueryRangeURL(srv.URL))
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503; body=%s", status, body)
	}
	assertLokiErrorEnvelope(t, body, loki.ErrCanceled)
}

// TestLokiQueryRange_DeadlineExceededStaysTimeout — the discrimination
// pin, mirroring the Prom head's. Both context sentinels map to 503, so
// a classifier that collapsed them onto a single branch would still
// return the right status and only the errorType would betray it. A
// budget that expired is "timeout"; a caller that stopped waiting is
// "canceled".
func TestLokiQueryRange_DeadlineExceededStaysTimeout(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{err: fmt.Errorf("engine: execute: %w", context.DeadlineExceeded)}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	status, body := getCanceled(t, canceledQueryRangeURL(srv.URL))
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503; body=%s", status, body)
	}
	assertLokiErrorEnvelope(t, body, loki.ErrTimeout)
}
