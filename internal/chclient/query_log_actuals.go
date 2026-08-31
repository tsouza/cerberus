package chclient

import (
	"context"
	"fmt"
	"time"
)

// query_log_actuals.go closes issue #2789's batch/fallback actuals source:
// system.query_log, read for the queries the native-protocol packet fast
// path (progress.go) could not observe — a query that failed before
// completing (a partial dispatch never reaches flush()'s success path), or
// a deployment mode where packet capture is not wired. internal/engine's
// QueryLogActualsReconciler is the polling half; this file is the transport
// half, mirroring explain_estimate.go's own "internal/chclient/X.go is the
// transport half, internal/engine/X_wiring.go is the engine-side half" split.
//
// The column list is VERIFIED against a live ClickHouse 26.6 server (via
// `SELECT name, type FROM system.columns WHERE database='system' AND
// table='query_log'`), not assumed from the upstream docs the issue itself
// links — the same discipline issue #2789's own risk note calls out (#2770's
// Loki catalog PR caught a real bug from an unverified assumption about
// system.view_refreshes's columns; this is the same class of check applied
// here): log_comment is String, read_rows/read_bytes/memory_usage are all
// UInt64, event_time is DateTime, and type is
// Enum8('QueryStart'=1,'QueryFinish'=2,'ExceptionBeforeStart'=3,'ExceptionWhileProcessing'=4)
// — exactly as expected, so no defensive re-typing was needed here.
//
// startsWith(log_comment, ?), not a LIKE pattern: the caller
// (internal/engine) owns the "cerb:" shape-id prefix constant
// (plan_shape_id.go's shapeIDPrefix) — chclient must not import engine — so
// the prefix is passed in as a bound parameter rather than this file
// duplicating the constant or fighting LIKE's own '%'/'_' escaping rules for
// a literal prefix match.
const queryLogActualsSQL = `SELECT log_comment, read_rows, read_bytes, memory_usage, event_time
FROM system.query_log
WHERE type = 'QueryFinish' AND event_time > ? AND startsWith(log_comment, ?)
ORDER BY event_time ASC
LIMIT ?`

// QueryLogActualRow is one system.query_log row for a cerberus-stamped
// query (log_comment carrying a plan-shape id).
type QueryLogActualRow struct {
	// LogComment is the plan-shape id verbatim, e.g. "cerb:agg;rw" —
	// internal/engine's SettingsRules stamped it via
	// chclient.WithQuerySetting(ctx, "log_comment", shapeID).
	LogComment string
	ReadRows   uint64
	ReadBytes  uint64
	// MemoryUsage is ClickHouse's own peak-memory-usage column for the
	// query, matching the packet path's ProfileEvents
	// "MemoryTrackerPeakUsage" reading (progress.go's own doc) — the two
	// sources report the SAME quantity by two different transports.
	MemoryUsage uint64
	EventTime   time.Time
}

// QueryLogActuals reads system.query_log for every QueryFinish row whose
// log_comment starts with shapeIDPrefix and event_time is strictly after
// since, oldest first, capped at limit rows. UNKNOWN_TABLE (query_log
// disabled, or a deployment that has never enabled it) degrades to an empty
// slice, nil error — the same honest degrade
// QueryViewRefreshState/FilesystemCacheState apply to an optional
// system-table dependency, rather than failing the whole reconciler poll
// over an operator choice this feature does not require.
//
// Guarded by the circuit breaker (see [Client] doc), exactly like every
// other query method here: a struggling ClickHouse server does not get an
// extra query per reconciler tick on top of everything else already backing
// off.
func (c *Client) QueryLogActuals(ctx context.Context, since time.Time, shapeIDPrefix string, limit int) ([]QueryLogActualRow, error) {
	if !c.br.allow() {
		return nil, c.br.openErr("chclient: query log actuals")
	}
	ctx = c.queryContext(ctx)
	ctx, span := startExecuteSpan(ctx, queryLogActualsSQL, c.addr)
	defer span.End()
	defer flushProgress(ctx)
	rows, err := c.queryOpen(ctx, queryLogActualsSQL, since, shapeIDPrefix, limit)
	c.br.record(ctx, err)
	if err != nil {
		if IsUnknownTable(err) {
			return nil, nil
		}
		span.RecordError(err)
		return nil, fmt.Errorf("chclient: query log actuals: %w", c.classifyDriverErr(ctx, err))
	}
	defer func() {
		_ = rows.Close()
	}()

	var out []QueryLogActualRow
	for rows.Next() {
		var row QueryLogActualRow
		if err := rows.Scan(&row.LogComment, &row.ReadRows, &row.ReadBytes, &row.MemoryUsage, &row.EventTime); err != nil {
			return nil, fmt.Errorf("chclient: query log actuals scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chclient: query log actuals rows.Err: %w", c.classifyDriverErr(ctx, err))
	}
	return out, nil
}
