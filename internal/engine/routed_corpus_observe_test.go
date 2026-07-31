package engine

import (
	"context"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/solver"
	"github.com/tsouza/cerberus/internal/telemetry"
)

// routedCorpusLang is a minimal Lang: the routed paths only read Name(), and
// classify() gates on it being the PromQL head.
type routedCorpusLang struct{}

func (routedCorpusLang) Name() string { return solver.LangPromQL }

func (routedCorpusLang) Parse(context.Context, string) (chplan.Node, Meta, error) {
	return memoWiringEligiblePlan(), Meta{IsMetric: true}, nil
}

func (routedCorpusLang) ProjectSamples(plan chplan.Node, _ Meta) chplan.Node { return plan }

// routedCorpusObserver records both corpus seams separately so a test can tell
// a route-A observation from a route-B one — and catch a dispatch recorded on
// both.
type routedCorpusObserver struct {
	mu sync.Mutex

	routeAIDs []string

	routedIDs      [][]string
	routedParallel []int
	routedShapes   []string
	routedLangs    []string
	routedPresent  []bool
	routedRoutes   []string
	routedReasons  []string
	routedKShards  []uint8
	outcomes       [][2]string
}

func (o *routedCorpusObserver) ObserveQuery(
	queryID, _ string, _ []string, _ string,
	_ bool, _ string, _, _, _, _, _ uint32, _ uint8, _ string,
) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.routeAIDs = append(o.routeAIDs, queryID)
}

func (o *routedCorpusObserver) ObserveRoutedQuery(
	shardQueryIDs []string, parallelism int, shapeID string, _ []string, language string,
	routePresent bool, route string, _, _, _, _, _ uint32, kShards uint8, decisionReason string,
) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.routedIDs = append(o.routedIDs, append([]string(nil), shardQueryIDs...))
	o.routedParallel = append(o.routedParallel, parallelism)
	o.routedShapes = append(o.routedShapes, shapeID)
	o.routedLangs = append(o.routedLangs, language)
	o.routedPresent = append(o.routedPresent, routePresent)
	o.routedRoutes = append(o.routedRoutes, route)
	o.routedReasons = append(o.routedReasons, decisionReason)
	o.routedKShards = append(o.routedKShards, kShards)
}

// assertRoutedRowIdentity pins the columns every routed observation carries
// regardless of which of the three route-B seams produced it: the language the
// row is filed under (a wrong one silos the row into a population no PromQL
// calibration reads), the real K, and the executor's effective parallelism
// (which the observer folds the wall-clock and memory columns at).
//
// wantLang is a parameter rather than a fixed solver.LangPromQL because a seam
// that ignored its language argument and stamped every row with the PromQL head
// would satisfy a hard-coded expectation. The seam that takes the language as a
// free argument is exercised with a different head below, so the assertion
// distinguishes "forwards what it was given" from "always says promql".
func (o *routedCorpusObserver) assertRoutedRowIdentity(
	t *testing.T, i int, wantLang string, wantK uint8, wantParallel int,
) {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.routedLangs[i] != wantLang {
		t.Errorf("language = %q; want %q — the row must be filed under the head that ran it",
			o.routedLangs[i], wantLang)
	}
	if o.routedKShards[i] != wantK {
		t.Errorf("k_shards = %d; want %d (the decision's real K)", o.routedKShards[i], wantK)
	}
	if o.routedParallel[i] != wantParallel {
		t.Errorf("parallelism = %d; want %d (the executor's effective P)", o.routedParallel[i], wantParallel)
	}
}

func (o *routedCorpusObserver) ObserveOutcome(queryID, statusToken string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.outcomes = append(o.outcomes, [2]string{queryID, statusToken})
}

func (o *routedCorpusObserver) ObserveRejection(
	string, []string, string, string, bool, string, uint32, uint32, uint32, uint32, uint32, uint8, string,
) {
}

func (o *routedCorpusObserver) ObserveDispatchedRejection(
	string, string, []string, string, string, bool, string,
	uint32, uint32, uint32, uint32, uint32, uint8, string,
) {
}

func (o *routedCorpusObserver) snapshot() ([]string, [][]string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.routeAIDs...), append([][]string(nil), o.routedIDs...)
}

