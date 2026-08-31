//go:build chdb

// This is the acceptance gate cerberus issue #2767 itself specifies: an
// EXPLAIN-based proof, against the package's own rendered production DDL
// (renderSignal, the same entry point trace_id_index_probe_chdb_test.go's
// bloom-filter bar and column_statistics_chdb_test.go's statistics probe
// both use), that the ClickHouse >= 25.5 proj_trace_id projection — stamped
// eligible for the >= 25.11 bitmap-filter path by
// internal/engine.settingMinTableRowsToUseProjectionIndex — is actually
// picked by the query optimizer for cerberus's REAL emitted query shapes,
// not a synthetic top-level `TraceId = ?` alone. The issue's own warning is
// the reason this file exists: cerberus's real TraceId predicates arrive
// inside a structure-tab IN-subquery (chplan.BoundedTraceScope) and a
// TraceQL structural join's recursive CTE (chplan.StructuralJoin) at least
// as often as a bare top-level equality, and a test that only proved the
// synthetic shape would pass even if the recursive/subquery shapes never
// touched the projection at all.
//
// Every plan below is the SAME chplan tree cerberus's own production code
// builds — the Tempo trace-by-id GET handler's equality Filter
// (internal/api/tempo/handler.go), the structure-tab top-N gate
// (internal/chplan/bounded_trace_scope.go), and a TraceQL structural join
// (internal/chplan/structural_join.go) — rendered through the real
// chsql.Emit production emitter, never a hand-copied SQL string. Only the
// EXPLAIN wrapper and the SETTINGS tail are test-only raw SQL, the same
// test-only raw-SQL convention trace_id_index_probe_chdb_test.go's own
// header comment documents.
package ddl

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"testing"

	_ "github.com/chdb-io/chdb-go/chdb/driver"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// traceIDProjectionSeedCount is the number of distinct traces seeded into
// otel_traces before each EXPLAIN probe below. It exists (rather than a
// single-row fixture, like trace_id_index_probe_chdb_test.go's probe) for
// TWO reasons: min_table_rows_to_use_projection_index is stamped to 0
// specifically because a real production table cannot be relied on to clear
// ClickHouse's own 1,000,000-row default inside a test — see
// internal/engine.traceIDBitmapFilterMinTableRows — and, more fundamentally,
// the 25.11 bitmap-filter mechanism PRUNES GRANULES: with a single granule
// (the default index_granularity is 8192 rows/part) there is nothing to
// prune and the optimizer has no reason to route through the projection at
// all regardless of any setting, the same way an EXPLAIN indexes=1 on one
// row shows "Granules: 1/1" for idx_trace_id too. traceIDProjectionSeedCount
// clears several granules' worth of rows so the plan has something to
// choose between.
const traceIDProjectionSeedCount = 50000

// openTraceIDProjectionChDB opens a fresh ephemeral chDB session and applies
// the package's own real production DDL for the logs and traces tables (via
// renderSignal) with TraceIDProjectionEnabled=true, mirroring
// openColumnStatisticsChDB / applyColumnStatisticsSignal's shape in
// column_statistics_chdb_test.go.
func openTraceIDProjectionChDB(t *testing.T) (db *sql.DB, cfg Config) {
	t.Helper()

	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping chdb: %v", err)
	}

	cfg = Config{TraceIDProjectionEnabled: true}.withDefaults()
	for _, s := range []Signal{Logs, Traces} {
		stmts, err := renderSignal(cfg, s)
		if err != nil {
			t.Fatalf("renderSignal(%s): %v", s, err)
		}
		for _, stmt := range stmts {
			if strings.HasPrefix(stmt, "CREATE TABLE IF NOT EXISTS") {
				stmt = promoteCreateTable(stmt)
			}
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("apply %s DDL:\n%s\nerr: %v", s, stmt, err)
			}
		}
	}
	return db, cfg
}

