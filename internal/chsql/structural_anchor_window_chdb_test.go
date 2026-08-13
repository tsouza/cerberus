//go:build chdb

// chDB-backed proof that the structural closure's ANCHOR-arm window bound is
// executable on the shape that could break it.
//
// The anchor conjunct is rendered in the arm's own scope, OUTSIDE the seed
// derived table (`… FROM (<L>) AS _seed WHERE Timestamp >= …`), so `Timestamp`
// has to resolve against whatever the L subquery projects. For a bare
// `{ } >> { }` that is a `SELECT *` over the spans table, but a left-
// associative CHAIN (`A >> B >> C`) nests a closure inside the outer closure's
// seed, and the outer anchor then resolves `Timestamp` against the inner
// join's aliased projection instead. CH 25.8's analyzer is exactly the layer
// that drops `R.*`-introduced columns from outer-scope resolution
// (structuralProjectionFrags documents the same constraint), so "does it still
// resolve?" is a question only execution answers — the emit-level assertions in
// scan_window_emit_test.go cannot.
//
// Gated by `//go:build chdb` so the default lane (CGO off, no libchdb) skips it;
// it runs in the `test-chdb` lane, which covers ./internal/chsql/....
package chsql_test

import (
	"context"
	"testing"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/chsqltest"
	"github.com/tsouza/cerberus/internal/schema"
	tql "github.com/tsouza/cerberus/internal/traceql"
	"github.com/tsouza/cerberus/internal/traceql/ast"
)

// anchorWindowSpansDDL mirrors the OTel-CH spans table the emitter targets,
// partitioned by `toDate(Timestamp)` — the partitioning that makes an
// unwindowed recursive arm a whole-table read in the first place.
const anchorWindowSpansDDL = "CREATE TABLE otel_traces (" +
	"TraceId String, SpanId String, ParentSpanId String, SpanName String, SpanKind String, " +
	"Duration Int64, Timestamp DateTime64(9), StatusCode String, StatusMessage String, " +
	"ScopeName String, ScopeVersion String, SpanAttributes Map(String, String), " +
	"ResourceAttributes Map(String, String)" +
	") ENGINE = MergeTree PARTITION BY toDate(Timestamp) ORDER BY (Timestamp, SpanId)"

// anchorWindowSeed is one three-span trace INSIDE the scanWindowEmitBounds
// window — root (service.name=a) -> mid (http.status_code=500) -> leaf (client)
// — so the chain query matches exactly the leaf span, plus one span of the same
// shape in a DIFFERENT day partition OUTSIDE the window, which the anchor bound
// must exclude.
const anchorWindowSeed = "INSERT INTO otel_traces VALUES " +
	"('t1','s1','','root','Server',1,toDateTime64('2026-06-27 14:45:00',9),'Unset','','','',map(),map('service.name','a'))," +
	"('t1','s2','s1','mid','Server',1,toDateTime64('2026-06-27 14:46:00',9),'Unset','','','',map('http.status_code','500'),map())," +
	"('t1','s3','s2','leaf','Client',1,toDateTime64('2026-06-27 14:47:00',9),'Unset','','','',map(),map())," +
	"('t2','r1','','root','Server',1,toDateTime64('2026-06-20 10:00:00',9),'Unset','','','',map(),map('service.name','a'))," +
	"('t2','r2','r1','mid','Server',1,toDateTime64('2026-06-20 10:01:00',9),'Unset','','','',map('http.status_code','500'),map())," +
	"('t2','r3','r2','leaf','Client',1,toDateTime64('2026-06-20 10:02:00',9),'Unset','','','',map(),map())"

// TestStructuralAnchorWindow_ChainExecutes_ChDB runs a WINDOWED nested
// structural chain end to end. Two failures are in scope and each has its own
// assertion: the anchor arms must carry the window (asserted on the SQL, so a
// shape regression cannot make the execution vacuous), and the statement must
// still return the in-window match — an anchor conjunct that resolved against
// the wrong scope, or bound the wrong nanos, would drop it. The out-of-window
// trace carries the identical chain shape, so an over-wide result is caught
// too.
func TestStructuralAnchorWindow_ChainExecutes_ChDB(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec(anchorWindowSpansDDL); err != nil {
		t.Fatalf("create spans table: %v", err)
	}
	if _, err := db.Exec(anchorWindowSeed); err != nil {
		t.Fatalf("seed spans: %v", err)
	}

	start, end := scanWindowEmitBounds()
	const q = `{ resource.service.name = "a" } >> { span.http.status_code = 500 } >> { kind = client }`
	expr, err := ast.Parse(q)
	if err != nil {
		t.Fatalf("parse %q: %v", q, err)
	}
	s := schema.DefaultOTelTraces()
	ctx := tql.WithSearchTraceLimit(context.Background(), scanWindowEmitLimit)
	ctx = tql.WithSearchWindow(ctx, start, end)
	plan, err := tql.Lower(ctx, expr, s)
	if err != nil {
		t.Fatalf("lower %q: %v", q, err)
	}
	sql, args, err := chsql.Emit(chsql.WithSpansTable(context.Background(), s.SpansTable), plan)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	// The chain emits two closures; both anchor arms must carry the window.
	assertAnchorArmsWindowed(t, sql, 2)

	// count() over the statement rather than a row drain: chdb-go's parquet
	// driver reads the wide span envelope back column-by-column, and the row
	// VALUES are not what this test is about — resolvability and the window are.
	var got int
	if err := db.QueryRow("SELECT count() FROM ("+sql+")", args...).Scan(&got); err != nil {
		t.Fatalf("execute windowed structural chain: %v\nSQL: %s", err, sql)
	}
	const wantInWindowMatches = 1 // trace t1's leaf span; t2 sits outside the window
	if got != wantInWindowMatches {
		t.Errorf("windowed chain returned %d rows, want %d (t2 is outside the request window)", got, wantInWindowMatches)
	}
}
