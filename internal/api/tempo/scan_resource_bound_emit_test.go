package tempo

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tql "github.com/tsouza/cerberus/internal/traceql"
	"github.com/tsouza/cerberus/internal/traceql/ast"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/schema"
)

// This test drives the REAL Traces Drilldown emit paths through their real
// emitters and the real engine seam, and asserts the spans-scan resource-bound
// invariant holds: every otel_traces scan that reaches ClickHouse is
// partition-pruned (a window or a finite trace-id set) or memory-streaming
// bounded. The tests deliberately do NOT hand-thread WithSpansTable with a
// literal table — prod threads it at the engine seam via the Lang's
// SpansTable(), so the tests derive the scope the same way (or go through the
// engine). Reverting that wiring un-bounds the path and FAILS these tests.

const scanBoundSpansTable = "otel_traces"

func tracesSchema() schema.Traces { return schema.DefaultOTelTraces() }

// emitScoped mirrors the engine seam (engine.emitForHead): it threads the spans
// table onto the emit context via the REAL Lang.SpansTable() — so if that
// method is reverted to "", the chokepoint no-ops and the negative cases below
// stop failing.
func emitScoped(t *testing.T, lang *traceqlLang, plan chplan.Node) (string, error) {
	t.Helper()
	ctx := chsql.WithSpansTable(context.Background(), lang.SpansTable())
	sql, _, err := chsql.Emit(ctx, plan)
	return sql, err
}

// assertEverySpansFromBounded checks that every otel_traces scan in sql sits in
// a scope that also carries a recognized bound — a TraceId membership, a
// Timestamp window, or the recursive depth cap for the memory-streaming arm.
func assertEverySpansFromBounded(t *testing.T, sql string) {
	t.Helper()
	scans := strings.Count(sql, scanBoundSpansTable)
	if scans == 0 {
		t.Fatalf("expected at least one %q scan in SQL:\n%s", scanBoundSpansTable, sql)
	}
	boundTokens := strings.Count(sql, "`TraceId` IN") +
		strings.Count(sql, "IN (SELECT `TraceId`")
	windowTokens := strings.Count(sql, "fromUnixTimestamp64Nano") +
		strings.Count(sql, "toDateTime64")
	depthTokens := strings.Count(sql, "_depth <")
	if boundTokens+windowTokens+depthTokens < scans {
		t.Errorf("found %d %q scans but only %d bound tokens (traceID=%d window=%d depth=%d):\n%s",
			scans, scanBoundSpansTable, boundTokens+windowTokens+depthTokens,
			boundTokens, windowTokens, depthTokens, sql)
	}
}

// capturingQuerier records the last SQL the handler emitted and returns a fixed
// sample set, so a handler entry point can be exercised end-to-end.
type capturingQuerier struct {
	lastSQL string
	samples []chclient.Sample
}

func (q *capturingQuerier) Query(_ context.Context, sql string, _ ...any) ([]chclient.Sample, error) {
	q.lastSQL = sql
	return q.samples, nil
}

func (q *capturingQuerier) QueryStrings(_ context.Context, sql string, _ ...any) ([]string, error) {
	q.lastSQL = sql
	return nil, nil
}

func searchCtx(start, end time.Time) context.Context {
	ctx := tql.WithSearchTraceLimit(context.Background(), 20)
	return tql.WithSearchWindow(ctx, start, end)
}

// TestScanResourceBound_EngineSeamEnforces proves the chokepoint actually runs
// over a Tempo plan at the engine seam. A bare, unbounded spans scan routed
// through the real engine with the real Tempo Lang must be rejected — because
// engine.emitForHead threads WithSpansTable(traceqlLang.SpansTable()). Revert
// that wiring (or SpansTable() -> "") and the scan emits unbounded, failing
// here.
func TestScanResourceBound_EngineSeamEnforces(t *testing.T) {
	t.Parallel()
	s := tracesSchema()
	h := New(&capturingQuerier{}, s, "v-test", nil)

	unbounded := chplan.Node(&chplan.Scan{Table: s.SpansTable})
	_, err := h.Engine.QueryPlan(context.Background(), h.Lang(), unbounded,
		engine.Meta{IsTraceByID: true, ResponseShape: "tempo-trace"})
	var v *chplan.ScanResourceBoundViolation
	if !errors.As(err, &v) {
		t.Fatalf("bare spans scan through the engine seam must be rejected (ScanResourceBoundViolation), got %v", err)
	}
}