// seedTraces bulk-inserts traceIDProjectionSeedCount distinct traces into
// cfg.Tables.Traces via a single `INSERT ... SELECT ... FROM numbers(...)` —
// row-at-a-time db.Exec would be prohibitively slow at this fixture size —
// each a single root span (ParentSpanId = ”) so the structure-tab's
// BoundedTraceScope subquery (which ranks root spans) and the StructuralJoin
// recursive closure (which walks from a root) both have real rows to find
// rather than resolving against an empty table. The LAST trace ID
// (numerically highest, so it is deterministic across runs) is returned for
// the equality-lookup probe.
func seedTraces(t *testing.T, db *sql.DB, table string) (lastTraceID string) {
	t.Helper()
	insert := fmt.Sprintf(
		"INSERT INTO %s (Timestamp, TraceId, SpanId, ParentSpanId, ServiceName, SpanName) "+
			"SELECT now64(9), lower(leftPad(hex(number + 1), 32, '0')), lower(leftPad(hex(number + 1), 16, '0')), "+
			"'', 'probe-svc', 'probe-op' FROM numbers(%d)",
		table, traceIDProjectionSeedCount,
	)
	if _, err := db.Exec(insert); err != nil {
		t.Fatalf("bulk seed %d trace rows:\n%s\nerr: %v", traceIDProjectionSeedCount, insert, err)
	}
	if _, err := db.Exec("OPTIMIZE TABLE " + table + " FINAL"); err != nil {
		t.Fatalf("optimize final: %v", err)
	}
	return fmt.Sprintf("%032x", traceIDProjectionSeedCount)
}

// explainUsesTraceIDProjection runs `EXPLAIN indexes = 1, projections = 1
// <sql>` against db with args bound, appending the SETTINGS tail that stamps
// min_table_rows_to_use_projection_index=0 the SAME way
// internal/engine.SettingsRules.apply stamps it in production, and reports
// whether traceIDProjectionName appears anywhere in the plan text.
//
// The `projections = 1` toggle is load-bearing, verified empirically against
// this exact chDB build (not assumed from docs): `EXPLAIN indexes = 1` ALONE
// — the flag trace_id_index_probe_chdb_test.go uses for the bloom_filter skip
// index, which IS unconditionally listed under "Indexes:" — renders NO
// "Projections:" section at all, even when the optimizer genuinely picks
// proj_trace_id, because a used PROJECTION is reported under a separate
// EXPLAIN toggle a plain skip-index probe never needed. Without `projections
// = 1` this test would silently prove nothing and still show a green
// diff — the exact false-positive risk the issue's own acceptance-gate
// instruction warns about, just one level lower (a missing EXPLAIN flag
// rather than a missing query shape).
func explainUsesTraceIDProjection(t *testing.T, db *sql.DB, sql string, args []any) (plan string) {
	t.Helper()
	explainQuery := "EXPLAIN indexes = 1, projections = 1 " + sql +
		" SETTINGS " + settingMinTableRowsToUseProjectionIndexForTest + " = " + strconv.Itoa(traceIDBitmapFilterMinTableRowsForTest)
	rows, err := db.Query(explainQuery, args...)
	if err != nil {
		t.Fatalf("explain:\n%s\nerr: %v", explainQuery, err)
	}
	defer func() { _ = rows.Close() }()

	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("explain scan: %v", err)
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain rows: %v", err)
	}
	return b.String()
}

// settingMinTableRowsToUseProjectionIndexForTest /
// traceIDBitmapFilterMinTableRowsForTest mirror
// internal/engine.settingMinTableRowsToUseProjectionIndex /
// traceIDBitmapFilterMinTableRows exactly. Both are unexported — every
// setting-name constant in query_settings_rules.go is package-private, the
// same posture settingUseQueryConditionCache / settingOptimizeAggregationInOrder
// already take — so this test cannot import and reference them directly; it
// pins the same literal name and value instead. Keeping the two pairs
// byte-identical is this test's whole point: it is proving THAT specific
// production stamp actually moves the optimizer, not a stand-in value, so a
// future rename of either constant in internal/engine without a matching
// update here would make this test assert something cerberus no longer
// stamps.
const (
	settingMinTableRowsToUseProjectionIndexForTest = "min_table_rows_to_use_projection_index"
	traceIDBitmapFilterMinTableRowsForTest         = 0
)

