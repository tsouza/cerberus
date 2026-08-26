package prom_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/routememo"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/solver"
)

// --- issue #2650 regression: the chaos lane's pinned route-memo query -----
//
// The live-stack chaos lane's `route-memo-activation` scenario
// (.github/scripts/chaos-run.mjs, scenarioRouteMemoActivation) drives
// chaosPinnedRouteMemoQuery below at a 24h/15s grid, expecting it to fail on
// route A (a genuine ClickHouse MEMORY_LIMIT_EXCEEDED) and get corroborated
// + rescued by the failure-driven route memo's A->B retry
// (internal/routememo, internal/engine/route_memo_wiring.go).
//
// It never did: investigation (issue #2650) found route B's shard fanout
// never dispatched once across 24 real, corroborated route-A resource
// failures. Two hypotheses were live — a gap specific to ClickHouse's
// ExceptionBeforeStart (planner-level, pre-execution) rejection shape, or
// the query being structurally solver-ineligible. Both are refuted by the
// tests below and by direct inspection of clickhouse-go/v2's connect.query
// (conn_query.go: firstBlock -> firstBlockImpl's packet loop treats
// proto.ServerException identically whether it arrives before or after the
// first proto.ServerData block, so an open-time 241 always surfaces via the
// SAME classifyDriverErr wrap site regardless of ExceptionBeforeStart vs
// ExceptionWhileProcessing — internal/chclient/timeout.go's "single wrap
// site" claim holds for both shapes). The query is also NOT
// solver-ineligible: internal/solver.Planner.Eligible computes K=8 for it
// unconditionally.
//
// The real root cause: under the solver's actual PRODUCTION default
// thresholds (Mode=auto, MinFanout=16, MinAnchorPairs=4000 —
// internal/solver/config.go's DefaultConfig + ConfigFromEnv's "unset
// CERBERUS_EVAL_ROUTE means auto" contract), this exact query/grid clears
// BOTH ordinary auto-mode cost gates on its own (fanout=20 >= 16, N*F=
// 5761*20=115220 >= 4000) — so the TOP-LEVEL classify() call at the very
// start of engine.Engine.QueryPlanCursor (internal/engine/engine.go) routes
// it directly to B and returns before route A is EVER dispatched once. The
// failure-driven route memo — probe admission, corroboration, the A->B
// retry, and its own cerberus_route_ab_success_total{route_choice="b"}
// telemetry — is reachable ONLY from the route-A dispatch path
// (dispatchRouteACursor's result.openErr / the drain-failure Retry hook),
// so a query that never touches route A can never exercise it, no matter
// how reliably it breaches CERBERUS_CH_QUERY_MAX_MEMORY.
//
// The fix ships alongside these tests: test/e2e/chaos/manifests/
// chaos-overlay.env now also pins CERBERUS_SHARD_MIN_FANOUT impossibly
// high for the chaos lane's OWN deployment (mirroring
// routeMemoImpossibleMinFanout below, already established precedent in
// this file), which keeps EVERY query on route A under ordinary
// classify() — Eligible()'s retry re-derivation never reads MinFanout, so
// the memo's own rescue path is unaffected.

// chaosPinnedRouteMemoQuery is byte-identical to chaos-run.mjs's
// ROUTE_MEMO_EXPR.
const chaosPinnedRouteMemoQuery = "sum by (cerberus_ql) (rate(cerberus_queries_total[5m]))"

// chaosPinnedRouteMemoStepSeconds / chaosPinnedRouteMemoRangeSeconds mirror
// chaos-run.mjs's ROUTE_MEMO_STEP_SECONDS / ROUTE_MEMO_RANGE_SECONDS (the
// pinned 24h/15s tuple — 5,760 anchors, matching iterate-time-ranges.spec.ts's
// documented CERBERUS_CH_QUERY_MAX_MEMORY-crossing shape).
const (
	chaosPinnedRouteMemoStepSeconds  = 15
	chaosPinnedRouteMemoRangeSeconds = 24 * 3600
)

func chaosPinnedRouteMemoPath(base string) string {
	end := time.Now().Add(-2 * chaosPinnedRouteMemoStepSeconds * time.Second)
	start := end.Add(-chaosPinnedRouteMemoRangeSeconds * time.Second)
	return fmt.Sprintf("%s/api/v1/query_range?query=%s&start=%d&end=%d&step=%d",
		base, url.QueryEscape(chaosPinnedRouteMemoQuery), start.Unix(), end.Unix(), chaosPinnedRouteMemoStepSeconds)
}

