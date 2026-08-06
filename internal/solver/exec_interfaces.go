package solver

import (
	"context"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
)

// The interfaces below are the seam between internal/solver and the
// chclient / admit / chsql packages. The import-cycle rule forbids
// internal/solver from importing internal/engine, and keeping the rest as
// narrow interfaces (rather than importing the concrete chsql emitter)
// keeps the package's dependency cone minimal and lets the executor tests
// run against fakes without a real ClickHouse.
//
// The concrete satisfiers live on the chclient / admit side:
//
//   - *chclient.Client satisfies CursorQuerier (QueryCursor) and
//     breakerPeeker (PeekBreakerState + BreakerRetryAfter).
//   - *admit.Limiter satisfies admitTopUp (TryAcquireTopUp).
//   - internal/chsql provides the SQLEmitter (wired by the engine adapter
//     in the next PR).

// CursorQuerier opens a streaming cursor over a CH result set and reports
// the per-query memory cap it stamps on every data-plane query.
// *chclient.Client satisfies it via QueryCursor + MaxQueryMemoryBytes (the
// engine already reads the latter for an unrelated spill-threshold sizing
// decision, so this is the same value read from a second call site, not a
// new capability).
type CursorQuerier interface {
	QueryCursor(ctx context.Context, sql string, args ...any) (chclient.Cursor, error)

	// MaxQueryMemoryBytes returns the per-query `max_memory_usage` cap
	// (bytes) this Client stamps on route A's single-statement queries, or
	// 0 when no cap is configured. Execute divides this by kEff to compute
	// the mandatory per-shard apportionment (docs §"Failure-driven route
	// memo" — a routed K-shard fan-out must not multiply route A's total
	// server-side memory exposure by K).
	MaxQueryMemoryBytes() int64
}

// SQLEmitter lowers a re-anchored shard plan into a parameterised
// ClickHouse SQL string + positional args. internal/chsql.Emit satisfies
// it; the solver takes it as an interface so this package never imports
// the emitter (scope + keeps the dependency cone tight). Emit is called
// for ALL K shards before any cursor opens, so an emit failure aborts the
// routed request with zero CH work.
type SQLEmitter interface {
	Emit(ctx context.Context, plan chplan.Node) (sql string, args []any, err error)
}

// breakerState is the stable, package-local mirror of chclient's breaker
// lifecycle, surfaced as a string by PeekBreakerState so the Executor can
// pre-flight without importing chclient's unexported breakerState enum.
const (
	// BreakerClosed is the normal operating state — routed requests may
	// proceed.
	BreakerClosed = "closed"
	// BreakerOpen is the fast-fail state — a routed request fails fast
	// with ErrCircuitOpen.
	BreakerOpen = "open"
	// BreakerHalfOpen admits exactly one probe; a routed fan-out would
	// burn the probe slot on a doomed K-shard request, so the Executor
	// fails fast WITHOUT consuming the probe.
	BreakerHalfOpen = "half-open"
)

// breakerPeeker reports the circuit-breaker lifecycle phase WITHOUT
// consuming a half-open probe, plus the breaker's OPEN-state recovery
// interval. *chclient.Client satisfies it via PeekBreakerState +
// BreakerRetryAfter. The Executor calls it once, before emitting, so a
// non-CLOSED breaker aborts the request before any CH work and — crucially
// — without spending the single half-open recovery probe on a fan-out.
//
// The interval belongs on this interface because the pre-flight builds its
// own fast-fail error rather than calling THROUGH the breaker: the
// `Retry-After` a route-B abort advertises must be the same interval the
// breaker enforces, and reading it here is what keeps the two in step.
type breakerPeeker interface {
	PeekBreakerState() string
	BreakerRetryAfter() time.Duration
}

// admitTopUp is the two-stage weighted-admission hook (docs/solver.md
// §"Execution and cursor model"). admit.Middleware already charged
// weight 1 at handler entry, before the route was known; at routing time
// the Executor asks for (P-1) ADDITIONAL units. TryAcquireTopUp is
// non-blocking: it returns the number of units actually obtained (0..want)
// plus a release closure. On a partial / zero grant the Executor clamps
// effective parallelism to 1+granted and runs — it NEVER 503s and NEVER
// proceeds at full P. The release closure is idempotent and runs exactly
// once at shardCursor.Close.
//
// *admit.Limiter satisfies it. A nil admitTopUp (admission disabled) grants
// the full request — the Executor treats it as "no cap".
type admitTopUp interface {
	TryAcquireTopUp(ctx context.Context, want int) (granted int, release func())
}
