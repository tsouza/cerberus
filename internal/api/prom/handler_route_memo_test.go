package prom_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/routememo"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/solver"
)

// --- fixtures shared by every test in this file -----------------------

// routeMemoQuery / routeMemoGrid* pin a query_range request whose lowered,
// optimized plan is BOTH below ModeAuto's predictive cost threshold (so the
// TOP-LEVEL classify() at the start of QueryPlanCursor never routes it
// itself) AND structurally eligible for Eligible()'s unconditional floor
// (so the failure-driven route memo's retry re-derivation CAN route it).
// N=41 anchors / D=5m lookback / step=15s clamps K to exactly
// routeMemoFixtureK=2 — the minimum shard count Eligible() ever produces
// (classify's "len(slices) < 2 -> not routed" floor). See
// internal/solver/eligible_test.go for the general Eligible()-bypasses-
// thresholds contract this fixture exercises end-to-end through the HTTP
// handler.
const (
	routeMemoQuery = "rate(up[5m])"
	routeMemoStep  = 15 * time.Second
	// routeMemoFixtureK is the shard count Eligible() computes for
	// routeMemoQuery over the grid below — the structural minimum (K=2),
	// chosen so the retry's Executor.Execute fan-out is as small as
	// possible while still exercising real multi-shard composition.
	routeMemoFixtureK = 2
	// routeMemoImpossibleMinFanout guarantees Plan()'s ModeAuto threshold
	// gate never clears for routeMemoQuery, so the top-level classify()
	// call in QueryPlanCursor stays on route A — only the failure-driven
	// retry's Eligible() re-derivation (which never reads MinFanout) may
	// route this fixture.
	routeMemoImpossibleMinFanout = 1_000_000
)

var (
	routeMemoStart = time.Date(2026, 6, 9, 11, 0, 0, 0, time.UTC)
	routeMemoEnd   = routeMemoStart.Add(10 * time.Minute)
)

func routeMemoURL(base string) string {
	return fmt.Sprintf("%s/api/v1/query_range?query=%s&start=%d&end=%d&step=%d",
		base, url.QueryEscape(routeMemoQuery), routeMemoStart.Unix(), routeMemoEnd.Unix(), int(routeMemoStep.Seconds()))
}

// routeMemoFakeEmitter always succeeds with a fixed, valid (no now64) SQL
// string, ignoring the shard plan entirely — the fake route-B backend below
// ignores the SQL text too, so shard-plan correctness is irrelevant to
// these tests (mirrors internal/engine/route_memo_wiring_test.go's
// fakeSolverEmitter).
type routeMemoFakeEmitter struct{}

func (routeMemoFakeEmitter) Emit(context.Context, chplan.Node) (string, []any, error) {
	return "SELECT 1", nil, nil
}

// routeMemoCursor is a minimal chclient.Cursor: it yields its canned
// samples, then terminates — cleanly if err is nil, or with err as the
// terminal error otherwise. Used for both the route-A fake (canned=nil,
// err=a resource-exhaustion rejection) and the route-B fake's failure mode
// (same shape, different canned rows / error).
type routeMemoCursor struct {
	samples []chclient.Sample
	idx     int
	cur     chclient.Sample
	err     error
	// onExhausted, when set, runs exactly once, SYNCHRONOUSLY, right before
	// Next reports end-of-stream — the deterministic in-process stand-in
	// for "the HTTP client disconnected while this cursor's drain was
	// still in flight". Because it fires in the same goroutine that will
	// next evaluate ctx.Err() (inside the engine's retry hook), there is
	// no timing window to race: by construction, ctx is already cancelled
	// by the time the retry guard checks it.
	onExhausted func()
}

func (c *routeMemoCursor) Next() bool {
	if c.idx >= len(c.samples) {
		if c.onExhausted != nil {
			c.onExhausted()
			c.onExhausted = nil
		}
		return false
	}
	c.cur = c.samples[c.idx]
	c.idx++
	return true
}

func (c *routeMemoCursor) Sample() chclient.Sample { return c.cur }
func (c *routeMemoCursor) Err() error              { return c.err }
func (c *routeMemoCursor) Close() error            { return nil }
func (c *routeMemoCursor) Inspected() int64        { return int64(c.idx) }

