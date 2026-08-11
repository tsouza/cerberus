//go:build chdb

// Regression guard for the TraceQL spanset-intersect (`A && B`) leaf-pass
// blowup (issue #1519).
//
// # The cost
//
// `&&` names each arm as a non-recursive `WITH _setand_{l,r}_<n>` CTE and
// references it three times — its UNION-ALL leg plus both trace-cohort
// `TraceId IN (…)` subqueries. A non-recursive CTE is a TEXTUAL
// substitution, not a materialisation: ClickHouse re-evaluates every use
// site, so a two-arm `&&` over bare selectors reads `otel_traces` — the
// largest table cerberus touches — FOUR times, and `A && B && C` reads it
// SEVEN times, all for a result one pass can produce.
//
// # The fix (chsql.fusableIntersect / emitFusedIntersect)
//
// Where every arm is a Filter chain over one shared Scan with a pure row
// predicate, the whole chain collapses to
//
//	SELECT * FROM otel_traces
//	WHERE <shared conjuncts> AND (<r1> OR <r2> …)
//	QUALIFY max(<r1>) OVER (PARTITION BY TraceId) AND max(<r2>) OVER (…)
//	LIMIT 1 BY TraceId, SpanId
//
// — one leaf pass. QUALIFY filters on the window value without projecting
// it, so the statement keeps the bare `SELECT *` column set.
//
// # This guard
//
//	PRONG 1 (correctness): the emitted `&&` returns exactly the span set
//	Tempo's semantics demand — the identity-deduped span-level UNION of both
//	arms, restricted to traces where BOTH arms matched at least one span.
//	The oracle is the union-gated shape this change replaces, written out
//	literally, so the two independent formulations must agree on a seed
//	built to separate them (a trace matching only one arm, and a trace whose
//	two arms match DIFFERENT spans — the case a span-level intersection
//	would wrongly return empty for).
//
//	PRONG 2 (cost): the fusable shapes must read exactly the seed's row
//	count out of otel_traces — one pass over the table — two-arm and
//	three-arm alike.
//
//	PRONG 3 (the decision boundary, pinned the other way): the sub-pipeline
//	shape `({…} | count() > 0) && ({…} | count() > 0)` must STILL read the
//	table more than once, because its arms' membership is a per-trace
//	aggregate no per-row `max(<p>) OVER (…)` can reproduce. Without this
//	prong the guard would rubber-stamp an emitter that fused everything
//	unconditionally — which is a correctness bug, not merely a slower one.
//	Sensitivity has to be pinned in BOTH directions or the pin is decorative.
//
// Build-tagged chdb; rides the perf-guards job.
package perf

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
	traceqlast "github.com/tsouza/cerberus/internal/traceql/ast"
)

// setIntersectDDL is the minimal spans shape the `&&` gate reads: the
// identity key it dedups on plus the two attribute sources the arms
// select over. MergeTree (not Memory) because PREWHERE promotion and the
// EXPLAIN leaf-read node names are MergeTree-specific.
const setIntersectDDL = `CREATE TABLE otel_traces (
    TraceId String, SpanId String, ParentSpanId String, SpanName String,
    SpanKind String, Duration Int64, Timestamp DateTime64(9),
    StatusCode String, StatusMessage String, ScopeName String, ScopeVersion String,
    SpanAttributes Map(String,String), ResourceAttributes Map(String,String)
) ENGINE = MergeTree() ORDER BY (TraceId, SpanId);`

