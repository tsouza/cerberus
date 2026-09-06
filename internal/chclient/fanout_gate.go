package chclient

import (
	"context"
	"errors"
	"fmt"
	"sync"

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
// KNOWN LIMITATION (cerberus issue #3128's post-move investigation, real e2e
// dispatch runs 34032272914's second attempt: N=2 peak concurrent 10 > cap 8,
// N=4 peak concurrent 24 > cap 8; every run's own client-side accounting
// stayed within cap the whole time, ruled out with a temporary held-weight
// counter that never once observed an over-cap acquisition): this gate
// bounds client-BELIEVED concurrent dispatch weight, not ClickHouse's own
// real concurrent per-shard statement count, and the two are not the same
// thing under cancellation. acquireDataShardFanout's release fires the
// instant queryOpen/queryCursorColumnar's underlying c.conn.Query / pool.Do
// call RETURNS — but when that call returns because ctx was cancelled
// (internal/solver/executor.go's errgroup.WithContext cancels every sibling
// shard's ctx the instant ANY one of a routed query's kEff concurrently-
// admitted shards errors, and an HTTP client disconnect/timeout cancels
// r.Context() the same way), clickhouse-go v2's own connect.cancel()
// (conn_process.go, both process() and firstBlock()) sends a single
// ClientCancel packet and closes the LOCAL connection immediately — it does
// NOT wait for ClickHouse to confirm the (possibly still-running, possibly
// Distributed-fanned-out) statement has actually stopped. This package
// therefore releases that dispatch's weight the instant the LOCAL socket
// closes, while the REAL per-shard ClickHouse statement(s) it dispatched may
// keep running server-side for an uncontrolled further interval — during
// which this gate believes that capacity is free and admits a new dispatch
// on top of it. The observed overshoot magnitude is consistent with exactly
// this: on both real runs above, (peak concurrent - cap) is an integer
// multiple of DataShardCount (16 = 4x4 on N=4, 2 = 1x2 on N=2) — i.e. a
// small, plausible number of premature-cancellation releases, not a
// diffuse accounting error.
//
// A durable fix needs this package to distinguish "the local dispatch
// finished" from "ClickHouse actually stopped executing it" on the
// cancellation path specifically — e.g. issuing `KILL QUERY WHERE query_id
// = ? SYNC` (ShardQueryIDs/mintQueryID already mint one for every dispatch)
// on a fresh, uncancelled connection before releasing weight acquired by a
// dispatch that is unwinding via ctx cancellation rather than a normal
// server-side finish — and has not been implemented or validated against a
// real cluster yet. Route A's admission gap this file closes is still a
// genuine improvement over the pre-move state (Route A was completely
// ungated before), but the gate is not yet a hard ceiling on ClickHouse's
// own concurrent per-shard statement count under cancellation. Tracked on
// issue #3128 until the cancellation path is fixed for real.

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
			c.dataShardFanoutGate.Release(weight)
		})
	}, nil
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
