package engine

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/routememo"
	"github.com/tsouza/cerberus/internal/solver"
	"github.com/tsouza/cerberus/internal/telemetry"
)

// --- fixtures shared by every test in this file --------------------------

var (
	memoWiringGridStart = time.Date(2026, 6, 13, 0, 0, 0, 0, time.UTC)
	memoWiringGridStep  = 15 * time.Second
	memoWiringGridOuter = time.Hour
	memoWiringGridEnd   = memoWiringGridStart.Add(memoWiringGridOuter)
)

// memoWiringEligiblePlan builds an eligible matrix RangeWindow(rate) over a
// Scan, pinned to the grid above — the same shape family
// solver_wiring_test.go's matrixPlan uses, with a real anchor count so
// Eligible produces K >= 2 (241 anchors / MinAnchorsPerSlice=30 -> K=8 under
// the default config).
func memoWiringEligiblePlan() chplan.Node {
	return &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_sum"},
		Func:            "rate",
		Range:           5 * time.Minute,
		Step:            memoWiringGridStep,
		OuterRange:      memoWiringGridOuter,
		Start:           memoWiringGridStart,
		End:             memoWiringGridEnd,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
}

// memoWiringIneligiblePlan is memoWiringEligiblePlan shrunk to a single
// grid anchor (OuterRange == Step) — too few anchors for the Planner to
// slice into k>=2 shards, so Solver.Eligible reports false structurally,
// independent of any threshold config. Used only by the not-eligible
// skip-reason case.
func memoWiringIneligiblePlan() chplan.Node {
	return &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_sum"},
		Func:            "rate",
		Range:           5 * time.Minute,
		Step:            memoWiringGridStep,
		OuterRange:      memoWiringGridStep,
		Start:           memoWiringGridStart,
		End:             memoWiringGridStart.Add(memoWiringGridStep),
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
}

// memoWiringNotRoutedDecision is the "seed" decision an initial classify()
// pass would have produced for memoWiringEligiblePlan under a config whose
// ModeAuto thresholds it does not clear (below-threshold) — it carries real
// NAnchors/Fanout/Step (populated for every decision, routed or not, per
// decision.go) so KeyFor has real cost-grid inputs to bucket.
func memoWiringNotRoutedDecision(t *testing.T) *solver.Decision {
	t.Helper()
	cfg := solver.DefaultConfig()
	cfg.Mode = solver.ModeAuto
	cfg.MinFanout = 1_000_000 // impossibly high — guarantees below-threshold
	p := &solver.Planner{Cfg: cfg}
	meta := solver.RequestMeta{Lang: solver.LangPromQL, Start: memoWiringGridStart, End: memoWiringGridEnd, Step: memoWiringGridStep}
	d, routed := p.Plan(memoWiringEligiblePlan(), meta)
	if routed {
		t.Fatalf("fixture must be below-threshold, not routed — got reason=%q", d.Reason)
	}
	if d.NAnchors == 0 {
		t.Fatalf("fixture decision carries no cost grid — NAnchors=0")
	}
	return d
}

// fakeSolverCursorClient is a minimal solver.CursorQuerier: every QueryCursor
// call either succeeds with a fixed row count or fails with a configured
// error. opens counts dispatches so a test can assert the Executor was (or
// was not) invoked.
type fakeSolverCursorClient struct {
	err            error
	rows           int
	maxMemoryBytes int64
	// opens is written concurrently — Executor.Execute dispatches K shards
	// in parallel goroutines, each calling QueryCursor on this SAME fake —
	// so it must be atomic, not a plain int (confirmed by -race: the first
	// version of this fake raced here).
	opens atomic.Int64
}

func (f *fakeSolverCursorClient) QueryCursor(context.Context, string, ...any) (chclient.Cursor, error) {
	f.opens.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return &fakeMemoWiringCursor{remaining: f.rows}, nil
}

func (f *fakeSolverCursorClient) MaxQueryMemoryBytes() int64 { return f.maxMemoryBytes }

// fakeMemoWiringCursor streams `remaining` empty-valued rows then stops.
type fakeMemoWiringCursor struct{ remaining int }

func (c *fakeMemoWiringCursor) Next() bool {
	if c.remaining <= 0 {
		return false
	}
	c.remaining--
	return true
}
func (c *fakeMemoWiringCursor) Sample() chclient.Sample { return chclient.Sample{} }
func (c *fakeMemoWiringCursor) Err() error              { return nil }
func (c *fakeMemoWiringCursor) Close() error            { return nil }
func (c *fakeMemoWiringCursor) Inspected() int64        { return 0 }

// fakeSolverEmitter always succeeds with a fixed, valid (no now64) SQL
// string — the Executor's now64 belt-and-braces guard must not fire.
type fakeSolverEmitter struct{}

func (fakeSolverEmitter) Emit(context.Context, chplan.Node) (string, []any, error) {
	return "SELECT 1", nil, nil
}

// alwaysClosedBreaker satisfies solver's package-local breakerPeeker via
// the Executor's exported field type — reuse Solver's own BreakerClosed
// zero-value contract instead (nil Executor.Breaker => BreakerClosed()
// reports true), so no fake is needed for the common case; a dedicated
// open-breaker fake is defined where a test needs one.

// newMemoWiringEngine builds an Engine wired end-to-end: a ModeAuto Solver
// backed by cursorClient, a fresh routememo.Memo, ready for tryRouteMemoHit
// / retryOnRouteAResourceFailure to dispatch through.
func newMemoWiringEngine(t *testing.T, cursorClient solver.CursorQuerier) (*Engine, *routememo.Memo) {
	t.Helper()
	cfg := solver.DefaultConfig()
	cfg.Mode = solver.ModeAuto
	if err := cfg.Validate(); err != nil {
		t.Fatalf("solver config invalid: %v", err)
	}
	sv := solver.New(cfg, fakeSolverEmitter{}, solver.ExecDeps{Client: cursorClient})
	memo := routememo.New(time.Minute)
	return &Engine{Solver: sv, RouteMemo: memo}, memo
}

// openBreakerFake satisfies solver.ExecDeps' unexported breakerPeeker
// interface structurally (PeekBreakerState + BreakerRetryAfter) — Go
// interface satisfaction is by method set, not by name, so this local type is
// enough to make the Solver see an OPEN breaker without importing chclient.
type openBreakerFake struct{}

