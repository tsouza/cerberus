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

// RouteFeatures is the routing classifier's read-out captured at dispatch:
// the route the pure classifier (internal/solver Planner.Plan) chose plus the
// RAW cost-grid scalars it computed (N anchors, fan-out, cumulative spine
// lookback D, outer range, step) and the decision reason. It is the
// dispatch-side half of the route A/B calibration corpus (stage 0): the
// reconciler joins it to the OBSERVED server-side cost so an operator can
// replay the classifier offline (counterfactual threshold testing) and
// measure the wrong-route overlap.
//
// Recording these features is a pure additive readout — it changes no routing
// behavior. The features are present for BOTH route A (not-routed,
// below-threshold) and route B (routed) decisions, because the overlap
// analysis compares the cost distributions of the two routes at equal
// (N, F, D). Buckets are keyed on the RAW scalars, never on Reason: the
// not-routed shadow header folds the high-D class into below-threshold, so the
// reason string alone hides it.
type RouteFeatures struct {
	// Route is "A" (single CH query) or "B" (time-slice sharded). It is the
	// route the classifier actually chose for this dispatch.
	Route string
	// NAnchors is N = OuterRange/Step + 1 on the outermost spine.
	NAnchors uint32
	// Fanout is F = max(Range/Step or Lookback/Step) over the windowed nodes.
	Fanout uint32
	// CumulativeD is D = Σ spine lookback, in seconds (the corpus stores
	// durations as whole seconds, matching the UInt32 columns).
	CumulativeD uint32
	// OuterRange is the outermost spine OuterRange, in seconds.
	OuterRange uint32
	// Step is the request grid step, in seconds.
	Step uint32
	// KShards is the shard count on route B, 0 on route A.
	KShards uint8
	// DecisionReason is the classifier's Reason* vocabulary value.
	DecisionReason string
	// Present reports whether routing features were captured for this
	// dispatch. It is false when the Solver is off or the head is not the
	// classified head, so the reconciler can leave the routing columns at
	// their zero defaults rather than record a fictitious route-A row.
	Present bool
}

// ExitStatus is how a dispatched query terminated, derived from the
// system.query_log row type plus its exception. It is the corpus's
// cost-distribution discriminator: an OOM or timeout exit is the very signal
// route B (time-slice sharding) exists to avoid, so the go/no-go analysis
// reads it directly.
type ExitStatus uint8

const (
	// ExitOK is a clean QueryFinish.
	ExitOK ExitStatus = iota
	// ExitOOM is a QueryExceptionWhileProcessing whose exception is a
	// ClickHouse memory-limit / OOM code. CH-side, derived from query_log.
	ExitOOM
	// ExitTimeout is a QueryExceptionWhileProcessing whose exception is a
	// ClickHouse timeout / exceeded-execution-time code. CH-side, derived from
	// query_log.
	ExitTimeout
	// ExitSampleBudget is a CERBERUS-side terminal outcome: the CH query
	// FINISHED cleanly (query_log shows ok with real cost), but cerberus
	// rejected the client with the query.maxSamples 422 during the Go-side
	// result drain. The richest calibration signal — "CH cost = X, but
	// cerberus rejected: too big" — so the cost columns are KEPT while the
	// exit status records cerberus's authoritative outcome. It takes
	// precedence over the query_log-derived ExitOK for the same query_id.
	ExitSampleBudget
	// ExitBreaker is a CERBERUS-side terminal outcome: the chclient circuit
	// breaker was OPEN, so cerberus fast-failed the request 503 BEFORE
	// dispatching any CH query. There is no query_log row to join — the corpus
	// row is decision-only, with no cost.
	ExitBreaker
	// ExitRejected is a CERBERUS-side terminal outcome: cerberus rejected the
	// request 400 BEFORE dispatching (resolution-cap / body-limit). There is
	// no CH query and no query_log row — the corpus row is decision-only, with
	// no cost.
	ExitRejected
)

