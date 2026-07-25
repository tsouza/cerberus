package ddl

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// idxTraceIDName is the skip-index name both the logs and traces DDL
// templates declare on their TraceId column (see traces_table.sql /
// logs_table.sql in the tsouza/opentelemetry-collector-contrib:cerberus-ddl
// fork). It is the single fact a query plan must reference for
// [TraceIDIndexProbe] to consider a lookup index-served rather than a full
// scan, whether the rendered branch is the bloom_filter index (the default,
// HasFullTextSearch=false) or the text index (HasFullTextSearch=true) — both
// branches keep this exact index name.
const idxTraceIDName = "idx_trace_id"

// TraceIDIndexProbeResult reports whether a candidate trace ID is
// discoverable, and specifically index-served (not full-scanned), on each
// side of the logs/traces boundary a Grafana exemplar-to-trace or
// logs-to-trace correlation hop must cross.
type TraceIDIndexProbeResult struct {
	LogsFound       bool
	TracesFound     bool
	LogsIndexUsed   bool
	TracesIndexUsed bool
}

// Consistent reports the bar MIG-21's verification methodology sets: the
// trace ID resolves on both tables AND both resolutions were served by the
// dedicated idx_trace_id skip index, not a full scan.
func (r TraceIDIndexProbeResult) Consistent() bool {
	return r.LogsFound && r.TracesFound && r.LogsIndexUsed && r.TracesIndexUsed
}

// TraceIDIndexProbe queries db — any live ClickHouse-speaking *sql.DB (chDB
// today, a real cluster once a Tier-1 migration scenario exists) — for
// traceID against logsTable.logsTraceIDCol and tracesTable.tracesTraceIDCol,
// and inspects each query's EXPLAIN indexes=1 plan to confirm idx_trace_id
// is actually consulted rather than a full scan.
//
// traceID must already be in the caller's canonical form (32-char
// lowercase hex, the OTel-CH exporter's hex.EncodeToString form — see the
// TraceIDColumn doc comments in internal/schema/logs.go /
// internal/schema/traces.go). This function performs no normalisation: that
// is Tempo's own inbound-boundary job (normaliseTraceID in
// internal/api/tempo/handler.go). Probing with a non-canonical variant of a
// genuinely stored ID (wrong case, stripped leading zeros) is precisely how
// a caller proves that canonicalisation step is load-bearing — ClickHouse
// String equality is case- and width-sensitive, so a non-canonical probe
// value is expected to report Consistent() == false.
func TraceIDIndexProbe(ctx context.Context, db *sql.DB, logsTable, logsTraceIDCol, tracesTable, tracesTraceIDCol, traceID string) (TraceIDIndexProbeResult, error) {
	var r TraceIDIndexProbeResult

	var err error
	r.LogsFound, r.LogsIndexUsed, err = traceIDLookup(ctx, db, logsTable, logsTraceIDCol, traceID)
	if err != nil {
		return TraceIDIndexProbeResult{}, fmt.Errorf("ddl: trace_id index probe against %s: %w", logsTable, err)
	}
	r.TracesFound, r.TracesIndexUsed, err = traceIDLookup(ctx, db, tracesTable, tracesTraceIDCol, traceID)
	if err != nil {
		return TraceIDIndexProbeResult{}, fmt.Errorf("ddl: trace_id index probe against %s: %w", tracesTable, err)
	}
	return r, nil
}

// traceIDLookup runs the count-matching lookup and its EXPLAIN indexes=1
// twin against one (table, column) pair, returning whether the trace ID was
// found and whether the plan shows idx_trace_id was consulted.
func traceIDLookup(ctx context.Context, db *sql.DB, table, col, traceID string) (found, indexUsed bool, err error) {
	countQuery := fmt.Sprintf("SELECT count() FROM %s WHERE %s = ?", table, col) //nolint:gosec // G201: table/col are trusted schema identifiers, not user input

	var count int64
	if err := db.QueryRowContext(ctx, countQuery, traceID).Scan(&count); err != nil {
		return false, false, fmt.Errorf("count lookup: %w", err)
	}

	explainQuery := fmt.Sprintf("EXPLAIN indexes = 1 SELECT count() FROM %s WHERE %s = ?", table, col) //nolint:gosec // G201: table/col are trusted schema identifiers, not user input
	rows, err := db.QueryContext(ctx, explainQuery, traceID)
	if err != nil {
		return false, false, fmt.Errorf("explain: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return false, false, fmt.Errorf("explain scan: %w", err)
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		return false, false, fmt.Errorf("explain rows: %w", err)
	}

	return count > 0, strings.Contains(plan.String(), idxTraceIDName), nil
}
