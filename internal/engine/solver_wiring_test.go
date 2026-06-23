package engine_test

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/solver"
)

// matrixGrid is the deterministic eval grid the wiring tests anchor on. The
// outermost RangeWindow carries it pinned so the solver's GridOf reads back
// exactly (meta.Start/End/Step) and the Planner sees a grid-matched,
// slice-invariant, eligible plan — which Mode=single classifies WITHOUT
// routing (Reason=below-threshold).
var (
	gridStart = time.Date(2026, 6, 13, 0, 0, 0, 0, time.UTC)
	gridStep  = 15 * time.Second
	gridOuter = time.Hour
	gridEnd   = gridStart.Add(gridOuter)
)

// matrixPlan builds an eligible matrix RangeWindow(rate) over a Scan, pinned
// to matrixGrid. Both engines (nil-Solver and Mode=single Solver) run it
// through the identical optimize+emit path, so any Result difference is the
// solver's doing.
func matrixPlan() chplan.Node {
	return &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_sum"},
		Func:            "rate",
		Range:           5 * time.Minute,
		Step:            gridStep,
		OuterRange:      gridOuter,
		Start:           gridStart,
		End:             gridEnd,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
}

// matrixLang returns matrixPlan from Parse with an identity ProjectSamples,
// so the plan reaching the seam is exactly matrixPlan().
func matrixLang() *fakeLang {
	return &fakeLang{
		name: "promql",
		parseFn: func(context.Context, string) (chplan.Node, engine.Meta, error) {
			return matrixPlan(), engine.Meta{IsMetric: true, ResponseShape: "prom-matrix"}, nil
		},
	}
}

// singleModeSolver builds a Mode=single Solver whose Executor's cursor client
// is the supplied recorder, so a test can prove the Executor is NEVER
// invoked on the dark path.
func singleModeSolver(t *testing.T, cursorClient solver.CursorQuerier) *solver.Solver {
	t.Helper()
	cfg := solver.DefaultConfig() // Mode == single
	if err := cfg.Validate(); err != nil {
		t.Fatalf("DefaultConfig invalid: %v", err)
	}
	return solver.New(cfg, engine.ChsqlEmitter{}, solver.ExecDeps{Client: cursorClient})
}

// recordingCursorClient counts QueryCursor opens so the no-invoke pin can
// assert the Executor's data plane was never touched under Mode=single.
type recordingCursorClient struct{ opens int }

func (r *recordingCursorClient) QueryCursor(context.Context, string, ...any) (chclient.Cursor, error) {
	r.opens++
	return nil, context.Canceled
}

// TestSolver_SingleMode_ByteIdenticalToNilSolver is the BYTE-REPRO PIN: with
// the default Mode=single Solver, the engine's Result — body (Samples / SQL /
// Args / Strategy / PlanNodeCount / Meta) and ALL pre-existing headers — is
// byte-identical to the nil-Solver (pre-solver) path. The ONLY permitted
// difference is the additive X-Cerberus-Route-Decision shadow header.
func TestSolver_SingleMode_ByteIdenticalToNilSolver(t *testing.T) {
	t.Parallel()

	rows := []chclient.Sample{
		{MetricName: "up", Labels: map[string]string{"job": "a"}, Timestamp: time.Unix(1, 0), Value: 1},
	}

	// Pre-solver baseline: nil Solver.
	baseQ := &fakeQuerier{rows: rows}
	baseEng := &engine.Engine{Optimizer: optimizer.Default(), Client: baseQ}
	baseRes, err := baseEng.Query(context.Background(), matrixLang(), "rate(up[5m])")
	if err != nil {
		t.Fatalf("baseline Query: %v", err)
	}

	// Mode=single Solver over the same plan.
	solQ := &fakeQuerier{rows: rows}
	rec := &recordingCursorClient{}
	solEng := &engine.Engine{
		Optimizer: optimizer.Default(),
		Client:    solQ,
		Solver:    singleModeSolver(t, rec),
	}
	solRes, err := solEng.Query(context.Background(), matrixLang(), "rate(up[5m])")
	if err != nil {
		t.Fatalf("solver Query: %v", err)
	}

	// Body fields must match exactly.
	if solRes.SQL != baseRes.SQL {
		t.Errorf("SQL diverged:\n base=%q\n  sol=%q", baseRes.SQL, solRes.SQL)
	}
	if len(solRes.Args) != len(baseRes.Args) {
		t.Errorf("Args len: base=%d sol=%d", len(baseRes.Args), len(solRes.Args))
	}
	if solRes.Strategy != baseRes.Strategy {
		t.Errorf("Strategy: base=%q sol=%q", baseRes.Strategy, solRes.Strategy)
	}
	if solRes.PlanNodeCount != baseRes.PlanNodeCount {
		t.Errorf("PlanNodeCount: base=%d sol=%d", baseRes.PlanNodeCount, solRes.PlanNodeCount)
	}
	if len(solRes.Samples) != len(baseRes.Samples) {
		t.Errorf("Samples len: base=%d sol=%d", len(baseRes.Samples), len(solRes.Samples))
	}
	if solRes.Meta.IsMetric != baseRes.Meta.IsMetric ||
		solRes.Meta.IsTraceByID != baseRes.Meta.IsTraceByID ||
		solRes.Meta.ResponseShape != baseRes.Meta.ResponseShape {
		t.Errorf("Meta: base=%+v sol=%+v", baseRes.Meta, solRes.Meta)
	}

	// Headers must match for EVERY pre-existing key; the shadow header is
	// the only addition. HeaderCHMillis is excluded: it's a measured CH
	// wall-clock duration, so base vs sol differ run-to-run (e.g. "0" vs
	// "1") — that's latency, not a divergence in the byte-identical OUTPUT
	// invariant this test guards.
	for k, bv := range baseRes.Headers {
		if k == engine.HeaderCHMillis {
			continue
		}
		if sv, ok := solRes.Headers[k]; !ok || sv != bv {
			t.Errorf("header %q: base=%q sol=%q (present=%v)", k, bv, sv, ok)
		}
	}
	extra := maps.Clone(solRes.Headers)
	for k := range baseRes.Headers {
		delete(extra, k)
	}
	if len(extra) != 1 {
		t.Fatalf("solver added %d headers beyond baseline, want exactly 1 (the shadow header): %v", len(extra), extra)
	}
	if _, ok := extra[engine.HeaderRouteDecision]; !ok {
		t.Fatalf("the one extra header is not %s: %v", engine.HeaderRouteDecision, extra)
	}

	// The Executor's data plane was NEVER touched: route A executed.
	if rec.opens != 0 {
		t.Errorf("Executor cursor opens under Mode=single: got %d, want 0", rec.opens)
	}
	if solQ.calls != 1 {
		t.Errorf("route-A Querier calls: got %d, want 1 (single == today)", solQ.calls)
	}
}