// routeMemoRouteAQuerier is route A's backend: every QueryCursor call opens
// fresh and succeeds, but the returned cursor's drain fails with err (a
// resource-exhaustion rejection in every test in this file) — the mid-
// stream shape the failure-driven route memo's A->B retry hook reacts to.
// opens counts dispatches so a test can assert exactly one route-A attempt
// happened per request (no internal retry loop on route A itself).
type routeMemoRouteAQuerier struct {
	stubQuerier
	opens       atomic.Int64
	err         error
	onExhausted func()
}

func (q *routeMemoRouteAQuerier) QueryCursor(_ context.Context, sql string, args ...any) (chclient.Cursor, error) {
	q.opens.Add(1)
	q.lastSQL = sql
	q.lastArgs = args
	return &routeMemoCursor{err: q.err, onExhausted: q.onExhausted}, nil
}

// routeMemoRouteBQuerier is the failure-driven route memo's route-B
// backend, wired as the Solver's Executor.Client (solver.CursorQuerier,
// NOT prom.Querier — a structurally separate data plane from route A, so
// counting opens on each fake unambiguously attributes a dispatch to its
// route). Every shard's QueryCursor call is counted; when failRows is set
// every shard streams zero rows and fails with err instead of succeeding.
type routeMemoRouteBQuerier struct {
	opens atomic.Int64
	rows  []chclient.Sample
	err   error
}

func (q *routeMemoRouteBQuerier) QueryCursor(context.Context, string, ...any) (chclient.Cursor, error) {
	q.opens.Add(1)
	if q.err != nil {
		return &routeMemoCursor{err: q.err}, nil
	}
	return newSliceCursor(q.rows), nil
}

func (q *routeMemoRouteBQuerier) MaxQueryMemoryBytes() int64 { return 0 }

// routeMemoValue is the sentinel route B's healthy fake returns — distinct
// from anything route A ever produces (route A's fake never yields a
// successful sample in this file), so a 200 response carrying this value
// unambiguously proves the data came from the retried route-B dispatch,
// not a silent route-A success.
const routeMemoValue = 99.0

func routeMemoHealthyRows() []chclient.Sample {
	return []chclient.Sample{
		{
			MetricName: "up",
			Labels:     map[string]string{"job": "api"},
			Timestamp:  routeMemoStart.Add(time.Minute),
			Value:      routeMemoValue,
		},
	}
}

// newRouteMemoHandler wires a *prom.Handler end-to-end: route A backed by
// routeA, route B backed by routeB through a real solver.Solver (ModeAuto,
// an impossibly high MinFanout so only the retry's Eligible() re-
// derivation can route), and a fresh routememo.Memo. The caller mounts the
// returned Handler itself (some tests want a real httptest.Server, the
// disconnect test wants direct mux.ServeHTTP so it controls the request's
// context precisely).
func newRouteMemoHandler(t *testing.T, routeA *routeMemoRouteAQuerier, routeB *routeMemoRouteBQuerier) *prom.Handler {
	t.Helper()
	h := prom.New(routeA, schema.DefaultOTelMetrics(), nil)

	cfg := solver.DefaultConfig()
	cfg.Mode = solver.ModeAuto
	cfg.MinFanout = routeMemoImpossibleMinFanout
	if err := cfg.Validate(); err != nil {
		t.Fatalf("solver config invalid: %v", err)
	}
	h.Engine.Solver = solver.New(cfg, routeMemoFakeEmitter{}, solver.ExecDeps{Client: routeB})
	h.Engine.RouteMemo = routememo.New(time.Minute)
	return h
}

// --- tests --------------------------------------------------------------

