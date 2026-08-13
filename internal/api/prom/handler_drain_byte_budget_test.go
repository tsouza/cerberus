package prom_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
)

// handler_drain_byte_budget_test.go — the no-bypass ratchet for the PromQL
// sample drain's Go-heap byte budget (issue #2038), and its wire shape.
//
// The row budget (MaxQuerySamples) is a fair proxy for bytes only while every
// buffered row is a float sample sharing an interned label map. A
// histogram-valued matrix breaks that proxy by one to two orders of magnitude,
// so chclient charges a byte budget during the decode — but only when the
// caller attached one. These tests pin that BOTH sample-drain endpoints attach
// it, sized off the configured row cap, and that a crossing surfaces as the
// resource-exhausted 422 rather than a 5xx.

// wantMatrixDrainBytesPerSample duplicates chclient's
// matrixDrainBytesPerSample by value rather than importing it (it is
// unexported, and deliberately so — the sizing is not an operator knob), so a
// drift in the production constant fails this pin instead of silently
// resizing every prom query's ceiling. Mirrors upstreamMaxSamplesMessage's own
// hardcoding in handler_sample_budget_test.go.
const wantMatrixDrainBytesPerSample = 128

// budgetCtxQuerier records the context each data-plane call receives, so a
// test can assert what the handler attached to it.
type budgetCtxQuerier struct {
	stubQuerier
	gotCtx context.Context
}

func (q *budgetCtxQuerier) Query(ctx context.Context, sql string, args ...any) ([]chclient.Sample, error) {
	q.gotCtx = ctx
	return q.stubQuerier.Query(ctx, sql, args...)
}

func (q *budgetCtxQuerier) QueryCursor(ctx context.Context, sql string, args ...any) (chclient.Cursor, error) {
	q.gotCtx = ctx
	return q.stubQuerier.QueryCursor(ctx, sql, args...)
}

// newServerWithMaxSamples builds the prom head over q with the engine's row
// cap wired the way cmd/cerberus wires it from chclient.Config.MaxQuerySamples
// — the value the drain byte budget is derived from.
func newServerWithMaxSamples(q prom.Querier, maxSamples int64) *httptest.Server {
	h := prom.New(q, schema.DefaultOTelMetrics(), nil)
	h.Engine.MaxQuerySamples = maxSamples
	mux := http.NewServeMux()
	h.Mount(mux)
	return httptest.NewServer(mux)
}

// TestSampleDrainEndpoints_AttachDrainByteBudget pins that every PromQL
// endpoint which buffers a whole result attaches the byte budget, sized off
// the configured row cap. A drain reached from a context WITHOUT one charges
// nothing, so an endpoint left unthreaded is silently unbounded on the byte
// axis — which is exactly the state /api/v1/query_range was in.
func TestSampleDrainEndpoints_AttachDrainByteBudget(t *testing.T) {
	t.Parallel()

	const maxSamples = 5000
	ts := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name string
		path string
	}{
		{
			name: "query",
			path: fmt.Sprintf("/api/v1/query?query=up&time=%d", ts.Unix()),
		},
		{
			name: "query_range",
			path: fmt.Sprintf("/api/v1/query_range?query=up&start=%d&end=%d&step=15",
				ts.Add(-time.Hour).Unix(), ts.Unix()),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := &budgetCtxQuerier{stubQuerier: stubQuerier{samples: []chclient.Sample{
				{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: ts, Value: 1},
			}}}
			srv := newServerWithMaxSamples(q, maxSamples)
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL + tc.path)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status: got %d, want 200", resp.StatusCode)
			}
			if q.gotCtx == nil {
				t.Fatal("the querier was never called — the request did not reach the data plane")
			}
			budget := chclient.DrainByteBudgetFromContext(q.gotCtx)
			if budget == nil {
				t.Fatal("no drain byte budget on the query context — this drain is unbounded on the byte axis")
			}
			if want := int64(maxSamples * wantMatrixDrainBytesPerSample); budget.Limit() != want {
				t.Fatalf("budget limit = %d, want %d (the configured row cap × the per-sample allowance)",
					budget.Limit(), want)
			}
		})
	}
}

