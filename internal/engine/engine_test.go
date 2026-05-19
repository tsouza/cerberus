package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
	rows    []chclient.Sample
	err     error
	gotSQL  string
	gotArgs []any
	calls   int
}

func (f *fakeQuerier) Query(_ context.Context, sql string, args ...any) ([]chclient.Sample, error) {
	f.calls++
	f.gotSQL = sql
	f.gotArgs = args
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
