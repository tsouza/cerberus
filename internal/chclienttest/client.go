//go:build chdb

package chclienttest

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/chdb-io/chdb-go/chdb/driver" // registers "chdb" sql driver

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/testsql"
)

// metadataColumn is the trailing projection alias the Loki log-stream
// path appends to carry per-row structured metadata. It mirrors the
// production rowsCursor's probe constant so this test double binds the
// same five- vs four-destination scan off the result-set column shape.
const metadataColumn = "Metadata"

// Client is a chDB-backed implementation of the Querier interface each
// handler defines (api/prom.Querier, api/loki.Querier, api/tempo.Querier).
// All three are subsets of *chclient.Client's surface, so a single struct
// satisfies them all by method set.
//
// Each Client owns one ephemeral chDB session — empty DSN, temp-dir
// backed — torn down via t.Cleanup when NewChDB returns. Concurrent
// use across goroutines is safe (database/sql is goroutine-safe), but
// tests typically don't need that.
//
// To inject upstream-error behaviour for negative-path tests use
// NewChDBWithError instead — that variant returns the stored error
// from every Querier method without opening a chDB session.
type Client struct {
	db  *sql.DB
	err error // when non-nil every Querier method returns this and bypasses db
}

// NewChDB opens an ephemeral chDB session bound to t's lifetime and
// returns a Client that satisfies the prom / loki / tempo Querier
// interfaces. Each test gets an isolated session — there is no
// process-wide shared state.
//
// A test that asserts a rejection followed by a success on the SAME
// session (the guard tests, which raise through throwIf) does not need
// to split that into two sessions. chDB hands results back as Parquet,
// and chdb-go v1.12.0 emits an undecodable page index for the first
// query that SUCCEEDS on a session where an earlier query raised a
// ClickHouse exception; parquet-go v0.30.1 panics inside NewGenericReader
// rather than returning an error (issue #1917). queryContext (see below)
// contains this at the source: it recovers the panic as a plain error
// and, after any query error, flushes the session with a trivial Exec
// that empirically prevents the corruption from reaching the next
// query — proven by TestSessionSurvivesErrorThenSuccess in
// session_recovery_test.go. Opening a fresh session per phase (as
// several existing chdb-tagged tests do) still works and remains
// harmless, just no longer necessary. Real ClickHouse speaks the native
// protocol and has no such coupling, so this binds the chdb lane only.
func NewChDB(t testing.TB) *Client {
	t.Helper()
	// Empty DSN -> chdb-go provisions a temp-dir-backed session that
	// the driver tears down on Close. There is no `:memory:` literal
	// in chdb-go v1.11.0; the temp-dir behaviour is functionally
	// equivalent for unit-test isolation.
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("chclienttest: open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("chclienttest: ping chdb: %v", err)
	}
	return &Client{db: db}
}

// NewChDBWithError returns a Client whose Querier methods all return
// err. Use it for upstream-error negative-path tests where exercising
// real CH would be cumbersome (e.g. simulating a connection refused).
// The returned Client never opens a chDB session.
func NewChDBWithError(_ *testing.T, err error) *Client {
	if err == nil {
		err = fmt.Errorf("chclienttest: injected error")
	}
	return &Client{err: err}
}

// queryContext is the single choke point every Querier method routes
// its QueryContext call through. It exists to contain issue #1917: a
// chDB session that raised a ClickHouse exception corrupts the Parquet
// page index of the NEXT query's result set, and chdb-go v1.12.0 /
// parquet-go v0.30.1 (both current as of the fix) still exhibit it —
// parquet-go panics inside NewGenericReader decoding the corrupted
// index instead of returning an error.
//
// Two independent layers address it:
//
//  1. flushSessionAfterError: measured empirically, issuing a trivial
//     ExecContext (a statement that never goes through the Parquet
//     decode path at all — Exec, not Query) on the SAME session
//     immediately after ANY query error reliably prevents the
//     corruption from reaching the next query. This is the real fix:
//     it is what makes the NEXT query come back with a genuine 200
//     instead of a spurious failure, which a recover() alone cannot
//     do (recover only stops a crash; it can't manufacture the correct
//     result). Best-effort: the flush's own error is discarded, since
//     if it fails there is nothing more we can do here and the
//     caller's original error is already on its way.
//  2. The recover in safeQueryContext converts the parquet-go panic
//     into a normal Go error rather than letting it unwind past this
//     package into the panic-recovery middleware, where it would read
//     as a genuine handler regression. This is defense-in-depth for
//     any sequence the flush does not fully cover (multiple pooled
//     connections, a caller that bypasses Seed) — it guarantees a
//     *caller-visible error*, never a guarantee of a correct result;
//     recovering a panic cannot undo corrupted bytes that already
//     decoded wrong.
//
// Both are test-harness-only mitigations for a third-party decoder
// that must not assume its input is well-formed; they narrowly recover
// from ONE documented panic site and never suppress a genuine cerberus
// bug — a real handler regression still surfaces as its own error or
// wrong-shaped response, not as this recovered panic.
func (c *Client) queryContext(ctx context.Context, queryText string, args ...any) (*sql.Rows, error) {
	rows, err := safeQueryContext(ctx, c.db, queryText, args...)
	if err != nil {
		flushSessionAfterError(ctx, c.db)
	}
	return rows, err
}

