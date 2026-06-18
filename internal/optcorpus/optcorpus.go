// Package optcorpus implements the asynchronous system.query_log
// performance-corpus reconciler. It closes the loop between a plan shape
// cerberus emitted (the literal-free cerb:<root>[;mod...] shape id from
// internal/engine.planShapeID, stamped into ClickHouse log_comment) and the
// server-side cost ClickHouse actually paid for it (read_rows, read_bytes,
// query_duration_ms, memory_usage, ProfileEvents), building a durable corpus
// an operator can mine to decide which optimizations to enable.
//
// The reconciler keeps a BOUNDED in-memory ring of recently-dispatched
// cerberus query_ids (the per-dispatch "<traceID>-<spanID>-<counter>" id the
// engine fixes at the dispatch seam and stamps as the CH query_id, the unique
// join key into system.query_log), periodically joins them back to
// system.query_log for finished rows, and appends the
// (shape-id, enabled-opts, timings) tuples to a durable sink (a JSONL file in
// v1; the Row shape is stable so a later CH-table sink is a column-for-column
// swap).
//
// It is production-only: chDB (the parity test substrate) has no
// system.query_log, so the reconciler is never started under the chDB build.
// Errors are LOGGED, never fatal — a query_log read failure degrades the
// corpus, it never takes the binary down.
package optcorpus

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Record is one dispatched query's identity, registered (via Observe) when
// cerberus sends the query. The QueryID is the join key into system.query_log;
// the rest is the metadata the reconciler joins onto each finished row.
type Record struct {
	// QueryID is the ClickHouse query_id (the per-dispatch
	// "<traceID>-<spanID>-<counter>" id; the cerberus trace id is its prefix),
	// the unique join key into system.query_log.
	QueryID string
	// ShapeID is the literal-free cerb:<root>[;mod...] plan shape id from
	// engine.planShapeID.
	ShapeID string
	// Opts is the resolved enabled-opts that rode this query (the EnabledSet
	// ids active at dispatch).
	Opts []string
	// Language is the query language: "promql" | "logql" | "traceql".
	Language string
}

// Row is the durable corpus tuple written to the sink. The field shape is
// stable so a later ClickHouse-table sink is a column-for-column swap. It
// carries the joined shape metadata plus the server-side cost columns read
// from system.query_log.
type Row struct {
	ShapeID             string           `json:"shape_id"`
	Opts                []string         `json:"opts"`
	Language            string           `json:"language"`
	NormalizedQueryHash uint64           `json:"normalized_query_hash"`
	ReadRows            uint64           `json:"read_rows"`
	ReadBytes           uint64           `json:"read_bytes"`
	QueryDurationMS     uint64           `json:"query_duration_ms"`
	MemoryUsage         uint64           `json:"memory_usage"`
	ProfileEvents       map[string]int64 `json:"profile_events,omitempty"`
}

// Sink is the durable write target for reconciled rows. JSONLSink is the v1
// implementation (see sink.go). Write may receive an empty slice (a no-op).
type Sink interface {
	Write(rows []Row) error
	Close() error
}

// QueryLogSource reads finished system.query_log rows for a batch of
// query_ids. The production implementation runs a single rate-limited SELECT
// against system.query_log; a fake backs the unit tests. The returned rows
// carry the cost columns and normalized_query_hash but NOT the shape metadata
// (ShapeID/Opts/Language) — the reconciler joins those back from its ring by
// matching on query_id, so the source returns the query_id alongside each row.
type QueryLogSource interface {
	// FinishedByQueryID returns the finished (type='QueryFinish') query_log
	// rows for the supplied query_ids. Each returned SourceRow carries the
	// query_id it belongs to so the reconciler can join it back to the
	// observed Record. ids is never empty when called.
	FinishedByQueryID(ctx context.Context, ids []string) ([]SourceRow, error)
}

// SourceRow is one finished system.query_log row as returned by a
// QueryLogSource, before the reconciler joins the shape metadata onto it. It
// carries the query_id (the join key) plus the raw cost columns.
type SourceRow struct {
	QueryID             string
	NormalizedQueryHash uint64
	ReadRows            uint64
	ReadBytes           uint64
	QueryDurationMS     uint64
	MemoryUsage         uint64
	ProfileEvents       map[string]int64
}

// defaultRingCapacity is the fallback ring capacity when Options.RingCapacity
// is non-positive. It bounds memory: at most this many recently-dispatched
// query_ids are tracked, the oldest evicted as new ones arrive.
const defaultRingCapacity = 4096

// Options configures a Reconciler.
type Options struct {
	// RingCapacity bounds the in-memory ring of observed Records. When <= 0
	// defaultRingCapacity is used. The ring drops the oldest record when full.
	RingCapacity int
	// Interval is how often Run reconciles the ring against the source. When
	// <= 0 reconciliation is driven only by an explicit reconcileOnce call
	// (used by tests); production always supplies a positive interval.
	Interval time.Duration
	// Logger receives the non-fatal error logs. When nil slog.Default is used.
	Logger *slog.Logger
}

// Reconciler holds the bounded ring of observed Records and reconciles them
// against a QueryLogSource on an interval, writing joined Rows to a Sink. It
// is safe for concurrent Observe calls (the engine dispatch seam) alongside a
// single Run loop.
type Reconciler struct {
	src    QueryLogSource
	sink   Sink
	cap    int
	every  time.Duration
	logger *slog.Logger

	mu   sync.Mutex
	ring []Record       // bounded FIFO; oldest at index 0
	byID map[string]int // query_id -> index in ring (for the join)
}

