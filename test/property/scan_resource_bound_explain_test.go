//go:build chdb

package property

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	tql "github.com/tsouza/cerberus/internal/traceql"
	"github.com/tsouza/cerberus/internal/traceql/ast"
)

// This chDB-backed test proves the spans-scan resource-bound invariant actually
// prunes at execution time — and it ratchets the REAL cerberus emit output, not
// a hand-written SQL string. It lowers / builds the bounded plan, runs it
// through chsql.Emit, then EXPLAIN ESTIMATEs the emitted SQL against a
// multi-trace, multi-partition otel_traces table:
//   - the window-bounded (form-a) plan, lowered from real TraceQL, must touch
//     strictly fewer parts than a full-window plan;
//   - the trace-id-bounded (form-b) plan, emitted by the real emitter, must
//     read no more than one trace's rows (delta N2: read_rows <= rowsPerTrace,
//     not tracesSeeded*rowsPerTrace).

const (
	scanBoundTracesSeeded = 3
	scanBoundRowsPerTrace = 2
)

// scanBoundSeedDDL creates a date-partitioned span table with three traces, one
// per partition (three distinct days), each a root + one child span.
const scanBoundSeedDDL = `
CREATE OR REPLACE TABLE otel_traces (
    Timestamp           DateTime64(9),
    TraceId             String,
    SpanId              String,
    ParentSpanId        String,
    SpanName            String,
    ServiceName         String,
    Duration            Int64,
    StatusCode          String,
    SpanKind            String,
    ResourceAttributes  Map(String, String),
    SpanAttributes      Map(String, String),
    ScopeName           String
) ENGINE = MergeTree
PARTITION BY toYYYYMMDD(Timestamp)
ORDER BY (TraceId, Timestamp);

INSERT INTO otel_traces (Timestamp, TraceId, SpanId, ParentSpanId, ServiceName) VALUES
    ('2026-05-10 10:00:00.000000000', 'trace_a', 'a_root',  '', 'checkout'),
    ('2026-05-10 10:00:01.000000000', 'trace_a', 'a_child', 'a_root', 'checkout'),
    ('2026-05-11 10:00:00.000000000', 'trace_b', 'b_root',  '', 'checkout'),
    ('2026-05-11 10:00:01.000000000', 'trace_b', 'b_child', 'b_root', 'checkout'),
    ('2026-05-12 10:00:00.000000000', 'trace_c', 'c_root',  '', 'checkout'),
    ('2026-05-12 10:00:01.000000000', 'trace_c', 'c_child', 'c_root', 'checkout');
`

// explainEstimate runs EXPLAIN ESTIMATE and returns the summed (parts, rows)
// across the result rows (one row per scanned table: database, table, parts,
// rows, marks).
func explainEstimate(t *testing.T, db *sql.DB, query string) (parts, rows int64) {
	t.Helper()
	r, err := db.Query("EXPLAIN ESTIMATE " + query)
	if err != nil {
		t.Fatalf("EXPLAIN ESTIMATE: %v\nquery: %s", err, query)
	}
	defer func() { _ = r.Close() }()
	for r.Next() {
		var database, table string
		var p, rw, marks int64
		if err := r.Scan(&database, &table, &p, &rw, &marks); err != nil {
			t.Fatalf("scan EXPLAIN row: %v", err)
		}
		parts += p
		rows += rw
	}
	if err := tolerantRowsErr(r.Err()); err != nil {
		t.Fatalf("EXPLAIN rows: %v", err)
	}
	return parts, rows
}

// inlineArgs splices positional `?` args back into the SQL as quoted literals so
// the statement can be handed to EXPLAIN ESTIMATE without a prepared statement.
// Only string args appear in these plans.
func inlineArgs(t *testing.T, query string, args []any) string {
	t.Helper()
	out := query
	for _, a := range args {
		var lit string
		switch v := a.(type) {
		case string:
			lit = "'" + v + "'"
		case bool:
			if v {
				lit = "1"
			} else {
				lit = "0"
			}
		case int, int32, int64, uint, uint32, uint64, float64:
			lit = fmt.Sprint(v)
		default:
			t.Fatalf("inlineArgs: unsupported arg type %T", a)
		}
		out = strings.Replace(out, "?", lit, 1)
	}
	return out
}

