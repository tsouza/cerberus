//go:build chdb

// chDB-backed proof that a subquery window narrower than one step answers
// the EMPTY matrix, for the BARE-selector shape.
//
// #3183's own example is `up[1s:1m]`. Two different code paths carry it,
// and both carried the same false claim that "reference clamps start to
// end":
//
//   - an inner with an operator (`(up * 2)[1s:1m]`) goes through
//     internal/promql's subqueryGridCtx, pinned by
//     subquery_grid_test.go's TestSubqueryGridCtxReportsAnEmptyWindow;
//   - the bare selector goes straight to internal/chsql's
//     stepAlignedAnchorCountFor via lowerSubqueryOverVectorSelector, and
//     never touches that grid at all.
//
// Reference does not clamp. promql/engine.go's evalSubquery leaves
// `newEv.endTimestamp` unsnapped and sets `newEv.startTimestamp` to the
// snapped base bumped by one interval, so a window narrower than one step
// leaves start > end and `if ev.endTimestamp < ev.startTimestamp {
// return Matrix{}, nil }` returns nothing.
//
// The seed is what makes the zero-row assertion mean something: it puts
// a sample INSIDE the [1s] window, so an emitted grid holding the one
// clamped anchor returns a row. An empty seed would make this test pass
// under the bug.
package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

const subStepSubqueryMetric = "substep_subquery_probe"

// subStepSubqueryEvalTS is deliberately OFF the 1m grid (δ = 41s), which
// is the condition under which the window spans no phase-0 anchor.
var subStepSubqueryEvalTS = time.Date(2026, 1, 1, 1, 4, 41, 0, time.UTC)

// The seed is what makes the zero-row assertion mean something.
//
//   - 01:04:40 and 01:04:41 sit INSIDE the `(end-1s, end]` window, so the
//     one clamped anchor the bug emitted returns a row. An empty seed, or
//     one whose samples fell outside, would let this test pass under the
//     bug.
//   - 01:00:30 sits before the snapped base, so the wide-window case has
//     real anchors to find it at and the zero above cannot be read as
//     "this shape returns nothing whatever the window".
//
// otel_metrics_sum is declared empty alongside the gauge table because the
// float read path scans
// `merge(currentDatabase(), '^(otel_metrics_gauge|otel_metrics_sum)$')`,
// and the harness refuses a seed that would resolve only through a sibling
// fixture's tables in the shared chDB session.
var subStepSubquerySeed = "" +
	"CREATE OR REPLACE TABLE otel_metrics_gauge (" +
	"`MetricName` String, `Attributes` Map(String, String), " +
	"`ResourceAttributes` Map(String, String) DEFAULT map(), `ServiceName` LowCardinality(String) DEFAULT '', " +
	"`TimeUnix` DateTime64(9), `Value` Float64" +
	") ENGINE = MergeTree ORDER BY (`MetricName`, `Attributes`, `TimeUnix`);\n" +
	"CREATE OR REPLACE TABLE otel_metrics_sum (" +
	"`MetricName` String, `Attributes` Map(String, String), " +
	"`ResourceAttributes` Map(String, String) DEFAULT map(), `ServiceName` LowCardinality(String) DEFAULT '', " +
	"`TimeUnix` DateTime64(9), `Value` Float64" +
	") ENGINE = MergeTree ORDER BY (`MetricName`, `Attributes`, `TimeUnix`);\n" +
	"INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES\n" +
	"    ('" + subStepSubqueryMetric + "', map('job', 'api'), toDateTime64('2026-01-01 01:04:41', 9), 7.0),\n" +
	"    ('" + subStepSubqueryMetric + "', map('job', 'api'), toDateTime64('2026-01-01 01:04:40', 9), 6.0),\n" +
	"    ('" + subStepSubqueryMetric + "', map('job', 'api'), toDateTime64('2026-01-01 01:00:30', 9), 5.0);\n"

func TestLower_SubStepSubqueryWindow_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, subStepSubquerySeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	cases := []struct {
		name     string
		query    string
		wantRows int
	}{
		{
			// The issue's example. Window (end-1s, end] holds no
			// phase-0 minute, so reference answers the empty matrix.
			name:     "bare selector, window narrower than one step",
			query:    subStepSubqueryMetric + `{job="api"}[1s:1m]`,
			wantRows: 0,
		},
		{
			// The other side of the boundary. δ = 41s, so the grid holds
			// ceil((5m − 41s)/1m) = 5 anchors: 01:00 … 01:04, every one
			// inside the (00:59:41, 01:04:41] window. Each anchor reads
			// its own 5m staleness lookback `(anchor − 5m, anchor]`, and
			// the 01:00:30 sample falls inside four of them — 01:01
			// through 01:04. 01:00:00 predates it, and the 01:04:40 /
			// 01:04:41 samples postdate every anchor.
			name:     "bare selector, window wide enough to span anchors",
			query:    subStepSubqueryMetric + `{job="api"}[5m:1m]`,
			wantRows: 4,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, subStepSubqueryEvalTS, subStepSubqueryEvalTS)
			if err != nil {
				t.Fatalf("LowerAt(%q): %v", tc.query, err)
			}
			sqlStr, args, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit(%q): %v", tc.query, err)
			}

			rows := fixture.queryOverEmitted(t, "Value", sqlStr, args)
			defer func() { _ = rows.Close() }()

			n := 0
			for rows.Next() {
				n++
				var v float64
				if err := rows.Scan(&v); err != nil {
					t.Fatalf("scan Value: %v", err)
				}
			}
			if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
				t.Fatalf("rows.Err: %v", err)
			}
			if n != tc.wantRows {
				t.Fatalf("query %q at %s: got %d rows, want %d (reference answers the empty "+
					"matrix for a window narrower than one step)", tc.query, subStepSubqueryEvalTS, n, tc.wantRows)
			}
		})
	}
}
