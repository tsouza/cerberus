package prom_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
)

// assertCanceled503 decodes a Prom error envelope and pins the
// cancellation contract: HTTP 503, errorType "canceled" — mirroring
// upstream Prometheus's handling of promql.ErrQueryCanceled
// (errorCanceled → 503). The point of the contract is what it is NOT: a
// client that hangs up mid-query must not be booked as a server fault,
// or the 5xx rate fills with events no server caused.
func assertCanceled503(t *testing.T, resp *http.Response) {
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
	if body.ErrorType != prom.ErrCanceled {
		t.Fatalf("errorType: got %q, want %q", body.ErrorType, prom.ErrCanceled)
	}
}

// TestQuery_CanceledAtOpen503 — the client walked away before the cursor
// opened, so engine.Query unwinds with context.Canceled wrapped in the
// engine's stage prefix. That must map onto 503 errorType=canceled, not
// the `engine: execute:` 502 the unclassified path produced.
func TestQuery_CanceledAtOpen503(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{err: fmt.Errorf("engine: execute: %w", context.Canceled)}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/query?query=up")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	assertCanceled503(t, resp)
}

// TestQueryRange_CanceledMidDrain503 — the client hung up after the
// query_range cursor opened, so the cancellation surfaces through
// cursor.Err() on the drain path rather than at open. classifyDrainError
// must reach the same verdict as classifyEngineError; a cancellation is
// no more a server fault at row 40 than it was at row 0.
func TestQueryRange_CanceledMidDrain503(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	q := &canceledCursorQuerier{
		stubQuerier: stubQuerier{
			samples: []chclient.Sample{
				{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: ts, Value: 1},
			},
		},
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
	assertCanceled503(t, resp)
}

// TestQuery_DeadlineExceededStaysTimeout — the discrimination pin. Both
// context sentinels now map to 503, so a classifier that collapsed them
// onto one branch would still return the right status code and only the
// errorType would betray it. A budget that expired is "timeout"; a
// caller that stopped waiting is "canceled". Without this test the two
// branches could be reordered or merged and every status assertion above
// would still pass.
func TestQuery_DeadlineExceededStaysTimeout(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{err: fmt.Errorf("engine: execute: %w", context.DeadlineExceeded)}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/query?query=up")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	assertTimeout503(t, resp)
}

// canceledCursor yields its canned samples, then terminates the drain
// with context.Canceled — the mid-stream shape a matrix query hits when
// the caller closes the connection while rows are still streaming.
type canceledCursor struct {
	samples []chclient.Sample
	idx     int
	cur     chclient.Sample
	err     error
}

func (c *canceledCursor) Next() bool {
	if c.err != nil {
		return false
	}
	if c.idx >= len(c.samples) {
		c.err = context.Canceled
		return false
	}
	c.cur = c.samples[c.idx]
	c.idx++
	return true
}

func (c *canceledCursor) Sample() chclient.Sample { return c.cur }
func (c *canceledCursor) Err() error              { return c.err }
func (c *canceledCursor) Close() error            { return nil }
func (c *canceledCursor) Inspected() int64        { return int64(c.idx) }

type canceledCursorQuerier struct {
	stubQuerier
}

func (q *canceledCursorQuerier) QueryCursor(_ context.Context, sql string, args ...any) (chclient.Cursor, error) {
	q.lastSQL = sql
	q.lastArgs = args
	return &canceledCursor{samples: q.samples}, nil
}
