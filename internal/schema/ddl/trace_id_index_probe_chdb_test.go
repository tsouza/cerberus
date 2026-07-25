//go:build chdb

// This is the "direct CH trace_id index probe" half of MIG-21's
// verification methodology (docs/migration-testing.md §6): an offline
// proof, against the package's own rendered production DDL, that TraceId
// is a genuinely indexed, first-class, cross-table-consistent column — not
// a text-parse of the CREATE TABLE string, which would prove nothing about
// query-time behavior. It does not exercise the Grafana-driven
// correlation-hop half of MIG-21 (Playwright reusing the Layer-9 crawl
// engine); that half needs the live Loki+Tempo+Grafana three-signal stack
// and is Phase 2b, unbuilt here.
package ddl

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "github.com/chdb-io/chdb-go/chdb/driver"
)

// traceIDColumn is the column name both otel_logs and otel_traces give
// their trace-id correlation column (see the TraceIDColumn doc comments in
// internal/schema/logs.go / internal/schema/traces.go, which pin it to the
// same literal). [TraceIDIndexProbe] itself takes the column name as a
// parameter rather than hardcoding it, since it is schema-derived; this
// package's own tests use the one name the rendered DDL actually declares.
const traceIDColumn = "TraceId"

// probeSeedTraceID is a canonical 32-char lowercase-hex trace ID — the form
// every cerberus read path agrees on (see the TraceIDColumn doc comments).
// It deliberately starts with a leading hex zero and contains letters, so a
// single fixture can produce both a leading-zero-stripped variant and an
// upper-cased variant that each genuinely differ from the canonical form
// (see TestTraceIDIndexProbe_NonCanonicalFormDoesNotMatch).
const probeSeedTraceID = "0123456789abcdef0123456789abcdef"

// openProbeChDB opens a fresh ephemeral chDB session and renders + applies
// the package's own real production DDL for the logs and traces tables (via
// the same [renderSignal] entry point [TestRenderSignal_Metrics] and
// friends already exercise) — never a hand-copied DDL string, so this test
// tracks any future schema change automatically. The trace_id_ts lookup
// table and its materialized view (renderSignal(cfg, Traces)'s 2nd/3rd
// statements) are not needed by this probe and are skipped.
func openProbeChDB(t *testing.T) (db *sql.DB, cfg Config) {
	t.Helper()

	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping chdb: %v", err)
	}

	cfg = Config{}.withDefaults()

	logsStmts, err := renderSignal(cfg, Logs)
	if err != nil {
		t.Fatalf("renderSignal(Logs): %v", err)
	}
	tracesStmts, err := renderSignal(cfg, Traces)
	if err != nil {
		t.Fatalf("renderSignal(Traces): %v", err)
	}

	for _, stmt := range []string{logsStmts[0], tracesStmts[0]} {
		if _, err := db.Exec(promoteCreateTable(stmt)); err != nil {
			t.Fatalf("apply DDL:\n%s\nerr: %v", stmt, err)
		}
	}
	return db, cfg
}

// promoteCreateTable rewrites the rendered production
// `CREATE TABLE IF NOT EXISTS ...` statement to `CREATE OR REPLACE TABLE
// ...` so each subtest's fresh chDB session starts from an empty table
// rather than accumulating rows a prior subtest inserted — the same
// bare/idempotent-vs-replace distinction test/property/chdb.go and
// test/spec/runner_chdb.go already draw for chDB re-runnability, applied
// here because the production DDL opts into IF NOT EXISTS (correctly, for
// a real cluster) rather than the bare form those helpers auto-promote.
func promoteCreateTable(stmt string) string {
	const needle = "CREATE TABLE IF NOT EXISTS "
	return strings.Replace(stmt, needle, "CREATE OR REPLACE TABLE ", 1)
}

// insertLogRow / insertSpanRow seed exactly the columns this probe cares
// about; every other column takes ClickHouse's implicit type-default value
// (empty string / zero), which is enough to exercise a genuine TraceId
// equality lookup against a genuine inserted row.
func insertLogRow(t *testing.T, db *sql.DB, table, traceID string) {
	t.Helper()
	_, err := db.Exec(
		"INSERT INTO "+table+" (Timestamp, TraceId, ServiceName) VALUES (now64(9), ?, 'probe-svc')",
		traceID,
	)
	if err != nil {
		t.Fatalf("insert log row (table=%s, traceID=%s): %v", table, traceID, err)
	}
}

