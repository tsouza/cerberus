//go:build chdb

package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/prometheus/prometheus/promql/parser"
	logqlsyntax "github.com/tsouza/cerberus/internal/logql/lsyntax"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
	traceqlast "github.com/tsouza/cerberus/internal/traceql/ast"
)

// e2eResult is one representative query's end-to-end latency on the large
// synthetic dataset. The query is lowered + optimized + emitted through the
// REAL cerberus production pipeline (parse -> lower -> optimizer.Default().Run
// -> chsql.Emit, the exact sequence internal/engine drives) and executed on
// chDB; the latency is best-of-N (the floor, the most stable single-process
// estimate).
type e2eResult struct {
	Name     string
	Head     string // promql / logql / traceql
	Query    string // the upstream-QL query text
	ScanRows int64  // rows in the table(s) the query reads
	Wall     time.Duration
}

// e2eDatasetRows is the per-table synthetic row count. Generated
// server-side via INSERT … SELECT FROM numbers(N) — no row-by-row inserts.
// Sized to a realistic dashboard panel scale (~300k–500k samples per
// signal), not a stress artifact.
const e2eDatasetRows = 500_000

// measureE2E seeds the large datasets and measures each representative
// query. Returns the results plus the actual seeded row counts (so the
// doc can report the real dataset size).
func measureE2E(s *session, iters int) (results []e2eResult, metricRows, logRows, traceRows int64, err error) {
	if err = seedE2EMetrics(s); err != nil {
		return nil, 0, 0, 0, fmt.Errorf("seed metrics: %w", err)
	}
	if err = seedE2ELogs(s); err != nil {
		return nil, 0, 0, 0, fmt.Errorf("seed logs: %w", err)
	}
	if err = seedE2ETraces(s); err != nil {
		return nil, 0, 0, 0, fmt.Errorf("seed traces: %w", err)
	}

	metricRows, _ = s.scalarCount("SELECT * FROM e2e_metrics_gauge")
	logRows, _ = s.scalarCount("SELECT * FROM e2e_logs")
	traceRows, _ = s.scalarCount("SELECT * FROM e2e_traces")

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	// 1. Instant query — a rate over a sum metric at a single eval time.
	instSQL, err := emitPromInstant(`sum(rate(e2e_http_requests[5m]))`, now)
	if err != nil {
		return nil, 0, 0, 0, fmt.Errorf("instant lower: %w", err)
	}
	instSQL = retargetMetrics(instSQL)
	instWall, err := s.bestWall(instSQL, iters)
	if err != nil {
		return nil, 0, 0, 0, fmt.Errorf("instant exec: %w", err)
	}
	results = append(results, e2eResult{
		Name: "instant query", Head: "promql",
		Query: `sum(rate(e2e_http_requests[5m]))`, ScanRows: metricRows, Wall: instWall,
	})

	// 2. Range query — a realistic step grid over 1h at 15s step.
	rangeSQL, err := emitPromRange(`sum(rate(e2e_http_requests[5m]))`,
		now.Add(-time.Hour), now, 15*time.Second)
	if err != nil {
		return nil, 0, 0, 0, fmt.Errorf("range lower: %w", err)
	}
	rangeSQL = retargetMetrics(rangeSQL)
	rangeWall, err := s.bestWall(rangeSQL, iters)
	if err != nil {
		return nil, 0, 0, 0, fmt.Errorf("range exec: %w", err)
	}
	results = append(results, e2eResult{
		Name: "range query (240 steps)", Head: "promql",
		Query: `sum(rate(e2e_http_requests[5m]))`, ScanRows: metricRows, Wall: rangeWall,
	})

	// 3. Label/series lookup — the canonical distinct-series shape the
	// series endpoint emits for a metric matcher.
	seriesSQL := `SELECT DISTINCT MetricName, Attributes
  FROM e2e_metrics_gauge WHERE MetricName = 'e2e.http.requests'`
	seriesWall, err := s.bestWall(seriesSQL, iters)
	if err != nil {
		return nil, 0, 0, 0, fmt.Errorf("series exec: %w", err)
	}
	results = append(results, e2eResult{
		Name: "series lookup", Head: "promql",
		Query: `/series?match[]=e2e_http_requests`, ScanRows: metricRows, Wall: seriesWall,
	})

	// 4. TraceQL search — a span-attribute filter over the trace table.
	tqlSQL, err := emitTraceQL(`{ span.http.status_code = 500 }`)
	if err != nil {
		return nil, 0, 0, 0, fmt.Errorf("traceql lower: %w", err)
	}
	tqlSQL = retargetTraces(tqlSQL)
	tqlWall, err := s.bestWall(tqlSQL, iters)
	if err != nil {
		return nil, 0, 0, 0, fmt.Errorf("traceql exec: %w", err)
	}
	results = append(results, e2eResult{
		Name: "TraceQL search", Head: "traceql",
		Query: `{ span.http.status_code = 500 }`, ScanRows: traceRows, Wall: tqlWall,
	})

	// 5. LogQL range — a count_over_time over a filtered log stream.
	logSQL, err := emitLogQLRange(`count_over_time({service="e2e"} |= "error" [5m])`,
		now.Add(-time.Hour), now, 15*time.Second)
	if err != nil {
		return nil, 0, 0, 0, fmt.Errorf("logql lower: %w", err)
	}
	logSQL = retargetLogs(logSQL)
	logWall, err := s.bestWall(logSQL, iters)
	if err != nil {
		return nil, 0, 0, 0, fmt.Errorf("logql exec: %w", err)
	}
	results = append(results, e2eResult{
		Name: "LogQL range", Head: "logql",
		Query: `count_over_time({service="e2e"} |= "error" [5m])`, ScanRows: logRows, Wall: logWall,
	})

	return results, metricRows, logRows, traceRows, nil
}