// idCapturingCursorClient is fakeSolverCursorClient plus the ClickHouse
// query_id each shard's dispatch context actually carries — the id
// system.query_log would record for that shard.
type idCapturingCursorClient struct {
	rows int

	mu  sync.Mutex
	ids []string
}

func (c *idCapturingCursorClient) QueryCursor(ctx context.Context, _ string, _ ...any) (chclient.Cursor, error) {
	c.mu.Lock()
	c.ids = append(c.ids, chclient.QueryIDFromContext(ctx))
	c.mu.Unlock()
	return &fakeMemoWiringCursor{remaining: c.rows}, nil
}

func (c *idCapturingCursorClient) MaxQueryMemoryBytes() int64 { return 0 }

func (c *idCapturingCursorClient) dispatchedIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.ids...)
}

// routedCorpusTraceCtx returns a ctx carrying a real span context, which is
// what the per-shard query_ids are minted from.
func routedCorpusTraceCtx() context.Context {
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{
			0xa1, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 0xa7, 0xa8,
			0xa9, 0xaa, 0xab, 0xac, 0xad, 0xae, 0xaf, 0xb0,
		},
		SpanID: trace.SpanID{0xb1, 0xb2, 0xb3, 0xb4, 0xb5, 0xb6, 0xb7, 0xb8},
	})
	return trace.ContextWithSpanContext(context.Background(), sc)
}

// newRoutedCorpusEngine builds an Engine whose Solver routes every eligible
// plan (ModeSharded floors the cost thresholds), with the corpus observer
// attached.
func newRoutedCorpusEngine(t *testing.T, cursorClient solver.CursorQuerier, obs QueryObserver) *Engine {
	t.Helper()
	cfg := solver.DefaultConfig()
	cfg.Mode = solver.ModeSharded
	if err := cfg.Validate(); err != nil {
		t.Fatalf("solver config invalid: %v", err)
	}
	sv := solver.New(cfg, fakeSolverEmitter{}, solver.ExecDeps{Client: cursorClient})
	eng := &Engine{Solver: sv}
	if obs != nil {
		eng.QueryObserver = obs
	}
	return eng
}

// wantParallelism is the effective P the Executor reports for d under this
// file's wiring (no admission top-up, no concurrency gate): the configured P,
// clamped down to K because a fan-out cannot run more shards than it has.
func wantParallelism(eng *Engine, d *solver.Decision) int {
	p := eng.Solver.Executor.Cfg.Parallel
	if len(d.Slices) < p {
		p = len(d.Slices)
	}
	return p
}

// fixtureShardCount is the K the eligible fixture plan slices into under
// ModeSharded. Every K-shaped assertion below compares the observed column
// against the decision's own len(Slices), which is the right question to ask of
// the seam but says nothing about the fixture: were the planner to start
// slicing this plan into a single shard, the whole route-B observation family
// would still pass while testing no fan-out at all. Pinning the number here
// turns that degeneration into a failure at the one place it originates.
const fixtureShardCount = 8

// routedDecision classifies the eligible fixture plan into the routed decision
// the engine's own route-B paths take.
func routedDecision(t *testing.T, eng *Engine, plan chplan.Node) *solver.Decision {
	t.Helper()
	d, routed := eng.classify(plan, routedCorpusLang{})
	if !routed || d == nil {
		t.Fatalf("fixture must classify as ROUTED under ModeSharded; got routed=%v decision=%+v", routed, d)
	}
	if d.Reason != solver.ReasonRouted {
		t.Fatalf("routed decision reason = %q, want %q", d.Reason, solver.ReasonRouted)
	}
	if len(d.Slices) != fixtureShardCount {
		t.Fatalf("fixture sliced into K=%d; want K=%d — a route-B fan-out test needs a real fan-out",
			len(d.Slices), fixtureShardCount)
	}
	return d
}

