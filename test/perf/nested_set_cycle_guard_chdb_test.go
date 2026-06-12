//go:build chdb

// Cycle-guard regression (RC1 TraceQL nested-set numbering / Grafana Traces
// Drilldown STRUCTURE tab). The `| select(nestedSetParent | nestedSetLeft |
// nestedSetRight)` projection (and the group-by-nested-set metric) lowers to
// a chplan.NestedSetAnnotate, which the emitter renders as a per-trace
// `WITH RECURSIVE _cerberus_ns_paths` parent-chain walk that recomputes
// Tempo's ingest-time DFS numbering from the (TraceId, SpanId, ParentSpanId)
// adjacency.
//
// That walk used to be emitted UNBOUNDED — the recursive CTE carried NO
// `_depth` column and NO `c._depth < N` cap, relying on the natural fixpoint
// (no child rows left once ParentSpanId stops matching a numbered SpanId). A
// trace whose parent chain contains a CYCLE — a span whose ParentSpanId
// points back into its own ancestry (clock skew, instrumentation bug, OTLP
// span-id reuse) — never reaches a fixpoint, so the unbounded CTE drives
// ClickHouse past its `max_recursive_cte_evaluation_depth` (default 1000) and
// FAILS the WHOLE query with error 306 (TOO_DEEP_RECURSION) → HTTP 500. A
// single malformed trace must never 500 the structure tab.
//
// This is the sibling of the structural `>>` / `<<` path that the cycle
// guard in structural_cycle_guard_chdb_test.go pins (PR #808 /
// chsql.defaultStructuralRecursionDepth); the nested-set numbering walk was
// never capped. The fix adds a `_depth` column (seed 0, increment per level)
// and a `c._depth < <cap>` bound — sharing the structural cap constant — so a
// cyclic trace degrades to a BOUNDED / partial numbering with no error.
//
// This guard pins both halves, mirroring the structural guard:
//
//   - fails-before: the SAME numbering walk WITHOUT the depth cap (the
//     pre-fix shape, reconstructed inline) errors with CH code 306 on the
//     cyclic seed — proving the bug was real.
//   - passes-after: the real cerberus-emitted SQL (which now always carries
//     the cap) returns a bounded result on the SAME seed with NO error, and
//     still numbers the ACYCLIC trace's spans correctly in the same table
//     (the cap doesn't truncate real, sub-cap numberings).
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

// nsCycleSeedDDL / nsCycleSeedInsert build two traces in otel_traces:
//
//   - trace 'a…1' is ACYCLIC: root → mid → leaf (a clean 3-span chain).
//     Its nested-set numbering must come out non-zero for every span (the
//     LEFT JOIN against the numbering CTE matches all three).
//   - trace 'c…1' contains a SPAN-ID SELF-LOOP: its 'mid' span appears a
//     second time with ParentSpanId == SpanId (the exact malformed shape the
//     fix calls out). The numbering walk `t.ParentSpanId = c.SpanId` re-joins
//     that span to itself on every level, so an UNBOUNDED CTE never
//     terminates and drives CH past its recursion limit (error 306). CH's own
//     recursive-row dedup does NOT catch a self-loop (the path string keeps
//     growing one element per level, so every iteration emits a new, distinct
//     row), which is why a hard depth bound is required.
const nsCycleSeedDDL = `CREATE TABLE otel_traces (
    TraceId String,
    SpanId String,
    ParentSpanId String,
    SpanName String DEFAULT '',
    Duration UInt64 DEFAULT 0,
    Timestamp DateTime64(9) DEFAULT toDateTime64(0, 9),
    ResourceAttributes Map(String, String) DEFAULT map(),
    SpanAttributes Map(String, String) DEFAULT map(),
    StatusCode String DEFAULT '',
    StatusMessage String DEFAULT '',
    SpanKind String DEFAULT '',
    ScopeName String DEFAULT '',
    ScopeVersion String DEFAULT ''
) ENGINE = Memory;`

