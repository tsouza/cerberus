package prom_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
)

// chTimeoutError builds the error chain chclient produces when ClickHouse
// aborts a data-plane query with TIMEOUT_EXCEEDED (code 159): a
// *chclient.QueryTimeoutError wrapping the typed *clickhouse.Exception
// (never a string match — see internal/chclient/timeout.go).
func chTimeoutError(budget time.Duration) error {
	return &chclient.QueryTimeoutError{
		Timeout: budget,
		Cause: &clickhouse.Exception{
			Code:    159,
			Name:    "TIMEOUT_EXCEEDED",
			Message: "Timeout exceeded: elapsed 120.4 seconds, maximum: 120 seconds",
		},
	}
}

// newServerWithTimeout stands up a Prom handler with a configured default
// query timeout, the ceiling the ?timeout= param min's against.
func newServerWithTimeout(q prom.Querier, def time.Duration) *httptest.Server {
	h := prom.New(q, schema.DefaultOTelMetrics(), nil)
	h.QueryTimeout = def
	mux := http.NewServeMux()
	h.Mount(mux)
	return httptest.NewServer(mux)
}

// assertTimeout503 decodes a Prom error envelope and pins the
// query-timeout contract: HTTP 503, errorType "timeout" — mirroring
// upstream Prometheus's handling of a query that hits query.timeout.
func assertTimeout503(t *testing.T, resp *http.Response) {
	t.Helper()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", resp.StatusCode)
	}
	var body queryResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	_ = resp.Body.Close()
	if body.Status != "error" {
		t.Fatalf("status field: got %q, want \"error\"", body.Status)
	}
	if body.ErrorType != prom.ErrTimeout {
		t.Fatalf("errorType: got %q, want %q", body.ErrorType, prom.ErrTimeout)
	}
}

// TestQuery_ServerSideTimeout503 — a 159 raised at query open (arriving
// through engine.Query wrapped as `engine: execute: ...`) must map onto
// 503 errorType=timeout, not the generic 500/502 the unclassified path
// produced.
func TestQuery_ServerSideTimeout503(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{err: chTimeoutError(2 * time.Minute)}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/query?query=up")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	assertTimeout503(t, resp)
}

// TestQueryRange_MidStreamTimeout503 — when ClickHouse aborts the
// query_range cursor drain mid-stream with TIMEOUT_EXCEEDED (code 159),
// the handler must answer 503 errorType=timeout. Reuses the mid-stream
// cursor shape from the memory-limit regression test.
func TestQueryRange_MidStreamTimeout503(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	q := &timeoutCursorQuerier{
		stubQuerier: stubQuerier{
			samples: []chclient.Sample{
				{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: ts, Value: 1},
			},
		},
		budget: 2 * time.Minute,
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	url := fmt.Sprintf(
		"%s/api/v1/query_range?query=up&start=%d&end=%d&step=15",
		srv.URL, ts.Add(-time.Hour).Unix(), ts.Unix(),
	)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	assertTimeout503(t, resp)
}

// timeoutCursor yields its canned samples, then terminates the drain with
// the wall-clock timeout rejection — the mid-stream shape a long-window
// matrix query hits when it crosses max_execution_time.
type timeoutCursor struct {
	samples []chclient.Sample
	idx     int
	cur     chclient.Sample
	budget  time.Duration
	err     error
}

func (c *timeoutCursor) Next() bool {
	if c.err != nil {
		return false
	}
	if c.idx >= len(c.samples) {
		c.err = chTimeoutError(c.budget)
		return false
	}
	c.cur = c.samples[c.idx]
	c.idx++
	return true
}

func (c *timeoutCursor) Sample() chclient.Sample { return c.cur }
func (c *timeoutCursor) Err() error              { return c.err }
func (c *timeoutCursor) Close() error            { return nil }
func (c *timeoutCursor) Inspected() int64        { return int64(c.idx) }

type timeoutCursorQuerier struct {
	stubQuerier
	budget time.Duration
}

func (q *timeoutCursorQuerier) QueryCursor(_ context.Context, sql string, args ...any) (chclient.Cursor, error) {
	q.lastSQL = sql
	q.lastArgs = args
	return &timeoutCursor{samples: q.samples, budget: q.budget}, nil
}

// TestQuery_TimeoutParamMalformed — a non-duration ?timeout= is a 400
// bad_data, mirroring upstream Prometheus's rejection of an invalid
// timeout parameter.
func TestQuery_TimeoutParamMalformed(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/query?query=up&timeout=not-a-duration")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", resp.StatusCode)
	}
	var body queryResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	_ = resp.Body.Close()
	if body.ErrorType != prom.ErrBadData {
		t.Fatalf("errorType: got %q, want %q", body.ErrorType, prom.ErrBadData)
	}
}