// New builds a Reconciler over src and sink with opts. It does not start any
// goroutine; the caller runs Run.
func New(src QueryLogSource, sink Sink, opts Options) *Reconciler {
	capacity := opts.RingCapacity
	if capacity <= 0 {
		capacity = defaultRingCapacity
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Reconciler{
		src:    src,
		sink:   sink,
		cap:    capacity,
		every:  opts.Interval,
		logger: logger,
		ring:   make([]Record, 0, capacity),
		byID:   make(map[string]int, capacity),
	}
}

// Observe registers a dispatched query's Record in the bounded ring. When the
// ring is full it drops the oldest record (FIFO eviction) to keep memory
// bounded. A Record with an empty QueryID is ignored (no join key). Safe for
// concurrent callers.
func (r *Reconciler) Observe(rec Record) {
	if rec.QueryID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	// Replace an existing record for the same query_id in place rather than
	// growing the ring (defensive: a re-Observe of the same per-dispatch
	// query_id updates rather than duplicates the ring entry).
	if idx, ok := r.byID[rec.QueryID]; ok {
		r.ring[idx] = rec
		return
	}
	if len(r.ring) >= r.cap {
		r.evictOldestLocked()
	}
	r.ring = append(r.ring, rec)
	r.byID[rec.QueryID] = len(r.ring) - 1
}

// ObserveQuery adapts the engine.QueryObserver seam onto Observe: it builds a
// Record from the dispatch-seam tuple and ring-buffers it. The engine calls
// this once per dispatched query when the reconciler is registered as the
// Engine's QueryObserver.
func (r *Reconciler) ObserveQuery(queryID, shapeID string, opts []string, language string) {
	r.Observe(Record{
		QueryID:  queryID,
		ShapeID:  shapeID,
		Opts:     opts,
		Language: language,
	})
}

// evictOldestLocked drops ring[0] and reindexes. Caller holds r.mu. It keeps
// the ring a simple bounded FIFO; the reindex is O(n) but only runs at
// capacity, and n is the (bounded) ring size.
func (r *Reconciler) evictOldestLocked() {
	oldest := r.ring[0]
	delete(r.byID, oldest.QueryID)
	r.ring = r.ring[1:]
	for i := range r.ring {
		r.byID[r.ring[i].QueryID] = i
	}
}

// snapshotIDs returns a copy of the currently-tracked query_ids. Safe for
// concurrent Observe.
func (r *Reconciler) snapshotIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, len(r.ring))
	for i := range r.ring {
		ids[i] = r.ring[i].QueryID
	}
	return ids
}

// recordFor returns the observed Record for query_id, or ok=false. Safe for
// concurrent Observe.
func (r *Reconciler) recordFor(id string) (Record, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	idx, ok := r.byID[id]
	if !ok {
		return Record{}, false
	}
	return r.ring[idx], true
}

// forget drops the supplied ids from the ring after they have been reconciled
// and written, so a later interval does not re-query and re-write them. Safe
// for concurrent Observe.
func (r *Reconciler) forget(ids []string) {
	if len(ids) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	drop := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		drop[id] = struct{}{}
	}
	kept := r.ring[:0]
	for _, rec := range r.ring {
		if _, gone := drop[rec.QueryID]; gone {
			delete(r.byID, rec.QueryID)
			continue
		}
		kept = append(kept, rec)
	}
	r.ring = kept
	for i := range r.ring {
		r.byID[r.ring[i].QueryID] = i
	}
}

// Run drives the reconcile loop on the configured interval until ctx is
// cancelled, then returns (clean shutdown). Each tick reconciles the current
// ring against the source; errors are logged, never fatal. When the interval
// is non-positive Run reconciles nothing and simply blocks until ctx cancel
// (a misconfiguration guard — production always supplies a positive interval).
func (r *Reconciler) Run(ctx context.Context) {
	if r.every <= 0 {
		<-ctx.Done()
		return
	}
	ticker := time.NewTicker(r.every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.reconcileOnce(ctx)
		}
	}
}

// reconcileOnce reads the current ring of query_ids, joins the source's
// finished rows back to their observed Records, writes the joined Rows to the
// sink, and forgets the reconciled ids. Every failure is logged and returns
// early WITHOUT taking the process down. Exposed (unexported) so tests can
// drive a single reconcile deterministically.
func (r *Reconciler) reconcileOnce(ctx context.Context) {
	ids := r.snapshotIDs()
	if len(ids) == 0 {
		return
	}
	srcRows, err := r.src.FinishedByQueryID(ctx, ids)
	if err != nil {
		r.logger.Warn("optcorpus: query_log read failed", "err", err, "ids", len(ids))
		return
	}
	if len(srcRows) == 0 {
		return
	}

	rows := make([]Row, 0, len(srcRows))
	reconciled := make([]string, 0, len(srcRows))
	for _, sr := range srcRows {
		rec, ok := r.recordFor(sr.QueryID)
		if !ok {
			// Evicted between snapshot and read, or a stray id — skip.
			continue
		}
		rows = append(rows, Row{
			ShapeID:             rec.ShapeID,
			Opts:                rec.Opts,
			Language:            rec.Language,
			NormalizedQueryHash: sr.NormalizedQueryHash,
			ReadRows:            sr.ReadRows,
			ReadBytes:           sr.ReadBytes,
			QueryDurationMS:     sr.QueryDurationMS,
			MemoryUsage:         sr.MemoryUsage,
			ProfileEvents:       sr.ProfileEvents,
		})
		reconciled = append(reconciled, sr.QueryID)
	}
	if len(rows) == 0 {
		return
	}
	if err := r.sink.Write(rows); err != nil {
		r.logger.Warn("optcorpus: sink write failed", "err", err, "rows", len(rows))
		// Do NOT forget on write failure: retry the same ids next interval.
		return
	}
	r.forget(reconciled)
}
