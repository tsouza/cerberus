//go:build chdb

// chDB-backed proof that a fused `&&` (the single-pass window gate of
// set_op.go) emits exactly the column list its plan's RowType() declares.
//
// A SetOperation's arms are closed positional projections (traceql's
// narrowSpanProjection wraps every open selector arm), and the fused shape
// reads its predicates straight off the Scan beneath that Project. Rendering
// the fused statement as `SELECT *` over the spans table makes it as wide as
// the TABLE while SetOperation.RowType() still reports the arm's narrow
// list — and a `||` around it aligns its OTHER arm to that narrow list, so
// ClickHouse rejects the UNION with `different number of columns` (code 53)
// on any spans table wider than the projection. The spec harness never saw
// it because every seed there declares exactly the projected columns, which
// makes `*` and the list coincide; a real OTel-CH table carries the nested
// Events.* / Links.* arrays and does not.
//
// The width test runs against that wide table, pre- and post-optimizer, and
// the row-type test checks every SetOperation of every fixture in the
// TraceQL corpus against the driver's projection — the contract
// AssertRowTypeMatchesDriver pins for the wrapped root only.
package chsql_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/chsqltest"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/schema"
	tql "github.com/tsouza/cerberus/internal/traceql"
	"github.com/tsouza/cerberus/internal/traceql/ast"
	"github.com/tsouza/cerberus/test/spec"
)

// fusedIntersectWideSpansDDL mirrors the real OTel-CH traces schema INCLUDING
// the nested Events.* / Links.* array columns, so `SELECT *` over it is
// strictly wider than the 13-column narrow span projection.
const fusedIntersectWideSpansDDL = `CREATE TABLE otel_traces (
    TraceId String, SpanId String, ParentSpanId String, SpanName String,
    SpanKind String, Duration Int64, Timestamp DateTime64(9),
    StatusCode String, StatusMessage String, ScopeName String, ScopeVersion String,
    SpanAttributes Map(String,String), ResourceAttributes Map(String,String),
    "Events.Timestamp" Array(DateTime64(9)), "Events.Name" Array(String), "Events.Attributes" Array(Map(String,String)),
    "Links.TraceId" Array(String), "Links.SpanId" Array(String), "Links.TraceState" Array(String), "Links.Attributes" Array(Map(String,String))
) ENGINE = MergeTree() ORDER BY (Timestamp)`

// fusedIntersectWideSeed plants trace t1 with one span matching `span.a =
// "x"` and another matching `span.b = "y"` (so the `&&` arm admits the
// trace and carries both spans), trace t2 with one span matching `span.c =
// "z"` (the `||` arm), and trace t3 matching only `span.a = "x"` (admitted
// by neither).
const fusedIntersectWideSeed = `INSERT INTO otel_traces
    (TraceId, SpanId, ParentSpanId, SpanName, SpanKind, Duration, Timestamp, StatusCode, SpanAttributes) VALUES
    ('t1', 's1', '', 'root', 'Server', 10, toDateTime64('2026-05-01 10:00:00', 9), 'Unset', map('a', 'x')),
    ('t1', 's2', 's1', 'child', 'Client', 10, toDateTime64('2026-05-01 10:00:01', 9), 'Unset', map('b', 'y')),
    ('t2', 's3', '', 'root', 'Server', 10, toDateTime64('2026-05-01 10:00:02', 9), 'Unset', map('c', 'z')),
    ('t3', 's4', '', 'root', 'Server', 10, toDateTime64('2026-05-01 10:00:03', 9), 'Unset', map('a', 'x'))`

// fusedIntersectWideWantSpans is the span count both compositions must
// return: t1's two spans through the `&&` arm plus t2's one through the
// plain arm.
const fusedIntersectWideWantSpans = 3

func lowerTraceQL(t *testing.T, q string) chplan.Node {
	t.Helper()
	expr, err := ast.Parse(q)
	if err != nil {
		t.Fatalf("parse %q: %v", q, err)
	}
	plan, err := tql.Lower(context.Background(), expr, schema.DefaultOTelTraces())
	if err != nil {
		t.Fatalf("lower %q: %v", q, err)
	}
	return plan
}

