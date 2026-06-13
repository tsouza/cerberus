package solver

import (
	"context"

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
//     breakerPeeker (PeekBreakerState).
//   - *admit.Limiter satisfies admitTopUp (TryAcquireTopUp).
//   - internal/chsql provides the SQLEmitter (wired by the engine adapter
//     in the next PR).

// CursorQuerier opens a streaming cursor over a CH result set. It is the
// single data-plane capability the Executor needs; *chclient.Client
// satisfies it via QueryCursor.
type CursorQuerier interface {
	QueryCursor(ctx context.Context, sql string, args ...any) (chclient.Cursor, error)
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
// consuming a half-open probe. *chclient.Client satisfies it via
// PeekBreakerState. The Executor calls it once, before emitting, so a
// non-CLOSED breaker aborts the request before any CH work and — crucially
// — without spending the single half-open recovery probe on a fan-out.
type breakerPeeker interface {
	PeekBreakerState() string
}

// admitTopUp is the two-stage weighted-admission hook (docs §Parallel #2).
// admit.Middleware already charged weight 1 at handler entry, before the
// route was known; at routing time the Executor asks for (P-1) ADDITIONAL
// units. TryAcquireTopUp is non-blocking: it returns the number of units
// actually obtained (0..want) plus a release closure. On a partial / zero
// grant the Executor clamps effective parallelism to 1+granted and runs —
// it NEVER 503s and NEVER proceeds at full P. The release closure is
// idempotent and runs exactly once at shardCursor.Close.
//
// *admit.Limiter satisfies it. A nil admitTopUp (admission disabled) grants
// the full request — the Executor treats it as "no cap".
type admitTopUp interface {
	TryAcquireTopUp(ctx context.Context, want int) (granted int, release func())
}