// setIntersectSeed separates the shapes rather than merely exercising them.
//
//	t1 — arm A matches s1 only, arm B matches s2 only. `&&` must return
//	     BOTH spans: it is a TRACE-level conjunction over a SPAN-level
//	     union, so a span-level intersection (which returns nothing here)
//	     is the wrong answer.
//	t2 — arm A matches, arm B does not. The whole trace must drop.
//	t3 — arm B matches, arm A does not. The whole trace must drop.
const setIntersectSeed = `INSERT INTO otel_traces
    (TraceId, SpanId, ParentSpanId, SpanName, SpanKind, Duration, Timestamp, StatusCode, ResourceAttributes)
VALUES
    ('t1','s1','','sp','Client', 100, toDateTime64('2026-05-01 10:00:00',9), 'Ok', map('service.name','a')),
    ('t1','s2','','sp','Client',9000, toDateTime64('2026-05-01 10:00:01',9), 'Ok', map('service.name','z')),
    ('t2','s3','','sp','Client', 100, toDateTime64('2026-05-01 10:00:02',9), 'Ok', map('service.name','a')),
    ('t3','s4','','sp','Client',9000, toDateTime64('2026-05-01 10:00:03',9), 'Ok', map('service.name','z'));`

// setIntersectQL is the two-arm bare-selector query issue #1519 measured:
// arm A selects on a resource attribute, arm B on duration.
const setIntersectQL = `{ resource.service.name = "a" } && { duration > 1000 }`

// setIntersectQL3 is the same shape one arm deeper. `A && B && C` arrives
// left-nested and must flatten into the SAME single pass, not nest a
// second union-gated statement inside the first.
const setIntersectQL3 = `{ resource.service.name = "a" } && { duration > 1000 } && { status = ok }`

// setIntersectPipelineQL is the shape that must NOT fuse: both operands are
// parenthesised sub-pipelines ending in a spanset aggregate.
const setIntersectPipelineQL = `({ resource.service.name = "a" } | count() > 0) && ({ duration > 1000 } | count() > 0)`

// unionGatedOracle is the shape emitFusedIntersect replaces, written out
// by hand as an independent formulation of the same Tempo semantics: both
// arms' rows unioned, deduped on span identity, gated on the trace
// appearing in BOTH arms.
const unionGatedOracle = "WITH " +
	"l AS (SELECT * FROM otel_traces WHERE ResourceAttributes['service.name'] = 'a'), " +
	"r AS (SELECT * FROM otel_traces WHERE Duration > 1000) " +
	"SELECT TraceId, SpanId FROM ((SELECT * FROM l) UNION ALL (SELECT * FROM r)) " +
	"WHERE TraceId IN (SELECT TraceId FROM l) AND TraceId IN (SELECT TraceId FROM r) " +
	"LIMIT 1 BY TraceId, SpanId"

// setIntersectSeedRows is the number of spans setIntersectSeed inserts.
// The fused shape must read exactly this many rows out of otel_traces —
// one pass over the table — whatever the arm count.
const setIntersectSeedRows = 4

func openSetIntersectDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	return db
}

func setIntersectExec(t *testing.T, db *sql.DB, s string) {
	t.Helper()
	if _, err := db.Exec(s); err != nil {
		t.Fatalf("exec: %v\n%s", err, s)
	}
}

func setIntersectSeedDB(t *testing.T, db *sql.DB) {
	t.Helper()
	setIntersectExec(t, db, "CREATE DATABASE IF NOT EXISTS default")
	setIntersectExec(t, db, "DROP TABLE IF EXISTS otel_traces")
	setIntersectExec(t, db, setIntersectDDL)
	setIntersectExec(t, db, setIntersectSeed)
}

// emitTraceQL lowers ql through the real parse -> lower -> emit chain, so
// the SQL under measurement is the SQL cerberus ships.
func emitTraceQL(t *testing.T, ql string) (string, []any) {
	t.Helper()
	expr, err := traceqlast.Parse(ql)
	if err != nil {
		t.Fatalf("parse %q: %v", ql, err)
	}
	plan, err := traceql.Lower(context.Background(), expr, schema.DefaultOTelTraces())
	if err != nil {
		t.Fatalf("lower %q: %v", ql, err)
	}
	sqlText, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("emit %q: %v", ql, err)
	}
	return stripTrailingSemi(sqlText), args
}