func countRows(t *testing.T, db *sql.DB, plan chplan.Node) int {
	t.Helper()
	sqlText, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	var got int
	if err := db.QueryRow("SELECT count() FROM ("+sqlText+")", args...).Scan(&got); err != nil {
		t.Fatalf("execute: %v\nSQL: %s", err, sqlText)
	}
	return got
}

// TestFusedIntersect_InsideUnion_WideTable_ChDB executes a spanset `&&`
// nested inside a spanset `||` — in both arm orders, pre- and
// post-optimizer — against the wide spans table and pins the span set.
func TestFusedIntersect_InsideUnion_WideTable_ChDB(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec(fusedIntersectWideSpansDDL); err != nil {
		t.Fatalf("create spans table: %v", err)
	}
	if _, err := db.Exec(fusedIntersectWideSeed); err != nil {
		t.Fatalf("seed spans: %v", err)
	}
	for _, q := range []string{
		`({ span.a = "x" } && { span.b = "y" }) || { span.c = "z" }`,
		`{ span.c = "z" } || ({ span.a = "x" } && { span.b = "y" })`,
	} {
		plan := lowerTraceQL(t, q)
		for name, n := range map[string]chplan.Node{
			"lowered":   plan,
			"optimized": optimizer.Default().Run(context.Background(), chplan.CloneNode(plan)),
		} {
			t.Run(fmt.Sprintf("%s/%s", q, name), func(t *testing.T) {
				if got := countRows(t, db, n); got != fusedIntersectWideWantSpans {
					t.Fatalf("%s returned %d spans, want %d", q, got, fusedIntersectWideWantSpans)
				}
			})
		}
	}
}

// TestFusedIntersect_InsideUnion_TempoSearch_ChDB drives the same two
// compositions through the production path — the Tempo /api/search handler,
// engine, optimizer and sample projection — on the wide table, and pins the
// matched spans per trace. This is the request Grafana sends; a width
// mismatch between the fused arm and its `||` sibling surfaces here as a
// 502.
func TestFusedIntersect_InsideUnion_TempoSearch_ChDB(t *testing.T) {
	c := chclienttest.NewChDB(t)
	c.Seed(t, fusedIntersectWideSpansDDL+";\n"+fusedIntersectWideSeed)
	h := tempo.New(c, schema.DefaultOTelTraces(), "v-test", nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// The seed's spans sit inside this window (2026-05-01 10:00 UTC).
	const window = "&start=1777593600&end=1777680000"
	for _, q := range []string{
		`({ span.a = "x" } && { span.b = "y" }) || { span.c = "z" }`,
		`{ span.c = "z" } || ({ span.a = "x" } && { span.b = "y" })`,
	} {
		t.Run(q, func(t *testing.T) {
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
				srv.URL+"/api/search?q="+url.QueryEscape(q)+window, nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
			}
			var sr tempo.SearchResponse
			if err := json.Unmarshal(body, &sr); err != nil {
				t.Fatalf("decode: %v\nbody: %s", err, body)
			}
			got := map[string]int{}
			for _, tr := range sr.Traces {
				if tr.SpanSet == nil {
					t.Fatalf("trace %s carries no spanSet: %s", tr.TraceID, body)
				}
				got[tr.TraceID] = len(tr.SpanSet.Spans)
			}
			want := map[string]int{"t1": 2, "t2": 1}
			if len(got) != len(want) {
				t.Fatalf("traces = %v, want %v: %s", got, want, body)
			}
			for id, n := range want {
				if got[id] != n {
					t.Fatalf("trace %s matched %d spans, want %d: %s", id, got[id], n, body)
				}
			}
		})
	}
}

