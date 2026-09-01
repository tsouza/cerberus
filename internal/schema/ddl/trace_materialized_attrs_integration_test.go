//go:build integration

package ddl_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
	"github.com/tsouza/cerberus/internal/traceql"
	tempo "github.com/tsouza/cerberus/internal/traceql/ast"
)

// TestApply_TraceMaterializedAttrColumns_LazyDefaultCorrectness is the
// end-to-end proof for cerberus issue #2776's central, most-likely-bug
// risk: whether a materialized attribute column — declared `LowCardinality
// (String) DEFAULT SpanAttributes['<key>']`, NOT `MATERIALIZED` — reads
// correctly for rows that existed BEFORE the ADD COLUMN ALTER ran, before
// any backfill (`MATERIALIZE COLUMN`) has touched their part.
//
// This is cerberus's first ADD COLUMN onto a table it does not own the
// CREATE TABLE body of (see schema.Traces.MaterializedSpanAttributeColumns'
// doc). Neither question below is answerable against chDB or a stubbed
// connection — only a real ClickHouse server settles a DEFAULT column's
// lazy-evaluation semantics for pre-existing parts, matching this
// package's other MATERIALIZE-adjacent integration tests (see
// column_ttl_integration_test.go's identical real-server justification).
func TestApply_TraceMaterializedAttrColumns_LazyDefaultCorrectness(t *testing.T) {
	conn, database := startClickHouse(t)
	ctx := context.Background()

	statusCodeColumn := "__cerberus_materialized_http.status_code"
	baseCfg := ddl.Config{Database: database}

	// Step 1: create otel_traces WITHOUT the materialized column — the
	// pre-#2776 shape.
	if err := ddl.ApplyWithConfig(ctx, conn, baseCfg, []ddl.Signal{ddl.Traces}); err != nil {
		t.Fatalf("Apply (base): %v", err)
	}

	// Step 2: insert rows BEFORE the column exists — these land in a part
	// the later ADD COLUMN will not rewrite.
	insertPreAlter := fmt.Sprintf(`
		INSERT INTO %s.otel_traces (Timestamp, TraceId, SpanId, ServiceName, SpanName, Duration, SpanAttributes)
		VALUES
		  (now(), 'trace-pre-1', 'span-pre-1', 'svc', 'op', 100, map('http.status_code', '200')),
		  (now(), 'trace-pre-2', 'span-pre-2', 'svc', 'op', 100, map('http.status_code', '404')),
		  (now(), 'trace-pre-3', 'span-pre-3', 'svc', 'op', 100, map('other_key', 'x'))
	`, database)
	if err := conn.Exec(ctx, insertPreAlter); err != nil {
		t.Fatalf("insert pre-ALTER rows: %v", err)
	}

	// Step 3: re-Apply with the materialized column enabled — this issues
	// ONLY the metadata-only ADD COLUMN (see
	// renderTraceMaterializedAttrColumns' doc for why MATERIALIZE COLUMN is
	// a separate, manual runbook step, never auto-issued here).
	matCfg := ddl.Config{
		Database:                           database,
		TraceMaterializedAttributesEnabled: true,
		MaterializedSpanAttributeColumns:   map[string]string{"http.status_code": statusCodeColumn},
	}
	if err := ddl.ApplyWithConfig(ctx, conn, matCfg, []ddl.Signal{ddl.Traces}); err != nil {
		t.Fatalf("Apply (materialized attrs): %v", err)
	}

	createSQL := createTableQuery(ctx, t, conn, database, "otel_traces")
	if !strings.Contains(createSQL, statusCodeColumn) ||
		!strings.Contains(createSQL, "LowCardinality(String)") ||
		!strings.Contains(createSQL, "DEFAULT") ||
		!strings.Contains(createSQL, "SpanAttributes") {
		t.Fatalf("otel_traces CREATE missing the expected materialized column DEFAULT clause:\n%s", createSQL)
	}

	// Step 4: THE central correctness assertion — read the new column on
	// the PRE-ALTER rows, with zero mutation queued (no MATERIALIZE COLUMN
	// issued yet). ClickHouse must compute the DEFAULT expression LAZILY
	// from each row's own stored SpanAttributes at read time; a NULL or
	// empty leak here would be the exact bug class this test exists to
	// catch.
	assertMaterializedColumnMatchesMap(ctx, t, conn, database, statusCodeColumn)

	pending := pendingMutationIDs(ctx, t, conn, database, "otel_traces")
	if len(pending) != 0 {
		t.Fatalf("mutations queued before any MATERIALIZE COLUMN was issued (test precondition violated): %v", pending)
	}

	// Step 5: a row inserted AFTER the ALTER must also read correctly (the
	// DEFAULT is computed eagerly at insert time into the new part).
	insertPostAlter := fmt.Sprintf(`
		INSERT INTO %s.otel_traces (Timestamp, TraceId, SpanId, ServiceName, SpanName, Duration, SpanAttributes)
		VALUES (now(), 'trace-post-1', 'span-post-1', 'svc', 'op', 100, map('http.status_code', '500'))
	`, database)
	if err := conn.Exec(ctx, insertPostAlter); err != nil {
		t.Fatalf("insert post-ALTER row: %v", err)
	}
	assertMaterializedColumnMatchesMap(ctx, t, conn, database, statusCodeColumn)

	// Step 6: run the MATERIALIZE COLUMN backfill via the typed
	// chsql.AlterTableMaterializeColumn builder — the manual runbook step
	// documented in docs/operations.md — and confirm the column stays
	// byte-identical to the map afterward: zero divergence before, during
	// (see the PR body's concurrent-write evidence), or after backfill.
	sql := chsql.AlterTableMaterializeColumn(database, "otel_traces", statusCodeColumn).SQL() + " SETTINGS mutations_sync = 1"
	if err := conn.Exec(ctx, sql); err != nil {
		t.Fatalf("MATERIALIZE COLUMN: %v", err)
	}
	assertMaterializedColumnMatchesMap(ctx, t, conn, database, statusCodeColumn)

	// Re-running Apply (the boot-time idempotency contract every DDL
	// statement in this package relies on) must be a safe no-op — both the
	// ADD COLUMN IF NOT EXISTS and a stray MATERIALIZE COLUMN against an
	// already-backfilled column.
	if err := ddl.ApplyWithConfig(ctx, conn, matCfg, []ddl.Signal{ddl.Traces}); err != nil {
		t.Fatalf("re-Apply (idempotency): %v", err)
	}
	if err := conn.Exec(ctx, sql); err != nil {
		t.Fatalf("re-run MATERIALIZE COLUMN on an already-backfilled column: %v", err)
	}
}

