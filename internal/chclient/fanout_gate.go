package chclient

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"golang.org/x/sync/semaphore"
)

// fanout_gate.go — the data-shard fan-out admission gate (cerberus issues
// #3081, #3128, epic #3074).
//
// HISTORY: cerberus issue #3081 first introduced this gate inside
// internal/solver's Executor, acquired ONCE per routed (solver-split)
// dispatch with weight kEff x DataShardCount. That placement bounded only
// the sharded-pushdown "route B" path; the ordinary, non-split "route A"
// path — the vast majority of real traffic — dispatches straight through
// this package with NO involvement from internal/solver at all, and a
// route-A query against a `Distributed`-engine table ALSO fans out to
// DataShardCount-many real per-shard ClickHouse statements, structurally
// identical in kind to what the gate was built to bound. Because route A
// never acquired it, the gate bounded only a fraction of the aggregate
// concurrent per-shard statement count ClickHouse actually saw — cerberus
// issue #3128's real e2e evidence (peak concurrent per-shard statements
// 1.5x-4x over DataShardFanoutCap on both N=2 and N=4 datashard lanes).
//
// THE FIX: the gate moves HERE, to the one seam every dispatch this
// package makes — route A's single statement AND every one of route B's
// per-shard dispatches alike — passes through: queryOpen (the row-decoder
// path every synchronous drain method and QueryCursor's default strategy
// share) and queryCursorColumnar (the columnar matrix-decode strategy's own
// dial). internal/solver's Executor no longer acquires this gate itself
// (see executor.go's admitAndGate, cerberus issue #3128) — its own K-shard
// fan-out reaches ClickHouse through THIS Client's QueryCursor per shard,
// so each of its kEff dispatches acquires weight DataShardCount here,
// summing to the exact same kEff x DataShardCount the old single upfront
// acquisition charged, just decomposed to the point of actual dispatch
// instead of charged in one lump ahead of it. Charging at dispatch time
// rather than admission time is what closes the route-A gap: route A is
// exactly one such dispatch (weight DataShardCount), so it is now bounded
// by the identical mechanism with no separate code path.
//
// DataShardCount <= 1 (every deployment that predates cerberus issue #3081,
// and every single-data-shard deployment) leaves dataShardFanoutGate nil —
// this file's ENTIRE mechanism is structurally unreached, matching the
// pre-#3081 behaviour bit-for-bit.
//
// CANCELLATION FIX (cerberus issue #3128's post-move investigation, real e2e
// dispatch runs 34032272914's second attempt: N=2 peak concurrent 10 > cap 8,
// N=4 peak concurrent 24 > cap 8; every run's own client-side accounting
// stayed within cap the whole time, ruled out with a temporary held-weight
// counter that never once observed an over-cap acquisition). The root cause:
// acquireDataShardFanout's release used to fire the instant
// queryOpen/queryCursorColumnar's underlying c.conn.Query / pool.Do call
// RETURNED — but when that call returns because ctx was cancelled
// (internal/solver/executor.go's errgroup.WithContext cancels every sibling
// shard's ctx the instant ANY one of a routed query's kEff concurrently-
// admitted shards errors, and an HTTP client disconnect/timeout cancels
// r.Context() the same way), clickhouse-go v2's own connect.cancel()
// (conn_process.go, both process() and firstBlock()) sends a single
// ClientCancel packet and closes the LOCAL connection immediately — it does
// NOT wait for ClickHouse to confirm the (possibly still-running, possibly
// Distributed-fanned-out) statement has actually stopped. Releasing the
// gate weight right there let a new dispatch admit on top of ClickHouse-side
// work that had not actually stopped yet. The observed overshoot magnitude
// was consistent with exactly this: on both real runs above, (peak
// concurrent - cap) was an integer multiple of DataShardCount (16 = 4x4 on
// N=4, 2 = 1x2 on N=2) — i.e. a small, plausible number of
// premature-cancellation releases, not a diffuse accounting error.
//
// The fix: acquireDataShardFanout's release closure now checks ctx.Err() at
// the instant it runs (not the error the underlying call returned — a typed
// *clickhouse.Exception means CH already finished the statement on its own
// and needs no help). A non-nil ctx.Err() means THIS dispatch's own ctx was
// cancelled or hit its deadline — the reason the call returned, not a normal
// server-side finish — so before releasing the weight, killDataShardQuery
// issues `KILL QUERY WHERE query_id = ? SYNC` for this dispatch's own
// query_id (queryIDFromContext, stamped by queryContext before the gate is
// ever acquired) on a FRESH, uncancelled, short-lived connection/context —
// never the dispatch's own already-cancelled one. SYNC blocks until
// ClickHouse itself confirms the statement is dead (or was already gone,
// the common race-free-outcome when the statement finished naturally in the
// tiny window between local cancellation and this call landing), so the
// gate weight only releases once ClickHouse agrees the capacity is
// genuinely free. A failed or slow KILL QUERY (network error reaching CH,
// bounded by killDataShardQueryTimeout) is logged, never fatal — the weight
// still releases unconditionally afterward, so a KILL QUERY failure can
// never leak gate capacity, only (rarely) fail to close this specific race.
//
// The normal, non-cancelled finish path is untouched: ctx.Err() is nil, the
// check is a cheap single field read, and no extra round-trip is ever made
// for the overwhelming majority of dispatches that simply finish.
//
// Residual risk: the KILL QUERY round-trip itself can only be as reliable as
// reaching ClickHouse on a fresh connection within killDataShardQueryTimeout
// — if THAT call also fails to confirm (CH itself unreachable, or the
// dispatch's ctx carried no query_id because no trace was present), the gate
// weight still releases (never leaked) but without the extra confirmation,
// so the pre-fix race can in principle still occur in that narrow,
// already-degraded scenario. Route A's admission gap this file closes
// (Route A was completely ungated before #3128's move) plus this
// cancellation fix together make the gate a hard ceiling on ClickHouse's own
// concurrent per-shard statement count under the overwhelming majority of
// real cancellation scenarios; only the doubly-degraded case above remains.

