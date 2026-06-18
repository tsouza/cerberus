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
	"sync/atomic"
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

// defaultObserveBuffer is the fallback capacity of the non-blocking ingest
// channel that decouples the data-plane dispatch seam (ObserveQuery) from the
// ring. It is sized to absorb a burst between drains without blocking; when it
// is momentarily full ObserveQuery drops the Record (the corpus is a
// best-effort sample, never a system of record), so a dispatch never blocks on
// a slow drain.
const defaultObserveBuffer = 8192

// Options configures a Reconciler.
type Options struct {
	// RingCapacity bounds the in-memory ring of observed Records. When <= 0
	// defaultRingCapacity is used. The ring drops the oldest record when full.
	RingCapacity int
	// ObserveBuffer is the capacity of the non-blocking ingest channel between
	// the data-plane dispatch seam (ObserveQuery) and the drain. When <= 0
	// defaultObserveBuffer is used.
	ObserveBuffer int
	// Interval is how often Run reconciles the ring against the source. When
	// <= 0 reconciliation is driven only by an explicit reconcileOnce call
	// (used by tests); production always supplies a positive interval.
	Interval time.Duration
	// TTL bounds how long an observed query_id is kept in the join index when it
	// is never joined to a finished query_log row (e.g. a query that errored, was
	// killed, or whose row never lands). Such ids are otherwise only forgotten on
	// a successful join, so without a TTL they linger in every per-interval
	// IN(...) until evicted by ring pressure. TTL is set to the query_log
	// lookback window: an id older than the window can no longer match a row the
	// source can still see, so it is dropped. When <= 0 TTL eviction is disabled
	// (ids are forgotten only on join or ring eviction).
	TTL time.Duration
	// Logger receives the non-fatal error logs. When nil slog.Default is used.
	Logger *slog.Logger
}

// Reconciler holds the bounded ring of observed Records and reconciles them
// against a QueryLogSource on an interval, writing joined Rows to a Sink.
//
// The data-plane dispatch seam (ObserveQuery) never touches the ring mutex: it
// does a single non-blocking channel send and returns, so it cannot serialize
// the three head engines (prom/loki/tempo) against each other nor pay any
// per-query ring cost. The Run goroutine drains that channel into the ring via
// the synchronous Observe, which itself is O(1): the ring is a fixed-size
// circular buffer, so eviction overwrites the slot in place with no reindex.
type Reconciler struct {
	src    QueryLogSource
	sink   Sink
	cap    int
	every  time.Duration
	ttl    time.Duration
	now    func() time.Time
	logger *slog.Logger

	// ingest carries Records from the data-plane seam to the drain. A
	// non-blocking send keeps the dispatch path off the ring mutex entirely.
	ingest chan Record

	mu    sync.Mutex
	ring  []Record       // fixed-size circular buffer, len == cap once filled
	head  int            // next write position (mod cap)
	count int            // number of live records (<= cap)
	byID  map[string]int // query_id -> slot index in ring (for the join)
	// seenAt records when each live query_id was observed, for TTL eviction of
	// ids that never join to a finished row. Kept in lockstep with byID: an entry
	// is added on Observe and removed wherever the byID entry is (forget, ring
	// eviction, replace-in-place refreshes the timestamp).
	seenAt map[string]time.Time

	// dropped counts ObserveQuery records shed because the ingest buffer was
	// full, for a rate-limited diagnostic. It is touched only via its atomic
	// methods (incremented on the data-plane seam, Swap-and-logged on the
	// drain), so it never needs the ring mutex.
	dropped atomic.Uint64
}

// New builds a Reconciler over src and sink with opts. It does not start any
// goroutine; the caller runs Run.
func New(src QueryLogSource, sink Sink, opts Options) *Reconciler {
	capacity := opts.RingCapacity
	if capacity <= 0 {
		capacity = defaultRingCapacity
	}
	buffer := opts.ObserveBuffer
	if buffer <= 0 {
		buffer = defaultObserveBuffer
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
		ttl:    opts.TTL,
		now:    time.Now,
		logger: logger,
		ingest: make(chan Record, buffer),
		ring:   make([]Record, capacity),
		byID:   make(map[string]int, capacity),
		seenAt: make(map[string]time.Time, capacity),
	}
}