// Rows 1-3: acyclic trace root(0x01) -> mid(0x02) -> leaf(0x03).
// Rows 4-7: cyclic trace. root(0x11) -> mid(0x12, parent=0x11). mid ALSO
// appears with a SELF-LOOP row (SpanId 0x12, ParentSpanId 0x12 — the
// malformed shape the fix calls out). leaf(0x13) hangs below mid. The
// numbering walk reaches 0x12 from root, then re-joins 0x12 to itself via
// the self-loop row on every level, so an UNBOUNDED closure never terminates
// and trips CH error 306.
const nsCycleSeedInsert = `INSERT INTO otel_traces (TraceId, SpanId, ParentSpanId, Timestamp, ResourceAttributes) VALUES
    ('a0000000000000000000000000000001', '0000000000000001', '',                 toDateTime64(1, 9), map('service.name', 'root')),
    ('a0000000000000000000000000000001', '0000000000000002', '0000000000000001', toDateTime64(2, 9), map('service.name', 'mid')),
    ('a0000000000000000000000000000001', '0000000000000003', '0000000000000002', toDateTime64(3, 9), map('service.name', 'leaf')),
    ('c0000000000000000000000000000001', '0000000000000011', '',                 toDateTime64(1, 9), map('service.name', 'root')),
    ('c0000000000000000000000000000001', '0000000000000012', '0000000000000011', toDateTime64(2, 9), map('service.name', 'mid')),
    ('c0000000000000000000000000000001', '0000000000000012', '0000000000000012', toDateTime64(2, 9), map('service.name', 'mid')),
    ('c0000000000000000000000000000001', '0000000000000013', '0000000000000012', toDateTime64(3, 9), map('service.name', 'leaf'));`