func insertSpanRow(t *testing.T, db *sql.DB, table, traceID string) {
	t.Helper()
	_, err := db.Exec(
		"INSERT INTO "+table+" (Timestamp, TraceId, SpanId, ServiceName, SpanName) "+
			"VALUES (now64(9), ?, 'a1b2c3d4e5f60708', 'probe-svc', 'probe-op')",
		traceID,
	)
	if err != nil {
		t.Fatalf("insert span row (table=%s, traceID=%s): %v", table, traceID, err)
	}
}

// TestTraceIDIndexProbe_Positive is the base case: the same canonical trace
// ID exists in both otel_logs and otel_traces, so every field of the
// result must independently be true — not just the aggregate. A test that
// only checked Consistent()==true could pass even if Consistent() itself
// were buggy (e.g. an accidental `||` in place of `&&`) as long as the
// buggy aggregate happened to still read true; asserting each field
// independently is what rules that out.
func TestTraceIDIndexProbe_Positive(t *testing.T) {
	db, cfg := openProbeChDB(t)
	insertLogRow(t, db, cfg.Tables.Logs, probeSeedTraceID)
	insertSpanRow(t, db, cfg.Tables.Traces, probeSeedTraceID)

	result, err := TraceIDIndexProbe(context.Background(), db,
		cfg.Tables.Logs, traceIDColumn, cfg.Tables.Traces, traceIDColumn,
		probeSeedTraceID)
	if err != nil {
		t.Fatalf("TraceIDIndexProbe: %v", err)
	}

	if !result.LogsFound {
		t.Error("LogsFound = false, want true: the seeded trace ID must resolve against otel_logs")
	}
	if !result.TracesFound {
		t.Error("TracesFound = false, want true: the seeded trace ID must resolve against otel_traces")
	}
	if !result.LogsIndexUsed {
		t.Error("LogsIndexUsed = false, want true: idx_trace_id must appear in the logs EXPLAIN plan")
	}
	if !result.TracesIndexUsed {
		t.Error("TracesIndexUsed = false, want true: idx_trace_id must appear in the traces EXPLAIN plan")
	}
	if !result.Consistent() {
		t.Error("Consistent() = false, want true for a trace ID genuinely present + index-served on both sides")
	}
}

// TestTraceIDIndexProbe_MissingOnTracesSide is the required negative case:
// the trace ID is seeded into otel_logs only — never into otel_traces —
// mirroring a real gap (late span, dropped export, retention skew). The
// probe must surface this as TracesFound==false / Consistent()==false, not
// silently pass. Seeding the same ID on both sides and calling that a
// "negative test" would not exercise this at all — the whole point of this
// subtest is that one side is genuinely, deliberately empty.
func TestTraceIDIndexProbe_MissingOnTracesSide(t *testing.T) {
	db, cfg := openProbeChDB(t)
	insertLogRow(t, db, cfg.Tables.Logs, probeSeedTraceID)
	// Deliberately never inserted into cfg.Tables.Traces.

	result, err := TraceIDIndexProbe(context.Background(), db,
		cfg.Tables.Logs, traceIDColumn, cfg.Tables.Traces, traceIDColumn,
		probeSeedTraceID)
	if err != nil {
		t.Fatalf("TraceIDIndexProbe: %v", err)
	}

	if !result.LogsFound {
		t.Error("LogsFound = false, want true: the logs-side seed must still resolve")
	}
	if result.TracesFound {
		t.Error("TracesFound = true, want false: this trace ID was never inserted into otel_traces")
	}
	if result.Consistent() {
		t.Error("Consistent() = true, want false: a genuine cross-table gap must be detected, not swallowed")
	}
}

