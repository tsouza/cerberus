package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/optimizer"
)

// fakeLang is a stand-in for a real per-language adapter. Each callback
// is a hook the tests can rebind to exercise specific pipeline stages.
type fakeLang struct {
	name         string
	parseFn      func(ctx context.Context, q string) (chplan.Node, engine.Meta, error)
	projectFn    func(plan chplan.Node, meta engine.Meta) chplan.Node
	parseCalls   int
	projectCalls int
}

func (f *fakeLang) Name() string { return f.name }

func (f *fakeLang) Parse(ctx context.Context, query string) (chplan.Node, engine.Meta, error) {
	f.parseCalls++
	if f.parseFn != nil {
		return f.parseFn(ctx, query)
	}
	return &chplan.Scan{Table: "otel_metrics_gauge"}, engine.Meta{IsMetric: true, ResponseShape: "prom-vector"}, nil
}

func (f *fakeLang) ProjectSamples(plan chplan.Node, meta engine.Meta) chplan.Node {
	f.projectCalls++
	if f.projectFn != nil {
		return f.projectFn(plan, meta)
	}
	return plan
}

// fakeQuerier captures the SQL the engine emitted and returns a fixed
// row set. It also remembers how many times it was called so the
// short-circuit tests can assert it was bypassed.
type fakeQuerier struct {
	rows       []chclient.Sample
	err        error
	gotSQL     string
	gotArgs    []any
	calls      int
	captureCtx func(context.Context)
}