// assertMaterializedColumnMatchesMap queries every row's map value and
// materialized-column value side by side and fails on any mismatch — the
// zero-divergence contract schema.Traces.MaterializedSpanAttributeColumns
// documents.
func assertMaterializedColumnMatchesMap(ctx context.Context, t *testing.T, conn driver.Conn, database, column string) {
	t.Helper()
	rows, err := conn.Query(ctx, fmt.Sprintf(
		"SELECT TraceId, SpanAttributes['http.status_code'], `%s` FROM %s.otel_traces ORDER BY TraceId",
		column, database,
	))
	if err != nil {
		t.Fatalf("query map vs materialized column: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var traceID, mapVal, colVal string
		if err := rows.Scan(&traceID, &mapVal, &colVal); err != nil {
			t.Fatalf("scan: %v", err)
		}
		n++
		if mapVal != colVal {
			t.Errorf("trace %s: map value %q != materialized column value %q — DEFAULT lazy-read divergence", traceID, mapVal, colVal)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if n == 0 {
		t.Fatalf("query returned zero rows — test precondition violated")
	}
}

// pendingMutationIDs returns the mutation_id values from system.mutations
// for table that have not yet completed (is_done = 0).
func pendingMutationIDs(ctx context.Context, t *testing.T, conn driver.Conn, database, table string) []string {
	t.Helper()
	rows, err := conn.Query(ctx, fmt.Sprintf(
		"SELECT mutation_id FROM system.mutations WHERE database = '%s' AND table = '%s' AND is_done = 0",
		database, table,
	))
	if err != nil {
		t.Fatalf("query system.mutations: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}

// TestLowerTraceQL_MaterializedColumnRoutingRealCH is the strongest form of
// cerberus issue #2776's TraceQL routing-correctness requirement: the
// SAME TraceQL query, lowered TWICE — once against a schema with the
// materialized column configured, once against schema.DefaultOTelTraces()
// (map-only) — must return IDENTICAL result sets when both are actually
// EXECUTED against a real ClickHouse server carrying real data. This ties
// internal/traceql's routing logic directly to execution, not just to the
// emitted SQL's shape (see TestLowerAttribute_MaterializedColumnRouting in
// internal/traceql for the shape-only assertions).
func TestLowerTraceQL_MaterializedColumnRoutingRealCH(t *testing.T) {
	conn, database := startClickHouse(t)
	ctx := context.Background()

	statusCodeColumn := "__cerberus_materialized_http.status_code"
	matCfg := ddl.Config{
		Database:                           database,
		TraceMaterializedAttributesEnabled: true,
		MaterializedSpanAttributeColumns:   map[string]string{"http.status_code": statusCodeColumn},
	}
	if err := ddl.ApplyWithConfig(ctx, conn, matCfg, []ddl.Signal{ddl.Traces}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	insert := fmt.Sprintf(`
		INSERT INTO %s.otel_traces (Timestamp, TraceId, SpanId, ServiceName, SpanName, Duration, SpanAttributes)
		VALUES
		  (now(), 'trace-200-a', 'span-1', 'svc', 'op', 100, map('http.status_code', '200')),
		  (now(), 'trace-200-b', 'span-2', 'svc', 'op', 100, map('http.status_code', '200')),
		  (now(), 'trace-404',   'span-3', 'svc', 'op', 100, map('http.status_code', '404')),
		  (now(), 'trace-500',   'span-4', 'svc', 'op', 100, map('http.status_code', '500'))
	`, database)
	if err := conn.Exec(ctx, insert); err != nil {
		t.Fatalf("insert: %v", err)
	}

	materialized := schema.DefaultOTelTraces()
	materialized.SpansTable = "otel_traces"
	materialized.MaterializedSpanAttributeColumns = map[string]string{"http.status_code": statusCodeColumn}
	mapOnly := schema.DefaultOTelTraces()
	mapOnly.SpansTable = "otel_traces"

	for _, tc := range []struct {
		name  string
		query string
		want  []string // expected TraceId set
	}{
		{"eq_200", `{ span.http.status_code = "200" }`, []string{"trace-200-a", "trace-200-b"}},
		{"eq_404", `{ span.http.status_code = "404" }`, []string{"trace-404"}},
		{"gt_400_numeric", `{ span.http.status_code > 400 }`, []string{"trace-404", "trace-500"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			matRows := execTraceQLTraceIDs(ctx, t, conn, database, tc.query, materialized)
			mapRows := execTraceQLTraceIDs(ctx, t, conn, database, tc.query, mapOnly)
			assertSameStringSet(t, "materialized-column", matRows, "map", mapRows)
			assertSameStringSet(t, "materialized-column", matRows, "expected", tc.want)
		})
	}
}

// execTraceQLTraceIDs lowers and executes query against s, returning the
// distinct TraceId values of the matched spans.
func execTraceQLTraceIDs(ctx context.Context, t *testing.T, conn driver.Conn, database, query string, s schema.Traces) []string {
	t.Helper()
	expr, err := tempo.Parse(query)
	if err != nil {
		t.Fatalf("Parse(%q): %v", query, err)
	}
	plan, err := traceql.Lower(ctx, expr, s)
	if err != nil {
		t.Fatalf("Lower(%q): %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(ctx, plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}
	// clickhouse-go's driver.Conn.Query accepts chsql's `?`-placeholder SQL
	// directly with the args passed positionally — the same call shape
	// internal/chclient's retry wrapper uses in production.
	rows, err := conn.Query(ctx, fmt.Sprintf("SELECT DISTINCT TraceId FROM (%s)", sqlStrWithDatabase(sqlStr, database)), args...)
	if err != nil {
		t.Fatalf("query %q %v: %v", sqlStr, args, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}

// sqlStrWithDatabase rewrites the bare (unqualified) table reference the
// TraceQL lowering emits into a database-qualified one, so the query runs
// against this test's per-test database rather than the connection's
// default. Every TraceQL-lowered SELECT this package's tests execute reads
// from exactly one table (`otel_traces`), so a literal substitution is
// sufficient — no general SQL rewriting is needed.
func sqlStrWithDatabase(sqlStr, database string) string {
	return strings.ReplaceAll(sqlStr, "`otel_traces`", fmt.Sprintf("%s.`otel_traces`", database))
}

// assertSameStringSet fails the test unless a and b contain the same
// elements (order-independent, duplicates ignored).
func assertSameStringSet(t *testing.T, aName string, a []string, bName string, b []string) {
	t.Helper()
	toSet := func(xs []string) map[string]bool {
		m := make(map[string]bool, len(xs))
		for _, x := range xs {
			m[x] = true
		}
		return m
	}
	as, bs := toSet(a), toSet(b)
	if len(as) != len(bs) {
		t.Fatalf("%s set %v != %s set %v", aName, a, bName, b)
	}
	for k := range as {
		if !bs[k] {
			t.Fatalf("%s set %v != %s set %v", aName, a, bName, b)
		}
	}
}