// TestTraceIDIndexProbe_IndexActuallyServesTheLookup exists specifically to
// pin that LogsIndexUsed/TracesIndexUsed are derived from a live
// EXPLAIN indexes=1 plan, not from a strings.Contains scan of the CREATE
// TABLE text. A DDL-text-only check already exists elsewhere in this
// package (the rendered-statement assertions in ddl_test.go) and proves
// nothing about whether ClickHouse actually consults the index at query
// time — a skip index can be declared and still go unused if the WHERE
// predicate shape doesn't match it. This subtest re-derives the EXPLAIN
// plan itself, independent of [traceIDLookup]'s own parsing, so the
// assertion cannot be satisfied by a hollow implementation that hardcodes
// indexUsed=true regardless of what EXPLAIN actually returns.
func TestTraceIDIndexProbe_IndexActuallyServesTheLookup(t *testing.T) {
	db, cfg := openProbeChDB(t)
	insertLogRow(t, db, cfg.Tables.Logs, probeSeedTraceID)
	insertSpanRow(t, db, cfg.Tables.Traces, probeSeedTraceID)

	logsFound, logsIndexUsed, err := traceIDLookup(context.Background(), db, cfg.Tables.Logs, traceIDColumn, probeSeedTraceID)
	if err != nil {
		t.Fatalf("traceIDLookup(logs): %v", err)
	}
	tracesFound, tracesIndexUsed, err := traceIDLookup(context.Background(), db, cfg.Tables.Traces, traceIDColumn, probeSeedTraceID)
	if err != nil {
		t.Fatalf("traceIDLookup(traces): %v", err)
	}
	if !logsFound || !logsIndexUsed {
		t.Errorf("logs: found=%v indexUsed=%v, want both true", logsFound, logsIndexUsed)
	}
	if !tracesFound || !tracesIndexUsed {
		t.Errorf("traces: found=%v indexUsed=%v, want both true", tracesFound, tracesIndexUsed)
	}

	rows, err := db.Query(
		"EXPLAIN indexes = 1 SELECT count() FROM "+cfg.Tables.Traces+" WHERE "+traceIDColumn+" = ?",
		probeSeedTraceID,
	)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan: %v", err)
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if !strings.Contains(plan.String(), idxTraceIDName) {
		t.Errorf("live EXPLAIN indexes=1 plan never mentions %s — index is not actually being consulted:\n%s",
			idxTraceIDName, plan.String())
	}
}

// TestTraceIDIndexProbe_NonCanonicalFormDoesNotMatch is the
// encoding-contract case: ClickHouse String equality is case- and
// width-sensitive, so probing a genuinely-stored canonical trace ID with an
// upper-cased or leading-zero-stripped variant must NOT match. This is the
// executable proof that Tempo's own normaliseTraceID canonicalisation step
// (internal/api/tempo/handler.go) is load-bearing, not decorative —
// removing it would break real lookups, and this test would catch that
// regression. Skipping this case "because the encoding chain was already
// found consistent" would miss the point: the chain is consistent BECAUSE
// every read path (including Tempo's own boundary) agrees to
// produce/expect this exact canonical form, and this test is what pins
// that agreement matters.
func TestTraceIDIndexProbe_NonCanonicalFormDoesNotMatch(t *testing.T) {
	db, cfg := openProbeChDB(t)
	insertLogRow(t, db, cfg.Tables.Logs, probeSeedTraceID)
	insertSpanRow(t, db, cfg.Tables.Traces, probeSeedTraceID)

	upperCased := strings.ToUpper(probeSeedTraceID)
	zeroStripped := strings.TrimLeft(probeSeedTraceID, "0")
	if upperCased == probeSeedTraceID {
		t.Fatalf("test fixture bug: probeSeedTraceID %q has no letters to uppercase", probeSeedTraceID)
	}
	if zeroStripped == probeSeedTraceID {
		t.Fatalf("test fixture bug: probeSeedTraceID %q has no leading zeros to strip", probeSeedTraceID)
	}

	for _, variant := range []struct {
		name string
		id   string
	}{
		{"upper_cased", upperCased},
		{"leading_zeros_stripped", zeroStripped},
	} {
		t.Run(variant.name, func(t *testing.T) {
			result, err := TraceIDIndexProbe(context.Background(), db,
				cfg.Tables.Logs, traceIDColumn, cfg.Tables.Traces, traceIDColumn,
				variant.id)
			if err != nil {
				t.Fatalf("TraceIDIndexProbe(%s): %v", variant.name, err)
			}
			if result.Consistent() {
				t.Errorf("Consistent() = true for non-canonical probe value %q (canonical stored form is %q) — "+
					"want false, ClickHouse String equality must not silently coerce case/width",
					variant.id, probeSeedTraceID)
			}
		})
	}
}
