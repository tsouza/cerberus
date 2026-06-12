//go:build chdb

// Cycle-guard regression (RC1 TraceQL structural `>>` / `<<`, task #78):
// the recursive `WITH RECURSIVE` closure that lowers a descendant/ancestor
// query used to be emitted UNBOUNDED (MaxDepth == 0 → no `c._depth < N`
// cap), relying on the natural fixpoint (no further rows once ParentSpanId
// hits the trace root). A trace whose parent-chain contains a CYCLE — a
// span whose ParentSpanId points back into its own ancestry (clock skew,
// instrumentation bug, OTLP span-id reuse) — never reaches a fixpoint, so
// the unbounded CTE drives ClickHouse past its
// `max_recursive_cte_evaluation_depth` (default 1000) and FAILS the WHOLE
// query with error 306 (TOO_DEEP_RECURSION). A single malformed trace must
// never 500 a structural TraceQL query.
//
// The #78 fix bounds the recursive arm with a default safety cap
// (chsql.defaultStructuralRecursionDepth) when the plan leaves MaxDepth
// unset, so a cyclic trace degrades to a BOUNDED / partial closure with no
// error. This guard pins both halves:
//
//   - fails-before: the SAME query WITHOUT the depth cap (the pre-fix
//     shape, reconstructed inline) errors with CH code 306 on the cyclic
//     seed — proving the bug was real.
//   - passes-after: the real cerberus-emitted SQL (which now always
//     carries the cap) returns a bounded result on the SAME seed with NO
//     error, and still returns the correct rows for the ACYCLIC trace in
//     the same table (the cap doesn't truncate real, sub-cap closures).
//
// Build-tagged `chdb`, same lane as the other chDB execs.
package perf

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "github.com/chdb-io/chdb-go/chdb/driver"

	tempo "github.com/grafana/tempo/pkg/traceql"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
)

// cycleSeed builds two traces in otel_traces:
//
//   - trace 'a…1' is ACYCLIC: root(svc=root) → mid(svc=mid) → leaf(svc=leaf).
//     `root >> leaf` must return exactly the leaf span.
//   - trace 'c…1' contains a SPAN-ID SELF-LOOP: its 'mid' span's
//     ParentSpanId points to itself (ParentSpanId == SpanId), the exact
//     malformed shape #78 calls out (clock skew / instrumentation bug /
//     OTLP span-id reuse). The descendant walk `t.ParentSpanId = c.SpanId`
//     re-joins that span to itself on every level, so an UNBOUNDED closure
//     never terminates and drives CH past its recursion limit (error 306).
//     CH's own recursive-row dedup does NOT catch a self-loop (the
//     self-join keeps re-emitting the same row), which is why a hard depth
//     bound is required.
const cycleSeedDDL = `CREATE TABLE otel_traces (
    TraceId String,
    SpanId String,
    ParentSpanId String,
    SpanName String DEFAULT '',
    Duration UInt64 DEFAULT 0,
    Timestamp UInt64 DEFAULT 0,
    ResourceAttributes Map(String, String) DEFAULT map(),
    SpanAttributes Map(String, String) DEFAULT map(),
    StatusCode String DEFAULT '',
    StatusMessage String DEFAULT '',
    SpanKind String DEFAULT '',
    ScopeName String DEFAULT '',
    ScopeVersion String DEFAULT ''
) ENGINE = Memory;`

// Rows 1-3: acyclic trace root -> mid -> leaf.
// Rows 4-7: cyclic trace. root(0x11) -> mid(0x12, parent=0x11). mid ALSO
// appears with a SELF-LOOP row (SpanId 0x12, ParentSpanId 0x12 — the
// malformed shape #78 calls out). leaf(0x13) hangs below mid. The
// descendant walk reaches 0x12 from root, then re-joins 0x12 to itself
// via the self-loop row on every level, so an UNBOUNDED closure never
// terminates and trips CH error 306.
const cycleSeedInsert = `INSERT INTO otel_traces (TraceId, SpanId, ParentSpanId, ResourceAttributes) VALUES
    ('a0000000000000000000000000000001', '0000000000000001', '',                 map('service.name', 'root')),
    ('a0000000000000000000000000000001', '0000000000000002', '0000000000000001', map('service.name', 'mid')),
    ('a0000000000000000000000000000001', '0000000000000003', '0000000000000002', map('service.name', 'leaf')),
    ('c0000000000000000000000000000001', '0000000000000011', '',                 map('service.name', 'root')),
    ('c0000000000000000000000000000001', '0000000000000012', '0000000000000011', map('service.name', 'mid')),
    ('c0000000000000000000000000000001', '0000000000000012', '0000000000000012', map('service.name', 'mid')),
    ('c0000000000000000000000000000001', '0000000000000013', '0000000000000012', map('service.name', 'leaf'));`