// TestTraceIDProjection_IdempotentReapply pins that re-applying the SAME
// curated ADD PROJECTION ALTERs a second time — the boot-every-time behavior
// applySignal exercises for every signal — is a genuine no-op against a real
// server (IF NOT EXISTS), not merely idempotent by construction of the
// rendered SQL text, mirroring
// column_statistics_chdb_test.go's TestColumnStatistics_IdempotentReapply.
func TestTraceIDProjection_IdempotentReapply(t *testing.T) {
	db, cfg := openTraceIDProjectionChDB(t)
	for _, s := range []Signal{Logs, Traces} {
		stmts, err := renderSignal(cfg, s)
		if err != nil {
			t.Fatalf("renderSignal(%s): %v", s, err)
		}
		for _, stmt := range stmts {
			if !strings.Contains(stmt, traceIDProjectionName) {
				continue
			}
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("second apply of %q: %v", stmt, err)
			}
		}
	}
}

// TestTraceIDProjection_BelowFloorAppliesCleanly proves the version-gate-off
// path directly against a real server, not just at the render layer
// (TestRenderSignal_TraceIDProjectionEnabled/disabled in
// trace_id_projection_test.go): with TraceIDProjectionEnabled=false — a
// deployment below the 25.5 floor, or the feature simply off — the full DDL
// applies without error and system.projections carries no proj_trace_id row
// afterward. There is nothing to half-apply because nothing referencing the
// projection is ever sent.
func TestTraceIDProjection_BelowFloorAppliesCleanly(t *testing.T) {
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping chdb: %v", err)
	}

	cfg := Config{TraceIDProjectionEnabled: false}.withDefaults()
	for _, s := range []Signal{Logs, Traces} {
		stmts, err := renderSignal(cfg, s)
		if err != nil {
			t.Fatalf("renderSignal(%s): %v", s, err)
		}
		for _, stmt := range stmts {
			if strings.HasPrefix(stmt, "CREATE TABLE IF NOT EXISTS") {
				stmt = promoteCreateTable(stmt)
			}
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("apply %s DDL below floor:\n%s\nerr: %v", s, stmt, err)
			}
		}
	}

	var count int
	if err := db.QueryRow(
		"SELECT count() FROM system.projections WHERE name = ?", traceIDProjectionName,
	).Scan(&count); err != nil {
		t.Fatalf("query system.projections: %v", err)
	}
	if count != 0 {
		t.Errorf("system.projections carries %d %s row(s) with TraceIDProjectionEnabled=false, want 0", count, traceIDProjectionName)
	}
}

// TestTraceIDBitmapFilter_EqualityLookup proves the Tempo trace-by-id GET
// handler's real shape — a top-level `TraceId = ?` Filter over Scan(otel_traces)
// (internal/api/tempo/handler.go) — routes onto proj_trace_id.
func TestTraceIDBitmapFilter_EqualityLookup(t *testing.T) {
	db, cfg := openTraceIDProjectionChDB(t)
	traceID := seedTraces(t, db, cfg.Tables.Traces)

	plan := &chplan.Filter{
		Input: &chplan.Scan{Table: cfg.Tables.Traces},
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: traceIdColumn},
			Right: &chplan.LitString{V: traceID},
		},
	}
	sql, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	explainPlan := explainUsesTraceIDProjection(t, db, sql, args)
	if !strings.Contains(explainPlan, traceIDProjectionName) {
		t.Errorf("equality lookup %q never routes onto %s:\n%s", sql, traceIDProjectionName, explainPlan)
	}
}