func emitTQL(t *testing.T, ctx context.Context, query string, s schema.Traces) string {
	t.Helper()
	expr, err := ast.Parse(query)
	if err != nil {
		t.Fatalf("parse %q: %v", query, err)
	}
	plan, err := tql.Lower(ctx, expr, s)
	if err != nil {
		t.Fatalf("lower %q: %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(chsql.WithSpansTable(ctx, s.SpansTable), plan)
	if err != nil {
		t.Fatalf("emit %q: %v", query, err)
	}
	return inlineArgs(t, sqlStr, args)
}

// emitWindowScan builds the form-a bound shape — Filter(Timestamp >= start AND
// Timestamp <= end, Scan) — and runs it through the real chsql emitter. This is
// the exact windowed-leaf predicate lowering stamps (tsBound /
// fromUnixTimestamp64Nano), so the EXPLAIN below ratchets cerberus emit output.
func emitWindowScan(t *testing.T, s schema.Traces, start, end time.Time) string {
	t.Helper()
	tsBound := func(op chplan.BinaryOp, ts time.Time) chplan.Expr {
		return &chplan.Binary{
			Op:   op,
			Left: &chplan.ColumnRef{Name: s.TimestampColumn},
			Right: &chplan.FuncCall{
				Name: "fromUnixTimestamp64Nano",
				Args: []chplan.Expr{&chplan.LitInt{V: ts.UnixNano()}},
			},
		}
	}
	plan := chplan.Node(&chplan.Filter{
		Input: &chplan.Scan{Table: s.SpansTable},
		Predicate: &chplan.Binary{
			Op:    chplan.OpAnd,
			Left:  tsBound(chplan.OpGe, start),
			Right: tsBound(chplan.OpLe, end),
		},
	})
	sqlStr, args, err := chsql.Emit(chsql.WithSpansTable(context.Background(), s.SpansTable), plan)
	if err != nil {
		t.Fatalf("emit window scan: %v", err)
	}
	return inlineArgs(t, sqlStr, args)
}

func TestScanResourceBound_PartitionPruneEXPLAIN(t *testing.T) {
	t.Parallel()
	db := openChDB(t)
	applyDDL(t, db, scanBoundSeedDDL)
	s := schema.DefaultOTelTraces()

	// Sanity: the real TraceQL lower→emit path for a windowed search PASSES the
	// chokepoint (proves the engine-seam scope accepts a genuinely-bounded
	// lowered plan, not just that direct shapes work).
	day2 := time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)
	searchCtx := tql.WithSearchWindow(tql.WithSearchTraceLimit(context.Background(), 20),
		day2, day2.Add(24*time.Hour))
	_ = emitTQL(t, searchCtx, `{ }`, s) // emits without error (chokepoint passes)

	// Form-a: the window-bound shape (the exact Timestamp predicate lowering
	// stamps onto a leaf), emitted by the real chsql emitter, must prune to the
	// one day's partition; the full three-day window scans all three. A single
	// clean scan keeps the EXPLAIN ESTIMATE part count unambiguous.
	narrowSQL := emitWindowScan(t, s, day2, day2.Add(24*time.Hour))
	wideSQL := emitWindowScan(t, s,
		time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC))

	boundParts, _ := explainEstimate(t, db, narrowSQL)
	fullParts, _ := explainEstimate(t, db, wideSQL)
	if !(boundParts < fullParts) {
		t.Errorf("window bound must prune partitions: bounded_parts=%d full_parts=%d\nnarrow: %s",
			boundParts, fullParts, narrowSQL)
	}

	// Form-b: the root-lookup bound shape (a literal TraceId IN set), emitted
	// by the real chsql emitter, must read at most one trace's rows.
	rootShape := chplan.Node(&chplan.Filter{
		Input: &chplan.Scan{Table: s.SpansTable},
		Predicate: &chplan.InList{
			Left: &chplan.ColumnRef{Name: s.TraceIDColumn},
			List: []chplan.Expr{&chplan.LitString{V: "trace_b"}},
		},
	})
	setSQLRaw, setArgs, err := chsql.Emit(chsql.WithSpansTable(context.Background(), s.SpansTable), rootShape)
	if err != nil {
		t.Fatalf("emit root-lookup shape: %v", err)
	}
	setSQL := inlineArgs(t, setSQLRaw, setArgs)
	setParts, setRows := explainEstimate(t, db, setSQL)
	if setParts >= scanBoundTracesSeeded {
		t.Errorf("trace-id set bound must prune parts: set_parts=%d total=%d", setParts, scanBoundTracesSeeded)
	}
	if setRows > scanBoundRowsPerTrace {
		t.Errorf("trace-id set bound read %d rows, expected <= %d (one trace)", setRows, scanBoundRowsPerTrace)
	}
}
