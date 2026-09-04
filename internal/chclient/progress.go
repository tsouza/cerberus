package chclient

import (
	"context"
	"sync"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/actuals"
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
	// Issue #2789: a route-B dispatch calls WithProgressFor TWICE on the
	// SAME logical request — once in routeBExecCtx (the outer ctx every
	// shard's own dispatch context derives from) and once more per shard,
	// inside internal/solver/executor.go's runShard ("Per-shard progress
	// recorder (one per ctx key)" — its own doc explains why a FRESH
	// recorder per shard is mandatory: sharing one would corrupt the
	// rows/bytes histograms across concurrent shards). That second call
	// would otherwise silently DISCARD whatever WithActualsCapture wired
	// onto the outer recorder — a NEW progressRecorder starts with
	// shapeID=="" regardless of what an ancestor ctx carried. actualsIntent
	// closes that gap: WithActualsCapture stores the (tracker, shapeID)
	// pair under its OWN, separate context key — a value that (unlike the
	// recorder itself) survives being shadowed, because nothing here
	// overwrites it — and EVERY WithProgressFor call, including a later
	// per-shard one, re-applies it to whichever fresh recorder it just
	// created. Route A never exercises the second read (it calls
	// WithProgressFor exactly once), so this is purely additive there.
	if intent, ok := actualsIntentFromContext(ctx); ok {
		rec.tracker = intent.tracker
		rec.shapeID = intent.shapeID
	}
	ctx = withRecorder(ctx, rec)
	opts := []clickhouse.QueryOption{clickhouse.WithProgress(rec.onProgress)}
	if rec.shapeID != "" {
		opts = append(opts, clickhouse.WithProfileEvents(rec.onProfileEvents))
	}
	return clickhouse.Context(ctx, opts...)
}

// actualsIntent is the (tracker, shapeID) pair WithActualsCapture stores on
// ctx — see WithProgressFor's own doc for why this rides as a SEPARATE,
// non-recorder-bound context value.
type actualsIntent struct {
	tracker *actuals.Tracker
	shapeID string
}

type actualsIntentKeyType struct{}

var actualsIntentKey = actualsIntentKeyType{}

func actualsIntentFromContext(ctx context.Context) (actualsIntent, bool) {
	intent, ok := ctx.Value(actualsIntentKey).(actualsIntent)
	return intent, ok
}

