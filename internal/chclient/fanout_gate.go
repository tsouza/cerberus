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
		once.Do(func() { c.dataShardFanoutGate.Release(weight) })
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