// safeQueryContext runs db.QueryContext and converts a parquet-go
// decode panic (issue #1917) into a plain error. The panic surfaces
// synchronously from inside QueryContext itself (chdb-go's PARQUET
// driver decodes the page index eagerly, before returning rows), so
// wrapping this one call site is sufficient — there is no rows object
// in flight yet when the panic fires.
func safeQueryContext(ctx context.Context, db *sql.DB, queryText string, args ...any) (rows *sql.Rows, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("chclienttest: recovered chdb/parquet-go decode panic on corrupted "+
				"Parquet page index (see #1917): %v", r)
		}
	}()
	return db.QueryContext(ctx, queryText, args...)
}

// flushSessionAfterError issues a trivial statement on db that never
// touches the Parquet decode path (Exec, not Query). Measured
// empirically against chdb-go v1.12.0 / parquet-go v0.30.1: this is
// what actually prevents issue #1917's corruption from reaching the
// next query on the same session. Best-effort — its own error is
// discarded because the caller's real error from the query that
// triggered it is already the one being returned.
func flushSessionAfterError(ctx context.Context, db *sql.DB) {
	_, _ = db.ExecContext(ctx, "SELECT 1")
}

// Seed runs ddl (a multi-statement script of `CREATE …; INSERT …;` etc)
// against the underlying chDB session. The runner splits on top-level
// semicolons so each statement reaches chdb-go's single-statement
// Exec individually. Empty statements are skipped. Any failure fails
// the test fatally — seeding is a test-setup concern, not an
// assertable behaviour.
//
// Cross-test isolation: chdb-go shares one engine across a process, so
// CREATE TABLE statements from a prior test would collide with this
// one's seed (chdb-go v1.11.0 has no `:memory:` flavour that resets
// per-Open — every connection lands in the same shared catalog). The
// seed-applier therefore promotes bare `CREATE TABLE` to `CREATE OR
// REPLACE TABLE` so each test's setup is idempotent against whatever
// the prior test left behind. Authors who want the upstream semantics
// can opt out by writing `CREATE OR REPLACE TABLE` / `CREATE TABLE IF
// NOT EXISTS` themselves — the rewrite only fires on the bare form.
func (c *Client) Seed(t testing.TB, ddl string) {
	t.Helper()
	if c.db == nil {
		t.Fatalf("chclienttest: Seed called on error-only client")
	}
	for _, stmt := range testsql.BackfillMetricsColumns(testsql.SplitStatements(ddl)) {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		stmt = testsql.PromoteCreateTable(stmt)
		if _, err := c.db.Exec(stmt); err != nil {
			t.Fatalf("chclienttest: seed exec failed:\n--- stmt ---\n%s\n--- err ---\n%v", stmt, err)
		}
	}
}