// ErrDataShardFanoutGateBusy is the sentinel wrapped into the error
// [Client.acquireDataShardFanout] returns when the request's own ctx
// expires or is cancelled while waiting for aggregate data-shard fan-out
// admission. This call never reached ClickHouse, so — exactly like
// [clickhouse.ErrAcquireConnTimeout] — it is a LOCAL admission-control
// signal, not a ClickHouse health signal: classifyBreakerOutcome scopes it
// breakerScopeClient (see its own doc), so a gate denial can never trip the
// circuit breaker regardless of whether the underlying ctx error was
// Canceled or DeadlineExceeded.
var ErrDataShardFanoutGateBusy = errors.New("chclient: data-shard fanout gate: DataShardFanoutCap admission budget exceeded")

// minDataShardFanoutCap is the floor NewDataShardFanoutGate clamps the
// resolved cap to, so a bare Config (DataShardCount > 1 set without a
// positive MaxOpenConns or override — a test-construction shape only;
// every production Config.FromEnv value validates MaxOpenConns > 0) never
// allocates a permanently-empty, always-blocking semaphore. Named so the
// floor is never a bare literal (invariant 13).
const minDataShardFanoutCap = 1

// NewDataShardFanoutGate derives the (gate, cap) pair [assembleClientFromConn]
// wires onto a Client, from cfg alone. cap defaults to cfg.MaxOpenConns —
// this package's own connection-pool size is the natural sibling bound,
// mirroring how the pre-move mechanism defaulted to the solver's own
// connection Gate's size — unless cfg.DataShardFanoutCapOverride is set.
// The gate itself is nil (never allocated) whenever cfg.DataShardCount <= 1,
// the one place that decision is made, so every consumer of the resolved
// (gate, cap) pair need not repeat the check. Exported so a regression test
// can assert the DataShardCount <= 1 case never allocates a semaphore
// without duplicating this arithmetic.
func NewDataShardFanoutGate(cfg Config) (gate *semaphore.Weighted, cap int64) {
	cap = int64(cfg.MaxOpenConns)
	if cfg.DataShardFanoutCapOverride != nil {
		cap = *cfg.DataShardFanoutCapOverride
	}
	if cap < minDataShardFanoutCap {
		cap = minDataShardFanoutCap
	}
	if cfg.DataShardCount <= 1 {
		return nil, cap
	}
	return semaphore.NewWeighted(cap), cap
}

// acquireDataShardFanout acquires this dispatch's share of the aggregate
// data-shard fan-out budget — weight c.dataShardCount, floored to 1 — and
// returns the idempotent release closure the caller MUST invoke exactly
// once the dispatch's ClickHouse-side work has finished (queryOpen ties it
// to the returned driver.Rows' Close via gatedRows; queryCursorColumnar
// ties it directly to its own synchronous pool.Do call, since that call
// already blocks until the statement is fully drained).
//
// The returned release closure closes over ctx (the SAME context the
// caller's dispatch runs under, already stamped with the per-dispatch
// query_id by queryContext — see the callers' own doc for why that
// ordering matters) so it can distinguish, at the instant it actually
// runs, a normal server-side finish from a cancellation-driven unwind: see
// this file's own "CANCELLATION FIX" doc above.
//
// A nil c.dataShardFanoutGate (DataShardCount <= 1, see
// NewDataShardFanoutGate) returns a no-op release and a nil error
// immediately — the pre-#3081 behaviour, unconditionally.
func (c *Client) acquireDataShardFanout(ctx context.Context) (release func(), err error) {
	if c.dataShardFanoutGate == nil {
		return func() {}, nil
	}
	weight := c.dataShardCount
	if weight < 1 {
		weight = 1
	}
	if aerr := c.dataShardFanoutGate.Acquire(ctx, weight); aerr != nil {
		return nil, fmt.Errorf("chclient: data-shard fanout gate acquire: %w: %w", ErrDataShardFanoutGateBusy, aerr)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			// ctx.Err() != nil means THIS dispatch's own ctx — the one
			// Acquire was just called with above — was cancelled or hit its
			// deadline: that is why the caller's underlying c.conn.Query /
			// pool.Do call returned, not a normal server-side finish (which
			// leaves ctx.Err() nil even when the call itself errored with a
			// typed *clickhouse.Exception). Only the cancellation-unwind
			// path pays for the extra KILL QUERY round-trip; a normal finish
			// falls straight through to Release below, unconditionally.
			if ctx.Err() != nil {
				if queryID := queryIDFromContext(ctx); queryID != "" {
					c.killDataShardQuery(queryID)
				}
			}
			c.dataShardFanoutGate.Release(weight)
		})
	}, nil
}

