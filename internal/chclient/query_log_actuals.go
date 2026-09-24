package chclient

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/chopt"
)

// query_log_actuals.go is the transport half of the query-actuals fallback
// source (issue #2789): it reads the server's query log for cerberus-stamped
// queries the native-protocol packet path (progress.go) did not observe.
// internal/engine's QueryLogActualsReconciler is the polling half.
//
// Two sources share one record-selection query and differ only in the table
// they read:
//
//   - system.query_log, the server's own current log (the default);
//   - system.all_query_log, the union table ClickHouse 26.8+ maintains when
//     the server's <create_union_system_log_tables> section is configured:
//     merge('system', '^query_log(_[0-9]+)?$') over the current table and
//     every query_log_N a schema change rotated the log into, wrapped in
//     clusterAllReplicas(<cluster>, ...) when the section names a cluster.
//
// Record selection. Every column below is read from the server's own schema
// (system.columns on 24.8.14.39 and 26.8.10.6, the pinned floor and the
// union-capable build test/querylog exercises), not assumed:
//
//   - type = 'QueryFinish': the one event carrying a finished query's totals.
//   - is_initial_query = 1: a Distributed read logs one row per remote child
//     on each server that ran a piece of it, each with its OWN query_id, the
//     initiator's query_id as initial_query_id, the propagated log_comment,
//     and only that child's fraction of read_rows. The initiator's row
//     already sums every child's read_rows/read_bytes (the server-initiator
//     summarises all received and local values), so a child row is a
//     fragment of work already accounted, never a query of its own.
//   - memory_usage is the initiator's own peak — the same quantity the packet
//     path's MemoryTrackerPeakUsage ProfileEvent reports on the dispatching
//     connection — never a sum across servers.
//   - LIMIT 1 BY hostname, query_id: a physical query is identified by the
//     server that ran it and its query_id. A local MergeTree log holds each
//     such row once, and the rotated tables are disjoint from the current
//     one, so on the logs test/querylog exercises this is a guard: it keeps
//     one row per identity should two union members ever return the same row
//     (an operator-configured replicated log engine).
//
// Pagination. Rows are ordered by the total order (event time in
// microseconds, hostname, query_id) and read strictly after the caller's
// cursor in that order, so rows sharing one timestamp page without loss or
// repetition. Rows younger than the settle delay (measured on the server's
// clock) are left for a later read: a query-log flush is asynchronous and
// per-server, so a row can surface after a row with a later timestamp has
// already been read; waiting out the flush lag before a row becomes readable
// is what lets a strictly advancing cursor not skip it, as long as the
// servers' clocks agree to within the settle delay less the flush interval.
// The event_date / event_time bounds restate the cursor on the log's sorting
// key so the read prunes to the cursor's partition and granules; the caller
// bounds every read by never placing the cursor further back than its
// lookback.
//
// The two statements are complete literals rather than one statement with a
// substituted table name; TestQueryLogActualsSQL_DifferOnlyInTable pins that
// they never drift apart.
const queryLogActualsLocalSQL = `SELECT hostname, log_comment, query_id, read_rows, read_bytes, memory_usage, toUnixTimestamp64Micro(event_time_microseconds)
FROM system.query_log
WHERE type = 'QueryFinish'
  AND is_initial_query = 1
  AND startsWith(log_comment, ?)
  AND event_date >= toDate(toDateTime(?))
  AND event_time >= toDateTime(?)
  AND (toUnixTimestamp64Micro(event_time_microseconds), hostname, query_id) > (?, ?, ?)
  AND event_time_microseconds <= now64(6) - toIntervalMillisecond(?)
ORDER BY event_time_microseconds, hostname, query_id
LIMIT 1 BY hostname, query_id
LIMIT ?`

const queryLogActualsUnionSQL = `SELECT hostname, log_comment, query_id, read_rows, read_bytes, memory_usage, toUnixTimestamp64Micro(event_time_microseconds)
FROM system.all_query_log
WHERE type = 'QueryFinish'
  AND is_initial_query = 1
  AND startsWith(log_comment, ?)
  AND event_date >= toDate(toDateTime(?))
  AND event_time >= toDateTime(?)
  AND (toUnixTimestamp64Micro(event_time_microseconds), hostname, query_id) > (?, ?, ?)
  AND event_time_microseconds <= now64(6) - toIntervalMillisecond(?)
ORDER BY event_time_microseconds, hostname, query_id
LIMIT 1 BY hostname, query_id
LIMIT ?`

// ErrQueryLogUnionRefused wraps a server's refusal of the system.all_query_log
// read as not provisioned (isQueryLogUnionRefusal): the table is absent (the
// server predates 26.8 or has no <create_union_system_log_tables> section),
// the SELECT is not granted, or a member lacks a selected column. The caller
// falls back to the local log; a transport failure, or any other server
// error, is never wrapped in it.
var ErrQueryLogUnionRefused = errors.New("chclient: system.all_query_log refused")

// QueryLogCursor is a position in the query-log reader's total order. The
// zero Hostname and QueryID sort before every real row sharing EventTime, so
// a cursor built from a bare time reads every row at that time.
type QueryLogCursor struct {
	// EventTime is the row's event_time_microseconds.
	EventTime time.Time
	Hostname  string
	QueryID   string
}