// --- emit helpers (real pipeline) ---------------------------------------

func emitPromInstant(q string, now time.Time) (string, error) {
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(q)
	if err != nil {
		return "", err
	}
	plan, err := promql.LowerAt(context.Background(), expr, schema.DefaultOTelMetrics(), now, now)
	if err != nil {
		return "", err
	}
	plan = optimizer.Default().Run(context.Background(), plan)
	sqlText, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		return "", err
	}
	return inlineArgs(sqlText, args), nil
}

func emitPromRange(q string, start, end time.Time, step time.Duration) (string, error) {
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(q)
	if err != nil {
		return "", err
	}
	plan, err := promql.LowerAtRange(context.Background(), expr, schema.DefaultOTelMetrics(), start, end, step)
	if err != nil {
		return "", err
	}
	plan = optimizer.Default().Run(context.Background(), plan)
	sqlText, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		return "", err
	}
	return inlineArgs(sqlText, args), nil
}

func emitTraceQL(q string) (string, error) {
	expr, err := traceqlast.Parse(q)
	if err != nil {
		return "", err
	}
	plan, err := traceql.Lower(context.Background(), expr, schema.DefaultOTelTraces())
	if err != nil {
		return "", err
	}
	plan = optimizer.Default().Run(context.Background(), plan)
	sqlText, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		return "", err
	}
	return inlineArgs(sqlText, args), nil
}

func emitLogQLRange(q string, start, end time.Time, step time.Duration) (string, error) {
	expr, err := logqlsyntax.ParseExpr(q)
	if err != nil {
		return "", err
	}
	plan, err := logql.LowerAtRange(context.Background(), expr, schema.DefaultOTelLogs(), start, end, step)
	if err != nil {
		return "", err
	}
	plan = optimizer.Default().Run(context.Background(), plan)
	sqlText, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		return "", err
	}
	return inlineArgs(sqlText, args), nil
}

// retarget* rewrite the schema-default table names the lowering emits to
// the private e2e_* tables the large dataset seeds.
func retargetMetrics(sql string) string {
	sql = strings.ReplaceAll(sql, "otel_metrics_sum", "e2e_metrics_gauge")
	sql = strings.ReplaceAll(sql, "otel_metrics_gauge", "e2e_metrics_gauge")
	return sql
}

func retargetTraces(sql string) string { return strings.ReplaceAll(sql, "otel_traces", "e2e_traces") }
func retargetLogs(sql string) string   { return strings.ReplaceAll(sql, "otel_logs", "e2e_logs") }

// --- large synthetic seeds (server-side via numbers(N)) -----------------