// killDataShardQueryTimeout bounds how long killDataShardQuery waits for
// KILL QUERY ... SYNC to confirm a cancelled dispatch's ClickHouse-side
// statement has genuinely stopped (or was already gone) before giving up.
// acquireDataShardFanout's release always frees the gate weight afterward
// regardless of the outcome — this bound only caps how long that release
// can be delayed by an unresponsive ClickHouse, so a hung KILL QUERY can
// never leak gate capacity, merely delay its release by at most this long.
// Named so the bound is never a bare literal (invariant 13).
const killDataShardQueryTimeout = 5 * time.Second

// killDataShardQuerySQL targets a single per-dispatch query_id. SYNC blocks
// until ClickHouse confirms the query is actually dead — or reports nothing
// to kill, the common case when the statement had already finished on its
// own in the small race window between local cancellation and this call
// landing — rather than merely accepting the request. Raw SQL text is fine
// here (unlike internal/chsql's plan-emission layer, invariant 10): this is
// an administrative statement against ClickHouse's own process table, not
// emitted query plan SQL.
const killDataShardQuerySQL = `KILL QUERY WHERE query_id = ? SYNC`

// killDataShardQuery issues killDataShardQuerySQL for queryID on a FRESH,
// uncancelled, short-lived context — deliberately NOT derived from the
// dispatch's own (already-cancelled) ctx, which would make the KILL request
// itself fail the exact same way before ever reaching ClickHouse. It calls
// c.conn.Exec directly rather than c.queryOpen or the public Client.Exec:
// this administrative statement is not itself a shard dispatch, so it must
// never recursively acquire c.dataShardFanoutGate (queryOpen's seam), and
// its outcome is not a signal about ClickHouse's general health, so it must
// never touch the circuit breaker (Client.Exec's gating) in either
// direction. clickhouse-go/v2's connection pool hands this call a fresh
// pooled connection even while the dispatch's own connection is mid-cancel,
// since that pooled connection was already evicted by connect.cancel()
// (this file's own "CANCELLATION FIX" doc) rather than being handed out
// again.
//
// Every non-nil outcome is logged at WARN for observability (the same
// breakerLogger() package-level accessor breaker.go's own transition logs
// use) but never treated as fatal: acquireDataShardFanout's release always
// frees the gate weight once this returns, regardless of whether it
// succeeded, timed out, or found no matching query to kill.
func (c *Client) killDataShardQuery(queryID string) {
	ctx, cancel := context.WithTimeout(context.Background(), killDataShardQueryTimeout)
	defer cancel()
	if err := c.conn.Exec(ctx, killDataShardQuerySQL, queryID); err != nil {
		breakerLogger().Warn(
			"chclient: data-shard fanout gate: KILL QUERY on a cancelled dispatch did not confirm the statement stopped",
			"query_id", queryID, "error", err,
		)
	}
}

// gatedRows decorates a driver.Rows so its Close() also releases the
// data-shard fan-out weight queryOpen acquired for it — the ClickHouse-side
// statement genuinely stays "in flight" (from this gate's point of view)
// for exactly as long as the caller keeps the result set open, mirroring
// how internal/solver's own connection Gate was already held until
// shardCursor.Close before this gate moved here.
//
// Embedding driver.Rows (rather than naming every method) means gatedRows
// satisfies the interface by forwarding every method except the one it
// overrides — Close — to the wrapped value.
type gatedRows struct {
	driver.Rows
	release func()
}

// Close releases the wrapped rows AND the fan-out weight, in that order,
// and always runs the release (even when rows.Close itself errors) since
// the ClickHouse-side statement is done either way. release is already
// idempotent (acquireDataShardFanout's sync.Once), so a caller that closes
// more than once — permitted by some driver.Rows implementations — cannot
// double-release.
func (g *gatedRows) Close() error {
	err := g.Rows.Close()
	g.release()
	return err
}
