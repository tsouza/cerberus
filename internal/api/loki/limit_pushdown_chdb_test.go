//go:build chdb

// chDB-backed correctness proof for cerberus issue #2829 (Loki's `limit`
// pushed into a real SQL `ORDER BY Timestamp {DESC|ASC} LIMIT N`).
//
// Two claims need proving, not asserting, per the issue's own correctness
// bar:
//
//  1. For a pipeline shape [pipelineCanDropRowsInGo] classifies SAFE, SQL
//     truncating the result set is IDENTICAL to today's decode-everything-
//     then-Go-clamp path — see TestLimitPushdown_SafeShapes_IdenticalToGoOnlyClamp_ChDB,
//     which runs the SAME query through the SAME buildRangeData post-
//     processing pipeline twice — once against a plan with no SQL Limit
//     (today's shape) and once against the pushed-down plan — and asserts
//     the two *QueryData results are deeply equal.
//  2. For the ONE shape it classifies UNSAFE (`| pattern` followed by a
//     `__error__` / `__error_details__` label filter), naively pushing the
//     SQL Limit ahead of that stage's Go-only re-filter (see
//     internal/api/loki/post_process.go's newLabelFilterStep) WOULD have
//     produced a wrong answer — see
//     TestLimitPushdown_UnsafeShape_NaivePushdownWouldBeWrong_ChDB, which
//     builds the naive (unguarded) plan by hand, alongside the actual
//     shipped plan, and shows they diverge (10 correct streams vs 0).
package loki

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/schema"
)

// limitPushdownLogsDDL is the otel_logs projection the log-line
// wrap-projection (internal/logql/lang.go's ProjectSamples) always
// selects — Body, LogAttributes, ResourceAttributes, SeverityText,
// Timestamp — regardless of which columns the query itself touches, so
// every column has to exist even for the bare-selector case. `__error__`
// rides as an ordinary ResourceAttributes key in the unsafe-shape test —
// see its doc comment for why that is a faithful, not contrived, way to
// exercise [FiltersErrorLabel]'s SQL-fallback path.
const limitPushdownLogsDDL = `CREATE TABLE otel_logs (
    Timestamp DateTime64(9),
    Body String,
    SeverityText LowCardinality(String) DEFAULT '',
    ResourceAttributes Map(String, String),
    LogAttributes Map(String, String)
) ENGINE = Memory;`

// newLimitPushdownEngine seeds ddl into a fresh chDB instance and returns
// an engine.Engine wired exactly like [New] wires Handler.Engine, so the
// plan lifecycle (wrap-projection, optimize, emit, execute) this test
// drives is the same one production traffic runs.
func newLimitPushdownEngine(t *testing.T, ddl string) *engine.Engine {
	t.Helper()
	c := chclienttest.NewChDB(t)
	c.Seed(t, ddl)
	h := New(c, schema.DefaultOTelLogs(), nil)
	return h.Engine
}

// insertRows renders n INSERT rows (one per second starting at start,
// each row's timestamp unique — sidestepping the tie-break
// non-determinism [clampLogRows]'s doc comment already flags as
// pre-existing, CH's natural row order for equal sort keys being
// unspecified, which would otherwise confound a byte-identical-output
// assertion with a question this change does not touch) and returns the
// full DDL+INSERT statement seedable via chclienttest.
func insertRows(n int, start time.Time, attrs func(i int) map[string]string, body func(i int) string) string {
	const tsFmt = "2006-01-02 15:04:05.000000000"
	stmt := limitPushdownLogsDDL + "\nINSERT INTO otel_logs (Timestamp, Body, ResourceAttributes) VALUES\n"
	for i := 0; i < n; i++ {
		ts := start.Add(time.Duration(i) * time.Second).Format(tsFmt)
		m := "map("
		first := true
		for k, v := range attrs(i) {
			if !first {
				m += ", "
			}
			first = false
			m += fmt.Sprintf("'%s', '%s'", k, v)
		}
		m += ")"
		if i > 0 {
			stmt += ",\n"
		}
		stmt += fmt.Sprintf("    (toDateTime64('%s', 9), '%s', %s)", ts, body(i), m)
	}
	return stmt + ";"
}

