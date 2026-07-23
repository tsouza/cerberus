//go:build chdb

// Regression guard for the TraceQL spanset-union (`A || B`) memory blowup
// (task #102 / showcase-traceql "Spanset && / ||" panel never loading).
//
// # The bug
//
// `||` (chplan.SetUnion) used to emit `(<left>) UNION DISTINCT (<right>)`
// where each arm is `SELECT * FROM otel_traces WHERE …` — so ClickHouse
// deduped on the FULL row tuple, hashing every (wide) column INCLUDING the
// nested Events.* / Links.* array columns the OTel-CH schema carries. At
// self-telemetry scale (~150k+ spans) the dedup hash table over wide rows
// with array columns blew memory / wall time — the showcase "Spanset ||"
// panel `{ kind = producer } || { kind = consumer }` never loaded. The same
// wide-row UNION DISTINCT sat at the top of the Drilldown structure-tab
// query `(... &>>) || (...)`.
//
// # The fix (chsql.emitSetOperation, SetUnion case)
//
// `SELECT * FROM (<left> UNION ALL <right>) LIMIT 1 BY (TraceId, SpanId)` —
// dedup on SPAN IDENTITY, not the full row. Both arms read the same spans
// table, so a span surfaced by both predicates is byte-identical; identity
// dedup is therefore RESULT-identical to the old full-row UNION DISTINCT
// (pinned by the test/spec/traceql/*_union*.txtar `-- expected_rows --`
// chDB round-trips) but streams cheaply via CH's `LIMIT n BY` — O(rows × 2
// ids) instead of O(rows × full-row-width).
//
// # This guard
//
//	PRONG 1 (correctness): on an OVERLAPPING-arms seed (every span
//	satisfies BOTH predicates) the cerberus-emitted `||` returns EXACTLY the
//	distinct span count — identity dedup must collapse the duplicate just
//	like UNION DISTINCT did, not double-count via UNION ALL.
//
//	PRONG 2 (cost): on a wide-row corpus (the nested Events/Links arrays) at
//	scale, the cerberus-emitted `||` must cost a FRACTION of the full-row
//	UNION DISTINCT it replaced — i.e. it must NOT carry the wide-row-dedup
//	tax. The yardstick is the regression shape itself (full-row UNION
//	DISTINCT over the same two arms), not a bare UNION ALL: both the emit and
//	the yardstick do real materialize+dedup work, so the ratio is stable
//	across ClickHouse versions and runners. (An earlier yardstick — ratio vs
//	a `count()` over bare UNION ALL — was fragile: CH answers that count
//	WITHOUT materializing the wide array columns, so the denominator is a
//	near-free ~5ms and the ratio swung with server version even though the
//	emit was byte-identical. It broke on the 25.8→26.5 substrate bump with no
//	code change. Measured on 26.5: cerberus `||` ~= 0.23x the full-row UNION
//	DISTINCT cost; a regression back to full-row dedup makes the two queries
//	identical, ratio ~= 1.0x.)
//
// Build-tagged chdb; rides the perf-guards job.
package perf

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	traceqlast "github.com/tsouza/cerberus/internal/traceql/ast"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
)

// setUnionWideDDL mirrors the real OTel-CH traces schema INCLUDING the
// nested Events.* / Links.* array columns — the columns the pre-fix
// full-row UNION DISTINCT had to hash.
const setUnionWideDDL = `CREATE TABLE otel_traces (
    TraceId String, SpanId String, ParentSpanId String, SpanName String,
    SpanKind String, Duration Int64, Timestamp DateTime64(9),
    StatusCode String, StatusMessage String, ScopeName String, ScopeVersion String,
    SpanAttributes Map(String,String), ResourceAttributes Map(String,String),
    "Events.Timestamp" Array(DateTime64(9)), "Events.Name" Array(String), "Events.Attributes" Array(Map(String,String)),
    "Links.TraceId" Array(String), "Links.SpanId" Array(String), "Links.TraceState" Array(String), "Links.Attributes" Array(Map(String,String))
) ENGINE = MergeTree() ORDER BY (Timestamp);`

// setUnionMaxFractionOfUnionDistinct is the max (cerberus `||` wall /
// full-row UNION DISTINCT wall) ratio PRONG 2 tolerates. Identity dedup via
// LIMIT 1 BY does a fraction of the wide-row dedup work: measured ~0.23x on
// the 26.5 substrate. A regression back to full-row dedup makes the emit
// identical to the yardstick (ratio ~= 1.0x), so a bound comfortably below 1
// catches it while leaving generous headroom for runner noise.
const setUnionMaxFractionOfUnionDistinct = 0.6

const setUnionScaleRows = 400_000