// rowsReadFromSpansTable returns ClickHouse's own estimate of how many
// rows the statement reads out of otel_traces, summed across every access
// of the table. Divided by the seed's row count it IS the scan
// amplification: one pass reads the table once, the union-gated shape
// reads it once per use site.
//
// Rows read, not plan nodes. Node-counting looked equivalent and is not:
// on MergeTree, `EXPLAIN PLAN` renders the `CreatingSets` step for the
// two `TraceId IN (…)` cohort subqueries WITHOUT its child plans, so the
// two set-building reads are invisible and a node count reports 2 for a
// shape that genuinely reads the table 4 times (the Memory engine, whose
// EXPLAIN does expand them, shows all 4). EXPLAIN ESTIMATE inherits the
// same blind spot, so the FALLBACK figure here is a LOWER BOUND on its
// true cost — which is harmless, because every assertion below wants the
// fallback to be bigger and the fused shape to be exactly one pass.
func rowsReadFromSpansTable(t *testing.T, db *sql.DB, sqlText string, args []any) int64 {
	t.Helper()
	rows, err := db.Query("EXPLAIN ESTIMATE "+sqlText, args...)
	if err != nil {
		t.Fatalf("EXPLAIN ESTIMATE failed: %v\nSQL: %s", err, sqlText)
	}
	defer func() { _ = rows.Close() }()
	var total int64
	seen := 0
	for rows.Next() {
		var database, table string
		var parts, estRows, marks int64
		if err := rows.Scan(&database, &table, &parts, &estRows, &marks); err != nil {
			t.Fatalf("scan EXPLAIN ESTIMATE row: %v", err)
		}
		if table != "otel_traces" {
			continue
		}
		seen++
		total += estRows
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("EXPLAIN ESTIMATE rows: %v", err)
	}
	if seen == 0 {
		t.Fatalf("EXPLAIN ESTIMATE reported no read of otel_traces — the plan shape changed "+
			"and this guard is no longer measuring scan amplification.\nSQL: %s", sqlText)
	}
	t.Logf("rows read from otel_traces = %d (seed holds %d) for\n  SQL: %s",
		total, setIntersectSeedRows, sqlText)
	return total
}