// WithActualsCapture extends ctx to capture this dispatch's peak memory
// (via the native protocol's ProfileEvents packets) and record the whole
// (rows, bytes, peak-memory) observation into tracker, keyed by shapeID,
// once the dispatch completes (issue #2789's ProfileEvents/progress-packet
// fast path). A nil tracker or an empty shapeID leaves ctx UNCHANGED —
// fail-open, exactly like every other optional dispatch annotation in this
// package (WithResponseShape, WithTSGridSetting, ...).
//
// Does TWO things, not one, and both are needed:
//
//  1. wires the CURRENT recorder directly, when ctx already carries one
//     from an earlier WithProgressFor call (the route-A shape — one
//     WithProgressFor call, no later one to inherit anything); a no-op
//     when none exists yet.
//  2. stamps actualsIntent onto ctx unconditionally, so a LATER
//     WithProgressFor call (the route-B per-shard shape — see that
//     function's own doc) re-applies it to its own fresh recorder even
//     though this call could not touch one directly.
//
// Every real call site in internal/engine calls WithProgressFor BEFORE this
// (mirroring chclient.WithResponseShape(chclient.WithProgressFor(ctx, ql))
// immediately above this file's own doc), so (1) is what actually fires in
// production; (2) is what makes route B's later, per-shard WithProgressFor
// call correct regardless.
//
// clickhouse.Context MERGES the new WithProfileEvents option onto whatever
// QueryOptions ctx already carries (client.go's own queryContext doc: "so
// stacking is safe and the existing options are preserved") — it does not
// replace the WithProgress callback WithProgressFor already installed, so
// both packet types keep flowing to the SAME recorder.
func WithActualsCapture(ctx context.Context, tracker *actuals.Tracker, shapeID string) context.Context {
	if tracker == nil || shapeID == "" {
		return ctx
	}
	ctx = context.WithValue(ctx, actualsIntentKey, actualsIntent{tracker: tracker, shapeID: shapeID})
	rec := recorderFromContext(ctx)
	if rec == nil {
		return ctx
	}
	rec.tracker = tracker
	rec.shapeID = shapeID
	return clickhouse.Context(ctx, clickhouse.WithProfileEvents(rec.onProfileEvents))
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

	// peakMemory, shapeID and tracker are set only when WithActualsCapture
	// (issue #2789) was layered on top of this recorder's ctx — see that
	// function's own doc. shapeID == "" (the zero value) means actuals
	// capture is off for this dispatch, matching the recorder's usual
	// zero-value-means-off posture.
	peakMemory uint64
	shapeID    string
	tracker    *actuals.Tracker
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

// profileEventMemoryTrackerPeakUsage is the ClickHouse ProfileEvents counter
// name carrying a query's peak memory usage in bytes — verified against a
// live ClickHouse 26.6 server (native protocol, WithProfileEvents): a plain
// `SELECT count() FROM ... WHERE ...` over an 8,192-row granule reported
// MemoryTrackerPeakUsage=301166 alongside MemoryTrackerUsage=174904 (the
// CURRENT, not peak, tracker reading at the time of that particular event —
// several ProfileEvents packets can arrive over a query's lifetime, one per
// server-side flush, so onProfileEvents latches the MAXIMUM seen across all
// of them, mirroring onProgress's own latch-the-running-total posture for
// rows/bytes). Neither name is documented in ClickHouse's own reference
// (docs/README.md's own citation for this feature only points at
// system.query_log), so this was confirmed empirically rather than assumed.
const profileEventMemoryTrackerPeakUsage = "MemoryTrackerPeakUsage"

// onProfileEvents is the driver-facing callback for the native protocol's
// ProfileEvents packets (issue #2789's ProfileEvents/progress-packet
// capture). Registered ONLY when WithActualsCapture has been layered onto
// this recorder's ctx — see that function's own doc for why this is not
// unconditional like onProgress: parsing ~80 named counters per packet has a
// real (if small) CPU/allocation cost the kill-switch-off default must not
// pay. The packets themselves are already streamed by the server for every
// query regardless (verified live: ClickHouse sends ServerProfileEvents
// whether or not the client registers a callback for it — see
// clickhouse-go/v2's WithoutProfileEvents, an explicit opt-OUT that only
// exists because the packets are on-by-default), so registering the
// callback adds no extra round trip — only the (already-flowing) packet's
// parse cost.
func (r *progressRecorder) onProfileEvents(events []clickhouse.ProfileEvent) {
	for _, e := range events {
		if e.Name != profileEventMemoryTrackerPeakUsage || e.Value < 0 {
			continue
		}
		v := uint64(e.Value) //nolint:gosec // G115 -- guarded by the e.Value < 0 check above; ClickHouse never reports a negative memory counter in practice, but the guard makes the conversion provably safe rather than merely assumed
		if v > r.peakMemory {
			r.peakMemory = v
		}
	}
}

// flush records the latched (rows, bytes) on the histograms and, when
// WithActualsCapture wired a tracker onto this dispatch, feeds the same
// observation (plus the latched peak memory) into it as the SourcePacket
// fast path (issue #2789). Safe to call with a nil-progress recorder
// (no-op).
//
// Issue #3033: when this recorder's ctx also carries a *ShardActualsFold
// (route B, K > 1 shards — see WithShardActualsFold's own doc), flush does
// NOT call tracker.RecordActual itself; it folds this shard's observation
// into the shared accumulator instead, which performs the SINGLE
// per-request RecordActual call once every shard has folded in. Route A,
// and a route-B dispatch with no fold wired (k < 2, or actuals capture
// off), are byte-unchanged: they still call tracker.RecordActual here,
// directly, exactly as before this issue.
func (r *progressRecorder) flush() {
	if r == nil {
		return
	}
	telemetry.RecordClickHouseProgress(r.ctx, r.ql, r.rows, r.bytes)
	if r.shapeID == "" {
		return
	}
	actual := actuals.Actual{
		ReadRows:   r.rows,
		ReadBytes:  r.bytes,
		PeakMemory: r.peakMemory,
	}
	if fold, ok := shardActualsFoldFromContext(r.ctx); ok {
		fold.add(actual)
		return
	}
	report, ok := r.tracker.RecordActual(r.shapeID, actual, actuals.SourcePacket)
	if ok && report.HasPredicted {
		telemetry.RecordEstimateDrift(r.ctx, report.Ratio, report.Alerting, actuals.SourcePacket.String())
	}
}

// ShardActualsFold folds a route-B K-shard dispatch's per-shard actuals
// captures into ONE per-request Actual observation before it reaches the
// tracker (issue #3033). Without it, each of route-B's K shards' own
// progressRecorder.flush() would call tracker.RecordActual independently —
// K calls against the ONE un-sharded RecordPredicted prediction the whole
// request made — corrupting the tracked EMA by roughly a factor of K.
//
// Folding rule:
//   - ReadRows / ReadBytes: SUM across shards — each shard scanned a
//     disjoint slice of the request's total.
//   - PeakMemory: MAX across shards, never summed. onProfileEvents already
//     latches the max PACKET within one shard's own dispatch; this is the
//     same idea applied ACROSS shards — ClickHouse's own per-query
//     MemoryTrackerPeakUsage is a single-process high-water mark, not
//     additive across K independent queries.
//
// Construct with WithShardActualsFold, which wires the SAME
// *ShardActualsFold pointer onto every shard's derived ctx via a context
// value — mirroring actualsIntent's own "survives being shadowed by a
// fresh recorder" shape (see WithProgressFor's doc) — so every shard's
// flush() feeds the SAME accumulator regardless of which goroutine it runs
// on. The driver invokes progress/ProfileEvents callbacks off-goroutine
// (WithProgressFor's own doc), and the K shards' cursors can each complete
// on a different goroutine, so every mutation below is guarded by mu.
//
// The shard whose add() call observes completed == k — necessarily the
// LAST one, since completed only increases — performs the single
// RecordActual (+ RecordEstimateDrift) call, inline, still holding mu. So
// "exactly one call per request" is enforced by construction: no separate
// finalize step exists for a caller to forget. A dispatch that never gets
// all k shards to flush (e.g. a shard failed to even open its cursor, so
// its own defer'd CloseCursor / progressRecorder.flush never runs) simply
// never fires — recording a (k-1)-of-k partial sum would reintroduce the
// very undercount class this fix exists to close, just at a smaller K.
type ShardActualsFold struct {
	// ctx is the per-request ctx WithShardActualsFold was called with — NOT
	// any one shard's own derived ctx — so the single RecordEstimateDrift
	// call this fold makes is request-scoped, like RecordPredicted's own
	// call was, rather than tied to whichever shard happened to finish last.
	ctx     context.Context
	tracker *actuals.Tracker
	shapeID string
	k       int

	mu         sync.Mutex
	completed  int
	rows       uint64
	bytes      uint64
	peakMemory uint64
}

type shardActualsFoldKeyType struct{}

var shardActualsFoldKey = shardActualsFoldKeyType{}

// WithShardActualsFold wires a fresh *ShardActualsFold onto ctx for a
// route-B dispatch of k shards, so every shard's progressRecorder.flush()
// (see that method's own "if fold, ok :=" branch) feeds the SAME
// accumulator instead of calling tracker.RecordActual directly.
//
// Returns ctx UNCHANGED — no fold wired — when actuals capture is not
// active for this dispatch (no actualsIntent on ctx yet; mirrors
// WithActualsCapture's own fail-open posture) or k < 2 (route A, or a
// degenerate single-shard route-B decision: exactly the shape that already
// records correctly today, so folding would be pure overhead for zero
// benefit). Callers MUST treat "no *ShardActualsFold on the returned ctx"
// as "nothing to finalize" — there is no separate off-path to remember.
//
// Call this ONCE per dispatch, on the SAME ctx that gets threaded into
// every shard's own derived context (solver.Executor.Execute's ctx
// parameter, BEFORE any per-shard ctx is derived from it) — a fold wired
// onto anything narrower than that would not reach every shard.
func WithShardActualsFold(ctx context.Context, k int) context.Context {
	intent, ok := actualsIntentFromContext(ctx)
	if !ok || k < 2 {
		return ctx
	}
	fold := &ShardActualsFold{ctx: ctx, tracker: intent.tracker, shapeID: intent.shapeID, k: k}
	return context.WithValue(ctx, shardActualsFoldKey, fold)
}

// ShardActualsFoldFromContext returns the *ShardActualsFold WithShardActualsFold
// wired onto ctx, if any. Exported for internal/solver's own tests, which
// verify the Executor wires the SAME fold onto every shard's ctx (and wires
// none at all for k < 2) without internal/solver importing internal/actuals
// itself (its own dependency allowlist in .go-arch-lint.yml deliberately
// omits it — the fold's tracker/Actual plumbing must stay inside chclient).
func ShardActualsFoldFromContext(ctx context.Context) (*ShardActualsFold, bool) {
	return shardActualsFoldFromContext(ctx)
}

func shardActualsFoldFromContext(ctx context.Context) (*ShardActualsFold, bool) {
	f, ok := ctx.Value(shardActualsFoldKey).(*ShardActualsFold)
	return f, ok
}

// add folds one shard's (rows, bytes, peakMemory) into the running totals —
// see the type doc for the SUM/MAX split and the exactly-once completion
// contract. Safe for concurrent use.
func (f *ShardActualsFold) add(a actuals.Actual) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows += a.ReadRows
	f.bytes += a.ReadBytes
	if a.PeakMemory > f.peakMemory {
		f.peakMemory = a.PeakMemory
	}
	f.completed++
	if f.completed != f.k {
		return
	}
	report, ok := f.tracker.RecordActual(f.shapeID, actuals.Actual{
		ReadRows:   f.rows,
		ReadBytes:  f.bytes,
		PeakMemory: f.peakMemory,
	}, actuals.SourcePacket)
	if ok && report.HasPredicted {
		telemetry.RecordEstimateDrift(f.ctx, report.Ratio, report.Alerting, actuals.SourcePacket.String())
	}
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