// TestExecuteRoutedCursor_ObservesTheFanOut pins the defect this seam exists to
// close: before it, observeQuery was reachable only from the route-A dispatch
// path, so a route-B query could never produce a corpus row and an empty
// route-B population read as "the router never routes" when it actually meant
// "a routed query is unobservable".
//
// Revert the observeRoutedQuery call in executeRoutedCursor and this fails on
// the very first assertion: zero routed observations for a dispatch that ran K
// shards. It also pins the property that makes the row usable — the observed
// ids are EXACTLY the ids ClickHouse ran the shards under, so the reconciler's
// system.query_log join resolves.
func TestExecuteRoutedCursor_ObservesTheFanOut(t *testing.T) {
	t.Parallel()

	cq := &idCapturingCursorClient{rows: 1}
	obs := &routedCorpusObserver{}
	eng := newRoutedCorpusEngine(t, cq, obs)
	plan := memoWiringEligiblePlan()
	d := routedDecision(t, eng, plan)

	cr, err := eng.executeRoutedCursor(routedCorpusTraceCtx(), routedCorpusLang{}, Meta{IsMetric: true}, plan, d)
	if err != nil {
		t.Fatalf("executeRoutedCursor: %v", err)
	}
	for cr.Cursor.Next() {
	}
	if err := cr.Cursor.Err(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if err := cr.Cursor.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	routeA, routed := obs.snapshot()
	if len(routed) != 1 {
		t.Fatalf("routed observations = %d; want exactly 1 (one row per routed QUERY)", len(routed))
	}
	if len(routeA) != 0 {
		t.Errorf("route-A observations = %d; want 0 (a routed dispatch must not also be recorded as route A)", len(routeA))
	}

	ids := routed[0]
	if len(ids) != len(d.Slices) {
		t.Errorf("observed %d shard ids; want %d (one per shard, so the join covers the whole fan-out)", len(ids), len(d.Slices))
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" {
			t.Fatalf("observed an empty shard query_id: %v", ids)
		}
		if seen[id] {
			t.Fatalf("shard query_id %q observed twice; ClickHouse rejects a duplicate live id", id)
		}
		seen[id] = true
	}

	// The ids the corpus recorded must be the ids the shards actually ran under,
	// or the query_log join silently matches nothing.
	dispatched := cq.dispatchedIDs()
	if len(dispatched) != len(ids) {
		t.Fatalf("ClickHouse saw %d shard dispatches; the corpus recorded %d ids", len(dispatched), len(ids))
	}
	for _, id := range dispatched {
		if !seen[id] {
			t.Errorf("shard dispatched with query_id %q, which the corpus never recorded (join would miss it)", id)
		}
	}

	if !obs.routedPresent[0] || obs.routedRoutes[0] != corpusRouteB {
		t.Errorf("routing read-out = (present=%v route=%q); want (true, %q)",
			obs.routedPresent[0], obs.routedRoutes[0], corpusRouteB)
	}
	if obs.routedReasons[0] != solver.ReasonRouted {
		t.Errorf("decision_reason = %q; want %q", obs.routedReasons[0], solver.ReasonRouted)
	}
	obs.assertRoutedRowIdentity(t, 0, solver.LangPromQL, clampU8(int64(len(d.Slices))), wantParallelism(eng, d))
	if obs.routedShapes[0] != planShapeID(plan) {
		t.Errorf("shape_id = %q; want the WHOLE plan's %q, so route-A and route-B rows join",
			obs.routedShapes[0], planShapeID(plan))
	}

	// The observer folds the K shards' wall-clock and memory into one row, which
	// it can only do knowing how many of them overlapped. Hand it 0 (or K) and
	// the fold reads a K=8/P=3 fan-out as if all eight shards ran at once:
	// route-B latency understated and memory overstated by roughly K/P.
	wantP := solver.DefaultConfig().Parallel
	if got := obs.routedParallel[0]; got != wantP {
		t.Errorf("observed parallelism = %d; want the Executor's effective P (%d)", got, wantP)
	}
	if obs.routedParallel[0] >= len(ids) {
		t.Errorf("observed parallelism %d >= %d shards; the fixture exists because P below K is the routine shape",
			obs.routedParallel[0], len(ids))
	}
}