// TestQueryRange_RouteMemo_RetrySucceeds pins the end-to-end A->B retry
// contract through the real HTTP handler: a route-A drain failure that
// classifies as genuine resource exhaustion, corroborated twice (the
// memo's minCorroboratingFailures floor — internal/routememo), dispatches
// route B and the handler answers 200 with route B's data.
//
// The failure-driven route memo requires the SAME Key to fail on route A
// twice with no intervening success before it admits a probe (Major-1's
// corroboration requirement — see internal/routememo/memo.go), so this
// test drives the fixture through two real HTTP requests: the first
// establishes corroboration (and gets the ordinary memory-limit error,
// asserted identical to the RouteMemo-off contract); the second is where
// the retry actually fires and this test's real assertions live.
func TestQueryRange_RouteMemo_RetrySucceeds(t *testing.T) {
	t.Parallel()

	routeA := &routeMemoRouteAQuerier{err: chMemLimitError(1 << 30)}
	routeB := &routeMemoRouteBQuerier{rows: routeMemoHealthyRows()}
	h := newRouteMemoHandler(t, routeA, routeB)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// First request: corroboration=1, below the memo's floor — must NOT
	// retry. The response is byte-identical to the plain (no-retry)
	// memory-limit contract (TestQueryRange_MemoryLimit422).
	resp1, err := http.Get(routeMemoURL(srv.URL))
	if err != nil {
		t.Fatalf("GET (priming): %v", err)
	}
	assertMemoryLimit422(t, resp1)
	if got := routeA.opens.Load(); got != 1 {
		t.Fatalf("route-A opens after priming request = %d, want 1", got)
	}
	if got := routeB.opens.Load(); got != 0 {
		t.Fatalf("route-B opens after priming request = %d, want 0 — retry must not have fired yet", got)
	}

	// Second request, identical query/grid (same Key): corroboration=2 —
	// the retry now fires and dispatches route B.
	resp2, err := http.Get(routeMemoURL(srv.URL))
	if err != nil {
		t.Fatalf("GET (retry): %v", err)
	}
	if resp2.StatusCode != http.StatusOK {
		body := readBody(t, resp2)
		t.Fatalf("status = %d, want 200; body=%s", resp2.StatusCode, body)
	}
	body := readBody(t, resp2)
	var parsed queryResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("unmarshal: %v\nbody=%s", err, body)
	}
	if parsed.Status != "success" {
		t.Fatalf("status field = %q, want success; body=%s", parsed.Status, body)
	}
	if parsed.Data.ResultType != "matrix" {
		t.Fatalf("resultType = %q, want matrix; body=%s", parsed.Data.ResultType, body)
	}
	rawResult, _ := json.Marshal(parsed.Data.Result)
	var matrix []prom.MatrixSample
	if err := json.Unmarshal(rawResult, &matrix); err != nil {
		t.Fatalf("decode matrix: %v", err)
	}
	wantValue := strconv.FormatFloat(routeMemoValue, 'f', -1, 64)
	found := false
	for _, series := range matrix {
		for _, v := range series.Values {
			if v[1] == wantValue {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("response matrix does not contain route B's sentinel value %s; body=%s", wantValue, body)
	}

	// Exactly two dispatches happened for THIS (second) request: one
	// route-A attempt (which failed the drain again) and one route-B
	// retry dispatch (Executor.Execute, which — because Eligible() never
	// produces K<2 — fans into exactly routeMemoFixtureK shard cursor
	// opens on the route-B fake). Neither backend was touched a further
	// time: no retry loop on route A, no second retry chained onto the
	// first.
	if got := routeA.opens.Load(); got != 2 {
		t.Fatalf("route-A opens after retry request = %d, want 2 (1 priming + 1 this request)", got)
	}
	if got := routeB.opens.Load(); got != routeMemoFixtureK {
		t.Fatalf("route-B shard opens after retry request = %d, want %d (exactly one Executor.Execute call, fanned into K=%d shards)", got, routeMemoFixtureK, routeMemoFixtureK)
	}
}

// TestQueryRange_RouteMemo_RetryAlsoFails pins the "retry itself fails"
// path: route A fails (twice, to clear corroboration), the retry fires and
// dispatches route B, but route B's drain ALSO fails. The handler must
// surface the RETRY's own drain error (never chain a second retry — at
// most one retry per request), and the wire response must match exactly
// what the plain (no-retry) error path would produce for that error class.
func TestQueryRange_RouteMemo_RetryAlsoFails(t *testing.T) {
	t.Parallel()

	routeA := &routeMemoRouteAQuerier{err: chMemLimitError(1 << 30)}
	routeB := &routeMemoRouteBQuerier{err: chMemLimitError(1 << 30)}
	h := newRouteMemoHandler(t, routeA, routeB)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp1, err := http.Get(routeMemoURL(srv.URL))
	if err != nil {
		t.Fatalf("GET (priming): %v", err)
	}
	assertMemoryLimit422(t, resp1)

	resp2, err := http.Get(routeMemoURL(srv.URL))
	if err != nil {
		t.Fatalf("GET (retry): %v", err)
	}
	// Both route A and route B failed with the identical memory-limit
	// rejection — the wire contract is exactly the plain no-retry 422,
	// because classifyDrainError maps the retry's OWN drain error the
	// same way it would have mapped route A's.
	assertMemoryLimit422(t, resp2)

	if got := routeA.opens.Load(); got != 2 {
		t.Fatalf("route-A opens = %d, want 2 (1 priming + 1 this request) — no retry loop on route A", got)
	}
	if got := routeB.opens.Load(); got != routeMemoFixtureK {
		t.Fatalf("route-B shard opens = %d, want %d — exactly one retry dispatch, never chained into a second", got, routeMemoFixtureK)
	}
}

// TestQueryRange_RouteMemo_ClientDisconnectNeverRetries proves the
// ctx.Err() guard inside retryOnRouteAResourceFailure (internal/engine/
// route_memo_wiring.go — already unit-pinned at the engine level by
// TestRetryOnRouteAResourceFailure_ClientGoneNeverRetried) also holds at
// the HANDLER-INTEGRATION level: when the request context is cancelled by
// the time the handler asks the route memo for a retry, no route-B
// dispatch must ever fire, no matter how corroborated the Key is.
//
// The cancellation is driven in-process (mux.ServeHTTP directly, no real
// socket) via the route-A fake's onExhausted hook, which cancels the SAME
// context object the handler is using — synchronously, from inside
// matrixFromCursor's own drain loop, before the handler ever reaches the
// retry decision. This is deterministic: no sleep, no timing race.
func TestQueryRange_RouteMemo_ClientDisconnectNeverRetries(t *testing.T) {
	t.Parallel()

	routeA := &routeMemoRouteAQuerier{err: chMemLimitError(1 << 30)}
	routeB := &routeMemoRouteBQuerier{rows: routeMemoHealthyRows()}
	h := newRouteMemoHandler(t, routeA, routeB)
	mux := http.NewServeMux()
	h.Mount(mux)

	// Prime corroboration to 1 with an ordinary (uncancelled) request.
	primeReq := httptest.NewRequest(http.MethodGet, routeMemoURL(""), nil)
	primeRec := httptest.NewRecorder()
	mux.ServeHTTP(primeRec, primeReq)
	assertMemoryLimit422(t, primeRec.Result())

	// Second request: corroboration reaches 2 (the retry would otherwise
	// fire), but its context is cancelled synchronously as route A's
	// cursor reports the terminal drain error.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	routeA.onExhausted = cancel

	req := httptest.NewRequest(http.MethodGet, routeMemoURL(""), nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	// The response is still the plain memory-limit error — the disconnect
	// guard must have refused the retry before any route-B dispatch.
	assertMemoryLimit422(t, rec.Result())
	if got := routeB.opens.Load(); got != 0 {
		t.Fatalf("route-B opens = %d, want 0 — a disconnected client must never trigger a route-B dispatch", got)
	}
	if got := routeA.opens.Load(); got != 2 {
		t.Fatalf("route-A opens = %d, want 2 (1 priming + 1 this request)", got)
	}
}

// TestQueryRange_RouteMemo_NilByteIdentical pins the off-by-default
// invariant this whole mechanism depends on: with Engine.RouteMemo (and
// Engine.Solver) left nil — every existing deployment and every existing
// test in this package — CursorResult.Retry and .ObserveDrainOutcome are
// both nil, the new handler branches never execute, and the response is
// byte-identical to the pre-existing memory-limit contract
// (TestQueryRange_MemoryLimit422), including the dispatch count: exactly
// one route-A attempt, since there is no route-B backend to fall back to
// at all.
func TestQueryRange_RouteMemo_NilByteIdentical(t *testing.T) {
	t.Parallel()

	routeA := &routeMemoRouteAQuerier{err: chMemLimitError(1 << 30)}
	// prom.New wires Engine.Solver / Engine.RouteMemo to their zero values
	// (nil) — no route-memo wiring at all, matching every deployment that
	// hasn't opted in.
	srv := newServer(routeA)
	t.Cleanup(srv.Close)

	resp, err := http.Get(routeMemoURL(srv.URL))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	assertMemoryLimit422(t, resp)
	if got := routeA.opens.Load(); got != 1 {
		t.Fatalf("route-A opens = %d, want 1 — RouteMemo unwired means no retry is ever attempted", got)
	}
}