func (f *fakeQuerier) Query(ctx context.Context, sql string, args ...any) ([]chclient.Sample, error) {
	f.calls++
	f.gotSQL = sql
	f.gotArgs = args
	if f.captureCtx != nil {
		f.captureCtx(ctx)
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

func newEngine(q *fakeQuerier) *engine.Engine {
	return &engine.Engine{
		Optimizer: optimizer.Default(),
		Client:    q,
	}
}

// recordingObserver captures the query_id and routing read-out the engine
// hands the corpus reconciler at the dispatch seam, so a test can assert the id
// equals the one the chclient query path observes on the same dispatch ctx and
// that the routing features ride along.
type recordingObserver struct {
	queryIDs     []string
	routePresent []bool
	routes       []string
	decisionRsns []string
	lastNAnchors uint32
	lastFanout   uint32
	lastCumD     uint32
	lastOuterRng uint32
	lastStep     uint32
	lastKShards  uint8

	// outcomes captures (queryID, token) pairs from ObserveOutcome; rejections
	// captures (language, token) pairs from ObserveRejection; dispatchedRej
	// captures (queryID, language, token) triples from ObserveDispatchedRejection
	// — the cerberus-side terminal-outcome seams.
	outcomes      [][2]string
	rejections    [][2]string
	dispatchedRej [][3]string
}

func (o *recordingObserver) ObserveQuery(
	queryID, _ string,
	_ []string,
	_ string,
	routePresent bool,
	route string,
	nAnchors, fanout, cumulativeD, outerRange, step uint32,
	kShards uint8,
	decisionReason string,
) {
	o.queryIDs = append(o.queryIDs, queryID)
	o.routePresent = append(o.routePresent, routePresent)
	o.routes = append(o.routes, route)
	o.decisionRsns = append(o.decisionRsns, decisionReason)
	o.lastNAnchors = nAnchors
	o.lastFanout = fanout
	o.lastCumD = cumulativeD
	o.lastOuterRng = outerRange
	o.lastStep = step
	o.lastKShards = kShards
}

func (o *recordingObserver) ObserveOutcome(queryID, statusToken string) {
	o.outcomes = append(o.outcomes, [2]string{queryID, statusToken})
}

func (o *recordingObserver) ObserveRejection(
	_ string,
	_ []string,
	language string,
	statusToken string,
	_ bool,
	_ string,
	_, _, _, _, _ uint32,
	_ uint8,
	_ string,
) {
	o.rejections = append(o.rejections, [2]string{language, statusToken})
}

func (o *recordingObserver) ObserveDispatchedRejection(
	queryID, _ string,
	_ []string,
	language string,
	statusToken string,
	_ bool,
	_ string,
	_, _, _, _, _ uint32,
	_ uint8,
	_ string,
) {
	o.dispatchedRej = append(o.dispatchedRej, [3]string{queryID, language, statusToken})
}

// TestEngine_QueryID_ObserverAndDispatchAgree proves the single-source-of-truth
// the per-dispatch query_id depends on: the id the engine records into the
// corpus reconciler (QueryObserver.ObserveQuery) is the EXACT same id present on
// the ctx the chclient dispatch sees (chclient.QueryIDFromContext), so the
// reconciler's later system.query_log join matches the id ClickHouse recorded.
// It also confirms the id is non-empty and trace-prefixed under a real trace.
func TestEngine_QueryID_ObserverAndDispatchAgree(t *testing.T) {
	t.Parallel()

	tid := trace.TraceID{
		0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88,
		0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x00,
	}
	sid := trace.SpanID{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	var dispatchID string
	q := &fakeQuerier{captureCtx: func(c context.Context) {
		dispatchID = chclient.QueryIDFromContext(c)
	}}
	obs := &recordingObserver{}
	eng := newEngine(q)
	eng.QueryObserver = obs

	if _, err := eng.Query(ctx, &fakeLang{name: "promql"}, "up"); err != nil {
		t.Fatalf("Query: unexpected err: %v", err)
	}

	if len(obs.queryIDs) != 1 {
		t.Fatalf("observer saw %d query_ids; want exactly 1", len(obs.queryIDs))
	}
	observed := obs.queryIDs[0]
	if observed == "" {
		t.Fatal("observer query_id is empty; want a per-dispatch id under a real trace")
	}
	if !strings.HasPrefix(observed, tid.String()+"-") {
		t.Errorf("observed query_id %q is not trace-prefixed", observed)
	}
	if dispatchID != observed {
		t.Errorf("dispatch ctx query_id = %q; want the observed %q (reconciler join would break)", dispatchID, observed)
	}
}

func TestEngine_Query_HappyPath(t *testing.T) {
	t.Parallel()

	rows := []chclient.Sample{
		{MetricName: "up", Labels: map[string]string{"job": "a"}, Timestamp: time.Unix(1, 0), Value: 1},
		{MetricName: "up", Labels: map[string]string{"job": "b"}, Timestamp: time.Unix(2, 0), Value: 0},
	}
	q := &fakeQuerier{rows: rows}
	lang := &fakeLang{name: "promql"}
	eng := newEngine(q)

	res, err := eng.Query(context.Background(), lang, "up")
	if err != nil {
		t.Fatalf("Query: unexpected err: %v", err)
	}

	if got := len(res.Samples); got != 2 {
		t.Errorf("Samples len: got %d, want 2", got)
	}
	if res.SQL == "" {
		t.Errorf("SQL: empty, want non-empty emitted SQL")
	}
	if !strings.Contains(res.SQL, "otel_metrics_gauge") {
		t.Errorf("SQL: missing scan table; got %q", res.SQL)
	}
	if got := res.PlanNodeCount; got != 1 {
		t.Errorf("PlanNodeCount: got %d, want 1 (Scan only — fakeLang.ProjectSamples is identity)", got)
	}
	if res.Meta.ResponseShape != "prom-vector" {
		t.Errorf("Meta.ResponseShape: got %q, want %q", res.Meta.ResponseShape, "prom-vector")
	}
	if !res.Meta.IsMetric {
		t.Errorf("Meta.IsMetric: false, want true (threaded through from Parse)")
	}
	if res.CHMillis < 0 {
		t.Errorf("CHMillis: got %d, want non-negative", res.CHMillis)
	}

	// Pipeline-stage call order: Parse fired once, ProjectSamples
	// fired once, Querier.Query fired once.
	if lang.parseCalls != 1 {
		t.Errorf("Lang.Parse calls: got %d, want 1", lang.parseCalls)
	}
	if lang.projectCalls != 1 {
		t.Errorf("Lang.ProjectSamples calls: got %d, want 1", lang.projectCalls)
	}
	if q.calls != 1 {
		t.Errorf("Querier.Query calls: got %d, want 1", q.calls)
	}
}

// TestEngine_Query_HeadersPopulated covers the response-header contract:
// every successful Result carries the canonical X-Cerberus-* header bag
// (Strategy / Plan-Nodes / CH-Millis), and the Strategy field on Result
// agrees with the header value.
func TestEngine_Query_HeadersPopulated(t *testing.T) {
	t.Parallel()

	q := &fakeQuerier{rows: []chclient.Sample{{MetricName: "up"}}}
	lang := &fakeLang{name: "promql"}
	eng := newEngine(q)

	res, err := eng.Query(context.Background(), lang, "up")
	if err != nil {
		t.Fatalf("Query: unexpected err: %v", err)
	}

	if got, want := res.Strategy, "native"; got != want {
		t.Errorf("Result.Strategy: got %q, want %q", got, want)
	}
	if res.Headers == nil {
		t.Fatalf("Result.Headers: nil, want populated bag")
	}
	if got, want := res.Headers[engine.HeaderStrategy], "native"; got != want {
		t.Errorf("Headers[%s]: got %q, want %q", engine.HeaderStrategy, got, want)
	}
	if got := res.Headers[engine.HeaderPlanNodes]; got == "" {
		t.Errorf("Headers[%s]: empty, want numeric", engine.HeaderPlanNodes)
	}
	if got := res.Headers[engine.HeaderCHMillis]; got == "" {
		t.Errorf("Headers[%s]: empty, want numeric", engine.HeaderCHMillis)
	}
}

// TestEngine_QueryPlan_IsTraceByID_StrategyHeader verifies the
// trace-by-id short-circuit also flips the Strategy label so debug
// dashboards can distinguish the row-by-id path from the optimised
// native path.
func TestEngine_QueryPlan_IsTraceByID_StrategyHeader(t *testing.T) {
	t.Parallel()

	q := &fakeQuerier{}
	lang := &fakeLang{name: "traceql"}
	eng := newEngine(q)

	plan := &chplan.Scan{Table: "otel_traces"}
	res, err := eng.QueryPlan(context.Background(), lang, plan, engine.Meta{IsTraceByID: true})
	if err != nil {
		t.Fatalf("QueryPlan: unexpected err: %v", err)
	}
	if got, want := res.Strategy, "trace-by-id"; got != want {
		t.Errorf("Result.Strategy: got %q, want %q", got, want)
	}
	if got, want := res.Headers[engine.HeaderStrategy], "trace-by-id"; got != want {
		t.Errorf("Headers[%s]: got %q, want %q", engine.HeaderStrategy, got, want)
	}
}

func TestEngine_QueryPlan_HappyPath(t *testing.T) {
	t.Parallel()

	q := &fakeQuerier{rows: []chclient.Sample{{MetricName: "x"}}}
	lang := &fakeLang{name: "traceql"}
	eng := newEngine(q)

	plan := &chplan.Scan{Table: "otel_traces"}
	res, err := eng.QueryPlan(context.Background(), lang, plan, engine.Meta{ResponseShape: "tempo-traces"})
	if err != nil {
		t.Fatalf("QueryPlan: unexpected err: %v", err)
	}
	if lang.parseCalls != 0 {
		t.Errorf("Lang.Parse calls: got %d, want 0 (QueryPlan bypasses parse)", lang.parseCalls)
	}
	if lang.projectCalls != 1 {
		t.Errorf("Lang.ProjectSamples calls: got %d, want 1", lang.projectCalls)
	}
	if !strings.Contains(res.SQL, "otel_traces") {
		t.Errorf("SQL: missing scan table; got %q", res.SQL)
	}
	if got := len(res.Samples); got != 1 {
		t.Errorf("Samples len: got %d, want 1", got)
	}
}

// TestEngine_QueryPlan_IsTraceByID_SkipsOptimizer verifies the
// optimizer-skip branch through an observable side-effect: a custom
// rule that rewrites every Scan into a different Scan only fires when
// the optimizer runs. With IsTraceByID = true the engine must
// short-circuit, so the post-pipeline SQL references the original
// table; with the flag cleared, the rewritten table appears.
func TestEngine_QueryPlan_IsTraceByID_SkipsOptimizer(t *testing.T) {
	t.Parallel()

	const originalTable = "otel_traces"
	const rewrittenTable = "otel_traces_rewritten"

	rewrite := optimizer.NewWithBatches(optimizer.Batch{
		Name:     "test.scan-rewrite",
		Strategy: optimizer.Once(),
		Rules: []optimizer.Rule{
			scanRewriteRule{from: originalTable, to: rewrittenTable},
		},
	})

	t.Run("flag_set_skips_optimizer", func(t *testing.T) {
		q := &fakeQuerier{}
		eng := &engine.Engine{Optimizer: rewrite, Client: q}
		lang := &fakeLang{name: "traceql"}

		plan := &chplan.Scan{Table: originalTable}
		res, err := eng.QueryPlan(context.Background(), lang, plan, engine.Meta{IsTraceByID: true})
		if err != nil {
			t.Fatalf("QueryPlan: unexpected err: %v", err)
		}
		if strings.Contains(res.SQL, rewrittenTable) {
			t.Errorf("SQL: contains rewritten table %q; optimizer should have been bypassed. SQL=%q",
				rewrittenTable, res.SQL)
		}
		if !strings.Contains(res.SQL, originalTable) {
			t.Errorf("SQL: missing original table %q; got %q", originalTable, res.SQL)
		}
	})

	t.Run("flag_cleared_runs_optimizer", func(t *testing.T) {
		q := &fakeQuerier{}
		eng := &engine.Engine{Optimizer: rewrite, Client: q}
		lang := &fakeLang{name: "traceql"}

		plan := &chplan.Scan{Table: originalTable}
		res, err := eng.QueryPlan(context.Background(), lang, plan, engine.Meta{IsTraceByID: false})
		if err != nil {
			t.Fatalf("QueryPlan: unexpected err: %v", err)
		}
		if !strings.Contains(res.SQL, rewrittenTable) {
			t.Errorf("SQL: missing rewritten table %q; optimizer should have run. SQL=%q",
				rewrittenTable, res.SQL)
		}
	})
}

func TestEngine_Query_ParseError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("syntax: unexpected token")
	q := &fakeQuerier{}
	lang := &fakeLang{
		name: "promql",
		parseFn: func(_ context.Context, _ string) (chplan.Node, engine.Meta, error) {
			return nil, engine.Meta{}, wantErr
		},
	}
	eng := newEngine(q)

	_, err := eng.Query(context.Background(), lang, "garbage")
	if err == nil {
		t.Fatalf("Query: expected error, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("Query: err = %v, want wrap of %v", err, wantErr)
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("Query: err = %q, want prefix mentioning parse stage", err.Error())
	}
	if q.calls != 0 {
		t.Errorf("Querier.Query: got %d calls, want 0 (parse failure must short-circuit)", q.calls)
	}
}

func TestEngine_QueryPlan_EmitError(t *testing.T) {
	t.Parallel()

	q := &fakeQuerier{}
	lang := &fakeLang{name: "promql"}
	eng := newEngine(q)

	// A Filter with a nil Predicate trips chsql.Emit (Builder.Expr
	// hits its default branch and returns ErrUnsupported). The
	// IsTraceByID flag is set so the optimizer pass doesn't see the
	// invalid node — we want the failure to land at emit, not
	// inside optimizer rules that don't tolerate nil predicates.
	plan := &chplan.Filter{Input: &chplan.Scan{Table: "t"}, Predicate: nil}
	_, err := eng.QueryPlan(context.Background(), lang, plan, engine.Meta{IsTraceByID: true})
	if err == nil {
		t.Fatalf("QueryPlan: expected emit error, got nil")
	}
	if !strings.Contains(err.Error(), "emit") {
		t.Errorf("QueryPlan: err = %q, want prefix mentioning emit stage", err.Error())
	}
	if q.calls != 0 {
		t.Errorf("Querier.Query: got %d calls, want 0 (emit failure must short-circuit)", q.calls)
	}
}

func TestEngine_QueryPlan_ExecuteError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("clickhouse: connection refused")
	q := &fakeQuerier{err: wantErr}
	lang := &fakeLang{name: "promql"}
	eng := newEngine(q)

	plan := &chplan.Scan{Table: "otel_metrics_gauge"}
	_, err := eng.QueryPlan(context.Background(), lang, plan, engine.Meta{})
	if err == nil {
		t.Fatalf("QueryPlan: expected execute error, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("QueryPlan: err = %v, want wrap of %v", err, wantErr)
	}
	if !strings.Contains(err.Error(), "execute") {
		t.Errorf("QueryPlan: err = %q, want prefix mentioning execute stage", err.Error())
	}
}

func TestEngine_Query_NilLang(t *testing.T) {
	t.Parallel()

	eng := newEngine(&fakeQuerier{})
	if _, err := eng.Query(context.Background(), nil, "x"); err == nil {
		t.Errorf("Query(nil lang): expected error")
	}
	if _, err := eng.QueryPlan(context.Background(), nil, &chplan.Scan{Table: "t"}, engine.Meta{}); err == nil {
		t.Errorf("QueryPlan(nil lang): expected error")
	}
}

func TestEngine_QueryPlan_NilPlan(t *testing.T) {
	t.Parallel()

	eng := newEngine(&fakeQuerier{})
	lang := &fakeLang{name: "promql"}
	if _, err := eng.QueryPlan(context.Background(), lang, nil, engine.Meta{}); err == nil {
		t.Errorf("QueryPlan(nil plan): expected error")
	}
}

// tracedCtx returns a context carrying a real span so EnsureQueryID mints a
// non-empty per-dispatch query_id (the corpus join key the outcome seams need).
func tracedCtx() context.Context {
	tid := trace.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
	sid := trace.SpanID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid})
	return trace.ContextWithSpanContext(context.Background(), sc)
}

// TestEngine_EagerSampleBudget_StampsOutcome pins that an eager-path drain that
// hits the sample budget stamps the cerberus-side outcome onto the SAME
// query_id the dispatch observed — so the reconciler later overrides the
// query_log "ok" with "sample_budget".
func TestEngine_EagerSampleBudget_StampsOutcome(t *testing.T) {
	t.Parallel()

	q := &fakeQuerier{err: chclient.ErrTooManySamples}
	obs := &recordingObserver{}
	eng := newEngine(q)
	eng.QueryObserver = obs

	_, err := eng.Query(tracedCtx(), &fakeLang{name: "promql"}, "up")
	if err == nil {
		t.Fatal("expected sample-budget error")
	}
	if len(obs.outcomes) != 1 {
		t.Fatalf("ObserveOutcome calls = %d; want 1", len(obs.outcomes))
	}
	gotID, gotToken := obs.outcomes[0][0], obs.outcomes[0][1]
	if gotToken != "sample_budget" {
		t.Errorf("outcome token = %q; want sample_budget", gotToken)
	}
	// The outcome must be stamped on the same id the dispatch observed.
	if len(obs.queryIDs) != 1 || obs.queryIDs[0] != gotID || gotID == "" {
		t.Errorf("outcome id %q must equal dispatch id %v", gotID, obs.queryIDs)
	}
	if len(obs.rejections) != 0 {
		t.Errorf("a dispatched 422 must not produce a rejection row: %v", obs.rejections)
	}
}

// TestEngine_EagerMemoryCap_StampsDispatchedRejection pins the observability-gap
// fix: an eager-path query aborted by the per-query memory cap (CH code 241,
// chclient.ErrMemoryLimitExceeded) records a DISPATCHED rejection — exit_status
// "oom", carrying the dispatch query_id (so the corpus can drop it from the
// query_log join) and the language. Before this fix the memory-cap error fell
// through outcomeTokenForErr's default and was invisible to the corpus.
func TestEngine_EagerMemoryCap_StampsDispatchedRejection(t *testing.T) {
	t.Parallel()

	q := &fakeQuerier{err: chclient.ErrMemoryLimitExceeded}
	obs := &recordingObserver{}
	eng := newEngine(q)
	eng.QueryObserver = obs

	_, err := eng.Query(tracedCtx(), &fakeLang{name: "promql"}, "up")
	if err == nil {
		t.Fatal("expected memory-cap error")
	}
	if len(obs.dispatchedRej) != 1 {
		t.Fatalf("ObserveDispatchedRejection calls = %d; want 1", len(obs.dispatchedRej))
	}
	got := obs.dispatchedRej[0]
	if got[1] != "promql" || got[2] != "oom" {
		t.Errorf("dispatched rejection = %v; want lang=promql token=oom", got)
	}
	// The terminal row must carry the SAME query_id the dispatch observed, so the
	// reconciler can forget it and the query_log join cannot double-write.
	if len(obs.queryIDs) != 1 || got[0] != obs.queryIDs[0] || got[0] == "" {
		t.Errorf("dispatched-rejection id %q must equal dispatch id %v", got[0], obs.queryIDs)
	}
	if len(obs.outcomes) != 0 || len(obs.rejections) != 0 {
		t.Errorf("a memory-cap abort must use the dispatched-rejection seam only: outcomes=%v rejections=%v", obs.outcomes, obs.rejections)
	}
}

// TestEngine_EagerBreaker_StampsRejection pins that a breaker fast-fail (no CH
// query ran) produces a decision-only rejection row, not an outcome stamp.
func TestEngine_EagerBreaker_StampsRejection(t *testing.T) {
	t.Parallel()

	q := &fakeQuerier{err: chclient.ErrCircuitOpen}
	obs := &recordingObserver{}
	eng := newEngine(q)
	eng.QueryObserver = obs

	_, err := eng.Query(tracedCtx(), &fakeLang{name: "promql"}, "up")
	if err == nil {
		t.Fatal("expected breaker error")
	}
	if len(obs.rejections) != 1 {
		t.Fatalf("ObserveRejection calls = %d; want 1", len(obs.rejections))
	}
	if lang, token := obs.rejections[0][0], obs.rejections[0][1]; lang != "promql" || token != "breaker" {
		t.Errorf("rejection = (%q, %q); want (promql, breaker)", lang, token)
	}
	if len(obs.outcomes) != 0 {
		t.Errorf("a breaker reject must not stamp a dispatched outcome: %v", obs.outcomes)
	}
}

// TestEngine_EagerOtherError_NoCerberusOutcome pins that a plain transport error
// is left to the query_log-derived path — neither seam fires.
func TestEngine_EagerOtherError_NoCerberusOutcome(t *testing.T) {
	t.Parallel()

	q := &fakeQuerier{err: errors.New("clickhouse: connection refused")}
	obs := &recordingObserver{}
	eng := newEngine(q)
	eng.QueryObserver = obs

	if _, err := eng.Query(tracedCtx(), &fakeLang{name: "promql"}, "up"); err == nil {
		t.Fatal("expected error")
	}
	if len(obs.outcomes) != 0 || len(obs.rejections) != 0 {
		t.Errorf("transport error must not produce a cerberus-side outcome: outcomes=%v rejections=%v", obs.outcomes, obs.rejections)
	}
}

// TestEngine_ObserveCapRejection pins the pre-parse cap seam: a decision-only
// "rejected" row carrying the language and no routing read-out.
func TestEngine_ObserveCapRejection(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	eng := newEngine(&fakeQuerier{})
	eng.QueryObserver = obs

	eng.ObserveCapRejection("traceql")
	if len(obs.rejections) != 1 {
		t.Fatalf("ObserveRejection calls = %d; want 1", len(obs.rejections))
	}
	if lang, token := obs.rejections[0][0], obs.rejections[0][1]; lang != "traceql" || token != "rejected" {
		t.Errorf("cap rejection = (%q, %q); want (traceql, rejected)", lang, token)
	}
}

// TestEngine_ObserveDrainOutcome pins the cursor-path drain seam: a
// sample-budget error surfacing during the handler drain stamps "sample_budget"
// on the supplied query_id; a memory-cap abort is recorded as a dispatched
// rejection carrying the language; a nil error / empty id / other error is a no-op.
func TestEngine_ObserveDrainOutcome(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	eng := newEngine(&fakeQuerier{})
	eng.QueryObserver = obs

	eng.ObserveDrainOutcome("qid-d", "promql", chclient.ErrTooManySamples)
	eng.ObserveDrainOutcome("qid-d", "promql", nil)                     // no-op
	eng.ObserveDrainOutcome("", "promql", chclient.ErrTooManySamples)   // no-op
	eng.ObserveDrainOutcome("qid-d", "promql", errors.New("transport")) // no-op

	if len(obs.outcomes) != 1 {
		t.Fatalf("ObserveOutcome calls = %d; want exactly 1", len(obs.outcomes))
	}
	if id, token := obs.outcomes[0][0], obs.outcomes[0][1]; id != "qid-d" || token != "sample_budget" {
		t.Errorf("drain outcome = (%q, %q); want (qid-d, sample_budget)", id, token)
	}
	if len(obs.dispatchedRej) != 0 {
		t.Errorf("a sample-budget drain must not produce a dispatched rejection: %v", obs.dispatchedRej)
	}

	// A memory-cap abort surfacing mid-drain is recorded as a dispatched
	// rejection (terminal, zero cost), carrying the language and the query_id.
	eng.ObserveDrainOutcome("qid-oom", "logql", chclient.ErrMemoryLimitExceeded)
	if len(obs.dispatchedRej) != 1 {
		t.Fatalf("ObserveDispatchedRejection calls = %d; want 1", len(obs.dispatchedRej))
	}
	if got := obs.dispatchedRej[0]; got[0] != "qid-oom" || got[1] != "logql" || got[2] != "oom" {
		t.Errorf("drain oom rejection = %v; want [qid-oom logql oom]", got)
	}
}

// TestEngine_NoObserver_OutcomeNoop pins that the cerberus-side seams are a
// no-op when no observer is registered (the default hot path).
func TestEngine_NoObserver_OutcomeNoop(t *testing.T) {
	t.Parallel()

	eng := newEngine(&fakeQuerier{err: chclient.ErrTooManySamples})
	// Must not panic with a nil observer.
	if _, err := eng.Query(tracedCtx(), &fakeLang{name: "promql"}, "up"); err == nil {
		t.Fatal("expected error")
	}
	eng.ObserveCapRejection("promql")
	eng.ObserveDrainOutcome("x", "promql", chclient.ErrTooManySamples)
}

// scanRewriteRule is a probe rule for the IsTraceByID test: it
// rewrites every Scan whose Table equals `from` into a Scan against
// `to`. We use a real optimizer pass with just this rule so the test
// can observe whether the engine ran the optimizer.
type scanRewriteRule struct {
	from, to string
}

func (scanRewriteRule) Name() string { return "test.scan-rewrite" }

func (r scanRewriteRule) Apply(n chplan.Node) (chplan.Node, bool) {
	scan, ok := n.(*chplan.Scan)
	if !ok || scan.Table != r.from {
		return n, false
	}
	return &chplan.Scan{Table: r.to, Columns: scan.Columns}, true
}