// emitCycleDescendantSQL lowers `{ resource.service.name = "root" } >>
// { resource.service.name = "leaf" }` through the real cerberus chain.
func emitCycleDescendantSQL(t *testing.T) (string, []any) {
	t.Helper()
	expr, err := tempo.Parse(`{ resource.service.name = "root" } >> { resource.service.name = "leaf" }`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	plan, err := traceql.Lower(context.Background(), expr, schema.DefaultOTelTraces())
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	sqlText, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	return sqlText, args
}

// unboundedDescendantSQL reconstructs the PRE-#78 closure shape: the same
// `root >> leaf` walk WITHOUT the `c._depth < N` cap in the recursive arm.
// On the cyclic trace this never reaches a fixpoint and drives CH past its
// recursive-CTE depth limit (error 306). Inlined (not emitted) precisely
// because the fix made the cap unconditional in the real emitter.
func unboundedDescendantSQL() string {
	return `SELECT count() FROM (
  WITH RECURSIVE _closure AS (
    SELECT DISTINCT TraceId, SpanId, ParentSpanId, 0 AS _depth
      FROM (SELECT * FROM otel_traces WHERE ResourceAttributes['service.name'] = 'root') AS _seed
    UNION ALL
    SELECT DISTINCT t.TraceId, t.SpanId, t.ParentSpanId, c._depth + 1
      FROM otel_traces AS t
      INNER JOIN _closure AS c
        ON t.TraceId = c.TraceId AND t.ParentSpanId = c.SpanId
  )
  SELECT DISTINCT TraceId, SpanId FROM _closure WHERE _depth > 0
) AS L INNER JOIN (SELECT * FROM otel_traces WHERE ResourceAttributes['service.name'] = 'leaf') AS R
  ON L.TraceId = R.TraceId AND L.SpanId = R.SpanId`
}

func TestStructuralRecursive_CycleGuard_ChDB(t *testing.T) {
	// Each half runs on its OWN chDB connection seeded fresh. A query that
	// errors 306 (the fails-before half) can leave the chDB-go session in
	// a state where the next query's parquet result decode panics in the
	// driver — an artefact of the embedded driver, unrelated to cerberus —
	// so the bounded passes-after query must never share a connection with
	// the deliberately-erroring one.
	seed := func(db *sql.DB) {
		if _, err := db.Exec("CREATE DATABASE IF NOT EXISTS default"); err != nil {
			t.Fatalf("create db: %v", err)
		}
		if _, err := db.Exec("DROP TABLE IF EXISTS otel_traces"); err != nil {
			t.Fatalf("drop: %v", err)
		}
		if _, err := db.Exec(cycleSeedDDL); err != nil {
			t.Fatalf("ddl: %v", err)
		}
		if _, err := db.Exec(cycleSeedInsert); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// --- passes-after: the cerberus-emitted (bounded) SQL does NOT error -
	// Run FIRST on a clean connection so it never inherits the poisoned
	// session the 306 error leaves behind.
	boundedSQL, boundedArgs := emitCycleDescendantSQL(t)
	if !strings.Contains(boundedSQL, "c._depth < ") {
		t.Fatalf("passes-after precondition: the emitted SQL must carry a depth bound (`c._depth < N`); "+
			"got:\n%s", boundedSQL)
	}
	dbOK := openChDB(t)
	seed(dbOK)
	inner := "(" + stripTrailingSemi(boundedSQL) + ")"
	var total int64
	if err := dbOK.QueryRow("SELECT count() FROM "+inner, boundedArgs...).Scan(&total); err != nil {
		t.Fatalf("passes-after FAILED: the bounded structural query errored on a table containing a "+
			"cyclic trace — a single malformed trace must not 500 the query: %v", err)
	}
	// The acyclic trace's leaf MUST be present — the bound must not have
	// truncated a real, sub-cap closure.
	const acyclicTrace = "a0000000000000000000000000000001"
	var acyclicHits int64
	if err := dbOK.QueryRow(
		"SELECT count() FROM "+inner+" WHERE TraceId = '"+acyclicTrace+"'", boundedArgs...,
	).Scan(&acyclicHits); err != nil {
		t.Fatalf("passes-after: acyclic-leaf probe errored: %v", err)
	}
	if acyclicHits < 1 {
		t.Errorf("passes-after: the acyclic trace's leaf is missing from the bounded result "+
			"(acyclic-trace rows = %d) — the depth cap must not truncate a real, sub-cap closure.",
			acyclicHits)
	}
	t.Logf("passes-after OK: bounded query returned %d total row(s) (acyclic-trace rows=%d) with no "+
		"error on the cyclic table.", total, acyclicHits)

	// --- fails-before: the unbounded shape errors on the cyclic seed ----
	// Separate connection: this query deliberately errors 306, which we
	// must not let bleed into the passes-after run above.
	dbErr := openChDB(t)
	seed(dbErr)
	var sink int64
	errUnbounded := dbErr.QueryRow("SELECT count() FROM (" + unboundedDescendantSQL() + ")").Scan(&sink)
	if errUnbounded == nil {
		t.Fatalf("fails-before precondition not reproduced: the UNBOUNDED closure must error on a "+
			"cyclic trace (CH code 306), but it succeeded (count=%d). The cycle-guard regression is "+
			"mis-seeded — the seed trace is not actually cyclic.", sink)
	}
	if !strings.Contains(errUnbounded.Error(), "306") &&
		!strings.Contains(strings.ToLower(errUnbounded.Error()), "recursi") {
		t.Fatalf("fails-before: expected a CH recursion-depth error (code 306) from the unbounded "+
			"closure on the cyclic trace, got a different error: %v", errUnbounded)
	}
	t.Logf("fails-before OK: unbounded closure errors on cyclic trace: %v", errUnbounded)
}
