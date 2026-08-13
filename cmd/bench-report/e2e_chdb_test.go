//go:build chdb

package main

import (
	"strings"
	"testing"
	"time"
)

// benchNow is the fixed evaluation instant the e2e measurements use. A
// fixed anchor keeps the emitted SQL comparable between runs.
var benchNow = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

// The report's headline claim is that its numbers come from the REAL
// cerberus pipeline (parse → lower → optimize → emit), so an emit helper
// that stopped producing SQL would turn the whole document into a
// measurement of something else. Each of the five representative queries is
// pinned here at the emit boundary, where no chDB session is needed.
func TestEmitHelpersLowerTheRepresentativeQueries(t *testing.T) {
	t.Parallel()

	instant, err := emitPromInstant(`sum(rate(e2e_http_requests[5m]))`, benchNow)
	if err != nil {
		t.Fatalf("emitPromInstant: %v", err)
	}
	rangeSQL, err := emitPromRange(`sum(rate(e2e_http_requests[5m]))`,
		benchNow.Add(-time.Hour), benchNow, 15*time.Second)
	if err != nil {
		t.Fatalf("emitPromRange: %v", err)
	}
	traces, err := emitTraceQL(`{ span.http.status_code = 500 }`)
	if err != nil {
		t.Fatalf("emitTraceQL: %v", err)
	}
	logs, err := emitLogQLRange(`count_over_time({service="e2e"} |= "error" [5m])`,
		benchNow.Add(-time.Hour), benchNow, 15*time.Second)
	if err != nil {
		t.Fatalf("emitLogQLRange: %v", err)
	}

	for _, tc := range []struct{ name, sql string }{
		{"instant", instant},
		{"range", rangeSQL},
		{"traceql", traces},
		{"logql", logs},
	} {
		if !strings.Contains(strings.ToUpper(tc.sql), "SELECT") {
			t.Fatalf("%s emitted no query: %q", tc.name, tc.sql)
		}
	}

	// The range query walks a step grid the instant query does not, so the
	// two must not collapse to the same statement — that would mean the
	// range measurement is silently re-measuring the instant one.
	if instant == rangeSQL {
		t.Fatal("the instant and range queries emitted identical SQL")
	}
}

// A benchmark that reads the schema-default tables measures an EMPTY table:
// the large synthetic dataset lives in the private e2e_* tables. The
// retarget helpers are the only thing connecting the two, and they match on
// literal table names — so a schema rename would leave them silently
// matching nothing and the report would publish sub-millisecond numbers for
// queries that scanned no rows.
func TestRetargetPointsTheEmittedSQLAtTheSeededTables(t *testing.T) {
	t.Parallel()

	metrics, err := emitPromInstant(`sum(rate(e2e_http_requests[5m]))`, benchNow)
	if err != nil {
		t.Fatalf("emitPromInstant: %v", err)
	}
	if !strings.Contains(metrics, "otel_metrics_sum") && !strings.Contains(metrics, "otel_metrics_gauge") {
		t.Fatalf("the metrics lowering named neither default table, so retargetMetrics matches nothing:\n%s", metrics)
	}
	retargeted := retargetMetrics(metrics)
	if !strings.Contains(retargeted, "e2e_metrics_gauge") {
		t.Fatalf("retargetMetrics produced no e2e table reference:\n%s", retargeted)
	}
	if strings.Contains(retargeted, "otel_metrics_sum") || strings.Contains(retargeted, "otel_metrics_gauge") {
		t.Fatalf("retargetMetrics left a default table behind:\n%s", retargeted)
	}

	traces, err := emitTraceQL(`{ span.http.status_code = 500 }`)
	if err != nil {
		t.Fatalf("emitTraceQL: %v", err)
	}
	if !strings.Contains(traces, "otel_traces") {
		t.Fatalf("the traces lowering did not name otel_traces:\n%s", traces)
	}
	if got := retargetTraces(traces); strings.Contains(got, "otel_traces") || !strings.Contains(got, "e2e_traces") {
		t.Fatalf("retargetTraces did not repoint the query:\n%s", got)
	}

	logs, err := emitLogQLRange(`count_over_time({service="e2e"} |= "error" [5m])`,
		benchNow.Add(-time.Hour), benchNow, 15*time.Second)
	if err != nil {
		t.Fatalf("emitLogQLRange: %v", err)
	}
	if !strings.Contains(logs, "otel_logs") {
		t.Fatalf("the logs lowering did not name otel_logs:\n%s", logs)
	}
	if got := retargetLogs(logs); strings.Contains(got, "otel_logs") || !strings.Contains(got, "e2e_logs") {
		t.Fatalf("retargetLogs did not repoint the query:\n%s", got)
	}
}