// TestSampleDrain_NoBudgetWhenRowCapDisabled pins the operator contract: the
// -1 sentinel deliberately disables the per-query sample budget, and it must
// disable the byte budget derived from it too rather than substituting a bound
// nobody configured.
func TestSampleDrain_NoBudgetWhenRowCapDisabled(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	q := &budgetCtxQuerier{stubQuerier: stubQuerier{samples: []chclient.Sample{
		{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: ts, Value: 1},
	}}}
	srv := newServerWithMaxSamples(q, -1)
	t.Cleanup(srv.Close)

	resp, err := http.Get(fmt.Sprintf("%s/api/v1/query?query=up&time=%d", srv.URL, ts.Unix()))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if q.gotCtx == nil {
		t.Fatal("the querier was never called")
	}
	if b := chclient.DrainByteBudgetFromContext(q.gotCtx); b != nil {
		t.Fatalf("byte budget %d attached while the row cap is disabled", b.Limit())
	}
}

// byteBudgetCursor yields its canned samples, then terminates with a
// *chclient.DrainByteBudgetError — the exact shape a real cursor produces when
// a decode crosses the drain byte budget.
type byteBudgetCursor struct {
	samples []chclient.Sample
	idx     int
	cur     chclient.Sample
	limit   int64
	err     error
}

func (c *byteBudgetCursor) Next() bool {
	if c.err != nil {
		return false
	}
	if c.idx >= len(c.samples) {
		c.err = &chclient.DrainByteBudgetError{Limit: c.limit}
		return false
	}
	c.cur = c.samples[c.idx]
	c.idx++
	return true
}

func (c *byteBudgetCursor) Sample() chclient.Sample { return c.cur }
func (c *byteBudgetCursor) Err() error              { return c.err }
func (c *byteBudgetCursor) Close() error            { return nil }
func (c *byteBudgetCursor) Inspected() int64        { return int64(c.idx) }

// byteBudgetQuerier answers both data-plane entry points with a drain that
// crosses the byte budget: the cursor for /query_range, the eager Query for
// /query (whose drain runs inside chclient and surfaces the same sentinel).
type byteBudgetQuerier struct {
	stubQuerier
	limit int64
}

func (q *byteBudgetQuerier) QueryCursor(_ context.Context, sql string, args ...any) (chclient.Cursor, error) {
	q.lastSQL = sql
	q.lastArgs = args
	return &byteBudgetCursor{samples: q.samples, limit: q.limit}, nil
}

func (q *byteBudgetQuerier) Query(_ context.Context, sql string, args ...any) ([]chclient.Sample, error) {
	q.lastSQL = sql
	q.lastArgs = args
	return nil, &chclient.DrainByteBudgetError{Limit: q.limit}
}

// TestSampleDrain_ByteBudget422 pins the wire shape of a byte-budget crossing
// on both endpoints: the resource-exhausted 422 errorType=execution the sample
// budget and the ClickHouse memory cap already use — never a 5xx, since
// nothing failed. The message keeps upstream Prometheus's phrasing and names
// the ceiling that fired.
func TestSampleDrain_ByteBudget422(t *testing.T) {
	t.Parallel()

	const limit = 640000
	ts := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	wantMessage := fmt.Sprintf("%s (result drain exceeded the %d-byte budget)", upstreamMaxSamplesMessage, limit)

	for _, tc := range []struct {
		name string
		path string
	}{
		{
			name: "query",
			path: fmt.Sprintf("/api/v1/query?query=up&time=%d", ts.Unix()),
		},
		{
			name: "query_range",
			path: fmt.Sprintf("/api/v1/query_range?query=up&start=%d&end=%d&step=15",
				ts.Add(-time.Hour).Unix(), ts.Unix()),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := &byteBudgetQuerier{
				stubQuerier: stubQuerier{samples: []chclient.Sample{
					{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: ts, Value: 1},
				}},
				limit: limit,
			}
			srv := newServerWithMaxSamples(q, 5000)
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL + tc.path)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status: got %d, want 422", resp.StatusCode)
			}
			var body queryResponse
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			_ = resp.Body.Close()
			if body.ErrorType != "execution" {
				t.Fatalf("errorType: got %q, want \"execution\"", body.ErrorType)
			}
			if body.Error != wantMessage {
				t.Fatalf("error message: got %q, want %q", body.Error, wantMessage)
			}
		})
	}
}
