package regression

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
)

// Layer 11 — cursor-teardown ordering at the HTTP seam.
//
// clickhouse-go releases a pooled connection only when a query reaches its
// terminal state with the query context STILL LIVE. Close on an already-
// cancelled context takes the other branch instead: ClientCancel plus an
// outright socket teardown, because a cancelled query leaves undrained bytes
// on the wire. So every pooled connection the gateway keeps depends on one
// ordering — cursor close, THEN cancel — and that ordering is invisible in a
// unit test of the handler's happy path: the response body is byte-identical
// either way.
//
// The prom range path composes two defers whose relative order decides it:
// the query timeout's cancel and the streaming cursor's close. This test
// drives a real /api/v1/query_range through httptest with a cursor that
// records ctx.Err() at the instant Close is entered, so hoisting the close
// past the cancel — a plausible refactor of handleQueryRange, and one no
// response assertion would notice — fails here.

// teardownOrderCursor records the state of its query context at the instant
// Close is entered. nil is the contract; context.Canceled is the driver state
// that destroys a pooled connection.
type teardownOrderCursor struct {
	ctx     context.Context
	samples []chclient.Sample
	idx     int
	cur     chclient.Sample

	mu       sync.Mutex
	closed   bool
	closeErr error
}

func (c *teardownOrderCursor) Next() bool {
	if c.idx >= len(c.samples) {
		return false
	}
	c.cur = c.samples[c.idx]
	c.idx++
	return true
}

func (c *teardownOrderCursor) Sample() chclient.Sample { return c.cur }
func (c *teardownOrderCursor) Err() error              { return nil }
func (c *teardownOrderCursor) Inspected() int64        { return int64(c.idx) }

func (c *teardownOrderCursor) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.closeErr = c.ctx.Err()
	return nil
}

// observed reports the recorded context error and whether Close ran at all.
func (c *teardownOrderCursor) observed() (error, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeErr, c.closed
}

// teardownOrderQuerier hands out teardownOrderCursors bound to the context the
// handler opened them on, and keeps every one it created so the test can assert
// on all of them (a retry path opens a second cursor).
type teardownOrderQuerier struct {
	samples []chclient.Sample

	mu      sync.Mutex
	cursors []*teardownOrderCursor
}

func (q *teardownOrderQuerier) QueryCursor(ctx context.Context, _ string, _ ...any) (chclient.Cursor, error) {
	cur := &teardownOrderCursor{ctx: ctx, samples: q.samples}
	q.mu.Lock()
	q.cursors = append(q.cursors, cur)
	q.mu.Unlock()
	return cur, nil
}

func (q *teardownOrderQuerier) opened() []*teardownOrderCursor {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]*teardownOrderCursor(nil), q.cursors...)
}

func (q *teardownOrderQuerier) Query(_ context.Context, _ string, _ ...any) ([]chclient.Sample, error) {
	return q.samples, nil
}

func (q *teardownOrderQuerier) QueryStrings(_ context.Context, _ string, _ ...any) ([]string, error) {
	return nil, nil
}

func (q *teardownOrderQuerier) QueryLabelSets(_ context.Context, _ string, _ ...any) ([]map[string]string, error) {
	return nil, nil
}

func (q *teardownOrderQuerier) QueryMetricMeta(_ context.Context, _, _ string, _ ...any) ([]chclient.MetricMetaRow, error) {
	return nil, nil
}

func (q *teardownOrderQuerier) QueryExemplars(_ context.Context, _ string, _ ...any) ([]chclient.ExemplarRow, error) {
	return nil, nil
}

func TestPromRangeCursorClosedBeforeContextCancel(t *testing.T) {
	start := time.Unix(1717995600, 0).UTC()
	end := start.Add(2 * time.Minute)
	q := &teardownOrderQuerier{samples: []chclient.Sample{
		{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: start, Value: 1},
		{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: end, Value: 2},
	}}

	h := prom.New(q, schema.DefaultOTelMetrics(), slog.Default())
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	url := fmt.Sprintf("%s/api/v1/query_range?query=up&start=%d&end=%d&step=60",
		srv.URL, start.Unix(), end.Unix())
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	// A non-200 would mean the streaming path was never reached, and the
	// teardown assertions below would pass vacuously.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("query_range status = %d; want 200 — the streaming cursor path must actually run", resp.StatusCode)
	}

	cursors := q.opened()
	if len(cursors) == 0 {
		t.Fatal("no cursor was opened; the range query did not take the streaming path")
	}
	for i, cur := range cursors {
		ctxErr, closed := cur.observed()
		if !closed {
			t.Fatalf("cursor %d was never closed; the handler must release every cursor it opens", i)
		}
		if ctxErr != nil {
			t.Fatalf("cursor %d: ctx.Err() at Close entry = %v; want nil. clickhouse-go returns a "+
				"connection to the pool only when the query ends on a live context — closing after "+
				"the cancel destroys the socket instead", i, ctxErr)
		}
	}
}
