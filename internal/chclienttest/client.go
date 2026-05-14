//go:build chdb

package chclienttest

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	_ "github.com/chdb-io/chdb-go/chdb/driver" // registers "chdb" sql driver

	"github.com/tsouza/cerberus/internal/chclient"
)

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
func NewChDB(t *testing.T) *Client {
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
func (c *Client) Seed(t *testing.T, ddl string) {
	t.Helper()
	if c.db == nil {
		t.Fatalf("chclienttest: Seed called on error-only client")
	}
	for _, stmt := range splitStatements(ddl) {
		if isBlank(stmt) {
			continue
		}
		stmt = promoteCreateTable(stmt)
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
	rewritten := rewriteMapProjections(query)
	rows, err := c.db.QueryContext(ctx, rewritten, args...)
	if err != nil {
		return nil, fmt.Errorf("chclienttest: query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []chclient.Sample
	for rows.Next() {
		var (
			name      string
			attrsJSON string
			ts        time.Time
			value     float64
		)
		if err := rows.Scan(&name, &attrsJSON, &ts, &value); err != nil {
			return nil, fmt.Errorf("chclienttest: scan: %w", err)
		}
		labels, err := decodeMapJSON(attrsJSON)
		if err != nil {
			return nil, err
		}
		out = append(out, chclient.Sample{
			MetricName: name,
			Labels:     labels,
			Timestamp:  ts,
			Value:      value,
		})
	}
	if err := tolerantRowsErr(rows.Err()); err != nil {
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
	rows, err := c.db.QueryContext(ctx, query, args...)
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
	if err := tolerantRowsErr(rows.Err()); err != nil {
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
	rewritten := rewriteMapProjections(query)
	rows, err := c.db.QueryContext(ctx, rewritten, args...)
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
	if err := tolerantRowsErr(rows.Err()); err != nil {
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
	rows, err := c.db.QueryContext(ctx, query, args...)
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
	if err := tolerantRowsErr(rows.Err()); err != nil {
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
	rows, err := c.db.QueryContext(ctx, query, args...)
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
	if err := tolerantRowsErr(rows.Err()); err != nil {
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
	rewritten := rewriteMapProjections(query)
	rows, err := c.db.QueryContext(ctx, rewritten, args...)
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
	if err := tolerantRowsErr(rows.Err()); err != nil {
		return nil, fmt.Errorf("chclienttest: rows.Err: %w", err)
	}
	return out, nil
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