// String renders the ExitStatus as the corpus enum token. The tokens are the
// stable wire/DDL contract shared by the JSONL sink, the CH Enum8 column, and
// the calibration SQL — keep them in lockstep with exitEnumValue (chtable.go)
// and the CH Enum8 DDL (corpusCreateTableSQL).
func (e ExitStatus) String() string {
	switch e {
	case ExitOOM:
		return "oom"
	case ExitTimeout:
		return "timeout"
	case ExitSampleBudget:
		return "sample_budget"
	case ExitBreaker:
		return "breaker"
	case ExitRejected:
		return "rejected"
	default:
		return "ok"
	}
}

// Exit-status tokens — the stable string contract shared with callers that do
// not import this package's enum (the engine passes these primitive tokens
// through the QueryObserver seam). They MUST match ExitStatus.String().
const (
	ExitTokenSampleBudget = "sample_budget"
	ExitTokenBreaker      = "breaker"
	ExitTokenRejected     = "rejected"
	// ExitTokenOOM is the dispatched memory-cap rejection token. Unlike the
	// three above it is NOT a cerberusSide() override token (oom is also the
	// query_log-derived label for the same physical event); it rides the
	// ObserveDispatchedRejection seam, which emits the row TERMINALLY at the
	// rejection site (zero cost, route features) without waiting for a
	// query_log join. The memory-cap 241 aborts the query on CH, but the
	// corpus must not depend on the join landing a row for it.
	ExitTokenOOM = "oom"
)

// parseExitStatus maps an exit_status token back to its ExitStatus. It accepts
// only the cerberus-side tokens (the in-process seams never pass a CH-derived
// token); an unrecognised token returns ok=false so a typo cannot masquerade as
// a real outcome. ExitOK / oom / timeout are query_log-derived and not parsed
// here on purpose.
func parseExitStatus(token string) (ExitStatus, bool) {
	switch token {
	case ExitTokenSampleBudget:
		return ExitSampleBudget, true
	case ExitTokenBreaker:
		return ExitBreaker, true
	case ExitTokenRejected:
		return ExitRejected, true
	default:
		return ExitOK, false
	}
}

// cerberusSide reports whether the ExitStatus is a CERBERUS-side terminal
// outcome that cerberus determined in-process (sample-budget / breaker /
// rejected), as opposed to a CH-side outcome derived from system.query_log
// (ok / oom / timeout). A cerberus-side outcome is authoritative: it takes
// precedence over the query_log-derived status when both exist for one query.
func (e ExitStatus) cerberusSide() bool {
	switch e {
	case ExitSampleBudget, ExitBreaker, ExitRejected:
		return true
	default:
		return false
	}
}

