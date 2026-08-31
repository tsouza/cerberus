package chclient

import (
	"context"
	"fmt"
)

// viewRefreshStateSQL reads ONE refreshable materialized view's row from
// system.view_refreshes (cerberus issue #2770) — the ClickHouse-native
// scheduler status for a `REFRESH EVERY ...` view (docs:
// https://clickhouse.com/docs/materialize/refreshable-materialized-view#monitoring).
// database/view are bound positionally so the query names exactly the loki
// label-catalog view internal/schema/ddl provisioned, never every
// refreshable view a deployment might carry.
//
// The column list is VERIFIED against a live ClickHouse 25.9 server (via
// `SELECT name, type FROM system.columns WHERE table='view_refreshes'`),
// not assumed from the upstream design doc — that doc's own prose mentions
// neither a "last refresh result" enum nor a refresh counter, and indeed
// system.view_refreshes carries no such columns: there is no
// last_refresh_result and no refresh_count. Failure detection instead reads
// exception (non-empty means the most recent completed attempt failed) and
// compares last_success_time against last_refresh_time (equal after a
// successful attempt, last_success_time UNCHANGED and now strictly older
// than last_refresh_time after a failed one) — verified live: forcing a
// refresh against a renamed-away source table left status="Scheduled",
// exception populated with the real UNKNOWN_TABLE error, last_refresh_time
// advanced, and last_success_time untouched, while the target table's data
// was provably unchanged (see
// internal/schema/ddl's TestLokiLabelCatalog_RefreshAndFailureMode, the
// same live-server proof for the "keeps serving the previous snapshot"
// claim this package's caller relies on).
//
// last_success_time / last_refresh_time are Nullable(DateTime) — NULL
// before the view's first successful / first attempted refresh
// respectively — so both go through ifNull(toString(x), <empty string>)
// and decode as plain (possibly empty) strings rather than fighting the
// driver's Nullable(DateTime) scan mapping; an empty string on the wire is
// the honest "never happened yet" answer /info reports (see
// ViewRefreshState.LastSuccessTime's doc comment).
const viewRefreshStateSQL = `SELECT status, exception, ifNull(toString(last_success_time), ''), ifNull(toString(last_refresh_time), ''), retry FROM system.view_refreshes WHERE database = ? AND view = ?`

// ViewRefreshState is the live scheduler status of ONE refreshable
// materialized view (cerberus issue #2770), read straight off
// system.view_refreshes with no cerberus-side interpretation layered on —
// callers (internal/api/info) surface the raw ClickHouse-reported fields
// rather than cerberus deciding "healthy"/"unhealthy" on its behalf, so a
// failed refresh reads as exactly that: Exception carries the real error
// text and LastSuccessTime stops advancing, instead of being silently
// folded into a boolean.
type ViewRefreshState struct {
	// Found reports whether system.view_refreshes carried a row for the
	// requested (database, view) — false when the view was never
	// provisioned (e.g. LokiLabelCatalogEnabled is off, or the connected
	// server predates the version floor) or the query itself failed; every
	// other field is the zero value in that case.
	Found bool
	// Status is the view's current scheduler state, e.g. "Scheduled",
	// "Running", "WaitingForDependencies", "Disabled" — read verbatim from
	// ClickHouse. NOTE: this does NOT flip to an "Error" value on a failed
	// refresh — verified live, a view whose refresh just failed still
	// reads Status="Scheduled" (it is simply waiting for its next
	// scheduled attempt); Exception is what carries the failure signal.
	Status string
	// Exception is the most recently COMPLETED attempt's error text, or ""
	// when that attempt succeeded (or none has completed yet). Populated
	// even when LastSuccessTime is non-empty: a LATER refresh can fail
	// after an earlier one succeeded, which is exactly the "keep serving
	// the previous snapshot" case this field lets an operator spot —
	// compare LastSuccessTime against LastRefreshTime to see whether the
	// MOST RECENT attempt was the one that failed.
	Exception string
	// LastSuccessTime is the last successful refresh's completion
	// timestamp ("2006-01-02 15:04:05" ClickHouse text form), or "" when
	// no refresh has EVER succeeded — the catalog table is then still
	// whatever the CREATE statement left it (empty), not stale data from a
	// prior success.
	LastSuccessTime string
	// LastRefreshTime is the most recent refresh ATTEMPT's completion
	// timestamp (successful or not), or "" when no attempt has completed
	// yet. LastRefreshTime > LastSuccessTime means the most recent attempt
	// failed (Exception is then non-empty); the two are equal after a
	// successful attempt.
	LastRefreshTime string
	// Retry is the current backoff retry counter for a REPEATEDLY failing
	// refresh (ClickHouse resets it to 0 on a successful attempt) — a
	// nonzero value alongside a non-empty Exception means the view has
	// failed more than once in a row, not just a single transient error.
	Retry uint64
}

// QueryViewRefreshState reads system.view_refreshes for the named
// (database, view) and returns its current status. UNKNOWN_TABLE (most
// commonly a pre-24.10 server that has never provisioned
// system.view_refreshes at all) degrades to Found=false, nil error — the
// same honest degrade FilesystemCacheState applies to an unconfigured
// server — rather than failing the whole /info response over an optional
// sub-fingerprint. Every other error (a genuine connectivity failure, a
// permission rejection) propagates normally: those are real problems
// worth surfacing, not "the view doesn't exist yet".
//
// Guarded by the circuit breaker (see [Client] doc): a struggling
// ClickHouse server does not get an extra query per /info poll on top of
// the readiness ping.
func (c *Client) QueryViewRefreshState(ctx context.Context, database, view string) (ViewRefreshState, error) {
	if !c.br.allow() {
		return ViewRefreshState{}, c.br.openErr("chclient: query")
	}
	ctx = c.queryContext(ctx)
	ctx, span := startExecuteSpan(ctx, viewRefreshStateSQL, c.addr)
	defer span.End()
	defer flushProgress(ctx)
	rows, err := c.queryOpen(ctx, viewRefreshStateSQL, database, view)
	c.br.record(ctx, err)
	if err != nil {
		if IsUnknownTable(err) {
			return ViewRefreshState{}, nil
		}
		span.RecordError(err)
		return ViewRefreshState{}, fmt.Errorf("chclient: query: %w", c.classifyDriverErr(ctx, err))
	}
	defer func() {
		_ = rows.Close()
	}()

	var out ViewRefreshState
	if rows.Next() {
		if err := rows.Scan(
			&out.Status, &out.Exception, &out.LastSuccessTime,
			&out.LastRefreshTime, &out.Retry,
		); err != nil {
			return ViewRefreshState{}, fmt.Errorf("chclient: scan: %w", err)
		}
		out.Found = true
	}
	if err := rows.Err(); err != nil {
		return ViewRefreshState{}, fmt.Errorf("chclient: rows.Err: %w", c.classifyDriverErr(ctx, err))
	}
	return out, nil
}
