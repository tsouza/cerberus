// Package chclient is a thin wrapper around clickhouse-go/v2 that the API
// layer uses to execute emitted SQL.
package chclient

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Config describes a single ClickHouse connection.
type Config struct {
	Addr     string // host:port, e.g. "clickhouse:9000"
	Database string
	Username string
	Password string

	// DialTimeout caps the initial connection dial. Zero falls back to 5s.
	DialTimeout time.Duration
}

// Client is a stateless wrapper over a clickhouse-go/v2 connection pool.
type Client struct {
	conn driver.Conn
}

// New opens a connection pool to ClickHouse and pings it once to confirm
// reachability. The returned Client is safe for concurrent use.
func New(ctx context.Context, cfg Config) (*Client, error) {
	dial := cfg.DialTimeout
	if dial == 0 {
		dial = 5 * time.Second
	}
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{cfg.Addr},
		Auth: clickhouse.Auth{
			Database: cfg.Database,
			Username: cfg.Username,
			Password: cfg.Password,
		},
		DialTimeout: dial,
	})
	if err != nil {
		return nil, fmt.Errorf("chclient: open: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, dial)
	defer cancel()
	if err := conn.Ping(pingCtx); err != nil {
		return nil, fmt.Errorf("chclient: ping %s: %w", cfg.Addr, err)
	}
	return &Client{conn: conn}, nil
}

// Close releases all pooled connections.
func (c *Client) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// Sample is one row of metrics data returned by Query. It's the shape the
// /api/v1/query and /api/v1/query_range handlers expect — see api/prom.
type Sample struct {
	MetricName string
	Labels     map[string]string
	Timestamp  time.Time
	Value      float64
}

// Exec runs sql with positional args against ClickHouse and returns any
// error. Use for DDL (CREATE TABLE, ...) and DML (INSERT, ...) that don't
// produce a result set.
func (c *Client) Exec(ctx context.Context, sql string, args ...any) error {
	if err := c.conn.Exec(ctx, sql, args...); err != nil {
		return fmt.Errorf("chclient: exec: %w", err)
	}
	return nil
}

// Query runs sql with positional args against ClickHouse and decodes each
// row into a Sample. The SQL must project MetricName, Attributes, TimeUnix,
// Value in that order — Scan binds positionally.
//
// For v0.1 the API layer ensures this projection shape via the chplan
// Project node wrapped around lowered PromQL output.
func (c *Client) Query(ctx context.Context, sql string, args ...any) ([]Sample, error) {
	rows, err := c.conn.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("chclient: query: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	var out []Sample
	for rows.Next() {
		var s Sample
		var labels map[string]string
		if err := rows.Scan(&s.MetricName, &labels, &s.Timestamp, &s.Value); err != nil {
			return nil, fmt.Errorf("chclient: scan: %w", err)
		}
		s.Labels = labels
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chclient: rows.Err: %w", err)
	}
	return out, nil
}

// QueryStrings runs sql and decodes a single-string-column result into a
// flat slice. Used by metadata endpoints (/api/v1/labels, label values,
// metadata) that return a list of names.
func (c *Client) QueryStrings(ctx context.Context, sql string, args ...any) ([]string, error) {
	rows, err := c.conn.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("chclient: query: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("chclient: scan: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chclient: rows.Err: %w", err)
	}
	return out, nil
}

// MetricMetaRow is one row from the metadata-discovery query — a metric
// name plus its OTel description and unit text and the cerberus-derived
// Prom-style type (gauge / counter / histogram).
type MetricMetaRow struct {
	Name        string
	Description string
	Unit        string
	Type        string
}

// QueryMetricMeta runs sql and decodes each row as a (name, description,
// unit) triple. The caller supplies the `metricType` (gauge / counter /
// histogram) since the table the row came from determines that — the SQL
// itself only returns the OTel columns.
func (c *Client) QueryMetricMeta(ctx context.Context, sql, metricType string, args ...any) ([]MetricMetaRow, error) {
	rows, err := c.conn.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("chclient: query: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	var out []MetricMetaRow
	for rows.Next() {
		var r MetricMetaRow
		r.Type = metricType
		if err := rows.Scan(&r.Name, &r.Description, &r.Unit); err != nil {
			return nil, fmt.Errorf("chclient: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chclient: rows.Err: %w", err)
	}
	return out, nil
}

// IndexStatsRow is the single aggregate row returned by the Loki
// /loki/api/v1/index/stats SQL — counts of distinct streams, log entries
// and total byte volume (sum(length(Body))) for the matched selector.
//
// Cerberus has no chunk model (it's sample-backed, not chunk-backed), so
// the chunks count is reported as 0 by the API handler — it is not part
// of this struct.
type IndexStatsRow struct {
	Streams uint64
	Entries uint64
	Bytes   uint64
}

// QueryIndexStats runs sql expecting a single row of three UInt64
// aggregates (streams, entries, bytes) and decodes it. An empty result
// set is treated as the all-zeros row.
func (c *Client) QueryIndexStats(ctx context.Context, sql string, args ...any) (IndexStatsRow, error) {
	rows, err := c.conn.Query(ctx, sql, args...)
	if err != nil {
		return IndexStatsRow{}, fmt.Errorf("chclient: query: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	var out IndexStatsRow
	if rows.Next() {
		if err := rows.Scan(&out.Streams, &out.Entries, &out.Bytes); err != nil {
			return IndexStatsRow{}, fmt.Errorf("chclient: scan: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return IndexStatsRow{}, fmt.Errorf("chclient: rows.Err: %w", err)
	}
	return out, nil
}

// IndexVolumeRow is one grouped (label-set, bytes) tuple from the Loki
// /loki/api/v1/index/volume SQL. The label set is the GROUP BY key — by
// default the full ResourceAttributes map, or a filtered subset when the
// caller supplied `targetLabels`.
type IndexVolumeRow struct {
	Labels map[string]string
	Bytes  uint64
}

// QueryIndexVolume runs sql expecting rows of (Map(String,String),
// UInt64) and decodes them into IndexVolumeRow.
func (c *Client) QueryIndexVolume(ctx context.Context, sql string, args ...any) ([]IndexVolumeRow, error) {
	rows, err := c.conn.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("chclient: query: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	var out []IndexVolumeRow
	for rows.Next() {
		var r IndexVolumeRow
		var labels map[string]string
		if err := rows.Scan(&labels, &r.Bytes); err != nil {
			return nil, fmt.Errorf("chclient: scan: %w", err)
		}
		r.Labels = labels
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chclient: rows.Err: %w", err)
	}
	return out, nil
}

// QueryLabelSets runs sql and decodes each row into a Map(String,String)
// label set. Used by /api/v1/series.
func (c *Client) QueryLabelSets(ctx context.Context, sql string, args ...any) ([]map[string]string, error) {
	rows, err := c.conn.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("chclient: query: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	var out []map[string]string
	for rows.Next() {
		var m map[string]string
		if err := rows.Scan(&m); err != nil {
			return nil, fmt.Errorf("chclient: scan: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chclient: rows.Err: %w", err)
	}
	return out, nil
}
