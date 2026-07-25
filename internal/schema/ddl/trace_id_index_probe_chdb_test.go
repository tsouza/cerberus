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
//
// TraceIDIndexProbe/traceIDLookup below are harness-internal proof
// infrastructure consumed ONLY from this test file (no production
// entrypoint calls them), so they live here rather than in a
// non-test-tagged production file — that keeps their fmt.Sprintf-composed
// EXPLAIN/count queries subject to this package's existing test-only raw-SQL
// convention (see the integration-tagged queries in ddl_integration_test.go)
// instead of the typed chsql builder rule that binds production SQL
// emission.
package ddl

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	_ "github.com/chdb-io/chdb-go/chdb/driver"
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

// scanner is the *sql.Row surface traceIDLookup needs.
type scanner interface {
	Scan(dest ...any) error
}

// rowIterator is the *sql.Rows surface traceIDLookup needs.
type rowIterator interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close() error
}

// sqlQuerier abstracts *sql.DB behind the two methods traceIDLookup calls,
// so tests can inject a fake that fails deterministically at each of
// traceIDLookup's four internal error-return points (count-query failure,
// EXPLAIN-query failure, row-scan failure, rows.Err() failure) — branches a
// real ClickHouse backend can't be coaxed into hitting individually without
// timing-dependent trickery. TestTraceIDIndexProbe_PropagatesLookupErrors
// separately proves the two outer TraceIDIndexProbe wrap-branches against a
// real chDB session.
type sqlQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) scanner
	QueryContext(ctx context.Context, query string, args ...any) (rowIterator, error)
}

// stdSQLQuerier adapts *sql.DB to sqlQuerier.
type stdSQLQuerier struct{ *sql.DB }

func (s stdSQLQuerier) QueryRowContext(ctx context.Context, query string, args ...any) scanner {
	return s.DB.QueryRowContext(ctx, query, args...)
}

func (s stdSQLQuerier) QueryContext(ctx context.Context, query string, args ...any) (rowIterator, error) {
	return s.DB.QueryContext(ctx, query, args...)
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
	q := stdSQLQuerier{db}

	var err error
	r.LogsFound, r.LogsIndexUsed, err = traceIDLookup(ctx, q, logsTable, logsTraceIDCol, traceID)
	if err != nil {
		return TraceIDIndexProbeResult{}, fmt.Errorf("ddl: trace_id index probe against %s: %w", logsTable, err)
	}
	r.TracesFound, r.TracesIndexUsed, err = traceIDLookup(ctx, q, tracesTable, tracesTraceIDCol, traceID)
	if err != nil {
		return TraceIDIndexProbeResult{}, fmt.Errorf("ddl: trace_id index probe against %s: %w", tracesTable, err)
	}
	return r, nil
}

