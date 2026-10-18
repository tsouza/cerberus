//go:build chdb

// chDB proof that a read whose window lies outside what a DateTime64(9)
// storage column can hold is answered by ClickHouse, empty, in both
// instant and range mode, while the same series read inside the domain
// answers its samples.
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

const instantDomainMetric = "instant_domain_probe"

// instantDomainSeed holds two samples just before instantDomainChDBEnd.
const instantDomainSeed = "" +
	"CREATE OR REPLACE TABLE otel_metrics_gauge (`MetricName` String, `Attributes` Map(String, String), `ResourceAttributes` Map(String, String) DEFAULT map(), `ServiceName` LowCardinality(String) DEFAULT '', `TimeUnix` DateTime64(9), `Value` Float64) ENGINE = MergeTree ORDER BY (`MetricName`, `Attributes`, `TimeUnix`);\n" +
	"CREATE OR REPLACE TABLE otel_metrics_sum (`MetricName` String, `Attributes` Map(String, String), `ResourceAttributes` Map(String, String) DEFAULT map(), `ServiceName` LowCardinality(String) DEFAULT '', `TimeUnix` DateTime64(9), `Value` Float64) ENGINE = MergeTree ORDER BY (`MetricName`, `Attributes`, `TimeUnix`);\n" +
	"INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES\n" +
	"    ('" + instantDomainMetric + "', map('job', 'api'), toDateTime64('2026-01-01 00:00:00', 9), 1.0),\n" +
	"    ('" + instantDomainMetric + "', map('job', 'api'), toDateTime64('2026-01-01 00:01:00', 9), 2.0);\n"

var instantDomainChDBEnd = time.Date(2026, 1, 1, 0, 2, 0, 0, time.UTC)

func TestInstantDomain_EmptyReadsAnsweredInSQL_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, instantDomainSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	const rangeSteps = 3 // [end-2m, end] at a 1m step

	cases := []struct {
		query string
		// want is the value every answered sample carries; nil means the
		// answer is empty.
		want []float64
	}{
		// Control: the series is present inside the domain
		// (2026-01-01T00:01:00Z).
		{query: instantDomainMetric + ` @ 1767225660`, want: []float64{2}},
		// Year one, before the int64-nanosecond floor.
		{query: instantDomainMetric + ` @ -62135596800`},
		{query: `count_over_time(` + instantDomainMetric + `[5m:1m] @ -62135596800)`},
		{query: `max_over_time(rate(` + instantDomainMetric + `[5m])[10m:1m] @ -62135596800)`},
		// 2286, past the int64-nanosecond ceiling.
		{query: `sum(rate(` + instantDomainMetric + `[5m] @ 10000000000))`},
		// The empty read still composes with what ClickHouse answers.
		{query: instantDomainMetric + ` @ -62135596800 or vector(7)`, want: []float64{7}},
	}
	for _, tc := range cases {
		for _, mode := range []string{"instant", "range"} {
			t.Run(tc.query+"/"+mode, func(t *testing.T) {
				expr, err := p.ParseExpr(tc.query)
				if err != nil {
					t.Fatalf("ParseExpr(%q): %v", tc.query, err)
				}
				start, step, wantRows := instantDomainChDBEnd, time.Duration(0), len(tc.want)
				if mode == "range" {
					start, step, wantRows = instantDomainChDBEnd.Add(-2*time.Minute), time.Minute, len(tc.want)*rangeSteps
				}
				plan, err := promql.LowerAtRange(context.Background(), expr, s, start, instantDomainChDBEnd, step)
				if err != nil {
					t.Fatalf("LowerAtRange(%q): %v", tc.query, err)
				}
				sqlStr, args, err := chsql.Emit(context.Background(), plan)
				if err != nil {
					t.Fatalf("Emit(%q): %v", tc.query, err)
				}
				rows := fixture.queryOverEmitted(t, "Value", sqlStr, args)
				defer func() { _ = rows.Close() }()
				n := 0
				for rows.Next() {
					var v float64
					if err := rows.Scan(&v); err != nil {
						t.Fatalf("scan Value: %v", err)
					}
					if v != tc.want[0] {
						t.Errorf("query %q: row %d Value = %v, want %v", tc.query, n, v, tc.want[0])
					}
					n++
				}
				if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
					t.Fatalf("rows.Err: %v", err)
				}
				if n != wantRows {
					t.Fatalf("query %q: got %d rows, want %d", tc.query, n, wantRows)
				}
			})
		}
	}
}
