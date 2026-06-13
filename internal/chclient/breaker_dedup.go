package chclient

import (
	"context"
	"sync/atomic"
)

// breakerDedup is a request-scoped, single-claim latch. It exists so a
// routed fan-out of K shard cursors against a degraded ClickHouse records
// AT MOST ONE breaker failure per logical request (docs §"Parallel
// execution" #6). Without it, every concurrent shard open that fails with a
// real (non-Canceled, non-241) CH-health error advances the shared breaker
// counter before the first failure's cancel propagates to its siblings — so
// one logical request could trip the threshold-5 breaker by up to gate/2 in a
// single shot, 503-ing all three heads far faster than a single route-A
// query ever would.
//
// It is born and dies with one request — there is NO cross-request state, so
// the no-caching invariant is untouched. Install a fresh latch on the SHARED
// shard-parent ctx with WithBreakerDedup before launching shards; every
// breaker.record under that ctx then consults the same latch and only the
// first real failure to win the CAS counts. Subsequent siblings' real
// failures are treated as breaker-NEUTRAL — they neither advance the failure
// counter nor corrupt the half-open state machine.
//
// The route-A path attaches no latch: breakerDedupFromContext returns nil and
// record() counts exactly as it does today (byte-unchanged behaviour).
type breakerDedup struct {
	// claimed is set by the first real failure to reach record() under this
	// request. The CAS winner counts; every later real failure observes the
	// claim and is treated as neutral.
	claimed atomic.Bool
}

// claim attempts to take the single per-request failure slot. It returns true
// for exactly ONE caller — the first real failure of the request — and false
// for every subsequent caller. A nil latch (route-A path) always returns true
// so the un-opted-in caller counts every failure exactly as today.
func (d *breakerDedup) claim() bool {
	if d == nil {
		return true
	}
	return d.claimed.CompareAndSwap(false, true)
}

type breakerDedupKeyType struct{}

var breakerDedupKey = breakerDedupKeyType{}

// WithBreakerDedup installs a FRESH per-request breaker-dedup latch on ctx and
// returns the derived context. The solver calls this once on the shared
// shard-parent ctx (the cause-carrying ctx all K shards derive from) before
// launching shards, so every shard's QueryCursor → breaker.record consults the
// same latch and the first real failure counts while siblings stay neutral.
//
// Each call installs a brand-new latch, so the latch is strictly per-request:
// re-deriving on a parent that already carries one replaces it (the new
// request owns a clean slot). Route-A callers never call this, so their
// record() path is unaffected.
func WithBreakerDedup(ctx context.Context) context.Context {
	return context.WithValue(ctx, breakerDedupKey, &breakerDedup{})
}

// breakerDedupFromContext returns the per-request latch on ctx, or nil when
// the request did not install one (the single-statement route-A path).
func breakerDedupFromContext(ctx context.Context) *breakerDedup {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(breakerDedupKey).(*breakerDedup)
	return v
}

// ClaimBreakerDedup reports whether a real failure observed under ctx should
// COUNT against the breaker, consuming the per-request latch exactly as
// breaker.record does. It returns true for the first real failure of a latched
// request (and for every failure on a route-A request with no latch), false
// for deduped siblings. Exported so the sharded solver's executor tests can
// drive the same per-request dedup the data-plane record() path uses without
// reaching into the unexported breaker — the test counts only the calls that
// ClaimBreakerDedup admits and asserts that K concurrent shard opens advance
// the counter by exactly 1.
func ClaimBreakerDedup(ctx context.Context) bool {
	return breakerDedupFromContext(ctx).claim()
}
