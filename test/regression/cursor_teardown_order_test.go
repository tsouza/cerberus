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

// Layer 10 — cursor-teardown ordering and bounding at the HTTP seam.
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

// blockedTeardownCursor models the half of the driver contract a cursor that
// closes instantly cannot exercise: a Close that does NOT return promptly.
// clickhouse-go's rows.Close() drains the stream to EndOfStream, so on a result
// set the caller walked away from it runs for as long as the remainder takes —
// unbounded, holding the pooled connection the whole time. It unblocks only when
// the query context dies (the driver's ClientCancel path).
type blockedTeardownCursor struct {
	ctx     context.Context
	samples []chclient.Sample
	idx     int
	cur     chclient.Sample

	// release is the test's own escape hatch so a handler that never bounds
	// the teardown cannot wedge the suite; the deadline assertion has already
	// fired by the time it closes.
	release chan struct{}

	entered  chan struct{}
	returned chan struct{}
	enterMu  sync.Mutex
	enterErr error
	closed   bool
}

func (c *blockedTeardownCursor) Next() bool {
	if c.idx >= len(c.samples) {
		return false
	}
	c.cur = c.samples[c.idx]
	c.idx++
	return true
}

func (c *blockedTeardownCursor) Sample() chclient.Sample { return c.cur }
func (c *blockedTeardownCursor) Err() error              { return nil }
func (c *blockedTeardownCursor) Inspected() int64        { return int64(c.idx) }

func (c *blockedTeardownCursor) Close() error {
	c.enterMu.Lock()
	if c.closed {
		c.enterMu.Unlock()
		return nil
	}
	c.closed = true
	c.enterErr = c.ctx.Err()
	c.enterMu.Unlock()

	close(c.entered)
	select {
	case <-c.ctx.Done():
	case <-c.release:
	}
	close(c.returned)
	return nil
}

func (c *blockedTeardownCursor) enteredWith() error {
	c.enterMu.Lock()
	defer c.enterMu.Unlock()
	return c.enterErr
}

// blockedTeardownQuerier hands out one blockedTeardownCursor per query.
type blockedTeardownQuerier struct {
	teardownOrderQuerier

	release chan struct{}

	mu       sync.Mutex
	blocking []*blockedTeardownCursor
}

func (q *blockedTeardownQuerier) QueryCursor(ctx context.Context, _ string, _ ...any) (chclient.Cursor, error) {
	cur := &blockedTeardownCursor{
		ctx:      ctx,
		samples:  q.samples,
		release:  q.release,
		entered:  make(chan struct{}),
		returned: make(chan struct{}),
	}
	q.mu.Lock()
	q.blocking = append(q.blocking, cur)
	q.mu.Unlock()
	return cur, nil
}

func (q *blockedTeardownQuerier) openedBlocking() []*blockedTeardownCursor {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]*blockedTeardownCursor(nil), q.blocking...)
}

// blockedTeardownDeadline is how long the test waits for a teardown to return
// before calling it unbounded. It is an order of magnitude over
// chclient.CursorDrainBudget so a loaded CI runner cannot manufacture a
// failure, while an unbounded teardown — which never returns at all — still
// fails deterministically.
const blockedTeardownDeadline = 20 * chclient.CursorDrainBudget

// TestPromRangeCursorTeardownIsBounded is the other half of the teardown
// contract, and the half TestPromRangeCursorClosedBeforeContextCancel cannot
// see: ordering alone is satisfied by an inline `defer cursor.Close()`, which
// closes on a live context and passes that test while holding the pooled
// connection for the entire unread remainder of the result set. A gateway
// serving Grafana walks away from result sets constantly, so an unbounded
// teardown pins pool slots for as long as ClickHouse takes to finish streaming.
//
// The handler must therefore own a CancelFunc for the cursor's query and route
// the teardown through chclient.CloseCursor, which drains on the live context
// and cancels once the drain budget expires. This test drives a real
// /api/v1/query_range with a cursor whose Close does not return until its query
// context dies, and requires the teardown to complete anyway.
func TestPromRangeCursorTeardownIsBounded(t *testing.T) {
	start := time.Unix(1717995600, 0).UTC()
	end := start.Add(2 * time.Minute)
	q := &blockedTeardownQuerier{
		teardownOrderQuerier: teardownOrderQuerier{samples: []chclient.Sample{
			{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: start, Value: 1},
			{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: end, Value: 2},
		}},
		release: make(chan struct{}),
	}

	h := prom.New(q, schema.DefaultOTelMetrics(), slog.Default())
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	// Registered after the server's cleanup so it runs FIRST (t.Cleanup is
	// LIFO): a wedged teardown must not leave srv.Close waiting forever.
	t.Cleanup(func() { close(q.release) })

	url := fmt.Sprintf("%s/api/v1/query_range?query=up&start=%d&end=%d&step=60",
		srv.URL, start.Unix(), end.Unix())
	// The teardown runs on the handler goroutine before it returns, so an
	// unbounded one holds the response open too. The client deadline is what
	// turns that wedge into a verdict.
	client := &http.Client{Timeout: blockedTeardownDeadline}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET did not complete within %s: %v. The handler holds no cancel for its cursor's "+
			"query, so the teardown is unbounded — a pooled connection stays occupied for the whole "+
			"unread remainder of the result set, and the request cannot finish until the drain does",
			blockedTeardownDeadline, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("query_range status = %d; want 200 — the streaming cursor path must actually run", resp.StatusCode)
	}

	cursors := q.openedBlocking()
	if len(cursors) == 0 {
		t.Fatal("no cursor was opened; the range query did not take the streaming path")
	}
	for i, cur := range cursors {
		select {
		case <-cur.entered:
		case <-time.After(blockedTeardownDeadline):
			t.Fatalf("cursor %d: Close was never entered; the handler must release every cursor it opens", i)
		}
		if ctxErr := cur.enteredWith(); ctxErr != nil {
			t.Fatalf("cursor %d: ctx.Err() at Close entry = %v; want nil — the drain must start on a live context", i, ctxErr)
		}
		select {
		case <-cur.returned:
		case <-time.After(blockedTeardownDeadline):
			t.Fatalf("cursor %d: Close has not returned after %s; the drain budget must cancel and join it",
				i, blockedTeardownDeadline)
		}
	}
}