// QueryLogActualsRequest is one page read.
type QueryLogActualsRequest struct {
	// Union reads system.all_query_log instead of system.query_log.
	Union bool
	// After is the exclusive lower bound in the reader's total order.
	After QueryLogCursor
	// SettleDelay holds back every row whose event time is within this much
	// of the server's current time.
	SettleDelay time.Duration
	// ShapeIDPrefix restricts the read to cerberus-stamped rows: log_comment
	// must start with it (internal/engine's shapeIDPrefix, passed in because
	// chclient must not import engine).
	ShapeIDPrefix string
	// Limit caps the page.
	Limit int
}

// QueryLogActualRow is one cerberus-stamped query's finished-query totals.
type QueryLogActualRow struct {
	// Hostname is the server that ran the query (its initiator).
	Hostname string
	// LogComment is the plan-shape id verbatim, e.g. "cerb:agg;rw".
	LogComment string
	// QueryID is ClickHouse's per-statement query_id — the id cerberus minted
	// for the dispatch (queryContext). The consumer takes a set difference on
	// it against the dispatches the packet path already recorded.
	QueryID   string
	ReadRows  uint64
	ReadBytes uint64
	// MemoryUsage is the initiator's peak memory usage, the quantity the
	// packet path reads from the MemoryTrackerPeakUsage ProfileEvent.
	MemoryUsage uint64
	// EventTime is event_time_microseconds.
	EventTime time.Time
}

// QueryLogActuals reads one page of cerberus-stamped finished queries after
// req.After, oldest first. On the local source an absent system.query_log
// (query logging disabled) degrades to an empty page, nil error — the log is
// an operator choice this feature does not require. On the union source a
// not-provisioned refusal is returned wrapped in ErrQueryLogUnionRefused.
//
// Guarded by the circuit breaker (see [Client] doc), like every other query
// method here.
func (c *Client) QueryLogActuals(ctx context.Context, req QueryLogActualsRequest) ([]QueryLogActualRow, error) {
	if !c.br.allow() {
		return nil, c.br.openErr("chclient: query log actuals")
	}
	sql := queryLogActualsLocalSQL
	if req.Union {
		sql = queryLogActualsUnionSQL
	}
	afterSeconds := req.After.EventTime.Unix()
	ctx = c.queryContext(ctx)
	ctx, span := startExecuteSpan(ctx, sql, c.addr)
	defer span.End()
	defer flushProgress(ctx)
	rows, err := c.queryOpen(
		ctx, sql,
		req.ShapeIDPrefix,
		afterSeconds, afterSeconds,
		req.After.EventTime.UnixMicro(), req.After.Hostname, req.After.QueryID,
		req.SettleDelay.Milliseconds(),
		req.Limit,
	)
	c.br.record(ctx, err)
	if err != nil {
		if !req.Union && IsUnknownTable(err) {
			return nil, nil
		}
		span.RecordError(err)
		return nil, c.queryLogActualsErr(ctx, req.Union, err)
	}
	defer func() {
		_ = rows.Close()
	}()

	var out []QueryLogActualRow
	for rows.Next() {
		var row QueryLogActualRow
		var eventMicros int64
		if err := rows.Scan(&row.Hostname, &row.LogComment, &row.QueryID, &row.ReadRows, &row.ReadBytes, &row.MemoryUsage, &eventMicros); err != nil {
			return nil, fmt.Errorf("chclient: query log actuals scan: %w", err)
		}
		row.EventTime = time.UnixMicro(eventMicros)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, c.queryLogActualsErr(ctx, req.Union, err)
	}
	return out, nil
}

// Server error codes that mean system.all_query_log is not provisioned for
// this reader, rather than that one read of it failed.
const (
	chCodeNoSuchColumnInTable = 16
	chCodeUnknownIdentifier   = 47
	chCodeAccessDenied        = 497
)

// queryLogActualsErr wraps a read failure: a union read the server refused as
// not provisioned (isQueryLogUnionRefusal) as ErrQueryLogUnionRefused,
// anything else — a transport failure, or a typed answer such as a timeout or
// a memory limit that says nothing about the table — as a classified driver
// error the caller retries from the same cursor.
func (c *Client) queryLogActualsErr(ctx context.Context, union bool, err error) error {
	if union && isQueryLogUnionRefusal(err) {
		return fmt.Errorf("%w: %w", ErrQueryLogUnionRefused, err)
	}
	return fmt.Errorf("chclient: query log actuals: %w", c.classifyDriverErr(ctx, err))
}

// isQueryLogUnionRefusal reports whether err is the server saying the union
// read cannot be served at all: the table is unknown (an older server, or no
// union section), the reader lacks the grant, or a member lacks a selected
// column.
func isQueryLogUnionRefusal(err error) bool {
	var ex *clickhouse.Exception
	if !errors.As(err, &ex) {
		return false
	}
	switch ex.Code {
	case chCodeUnknownTable, chCodeAccessDenied, chCodeUnknownIdentifier, chCodeNoSuchColumnInTable:
		return true
	default:
		return false
	}
}

// ProbeQueryLogUnionCapability runs the reader's own record-selection query
// against system.all_query_log for an empty page, so the verdict covers what
// the server checks before it reads a row: the table exists, its columns are
// the ones selected, and cerberus's user may SELECT it. A nil error is
// Available, a typed server rejection is Forbidden, and a transport failure
// is Unreachable (classifyCapabilityFromProbeErr). Never returns an error.
func (c *Client) ProbeQueryLogUnionCapability(ctx context.Context) chopt.Capability {
	_, err := c.QueryLogActuals(ctx, QueryLogActualsRequest{
		Union: true,
		After: QueryLogCursor{EventTime: time.Now()},
	})
	return classifyCapabilityFromProbeErr(err)
}