func TestScanResourceBound_RootLookupRealEntry(t *testing.T) {
	t.Parallel()
	s := tracesSchema()
	q := &capturingQuerier{}
	h := New(q, s, "v-test", nil)

	// Real entry, no test-supplied WithSpansTable: resolveTraceRoots threads it
	// itself (root_lookup.go) before chsql.Emit.
	if _, err := h.resolveTraceRoots(context.Background(), []string{"0123456789abcdef0123456789abcdef"}); err != nil {
		t.Fatalf("resolveTraceRoots: %v", err)
	}
	if !strings.Contains(q.lastSQL, "`TraceId` IN") {
		t.Errorf("root lookup must scan otel_traces under a TraceId IN set:\n%s", q.lastSQL)
	}
	assertEverySpansFromBounded(t, q.lastSQL)

	// The chokepoint the root-lookup path relies on is real and table-scoped:
	// an unbounded root-lookup-shaped plan is rejected only when the spans
	// table is on the context.
	bare := chplan.Node(&chplan.Scan{Table: s.SpansTable})
	if _, _, err := chsql.Emit(chsql.WithSpansTable(context.Background(), s.SpansTable), bare); err == nil {
		t.Errorf("unbounded root-lookup-shaped scan must be rejected under WithSpansTable")
	}
}

// TestScanResourceBound_RootLookupChokepointPins independently pins the
// chokepoint's coverage of the root-lookup shape (not the wiring line): a
// root-lookup-shaped plan — Aggregate grouping by TraceId — with the TraceId
// InList bound STRIPPED must be rejected under the spans scope, while the
// InList-present plan is accepted. This catches a regression where the gate
// stops covering the root-lookup table, even though the InList-present path
// stays output-equivalent.
func TestScanResourceBound_RootLookupChokepointPins(t *testing.T) {
	t.Parallel()
	s := tracesSchema()
	ctx := chsql.WithSpansTable(context.Background(), s.SpansTable)
	groupBy := []chplan.Expr{&chplan.ColumnRef{Name: s.TraceIDColumn}}
	groupAliases := []string{"TraceId"}

	// Stripped: Aggregate(bare spans Scan) — no TraceId bound -> rejected.
	stripped := chplan.Node(&chplan.Aggregate{
		Input:          &chplan.Scan{Table: s.SpansTable},
		GroupBy:        groupBy,
		GroupByAliases: groupAliases,
	})
	if _, _, err := chsql.Emit(ctx, stripped); err == nil {
		t.Fatalf("stripped root-lookup shape (no TraceId IN) must be rejected under WithSpansTable")
	}

	// Bound: the same aggregate over a TraceId IN set -> accepted.
	bounded := chplan.Node(&chplan.Aggregate{
		Input: &chplan.Filter{
			Input: &chplan.Scan{Table: s.SpansTable},
			Predicate: &chplan.InList{
				Left: &chplan.ColumnRef{Name: s.TraceIDColumn},
				List: []chplan.Expr{&chplan.LitString{V: "abc"}},
			},
		},
		GroupBy:        groupBy,
		GroupByAliases: groupAliases,
	})
	if _, _, err := chsql.Emit(ctx, bounded); err != nil {
		t.Fatalf("bounded root-lookup shape (TraceId IN) must be accepted, got %v", err)
	}
}

// TestScanResourceBound_HistogramFailClosed pins the 5th gate
// (emitRangeWindowHistogram): a zero-window spans inner under the spans scope
// must fail closed.
func TestScanResourceBound_HistogramFailClosed(t *testing.T) {
	t.Parallel()
	s := tracesSchema()
	m := &chplan.MetricsHistogramOverTime{
		Attr:       &chplan.ColumnRef{Name: "Duration"},
		ValueAlias: "Value",
		Inner:      &chplan.Scan{Table: s.SpansTable},
	}
	rw := &chplan.RangeWindow{
		Input: m, Step: time.Minute, Range: time.Minute, TimestampColumn: "Timestamp",
	}
	ctx := chsql.WithSpansTable(context.Background(), s.SpansTable)
	if _, _, err := chsql.Emit(ctx, rw); !errors.Is(err, chsql.ErrUnboundedSpansScan) {
		t.Fatalf("zero-window histogram_over_time over spans inner must fail closed, got %v", err)
	}
}