// Query satisfies the *chclient.Client.Query surface — it runs sql
// with positional args and decodes each row into a chclient.Sample.
// The SQL must project (MetricName, Attributes, TimeUnix, Value) in
// that order; the Attributes column is rewritten to toJSONString(…)
// before the round-trip and JSON-decoded back to a map[string]string
// on the Go side.
func (c *Client) Query(ctx context.Context, query string, args ...any) ([]chclient.Sample, error) {
	if c.err != nil {
		return nil, c.err
	}
	rewritten := withQuerySettings(ctx, testsql.RewriteMapProjections(query))
	rows, err := c.queryContext(ctx, rewritten, args...)
	if err != nil {
		return nil, fmt.Errorf("chclienttest: query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// Mirror the production rowsCursor metadata probe: the Loki log-stream
	// projection appends a fifth `Metadata` column (a `toJSONString(<Map>)`
	// JSON-object String), every other path projects four. Bind the scan
	// to the column shape so the four-column metric / prom / tempo paths
	// stay a four-destination scan and the log-stream path picks up the
	// structured-metadata string.
	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("chclienttest: columns: %w", err)
	}
	hasMetadata := len(cols) > 0 && cols[len(cols)-1] == metadataColumn

	var out []chclient.Sample
	for rows.Next() {
		var (
			name         string
			attrsJSON    string
			ts           time.Time
			value        float64
			metadataJSON string
		)
		if hasMetadata {
			if err := rows.Scan(&name, &attrsJSON, &ts, &value, &metadataJSON); err != nil {
				return nil, fmt.Errorf("chclienttest: scan: %w", err)
			}
		} else if err := rows.Scan(&name, &attrsJSON, &ts, &value); err != nil {
			return nil, fmt.Errorf("chclienttest: scan: %w", err)
		}
		labels, err := decodeMapJSON(attrsJSON)
		if err != nil {
			return nil, err
		}
		metadata, err := decodeMapJSON(metadataJSON)
		if err != nil {
			return nil, err
		}
		out = append(out, chclient.Sample{
			MetricName: name,
			Labels:     labels,
			Timestamp:  ts,
			Value:      value,
			Metadata:   metadata,
		})
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		return nil, fmt.Errorf("chclienttest: rows.Err: %w", err)
	}
	return out, nil
}

// QueryCursor returns a streaming chclient.Cursor over the result
// set. Internally this drains the underlying database/sql rows into a
// slice and returns a slice-backed cursor — chdb-go does not surface
// the same Rows lifetime guarantees clickhouse-go does, and handler
// tests don't exercise the streaming-memory contract anyway (the
// allocation benchmark lives in a separate fixture).
func (c *Client) QueryCursor(ctx context.Context, query string, args ...any) (chclient.Cursor, error) {
	if c.err != nil {
		return nil, c.err
	}
	samples, err := c.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return newSliceCursor(samples), nil
}

// QueryStrings runs sql and decodes a single-string-column result.
// No Map column is involved so the SQL is passed through verbatim.
func (c *Client) QueryStrings(ctx context.Context, query string, args ...any) ([]string, error) {
	if c.err != nil {
		return nil, c.err
	}
	rows, err := c.queryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("chclienttest: query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("chclienttest: scan: %w", err)
		}
		out = append(out, s)
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		return nil, fmt.Errorf("chclienttest: rows.Err: %w", err)
	}
	return out, nil
}