// TestTraceIDBitmapFilter_BoundedTraceScope proves the structure-tab top-N
// gate's real shape — chplan.BoundedTraceScope, an `IN (<subquery>)`
// membership test (internal/chplan/bounded_trace_scope.go) — also routes
// onto proj_trace_id. This is exactly the "arrives inside an IN-subquery,
// not a top-level equality" shape the issue warns a naive test would miss.
func TestTraceIDBitmapFilter_BoundedTraceScope(t *testing.T) {
	db, cfg := openTraceIDProjectionChDB(t)
	seedTraces(t, db, cfg.Tables.Traces)

	plan := &chplan.Filter{
		Input: &chplan.Scan{Table: cfg.Tables.Traces},
		Predicate: &chplan.BoundedTraceScope{
			SpansTable:         cfg.Tables.Traces,
			TraceIDColumn:      traceIdColumn,
			ParentSpanIDColumn: "ParentSpanId",
			TimestampColumn:    "Timestamp",
			TraceLimit:         20,
		},
	}
	sql, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	explainPlan := explainUsesTraceIDProjection(t, db, sql, args)
	if !strings.Contains(explainPlan, traceIDProjectionName) {
		t.Errorf("BoundedTraceScope IN-subquery %q never routes onto %s:\n%s", sql, traceIDProjectionName, explainPlan)
	}
}

// traceIDProjectionRestrictionSize is how many literal trace IDs
// TestTraceIDBitmapFilter_StructuralJoinRecursiveCTE restricts a
// StructuralJoin to — small enough that resolving them via proj_trace_id
// (a handful of granules) beats a full unrestricted table scan, mirroring
// the /api/search two-phase orchestrator's own real usage: TraceIDRestriction
// is set ONLY between phase A (a narrow top-N rank) and phase B (a wide fetch
// of just those traces) — see chplan.StructuralJoin.TraceIDRestriction's own
// doc comment — never left unbounded the way a synthetic "does this ever
// route onto the projection at all" smoke test might construct it.
const traceIDProjectionRestrictionSize = 5

// TestTraceIDBitmapFilter_StructuralJoinRecursiveCTE proves a TraceQL
// structural join's real BOUNDED shape — chplan.StructuralJoin with
// TraceIDRestriction set (the /api/search two-phase orchestrator's phase B,
// internal/chplan/structural_join.go), which lowers to a `WITH RECURSIVE`
// closure whose every physical scan carries a literal `TraceId IN (...)` —
// also routes onto proj_trace_id, even though StructuralJoin itself carries
// no Expr-typed TraceId field (see internal/engine.
// eligibleForTraceIDBitmapFilter's own doc comment on why that node is
// matched by TYPE, not by expression). This is the "arrives inside a
// recursive CTE" shape the issue warns a naive test would miss.
//
// An UNRESTRICTED StructuralJoin (TraceIDRestriction nil) is deliberately
// NOT probed here: with no selective predicate anywhere in the closure, a
// full scan of every span is exactly what the query asks for, and no
// index — bloom filter or projection — has anything to prune. Asserting the
// projection fires there would be asserting a regression against a query
// that never asks for one, not proving cerberus's real emitted shape.
func TestTraceIDBitmapFilter_StructuralJoinRecursiveCTE(t *testing.T) {
	db, cfg := openTraceIDProjectionChDB(t)
	seedTraces(t, db, cfg.Tables.Traces)

	restriction := make([]string, traceIDProjectionRestrictionSize)
	for i := range restriction {
		restriction[i] = fmt.Sprintf("%032x", i+1)
	}
	plan := &chplan.StructuralJoin{
		Left:               &chplan.Scan{Table: cfg.Tables.Traces},
		Right:              &chplan.Scan{Table: cfg.Tables.Traces},
		Op:                 chplan.StructuralDescendant,
		TraceIDColumn:      traceIdColumn,
		SpanIDColumn:       "SpanId",
		ParentSpanIDColumn: "ParentSpanId",
		TraceIDRestriction: restriction,
	}
	sql, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "WITH RECURSIVE") {
		t.Fatalf("test fixture bug: StructuralJoin did not emit a recursive CTE:\n%s", sql)
	}

	explainPlan := explainUsesTraceIDProjection(t, db, sql, args)
	if !strings.Contains(explainPlan, traceIDProjectionName) {
		t.Errorf("structural join recursive CTE %q never routes onto %s:\n%s", sql, traceIDProjectionName, explainPlan)
	}
}