func (openBreakerFake) PeekBreakerState() string { return solver.BreakerOpen }

func (openBreakerFake) BreakerRetryAfter() time.Duration { return 0 }

// newMemoWiringEngineWithOpenBreaker is newMemoWiringEngine plus a breaker
// that always reports OPEN — used only by the breaker-open skip-reason
// case; every other test in this file relies on the nil-breaker zero value
// (BreakerClosed() reports true) instead.
func newMemoWiringEngineWithOpenBreaker(t *testing.T, cursorClient solver.CursorQuerier) (*Engine, *routememo.Memo) {
	t.Helper()
	cfg := solver.DefaultConfig()
	cfg.Mode = solver.ModeAuto
	if err := cfg.Validate(); err != nil {
		t.Fatalf("solver config invalid: %v", err)
	}
	sv := solver.New(cfg, fakeSolverEmitter{}, solver.ExecDeps{Client: cursorClient, Breaker: openBreakerFake{}})
	memo := routememo.New(time.Minute)
	return &Engine{Solver: sv, RouteMemo: memo}, memo
}

// --- OpenTelemetry instrument assertions ------------------------------------
//
// Every test below installs its own ManualReader-backed MeterProvider via
// installMemoWiringTelemetryReader and therefore runs SERIALLY (no
// t.Parallel()): both the process-global MeterProvider
// (telemetry.Install/Get) and route_memo_wiring.go's own package-level
// routeMemoPressureState flag are shared process state. Go's testing
// package only actually runs t.Parallel() tests concurrently once every
// serial top-level test (these included) has finished, so a serial test
// here never overlaps with the rest of this file's parallel tests, which
// all exercise the same wiring functions and would otherwise race on both.

// installMemoWiringTelemetryReader swaps the process-global MeterProvider
// for one backed by a ManualReader, mirroring
// internal/telemetry/metrics_test.go's own installManualReader helper.
func installMemoWiringTelemetryReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	telemetry.Install(mp)
	t.Cleanup(func() { telemetry.Install(nil) })
	return reader
}

// collectMemoWiringScope drains reader and returns the cerberus telemetry
// scope's metrics. An entirely absent scope (nothing has called
// telemetry.Get() against this reader's provider yet — e.g. a "before"
// snapshot taken before the first Record/Observe call in a test) is a
// legitimate all-zero state, not a failure: it returns the zero
// ScopeMetrics, which every lookup helper below already treats the same as
// "metric present with zero data points".
func collectMemoWiringScope(t *testing.T, reader *sdkmetric.ManualReader) metricdata.ScopeMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		if strings.HasSuffix(sm.Scope.Name, "internal/telemetry") {
			return sm
		}
	}
	return metricdata.ScopeMetrics{}
}

// routeMemoHitSkippedByReason sums cerberus_route_memo_hit_skipped_total by
// its `reason` attribute. A reason never recorded is simply absent from the
// returned map (zero value on lookup), not a failure.
func routeMemoHitSkippedByReason(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, m := range collectMemoWiringScope(t, reader).Metrics {
		if m.Name != "cerberus_route_memo_hit_skipped_total" {
			continue
		}
		sum, ok := m.Data.(metricdata.Sum[int64])
		if !ok {
			t.Fatalf("route_memo_hit_skipped_total data: want Sum[int64], got %T", m.Data)
		}
		for _, dp := range sum.DataPoints {
			reason, ok := dp.Attributes.Value("reason")
			if !ok {
				t.Fatalf("route_memo_hit_skipped_total data point missing reason attribute: %v", dp.Attributes.ToSlice())
			}
			out[reason.AsString()] += dp.Value
		}
	}
	return out
}

// routeABSuccessByRoute sums cerberus_route_ab_success_total by its
// cerberus.route_choice attribute.
func routeABSuccessByRoute(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, m := range collectMemoWiringScope(t, reader).Metrics {
		if m.Name != "cerberus_route_ab_success_total" {
			continue
		}
		sum, ok := m.Data.(metricdata.Sum[int64])
		if !ok {
			t.Fatalf("route_ab_success_total data: want Sum[int64], got %T", m.Data)
		}
		for _, dp := range sum.DataPoints {
			route, ok := dp.Attributes.Value("cerberus.route_choice")
			if !ok {
				t.Fatalf("route_ab_success_total data point missing cerberus.route_choice attribute: %v", dp.Attributes.ToSlice())
			}
			out[route.AsString()] += dp.Value
		}
	}
	return out
}

// routedDispatchInflightValue reads the current net value of
// cerberus_routed_dispatch_inflight — the instrument carries no
// attributes, so every collection yields at most one data point.
func routedDispatchInflightValue(t *testing.T, reader *sdkmetric.ManualReader) int64 {
	t.Helper()
	return sumUnlabeledUpDownCounter(t, reader, "cerberus_routed_dispatch_inflight")
}

// routeMemoPressureActiveValue reads the current net value of
// cerberus_route_memo_pressure_active — 0 (inactive) or 1 (active), since
// recordRouteMemoPressureActive only ever moves it by a transition delta.
func routeMemoPressureActiveValue(t *testing.T, reader *sdkmetric.ManualReader) int64 {
	t.Helper()
	return sumUnlabeledUpDownCounter(t, reader, "cerberus_route_memo_pressure_active")
}

func sumUnlabeledUpDownCounter(t *testing.T, reader *sdkmetric.ManualReader, name string) int64 {
	t.Helper()
	var total int64
	for _, m := range collectMemoWiringScope(t, reader).Metrics {
		if m.Name != name {
			continue
		}
		sum, ok := m.Data.(metricdata.Sum[int64])
		if !ok {
			t.Fatalf("%s data: want Sum[int64], got %T", name, m.Data)
		}
		for _, dp := range sum.DataPoints {
			total += dp.Value
		}
	}
	return total
}

