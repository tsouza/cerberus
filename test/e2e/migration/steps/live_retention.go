package steps

import (
	"context"
	"fmt"
	"time"

	"github.com/tsouza/cerberus/test/e2e/migration/seed"
)

// liveRetentionQuery reads every table's own CREATE statement out of
// ClickHouse's system catalogue, over the wire — the same text `SHOW CREATE
// TABLE` renders. This is the tier-1 half's whole point: MIG-14's and
// MIG-26's Tier-0 halves both read the retention `cerberus migrate schema`
// would RENDER (see givenConfiguredRetention in then_lookback.go); this reads
// what the live cluster ACTUALLY provisions, which the schema-render text can
// drift from the moment an operator (or a different collector version) applies
// DDL cerberus never rendered.
const liveRetentionQuery = "SELECT create_table_query FROM system.tables WHERE database = ?"

// liveCHDialBudget bounds the connection this step opens purely to read
// retention; the compose healthcheck already gates on the cluster answering
// `SELECT 1`, so a dial slower than this is a real failure, not a cold start.
const liveCHDialBudget = 15 * time.Second

// readLiveRetention dials the tier-1 stack's live ClickHouse and reads the
// per-signal retention it actually provisions, keeping the shortest TTL per
// signal — exactly what retentionBySignal already does for the offline
// (rendered-text) half, reused here unchanged against a different text
// source. The live text is the COLLECTOR's DDL, not cerberus's own render;
// ttlClauseRe reads both dialects precisely so this reuse is sound.
//
// A signal whose tables carry no TTL clause at all is absent from the result
// — never coerced to zero or read as "unbounded" — so a caller comparing
// against a mandate fails closed exactly as thenRetentionCoversLookback
// already does for MIG-14. A read that establishes NO signal's retention is an
// error rather than an empty map: callers spell "absent" as a refusal, so an
// empty map would let a total parse failure be reported as a refusal the
// operator earned.
func (w *World) readLiveRetention(ctx context.Context) (map[string]time.Duration, error) {
	if !w.liveSet {
		return nil, fmt.Errorf("the tier-1 stack has not been established; the scenario must establish it first")
	}
	dialCtx, cancel := context.WithTimeout(ctx, liveCHDialBudget)
	defer cancel()
	conn, err := seed.DialCH(dialCtx, seed.CHConfig{
		Addr:     w.live.CHAddr,
		Database: w.live.CHDatabase,
		Username: w.live.CHUsername,
		Password: w.live.CHPassword,
	})
	if err != nil {
		return nil, fmt.Errorf("migration harness: dial the live clickhouse to read retention: %w", err)
	}
	defer func() { _ = conn.Close() }()

	rows, err := conn.Query(ctx, liveRetentionQuery, w.live.CHDatabase)
	if err != nil {
		return nil, fmt.Errorf("migration harness: read system.tables: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var stmts []string
	for rows.Next() {
		var ddl string
		if err := rows.Scan(&ddl); err != nil {
			return nil, fmt.Errorf("migration harness: scan a system.tables row: %w", err)
		}
		stmts = append(stmts, ddl)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("migration harness: iterate system.tables: %w", err)
	}
	if len(stmts) == 0 {
		return nil, fmt.Errorf("migration harness: the live clickhouse database %q holds no tables at all", w.live.CHDatabase)
	}
	return retentionBySignal("the live clickhouse database "+w.live.CHDatabase, stmts)
}

// requireLiveSignalRetention reads the live cluster's retention and returns
// what it provisions for signal, failing when the read establishes nothing for
// that signal.
//
// It exists so no caller can spell "we could not read a retention" and "the
// retention we read does not cover what is asked of it" the same way. The
// second is a verdict an operator earned; the first is a harness that proved
// nothing, and a gate deciding on it is a gate with no evidence behind it.
// Every derived assertion goes through here first, so a scenario that asserts
// a refusal has already asserted the refusal rests on a real, positive number.
func (w *World) requireLiveSignalRetention(ctx context.Context, signal string) (time.Duration, error) {
	bySignal, err := w.readLiveRetention(ctx)
	if err != nil {
		return 0, err
	}
	have, ok := bySignal[signal]
	if !ok || have <= 0 {
		return 0, fmt.Errorf(
			"migration harness: the live clickhouse provisions no %s retention at all, so nothing compared against it has been decided either way",
			signal,
		)
	}
	return have, nil
}