// TestExecuteRoutedCursor_DrainOutcomeUsesLogicalRouteBRecord pins the cursor
// drain seam: a route-B CursorResult exposes one of its shard ids, and that id
// addresses the one logical record ObserveRoutedQuery already created.
func TestExecuteRoutedCursor_DrainOutcomeUsesLogicalRouteBRecord(t *testing.T) {
	t.Parallel()

	cq := &idCapturingCursorClient{rows: 1}
	obs := &routedCorpusObserver{}
	eng := newRoutedCorpusEngine(t, cq, obs)
	plan := memoWiringEligiblePlan()
	d := routedDecision(t, eng, plan)

	cr, err := eng.executeRoutedCursor(routedCorpusTraceCtx(), routedCorpusLang{}, Meta{IsMetric: true}, plan, d)
	if err != nil {
		t.Fatalf("executeRoutedCursor: %v", err)
	}
	defer func() { _ = cr.Cursor.Close() }()

	_, routed := obs.snapshot()
	if len(routed) != 1 {
		t.Fatalf("routed observations = %d; want 1", len(routed))
	}
	if cr.QueryID == "" {
		t.Fatal("routed cursor QueryID is empty; drain outcomes cannot reach the logical route-B record")
	}
	if !containsID(routed[0], cr.QueryID) {
		t.Fatalf("routed cursor QueryID %q is not one of the logical record's shard ids %v", cr.QueryID, routed[0])
	}

	eng.ObserveDrainOutcome(cr.QueryID, solver.LangPromQL, chclient.ErrTooManySamples)
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.outcomes) != 1 {
		t.Fatalf("drain outcomes = %d; want 1", len(obs.outcomes))
	}
	if got := obs.outcomes[0]; got != [2]string{cr.QueryID, optcorpusExitSampleBudget} {
		t.Errorf("drain outcome = %v; want [%q %q]", got, cr.QueryID, optcorpusExitSampleBudget)
	}
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// TestExecuteRouted_ObservesTheFanOut pins the same contract on the eager
// route-B path (QueryPlan's sibling of the streaming one). The two paths return
// through different code, so wiring one and not the other leaves half of route
// B invisible — reverting executeRouted's call makes this fail with zero routed
// observations while the streaming test above still passes.
func TestExecuteRouted_ObservesTheFanOut(t *testing.T) {
	t.Parallel()

	cq := &idCapturingCursorClient{rows: 2}
	obs := &routedCorpusObserver{}
	eng := newRoutedCorpusEngine(t, cq, obs)
	plan := memoWiringEligiblePlan()
	d := routedDecision(t, eng, plan)

	res, err := eng.executeRouted(routedCorpusTraceCtx(), routedCorpusLang{}, Meta{IsMetric: true}, plan, d)
	if err != nil {
		t.Fatalf("executeRouted: %v", err)
	}
	if len(res.Samples) == 0 {
		t.Fatal("eager routed execution returned no samples; the fixture streams rows per shard")
	}

	routeA, routed := obs.snapshot()
	if len(routed) != 1 {
		t.Fatalf("routed observations = %d; want exactly 1", len(routed))
	}
	if len(routeA) != 0 {
		t.Errorf("route-A observations = %d; want 0", len(routeA))
	}
	if len(routed[0]) != len(d.Slices) {
		t.Errorf("observed %d shard ids; want %d", len(routed[0]), len(d.Slices))
	}
	obs.assertRoutedRowIdentity(t, 0, solver.LangPromQL, clampU8(int64(len(d.Slices))), wantParallelism(eng, d))
}

