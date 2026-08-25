//go:build chdb

// chDB-backed proof that `absent(<exp-histogram selector>)[<range>:<step>]`
// (cerberus issue #2602) lowers to valid SQL and EXECUTES against real
// ClickHouse, instead of hard-rejecting via expHistogramSelectorRouting's
// catch-all — the subquery sibling of #2443 (instant absent()) and #2226
// (absent_over_time()), both of which already had chdb-free structural
// coverage before this fix; this file adds the execution-level proof the
// composition never had.
//
// Two arms:
//   - "densely_present": the series has a sample every minute across the
//     whole subquery window, so every anchor's 5-minute staleness lookback
//     finds a match — absent() must report NOTHING (zero output rows).
//   - "genuinely_absent": the series has zero rows anywhere in the table,
//     so every anchor's lookback comes up empty — absent() must report a
//     synthesised `1` row at EVERY anchor of the subquery grid.
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

const absentSubqueryPresentMetric = "absent_subquery_present_probe_exp_hist"

// absentSubqueryEvalTS anchors the [10m:1m] subquery grid at
// [00:10:00, 00:20:00] (11 anchors, one per minute).
var (
	absentSubqueryEvalTS   = time.Date(2026, 1, 1, 0, 20, 0, 0, time.UTC)
	absentSubqueryGridSize = 11 // (10m / 1m) + 1, inclusive of both grid ends
)

// absentSubquerySeed seeds one series (job="api") with a sample every
// minute from 00:00:00 through 00:25:00 — comfortably covering the
// [00:05:00, 00:20:00] span every one of the grid's 11 anchors' own 5m
// staleness lookback reads from.
var absentSubquerySeed = "" +
	"CREATE OR REPLACE TABLE otel_metrics_exponential_histogram (" +
	"`MetricName` String, `Attributes` Map(String, String), " +
	"`ResourceAttributes` Map(String, String) DEFAULT map(), `ServiceName` LowCardinality(String) DEFAULT '', " +
	"`TimeUnix` DateTime64(9), " +
	"`Count` UInt64, `Sum` Float64, `Scale` Int32, `ZeroCount` UInt64, " +
	"`PositiveOffset` Int32, `PositiveBucketCounts` Array(UInt64), " +
	"`NegativeOffset` Int32, `NegativeBucketCounts` Array(UInt64)" +
	") ENGINE = MergeTree ORDER BY (`MetricName`, `Attributes`, `TimeUnix`);\n" +
	absentSubqueryPresentInserts()

func absentSubqueryPresentInserts() string {
	out := "INSERT INTO otel_metrics_exponential_histogram " +
		"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n"
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i <= 25; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		sep := ",\n"
		if i == 25 {
			sep = ";\n"
		}
		out += "    ('" + absentSubqueryPresentMetric + "', map('job', 'api'), toDateTime64('" +
			ts.Format("2006-01-02 15:04:05") + "', 9), 1, 1.0, 0, 0, 0, [1], 0, [])" + sep
	}
	return out
}

func TestLower_ExpHistogram_AbsentSubquery_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, absentSubquerySeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	cases := []struct {
		name      string
		query     string
		wantRows  int
		wantValue float64
	}{
		// The fixed composition, executed over a series with a matching
		// sample inside every anchor's 5m lookback: absent() must find
		// the series present at every one of the 11 anchors and emit
		// nothing.
		{
			name:     "densely_present",
			query:    `absent(` + absentSubqueryPresentMetric + `{job="api"})[10m:1m]`,
			wantRows: 0,
		},
		// The genuinely-empty case: a selector matching zero rows
		// anywhere in the exp-histogram table. Every anchor's lookback
		// comes up empty, so absent() must emit the synthesised `1` row
		// at each of the grid's 11 anchors.
		{
			name:      "genuinely_absent",
			query:     `absent(` + absentSubqueryPresentMetric + `{job="nonexistent"})[10m:1m]`,
			wantRows:  absentSubqueryGridSize,
			wantValue: 1.0,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, absentSubqueryEvalTS, absentSubqueryEvalTS)
			if err != nil {
				t.Fatalf("LowerAt(%q): unexpected error (this exact composition was hard-rejected via expHistogramSelectorRouting's catch-all before cerberus issue #2602's fix): %v", tc.query, err)
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
				if tc.wantRows > 0 && v != tc.wantValue {
					t.Errorf("query %q: row %d Value = %v, want %v", tc.query, n, v, tc.wantValue)
				}
			}
			if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
				t.Fatalf("rows.Err: %v", err)
			}
			if n != tc.wantRows {
				t.Fatalf("query %q: got %d rows, want %d", tc.query, n, tc.wantRows)
			}
		})
	}
}
