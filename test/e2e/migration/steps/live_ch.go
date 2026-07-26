package steps

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/tsouza/cerberus/test/e2e/migration/seed"
)

// livePollBudget bounds a direct ClickHouse probe against the Tier-1 stack —
// dialing, or one round-trip read after a write this scenario itself just
// made. The stack `just migration-tier1-up` brings up already waits `--wait`
// on container healthchecks, so this only guards a stale env var left over
// from a torn-down run, or the brief window before a synchronous single-node
// INSERT is visible to a following SELECT.
const livePollBudget = 30 * time.Second

// livePollInterval is the gap between read-back polls.
const livePollInterval = time.Second

// dialLiveCH opens a ClickHouse connection to the Tier-1 stack's database,
// failing clearly when the stack was never established — every direct-CH
// step (MIG-10's schema diff, MIG-12's table-routing and exp-histogram
// probes, MIG-13's recorded-series probe) shares this rather than each
// re-deriving the connection config from w.live.
func (w *World) dialLiveCH(ctx context.Context) (driver.Conn, error) {
	if !w.liveSet {
		return nil, fmt.Errorf("the tier-1 stack has not been established; the scenario must establish it first")
	}
	conn, err := seed.DialCH(ctx, seed.CHConfig{
		Addr:     w.live.CHAddr,
		Database: w.live.CHDatabase,
		Username: w.live.CHUsername,
		Password: w.live.CHPassword,
	})
	if err != nil {
		return nil, fmt.Errorf("dial the live clickhouse: %w", err)
	}
	return conn, nil
}

// tableExistsLive reports whether table exists in database on the live
// ClickHouse the scenario dialed.
func tableExistsLive(ctx context.Context, conn driver.Conn, database, table string) (bool, error) {
	const q = `SELECT count() FROM system.tables WHERE database = ? AND name = ?`
	var n uint64
	if err := conn.QueryRow(ctx, q, database, table).Scan(&n); err != nil {
		return false, fmt.Errorf("probe table %s.%s: %w", database, table, err)
	}
	return n > 0, nil
}

// liveColumnSet returns the real column names ClickHouse holds for table, so
// a caller can diff cerberus's own expected column names against what the
// collector actually created — the set membership check MIG-10's tier-1 half
// needs, without hand-parsing the rendered CREATE statement's SQL text.
func liveColumnSet(ctx context.Context, conn driver.Conn, database, table string) (map[string]struct{}, error) {
	const q = `SELECT name FROM system.columns WHERE database = ? AND table = ?`
	rows, err := conn.Query(ctx, q, database, table)
	if err != nil {
		return nil, fmt.Errorf("list columns for %s.%s: %w", database, table, err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]struct{}{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan column name for %s.%s: %w", database, table, err)
		}
		out[name] = struct{}{}
	}
	return out, rows.Err()
}
