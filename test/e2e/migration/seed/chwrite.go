// Package seed holds the write clients for the Tier-1 migration substrate:
// one ClickHouse connection helper plus one writer per reference backend
// (Prometheus, Loki, Tempo). Every writer takes the same shape — a plain Go
// fixture value in, a wire-format push out — so a single in-memory fixture can
// be written to both sides of a parity diff and the two sides are comparable
// by construction rather than by luck.
//
// Schema authority is deliberately absent from this package: nothing here
// creates a ClickHouse table. The OTel collector's clickhouseexporter
// (create_schema: true) is the sole schema authority in the Tier-1 stack, so
// callers WAIT for its tables with WaitForTables before writing. A cerberus-DDL
// create path here would mint cerberus-named tables and mask a real drift
// between the exporter's names and cerberus's read-side defaults.
package seed

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// CHConfig is the ClickHouse connection input set. It mirrors the env contract
// the other seeders in the tree use (CH_ADDR / CH_DATABASE / CH_USERNAME /
// CH_PASSWORD) without reading the environment itself, so callers stay in
// control of resolution order.
type CHConfig struct {
	Addr     string
	Database string
	Username string
	Password string
}

// chDialTimeout bounds the initial ClickHouse handshake. The stack's compose
// healthcheck already gates on `SELECT 1` answering, so a dial that takes
// longer than this is a real failure, not a cold start.
const chDialTimeout = 10 * time.Second

// DialCH opens a native-protocol connection and proves it answers before
// returning, so a caller never holds a handle that has not completed a
// round-trip.
func DialCH(ctx context.Context, cfg CHConfig) (driver.Conn, error) {
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{cfg.Addr},
		Auth: clickhouse.Auth{
			Database: cfg.Database,
			Username: cfg.Username,
			Password: cfg.Password,
		},
		DialTimeout: chDialTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("open clickhouse %s: %w", cfg.Addr, err)
	}
	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ping clickhouse %s: %w", cfg.Addr, err)
	}
	return conn, nil
}

// ExporterTables are the OTel tables the collector's clickhouseexporter
// provisions and the migration lane reads back through cerberus. The exporter
// creates them eagerly at start(), so on a warm stack the first existence poll
// returns immediately; the wait only absorbs a cold boot where a caller races
// ahead of the exporter.
var ExporterTables = []string{
	"otel_metrics_gauge",
	"otel_metrics_sum",
	"otel_metrics_histogram",
	"otel_metrics_exponential_histogram",
	"otel_logs",
	"otel_traces",
}

// tableWaitPoll is the gap between table-existence polls.
const tableWaitPoll = 2 * time.Second

// WaitForTables blocks until every table exists in database, or timeout
// elapses (or ctx is cancelled). On timeout it names the tables still missing,
// so a failure points straight at the stuck creator instead of surfacing later
// as an opaque UNKNOWN_TABLE from an INSERT.
func WaitForTables(ctx context.Context, conn driver.Conn, database string, tables []string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(tableWaitPoll)
	defer ticker.Stop()
	for {
		missing, err := MissingTables(ctx, conn, database, tables)
		if err != nil {
			return err
		}
		if len(missing) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf(
				"timed out after %s waiting for the collector to create tables in %s: %v",
				timeout, database, missing,
			)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// MissingTables returns the members of tables that do not yet exist in
// database. The query is static with both names bound as parameters — no SQL
// string interpolation.
func MissingTables(ctx context.Context, conn driver.Conn, database string, tables []string) ([]string, error) {
	const existsSQL = "SELECT count() FROM system.tables WHERE database = ? AND name = ?"
	var missing []string
	for _, t := range tables {
		var n uint64
		if err := conn.QueryRow(ctx, existsSQL, database, t).Scan(&n); err != nil {
			return nil, fmt.Errorf("introspect table %s: %w", t, err)
		}
		if n == 0 {
			missing = append(missing, t)
		}
	}
	return missing, nil
}