// TestSetOperation_RowTypeMatchesEmittedProjection_ChDB walks every seeded
// TraceQL fixture, emits every SetOperation node of its lowered plan on its
// own, and asserts the driver's projection is exactly the node's
// RowType() — for the fused `&&` shape and the union-gated fallback alike.
// The seed is widened with the nested Events.* / Links.* arrays so a
// `SELECT *` cannot coincide with the projected list by accident.
func TestSetOperation_RowTypeMatchesEmittedProjection_ChDB(t *testing.T) {
	fixtureDir := filepath.Join("..", "..", "test", "spec", "traceql")
	s := schema.DefaultOTelTraces()
	checked := 0
	spec.Walk(t, fixtureDir, func(t *testing.T, c *spec.Case) {
		q, ok := c.Section("query.traceql")
		if !ok {
			return
		}
		seed, ok := c.Section("seed")
		if !ok {
			return
		}
		expr, err := ast.Parse(strings.TrimSpace(q))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		plan, err := tql.Lower(context.Background(), expr, s)
		if err != nil {
			t.Fatalf("lower: %v", err)
		}
		var setOps []*chplan.SetOperation
		chplan.WalkDeep(plan, func(n chplan.Node) bool {
			if so, ok := n.(*chplan.SetOperation); ok {
				setOps = append(setOps, so)
			}
			return true
		})
		if len(setOps) == 0 {
			return
		}
		db := chsqltest.OpenIsolatedChDB(t)
		spec.ApplySeed(t, db, seed)
		widenSpansTable(t, db)
		for i, so := range setOps {
			sqlText, args, err := chsql.Emit(context.Background(), so)
			if err != nil {
				t.Fatalf("emit SetOperation %d: %v", i, err)
			}
			cols := describeColumns(t, db, sqlText, args)
			want := so.RowType()
			if want.Open {
				t.Fatalf("SetOperation %d has an open RowType; the emitter must refuse it", i)
			}
			if len(cols) != len(want.Columns) {
				t.Fatalf("SetOperation %d (%s): RowType has %d columns, the emitted statement %d\nRowType=%q\ndriver=%q\nSQL: %s",
					i, so.Op, len(want.Columns), len(cols), columnNames(want), cols, sqlText)
			}
			for j, column := range want.Columns {
				if column.Name != cols[j] {
					t.Fatalf("SetOperation %d (%s): RowType column %d = %q, driver = %q", i, so.Op, j, column.Name, cols[j])
				}
			}
			checked++
		}
	})
	if checked == 0 {
		t.Fatal("no SetOperation node was checked; the TraceQL corpus carries set-op fixtures")
	}
}

// describeColumns returns the statement's output column names in order, via
// `DESCRIBE TABLE (<sql>)` — the server's own view of the projection, read
// as plain strings so the Map-typed span columns never have to cross the
// driver's value decoding.
func describeColumns(t *testing.T, db *sql.DB, sqlText string, args []any) []string {
	t.Helper()
	rows, err := db.Query("DESCRIBE TABLE ("+sqlText+")", args...)
	if err != nil {
		t.Fatalf("describe: %v\nSQL: %s", err, sqlText)
	}
	defer func() { _ = rows.Close() }()
	var (
		out                                                      []string
		name, typ, defKind, defExpr, comment, codec, ttl, ignore string
	)
	for rows.Next() {
		// DESCRIBE's column set varies by server version; scan the leading
		// `name` and tolerate the trailing metadata columns.
		cols, err := rows.Columns()
		if err != nil {
			t.Fatalf("describe columns: %v", err)
		}
		dest := []any{&name, &typ, &defKind, &defExpr, &comment, &codec, &ttl, &ignore}[:len(cols)]
		if err := rows.Scan(dest...); err != nil {
			t.Fatalf("describe scan: %v", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("describe rows: %v", err)
	}
	return out
}

func columnNames(s chplan.Schema) []string {
	out := make([]string, len(s.Columns))
	for i, c := range s.Columns {
		out[i] = c.Name
	}
	return out
}

// widenSpansTable adds the nested Events.* / Links.* array columns to a
// fixture's seeded otel_traces so the table is wider than any projection the
// query names, without touching the fixture's own CREATE/INSERT text.
func widenSpansTable(t *testing.T, db *sql.DB) {
	t.Helper()
	const alter = `ALTER TABLE otel_traces
    ADD COLUMN "Events.Timestamp" Array(DateTime64(9)) DEFAULT [],
    ADD COLUMN "Events.Name" Array(String) DEFAULT [],
    ADD COLUMN "Links.TraceId" Array(String) DEFAULT [],
    ADD COLUMN "Links.SpanId" Array(String) DEFAULT []`
	if _, err := db.Exec(alter); err != nil {
		t.Fatalf("widen otel_traces: %v", err)
	}
}
