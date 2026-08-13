package chclient

import (
	"context"
	"errors"
	"fmt"
	"math"
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
// 256 MiB matches maxLogPeekBytes (the Loki line-peek sibling). The ceiling sits
// at the OOM-adjacent point, not a query-restrictive one — TestDrainByteBudget_
// CeilingHeadroom measures the real charge: a 1000-trace / 20-service search
// charges ~16 KB (17,000× margin, because resource attrs intern and share), and
// a realistic 10k-distinct-attr-span trace-by-id charges ~12 MiB (21× margin).
// To reach 256 MiB a trace needs ~200k+ spans or genuinely fat attrs — i.e. it
// already holds 256+ MiB of attribute maps on the heap, the OOM itself. So a
// crossing 422s only an OOM-scale query, never a servable one.
const maxTempoSpanDrainBytes = 256 << 20

// ErrDrainBytesExceeded is the sentinel matched (via errors.Is) when a drain
// crosses its cumulative result-byte budget — the Tempo span search's
// wide-projection maps, or the PromQL matrix drain's per-row native-histogram
// payload. It maps to the same resource-exhausted rejection (Tempo 422, Prom
// 422) as ErrTooManySamples — the byte-axis sibling of the row-axis sample
// budget, and the Go-heap sibling of the CH-side max_memory_usage cap.
var ErrDrainBytesExceeded = errors.New("query drain byte budget exceeded")

// DrainByteBudgetError wraps ErrDrainBytesExceeded and names the configured
// ceiling, mirroring *TooManySamplesError so the handler + gRPC error mappers
// classify it in the same resource-exhausted branch.
type DrainByteBudgetError struct{ Limit int64 }

func (e *DrainByteBudgetError) Error() string {
	return fmt.Sprintf("query drain exceeded the %d-byte result budget", e.Limit)
}

func (e *DrainByteBudgetError) Unwrap() error { return ErrDrainBytesExceeded }

// DrainByteBudget is a per-REQUEST cap on the cumulative result bytes a query
// may drain across all its cursors — the byte-axis sibling of SampleBudget. It
// is attached to the request context by the read path that wants it
// (WithDrainByteBudget): the Tempo span search, which charges its
// wide-projection attribute maps, and the PromQL sample drain, which charges
// those plus each row's native-histogram payload. A drain whose context carries
// no budget never charges. Lifecycle: born and dies with one request, no
// cross-request state.
type DrainByteBudget struct {
	// remaining is the wide-projection bytes the request may still drain
	// across all its cursors. Decremented atomically as each unique decoded
	// attribute map is charged; the limit is crossed when it would go negative.
	remaining atomic.Int64
	// limit is the original ceiling, carried so a *DrainByteBudgetError can
	// report the configured cap rather than the residual.
	limit int64
	// peak is the high-water of cumulative charged bytes over the request's
	// lifetime — the actual Go-heap the wide projection reached. Readable after
	// a drain (Peak) so an observe-only rollout / e2e can report the real charge
	// distribution and confirm the ceiling is never legitimately approached.
	peak atomic.Int64
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

// matrixDrainBytesPerSample is the Go-heap allowance the PromQL sample-drain
// budget grants for each row the configured MaxQuerySamples already admits: the
// width of ONE buffered float Sample — a 16-byte string header, the 8-byte
// pointer to a label map interned once per series, a 24-byte time.Time, a
// float64, a SeriesID and the two nil map/pointer slots — rounded up to a power
// of two. So maxSamples × this const is (approximately) the Go heap a FLOAT
// matrix drained to the row cap already occupies.
//
// Sizing the byte ceiling off the row cap is what makes the two axes agree. The
// row budget admits the same NUMBER of rows whether each carries a float64 or a
// native histogram, but a histogram row additionally owns a heap HistogramValue
// and two []float64 bucket ladders, so the same maxSamples admits one to two
// orders of magnitude more bytes for a histogram matrix than for a float one
// (issue #2038). Because the charge is only the per-series label maps plus the
// per-row histogram payload, a float matrix stays orders of magnitude under this
// ceiling and behaves exactly as it did before it existed; a histogram matrix
// trips at the point where its Go heap reaches what a float matrix of
// maxSamples rows would have cost.
const matrixDrainBytesPerSample = 128

// NewMatrixDrainBudget returns the per-request Go-heap byte budget for a PromQL
// sample drain (/api/v1/query and /api/v1/query_range), sized off the SAME
// configured row cap the cursor enforces as maxSamples
// (Config.MaxQuerySamples). A non-positive maxSamples — the -1 "sample budget
// deliberately disabled" sentinel — yields an inert budget, so switching the row
// cap off switches the byte cap off with it rather than silently substituting a
// bound the operator did not ask for.
func NewMatrixDrainBudget(maxSamples int64) *DrainByteBudget {
	switch {
	case maxSamples <= 0:
		return NewDrainByteBudget(0)
	case maxSamples > math.MaxInt64/matrixDrainBytesPerSample:
		// A row cap this large is already "effectively unlimited"; clamp
		// rather than wrap into a negative (inert-looking) ceiling.
		return NewDrainByteBudget(math.MaxInt64)
	default:
		return NewDrainByteBudget(maxSamples * matrixDrainBytesPerSample)
	}
}

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
	rem := b.remaining.Add(-n)
	// Record the high-water (best-effort monotonic CAS) even on the crossing
	// draw, so Peak reflects how far over the ceiling the request reached.
	charged := b.limit - rem
	for {
		p := b.peak.Load()
		if charged <= p || b.peak.CompareAndSwap(p, charged) {
			break
		}
	}
	return rem >= 0
}

// Peak returns the high-water of cumulative charged bytes over the request — the
// actual Go-heap the wide projection reached, for observability. 0 on a nil or
// inert budget.
func (b *DrainByteBudget) Peak() int64 {
	if b == nil {
		return 0
	}
	return b.peak.Load()
}

// SumWideProjectionBytes charges each of labelSets against a fresh,
// effectively-unlimited budget and returns the resulting Peak — i.e. the
// SAME per-map cost formula the production rowsCursor / columnarCursor
// decode paths charge (labelMapBytes), applied to caller-supplied label
// sets rather than a live decode. Exported so out-of-package corpus-floor
// checks (e.g. compatibility/tempo's fixture-derived measurement) can
// grade real data against the production accounting without duplicating
// its arithmetic — the whole point of grounding a ceiling in measurement
// is that the measurement uses the exact formula the ceiling enforces.
func SumWideProjectionBytes(labelSets []map[string]string) int64 {
	b := NewDrainByteBudget(unlimitedMeasurementBudget)
	for _, m := range labelSets {
		b.consume(labelMapBytes(m))
	}
	return b.Peak()
}

// unlimitedMeasurementBudget is the ceiling SumWideProjectionBytes measures
// against — large enough that no realistic corpus trips it (the budget
// exists only to drive consume's peak-tracking, never to reject).
const unlimitedMeasurementBudget = 1 << 62

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
// also retains a format.CanonicalKey string of the same content per unique series,
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

// histogramValueFixedBytes approximates the heap one decoded *HistogramValue
// retains independently of its bucket count: the heap-allocated struct itself
// (four float64 scalars, three int32s and two slice headers, padded to 104
// bytes on a 64-bit platform) plus the 8-byte Sample.Histogram pointer that
// holds it.
const histogramValueFixedBytes = 112

// bucketCountBytes is the width of one native-histogram bucket-ladder element.
// The ladders are []float64 rather than integer counts because a
// histogram-VALUED rate() / increase() produces fractional counts — see
// HistogramValue.Count.
const bucketCountBytes = 8

// histogramValueBytes returns the on-heap byte width a cursor RETAINS for one
// decoded native-histogram sample: the fixed struct plus both bucket ladders.
//
// Unlike labelMapBytes this is charged per ROW, not per unique series. Both
// decode paths copy each row's ladders into FRESH slices (ch-go's ColArr.Row
// on the columnar path, clickhouse-go's Scan on the row path), so every
// buffered row owns its own histogram — there is nothing shared to dedup the
// way an interned label map is.
func histogramValueBytes(h *HistogramValue) int64 {
	if h == nil {
		return 0
	}
	buckets := int64(len(h.PositiveBucketCounts)) + int64(len(h.NegativeBucketCounts))
	return histogramValueFixedBytes + buckets*bucketCountBytes
}