// TestBuildRoutedCursorResult_ObservesOnce pins the third route-B family: the
// memo-hit and the two A->B retry sites all return through
// buildRoutedCursorResult, which is therefore their single corpus seam. It is
// also the double-record guard: executeRoutedCursor records for itself and
// never calls here, so exactly one observation exists per physical dispatch.
func TestBuildRoutedCursorResult_ObservesOnce(t *testing.T) {
	t.Parallel()

	cq := &idCapturingCursorClient{rows: 1}
	obs := &routedCorpusObserver{}
	eng := newRoutedCorpusEngine(t, cq, obs)
	plan := memoWiringEligiblePlan()
	d := routedDecision(t, eng, plan)

	cursor, info, err := eng.Solver.Executor.Execute(routedCorpusTraceCtx(), solver.LangPromQL, d, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	defer func() { _ = cursor.Close() }()

	// The language reaches this seam as a plain argument, so it is the one place
	// that can prove the column carries what the caller passed rather than a
	// stamped-in head name. Feed it a head the rest of this file never produces:
	// the two engine-driven tests above can only ever be promql (classify reads
	// the fixture Lang's own Name()), so a hard-coded "promql" would satisfy them.
	cr := eng.buildRoutedCursorResult(Meta{IsMetric: true}, plan, telemetry.QLLogQL, d, cursor, info, "memo-hit")
	if cr.Cursor == nil {
		t.Fatal("buildRoutedCursorResult returned no cursor")
	}

	routeA, routed := obs.snapshot()
	if len(routed) != 1 {
		t.Fatalf("routed observations = %d; want exactly 1", len(routed))
	}
	if len(routeA) != 0 {
		t.Errorf("route-A observations = %d; want 0", len(routeA))
	}
	// The corpus carries the CLASSIFIER's vocabulary, not the response header's
	// "memo-hit"/"retry" label: "route B iff decision_reason=routed" is a pinned
	// corpus invariant (test/regression/router_corpus_seed_test.go).
	if obs.routedReasons[0] != solver.ReasonRouted {
		t.Errorf("decision_reason = %q; want %q even on a memo-hit dispatch", obs.routedReasons[0], solver.ReasonRouted)
	}
	// This site holds the ExecInfo the Executor actually produced, so it pins
	// parallelism against the executor's own read-out rather than a recomputed
	// expectation — the row must carry what the fan-out really ran at.
	obs.assertRoutedRowIdentity(t, 0, telemetry.QLLogQL, clampU8(int64(len(d.Slices))), info.Parallelism)
	if info.Parallelism < 1 {
		t.Fatalf("ExecInfo.Parallelism = %d; the fixture must exercise a real concurrency read-out", info.Parallelism)
	}
}

// TestExecuteRoutedCursor_UntracedRequestStillSucceeds pins the "recording can
// never fail the query" half of the seam's contract at its one branch that can
// legitimately produce nothing: without a trace context there is no query_id to
// mint, so there is no join key and nothing is recorded — and the routed query
// must still run and stream its rows.
func TestExecuteRoutedCursor_UntracedRequestStillSucceeds(t *testing.T) {
	t.Parallel()

	cq := &idCapturingCursorClient{rows: 1}
	obs := &routedCorpusObserver{}
	eng := newRoutedCorpusEngine(t, cq, obs)
	plan := memoWiringEligiblePlan()
	d := routedDecision(t, eng, plan)

	cr, err := eng.executeRoutedCursor(context.Background(), routedCorpusLang{}, Meta{IsMetric: true}, plan, d)
	if err != nil {
		t.Fatalf("executeRoutedCursor without a trace: %v", err)
	}
	rows := 0
	for cr.Cursor.Next() {
		rows++
	}
	if err := cr.Cursor.Err(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if err := cr.Cursor.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if rows == 0 {
		t.Error("untraced routed query streamed no rows; the corpus seam must never change what the query returns")
	}

	if _, routed := obs.snapshot(); len(routed) != 0 {
		t.Errorf("routed observations = %d for an untraced request; want 0 (no join key exists)", len(routed))
	}
}

// TestExecuteRoutedCursor_NoObserverStillSucceeds pins the default deployment:
// with no corpus wired (QueryObserver nil — the zero value), the routed path
// must behave exactly as before the seam existed.
func TestExecuteRoutedCursor_NoObserverStillSucceeds(t *testing.T) {
	t.Parallel()

	cq := &idCapturingCursorClient{rows: 1}
	eng := newRoutedCorpusEngine(t, cq, nil)
	plan := memoWiringEligiblePlan()
	d := routedDecision(t, eng, plan)

	cr, err := eng.executeRoutedCursor(routedCorpusTraceCtx(), routedCorpusLang{}, Meta{IsMetric: true}, plan, d)
	if err != nil {
		t.Fatalf("executeRoutedCursor with no observer: %v", err)
	}
	rows := 0
	for cr.Cursor.Next() {
		rows++
	}
	if err := cr.Cursor.Err(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if err := cr.Cursor.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if rows == 0 {
		t.Error("routed query with no observer streamed no rows")
	}
}