// TestSolver_SingleMode_ShadowHeaderReportsClassification pins the dark
// contract: the shadow header REPORTS the classification (route-a;reason=...)
// while route A executes and the Executor is never invoked.
func TestSolver_SingleMode_ShadowHeaderReportsClassification(t *testing.T) {
	t.Parallel()

	q := &fakeQuerier{rows: []chclient.Sample{{MetricName: "up", Timestamp: time.Unix(1, 0), Value: 1}}}
	rec := &recordingCursorClient{}
	eng := &engine.Engine{
		Optimizer: optimizer.Default(),
		Client:    q,
		Solver:    singleModeSolver(t, rec),
	}

	res, err := eng.Query(context.Background(), matrixLang(), "rate(up[5m])")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	got, ok := res.Headers[engine.HeaderRouteDecision]
	if !ok {
		t.Fatalf("shadow header %s missing; headers=%v", engine.HeaderRouteDecision, res.Headers)
	}
	// Mode=single classifies an eligible plan as route-a / below-threshold.
	const want = "route-a;reason=" + solver.ReasonBelowThreshold
	if got != want {
		t.Errorf("shadow header: got %q, want %q", got, want)
	}
	if rec.opens != 0 {
		t.Errorf("Executor invoked under Mode=single: cursor opens=%d, want 0", rec.opens)
	}
	if q.calls != 1 {
		t.Errorf("route-A Querier calls: got %d, want 1", q.calls)
	}
}

// TestSolver_NonPromQLHead_NoShadowHeader pins the Lang gate: a non-PromQL
// head skips the solver entirely, so the shadow header is OMITTED and the
// response is byte-identical to the nil-Solver path.
func TestSolver_NonPromQLHead_NoShadowHeader(t *testing.T) {
	t.Parallel()

	q := &fakeQuerier{rows: []chclient.Sample{{MetricName: "up", Timestamp: time.Unix(1, 0), Value: 1}}}
	rec := &recordingCursorClient{}
	eng := &engine.Engine{
		Optimizer: optimizer.Default(),
		Client:    q,
		Solver:    singleModeSolver(t, rec),
	}

	lang := matrixLang()
	lang.name = "logql" // not the PromQL head — classification is bypassed.

	res, err := eng.Query(context.Background(), lang, "rate(up[5m])")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if _, ok := res.Headers[engine.HeaderRouteDecision]; ok {
		t.Errorf("shadow header present for non-PromQL head; want omitted: %v", res.Headers)
	}
	if rec.opens != 0 {
		t.Errorf("Executor invoked for non-PromQL head: opens=%d", rec.opens)
	}
}

// TestSolver_GridOf_InstantPlanZeroGrid pins that an instant / non-range plan
// yields a zero grid (Step == 0), which the Planner routes A on the instant
// guard — and the shadow header reports reason=instant.
func TestSolver_GridOf_InstantPlanZeroGrid(t *testing.T) {
	t.Parallel()

	start, end, step := solver.GridOf(&chplan.Scan{Table: "otel_metrics_sum"})
	if step != 0 || !start.IsZero() || !end.IsZero() {
		t.Fatalf("GridOf(Scan): got start=%v end=%v step=%v, want zero grid", start, end, step)
	}

	q := &fakeQuerier{rows: []chclient.Sample{{MetricName: "up", Timestamp: time.Unix(1, 0), Value: 1}}}
	eng := &engine.Engine{
		Optimizer: optimizer.Default(),
		Client:    q,
		Solver:    singleModeSolver(t, &recordingCursorClient{}),
	}
	// A bare Scan plan (instant shape) — the default fakeLang returns Scan.
	res, err := eng.Query(context.Background(), &fakeLang{name: "promql"}, "up")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	got := res.Headers[engine.HeaderRouteDecision]
	if want := "route-a;reason=" + solver.ReasonInstant; got != want {
		t.Errorf("instant shadow header: got %q, want %q", got, want)
	}
}