func TestScanResourceBound_StructureTabBounded(t *testing.T) {
	t.Parallel()
	s := tracesSchema()
	lang := &traceqlLang{schema: s}
	expr, err := ast.Parse(`{ .service.name = "checkout" } >> { .http.method = "GET" }`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	start := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	ctx := searchCtx(start, start.Add(time.Hour))
	plan, err := tql.Lower(ctx, expr, s)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	sql, err := emitScoped(t, lang, plan)
	if err != nil {
		t.Fatalf("structure emit: %v", err)
	}
	// The recursive step scan is bounded by the request window sitting DIRECTLY
	// on the `otel_traces AS t` scan (a toDate(Timestamp) partition prune). The
	// seed-trace-id IN pushdown was dropped — it was redundant with the step
	// JOIN ON `t.TraceId = c.TraceId` and inert for partition pruning — so the
	// step is now bounded by this stronger, partition-pruning window predicate
	// rather than a trace-id membership.
	if !strings.Contains(sql, "c._depth < 128 AND `Timestamp` >=") {
		t.Errorf("structural recursive step must be window-pruned directly on the t scan:\n%s", sql)
	}
	assertEverySpansFromBounded(t, sql)
}

func TestScanResourceBound_NestedSetStructureBounded(t *testing.T) {
	t.Parallel()
	s := tracesSchema()
	lang := &traceqlLang{schema: s}
	expr, err := ast.Parse(`{ } | select(nestedSetParent)`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	start := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	ctx := searchCtx(start, start.Add(time.Hour))
	plan, err := tql.Lower(ctx, expr, s)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	sql, err := emitScoped(t, lang, plan)
	if err != nil {
		t.Fatalf("nested-set emit: %v", err)
	}
	if strings.Count(sql, "`TraceId` IN") < 2 {
		t.Errorf("nested-set anchor + step must both be trace-id scoped:\n%s", sql)
	}
	assertEverySpansFromBounded(t, sql)
}

func TestScanResourceBound_ExemplarsBoundedAndFailClosed(t *testing.T) {
	t.Parallel()
	s := tracesSchema()
	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	m := &chplan.MetricsAggregate{
		Op:             chplan.MetricsOpRate,
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "resource.service.name"}},
		GroupByAliases: []string{"resource.service.name"},
		ValueAlias:     "Value",
		Inner:          &chplan.Scan{Table: s.SpansTable},
	}
	rw := &chplan.RangeWindow{
		Input: m, Step: time.Minute, Range: time.Minute,
		Start: start, End: start.Add(5 * time.Minute), TimestampColumn: "Timestamp",
	}
	sql, _, err := chsql.EmitMetricsExemplars(context.Background(), rw, m,
		s.TraceIDColumn, s.SpanIDColumn, 1, s.SpansTable)
	if err != nil {
		t.Fatalf("exemplars (windowed) emit: %v", err)
	}
	assertEverySpansFromBounded(t, sql)

	rwUnbounded := &chplan.RangeWindow{
		Input: m, Step: time.Minute, Range: time.Minute, TimestampColumn: "Timestamp",
	}
	_, _, err = chsql.EmitMetricsExemplars(context.Background(), rwUnbounded, m,
		s.TraceIDColumn, s.SpanIDColumn, 1, s.SpansTable)
	if !errors.Is(err, chsql.ErrUnboundedSpansScan) {
		t.Fatalf("zero-window exemplars over spans inner must fail closed, got %v", err)
	}
}

// TestScanResourceBound_CompareFailClosed covers the compare() matrix gate
// (item 6): a RangeWindow with a half-open / zero window over a spans-inner
// compare node must fail closed when the emit context is spans-scoped.
func TestScanResourceBound_CompareFailClosed(t *testing.T) {
	t.Parallel()
	s := tracesSchema()
	cmp := &chplan.MetricsCompare{
		Inner:     &chplan.Scan{Table: s.SpansTable},
		Selection: &chplan.LitBool{V: true},
	}
	rw := &chplan.RangeWindow{
		Input: cmp, Step: time.Minute, Range: time.Minute, TimestampColumn: "Timestamp",
	}
	ctx := chsql.WithSpansTable(context.Background(), s.SpansTable)
	if _, _, err := chsql.Emit(ctx, rw); !errors.Is(err, chsql.ErrUnboundedSpansScan) {
		t.Fatalf("zero-window compare over spans inner must fail closed, got %v", err)
	}
}

// TestScanResourceBound_MetricsMatrixFailClosed covers item 3: the metrics
// matrix emitter (emitRangeWindowMetrics) fails closed on a zero-window spans
// inner when the emit context is spans-scoped (as the engine threads it).
func TestScanResourceBound_MetricsMatrixFailClosed(t *testing.T) {
	t.Parallel()
	s := tracesSchema()
	m := &chplan.MetricsAggregate{
		Op:             chplan.MetricsOpCountOverTime,
		GroupByAliases: nil,
		ValueAlias:     "Value",
		Inner:          &chplan.Scan{Table: s.SpansTable},
	}
	rw := &chplan.RangeWindow{
		Input: m, Step: time.Minute, Range: time.Minute, TimestampColumn: "Timestamp",
	}
	ctx := chsql.WithSpansTable(context.Background(), s.SpansTable)
	if _, _, err := chsql.Emit(ctx, rw); !errors.Is(err, chsql.ErrUnboundedSpansScan) {
		t.Fatalf("zero-window metrics matrix over spans inner must fail closed, got %v", err)
	}
	// Without the spans scope it is a no-op (table-scoped): the now64 fallback
	// emits (the established emitter unit-test behaviour).
	if _, _, err := chsql.Emit(context.Background(), rw); err != nil {
		t.Fatalf("metrics matrix without WithSpansTable must emit, got %v", err)
	}
}

func TestScanResourceBound_BareScanIsTableScoped(t *testing.T) {
	t.Parallel()
	bare := chplan.Node(&chplan.Scan{Table: scanBoundSpansTable})
	_, _, err := chsql.Emit(chsql.WithSpansTable(context.Background(), scanBoundSpansTable), bare)
	var v *chplan.ScanResourceBoundViolation
	if !errors.As(err, &v) {
		t.Fatalf("bare spans scan under WithSpansTable must be a ScanResourceBoundViolation, got %v", err)
	}
	if _, _, err := chsql.Emit(context.Background(), bare); err != nil {
		t.Fatalf("bare scan without WithSpansTable must emit (table-scoped no-op), got %v", err)
	}
}