// TestTryRouteMemoHit_RecordsDeclineReasons pins RouteMemoHitSkippedTotal's
// `reason` attribute against every real decline branch tryRouteMemoHit
// itself distinguishes: deriveRouteMemoDispatch's eligibility and
// freshness gates, Memo.Lookup's verdict and staleness gates, the
// ClickHouse breaker gate, and Memo.AdmitDispatch's admission-token gate.
func TestTryRouteMemoHit_RecordsDeclineReasons(t *testing.T) {
	reader := installMemoWiringTelemetryReader(t)
	plan := memoWiringEligiblePlan()

	t.Run("not-eligible", func(t *testing.T) {
		cq := &fakeSolverCursorClient{rows: 1}
		eng, _ := newMemoWiringEngine(t, cq)
		seed := memoWiringNotRoutedDecision(t)
		before := routeMemoHitSkippedByReason(t, reader)[telemetry.RouteMemoDeclineNotEligible]
		if _, _, _, _, ok := eng.tryRouteMemoHit(t.Context(), "promql", memoWiringIneligiblePlan(), seed, nil); ok {
			t.Fatal("tryRouteMemoHit dispatched despite a structurally ineligible plan")
		}
		after := routeMemoHitSkippedByReason(t, reader)[telemetry.RouteMemoDeclineNotEligible]
		if after != before+1 {
			t.Errorf("not-eligible skip count = %d, want %d", after, before+1)
		}
		if cq.opens.Load() != 0 {
			t.Errorf("Executor cursor opens = %d, want 0", cq.opens.Load())
		}
	})

	t.Run("not-fresh", func(t *testing.T) {
		cq := &fakeSolverCursorClient{rows: 1}
		eng, _ := newMemoWiringEngine(t, cq)
		seed := memoWiringNotRoutedDecision(t)
		// tryRouteMemoHit calls time.Now() internally (not injectable), so
		// the ONLY way to land in the not-fresh branch regardless of when
		// this test runs is a grid whose End sits at the real live edge —
		// same anchor-count shape as memoWiringEligiblePlan (so eligibility
		// still holds), just anchored to now instead of a fixed past date.
		end := time.Now().Truncate(memoWiringGridStep)
		notFreshPlan := &chplan.RangeWindow{
			Input:           &chplan.Scan{Table: "otel_metrics_sum"},
			Func:            "rate",
			Range:           5 * time.Minute,
			Step:            memoWiringGridStep,
			OuterRange:      memoWiringGridOuter,
			Start:           end.Add(-memoWiringGridOuter),
			End:             end,
			TimestampColumn: "TimeUnix",
			ValueColumn:     "Value",
			GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		}
		before := routeMemoHitSkippedByReason(t, reader)[telemetry.RouteMemoDeclineNotFresh]
		if _, _, _, _, ok := eng.tryRouteMemoHit(t.Context(), "promql", notFreshPlan, seed, nil); ok {
			t.Fatal("tryRouteMemoHit dispatched on a request whose End has not aged past the live-edge margin")
		}
		after := routeMemoHitSkippedByReason(t, reader)[telemetry.RouteMemoDeclineNotFresh]
		if after != before+1 {
			t.Errorf("not-fresh skip count = %d, want %d", after, before+1)
		}
		if cq.opens.Load() != 0 {
			t.Errorf("Executor cursor opens = %d, want 0", cq.opens.Load())
		}
	})

	t.Run("no-preferb", func(t *testing.T) {
		cq := &fakeSolverCursorClient{rows: 1}
		eng, _ := newMemoWiringEngine(t, cq)
		seed := memoWiringNotRoutedDecision(t)
		before := routeMemoHitSkippedByReason(t, reader)[telemetry.RouteMemoDeclineNoPreferB]
		// Fresh memo: Lookup(k) reports Unknown, not PreferB.
		if _, _, _, _, ok := eng.tryRouteMemoHit(t.Context(), "promql", plan, seed, nil); ok {
			t.Fatal("tryRouteMemoHit dispatched with no recorded verdict")
		}
		after := routeMemoHitSkippedByReason(t, reader)[telemetry.RouteMemoDeclineNoPreferB]
		if after != before+1 {
			t.Errorf("no-preferb skip count = %d, want %d", after, before+1)
		}
	})

	t.Run("stale", func(t *testing.T) {
		cq := &fakeSolverCursorClient{rows: 1}
		eng, memo := newMemoWiringEngine(t, cq)
		seed := memoWiringNotRoutedDecision(t)
		fixedNow := memoWiringGridEnd.Add(2 * memoWiringGridStep)
		memo.SetNowForTest(func() time.Time { return fixedNow })
		d := eng.deriveRouteMemoDispatch(plan, seed, fixedNow)
		memo.Observe(d.key, routememo.RouteB, routememo.OutcomeSuccess)
		memo.SetNowForTest(func() time.Time { return fixedNow.Add(20 * time.Minute) }) // past re-validation midpoint
		before := routeMemoHitSkippedByReason(t, reader)[telemetry.RouteMemoDeclineStale]
		if _, _, _, _, ok := eng.tryRouteMemoHit(t.Context(), "promql", plan, seed, nil); ok {
			t.Fatal("tryRouteMemoHit dispatched on a STALE PreferB verdict")
		}
		after := routeMemoHitSkippedByReason(t, reader)[telemetry.RouteMemoDeclineStale]
		if after != before+1 {
			t.Errorf("stale skip count = %d, want %d", after, before+1)
		}
	})

	t.Run("breaker-open", func(t *testing.T) {
		cq := &fakeSolverCursorClient{rows: 1}
		eng, memo := newMemoWiringEngineWithOpenBreaker(t, cq)
		seed := memoWiringNotRoutedDecision(t)
		d := eng.deriveRouteMemoDispatch(plan, seed, memoWiringGridEnd.Add(2*memoWiringGridStep))
		memo.Observe(d.key, routememo.RouteB, routememo.OutcomeSuccess)
		before := routeMemoHitSkippedByReason(t, reader)[telemetry.RouteMemoDeclineBreakerOpen]
		if _, _, _, _, ok := eng.tryRouteMemoHit(t.Context(), "promql", plan, seed, nil); ok {
			t.Fatal("tryRouteMemoHit dispatched with the breaker OPEN")
		}
		after := routeMemoHitSkippedByReason(t, reader)[telemetry.RouteMemoDeclineBreakerOpen]
		if after != before+1 {
			t.Errorf("breaker-open skip count = %d, want %d", after, before+1)
		}
	})

	t.Run("no-dispatch-token", func(t *testing.T) {
		cq := &fakeSolverCursorClient{rows: 1}
		eng, memo := newMemoWiringEngine(t, cq)
		seed := memoWiringNotRoutedDecision(t)
		d := eng.deriveRouteMemoDispatch(plan, seed, memoWiringGridEnd.Add(2*memoWiringGridStep))
		memo.Observe(d.key, routememo.RouteB, routememo.OutcomeSuccess)
		// Exhaust the shared admission semaphore (maxConcurrentRoutedDispatches
		// == 4) directly, without releasing, so AdmitDispatch itself refuses.
		for i := 0; i < 4; i++ {
			if _, ok := memo.AdmitDispatch(); !ok {
				t.Fatalf("failed to pre-admit token %d while exhausting the semaphore", i)
			}
		}
		before := routeMemoHitSkippedByReason(t, reader)[telemetry.RouteMemoDeclineNoDispatchToken]
		if _, _, _, _, ok := eng.tryRouteMemoHit(t.Context(), "promql", plan, seed, nil); ok {
			t.Fatal("tryRouteMemoHit dispatched despite an exhausted admission semaphore")
		}
		after := routeMemoHitSkippedByReason(t, reader)[telemetry.RouteMemoDeclineNoDispatchToken]
		if after != before+1 {
			t.Errorf("no-dispatch-token skip count = %d, want %d", after, before+1)
		}
	})
}