// QueryDetectedFieldRows runs sql and decodes a (String, Map, Map, Map,
// Map) five-column result set into chclient.DetectedFieldRow tuples.
// Used by /loki/api/v1/detected_fields and
// /loki/api/v1/detected_field/{name}/values: the line, its structured
// metadata, its stream labels, and the `| logfmt` / `| json`
// parser-stage extractions ClickHouse evaluated for it. The four Map
// columns are wrapped server-side in toJSONString(...) by
// rewriteMapProjections and decoded back on the Go side per the chDB
// driver Map-panic probe.
func (c *Client) QueryDetectedFieldRows(ctx context.Context, query string, args ...any) ([]chclient.DetectedFieldRow, error) {
	if c.err != nil {
		return nil, c.err
	}
	rewritten := testsql.RewriteMapProjections(query)
	rows, err := c.queryContext(ctx, rewritten, args...)
	if err != nil {
		return nil, fmt.Errorf("chclienttest: query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []chclient.DetectedFieldRow
	for rows.Next() {
		var (
			line         string
			attrsJSON    string
			resourceJSON string
			logfmtJSON   string
			jsonJSON     string
		)
		if err := rows.Scan(&line, &attrsJSON, &resourceJSON, &logfmtJSON, &jsonJSON); err != nil {
			return nil, fmt.Errorf("chclienttest: scan: %w", err)
		}
		attrs, err := decodeMapJSON(attrsJSON)
		if err != nil {
			return nil, err
		}
		resource, err := decodeMapJSON(resourceJSON)
		if err != nil {
			return nil, err
		}
		logfmtFields, err := decodeMapJSON(logfmtJSON)
		if err != nil {
			return nil, err
		}
		jsonFields, err := decodeMapJSON(jsonJSON)
		if err != nil {
			return nil, err
		}
		out = append(out, chclient.DetectedFieldRow{
			Line:         line,
			Attributes:   attrs,
			Resource:     resource,
			LogfmtFields: logfmtFields,
			JSONFields:   jsonFields,
		})
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		return nil, fmt.Errorf("chclienttest: rows.Err: %w", err)
	}
	return out, nil
}

// QueryTimestampedLines runs sql and decodes a (DateTime64, String,
// String) three-column result set into chclient.TimestampedLine tuples.
// Used by /loki/api/v1/patterns to feed the drain template miner.
func (c *Client) QueryTimestampedLines(ctx context.Context, query string, args ...any) ([]chclient.TimestampedLine, error) {
	if c.err != nil {
		return nil, c.err
	}
	rows, err := c.queryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("chclienttest: query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []chclient.TimestampedLine
	for rows.Next() {
		var (
			ts       time.Time
			body     string
			severity string
		)
		if err := rows.Scan(&ts, &body, &severity); err != nil {
			return nil, fmt.Errorf("chclienttest: scan: %w", err)
		}
		out = append(out, chclient.TimestampedLine{Timestamp: ts, Body: body, Severity: severity})
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		return nil, fmt.Errorf("chclienttest: rows.Err: %w", err)
	}
	return out, nil
}

// QueryExemplars runs sql expecting the 8-column shape EmitQueryExemplars
// projects (MetricName, Attributes, ServiceName, Timestamp, Value,
// TraceID, SpanID, ExemplarAttributes). Used by /api/v1/query_exemplars.
// Map(String,String) columns are wrapped server-side in toJSONString(...)
// by rewriteMapProjections and decoded back on the Go side per the chDB
// driver Map-panic probe.
func (c *Client) QueryExemplars(ctx context.Context, query string, args ...any) ([]chclient.ExemplarRow, error) {
	if c.err != nil {
		return nil, c.err
	}
	rewritten := testsql.RewriteMapProjections(query)
	rows, err := c.queryContext(ctx, rewritten, args...)
	if err != nil {
		return nil, fmt.Errorf("chclienttest: query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []chclient.ExemplarRow
	for rows.Next() {
		var (
			r           chclient.ExemplarRow
			attrsJSON   string
			exAttrsJSON string
		)
		if err := rows.Scan(
			&r.MetricName,
			&attrsJSON,
			&r.ServiceName,
			&r.Timestamp,
			&r.Value,
			&r.TraceID,
			&r.SpanID,
			&exAttrsJSON,
		); err != nil {
			return nil, fmt.Errorf("chclienttest: scan: %w", err)
		}
		attrs, err := decodeMapJSON(attrsJSON)
		if err != nil {
			return nil, err
		}
		exAttrs, err := decodeMapJSON(exAttrsJSON)
		if err != nil {
			return nil, err
		}
		r.Attributes = attrs
		r.ExemplarAttributes = exAttrs
		out = append(out, r)
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		return nil, fmt.Errorf("chclienttest: rows.Err: %w", err)
	}
	return out, nil
}

// QueryLabelSets runs sql expecting a single Map(String,String) column
// per row. The column is rewritten to toJSONString(…) and decoded back
// on the Go side.
func (c *Client) QueryLabelSets(ctx context.Context, query string, args ...any) ([]map[string]string, error) {
	if c.err != nil {
		return nil, c.err
	}
	rewritten := testsql.RewriteMapProjections(query)
	rows, err := c.queryContext(ctx, rewritten, args...)
	if err != nil {
		return nil, fmt.Errorf("chclienttest: query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []map[string]string
	for rows.Next() {
		var attrsJSON string
		if err := rows.Scan(&attrsJSON); err != nil {
			return nil, fmt.Errorf("chclienttest: scan: %w", err)
		}
		labels, err := decodeMapJSON(attrsJSON)
		if err != nil {
			return nil, err
		}
		out = append(out, labels)
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		return nil, fmt.Errorf("chclienttest: rows.Err: %w", err)
	}
	return out, nil
}

// QueryMetricMeta runs sql expecting (name, description, unit) string
// triples per row. metricType is stamped onto every returned row, the
// same convention chclient.Client.QueryMetricMeta uses (the metric
// type is a property of the table the row came from, not the row).
func (c *Client) QueryMetricMeta(
	ctx context.Context, query, metricType string, args ...any,
) ([]chclient.MetricMetaRow, error) {
	if c.err != nil {
		return nil, c.err
	}
	rows, err := c.queryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("chclienttest: query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []chclient.MetricMetaRow
	for rows.Next() {
		var r chclient.MetricMetaRow
		r.Type = metricType
		if err := rows.Scan(&r.Name, &r.Description, &r.Unit); err != nil {
			return nil, fmt.Errorf("chclienttest: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		return nil, fmt.Errorf("chclienttest: rows.Err: %w", err)
	}
	return out, nil
}

// QueryIndexStats runs sql expecting one (streams, entries, bytes) row.
// Loki /index/stats consumes this; the prom head doesn't but the
// signature is here so a single Client covers all three handlers.
func (c *Client) QueryIndexStats(ctx context.Context, query string, args ...any) (chclient.IndexStatsRow, error) {
	if c.err != nil {
		return chclient.IndexStatsRow{}, c.err
	}
	rows, err := c.queryContext(ctx, query, args...)
	if err != nil {
		return chclient.IndexStatsRow{}, fmt.Errorf("chclienttest: query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out chclient.IndexStatsRow
	if rows.Next() {
		if err := rows.Scan(&out.Streams, &out.Entries, &out.Bytes); err != nil {
			return chclient.IndexStatsRow{}, fmt.Errorf("chclienttest: scan: %w", err)
		}
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		return chclient.IndexStatsRow{}, fmt.Errorf("chclienttest: rows.Err: %w", err)
	}
	return out, nil
}

// QueryIndexVolume runs sql expecting (Map(String,String), UInt64)
// rows. Same Map-column rewrite as QueryLabelSets.
func (c *Client) QueryIndexVolume(ctx context.Context, query string, args ...any) ([]chclient.IndexVolumeRow, error) {
	if c.err != nil {
		return nil, c.err
	}
	rewritten := testsql.RewriteMapProjections(query)
	rows, err := c.queryContext(ctx, rewritten, args...)
	if err != nil {
		return nil, fmt.Errorf("chclienttest: query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []chclient.IndexVolumeRow
	for rows.Next() {
		var (
			attrsJSON string
			bytes     uint64
		)
		if err := rows.Scan(&attrsJSON, &bytes); err != nil {
			return nil, fmt.Errorf("chclienttest: scan: %w", err)
		}
		labels, err := decodeMapJSON(attrsJSON)
		if err != nil {
			return nil, err
		}
		out = append(out, chclient.IndexVolumeRow{Labels: labels, Bytes: bytes})
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		return nil, fmt.Errorf("chclienttest: rows.Err: %w", err)
	}
	return out, nil
}

// withQuerySettings appends a `SETTINGS k=v, …` clause for any per-query
// ClickHouse settings the engine stamped onto ctx via
// chclient.WithQuerySetting — most notably
// allow_experimental_time_series_aggregate_functions=1, which the engine
// attaches whenever the optimized plan carries a chplan.RangeWindowNative
// node (the native timeSeriesRateToGrid family).
//
// Production's *chclient.Client applies these through the clickhouse-go
// protocol settings map; the chdb-go database/sql driver has no protocol
// settings hook, so the faithful equivalent is the SQL-level SETTINGS
// clause chDB honours identically. Without it a native-rate plan fails in
// chDB with "Aggregate function timeSeriesRateToGrid is experimental and
// disabled by default" (UNKNOWN_AGGREGATE_FUNCTION) — the exact gate
// production satisfies via the protocol setting. The clause is a no-op for
// every query that carries no stamped setting (the common case), so the
// non-native paths are byte-unchanged.
func withQuerySettings(ctx context.Context, sql string) string {
	settings := chclient.QuerySettingsFromContext(ctx)
	if len(settings) == 0 {
		return sql
	}
	keys := make([]string, 0, len(settings))
	for k := range settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(sql)
	b.WriteString(" SETTINGS ")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(k)
		b.WriteByte('=')
		fmt.Fprintf(&b, "%v", settings[k])
	}
	return b.String()
}

// decodeMapJSON unmarshals the toJSONString(…) output. An empty
// payload (Map() literal) decodes to nil to match clickhouse-go's
// behaviour on an empty Map.
func decodeMapJSON(s string) (map[string]string, error) {
	if s == "" || s == "{}" {
		return nil, nil
	}
	out := map[string]string{}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("chclienttest: decode map %q: %w", s, err)
	}
	return out, nil
}