// parseTerminalRejectionStatus maps a token to its ExitStatus for the
// terminal-at-rejection seam (ObserveDispatchedRejection). It accepts the three
// cerberus-side tokens PLUS oom: a dispatched memory-cap (CH code 241) abort the
// engine knows in-process at the error site, recorded directly with zero cost
// rather than waiting for a system.query_log row that may never be joined. An
// unrecognised token returns ok=false so a typo cannot masquerade as a real
// outcome. ok / timeout stay query_log-derived and are not parsed here.
func parseTerminalRejectionStatus(token string) (ExitStatus, bool) {
	if token == ExitTokenOOM {
		return ExitOOM, true
	}
	return parseExitStatus(token)
}

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
	// Route is the routing classifier read-out captured at dispatch. It is the
	// dispatch-side half of the route A/B calibration corpus; the reconciler
	// joins it to the observed cost. Route.Present is false when no routing
	// classification ran (Solver off / unclassified head).
	Route RouteFeatures

	// Outcome is the CERBERUS-side terminal outcome cerberus determined
	// in-process for this request (ExitSampleBudget / ExitBreaker /
	// ExitRejected). It is the authoritative exit status query_log cannot
	// reflect: the sample-budget 422 fires AFTER a clean CH finish, and the
	// breaker / cap rejections fire BEFORE any CH dispatch. HasOutcome gates
	// it so the zero value (ExitOK) is not mistaken for a real outcome. When
	// set on a query_id that also joins a query_log row, the reconciler keeps
	// the query_log COST but overrides exit_status with this value.
	Outcome    ExitStatus
	HasOutcome bool

	// DecisionOnly marks a row that has NO CH query to join (a pre-dispatch
	// breaker / cap rejection): the Decision + N/F/D are known at classify
	// time but there is no query_id and no cost. The reconciler writes these
	// straight to the sink with zero cost rather than carrying them in the
	// query_log IN(...) join.
	DecisionOnly bool
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

	// Routing features (stage 0 route A/B calibration). These join each
	// routing DECISION to its OBSERVED cost so the pure classifier can be
	// replayed offline. They are zero-valued when the dispatch carried no
	// routing classification (Solver off / unclassified head) — Route is then
	// "" and the scalar columns are 0. The field shape stays column-for-column
	// aligned with the cerberus_router_corpus MergeTree (see chtable.go) so the
	// JSONL and CH-table sinks write the same Row.
	NAnchors       uint32 `json:"n_anchors"`
	Fanout         uint32 `json:"fanout"`
	CumulativeD    uint32 `json:"cumulative_d"`
	OuterRange     uint32 `json:"outer_range"`
	Step           uint32 `json:"step"`
	Route          string `json:"route"`
	KShards        uint8  `json:"k_shards"`
	DecisionReason string `json:"decision_reason"`
	// ExitStatus is "ok" | "oom" | "timeout", derived by the reconciler from
	// the system.query_log row type + exception.
	ExitStatus string `json:"exit_status"`
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
	// FinishedByQueryID returns the terminal query_log rows for the supplied
	// query_ids: clean finishes (type='QueryFinish') AND exception exits
	// (type='QueryExceptionWhileProcessing'), so the corpus can record how a
	// query terminated. Each returned SourceRow carries the query_id it belongs
	// to so the reconciler can join it back to the observed Record. ids is
	// never empty when called.
	FinishedByQueryID(ctx context.Context, ids []string) ([]SourceRow, error)
}