// TestLimitPushdown_SafeShapes_IdenticalToGoOnlyClamp_ChDB is the primary
// correctness proof for the shapes this PR pushes a SQL Limit for: for
// each of a bare stream selector and a pipeline whose only stages lower
// to real SQL predicates, decoding the FULL matching window and
// Go-clamping (the [logql.Lang.LogLineLimit] == 0 plan) produces the
// EXACT SAME [QueryData] as letting SQL truncate to the request's limit
// first (LogLineLimit == N) and then running the identical
// [buildRangeData] post-processing on the smaller row set.
//
// Falsifiability: if [pipelineCanDropRowsInGo] under-classified a shape
// as safe (missed a Go-side row-dropping stage), the two paths would
// disagree whenever a dropped-and-not-yet-decoded row would have
// displaced a kept one inside the requested limit — this test's window
// (200 rows, limit 10) gives that scenario ample room to manifest.
func TestLimitPushdown_SafeShapes_IdenticalToGoOnlyClamp_ChDB(t *testing.T) {
	const (
		seedRows = 200
		limit    = 10
	)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Duration(seedRows) * time.Second)
	s := schema.DefaultOTelLogs()

	cases := []struct {
		name  string
		query string
		body  func(i int) string
		attrs func(i int) map[string]string
	}{
		{
			name:  "bare stream selector",
			query: `{app="x"}`,
			body:  func(i int) string { return fmt.Sprintf("line %d", i) },
			attrs: func(i int) map[string]string { return map[string]string{"app": "x"} },
		},
		{
			name:  "line filter",
			query: `{app="x"} |= "even"`,
			body: func(i int) string {
				if i%2 == 0 {
					return fmt.Sprintf("line %d even", i)
				}
				return fmt.Sprintf("line %d odd", i)
			},
			attrs: func(i int) map[string]string { return map[string]string{"app": "x"} },
		},
		{
			name:  "json parser + label filter on an extracted key",
			query: `{app="x"} | json | status="200"`,
			body: func(i int) string {
				status := "500"
				if i%3 == 0 {
					status = "200"
				}
				return fmt.Sprintf(`{"status":"%s"}`, status)
			},
			attrs: func(i int) map[string]string { return map[string]string{"app": "x"} },
		},
		{
			name:  "pattern parser, no downstream error filter",
			query: `{app="x"} | pattern "<lvl> <_>"`,
			body:  func(i int) string { return fmt.Sprintf("info line-%d", i) },
			attrs: func(i int) map[string]string { return map[string]string{"app": "x"} },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ddl := insertRows(seedRows, start, tc.attrs, tc.body)
			eng := newLimitPushdownEngine(t, ddl)

			expr, err := logql.ParseExprPermissive(tc.query)
			if err != nil {
				t.Fatalf("ParseExprPermissive(%q): %v", tc.query, err)
			}

			ctx := context.Background()
			langFull := &logql.Lang{Schema: s, Start: start, End: end}
			langPushed := &logql.Lang{Schema: s, Start: start, End: end, LogLineLimit: limit, LogLineBackward: true}

			resFull, err := eng.Query(ctx, langFull, tc.query)
			if err != nil {
				t.Fatalf("Query (no limit pushed): %v", err)
			}
			resPushed, err := eng.Query(ctx, langPushed, tc.query)
			if err != nil {
				t.Fatalf("Query (limit pushed): %v", err)
			}

			// Anti-vacuous: the pushed plan must actually have asked
			// ClickHouse to truncate — otherwise a byte-identical result
			// would be trivially true regardless of whether the pushdown
			// logic works at all.
			if len(resFull.Samples) <= limit {
				t.Fatalf("fixture too small to be a meaningful check: full window returned %d rows, want > %d", len(resFull.Samples), limit)
			}
			if len(resPushed.Samples) != limit {
				t.Errorf("SQL-pushed query returned %d rows, want exactly %d (SQL should have truncated) — pushdown did not actually engage", len(resPushed.Samples), limit)
			}

			dataFull, err := buildRangeData(expr, resFull.Samples, start, end, time.Minute, s, limit, directionBackward, false)
			if err != nil {
				t.Fatalf("buildRangeData (full decode + Go clamp): %v", err)
			}
			dataPushed, err := buildRangeData(expr, resPushed.Samples, start, end, time.Minute, s, limit, directionBackward, false)
			if err != nil {
				t.Fatalf("buildRangeData (SQL-pushed + Go clamp): %v", err)
			}

			if !reflect.DeepEqual(dataFull, dataPushed) {
				t.Errorf("shape %q: SQL-pushed result diverges from the decode-everything-then-Go-clamp result\nfull:   %#v\npushed: %#v", tc.name, dataFull, dataPushed)
			}
		})
	}
}

