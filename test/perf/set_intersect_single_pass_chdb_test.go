//go:build chdb

// Regression guard for the TraceQL spanset-intersect (`A && B`) leaf-pass
// blowup (issues #1519 and #1525).
//
// # The cost
//
// `&&` used to name each arm as a non-recursive `WITH _setand_{l,r}_<n>`
// CTE and reference it twice — its UNION-ALL leg and its own trace-cohort
// `TraceId IN (…)` subquery. A non-recursive CTE is a TEXTUAL
// substitution, not a materialisation: ClickHouse re-evaluates every use
// site, so a two-arm `&&` read `otel_traces` — the largest table cerberus
// touches — FOUR times for a result one pass can produce.
//
// Two changes closed that, and this file guards both. #1519 fused the
// common bare-selector case into a single pass. #1525 fixed the FALLBACK
// the fused path deliberately declines (sub-pipeline arms, nested `||`,
// BoundedTraceScope, arms over different Scans): it still named each arm
// and referenced it twice, so it still evaluated every arm twice. It now
// tags each UNION-ALL leg with the arm that produced it and reads both
// cohorts off that one union, which is one evaluation per arm.
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
//	PRONG 4 (#1525, correctness of the FALLBACK): the union-tagged fallback
//	and the CTE-gated shape it replaces must select the same spans, oracle
//	against oracle, including the edge where one arm's cohort is globally
//	empty and the whole result must vanish.
//
//	PRONG 5 (#1525, cost of the FALLBACK): neither shape may read an arm
//	twice. This one counts leaf reads off EXPLAIN PLAN on a MEMORY-engine
//	table rather than EXPLAIN ESTIMATE on MergeTree, because MergeTree's
//	EXPLAIN hides the cohort gates' reads inside an unexpanded
//	`CreatingSets` step — it reports the two-evaluations shape and the
//	one-evaluation shape as identical, so an assertion built on it could
//	not fail for the regression it exists to catch.
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

// setIntersectEmptyCohortQL is the fallback shape with one arm whose
// cohort is GLOBALLY empty. `len(rhs) > 0` fails for every trace, so the
// whole result must be empty — the edge the arm-marker gate has to get
// right, and the one a gate that defaulted to "present" would pass.
const setIntersectEmptyCohortQL = `({ resource.service.name = "a" } | count() > 0) && ` +
	`({ resource.service.name = "no-such-service" } | count() > 0)`

// setOpFallbackMarker is the column the union-tagged fallback stamps on
// each UNION-ALL leg (chsql's intersectSideCol). Its presence is what
// distinguishes the fallback from the fused single-pass shape, now that
// BOTH read their trace gate through QUALIFY.
const setOpFallbackMarker = "_setand_side"

// setIntersectSeedRows is the number of spans setIntersectSeed inserts.
// The fused shape must read exactly this many rows out of otel_traces —
// one pass over the table — whatever the arm count.
const setIntersectSeedRows = 4

// setIntersectMemDDL is setIntersectDDL on the Memory engine.
//
// It exists because the MergeTree `EXPLAIN` blind spot documented on
// rowsReadFromSpansTable makes that measurement unable to FAIL for the
// regression #1525 fixed: a shape that re-derives each cohort from its own
// arm hides those reads inside a `CreatingSets` step whose children
// MergeTree's EXPLAIN does not render, so its estimate is identical to the
// single-reference shape's. Memory's EXPLAIN expands them, so every read of
// the table is visible and countable — which is the only way an assertion
// here can tell "each arm read once" from "each arm read three times".
const setIntersectMemDDL = `CREATE TABLE otel_traces (
    TraceId String, SpanId String, ParentSpanId String, SpanName String,
    SpanKind String, Duration Int64, Timestamp DateTime64(9),
    StatusCode String, StatusMessage String, ScopeName String, ScopeVersion String,
    SpanAttributes Map(String,String), ResourceAttributes Map(String,String)
) ENGINE = Memory;`

// Leaf reads of otel_traces each shape is allowed, counted off EXPLAIN PLAN
// over setIntersectMemDDL.
//
//	fused — one pass, the whole point of #1519.
//	fallback — the sub-pipeline query's two arms, each evaluated ONCE. An
//	  arm here is `Filter(TraceId IN <aggregate>) -> Filter(<sel>) -> Scan`,
//	  so ONE evaluation of it is TWO reads: its own row scan plus its
//	  aggregate's. Two arms, evaluated once each, is 4. Before #1525 the
//	  fallback named each arm in a relational `WITH` and referenced it
//	  twice — its union leg and its own cohort gate — and since ClickHouse
//	  inlines a relational WITH at every reference, that measured 8.
const (
	setIntersectFusedLeafReads    = 1
	setIntersectFallbackLeafReads = 4
)