// SourceRow is one terminal system.query_log row as returned by a
// QueryLogSource, before the reconciler joins the shape metadata onto it. It
// carries the query_id (the join key), the raw cost columns, and the derived
// exit status (clean / oom / timeout).
type SourceRow struct {
	QueryID             string
	NormalizedQueryHash uint64
	ReadRows            uint64
	ReadBytes           uint64
	QueryDurationMS     uint64
	MemoryUsage         uint64
	ProfileEvents       map[string]int64
	// ExitStatus is how the query terminated, derived by the source from the
	// query_log row type + exception code (ExitOK on a QueryFinish).
	ExitStatus ExitStatus
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

	// pendingRejections buffers decision-only rejection Records (pre-dispatch
	// breaker / cap outcomes) that have no query_id to join. The Run drain
	// appends to it under the ring mutex; reconcileOnce flushes it straight to
	// the sink with zero cost, then clears it. It is bounded by the same
	// drop-under-burst contract as the ring (a rejection is a best-effort
	// sample, never a system record).
	pendingRejections []Record
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
// eviction, no reindex). Safe for concurrent callers, though in production only
// the single Run drain calls it; the data-plane seam is ObserveQuery /
// ObserveOutcome / ObserveRejection / ObserveDispatchedRejection (all
// non-blocking).
//
// It handles three Record kinds, in FIFO order off the ingest channel:
//   - decision-only (DecisionOnly): a rejection recorded terminally at the
//     engine site — buffered for the next reconcile flush, never ringed. A
//     pre-dispatch breaker / cap rejection has no query_id; a DISPATCHED
//     memory-cap (oom) rejection carries the dispatch query_id, which is dropped
//     from the join index here so the query_log reconcile cannot double-write it.
//   - outcome-update (HasOutcome with no ShapeID): merges a cerberus-side
//     terminal outcome onto an already-ringed dispatch record by query_id.
//   - dispatch (the default): the normal at-dispatch metadata record.
func (r *Reconciler) Observe(rec Record) {
	if rec.DecisionOnly {
		// A terminal rejection recorded at the engine site: no usable CH cost,
		// buffered for the next reconcile flush rather than carried in the join
		// ring. A pre-dispatch rejection (breaker / cap) has no QueryID. A
		// DISPATCHED rejection (memory-cap oom) DOES carry the dispatch query_id:
		// drop it from the join index here so the later query_log reconcile
		// cannot ALSO emit a row for the same physical event (no double-count).
		r.mu.Lock()
		if rec.QueryID != "" {
			if idx, ok := r.byID[rec.QueryID]; ok && r.ring[idx].QueryID == rec.QueryID {
				delete(r.byID, rec.QueryID)
				delete(r.seenAt, rec.QueryID)
			}
		}
		r.pendingRejections = append(r.pendingRejections, rec)
		r.mu.Unlock()
		return
	}
	if rec.QueryID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	// Outcome-update: a cerberus-side terminal outcome (sample-budget 422)
	// arriving AFTER the dispatch record. Merge the outcome onto the existing
	// ring record, preserving its metadata, rather than clobbering it with an
	// otherwise-empty record. If the dispatch record is not present (evicted /
	// never observed) the outcome is dropped — there is nothing to join it to.
	if rec.HasOutcome && rec.ShapeID == "" {
		if idx, ok := r.byID[rec.QueryID]; ok {
			r.ring[idx].Outcome = rec.Outcome
			r.ring[idx].HasOutcome = true
		}
		return
	}

	// Replace an existing record for the same query_id in place rather than
	// consuming a new slot (defensive: a re-Observe of the same per-dispatch
	// query_id updates rather than duplicates the ring entry). Refresh its
	// observation time so a re-observed id is not TTL-evicted prematurely.
	// Preserve any cerberus-side outcome already merged onto the slot so an
	// out-of-order outcome-update is not lost on a late dispatch re-observe.
	if idx, ok := r.byID[rec.QueryID]; ok {
		if r.ring[idx].HasOutcome && !rec.HasOutcome {
			rec.Outcome = r.ring[idx].Outcome
			rec.HasOutcome = true
		}
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
//
// The trailing route* parameters carry the routing classifier read-out for
// this dispatch (stage 0 route A/B calibration). They are passed as primitive
// scalars — not a shared struct — so neither package imports the other's types
// (the engine declares the QueryObserver interface; optcorpus supplies the
// concrete *Reconciler, and a shared struct would couple them and risk the
// nil-interface trap the engine guards against). routePresent is false when
// the dispatch carried no routing classification (Solver off / unclassified
// head); the reconciler then leaves the routing columns at zero.
func (r *Reconciler) ObserveQuery(
	queryID, shapeID string,
	opts []string,
	language string,
	routePresent bool,
	route string,
	nAnchors, fanout, cumulativeD, outerRange, step uint32,
	kShards uint8,
	decisionReason string,
) {
	if queryID == "" {
		return
	}
	rec := Record{
		QueryID:  queryID,
		ShapeID:  shapeID,
		Opts:     opts,
		Language: language,
		Route: RouteFeatures{
			Present:        routePresent,
			Route:          route,
			NAnchors:       nAnchors,
			Fanout:         fanout,
			CumulativeD:    cumulativeD,
			OuterRange:     outerRange,
			Step:           step,
			KShards:        kShards,
			DecisionReason: decisionReason,
		},
	}
	r.enqueue(rec)
}

// ObserveOutcome is the data-plane seam for a CERBERUS-side terminal outcome on
// a DISPATCHED query — currently the query.maxSamples 422, which fires during
// the Go-side result drain AFTER the CH query finished cleanly. It stamps the
// authoritative cerberus outcome onto the already-observed dispatch record
// (matched by query_id) so the reconciler keeps the joined query_log COST but
// overrides exit_status with this value. Like ObserveQuery it does a single
// non-blocking channel send: the data plane never touches the ring mutex and a
// momentarily-full buffer drops the update (best-effort sample).
//
// statusToken is the stable exit_status token (the engine passes a primitive
// string rather than the optcorpus enum so the engine stays decoupled from this
// package). A token that is not a cerberus-side outcome, or an empty queryID,
// is ignored.
func (r *Reconciler) ObserveOutcome(queryID, statusToken string) {
	status, ok := parseExitStatus(statusToken)
	if queryID == "" || !ok || !status.cerberusSide() {
		return
	}
	r.enqueue(Record{QueryID: queryID, Outcome: status, HasOutcome: true})
}

// ObserveRejection is the data-plane seam for a CERBERUS-side terminal outcome
// on a query that was rejected BEFORE any CH dispatch (breaker 503 / cap 400):
// there is no query_id and no CH cost, but the routing Decision + N/F/D are
// known at classify time. It records a decision-only corpus row (zero cost,
// the supplied outcome) so these pre-CH rejections — the most diagnostic
// misroute signals — are not missed entirely. The routing scalars mirror
// ObserveQuery; routePresent is false when no classification ran. statusToken
// is the stable exit_status token; a token that is not a cerberus-side outcome
// is ignored. Non-blocking, drop-under-burst.
func (r *Reconciler) ObserveRejection(
	shapeID string,
	opts []string,
	language string,
	statusToken string,
	routePresent bool,
	route string,
	nAnchors, fanout, cumulativeD, outerRange, step uint32,
	kShards uint8,
	decisionReason string,
) {
	status, ok := parseExitStatus(statusToken)
	if !ok || !status.cerberusSide() {
		return
	}
	r.enqueue(Record{
		ShapeID:      shapeID,
		Opts:         opts,
		Language:     language,
		Outcome:      status,
		HasOutcome:   true,
		DecisionOnly: true,
		Route: RouteFeatures{
			Present:        routePresent,
			Route:          route,
			NAnchors:       nAnchors,
			Fanout:         fanout,
			CumulativeD:    cumulativeD,
			OuterRange:     outerRange,
			Step:           step,
			KShards:        kShards,
			DecisionReason: decisionReason,
		},
	})
}

// ObserveDispatchedRejection is the data-plane seam for a CERBERUS-side terminal
// outcome on a query that DID dispatch a CH query but was aborted by a
// resource cap the engine recognises in-process at the error site — currently
// the per-query memory cap (max_memory_usage, CH code 241 MEMORY_LIMIT_EXCEEDED,
// statusToken "oom"). The abort runs ON ClickHouse, so a system.query_log row
// MAY land (type QueryExceptionWhileProcessing), but the corpus must not depend
// on the join landing: the row is recorded TERMINALLY here with the dispatch's
// known route features and ZERO cost (read_rows / bytes / duration / memory are
// unknowable at the engine error site — kept honestly unset, never fabricated).
//
// queryID is the dispatched query's join key: the drain FORGETS it from the
// join ring so the later query_log reconcile cannot ALSO emit a row for the same
// physical event (no double-count). An empty queryID still records the terminal
// row; it just has no ring entry to forget. The routing scalars mirror
// ObserveQuery; routePresent is false when no classification ran. statusToken is
// the stable exit_status token (oom or a cerberus-side token); an unrecognised
// token is ignored. Non-blocking, drop-under-burst.
func (r *Reconciler) ObserveDispatchedRejection(
	queryID, shapeID string,
	opts []string,
	language string,
	statusToken string,
	routePresent bool,
	route string,
	nAnchors, fanout, cumulativeD, outerRange, step uint32,
	kShards uint8,
	decisionReason string,
) {
	status, ok := parseTerminalRejectionStatus(statusToken)
	if !ok {
		return
	}
	r.enqueue(Record{
		QueryID:      queryID,
		ShapeID:      shapeID,
		Opts:         opts,
		Language:     language,
		Outcome:      status,
		HasOutcome:   true,
		DecisionOnly: true,
		Route: RouteFeatures{
			Present:        routePresent,
			Route:          route,
			NAnchors:       nAnchors,
			Fanout:         fanout,
			CumulativeD:    cumulativeD,
			OuterRange:     outerRange,
			Step:           step,
			KShards:        kShards,
			DecisionReason: decisionReason,
		},
	})
}

// enqueue does the shared single non-blocking send onto the ingest channel,
// counting a drop when the buffer is momentarily full. All data-plane seams
// (ObserveQuery / ObserveOutcome / ObserveRejection / ObserveDispatchedRejection)
// funnel through it so the drop-under-burst contract is identical and stated once.
func (r *Reconciler) enqueue(rec Record) {
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
	// Flush decision-only rejection rows first: these pre-dispatch breaker / cap
	// outcomes have no query_id and so never enter the query_log join. They are
	// written even when the join below has nothing (the most diagnostic misroute
	// rows must not depend on a CH query existing).
	r.flushRejections()

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
		row := Row{
			ShapeID:             rec.ShapeID,
			Opts:                rec.Opts,
			Language:            rec.Language,
			NormalizedQueryHash: sr.NormalizedQueryHash,
			ReadRows:            sr.ReadRows,
			ReadBytes:           sr.ReadBytes,
			QueryDurationMS:     sr.QueryDurationMS,
			MemoryUsage:         sr.MemoryUsage,
			ProfileEvents:       sr.ProfileEvents,
			// exit_status is the observed-cost discriminator the go/no-go
			// analysis reads (oom / timeout = the cost route B avoids).
			// Precedence: a CERBERUS-side outcome (e.g. the sample-budget 422
			// that fired after a clean CH finish) is authoritative over the
			// query_log-derived status — the query_log shows 'ok' with real
			// cost, but cerberus actually rejected the client. The COST columns
			// are kept (the richest signal: "CH cost = X, but cerberus
			// rejected"); only the exit status is overridden.
			ExitStatus: exitStatusToken(sr.ExitStatus, rec),
		}
		joinRouteFeatures(&row, rec)
		rows = append(rows, row)
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

// exitStatusToken resolves the exit_status token for a joined row, giving a
// CERBERUS-side outcome (sample-budget / breaker / rejected) precedence over
// the query_log-derived CH-side status. The CH cost stays on the row either
// way; only the discriminator changes.
func exitStatusToken(chStatus ExitStatus, rec Record) string {
	if rec.HasOutcome && rec.Outcome.cerberusSide() {
		return rec.Outcome.String()
	}
	return chStatus.String()
}

// joinRouteFeatures copies the dispatch-side routing read-out from rec onto
// row. Left at zero when no classification ran for the dispatch (route stays
// ""). Shared by the query_log join and the decision-only rejection flush.
func joinRouteFeatures(row *Row, rec Record) {
	if !rec.Route.Present {
		return
	}
	row.Route = rec.Route.Route
	row.NAnchors = rec.Route.NAnchors
	row.Fanout = rec.Route.Fanout
	row.CumulativeD = rec.Route.CumulativeD
	row.OuterRange = rec.Route.OuterRange
	row.Step = rec.Route.Step
	row.KShards = rec.Route.KShards
	row.DecisionReason = rec.Route.DecisionReason
}

// flushRejections drains the buffered decision-only rejection Records into
// zero-cost corpus Rows and writes them to the sink. On a sink-write failure
// the rejections are re-buffered for the next interval (failure-open: a sink
// outage degrades the corpus, never the data plane), mirroring the join path's
// "do not forget on write failure" retry contract.
func (r *Reconciler) flushRejections() {
	pending := r.takeRejections()
	if len(pending) == 0 {
		return
	}
	rows := make([]Row, 0, len(pending))
	for _, rec := range pending {
		row := Row{
			ShapeID:  rec.ShapeID,
			Opts:     rec.Opts,
			Language: rec.Language,
			// No CH query ran: cost columns stay zero. exit_status carries the
			// cerberus-side outcome (breaker / rejected).
			ExitStatus: rec.Outcome.String(),
		}
		joinRouteFeatures(&row, rec)
		rows = append(rows, row)
	}
	if err := r.sink.Write(rows); err != nil {
		r.logger.Warn("optcorpus: rejection sink write failed", "err", err, "rows", len(rows))
		r.rebufferRejections(pending)
	}
}

// takeRejections atomically swaps out the pending decision-only rejection
// buffer under the ring mutex, returning what was buffered and leaving the
// buffer empty for the next interval.
func (r *Reconciler) takeRejections() []Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.pendingRejections) == 0 {
		return nil
	}
	out := r.pendingRejections
	r.pendingRejections = nil
	return out
}

// rebufferRejections prepends not-yet-written rejections back onto the pending
// buffer after a sink-write failure, so the next interval retries them.
func (r *Reconciler) rebufferRejections(recs []Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pendingRejections = append(recs, r.pendingRejections...)
}