// TestRetryOnRouteAResourceFailure_RecordsProbeDeclineReasons pins the two
// decline reasons unique to the A->B retry's probe-admission gate:
// under-pressure (Memo.UnderPressure sampled true immediately before the
// atomic record-and-admit call) and probe-not-admitted (the same call
// refusing for any OTHER reason — here, corroboration not yet met).
func TestRetryOnRouteAResourceFailure_RecordsProbeDeclineReasons(t *testing.T) {
	reader := installMemoWiringTelemetryReader(t)
	plan := memoWiringEligiblePlan()

	t.Run("probe-not-admitted", func(t *testing.T) {
		cq := &fakeSolverCursorClient{rows: 1}
		eng, _ := newMemoWiringEngine(t, cq)
		seed := memoWiringNotRoutedDecision(t)
		before := routeMemoHitSkippedByReason(t, reader)[telemetry.RouteMemoDeclineProbeNotAdmitted]
		// First resource failure on a fresh Unknown key: corroboration=1,
		// below minCorroboratingFailures — ObserveRouteAFailureAndMaybeBeginProbe
		// itself refuses, for a reason OTHER than cluster-wide pressure.
		if _, _, _, _, retried := eng.retryOnRouteAResourceFailure(t.Context(), "promql", plan, seed, nil, chclient.ErrMemoryLimitExceeded); retried {
			t.Fatal("retried on the first route-A resource failure")
		}
		after := routeMemoHitSkippedByReason(t, reader)[telemetry.RouteMemoDeclineProbeNotAdmitted]
		if after != before+1 {
			t.Errorf("probe-not-admitted skip count = %d, want %d", after, before+1)
		}
		if cq.opens.Load() != 0 {
			t.Errorf("Executor cursor opens = %d, want 0", cq.opens.Load())
		}
	})

	t.Run("under-pressure", func(t *testing.T) {
		cq := &fakeSolverCursorClient{rows: 1}
		eng, memo := newMemoWiringEngine(t, cq)
		routeMemoPressureState.Store(false) // pin a known baseline regardless of prior subtest ordering
		seed := memoWiringNotRoutedDecision(t)
		// Push the memo into cluster-wide pressure directly via Observe,
		// mirroring routememo's own TestPressureDamperSuppressesBothActionAndLearning
		// fixture — enough DISTINCT keys failing to cross the (unexported)
		// pressureFailureThreshold without any of them individually
		// reaching corroboration.
		const pressureBurstKeys = 8 // comfortably above routememo's pressureFailureThreshold (3)
		for i := 0; i < pressureBurstKeys; i++ {
			k := routememo.Key{RootKind: "*fixture.PressureBurst", RangeFuncs: string(rune('a' + i))}
			memo.Observe(k, routememo.RouteA, routememo.OutcomeResourceFailure)
		}
		if !memo.UnderPressure() {
			t.Fatalf("fixture failed to establish UnderPressure()==true")
		}
		before := routeMemoHitSkippedByReason(t, reader)[telemetry.RouteMemoDeclineUnderPressure]
		if _, _, _, _, retried := eng.retryOnRouteAResourceFailure(t.Context(), "promql", plan, seed, nil, chclient.ErrMemoryLimitExceeded); retried {
			t.Fatal("retried while the memo is under cluster-wide pressure")
		}
		after := routeMemoHitSkippedByReason(t, reader)[telemetry.RouteMemoDeclineUnderPressure]
		if after != before+1 {
			t.Errorf("under-pressure skip count = %d, want %d", after, before+1)
		}
		if cq.opens.Load() != 0 {
			t.Errorf("Executor cursor opens = %d, want 0", cq.opens.Load())
		}
	})
}

// TestRetryOnRouteAResourceFailure_RoutedDispatchInflight pins
// RoutedDispatchInflight's begin/end pairing: it goes to 1 the moment the
// probe is admitted and stays there — even after retryOnRouteAResourceFailure
// itself has returned — until the caller's observeFn reports the drain
// outcome, exactly mirroring when the memo's own admission token releases.
func TestRetryOnRouteAResourceFailure_RoutedDispatchInflight(t *testing.T) {
	reader := installMemoWiringTelemetryReader(t)
	cq := &fakeSolverCursorClient{rows: 2}
	eng, _ := newMemoWiringEngine(t, cq)
	plan := memoWiringEligiblePlan()
	seed := memoWiringNotRoutedDecision(t)
	ctx := t.Context()

	if got := routedDispatchInflightValue(t, reader); got != 0 {
		t.Fatalf("routed_dispatch_inflight before any dispatch = %d, want 0", got)
	}

	// corroboration=1: below threshold, no dispatch, no inflight movement.
	if _, _, _, _, retried := eng.retryOnRouteAResourceFailure(ctx, "promql", plan, seed, nil, chclient.ErrMemoryLimitExceeded); retried {
		t.Fatal("retried on the first route-A resource failure")
	}
	if got := routedDispatchInflightValue(t, reader); got != 0 {
		t.Fatalf("routed_dispatch_inflight after a declined probe = %d, want 0", got)
	}

	// corroboration=2: probe admitted, dispatch begins.
	_, _, _, observeFn, retried := eng.retryOnRouteAResourceFailure(ctx, "promql", plan, seed, nil, chclient.ErrMemoryLimitExceeded)
	if !retried {
		t.Fatal("did not retry on the 2nd consecutive route-A resource failure")
	}
	if got := routedDispatchInflightValue(t, reader); got != 1 {
		t.Errorf("routed_dispatch_inflight while the probe dispatch is outstanding = %d, want 1", got)
	}

	observeFn(nil) // caller's actual drain succeeded cleanly
	if got := routedDispatchInflightValue(t, reader); got != 0 {
		t.Errorf("routed_dispatch_inflight after observeFn released it = %d, want 0", got)
	}
}