// leafReadsOfSpansTable counts how many times sqlText reads otel_traces,
// off EXPLAIN PLAN against a Memory-engine table. Unlike
// rowsReadFromSpansTable this is an exact reference count, not an estimate.
func leafReadsOfSpansTable(t *testing.T, db *sql.DB, sqlText string, args []any) int {
	t.Helper()
	rows, err := db.Query("EXPLAIN PLAN "+sqlText, args...)
	if err != nil {
		t.Fatalf("EXPLAIN PLAN failed: %v\nSQL: %s", err, sqlText)
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan EXPLAIN PLAN row: %v", err)
		}
		if strings.Contains(line, "ReadFromMemoryStorage") || strings.Contains(line, "ReadFromStorage") {
			n++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("EXPLAIN PLAN rows: %v", err)
	}
	if n == 0 {
		t.Fatalf("EXPLAIN PLAN reported no read of otel_traces — the plan shape changed and "+
			"this guard is no longer counting leaf reads.\nSQL: %s", sqlText)
	}
	return n
}

// TestSetIntersect_ArmsAreReadOnce pins the fix for #1525: neither `&&`
// shape reads an arm more than once.
//
// The fallback used to name each arm in a non-recursive `WITH` and
// reference it twice, on the assumption that naming a relation
// materialises it. ClickHouse INLINES a relational `WITH` at every
// reference, so that cost two evaluations of every arm; the cohorts are
// now read off the union the shape already scans. Measured on chDB over a
// 2M-span corpus, the same three formulations of the two-arm shape:
//
//	shape                      leaf reads   min-of-9 wall
//	CTE-named arms (before)         4          493 ms
//	arms written out in full        4          460 ms
//	union-tagged (after)            2          411 ms
//
// The first two rows ARE the issue: naming an arm and splicing it in full
// cost exactly the same, because the name was never a materialisation.
func TestSetIntersect_ArmsAreReadOnce(t *testing.T) {
	db := openSetIntersectDB(t)
	setIntersectExec(t, db, "CREATE DATABASE IF NOT EXISTS default")
	setIntersectExec(t, db, "DROP TABLE IF EXISTS otel_traces")
	setIntersectExec(t, db, setIntersectMemDDL)
	setIntersectExec(t, db, setIntersectSeed)

	t.Run("fused shape reads the table once", func(t *testing.T) {
		sqlText, args := emitTraceQL(t, setIntersectQL)
		if n := leafReadsOfSpansTable(t, db, sqlText, args); n != setIntersectFusedLeafReads {
			t.Errorf("the fused `&&` reads otel_traces %d times, want %d.\nSQL: %s",
				n, setIntersectFusedLeafReads, sqlText)
		}
	})

	t.Run("fallback reads each arm exactly once", func(t *testing.T) {
		sqlText, args := emitTraceQL(t, setIntersectPipelineQL)
		if !strings.Contains(sqlText, setOpFallbackMarker) {
			t.Fatalf("the sub-pipeline `&&` fused, so this subtest is not measuring the "+
				"fallback.\nSQL: %s", sqlText)
		}
		n := leafReadsOfSpansTable(t, db, sqlText, args)
		if n != setIntersectFallbackLeafReads {
			t.Errorf("the `&&` fallback reads otel_traces %d times, want %d — one evaluation of "+
				"each of the two arms. %d means an arm is being re-derived: the cohort gates must "+
				"read off the tagged union, never from a second reference to the arm, because a "+
				"relational `WITH` names a relation without materialising it (#1525).\nSQL: %s",
				n, setIntersectFallbackLeafReads, n, sqlText)
		}
	})
}

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

