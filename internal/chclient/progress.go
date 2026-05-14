package chclient

import (
	"context"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/telemetry"
)

// WithProgressFor returns a context derived from ctx that asks the
// ClickHouse driver to invoke a progress callback on every Progress
// packet streamed back from the server. The callback aggregates the
// per-packet (Rows, Bytes) deltas and, when the query finishes (i.e.
// the call site closes its cursor / returns), records the totals on
// the cerberus.clickhouse.{rows,bytes}_read histograms labelled with
// ql.
//
// Rationale for going through clickhouse.WithProgress rather than the
// older X-ClickHouse-Summary HTTP header: clickhouse-go/v2 uses the
// native protocol by default, where Progress is a streamed packet not
// an HTTP header. The progress callback is the only stable surface the
// driver exposes that covers both the HTTP and native paths.
//
// Aggregation lives in a heap-allocated closure rather than a context
// value because the driver invokes the callback off-goroutine from the
// QueryRow / Query goroutine — we want all increments to land on the
// same totals regardless of which goroutine ran them. The closure
// captures pointers; the (rows, bytes) atomicity is unnecessary here
// because the driver guarantees a single in-flight Progress dispatcher
// per query.
//
// The recorder is invoked from a finalizer-like helper rather than
// from the progress callback itself: each Progress packet is a
// snapshot, not a delta, so summing every packet would double-count.
// Instead the closure latches the latest snapshot and the per-query
// flush captures it once at end-of-query.
func WithProgressFor(ctx context.Context, ql string) context.Context {
	rec := &progressRecorder{ql: ql, ctx: ctx}
	ctx = withRecorder(ctx, rec)
	return clickhouse.Context(ctx, clickhouse.WithProgress(rec.onProgress))
}

// progressRecorder latches the most recent Progress snapshot for a
// single query. The driver may emit several packets as the server
// streams partial results; we keep only the final one because each
// packet reports running totals (rows-so-far / bytes-so-far) rather
// than per-packet deltas. flushOnce is wired via the cursor's Close
// path; for non-cursor queries the synchronous Query call site invokes
// flush explicitly after the call returns.
type progressRecorder struct {
	ql    string
	ctx   context.Context
	rows  uint64
	bytes uint64
}

// onProgress is the driver-facing callback. It overwrites the latched
// snapshot — see the recorder docstring for why we don't accumulate.
func (r *progressRecorder) onProgress(p *clickhouse.Progress) {
	if p == nil {
		return
	}
	if p.Rows > r.rows {
		r.rows = p.Rows
	}
	if p.Bytes > r.bytes {
		r.bytes = p.Bytes
	}
}

// flush records the latched (rows, bytes) on the histograms. Safe to
// call with a nil-progress recorder (no-op).
func (r *progressRecorder) flush() {
	if r == nil {
		return
	}
	telemetry.RecordClickHouseProgress(r.ctx, r.ql, r.rows, r.bytes)
}

// recorderFromContext digs the progressRecorder out of ctx by the
// clickhouse driver's own context value — but the driver doesn't
// surface the option struct, so instead we attach the recorder under
// our own private key alongside the clickhouse.WithProgress option.
// The callsite plumbing (WithProgressFor) sets both; this getter is
// the read side used by the synchronous Query / cursor paths to flush
// the histograms after the query completes.
func recorderFromContext(ctx context.Context) *progressRecorder {
	v, _ := ctx.Value(progressKey).(*progressRecorder)
	return v
}

type progressKeyType struct{}

var progressKey = progressKeyType{}

// withRecorder is the bookkeeping side of WithProgressFor — it stores
// the recorder under progressKey so recorderFromContext can pull it
// out after the query runs.
func withRecorder(ctx context.Context, rec *progressRecorder) context.Context {
	return context.WithValue(ctx, progressKey, rec)
}
