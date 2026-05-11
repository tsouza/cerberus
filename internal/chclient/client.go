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