// emitNestedSetSelectSQL lowers `{ } | select(nestedSetParent, nestedSetLeft,
// nestedSetRight)` — the Grafana Traces Drilldown STRUCTURE-tab projection —
// through the real cerberus chain. The empty spanset filter keeps every span
// of both traces in the input, so the numbering CTE walks the cyclic trace.
func emitNestedSetSelectSQL(t *testing.T) (string, []any) {
	t.Helper()
	expr, err := tempo.Parse(`{ } | select(nestedSetParent, nestedSetLeft, nestedSetRight)`)
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

// unboundedNestedSetNumberingSQL reconstructs the PRE-fix numbering shape:
// the same per-trace `_cerberus_ns_paths` parent-chain walk WITHOUT the
// `_depth` column / `c._depth < N` cap in the recursive arm. On the cyclic
// trace this never reaches a fixpoint (the self-loop row re-extends the path
// on every level) and drives CH past its recursive-CTE depth limit (error
// 306). Inlined (not emitted) precisely because the fix made the cap
// unconditional in the real emitter. It mirrors the live emitter's CTE down
// to the entry/exit/parent-lookup event ARRAY JOIN so the only structural
// difference from cerberus's output is the missing depth bound.
func unboundedNestedSetNumberingSQL() string {
	return `SELECT TraceId, SpanId,
       toInt64(maxIf(_erank, _etype = 0)) AS ns_left,
       toInt64(maxIf(_erank, _etype = 2)) AS ns_right
FROM (
  SELECT TraceId, SpanId, _ekey, _etype,
         sum(_etype != 1) OVER (PARTITION BY TraceId ORDER BY _ekey ASC, _etype ASC
                                ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS _erank
  FROM (
    SELECT TraceId, SpanId, _ev.1 AS _ekey, _ev.2 AS _etype
    FROM (
      WITH RECURSIVE _cerberus_ns_paths AS (
        SELECT TraceId, SpanId, ParentSpanId,
               concat(leftPad(toString(toUnixTimestamp64Nano(Timestamp)), 20, '0'),
                      leftPad(toString(sipHash64(SpanId)), 20, '0')) AS _path
          FROM otel_traces
         WHERE ParentSpanId = ''
        UNION ALL
        SELECT t.TraceId, t.SpanId, t.ParentSpanId,
               concat(c._path,
                      concat(leftPad(toString(toUnixTimestamp64Nano(t.Timestamp)), 20, '0'),
                             leftPad(toString(sipHash64(t.SpanId)), 20, '0')))
          FROM otel_traces AS t
          INNER JOIN _cerberus_ns_paths AS c
            ON t.TraceId = c.TraceId AND t.ParentSpanId = c.SpanId
      )
      SELECT TraceId, SpanId, _ev
      FROM _cerberus_ns_paths
      ARRAY JOIN arrayFilter(e -> NOT (e.2 = 1 AND e.1 = ''),
                 [(_path, 0), (concat(_path, '~'), 2),
                  (substring(_path, 1, length(_path) - 40), 1)]) AS _ev
    )
  )
)
GROUP BY TraceId, SpanId`
}

func TestNestedSet_CycleGuard_ChDB(t *testing.T) {
	// Each half runs on its OWN chDB connection seeded fresh. A query that
	// errors 306 (the fails-before half) can leave the chDB-go session in a
	// state where the next query's parquet result decode panics in the
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
		if _, err := db.Exec(nsCycleSeedDDL); err != nil {
			t.Fatalf("ddl: %v", err)
		}
		if _, err := db.Exec(nsCycleSeedInsert); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// --- passes-after: the cerberus-emitted (bounded) SQL does NOT error ---
	// Run FIRST on a clean connection so it never inherits the poisoned
	// session the 306 error leaves behind.
	boundedSQL, boundedArgs := emitNestedSetSelectSQL(t)
	if !strings.Contains(boundedSQL, "_cerberus_ns_paths") {
		t.Fatalf("passes-after precondition: the emitted SQL must lower the nested-set numbering "+
			"through the `_cerberus_ns_paths` recursive CTE; got:\n%s", boundedSQL)
	}
	if !strings.Contains(boundedSQL, "c._depth < ") {
		t.Fatalf("passes-after precondition: the emitted nested-set numbering CTE must carry a depth "+
			"bound (`c._depth < N`); got:\n%s", boundedSQL)
	}
	dbOK := openChDB(t)
	seed(dbOK)
	inner := "(" + stripTrailingSemi(boundedSQL) + ")"
	var total int64
	if err := dbOK.QueryRow("SELECT count() FROM "+inner, boundedArgs...).Scan(&total); err != nil {
		t.Fatalf("passes-after FAILED: the bounded nested-set query errored on a table containing a "+
			"cyclic trace — a single malformed trace must not 500 the structure tab: %v", err)
	}
	// The acyclic trace's spans MUST be numbered non-zero — the bound must
	// not have truncated a real, sub-cap numbering. nestedSetLeft > 0 holds
	// for every span reachable from a root, so the acyclic trace's three
	// spans all qualify.
	const acyclicTrace = "a0000000000000000000000000000001"
	var acyclicNumbered int64
	if err := dbOK.QueryRow(
		"SELECT count() FROM "+inner+" WHERE TraceId = '"+acyclicTrace+"' AND nestedSetLeft > 0",
		boundedArgs...,
	).Scan(&acyclicNumbered); err != nil {
		t.Fatalf("passes-after: acyclic-numbering probe errored: %v", err)
	}
	if acyclicNumbered < 3 {
		t.Errorf("passes-after: the acyclic trace's spans are under-numbered in the bounded result "+
			"(spans with nestedSetLeft > 0 = %d, want 3) — the depth cap must not truncate a real, "+
			"sub-cap numbering.", acyclicNumbered)
	}
	t.Logf("passes-after OK: bounded nested-set query returned %d total row(s) (acyclic-trace numbered "+
		"spans=%d) with no error on the cyclic table.", total, acyclicNumbered)

	// --- fails-before: the unbounded numbering shape errors on the cyclic
	// seed. Separate connection: this query deliberately errors 306, which we
	// must not let bleed into the passes-after run above.
	dbErr := openChDB(t)
	seed(dbErr)
	var sink int64
	errUnbounded := dbErr.QueryRow("SELECT count() FROM (" + unboundedNestedSetNumberingSQL() + ")").Scan(&sink)
	if errUnbounded == nil {
		t.Fatalf("fails-before precondition not reproduced: the UNBOUNDED numbering walk must error on a "+
			"cyclic trace (CH code 306), but it succeeded (count=%d). The cycle-guard regression is "+
			"mis-seeded — the seed trace is not actually cyclic.", sink)
	}
	if !strings.Contains(errUnbounded.Error(), "306") &&
		!strings.Contains(strings.ToLower(errUnbounded.Error()), "recursi") {
		t.Fatalf("fails-before: expected a CH recursion-depth error (code 306) from the unbounded "+
			"numbering walk on the cyclic trace, got a different error: %v", errUnbounded)
	}
	t.Logf("fails-before OK: unbounded nested-set numbering errors on cyclic trace: %v", errUnbounded)
}