// TestLimitPushdown_UnsafeShape_NaivePushdownWouldBeWrong_ChDB is the
// negative-side proof: it demonstrates that if the `| pattern` +
// `__error__`-filter shape were NOT excluded from pushdown, cerberus
// would silently return the wrong log lines.
//
// `| pattern` never changes labelsExpr in the lowering (it extracts
// purely in Go — see internal/logql/lower.go's IsDynamicLabelStage /
// lowerStage's OpParserTypePattern case), so for
// `{app="x"} | pattern "<lvl> <_>" | __error__=""` the SQL Filter this
// PR would wrap a naive Limit(OrderBy(...)) around carries ONLY
// `app="x"` plus the request window — the `__error__=""` predicate is
// entirely absent from SQL (internal/logql/lower.go's
// lowerPipelineWithLabels `continue`-skips it), so the row's real
// ResourceAttributes `__error__` value (a legitimate Map key like any
// other — nothing in the OTel-CH schema or the lowering's `Map(String,
// String)` access reserves it) is the ONLY place that predicate is ever
// evaluated: internal/api/loki/post_process.go's newLabelFilterStep,
// Go-side, AFTER SQL has already returned rows.
//
// Seed: the 60 most-recent rows carry `__error__="boom"` (should be
// excluded); the 50 rows before them carry no `__error__` key (absent
// reads as "", so they pass `__error__=""` and should be kept).
// direction=backward (Loki's default, most-recent-first), limit=10.
//
//   - Correct answer (what the shipped code returns, because
//     [pipelineCanDropRowsInGo] excludes this shape from pushdown): SQL
//     returns the full 110-row candidate set, the Go filter drops the 60
//     "boom" rows, and the clamp takes the top 10 of the remaining 50 —
//     10 streams.
//   - Naive answer (what an UNGUARDED pushdown would have returned): SQL
//     truncates to the 10 most-recent rows BEFORE the Go filter ever
//     runs — all 10 are "boom" rows — and the Go filter then drops all
//     10, leaving ZERO. limit=10 in the request, 0 in the naive
//     response: a silently wrong answer, not merely a perf regression.
func TestLimitPushdown_UnsafeShape_NaivePushdownWouldBeWrong_ChDB(t *testing.T) {
	const (
		errorRows = 60
		cleanRows = 50
		limit     = 10
	)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Duration(errorRows+cleanRows) * time.Second)
	s := schema.DefaultOTelLogs()

	attrs := func(i int) map[string]string {
		// i indexes rows in seed (insertion/timestamp) order, oldest
		// first: the first cleanRows rows are clean, the last errorRows
		// (most recent) carry __error__.
		if i < cleanRows {
			return map[string]string{"app": "x"}
		}
		return map[string]string{"app": "x", "__error__": "boom"}
	}
	body := func(i int) string { return fmt.Sprintf("info line-%d", i) }

	ddl := insertRows(errorRows+cleanRows, start, attrs, body)
	eng := newLimitPushdownEngine(t, ddl)

	const query = `{app="x"} | pattern "<lvl> <_>" | __error__=""`
	expr, err := logql.ParseExprPermissive(query)
	if err != nil {
		t.Fatalf("ParseExprPermissive: %v", err)
	}

	ctx := context.Background()
	lang := &logql.Lang{Schema: s, Start: start, End: end, LogLineLimit: limit, LogLineBackward: true}

	// The correct (shipped) path: Lang.Parse itself, exactly as
	// production runs it. Because this shape is UNSAFE,
	// maybePushLogLineLimit leaves the plan un-pushed regardless of
	// LogLineLimit being set — confirmed structurally by
	// TestLogLineLimitPushdown_ShapeAndGating in internal/logql; this
	// assertion is the end-to-end behavioral half of that same claim.
	resShipped, err := eng.Query(ctx, lang, query)
	if err != nil {
		t.Fatalf("Query (shipped path): %v", err)
	}
	if len(resShipped.Samples) != errorRows+cleanRows {
		t.Fatalf("shipped path returned %d rows, want all %d (SQL must NOT have truncated this unsafe shape)", len(resShipped.Samples), errorRows+cleanRows)
	}
	dataShipped, err := buildRangeData(expr, resShipped.Samples, start, end, time.Minute, s, limit, directionBackward, false)
	if err != nil {
		t.Fatalf("buildRangeData (shipped): %v", err)
	}
	gotShipped := countStreamEntries(t, dataShipped)
	if gotShipped != limit {
		t.Fatalf("shipped path returned %d entries, want %d (the %d clean rows immediately preceding the %d error rows)", gotShipped, limit, cleanRows, errorRows)
	}

	// The naive counterexample: build Limit(OrderBy(<the SAME base
	// Filter LowerAt produces>)) by hand — simulating what this PR would
	// have shipped WITHOUT the pipelineCanDropRowsInGo guard.
	basePlan, err := logql.LowerAt(ctx, expr, s, start, end)
	if err != nil {
		t.Fatalf("LowerAt (base plan): %v", err)
	}
	naivePlan := &chplan.Limit{
		Count: limit,
		Input: &chplan.OrderBy{
			Input: basePlan,
			Keys: []chplan.OrderKey{
				{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Desc: true},
			},
		},
	}
	meta := engine.Meta{IsMetric: false, ResponseShape: "loki-streams", Extra: map[string]any{"expr": expr}}
	resNaive, err := eng.QueryPlan(ctx, lang, naivePlan, meta)
	if err != nil {
		t.Fatalf("QueryPlan (naive counterexample): %v", err)
	}
	if len(resNaive.Samples) != limit {
		t.Fatalf("naive plan returned %d rows from SQL, want exactly %d (it must have truncated BEFORE the Go filter runs, or this counterexample doesn't demonstrate anything)", len(resNaive.Samples), limit)
	}
	dataNaive, err := buildRangeData(expr, resNaive.Samples, start, end, time.Minute, s, limit, directionBackward, false)
	if err != nil {
		t.Fatalf("buildRangeData (naive): %v", err)
	}
	gotNaive := countStreamEntries(t, dataNaive)

	if gotNaive != 0 {
		t.Fatalf("counterexample construction failed to reproduce the bug: naive pushdown returned %d entries, want 0 (all 10 SQL-selected rows should have been __error__=\"boom\" and dropped Go-side)", gotNaive)
	}

	t.Logf("shipped (correct, gated off): %d entries; naive (unguarded pushdown): %d entries — request limit was %d", gotShipped, gotNaive, limit)
}

// countStreamEntries sums the entry count across every stream in a
// streams-shaped *QueryData, so the two paths above can be compared by a
// single scalar without caring about incidental stream grouping.
func countStreamEntries(t *testing.T, data *QueryData) int {
	t.Helper()
	streams, ok := data.Result.([]Stream)
	if !ok {
		t.Fatalf("QueryData.Result = %T, want []Stream", data.Result)
	}
	n := 0
	for _, s := range streams {
		n += len(s.Values)
	}
	return n
}
