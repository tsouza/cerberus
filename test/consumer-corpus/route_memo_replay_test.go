package consumercorpus

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/routememo"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/solver"
)

// routeMemoRetryEntryName names the corpus entry
// (grafana-12.2.9/query-range-route-memo-retry.json) this file drives
// through a real failure-driven route-memo A->B retry. See that file's
// provenance for why it exists: query_range is the only request shape
// that reaches engine.Engine.QueryCursor / QueryPlanCursor, the
// streaming path internal/engine/route_memo_wiring.go hooks into, and
// no other corpus entry exercises that HTTP surface at all.
const routeMemoRetryEntryName = "query-range-route-memo-retry"

// routeMemoRetryMinFanout is an impossibly high solver.Config.MinFanout
// so the top-level classify() call never routes this fixture on its
// own merits — only the failure-driven retry's independent
// solver.Eligible() re-derivation may dispatch route B. Mirrors
// internal/api/prom/handler_route_memo_test.go's routeMemoImpossibleMinFanout.
const routeMemoRetryMinFanout = 1_000_000

// routeMemoRetryValue is the sentinel value route B's healthy fake
// returns — distinct from anything route A ever produces (route A's
// fake never yields a successful sample in this file), so finding it
// in the decoded response proves the retry's data reached the
// consumer, not a silent route-A success.
const routeMemoRetryValue = 77.0

// routeMemoRetryWindowStartUnix / routeMemoRetryWindowEndUnix mirror
// grafana-12.2.9/query-range-route-memo-retry.json's fixed
// request.query.start / end literals exactly — they must stay in
// lock-step with that file (matrixFromCursor drops any sample outside
// [start, end], so route B's canned row below has to land inside this
// window to survive the response pivot).
const (
	routeMemoRetryWindowStartUnix = 1781002800
	routeMemoRetryWindowEndUnix   = 1781003400
)

// routeMemoRetryEmitter always succeeds with fixed, valid SQL — the
// shard SQL text is irrelevant to every fake querier below, which
// ignores it entirely and answers from canned state.
type routeMemoRetryEmitter struct{}

func (routeMemoRetryEmitter) Emit(context.Context, chplan.Node) (string, []any, error) {
	return "SELECT 1", nil, nil
}

// routeMemoRetryCursor yields its canned samples, then terminates —
// cleanly if err is nil, with err as the terminal drain error
// otherwise. Route A's fake below carries no samples, so it fails
// immediately on the first Next(), mirroring a ClickHouse memory-limit
// abort before any row streamed.
type routeMemoRetryCursor struct {
	samples []chclient.Sample
	idx     int
	cur     chclient.Sample
	err     error
}

func (c *routeMemoRetryCursor) Next() bool {
	if c.idx >= len(c.samples) {
		return false
	}
	c.cur = c.samples[c.idx]
	c.idx++
	return true
}

func (c *routeMemoRetryCursor) Sample() chclient.Sample { return c.cur }
func (c *routeMemoRetryCursor) Err() error              { return c.err }
func (c *routeMemoRetryCursor) Close() error            { return nil }
func (c *routeMemoRetryCursor) Inspected() int64        { return int64(c.idx) }

// routeMemoRetryQuerierA is route A's backend: every QueryCursor call
// opens cleanly but drains straight into a ClickHouse
// MEMORY_LIMIT_EXCEEDED rejection (chclient.MemoryLimitError, which
// wraps chclient.ErrMemoryLimitExceeded — the exact sentinel
// classifyRouteOutcome matches to classify a failure as
// routememo.OutcomeResourceFailure). Every other Querier method
// embeds *StubQuerier so this satisfies prom.Querier in full.
type routeMemoRetryQuerierA struct {
	*StubQuerier
	opens int
}

func (q *routeMemoRetryQuerierA) QueryCursor(context.Context, string, ...any) (chclient.Cursor, error) {
	q.opens++
	return &routeMemoRetryCursor{err: &chclient.MemoryLimitError{Limit: 1 << 30}}, nil
}

// routeMemoRetryQuerierB is route B's backend: solver.CursorQuerier,
// wired as the Solver's Executor.Client — a structurally separate data
// plane from route A, so counting its opens unambiguously proves the
// retry dispatched route B rather than a silent route-A success.
type routeMemoRetryQuerierB struct {
	opens int
	rows  []chclient.Sample
}

func (q *routeMemoRetryQuerierB) QueryCursor(context.Context, string, ...any) (chclient.Cursor, error) {
	q.opens++
	return &sliceCursor{samples: q.rows}, nil
}

func (q *routeMemoRetryQuerierB) MaxQueryMemoryBytes() int64 { return 0 }