// TestRouteABSuccessTotal pins RouteABSuccessTotal's route attribute against
// both classifyRouteOutcome call sites inside this file that can actually
// resolve OutcomeSuccess for a memo-tracked dispatch: route A's own
// classification (a defensive branch — retryOnRouteAResourceFailure is
// documented as called only on failure, but the classification itself does
// not special-case a nil err) and route B's clean-drain observeFn path.
func TestRouteABSuccessTotal(t *testing.T) {
	reader := installMemoWiringTelemetryReader(t)
	plan := memoWiringEligiblePlan()

	t.Run("route-a", func(t *testing.T) {
		cq := &fakeSolverCursorClient{rows: 1}
		eng, _ := newMemoWiringEngine(t, cq)
		seed := memoWiringNotRoutedDecision(t)
		before := routeABSuccessByRoute(t, reader)[telemetry.RouteChoiceA]
		if _, _, _, _, retried := eng.retryOnRouteAResourceFailure(t.Context(), "promql", plan, seed, nil, nil); retried {
			t.Fatal("a Success classification must never itself trigger a retry dispatch")
		}
		after := routeABSuccessByRoute(t, reader)[telemetry.RouteChoiceA]
		if after != before+1 {
			t.Errorf("route=a success count = %d, want %d", after, before+1)
		}
	})

	t.Run("route-b", func(t *testing.T) {
		cq := &fakeSolverCursorClient{rows: 2}
		eng, _ := newMemoWiringEngine(t, cq)
		seed := memoWiringNotRoutedDecision(t)
		ctx := t.Context()
		if _, _, _, _, retried := eng.retryOnRouteAResourceFailure(ctx, "promql", plan, seed, nil, chclient.ErrMemoryLimitExceeded); retried {
			t.Fatal("retried on the first route-A resource failure")
		}
		_, _, _, observeFn, retried := eng.retryOnRouteAResourceFailure(ctx, "promql", plan, seed, nil, chclient.ErrMemoryLimitExceeded)
		if !retried {
			t.Fatal("did not retry on the 2nd consecutive route-A resource failure")
		}
		before := routeABSuccessByRoute(t, reader)[telemetry.RouteChoiceB]
		observeFn(nil) // clean drain
		after := routeABSuccessByRoute(t, reader)[telemetry.RouteChoiceB]
		if after != before+1 {
			t.Errorf("route=b success count = %d, want %d", after, before+1)
		}
	})
}

// TestRouteMemoPressureActive pins RouteMemoPressureActive's transition
// encoding: it moves by exactly one unit on a false->true edge and back by
// one unit on the true->false edge, never drifting further on repeated
// same-state decisions in between (the whole point of encoding it as a
// transition delta rather than a raw per-call snapshot).
func TestRouteMemoPressureActive(t *testing.T) {
	reader := installMemoWiringTelemetryReader(t)
	routeMemoPressureState.Store(false) // pin a known baseline regardless of prior test ordering
	cq := &fakeSolverCursorClient{rows: 1}
	eng, memo := newMemoWiringEngine(t, cq)
	plan := memoWiringEligiblePlan()
	seed := memoWiringNotRoutedDecision(t)

	if got := routeMemoPressureActiveValue(t, reader); got != 0 {
		t.Fatalf("route_memo_pressure_active before any pressure = %d, want 0", got)
	}

	// Two decisions while NOT under pressure: no movement at all.
	eng.deriveRouteMemoDispatch(plan, seed, memoWiringGridEnd.Add(2*memoWiringGridStep))
	eng.deriveRouteMemoDispatch(plan, seed, memoWiringGridEnd.Add(2*memoWiringGridStep))
	if got := routeMemoPressureActiveValue(t, reader); got != 0 {
		t.Fatalf("route_memo_pressure_active after two not-under-pressure decisions = %d, want 0", got)
	}

	const pressureBurstKeys = 8 // comfortably above routememo's pressureFailureThreshold (3)
	for i := 0; i < pressureBurstKeys; i++ {
		k := routememo.Key{RootKind: "*fixture.PressureBurst", RangeFuncs: string(rune('a' + i))}
		memo.Observe(k, routememo.RouteA, routememo.OutcomeResourceFailure)
	}
	if !memo.UnderPressure() {
		t.Fatalf("fixture failed to establish UnderPressure()==true")
	}

	// One decision while under pressure: exactly one +1 transition, even
	// though a second decision immediately after stays at the same level.
	eng.deriveRouteMemoDispatch(plan, seed, memoWiringGridEnd.Add(2*memoWiringGridStep))
	eng.deriveRouteMemoDispatch(plan, seed, memoWiringGridEnd.Add(2*memoWiringGridStep))
	if got := routeMemoPressureActiveValue(t, reader); got != 1 {
		t.Fatalf("route_memo_pressure_active after entering pressure = %d, want 1", got)
	}
}

// --- tests -----------------------------------------------------------------