func spanIdentities(t *testing.T, db *sql.DB, sqlText string, args []any) []string {
	t.Helper()
	rows, err := db.Query("SELECT TraceId, SpanId FROM ("+sqlText+") ORDER BY TraceId, SpanId", args...)
	if err != nil {
		t.Fatalf("query failed: %v\nSQL: %s", err, sqlText)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var tid, sid string
		if err := rows.Scan(&tid, &sid); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, tid+"/"+sid)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// TestSetIntersect_SinglePass_MatchesUnionGatedSemantics is PRONG 1: the
// fused single-pass gate and the union-gated shape it replaces must select
// the same spans, on a seed built to catch the ways a rewrite of `&&` goes
// wrong (dropping the trace whose arms matched different spans, or keeping
// a trace only one arm matched).
func TestSetIntersect_SinglePass_MatchesUnionGatedSemantics(t *testing.T) {
	db := openSetIntersectDB(t)
	setIntersectSeedDB(t, db)

	sqlText, args := emitTraceQL(t, setIntersectQL)
	got := spanIdentities(t, db, sqlText, args)

	// Wrapped rather than suffixed: ORDER BY cannot follow `LIMIT n BY`.
	oracleRows, err := db.Query("SELECT TraceId, SpanId FROM (" + unionGatedOracle + ") ORDER BY TraceId, SpanId")
	if err != nil {
		t.Fatalf("oracle query failed: %v", err)
	}
	defer func() { _ = oracleRows.Close() }()
	var want []string
	for oracleRows.Next() {
		var tid, sid string
		if err := oracleRows.Scan(&tid, &sid); err != nil {
			t.Fatalf("oracle scan: %v", err)
		}
		want = append(want, tid+"/"+sid)
	}

	// t1's two spans and nothing else: arm A matched s1, arm B matched s2,
	// so the trace passes the gate and contributes BOTH spans.
	if strings.Join(want, ",") != "t1/s1,t1/s2" {
		t.Fatalf("oracle itself returned %v, want [t1/s1 t1/s2] — the seed no longer "+
			"separates the shapes and PRONG 1 would pass vacuously", want)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("single-pass `&&` selected %v; the union-gated shape it replaces selects %v.\nSQL: %s",
			got, want, sqlText)
	}
}

// TestSetIntersect_SinglePass_LeafReads is PRONG 2 + PRONG 3: the fusable
// shapes cost one pass, and the sub-pipeline shape — which must keep the
// costlier but correct union-gated form — still costs more than one.
func TestSetIntersect_SinglePass_LeafReads(t *testing.T) {
	db := openSetIntersectDB(t)
	setIntersectSeedDB(t, db)

	t.Run("two-arm bare selectors fuse to one pass", func(t *testing.T) {
		sqlText, args := emitTraceQL(t, setIntersectQL)
		if !strings.Contains(sqlText, "QUALIFY") {
			t.Fatalf("`&&` over two bare selectors did not emit the single-pass QUALIFY "+
				"gate at all — the rows-read assertion below would be measuring the wrong "+
				"shape.\nSQL: %s", sqlText)
		}
		if n := rowsReadFromSpansTable(t, db, sqlText, args); n != setIntersectSeedRows {
			t.Errorf("`&&` over two bare selectors reads %d rows of otel_traces, want %d "+
				"(exactly one pass over the %d-row seed). The union-gated CTE shape reads it "+
				"at least twice — anything above %d means the single-pass QUALIFY gate stopped "+
				"firing (see chsql.fusableIntersect).\nSQL: %s",
				n, setIntersectSeedRows, setIntersectSeedRows, setIntersectSeedRows, sqlText)
		}
	})

	t.Run("three-arm chain flattens to one pass", func(t *testing.T) {
		sqlText, args := emitTraceQL(t, setIntersectQL3)
		if n := rowsReadFromSpansTable(t, db, sqlText, args); n != setIntersectSeedRows {
			t.Errorf("`A && B && C` reads %d rows of otel_traces, want %d (one pass over the "+
				"%d-row seed). The nested union-gated shape reads it once per arm — anything "+
				"above %d means the chain stopped flattening.\nSQL: %s",
				n, setIntersectSeedRows, setIntersectSeedRows, setIntersectSeedRows, sqlText)
		}
	})

	// The boundary, pinned the other way. This subtest FAILING is the
	// signal that the emitter began fusing sub-pipeline arms — which
	// returns wrong rows, since a per-trace aggregate is not a row
	// predicate. It is deliberately an assertion about the shape being
	// MORE expensive, and that is the correct expectation here.
	t.Run("sub-pipeline arms keep the union-gated shape", func(t *testing.T) {
		sqlText, args := emitTraceQL(t, setIntersectPipelineQL)
		if !strings.Contains(sqlText, "_setand_l_") {
			t.Errorf("sub-pipeline `&&` no longer emits the union-gated arm CTEs. Its arms' "+
				"membership is a per-trace aggregate, which no `max(<p>) OVER (PARTITION BY "+
				"TraceId)` can reproduce, so fusing it returns WRONG rows.\nSQL: %s", sqlText)
		}
		if strings.Contains(sqlText, "QUALIFY") {
			t.Errorf("sub-pipeline `&&` emitted the single-pass QUALIFY gate; see above — "+
				"this shape must not fuse.\nSQL: %s", sqlText)
		}
		if n := rowsReadFromSpansTable(t, db, sqlText, args); n <= setIntersectSeedRows {
			t.Errorf("sub-pipeline `&&` reads %d rows of otel_traces; want more than %d. "+
				"A single pass here means the shape fused after all.\nSQL: %s",
				n, setIntersectSeedRows, sqlText)
		}
	})
}
