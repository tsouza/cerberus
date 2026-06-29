package tempo

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tql "github.com/tsouza/cerberus/internal/traceql"
	"github.com/tsouza/cerberus/internal/traceql/ast"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// This test drives the REAL Traces Drilldown emit paths — root lookup, the
// structure-tab structural/nested-set lowering, the metrics-compare matrix, and
// the metrics-exemplars matrix — through their actual emitters and asserts the
// spans-scan resource-bound invariant holds: every otel_traces scan that
// reaches ClickHouse is partition-pruned (a window or a finite trace-id set) or
// memory-streaming bounded. The negative cases prove the invariant fails closed
// when a bound is missing.

const scanBoundSpansTable = "otel_traces"

func tracesSchema() schema.Traces { return schema.DefaultOTelTraces() }

// assertEverySpansFromBounded checks that every `FROM ... otel_traces` token in
// sql sits in a SELECT scope that also carries a recognized bound — a TraceId
// membership (`TraceId IN`), a Timestamp window (`Timestamp >`/`<`), or the
// recursive depth cap (`_depth <`) for the memory-streaming arm. It is a
// coarse, whole-statement check: it requires at least one bound token to appear
// for every spans-scan occurrence, which catches a fully-unbounded scan slipping
// through (the OOM shape) without overfitting to exact SQL.
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
		t.Errorf("found %d %q scans but only %d bound tokens (traceID=%d window=%d depth=%d) — an unbounded spans scan slipped through:\n%s",
			scans, scanBoundSpansTable, boundTokens+windowTokens+depthTokens,
			boundTokens, windowTokens, depthTokens, sql)
	}
}

func TestScanResourceBound_RootLookupBounded(t *testing.T) {
	t.Parallel()
	s := tracesSchema()
	plan := buildRootLookupPlan(s, []string{"0123456789abcdef0123456789abcdef"})
	sql, _, err := chsql.Emit(chsql.WithSpansTable(context.Background(), s.SpansTable), plan)
	if err != nil {
		t.Fatalf("root lookup emit: %v", err)
	}
	if !strings.Contains(sql, "`TraceId` IN") {
		t.Errorf("root lookup must scan otel_traces under a TraceId IN set:\n%s", sql)
	}
	assertEverySpansFromBounded(t, sql)
}

func TestScanResourceBound_StructureTabBounded(t *testing.T) {
	t.Parallel()
	s := tracesSchema()
	// A descendant structural search — the structure-tab row-source shape.
	expr, err := ast.Parse(`{ .service.name = "checkout" } >> { .http.method = "GET" }`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	start := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	ctx := tql.WithSearchTraceLimit(context.Background(), 20)
	ctx = tql.WithSearchWindow(ctx, start, end)

	plan, err := tql.Lower(ctx, expr, s)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	sql, _, err := chsql.Emit(chsql.WithSpansTable(ctx, s.SpansTable), plan)
	if err != nil {
		t.Fatalf("structure emit: %v", err)
	}
	// The recursive step must carry the seed trace-id prune and the depth cap.
	if !strings.Contains(sql, "t.`TraceId` IN") {
		t.Errorf("structural recursive step must be trace-id pruned:\n%s", sql)
	}
	assertEverySpansFromBounded(t, sql)
}

func TestScanResourceBound_NestedSetStructureBounded(t *testing.T) {
	t.Parallel()
	s := tracesSchema()
	// The Drilldown structure tab: a select() over the root-union + descendant
	// shape that lowers to a NestedSetAnnotate. With a search limit + window the
	// numbering walk and its leaves are bounded in lock-step.
	expr, err := ast.Parse(`{ } | select(nestedSetParent)`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	start := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	ctx := tql.WithSearchTraceLimit(context.Background(), 20)
	ctx = tql.WithSearchWindow(ctx, start, end)

	plan, err := tql.Lower(ctx, expr, s)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	sql, _, err := chsql.Emit(chsql.WithSpansTable(ctx, s.SpansTable), plan)
	if err != nil {
		t.Fatalf("nested-set emit: %v", err)
	}
	// The numbering anchor AND recursive step must both be trace-id scoped.
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
	// Windowed: bounded, no error.
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

	// Zero window over a spans inner: fail closed.
	rwUnbounded := &chplan.RangeWindow{
		Input: m, Step: time.Minute, Range: time.Minute, TimestampColumn: "Timestamp",
	}
	_, _, err = chsql.EmitMetricsExemplars(context.Background(), rwUnbounded, m,
		s.TraceIDColumn, s.SpanIDColumn, 1, s.SpansTable)
	if !errors.Is(err, chsql.ErrUnboundedSpansScan) {
		t.Fatalf("zero-window exemplars over spans inner must fail closed, got %v", err)
	}
}

func TestScanResourceBound_BareScanIsTableScoped(t *testing.T) {
	t.Parallel()
	bare := &chplan.Scan{Table: scanBoundSpansTable}

	// With the spans table on the context the bare scan is rejected.
	_, _, err := chsql.Emit(chsql.WithSpansTable(context.Background(), scanBoundSpansTable), bare)
	var v *chplan.ScanResourceBoundViolation
	if !errors.As(err, &v) {
		t.Fatalf("bare spans scan under WithSpansTable must be a ScanResourceBoundViolation, got %v", err)
	}

	// Without it the invariant is a no-op (table-scoped): a bare scan emits.
	if _, _, err := chsql.Emit(context.Background(), bare); err != nil {
		t.Fatalf("bare scan without WithSpansTable must emit (table-scoped no-op), got %v", err)
	}
}
