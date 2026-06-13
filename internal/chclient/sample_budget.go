package chclient

import (
	"context"
	"sync/atomic"
)

// SampleBudget is a per-REQUEST cap on the total number of Sample rows a
// request may drain across ALL the cursors it opens, collectively. It
// exists for the sharded-pushdown solver: when one logical query
// fans out into N concurrent shard cursors, the per-cursor
// Config.MaxQuerySamples limit would let the request drain up to N times
// the configured cap in aggregate. A SampleBudget threads a single
// shared counter through every cursor of the request so the 422
// max-samples rejection trips at the request total, not per cursor.
//
// Lifecycle: a SampleBudget is born and dies with one request. It carries
// NO cross-request state — construct a fresh one per request via
// NewSampleBudget, attach it to the request context with WithSampleBudget,
// and let it fall out of scope when the request completes. It is NOT a
// pool, a quota refilled over time, or anything shared between requests;
// reusing one across requests would leak one request's drained-sample
// count into the next.
//
// The shared counter is consumed atomically, so the concurrent shard
// cursors of one fan-out can decrement it from multiple goroutines
// without a lock. Crossing the budget surfaces the IDENTICAL
// *TooManySamplesError (errors.Is ErrTooManySamples) that the per-cursor
// limit produces — the verbatim upstream max-samples 422 message and
// behaviour are the same whether the limit came from the per-cursor max
// or the shared budget.
type SampleBudget struct {
	// remaining is the number of samples the request may still drain
	// across all its cursors. Decremented atomically as each row is
	// decoded; the limit is crossed when it would go negative.
	remaining atomic.Int64
	// limit is the original budget, carried so a *TooManySamplesError
	// can report the configured cap (the same Limit field the
	// per-cursor path reports).
	limit int64
}

// NewSampleBudget returns a SampleBudget admitting up to max total
// samples across every cursor of one request. max must be > 0 to be
// meaningful; a non-positive max yields a budget that is never consulted
// (the cursor falls back to its per-cursor limit) — see budgetFromContext.
func NewSampleBudget(max int64) *SampleBudget {
	b := &SampleBudget{limit: max}
	b.remaining.Store(max)
	return b
}

// consume draws n samples against the shared budget. It returns true when
// the draw fits (the request may keep draining) and false when it would
// cross the budget — at which point the caller aborts iteration with a
// *TooManySamplesError{Limit: b.Limit()}.
//
// A non-positive limit is "unlimited": consume short-circuits to true
// without touching remaining, mirroring the per-cursor maxSamples == 0
// contract. The decrement is otherwise atomic so the concurrent shard
// cursors of one fan-out share the counter without a lock. Once remaining
// has gone negative the budget stays tripped for every later consume
// across every cursor.
func (b *SampleBudget) consume(n int64) bool {
	if b == nil || b.limit <= 0 {
		return true
	}
	return b.remaining.Add(-n) >= 0
}

// Consume draws n samples from the budget, returning false once the
// request has exhausted it. Exported for the solver's shared-budget tests;
// the data-plane cursor calls the unexported consume.
func (b *SampleBudget) Consume(n int64) bool { return b.consume(n) }

// Limit returns the original per-request budget (the configured cap),
// carried so a *TooManySamplesError can name the limit rather than the
// residual count. Returns 0 on a nil budget.
func (b *SampleBudget) Limit() int64 {
	if b == nil {
		return 0
	}
	return b.limit
}

// active reports whether the budget carries a positive limit and should
// therefore be consulted. A non-positive limit (e.g. an unset request)
// is inert: the cursor falls back to its per-cursor max-samples limit.
func (b *SampleBudget) active() bool { return b != nil && b.limit > 0 }

// sampleBudgetKey is the unexported context key under which a
// *SampleBudget travels. Unexported so no other package can collide with
// or overwrite the request's budget.
type sampleBudgetKey struct{}

// WithSampleBudget attaches b to ctx so every cursor the request opens
// (via QueryCursor / Query) shares b's counter. Pass the derived context
// into the read-path calls; cursors opened from a context WITHOUT a
// budget fall back to their per-cursor Config.MaxQuerySamples limit.
func WithSampleBudget(ctx context.Context, b *SampleBudget) context.Context {
	return context.WithValue(ctx, sampleBudgetKey{}, b)
}

// budgetFromContext returns the *SampleBudget attached to ctx, or nil
// when none is present or the attached one is inert (non-positive
// limit). A nil result means "use the per-cursor limit".
func budgetFromContext(ctx context.Context) *SampleBudget {
	if ctx == nil {
		return nil
	}
	b, _ := ctx.Value(sampleBudgetKey{}).(*SampleBudget)
	if !b.active() {
		return nil
	}
	return b
}

// SampleBudgetFromContext returns the shared *SampleBudget attached to ctx
// by WithSampleBudget, or nil. Exported so the sharded solver's tests can
// confirm the per-request budget is threaded into every shard's ctx; the
// data-plane cursor uses the unexported budgetFromContext.
func SampleBudgetFromContext(ctx context.Context) *SampleBudget {
	return budgetFromContext(ctx)
}