// Observe registers a dispatched query's Record in the bounded ring. When the
// ring is full it overwrites the oldest slot in place (O(1) circular-buffer
// eviction, no reindex). A Record with an empty QueryID is ignored (no join
// key). Safe for concurrent callers, though in production only the single Run
// drain calls it; the data-plane seam is ObserveQuery (non-blocking).
func (r *Reconciler) Observe(rec Record) {
	if rec.QueryID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	// Replace an existing record for the same query_id in place rather than
	// consuming a new slot (defensive: a re-Observe of the same per-dispatch
	// query_id updates rather than duplicates the ring entry). Refresh its
	// observation time so a re-observed id is not TTL-evicted prematurely.
	if idx, ok := r.byID[rec.QueryID]; ok {
		r.ring[idx] = rec
		r.seenAt[rec.QueryID] = r.now()
		return
	}

	slot := r.head
	if r.count == r.cap {
		// Full: the slot we are about to overwrite holds the oldest record;
		// drop its index so the join no longer points at the reused slot.
		evicted := r.ring[slot]
		if r.byID[evicted.QueryID] == slot {
			delete(r.byID, evicted.QueryID)
			delete(r.seenAt, evicted.QueryID)
		}
	} else {
		r.count++
	}
	r.ring[slot] = rec
	r.byID[rec.QueryID] = slot
	r.seenAt[rec.QueryID] = r.now()
	r.head = (r.head + 1) % r.cap
}

// ObserveQuery is the data-plane dispatch seam (engine.QueryObserver). It does
// a single non-blocking channel send and returns, so a dispatched query never
// touches the ring mutex, never serializes against the other head engines, and
// never pays any per-query ring cost. When the ingest buffer is momentarily
// full the Record is DROPPED (the corpus is a best-effort sample, not a system
// of record) and counted for a rate-limited diagnostic; the drop is strictly
// preferable to blocking a data-plane dispatch on the corpus.
func (r *Reconciler) ObserveQuery(queryID, shapeID string, opts []string, language string) {
	if queryID == "" {
		return
	}
	rec := Record{
		QueryID:  queryID,
		ShapeID:  shapeID,
		Opts:     opts,
		Language: language,
	}
	select {
	case r.ingest <- rec:
	default:
		r.dropped.Add(1)
	}
}

// drainIngest moves all currently-buffered Records from the ingest channel
// into the ring. Called from the Run goroutine (the reconcile tick and at
// startup), never from the data plane. It is bounded by what is buffered so it
// cannot spin: it stops as soon as the channel is momentarily empty.
func (r *Reconciler) drainIngest() {
	for {
		select {
		case rec := <-r.ingest:
			r.Observe(rec)
		default:
			if n := r.dropped.Swap(0); n > 0 {
				r.logger.Warn("optcorpus: dropped observed queries (ingest buffer full)", "dropped", n)
			}
			return
		}
	}
}

// snapshotIDs returns a copy of the currently-tracked query_ids. The byID map
// is the canonical live set (the circular ring may hold stale slots that byID
// no longer points at). Safe for concurrent Observe.
func (r *Reconciler) snapshotIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.byID))
	for id := range r.byID {
		ids = append(ids, id)
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

// forget drops the supplied ids from the join index after they have been
// reconciled and written, so a later interval does not re-query and re-write
// them. The ring slot itself is left in place (it will be overwritten by a
// future Observe); only the byID entry -- the canonical live set -- is removed.
// Safe for concurrent Observe.
func (r *Reconciler) forget(ids []string) {
	if len(ids) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range ids {
		// Drop only the join index. The ring slot stays physically occupied
		// (r.count unchanged) so eviction keeps advancing head correctly; the
		// slot is simply no longer reachable for a join and will be overwritten
		// by a future Observe.
		delete(r.byID, id)
		delete(r.seenAt, id)
	}
}

// evictExpired drops join-index entries for ids observed longer ago than the
// TTL. These are queries that were dispatched but never joined to a finished
// query_log row (errored, killed, or whose row never landed); once they are
// older than the source's lookback window they can no longer match a visible
// row, so keeping them only bloats every per-interval IN(...). Returns the
// number evicted (for a diagnostic). A non-positive TTL disables eviction. Like
// forget, it drops only the byID/seenAt entries; the ring slot is reclaimed by
// a future Observe. Safe for concurrent Observe.
func (r *Reconciler) evictExpired() int {
	if r.ttl <= 0 {
		return 0
	}
	cutoff := r.now().Add(-r.ttl)
	r.mu.Lock()
	defer r.mu.Unlock()
	evicted := 0
	for id, seen := range r.seenAt {
		if seen.Before(cutoff) {
			delete(r.byID, id)
			delete(r.seenAt, id)
			evicted++
		}
	}
	return evicted
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
			// Final drain so Records buffered on the seam at shutdown are not
			// silently lost; no reconcile (ctx is already done).
			r.drainIngest()
			return
		case <-ticker.C:
			// Pull everything the data-plane seam buffered since the last tick
			// into the ring, then reconcile the ring against the source.
			r.drainIngest()
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
	// Drop ids older than the lookback window before snapshotting, so a
	// never-finished query is not carried in the IN(...) forever (it can no
	// longer match a row the source can still see).
	if n := r.evictExpired(); n > 0 {
		r.logger.Debug("optcorpus: evicted stale unobserved query_ids", "evicted", n)
	}
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