// TestQueryRange_RouteMemo_ChaosQueryAutoRoutesUnderProductionThresholds
// pins the ROOT CAUSE: with the solver's real, UNMODIFIED production
// defaults (Mode=auto, MinFanout=16, MinAnchorPairs=4000 — nothing like
// routeMemoImpossibleMinFanout applied), the exact chaos-lane pinned
// query+grid never dispatches route A at all, even once — the ordinary
// top-level classify() claims it and routes straight to B. This is why the
// chaos scenario, run against a deployment that never overrides
// CERBERUS_SHARD_MIN_FANOUT, could never observe a route-A resource
// failure for this query in the first place: the failure-driven route
// memo's entire machinery (corroboration, probe, retry,
// RecordRouteABSuccess) sits behind a code path this query never reaches.
func TestQueryRange_RouteMemo_ChaosQueryAutoRoutesUnderProductionThresholds(t *testing.T) {
	t.Parallel()

	routeA := &routeMemoRouteAQuerier{err: chMemLimitError(1 << 30)}
	routeB := &routeMemoRouteBQuerier{rows: routeMemoHealthyRows()}

	h := prom.New(routeA, schema.DefaultOTelMetrics(), nil)
	cfg := solver.DefaultConfig()
	cfg.Mode = solver.ModeAuto // the deployed default (ConfigFromEnv: unset CERBERUS_EVAL_ROUTE => auto)
	// MinFanout / MinAnchorPairs / MaxK / MinAnchorsPerSlice deliberately
	// left at DefaultConfig's real production values — the whole point of
	// this test is to prove what happens WITHOUT the chaos-overlay fix.
	if err := cfg.Validate(); err != nil {
		t.Fatalf("solver config invalid: %v", err)
	}
	h.Engine.Solver = solver.New(cfg, routeMemoFakeEmitter{}, solver.ExecDeps{Client: routeB})
	h.Engine.RouteMemo = routememo.New(time.Minute)

	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(chaosPinnedRouteMemoPath(srv.URL))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (ordinary route-B dispatch on the first attempt)", resp.StatusCode)
	}
	if got := routeA.opens.Load(); got != 0 {
		t.Fatalf("route-A opens = %d, want 0 — the pinned query auto-routes to B via the top-level "+
			"classify() before route A is ever dispatched, so it can never fail on route A at all", got)
	}
	if got := routeB.opens.Load(); got == 0 {
		t.Fatalf("route-B shard opens = %d, want > 0 — expected the ordinary (non-memo) route-B dispatch to fire", got)
	}
}

// TestQueryRange_RouteMemo_ChaosQueryRescuedUnderFixedThresholds proves the
// fix: with CERBERUS_SHARD_MIN_FANOUT raised past the pinned query's own
// fanout (mirroring the chaos-overlay.env change and this file's existing
// routeMemoImpossibleMinFanout precedent), the SAME query+grid now stays on
// route A under the top-level classify() — priming corroboration on the
// first request exactly like the plain memory-limit contract — and the
// failure-driven route memo's retry rescues it via route B on the second,
// corroborated request. This is the behaviour the chaos scenario actually
// needs the deployment to exhibit.
func TestQueryRange_RouteMemo_ChaosQueryRescuedUnderFixedThresholds(t *testing.T) {
	t.Parallel()

	routeA := &routeMemoRouteAQuerier{err: chMemLimitError(1 << 30)}
	routeB := &routeMemoRouteBQuerier{rows: routeMemoHealthyRows()}

	h := prom.New(routeA, schema.DefaultOTelMetrics(), nil)
	cfg := solver.DefaultConfig()
	cfg.Mode = solver.ModeAuto
	cfg.MinFanout = routeMemoImpossibleMinFanout // the chaos-overlay.env fix
	if err := cfg.Validate(); err != nil {
		t.Fatalf("solver config invalid: %v", err)
	}
	h.Engine.Solver = solver.New(cfg, routeMemoFakeEmitter{}, solver.ExecDeps{Client: routeB})
	h.Engine.RouteMemo = routememo.New(time.Minute)

	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	path := chaosPinnedRouteMemoPath(srv.URL)

	// First request: corroboration=1, priming — must fail plainly on route
	// A, no retry yet.
	resp1, err := http.Get(path)
	if err != nil {
		t.Fatalf("GET (priming): %v", err)
	}
	assertMemoryLimit422(t, resp1)
	if got := routeA.opens.Load(); got != 1 {
		t.Fatalf("route-A opens after priming = %d, want 1", got)
	}
	if got := routeB.opens.Load(); got != 0 {
		t.Fatalf("route-B opens after priming = %d, want 0 — retry must not have fired yet", got)
	}

	// Second request: corroboration=2 — the retry fires and rescues via
	// route B.
	resp2, err := http.Get(path)
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
	if got := routeA.opens.Load(); got != 2 {
		t.Fatalf("route-A opens after retry = %d, want 2 (1 priming + 1 this request)", got)
	}
	if got := routeB.opens.Load(); got == 0 {
		t.Fatalf("route-B shard opens after retry = %d, want > 0 — the memo's A->B retry must have dispatched", got)
	}
}