// newRouteMemoReplayServer builds a *prom.Handler wired end-to-end —
// route A backed by routeA, route B backed by routeB through a real
// ModeAuto solver.Solver, and a fresh routememo.Memo — and mounts it
// behind an httptest.Server. Mirrors
// internal/api/prom/handler_route_memo_test.go's newRouteMemoHandler,
// adapted to this package's StubQuerier fixture instead of a
// bespoke stub.
func newRouteMemoReplayServer(t *testing.T, routeA *routeMemoRetryQuerierA, routeB *routeMemoRetryQuerierB) *httptest.Server {
	t.Helper()
	h := prom.New(routeA, schema.DefaultOTelMetrics(), nil)

	cfg := solver.DefaultConfig()
	cfg.Mode = solver.ModeAuto
	cfg.MinFanout = routeMemoRetryMinFanout
	if err := cfg.Validate(); err != nil {
		t.Fatalf("solver config invalid: %v", err)
	}
	h.Engine.Solver = solver.New(cfg, routeMemoRetryEmitter{}, solver.ExecDeps{Client: routeB})
	h.Engine.RouteMemo = routememo.New(time.Minute)

	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestConsumerCorpus_RouteMemoRetry_PreservesConsumerDecodeContract
// closes the coverage gap this file exists for: with a non-nil
// Engine.RouteMemo wired (the shape cmd/cerberus's prod wiring uses,
// never exercised anywhere else in this package), a query_range corpus
// entry is replayed twice against a route-A backend that always fails
// with a resource-exhaustion error. The first request only corroborates
// the failure (internal/routememo's minCorroboratingFailures floor is
// 2) and must still surface the plain memory-limit rejection with NO
// route-B dispatch. The second request crosses the corroboration
// floor: the failure-driven retry fires, route B answers, and the
// replayed response must STILL decode and satisfy this entry's exact
// consumer contract (test/consumer-corpus/grafana-12.2.9/query-range-route-memo-retry.json)
// — proving a route-B retry cannot silently drop labels, flip the
// resultType, or otherwise break what the consumer actually reads.
func TestConsumerCorpus_RouteMemoRetry_PreservesConsumerDecodeContract(t *testing.T) {
	entries, err := Load(".")
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	var entry *Entry
	for i := range entries {
		if entries[i].Name == routeMemoRetryEntryName {
			entry = &entries[i]
			break
		}
	}
	if entry == nil {
		t.Fatalf("corpus entry %q not found", routeMemoRetryEntryName)
	}

	routeA := &routeMemoRetryQuerierA{StubQuerier: &StubQuerier{}}
	routeB := &routeMemoRetryQuerierB{rows: []chclient.Sample{
		{
			MetricName: "cerberus_query_inflight",
			Labels:     map[string]string{"cerberus_ql": "promql"},
			Timestamp:  time.Unix(routeMemoRetryWindowStartUnix, 0).Add(time.Minute),
			Value:      routeMemoRetryValue,
		},
	}}
	srv := newRouteMemoReplayServer(t, routeA, routeB)

	// Priming request: corroboration=1, below the memo's floor — must
	// answer the plain memory-limit rejection with no route-B dispatch.
	primeReq, err := buildRequest(srv.URL, *entry, nil)
	if err != nil {
		t.Fatalf("build priming request: %v", err)
	}
	primeResp, err := srv.Client().Do(primeReq)
	if err != nil {
		t.Fatalf("priming request: %v", err)
	}
	_ = primeResp.Body.Close()
	if primeResp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("priming response status = %d, want %d (plain memory-limit rejection, no retry yet)", primeResp.StatusCode, http.StatusUnprocessableEntity)
	}
	if routeB.opens != 0 {
		t.Fatalf("route-B opens after priming request = %d, want 0 — retry must not have fired yet", routeB.opens)
	}

	// Second request, identical query/grid (same routememo.Key):
	// corroboration reaches 2 — the retry fires, route B answers, and
	// the response must satisfy the corpus entry's full consumer
	// contract exactly as the default stub lane would demand.
	for _, replayErr := range Replay(srv.Client(), srv.URL, *entry, nil, false) {
		t.Errorf("%v", replayErr)
	}
	if routeB.opens != routeMemoRetryK {
		t.Errorf("route-B shard opens on the retry request = %d, want %d (Executor.Execute fanned into K=%d shards)", routeB.opens, routeMemoRetryK, routeMemoRetryK)
	}
	if routeA.opens != 2 {
		t.Errorf("route-A opens across both requests = %d, want 2 (one per request; the retry dispatches route B, it does not re-attempt route A)", routeA.opens)
	}
}

// routeMemoRetryK is the shard fan-out solver.Eligible() computes for
// this entry's grid (10-minute window, 15s step -> N=41 anchors) — the
// structural minimum K=2, per
// internal/api/prom/handler_route_memo_test.go's routeMemoFixtureK.
const routeMemoRetryK = 2