// traceIDLookup runs the count-matching lookup and its EXPLAIN indexes=1
// twin against one (table, column) pair, returning whether the trace ID was
// found and whether the plan shows idx_trace_id was consulted.
func traceIDLookup(ctx context.Context, db sqlQuerier, table, col, traceID string) (found, indexUsed bool, err error) {
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

// promoteCreateTable rewrites a rendered production
// `CREATE TABLE IF NOT EXISTS ...` statement into `CREATE OR REPLACE TABLE
// ...` so each subtest's fresh chDB session starts from an empty table
// instead of accumulating rows a prior subtest inserted — the
// bare/idempotent-vs-replace distinction test/property/chdb.go and
// test/spec/runner_chdb.go already draw for chDB re-runnability, applied
// here because the production DDL opts into IF NOT EXISTS (correctly, for a
// real cluster) and the bare form is what these helpers auto-promote.
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

	logsFound, logsIndexUsed, err := traceIDLookup(context.Background(), stdSQLQuerier{db}, cfg.Tables.Logs, traceIDColumn, probeSeedTraceID)
	if err != nil {
		t.Fatalf("traceIDLookup(logs): %v", err)
	}
	tracesFound, tracesIndexUsed, err := traceIDLookup(context.Background(), stdSQLQuerier{db}, cfg.Tables.Traces, traceIDColumn, probeSeedTraceID)
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

// TestTraceIDIndexProbe_PropagatesLookupErrors proves the two error-wrap
// branches inside TraceIDIndexProbe itself (one per table) surface a real
// backend failure rather than folding it into a false "gap" result. Without
// this test, a regression that changed either branch to swallow the error
// (e.g. `return r, nil` instead of `return TraceIDIndexProbeResult{}, err`)
// would make TraceIDIndexProbe report Consistent()==false for a schema/
// connectivity failure exactly as it would for a genuine missing-trace-id
// gap — the two cases must stay distinguishable via a non-nil error.
func TestTraceIDIndexProbe_PropagatesLookupErrors(t *testing.T) {
	db, cfg := openProbeChDB(t)
	const missingTable = "table_that_does_not_exist"

	t.Run("bad logs table", func(t *testing.T) {
		_, err := TraceIDIndexProbe(context.Background(), db,
			missingTable, traceIDColumn, cfg.Tables.Traces, traceIDColumn,
			probeSeedTraceID)
		if err == nil {
			t.Fatal("TraceIDIndexProbe returned a nil error for a nonexistent logs table")
		}
		if !strings.Contains(err.Error(), missingTable) {
			t.Errorf("error %q does not name the failing logs table %q", err.Error(), missingTable)
		}
	})

	t.Run("bad traces table", func(t *testing.T) {
		_, err := TraceIDIndexProbe(context.Background(), db,
			cfg.Tables.Logs, traceIDColumn, missingTable, traceIDColumn,
			probeSeedTraceID)
		if err == nil {
			t.Fatal("TraceIDIndexProbe returned a nil error for a nonexistent traces table")
		}
		if !strings.Contains(err.Error(), missingTable) {
			t.Errorf("error %q does not name the failing traces table %q", err.Error(), missingTable)
		}
	})
}

// fakeScanner and fakeRows below let TestTraceIDLookup_ErrorBranches inject
// a failure at each of traceIDLookup's four internal error-return points
// deterministically — a real ClickHouse backend can make the count query or
// the EXPLAIN query fail together (bad table/column), but can't be coaxed
// into failing the EXPLAIN query specifically while the count query
// succeeds, nor into failing a row-scan or rows.Err() independently.

type fakeScanner struct{ err error }

func (f fakeScanner) Scan(dest ...any) error { return f.err }

type fakeRows struct {
	hasRow  bool
	scanErr error
	errErr  error
	used    bool
}

func (r *fakeRows) Next() bool {
	if !r.hasRow || r.used {
		return false
	}
	r.used = true
	return true
}

func (r *fakeRows) Scan(dest ...any) error { return r.scanErr }
func (r *fakeRows) Err() error             { return r.errErr }
func (r *fakeRows) Close() error           { return nil }

type fakeQuerier struct {
	countErr   error
	explainErr error
	hasRow     bool
	scanErr    error
	rowsErr    error
}

func (f fakeQuerier) QueryRowContext(ctx context.Context, query string, args ...any) scanner {
	return fakeScanner{err: f.countErr}
}

func (f fakeQuerier) QueryContext(ctx context.Context, query string, args ...any) (rowIterator, error) {
	if f.explainErr != nil {
		return nil, f.explainErr
	}
	return &fakeRows{hasRow: f.hasRow, scanErr: f.scanErr, errErr: f.rowsErr}, nil
}

// TestTraceIDLookup_ErrorBranches pins that all four of traceIDLookup's
// internal error-return points actually propagate the underlying error
// (wrapped with distinguishing context) instead of folding it into the
// zero-value (found=false, indexUsed=false, err=nil) result — which would
// be indistinguishable from a genuine "trace ID not found" outcome and
// silently turn a real backend/connectivity failure into a false report of
// a cross-signal correlation gap.
func TestTraceIDLookup_ErrorBranches(t *testing.T) {
	sentinel := errors.New("boom")

	tests := []struct {
		name    string
		q       fakeQuerier
		wantMsg string
	}{
		{
			name:    "count query fails",
			q:       fakeQuerier{countErr: sentinel},
			wantMsg: "count lookup",
		},
		{
			name:    "explain query fails",
			q:       fakeQuerier{explainErr: sentinel},
			wantMsg: "explain:",
		},
		{
			name:    "explain row scan fails",
			q:       fakeQuerier{hasRow: true, scanErr: sentinel},
			wantMsg: "explain scan",
		},
		{
			name:    "explain rows.Err fails",
			q:       fakeQuerier{rowsErr: sentinel},
			wantMsg: "explain rows",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			found, indexUsed, err := traceIDLookup(context.Background(), tt.q, "otel_logs", "TraceId", probeSeedTraceID)
			if err == nil {
				t.Fatalf("traceIDLookup returned a nil error, want one wrapping %q", tt.wantMsg)
			}
			if !errors.Is(err, sentinel) {
				t.Errorf("traceIDLookup error %q does not wrap the injected sentinel error", err.Error())
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("traceIDLookup error %q does not contain expected context %q", err.Error(), tt.wantMsg)
			}
			if found || indexUsed {
				t.Errorf("traceIDLookup on error path returned found=%v indexUsed=%v, want both false", found, indexUsed)
			}
		})
	}
}