// TestSetIntersect_UnionTagged_MatchesCTEGatedSemantics is PRONG 1 for the
// FALLBACK shape (issue #1525).
//
// The shapes fusableIntersect declines still had each arm referenced three
// times behind a relational `WITH` — a name ClickHouse inlines, so three
// references meant three evaluations. The fallback now tags each UNION-ALL
// leg with the arm that produced it and reads both cohorts off that one
// union, so each arm is evaluated ONCE. That is a shape change, not a
// rename, so it needs the same independent-formulation check the fused path
// got: the oracle is the CTE-gated shape written out by hand.
//
// `| count() > 0` over a selector's own spanset is a semantic no-op — it
// keeps every trace with at least one matching span, which is every trace
// the selector produced a row for — so unionGatedOracle is the oracle for
// the sub-pipeline query too, and the two formulations must agree on the
// same seed that separates trace-level from span-level conjunction.
func TestSetIntersect_UnionTagged_MatchesCTEGatedSemantics(t *testing.T) {
	db := openSetIntersectDB(t)
	setIntersectSeedDB(t, db)

	sqlText, args := emitTraceQL(t, setIntersectPipelineQL)
	if !strings.Contains(sqlText, setOpFallbackMarker) {
		t.Fatalf("the sub-pipeline `&&` fused after all, so this test is measuring the fused "+
			"shape and not the fallback it claims to check.\nSQL: %s", sqlText)
	}
	got := spanIdentities(t, db, sqlText, args)

	oracle := spanIdentities(t, db, unionGatedOracle, nil)
	// t1's two spans and nothing else — see setIntersectSeed.
	if strings.Join(oracle, ",") != "t1/s1,t1/s2" {
		t.Fatalf("oracle itself returned %v, want [t1/s1 t1/s2] — the seed no longer separates "+
			"the shapes and this check would pass vacuously", oracle)
	}
	if strings.Join(got, ",") != strings.Join(oracle, ",") {
		t.Errorf("the union-tagged `&&` fallback selected %v; the CTE-gated shape it replaces "+
			"selects %v.\nSQL: %s", got, oracle, sqlText)
	}

	// The empty-cohort edge, asserted separately because it is the one the
	// arm-marker gate can get wrong on its own: `max(_setand_side = 1)` is 0
	// for every partition, so QUALIFY must drop everything. A gate that read
	// "no rows from this arm" as "unconstrained" would return t1 and t2 here.
	t.Run("an arm with an empty cohort empties the result", func(t *testing.T) {
		emptySQL, emptyArgs := emitTraceQL(t, setIntersectEmptyCohortQL)
		if n := spanIdentities(t, db, emptySQL, emptyArgs); len(n) != 0 {
			t.Errorf("`&&` with a globally-empty right cohort returned %v, want no rows: no trace "+
				"can satisfy Tempo's `len(rhs) > 0`.\nSQL: %s", n, emptySQL)
		}
	})
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
	t.Run("sub-pipeline arms keep the union-tagged fallback", func(t *testing.T) {
		sqlText, args := emitTraceQL(t, setIntersectPipelineQL)
		// The fallback's discriminator is the arm marker column, NOT the
		// absence of QUALIFY: since #1525 the fallback reads its cohorts
		// off the union with QUALIFY too, so both shapes carry the
		// keyword and only the marker separates them.
		if !strings.Contains(sqlText, setOpFallbackMarker) {
			t.Errorf("sub-pipeline `&&` no longer emits the union-tagged fallback (%q). Its arms' "+
				"membership is a per-trace aggregate, which no `max(<p>) OVER (PARTITION BY "+
				"TraceId)` can reproduce, so fusing it returns WRONG rows.\nSQL: %s",
				setOpFallbackMarker, sqlText)
		}
		if n := rowsReadFromSpansTable(t, db, sqlText, args); n <= setIntersectSeedRows {
			t.Errorf("sub-pipeline `&&` reads %d rows of otel_traces; want more than %d. "+
				"A single pass here means the shape fused after all.\nSQL: %s",
				n, setIntersectSeedRows, sqlText)
		}
	})

	// The fused shape must NOT carry the fallback's marker. Without this
	// the marker assertion above would pass for an emitter that stamped
	// `_setand_side` on everything, and PRONG 3 would stop discriminating.
	t.Run("the fused shape carries no arm marker", func(t *testing.T) {
		sqlText, _ := emitTraceQL(t, setIntersectQL)
		if strings.Contains(sqlText, setOpFallbackMarker) {
			t.Errorf("the fused single-pass shape emitted the fallback's arm marker %q, so the "+
				"marker no longer separates the two shapes and PRONG 3 is decorative.\nSQL: %s",
				setOpFallbackMarker, sqlText)
		}
	})
}