func openSetUnionDB(t *testing.T) *sql.DB {
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

func setUnionExec(t *testing.T, db *sql.DB, s string) {
	t.Helper()
	if _, err := db.Exec(s); err != nil {
		t.Fatalf("exec: %v\n%s", err, s)
	}
}

// emitSpansetUnionSQL lowers `{ kind = producer } || { kind = consumer }`
// through the real chain (the showcase "Spanset ||" panel query).
func emitSpansetUnionSQL(t *testing.T, ql string) (string, []any) {
	t.Helper()
	expr, err := traceqlast.Parse(ql)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	plan, err := traceql.Lower(context.Background(), expr, schema.DefaultOTelTraces())
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	sqlText, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	return sqlText, args
}

func TestSetUnion_IdentityDedup_Correctness(t *testing.T) {
	db := openSetUnionDB(t)
	setUnionExec(t, db, "CREATE DATABASE IF NOT EXISTS default")
	setUnionExec(t, db, "DROP TABLE IF EXISTS otel_traces")
	setUnionExec(t, db, setUnionWideDDL)
	// 7 spans, all StatusCode='Error' AND Duration>1500 → every span
	// satisfies BOTH arms of `{ status = error } || { duration > 1500ns }`.
	const nOverlap = 7
	setUnionExec(t, db, fmt.Sprintf(`INSERT INTO otel_traces
      (TraceId, SpanId, ParentSpanId, SpanName, SpanKind, Duration, Timestamp, StatusCode)
    SELECT leftPad(hex(0),32,'0'), leftPad(hex(number),16,'0'), '', 'sp', 'Client',
           toInt64(2000), toDateTime64('2026-05-01 10:00:00',9)+toIntervalNanosecond(number), 'Error'
    FROM numbers(%d)`, nOverlap))

	sqlText, args := emitSpansetUnionSQL(t, `{ status = error } || { duration > 1500 }`)
	var got int64
	if err := db.QueryRow("SELECT count() FROM ("+stripTrailingSemi(sqlText)+")", args...).Scan(&got); err != nil {
		t.Fatalf("union query failed: %v\nSQL: %s", err, sqlText)
	}
	if got != nOverlap {
		t.Errorf("overlapping-arms `||` returned %d rows, want %d (the distinct span count). "+
			"Identity dedup must collapse the in-both-arms duplicate exactly like the old full-row "+
			"UNION DISTINCT — a count of %d means the fix regressed to UNION ALL (double-counting).",
			got, nOverlap, nOverlap*2)
	}
}

func TestSetUnion_IdentityDedup_Cost(t *testing.T) {
	db := openSetUnionDB(t)
	setUnionExec(t, db, "CREATE DATABASE IF NOT EXISTS default")
	setUnionExec(t, db, "DROP TABLE IF EXISTS otel_traces")
	setUnionExec(t, db, setUnionWideDDL)
	setUnionExec(t, db, fmt.Sprintf(`INSERT INTO otel_traces
      (TraceId, SpanId, ParentSpanId, SpanName, SpanKind, Duration, Timestamp,
       StatusCode, StatusMessage, ScopeName, ScopeVersion, SpanAttributes, ResourceAttributes,
       "Events.Timestamp","Events.Name","Events.Attributes","Links.TraceId","Links.SpanId","Links.TraceState","Links.Attributes")
    SELECT leftPad(hex(intDiv(number,10)),32,'0'), leftPad(hex(number),16,'0'), '', concat('span',toString(number)),
      if(number %% 2 = 0,'Producer','Consumer'), toInt64(1000+number %% 1000),
      toDateTime64('2026-05-01 10:00:00',9)+toIntervalNanosecond(number),
      'Unset','','scope','1.0', map('k',toString(number %% 100)), map('service.name',concat('svc',toString(number %% 5))),
      [toDateTime64('2026-05-01 10:00:00',9)],['exception'],[map('exception.message','boom')], [],[],[],[]
    FROM numbers(%d)`, setUnionScaleRows))

	sqlText, args := emitSpansetUnionSQL(t, `{ kind = producer } || { kind = consumer }`)

	// Full-row UNION DISTINCT over the same two arms — the regression shape
	// this guard exists to reject (dedup on every wide column INCLUDING the
	// nested Events/Links arrays). The cerberus emit must cost a fraction of
	// it. Both do real materialize+dedup work, so the ratio is stable across
	// ClickHouse versions — unlike a bare-UNION-ALL count, whose wide columns
	// CH never materializes.
	unionDistinct := `(SELECT * FROM otel_traces WHERE SpanKind = 'Producer') UNION DISTINCT (SELECT * FROM otel_traces WHERE SpanKind = 'Consumer')`

	best := func(q string, a []any) time.Duration {
		// warm-up
		var sink int64
		if err := db.QueryRow("SELECT count() FROM ("+q+")", a...).Scan(&sink); err != nil {
			t.Fatalf("query failed: %v\nSQL: %s", err, q)
		}
		b := time.Hour
		for i := 0; i < 5; i++ {
			s := time.Now()
			_ = db.QueryRow("SELECT count() FROM ("+q+")", a...).Scan(&sink)
			if d := time.Since(s); d < b {
				b = d
			}
		}
		return b
	}

	emitWall := best(stripTrailingSemi(sqlText), args)
	distinctWall := best(unionDistinct, nil)
	ratio := float64(emitWall) / float64(distinctWall)
	t.Logf("cerberus `||` wall=%s / full-row UNION DISTINCT wall=%s = %.2fx (bound %.2fx)",
		emitWall.Round(time.Microsecond), distinctWall.Round(time.Microsecond), ratio, setUnionMaxFractionOfUnionDistinct)
	if ratio > setUnionMaxFractionOfUnionDistinct {
		t.Errorf("cerberus `||` is %.2fx the cost of the full-row UNION DISTINCT it replaced, over the "+
			"committed %.2fx bound. The spanset-union emit is carrying a wide-row-dedup tax again — "+
			"it must dedup on (TraceId, SpanId) via `LIMIT 1 BY`, not on the full row tuple "+
			"(UNION DISTINCT). See chsql.emitSetOperation.", ratio, setUnionMaxFractionOfUnionDistinct)
	}
}