// TestTryRouteMemoHit_DispatchesOnLivePreferBVerdict pins the core memo-hit
// contract: a live (non-stale) PreferB verdict routes B directly, without
// the caller ever touching route A.
func TestTryRouteMemoHit_DispatchesOnLivePreferBVerdict(t *testing.T) {
	t.Parallel()
	cq := &fakeSolverCursorClient{rows: 3}
	eng, memo := newMemoWiringEngine(t, cq)
	plan := memoWiringEligiblePlan()
	seed := memoWiringNotRoutedDecision(t)

	// Establish the key exactly as the wiring itself would derive it, and
	// seed a live PreferB verdict — the state a prior successful probe or
	// retry would have left behind.
	d := eng.deriveRouteMemoDispatch(plan, seed, memoWiringGridEnd.Add(2*memoWiringGridStep))
	if !d.eligible {
		t.Fatalf("fixture plan must be structurally eligible")
	}
	memo.Observe(d.key, routememo.RouteB, routememo.OutcomeSuccess)

	cur, info, usedDecision, gotKey, ok := eng.tryRouteMemoHit(context.Background(), "promql", plan, seed, nil)
	if !ok {
		t.Fatal("tryRouteMemoHit did not dispatch on a live PreferB verdict")
	}
	if gotKey != d.key {
		t.Errorf("returned key = %+v, want %+v", gotKey, d.key)
	}
	if cur == nil || info == nil || usedDecision == nil {
		t.Fatalf("dispatched=true but cur=%v info=%v usedDecision=%v", cur, info, usedDecision)
	}
	if usedDecision.K < 2 {
		t.Errorf("usedDecision.K = %d, want >= 2 (the FRESHLY derived eligible decision, not the not-routed seed)", usedDecision.K)
	}
	// Execute opens one cursor PER SHARD (kEff of them, not one per dispatch) —
	// the exact count depends on the decision's K clamped by Cfg.Parallel/MaxK,
	// which this test does not pin. >=1 is the meaningful assertion: a dispatch
	// actually happened.
	awaitDispatch(t, cq, "a dispatch must have happened")
}

// TestTryRouteMemoHit_UnknownKeyNeverDispatches pins the negative case: a
// key with no recorded verdict (the common case — every new query shape)
// must never dispatch route B via the memo-hit path.
func TestTryRouteMemoHit_UnknownKeyNeverDispatches(t *testing.T) {
	t.Parallel()
	cq := &fakeSolverCursorClient{rows: 3}
	eng, _ := newMemoWiringEngine(t, cq)
	plan := memoWiringEligiblePlan()
	seed := memoWiringNotRoutedDecision(t)

	_, _, _, _, ok := eng.tryRouteMemoHit(context.Background(), "promql", plan, seed, nil)
	if ok {
		t.Fatal("tryRouteMemoHit dispatched for an Unknown key — must never memo-hit without a recorded verdict")
	}
	if cq.opens.Load() != 0 {
		t.Errorf("Executor cursor opens = %d, want 0 (no dispatch should have been attempted)", cq.opens.Load())
	}
}

// TestTryRouteMemoHit_StaleVerdictFallsBackToCaller pins Major-5's
// re-validation contract: a PreferB verdict that has crossed its
// re-validation midpoint is stale — Lookup reports it, but the memo-hit
// path must NOT act on it; the caller falls through to its own route-A
// dispatch.
func TestTryRouteMemoHit_StaleVerdictFallsBackToCaller(t *testing.T) {
	t.Parallel()
	cq := &fakeSolverCursorClient{rows: 3}
	eng, memo := newMemoWiringEngine(t, cq)
	plan := memoWiringEligiblePlan()
	seed := memoWiringNotRoutedDecision(t)

	fixedNow := memoWiringGridEnd.Add(2 * memoWiringGridStep)
	memo.SetNowForTest(func() time.Time { return fixedNow })
	d := eng.deriveRouteMemoDispatch(plan, seed, fixedNow)
	memo.Observe(d.key, routememo.RouteB, routememo.OutcomeSuccess)

	// Advance past the re-validation midpoint (memoEntryTTL/2) without
	// crossing the TTL itself.
	memo.SetNowForTest(func() time.Time { return fixedNow.Add(20 * time.Minute) })

	_, _, _, _, ok := eng.tryRouteMemoHit(context.Background(), "promql", plan, seed, nil)
	if ok {
		t.Fatal("tryRouteMemoHit dispatched on a STALE PreferB verdict — must fall through to route A instead")
	}
	if cq.opens.Load() != 0 {
		t.Errorf("Executor cursor opens = %d, want 0", cq.opens.Load())
	}
}

// fakeFailingSolverEmitter always fails Emit — models a pre-flight failure
// Executor.Execute CAN detect synchronously (unlike a per-shard QueryCursor
// open error, which happens inside an async runShard goroutine and only
// surfaces later, from the CALLER's drain of the returned cursor).
type fakeFailingSolverEmitter struct{ err error }

func (f fakeFailingSolverEmitter) Emit(context.Context, chplan.Node) (string, []any, error) {
	return "", nil, f.err
}

// TestTryRouteMemoHit_PreFlightFailureFallsBackToRouteA pins the half of
// the symmetric fallback (Major-2) tryRouteMemoHit can decide on its own: a
// PRE-FLIGHT failure (emit/breaker/gate/now64 — the only failure classes
// Executor.Execute's synchronous return can ever carry, see
// classifyRouteOutcome's doc) must report dispatched=false so the caller
// falls through to its own safe route-A path. A per-shard resource failure
// is a DIFFERENT case: Execute opens a streaming cursor asynchronously, so
// that failure only surfaces once the CALLER drains it — which is exactly
// why QueryPlanCursor attaches its own Retry closure to a memo-hit's
// CursorResult rather than expecting tryRouteMemoHit to detect it
// synchronously; that closure is covered at the QueryPlanCursor level.
func TestTryRouteMemoHit_PreFlightFailureFallsBackToRouteA(t *testing.T) {
	t.Parallel()
	cq := &fakeSolverCursorClient{rows: 2}
	cfg := solver.DefaultConfig()
	cfg.Mode = solver.ModeAuto
	sv := solver.New(cfg, fakeFailingSolverEmitter{err: errors.New("emit boom")}, solver.ExecDeps{Client: cq})
	memo := routememo.New(time.Minute)
	eng := &Engine{Solver: sv, RouteMemo: memo}
	plan := memoWiringEligiblePlan()
	seed := memoWiringNotRoutedDecision(t)

	fixedNow := memoWiringGridEnd.Add(2 * memoWiringGridStep)
	d := eng.deriveRouteMemoDispatch(plan, seed, fixedNow)
	memo.Observe(d.key, routememo.RouteB, routememo.OutcomeSuccess)

	cur, info, usedDecision, _, ok := eng.tryRouteMemoHit(context.Background(), "promql", plan, seed, nil)
	if ok {
		t.Fatal("tryRouteMemoHit reported dispatched=true despite a pre-flight emit failure")
	}
	if cur != nil || info != nil || usedDecision != nil {
		t.Errorf("dispatched=false but returned non-nil values: cur=%v info=%v usedDecision=%v", cur, info, usedDecision)
	}
	if cq.opens.Load() != 0 {
		t.Errorf("Executor cursor opens = %d, want 0 — emit fails before any shard is dispatched", cq.opens.Load())
	}
	// A pre-flight failure classifies NoEvidence (never ResourceFailure —
	// see classifyRouteOutcome's doc), so Observe is correctly a no-op: the
	// pre-existing PreferB verdict must be UNCHANGED, not flipped to
	// BothFail on evidence that carries no information about resource use.
	state, stale := memo.Lookup(d.key)
	if state != routememo.PreferB || stale {
		t.Errorf("verdict after a NoEvidence-class pre-flight failure = (%v, stale=%v), want unchanged (PreferB, stale=false)", state, stale)
	}
}