// seedE2EMetrics builds the gauge table: up to 5000 series × samples
// for one metric. Schema columns match what cerberus's metrics
// lowering reads (ServiceName, MetricName, Attributes, TimeUnix, Value).
func seedE2EMetrics(s *session) error {
	ddl := `CREATE OR REPLACE TABLE e2e_metrics_gauge (
  ServiceName String, MetricName String, Attributes Map(String,String),
  TimeUnix DateTime64(9), Value Float64
) ENGINE = MergeTree() ORDER BY (MetricName, Attributes, toUnixTimestamp64Nano(TimeUnix));`
	if err := s.exec(ddl); err != nil {
		return err
	}
	// 5000 series (instance dim) × (rows / 5000) timestamps. Samples span
	// the hour before the eval anchor (2026-06-01 12:00) at ~3.6s spacing so
	// the 5m-rate windows have real data.
	ins := fmt.Sprintf(`INSERT INTO e2e_metrics_gauge SELECT
  concat('svc-', toString(number %% 10)),
  'e2e.http.requests',
  map('instance', concat('i', toString(number %% 5000)), 'method', if(number %% 2 = 0, 'GET', 'POST')),
  toDateTime64('2026-06-01 11:00:00', 9) + toIntervalMillisecond((intDiv(number, 5000)) * 3600),
  toFloat64(number %% 1000)
FROM numbers(%d)`, e2eDatasetRows)
	return s.exec(ins)
}

// seedE2ELogs builds the log table. Half the bodies contain "error"
// so the |= "error" filter selects a real subset.
func seedE2ELogs(s *session) error {
	ddl := `CREATE OR REPLACE TABLE e2e_logs (
  Timestamp DateTime64(9), Body String,
  SeverityText LowCardinality(String) DEFAULT '',
  ResourceAttributes Map(String,String),
  LogAttributes Map(String,String) DEFAULT map(),
  ServiceName String DEFAULT ''
) ENGINE = MergeTree() ORDER BY (ServiceName, Timestamp);`
	if err := s.exec(ddl); err != nil {
		return err
	}
	ins := fmt.Sprintf(`INSERT INTO e2e_logs (Timestamp, Body, SeverityText, ResourceAttributes, LogAttributes, ServiceName) SELECT
  toDateTime64('2026-06-01 11:00:00', 9) + toIntervalMillisecond(number),
  if(number %% 2 = 0, concat('request error code=', toString(number %% 500)), concat('ok path=/p', toString(number %% 100))),
  if(number %% 2 = 0, 'ERROR', 'INFO'),
  map('host', concat('h', toString(number %% 200))),
  map('level', if(number %% 2 = 0, 'error', 'info')),
  'e2e'
FROM numbers(%d)`, e2eDatasetRows)
	return s.exec(ins)
}

// seedE2ETraces builds the span table. ~half the spans carry
// http.status_code=500 so the search selects a real subset.
func seedE2ETraces(s *session) error {
	ddl := `CREATE OR REPLACE TABLE e2e_traces (
  TraceId String, SpanId String, ParentSpanId String,
  SpanName String DEFAULT '', Duration UInt64 DEFAULT 0,
  Timestamp DateTime64(9) DEFAULT toDateTime64(0,9),
  ResourceAttributes Map(String,String) DEFAULT map(),
  SpanAttributes Map(String,String) DEFAULT map()
) ENGINE = MergeTree() ORDER BY (TraceId, SpanId);`
	if err := s.exec(ddl); err != nil {
		return err
	}
	ins := fmt.Sprintf(`INSERT INTO e2e_traces (TraceId, SpanId, ParentSpanId, SpanName, Duration, SpanAttributes) SELECT
  leftPad(hex(intDiv(number, 10)), 32, '0'),
  leftPad(hex(number), 16, '0'),
  if(number %% 10 = 0, '', leftPad(hex(number - 1), 16, '0')),
  concat('op-', toString(number %% 50)),
  toUInt64((number %% 1000) * 1000000),
  map('http.status_code', if(number %% 2 = 0, '500', '200'), 'http.method', if(number %% 3 = 0, 'GET', 'POST'))
FROM numbers(%d)`, e2eDatasetRows)
	return s.exec(ins)
}
