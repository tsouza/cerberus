package prom_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
)

// upstreamMaxSamplesMessage is upstream Prometheus's exact wire message
// for a query that crosses --query.max-samples (promql.ErrTooManySamples
// with env "query execution", surfaced by web/api/v1 as HTTP 422
// errorType=execution). Hardcoded here — not imported from the handler —
// so a drift in the production constant fails this pin.
const upstreamMaxSamplesMessage = "query processing would load too many samples into memory in query execution"

// budgetCursor yields its canned samples, then terminates with a
// *chclient.TooManySamplesError — the exact shape a real rowsCursor
// produces when a drain crosses CERBERUS_QUERY_MAX_SAMPLES.
type budgetCursor struct {
	samples []chclient.Sample
	idx     int
	cur     chclient.Sample
	limit   int64
	err     error
}

func (c *budgetCursor) Next() bool {
	if c.err != nil {
		return false
	}
	if c.idx >= len(c.samples) {
		c.err = &chclient.TooManySamplesError{Limit: c.limit}
		return false
	}
	c.cur = c.samples[c.idx]
	c.idx++
	return true
}

func (c *budgetCursor) Sample() chclient.Sample { return c.cur }
func (c *budgetCursor) Err() error              { return c.err }
func (c *budgetCursor) Close() error            { return nil }
func (c *budgetCursor) Inspected() int64        { return int64(c.idx) }

// budgetQuerier reuses stubQuerier for every endpoint except
// QueryCursor, which returns a cursor that fails the drain with the
// sample-budget sentinel after surfacing its canned rows.
type budgetQuerier struct {
	stubQuerier
	limit int64
}

func (q *budgetQuerier) QueryCursor(_ context.Context, sql string, args ...any) (chclient.Cursor, error) {
	q.lastSQL = sql
	q.lastArgs = args
	return &budgetCursor{samples: q.samples, limit: q.limit}, nil
}

// assertBudget422 decodes a Prom error envelope and pins the
// Prometheus-parity contract: HTTP 422, errorType "execution",
// upstream's exact wire message.
func assertBudget422(t *testing.T, resp *http.Response) {
	t.Helper()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422", resp.StatusCode)
	}
	var body queryResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	_ = resp.Body.Close()
	if body.Status != "error" {
		t.Fatalf("status field: got %q, want \"error\"", body.Status)
	}
	if body.ErrorType != "execution" {
		t.Fatalf("errorType: got %q, want \"execution\"", body.ErrorType)
	}
	if body.Error != upstreamMaxSamplesMessage {
		t.Fatalf("error message: got %q, want the exact upstream message %q",
			body.Error, upstreamMaxSamplesMessage)
	}
}

// TestQueryRange_SampleBudget422 — the regression pin for the
// query-of-death OOM class (k3d run 27269987620): when the cursor
// drain behind /api/v1/query_range crosses the per-query sample
// budget, the handler must answer with upstream Prometheus's
// --query.max-samples rejection — 422, errorType=execution, exact
// message — instead of buffering an unbounded matrix or dressing the
// abort as a 5xx transport failure.
func TestQueryRange_SampleBudget422(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	q := &budgetQuerier{
		stubQuerier: stubQuerier{
			samples: []chclient.Sample{
				{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: ts, Value: 1},
				{MetricName: "up", Labels: map[string]string{"job": "db"}, Timestamp: ts, Value: 1},
			},
		},
		limit: 2,
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
	assertBudget422(t, resp)
}

// TestQuery_SampleBudget422 — instant-query sibling: the eager path
// (engine.Query → chclient.Client.Query) drains a cursor internally,
// so the same sentinel arrives wrapped as `engine: execute: ...` and
// must map onto the identical 422 contract.
func TestQuery_SampleBudget422(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{err: &chclient.TooManySamplesError{Limit: 5}}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/query?query=up")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	assertBudget422(t, resp)
}