// TestTryRouteMemoHit_MidDrainResourceFailure_ObserveViaClassify pins the
// OTHER half of Major-2: a per-shard resource failure that only surfaces
// once the returned cursor is DRAINED (Execute's own synchronous return
// cannot see it — see the doc on TestTryRouteMemoHit_PreFlightFailureFallsBackToRouteA)
// still ends up correctly classified and recorded once the caller reports
// it — exercising the exact classify-then-Observe sequence QueryPlanCursor's
// Retry closure runs on a memo-hit's drain failure, using the real
// classifyRouteOutcome + Memo.Observe this engine wires together (not a
// re-implementation of them).
func TestTryRouteMemoHit_MidDrainResourceFailure_ObserveViaClassify(t *testing.T) {
	t.Parallel()
	cq := &fakeSolverCursorClient{err: chclient.ErrMemoryLimitExceeded}
	eng, memo := newMemoWiringEngine(t, cq)
	plan := memoWiringEligiblePlan()
	seed := memoWiringNotRoutedDecision(t)

	fixedNow := memoWiringGridEnd.Add(2 * memoWiringGridStep)
	d := eng.deriveRouteMemoDispatch(plan, seed, fixedNow)
	memo.Observe(d.key, routememo.RouteB, routememo.OutcomeSuccess)

	cur, _, _, gotKey, ok := eng.tryRouteMemoHit(context.Background(), "promql", plan, seed, nil)
	if !ok {
		t.Fatal("tryRouteMemoHit must dispatch (Execute's synchronous return has no visibility into a per-shard open error)")
	}
	// Drain the cursor exactly as the real handler would — this is where
	// the shard's QueryCursor error actually surfaces.
	for cur.Next() {
	}
	drainErr := cur.Err()
	if drainErr == nil {
		t.Fatal("fixture must produce a drain error (fakeSolverCursorClient.err is set)")
	}
	// This is what QueryPlanCursor's memo-hit Retry closure does on a drain
	// failure: classify the real error and Observe it.
	memo.Observe(gotKey, routememo.RouteB, classifyRouteOutcome(routememo.RouteB, drainErr))

	state, _ := memo.Lookup(gotKey)
	if state != routememo.BothFail {
		t.Errorf("verdict after the drained route-B resource failure = %v, want BothFail", state)
	}
}

// TestRetryOnRouteAResourceFailure_ProbesAfterCorroboration pins the A->B
// retry's core contract: a route-A resource failure alone is NOT enough —
// it takes minCorroboratingFailures consecutive failures with no
// intervening success before a probe (retry) is admitted.
func TestRetryOnRouteAResourceFailure_ProbesAfterCorroboration(t *testing.T) {
	t.Parallel()
	cq := &fakeSolverCursorClient{rows: 2}
	eng, memo := newMemoWiringEngine(t, cq)
	plan := memoWiringEligiblePlan()
	seed := memoWiringNotRoutedDecision(t)
	ctx := context.Background()

	// First resource failure: corroboration=1, below the threshold — must
	// NOT retry.
	_, _, _, _, retried := eng.retryOnRouteAResourceFailure(ctx, "promql", plan, seed, nil, chclient.ErrMemoryLimitExceeded)
	if retried {
		t.Fatal("retried on the FIRST route-A resource failure — corroboration requires more than one")
	}
	if cq.opens.Load() != 0 {
		t.Errorf("Executor cursor opens after 1st failure = %d, want 0", cq.opens.Load())
	}

	// Second consecutive resource failure, same key, no intervening
	// success: corroboration=2 (the default minCorroboratingFailures) —
	// NOW a probe is admitted and route B dispatches.
	cur, info, usedDecision, observeFn, retried := eng.retryOnRouteAResourceFailure(ctx, "promql", plan, seed, nil, chclient.ErrMemoryLimitExceeded)
	if !retried {
		t.Fatal("did not retry on the 2nd consecutive route-A resource failure")
	}
	if cur == nil || info == nil || usedDecision == nil || observeFn == nil {
		t.Fatalf("retried=true but cur=%v info=%v usedDecision=%v observeFn-is-nil=%v", cur, info, usedDecision, observeFn == nil)
	}
	// Execute opens one cursor PER SHARD, not one per dispatch (see the comment
	// in TestTryRouteMemoHit_DispatchesOnLivePreferBVerdict).
	awaitDispatch(t, cq, "a retry dispatch must have happened")

	// The probe's SUCCESS is what creates the memo's first verdict for this
	// Key (Memo.Observe's OutcomeSuccess branch) — nothing else does. Before
	// the caller reports the drain outcome, the Key must still be Unknown;
	// calling observeFn(nil) (a clean drain) is what must flip it to
	// PreferB. This is the exact mechanism "a query that failed gets
	// retried on route B, and the decision is remembered" the whole
	// failure-driven route memo exists to provide.
	d := eng.deriveRouteMemoDispatch(plan, seed, time.Now())
	if state, _ := memo.Lookup(d.key); state != routememo.Unknown {
		t.Fatalf("verdict BEFORE observeFn is called = %v, want Unknown (nothing should be recorded from Execute's return alone)", state)
	}
	observeFn(nil) // the caller's actual drain succeeded cleanly
	state, stale := memo.Lookup(d.key)
	if state != routememo.PreferB || stale {
		t.Errorf("verdict AFTER observeFn(nil) = (%v, stale=%v), want (PreferB, stale=false)", state, stale)
	}
}

