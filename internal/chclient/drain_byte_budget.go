package chclient

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
)

// maxTempoSpanDrainBytes hard-caps the cumulative wide-projection bytes — the
// ResourceAttributes + SpanAttributes maps, folded into Sample.Labels — that a
// single Tempo /api/search (or gRPC Search) drain may buffer into the Go heap
// before the cursor aborts fail-closed.
//
// It is the byte axis that every other Tempo bound structurally misses: the
// two-phase split and the trace-limit pushdown cap the TRACE count, the
// resource-bound gate caps TIME (partition prune), and MaxQuerySamples caps
// ROWS — but the OOM cost is BYTES = matched-span-count × per-span-map-width. A
// selective-but-fat-map search (a few thousand error spans each carrying a
// ~60 KB stacktrace/body map) sails under every row/trace/time threshold and
// heaps gigabytes. This budget is charged INCREMENTALLY during the streaming
// drain, so the Go-heap high-water is bounded to the ceiling plus one CH block
// — the full wide set never materialises first.
//
// 256 MiB matches maxLogPeekBytes (the Loki line-peek sibling). It is a byte
// count of attribute-map key+value lengths, so a legitimate result would need a
// genuinely enormous matched-span × map-width product to approach it; the
// compatibility/tempo corpus pass confirms no valid Matched set does before this
// ceiling ships.
const maxTempoSpanDrainBytes = 256 << 20

// ErrDrainBytesExceeded is the sentinel matched (via errors.Is) when a Tempo
// span drain crosses its cumulative wide-projection byte budget. It maps to the
// same resource-exhausted rejection (Tempo 422) as ErrTooManySamples — the
// byte-axis sibling of the row-axis sample budget, and the Go-heap sibling of
// the CH-side max_memory_usage cap.
var ErrDrainBytesExceeded = errors.New("tempo span drain byte budget exceeded")

// DrainByteBudgetError wraps ErrDrainBytesExceeded and names the configured
// ceiling, mirroring *TooManySamplesError so the handler + gRPC error mappers
// classify it in the same resource-exhausted branch.
type DrainByteBudgetError struct{ Limit int64 }

func (e *DrainByteBudgetError) Error() string {
	return fmt.Sprintf("tempo span drain exceeded the %d-byte wide-projection budget", e.Limit)
}

func (e *DrainByteBudgetError) Unwrap() error { return ErrDrainBytesExceeded }

// DrainByteBudget is a per-REQUEST cap on the cumulative wide-projection bytes a
// Tempo span search may drain across all its cursors — the byte-axis sibling of
// SampleBudget. It is attached to the request context by the Tempo read path
// ONLY (WithDrainByteBudget), so the cursor charges bytes for span searches and
// leaves every PromQL / LogQL drain untouched (no budget on the context → no
// charge). Lifecycle: born and dies with one request, no cross-request state.
type DrainByteBudget struct {
	// remaining is the wide-projection bytes the request may still drain
	// across all its cursors. Decremented atomically as each unique decoded
	// attribute map is charged; the limit is crossed when it would go negative.
	remaining atomic.Int64
	// limit is the original ceiling, carried so a *DrainByteBudgetError can
	// report the configured cap rather than the residual.
	limit int64
}

// NewDrainByteBudget returns a budget admitting up to max wide-projection bytes
// across every cursor of one request. A non-positive max is inert (never
// consulted) — see drainByteBudgetFromContext.
func NewDrainByteBudget(max int64) *DrainByteBudget {
	b := &DrainByteBudget{limit: max}
	b.remaining.Store(max)
	return b
}

// NewTempoSpanDrainBudget returns the default-on wide-projection byte budget for
// a Tempo span search, sized to maxTempoSpanDrainBytes. The Tempo read path
// attaches it to every span-search request context so the const stays internal
// to chclient (no exported knob, no per-request override — the fixed default-on
// ratchet).
func NewTempoSpanDrainBudget() *DrainByteBudget { return NewDrainByteBudget(maxTempoSpanDrainBytes) }

// consume draws n wide-projection bytes against the shared budget. Returns true
// when the draw fits and false when it would cross the ceiling — at which point
// the caller aborts iteration with a *DrainByteBudgetError{Limit: b.Limit()}.
// A non-positive limit is "unlimited". The decrement is atomic so concurrent
// shard cursors share the counter without a lock; once negative it stays
// tripped for every later consume.
func (b *DrainByteBudget) consume(n int64) bool {
	if b == nil || b.limit <= 0 {
		return true
	}
	return b.remaining.Add(-n) >= 0
}

// Limit returns the configured ceiling (0 on a nil budget), carried so the
// over-budget error names the cap rather than the residual.
func (b *DrainByteBudget) Limit() int64 {
	if b == nil {
		return 0
	}
	return b.limit
}

// active reports whether the budget carries a positive limit and should be
// consulted. A non-positive limit is inert.
func (b *DrainByteBudget) active() bool { return b != nil && b.limit > 0 }

// drainByteBudgetKey is the unexported context key under which a
// *DrainByteBudget travels.
type drainByteBudgetKey struct{}

// WithDrainByteBudget attaches b to ctx so every cursor the Tempo span request
// opens shares b's counter. Cursors opened from a context WITHOUT a budget (the
// PromQL / LogQL paths) never charge bytes.
func WithDrainByteBudget(ctx context.Context, b *DrainByteBudget) context.Context {
	return context.WithValue(ctx, drainByteBudgetKey{}, b)
}

// drainByteBudgetFromContext returns the *DrainByteBudget attached to ctx, or
// nil when none is present or the attached one is inert. A nil result means
// "do not charge bytes on this drain".
func drainByteBudgetFromContext(ctx context.Context) *DrainByteBudget {
	if ctx == nil {
		return nil
	}
	b, _ := ctx.Value(drainByteBudgetKey{}).(*DrainByteBudget)
	if !b.active() {
		return nil
	}
	return b
}

// perMapEntryHeapBytes approximates the Go-runtime heap the cursor RETAINS per
// attribute-map entry beyond the raw string content: two ~16-byte string headers
// (key + value) plus amortised map-bucket overhead. Included so the byte ceiling
// tracks the real Go-heap high-water rather than just the wire content — a
// content-only count under-charges the retained heap several-fold and would fire
// the gate well past the memory it is meant to bound.
const perMapEntryHeapBytes = 48

// DrainByteBudgetFromContext returns the *DrainByteBudget attached to ctx by
// WithDrainByteBudget, or nil. Exported so the Tempo handler tests can confirm
// every wide-map drain endpoint attaches the budget (the no-bypass ratchet).
func DrainByteBudgetFromContext(ctx context.Context) *DrainByteBudget {
	return drainByteBudgetFromContext(ctx)
}

// labelMapBytes returns the on-heap byte width the cursor RETAINS for one unique
// interned attribute map. That is NOT just the key/value content: internLabels
// also retains a canonicalLabelKey string of the same content per unique series,
// and each entry carries Go map-header + string-header overhead. So the charge
// is ~2× the content (map + canonical-key duplicate) plus per-entry overhead —
// a deliberately conservative estimate of the true retained heap.
func labelMapBytes(m map[string]string) int64 {
	var content int64
	for k, v := range m {
		content += int64(len(k)) + int64(len(v))
	}
	return content*2 + int64(len(m))*perMapEntryHeapBytes
}