// deadlineCapturingQuerier records the deadline on the ctx its Query
// receives, so a test can assert the handler installed a context deadline
// from the resolved per-query budget.
type deadlineCapturingQuerier struct {
	stubQuerier
	gotDeadline time.Time
	hadDeadline bool
}

func (q *deadlineCapturingQuerier) Query(ctx context.Context, sql string, args ...any) ([]chclient.Sample, error) {
	q.lastSQL = sql
	q.lastArgs = args
	q.gotDeadline, q.hadDeadline = ctx.Deadline()
	return q.samples, nil
}

// TestQuery_TimeoutParamAppliesDeadline — a ?timeout= smaller than the
// configured default installs a context deadline on the query ctx at
// roughly now + the param (the smaller value wins the min).
func TestQuery_TimeoutParamAppliesDeadline(t *testing.T) {
	t.Parallel()

	q := &deadlineCapturingQuerier{}
	srv := newServerWithTimeout(q, 2*time.Minute)
	t.Cleanup(srv.Close)

	before := time.Now()
	resp, err := http.Get(srv.URL + "/api/v1/query?query=up&timeout=10s")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()

	if !q.hadDeadline {
		t.Fatal("query ctx carried no deadline; want one from ?timeout=10s")
	}
	// The deadline must sit ~10s out (the param), well below the 2m
	// default — proving the smaller value won the min.
	delta := q.gotDeadline.Sub(before)
	if delta < 5*time.Second || delta > 30*time.Second {
		t.Errorf("deadline delta = %s; want ~10s (the ?timeout= param, min'd below the 2m default)", delta)
	}
}

// TestQuery_DefaultAppliesWithoutParam — with no ?timeout=, the
// configured default still installs a context deadline (the route-A
// wall-clock cap that closes the gap), at roughly now + the default.
func TestQuery_DefaultAppliesWithoutParam(t *testing.T) {
	t.Parallel()

	q := &deadlineCapturingQuerier{}
	srv := newServerWithTimeout(q, 90*time.Second)
	t.Cleanup(srv.Close)

	before := time.Now()
	resp, err := http.Get(srv.URL + "/api/v1/query?query=up")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()

	if !q.hadDeadline {
		t.Fatal("query ctx carried no deadline; want one from the configured default")
	}
	delta := q.gotDeadline.Sub(before)
	if delta < 60*time.Second || delta > 120*time.Second {
		t.Errorf("deadline delta = %s; want ~90s (the configured default)", delta)
	}
}

// TestQuery_TimeoutDisabledNoDeadline — with the default disabled (0) and
// no ?timeout=, the handler installs no deadline (route A runs under the
// bare request ctx, as before; the cap is opt-in).
func TestQuery_TimeoutDisabledNoDeadline(t *testing.T) {
	t.Parallel()

	q := &deadlineCapturingQuerier{}
	srv := newServerWithTimeout(q, 0)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/query?query=up")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()

	if q.hadDeadline {
		t.Errorf("query ctx carried a deadline %v; want none (timeout disabled, no param)", q.gotDeadline)
	}
}