// TestRetryOnRouteAResourceFailure_NonResourceErrorNeverRetried pins
// Major-6: a route-A failure classified as anything OTHER than a resource
// failure (a timeout, a broken connection, a cancellation) must never
// trigger a retry dispatch, regardless of corroboration state.
func TestRetryOnRouteAResourceFailure_NonResourceErrorNeverRetried(t *testing.T) {
	t.Parallel()
	cq := &fakeSolverCursorClient{rows: 2}
	eng, _ := newMemoWiringEngine(t, cq)
	plan := memoWiringEligiblePlan()
	seed := memoWiringNotRoutedDecision(t)
	ctx := context.Background()

	timeoutErr := &solver.SolverTimeoutError{Timeout: "60s"}
	for i := 0; i < 5; i++ {
		_, _, _, _, retried := eng.retryOnRouteAResourceFailure(ctx, "promql", plan, seed, nil, timeoutErr)
		if retried {
			t.Fatalf("iteration %d: retried on a NoEvidence-class error (solver timeout)", i)
		}
	}
	if cq.opens.Load() != 0 {
		t.Errorf("Executor cursor opens = %d, want 0 — no dispatch should ever have been attempted", cq.opens.Load())
	}
}

// TestRetryOnRouteAResourceFailure_ClientGoneNeverRetried pins the
// client-disconnect guard: a cancelled request context must never trigger
// a retry dispatch for a caller that is no longer there to receive it.
func TestRetryOnRouteAResourceFailure_ClientGoneNeverRetried(t *testing.T) {
	t.Parallel()
	cq := &fakeSolverCursorClient{rows: 2}
	eng, _ := newMemoWiringEngine(t, cq)
	plan := memoWiringEligiblePlan()
	seed := memoWiringNotRoutedDecision(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, _, _, retried := eng.retryOnRouteAResourceFailure(ctx, "promql", plan, seed, nil, chclient.ErrMemoryLimitExceeded)
	if retried {
		t.Fatal("retried on a cancelled context — no client is there to receive the retry's answer")
	}
	if cq.opens.Load() != 0 {
		t.Errorf("Executor cursor opens = %d, want 0", cq.opens.Load())
	}
}

// TestRouteMemoWiring_InactiveWhenRouteMemoNil pins the off-by-default
// contract: with RouteMemo nil (the zero value — no cmd/cerberus wiring),
// neither function ever dispatches, regardless of plan/decision/error.
func TestRouteMemoWiring_InactiveWhenRouteMemoNil(t *testing.T) {
	t.Parallel()
	cq := &fakeSolverCursorClient{rows: 2}
	cfg := solver.DefaultConfig()
	cfg.Mode = solver.ModeAuto
	sv := solver.New(cfg, fakeSolverEmitter{}, solver.ExecDeps{Client: cq})
	eng := &Engine{Solver: sv} // RouteMemo left nil
	plan := memoWiringEligiblePlan()
	seed := memoWiringNotRoutedDecision(t)

	if _, _, _, _, ok := eng.tryRouteMemoHit(context.Background(), "promql", plan, seed, nil); ok {
		t.Fatal("tryRouteMemoHit dispatched with RouteMemo nil")
	}
	if _, _, _, _, ok := eng.retryOnRouteAResourceFailure(context.Background(), "promql", plan, seed, nil, chclient.ErrMemoryLimitExceeded); ok {
		t.Fatal("retryOnRouteAResourceFailure dispatched with RouteMemo nil")
	}
	if cq.opens.Load() != 0 {
		t.Errorf("Executor cursor opens = %d, want 0", cq.opens.Load())
	}
}

// TestFreshEnoughForRouteMemo pins the live-edge freshness gate's exact
// boundary: End must have aged past liveEdgeFreshnessMarginSteps*Step as of
// now, not merely be in the past.
func TestFreshEnoughForRouteMemo(t *testing.T) {
	t.Parallel()
	step := 15 * time.Second
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		end  time.Time
		now  time.Time
		want bool
	}{
		{"end exactly at now: live edge, not fresh", base, base, false},
		{"end one step before now: exactly at the margin, fresh", base, base.Add(step), true},
		{"end well in the past: fresh", base, base.Add(time.Hour), true},
		{"end in the future (clock skew / clamp bug): not fresh", base, base.Add(-time.Second), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := freshEnoughForRouteMemo(tc.end, step, tc.now)
			if got != tc.want {
				t.Errorf("freshEnoughForRouteMemo(end=%v, step=%v, now=%v) = %v, want %v", tc.end, step, tc.now, got, tc.want)
			}
		})
	}
}

// TestFreshEnoughForRouteMemo_ZeroStepNeverFresh pins a defensive edge: a
// zero step (should not occur for a plan that passed Eligible, but a
// dispatch-site bug must fail closed, not divide/multiply into a bogus
// margin).
func TestFreshEnoughForRouteMemo_ZeroStepNeverFresh(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if freshEnoughForRouteMemo(now.Add(-time.Hour), 0, now) {
		t.Fatal("freshEnoughForRouteMemo reported fresh with step=0 — must fail closed")
	}
}

// dispatchAwaitBudget / dispatchAwaitPoll bound awaitDispatch's poll. The
// budget is generous relative to the microseconds a fake dispatch actually
// takes, so a loaded CI runner cannot flake it; the poll is short enough that a
// passing run costs nothing.
const (
	dispatchAwaitBudget = 2 * time.Second
	dispatchAwaitPoll   = time.Millisecond
)

// awaitDispatch waits for the Executor to have opened at least one shard
// cursor. Execute hands its shards to a launcher goroutine and returns without
// waiting for any of them to be admitted — it MUST, since a shard that has
// filled its channel can only be unparked by a composer that cannot run until
// Execute has returned. So the opens have not necessarily happened yet at the
// instant Execute returns. Polling for the observable effect is the honest form
// of "a dispatch happened"; sampling the counter on return tests goroutine
// scheduling instead.
func awaitDispatch(t *testing.T, cq *fakeSolverCursorClient, what string) {
	t.Helper()
	deadline := time.Now().Add(dispatchAwaitBudget)
	for time.Now().Before(deadline) {
		if cq.opens.Load() >= 1 {
			return
		}
		time.Sleep(dispatchAwaitPoll)
	}
	t.Fatalf("Executor cursor opens = %d, want >= 1 (%s)", cq.opens.Load(), what)
}
