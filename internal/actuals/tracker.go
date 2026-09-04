package actuals

import (
	"sync"
	"time"
)

// trackerCapacity bounds the Tracker's resident size, mirroring
// internal/engine's own scanEstimateCacheCapacity / perRungLearnerCapacity
// (both 4096): a bounded map with coarse (any-single-entry) eviction, whose
// worst case is only ever "this shape re-learns its calibration a little
// sooner than it otherwise would have" — the always-safe direction for an
// advisory-only signal, exactly like those two caches' own eviction.
const trackerCapacity = 4096

// minCalibrationFactor / maxCalibrationFactor bound CalibrationFactor's
// output: even when a shape's actual EMA and predicted rows disagree
// wildly, the returned multiplier can move the EXPLAIN ESTIMATE-derived
// row count no further than 2x up or down in one request. This is the
// bounded-influence property applied to Hook 1 (carrier-geometry cost-model
// calibration, engine's calibrateEstimate) — a single request's advisory
// estimate can nudge classify()'s K clamp, never swing it by an order of
// magnitude off one shape's noisy history.
const (
	minCalibrationFactor = 0.5
	maxCalibrationFactor = 2.0
)

// Source identifies which capture path produced an Actual observation.
type Source int

const (
	// SourcePacket is the native-protocol progress/ProfileEvents packet fast
	// path (internal/chclient/progress.go) — free, since the production
	// deployment's driver already streams these packets for every query; no
	// extra round trip.
	SourcePacket Source = iota
	// SourceQueryLog is the system.query_log batch/fallback path
	// (internal/engine's QueryLogActualsReconciler) — used for a query the
	// packet path missed (one that failed before completing, or a deployment
	// mode where packet capture is not wired) and inherently a SLOW path:
	// query_log's own async flush lag means a row surfaces here well after
	// the query that produced it finished.
	SourceQueryLog
)

// String renders Source for logs/metrics labels.
func (s Source) String() string {
	switch s {
	case SourcePacket:
		return "packet"
	case SourceQueryLog:
		return "query_log"
	default:
		return "unknown"
	}
}

// Actual is one query's measured resource usage, from either capture path.
type Actual struct {
	// ReadRows / ReadBytes are the query's total scanned rows/bytes — the
	// SAME quantities system.query_log.read_rows/read_bytes report, and the
	// packet path's rows/bytes running totals converge to (see
	// internal/chclient/progress.go's own doc on why the LATEST progress
	// packet, not a sum, is the query total).
	ReadRows  uint64
	ReadBytes uint64
	// PeakMemory is the query's peak memory usage in bytes. The packet path
	// reads it from the ProfileEvents "MemoryTrackerPeakUsage" counter
	// (verified against a live ClickHouse 26.6 server — see
	// internal/chclient/progress.go); the query_log path reads it straight
	// from system.query_log.memory_usage. Zero when unknown.
	PeakMemory uint64
}

// DriftReport is a point-in-time read-out of one plan shape's tracked state.
type DriftReport struct {
	ShapeID string

	// HasPredicted / PredictedRows are the most recently recorded advisory
	// EXPLAIN ESTIMATE / cardinality-probe row prediction for this shape
	// (RecordPredicted's last call), or HasPredicted=false when no
	// prediction has been recorded yet.
	HasPredicted  bool
	PredictedRows uint64

	// ActualEMARows is the bounded exponential moving average of ReadRows
	// across every RecordActual call for this shape (see Config.EMAAlpha).
	ActualEMARows float64
	Observations  int
	LastSource    Source
	LastObserved  time.Time

	// Ratio is ActualEMARows / PredictedRows, or 0 when there is no
	// prediction to compare against (HasPredicted false) or PredictedRows is
	// zero (a "the index pruned everything" prediction — see
	// driftRatio's own doc for why that is never treated as alerting).
	Ratio float64

	// Alerting is true when Observations has reached Config.MinObservations
	// AND Ratio falls outside [Config.DriftLowerRatio, Config.DriftUpperRatio]
	// — the drift-detection core's own verdict (issue #2789's primary
	// deliverable). False whenever there is no prediction, too few
	// observations, or the ratio is inside the expected band.
	Alerting bool
}

// state is one plan-shape's tracked predicted/actual pair.
type state struct {
	hasPredicted  bool
	predictedRows uint64

	hasActual    bool
	emaRows      float64
	observations int
	lastSource   Source
	lastObserved time.Time
}

// Tracker is the bounded, in-process predicted-vs-actual drift tracker (this
// package's own doc). Safe for concurrent use. The zero value is not usable;
// construct with NewTracker.
//
// # Durability is intentionally NOT provided (issue #3036)
//
// Tracker's state is a plain in-memory map (states below): every shape's
// calibration history — including any shape currently drift-alerting — is
// lost on every process restart or deploy. Unlike internal/optcorpus, which
// persists its query-outcome corpus to a JSONL file or a ClickHouse table
// (optcorpus.Sink) precisely because that corpus is expensive to
// re-accumulate (it joins system.query_log rows against dispatch-time
// records over a long observation window), Tracker deliberately has no
// durable-sink counterpart. This was evaluated against that same design —
// the pattern issue #3036 names as the one to mirror — and rejected for
// this type specifically, for three compounding reasons:
//
//  1. The tracked state is CHEAP to rebuild. Config.MinObservations
//     defaults to 2 and Config.EMAAlpha defaults to 0.2: a shape regains a
//     trusted verdict (CalibrationFactor ok, DriftReport.Alerting
//     meaningful) after just two fresh RecordActual calls post-restart, and
//     its EMA re-converges to within a few percent of its pre-restart value
//     within roughly 5-10 observations (2-3 alpha=0.2 half-lives) — a matter
//     of minutes of ordinary traffic for any shape common enough for its
//     calibration to matter. A shape too rare to see two observations in a
//     reasonable window was never going to accumulate meaningful evidence
//     between deploys either way.
//  2. Config.EntryTTL (default 30m) ALREADY caps how long any state is
//     trusted, restart or not. A shape not queried again within EntryTTL is
//     treated as expired and re-seeded from scratch by getOrCreateLocked
//     regardless of whether the process restarted in between. Persisting
//     across a restart would only ever help the narrow intersection "a
//     restart happens" AND "within 30 minutes of that shape's last query" —
//     a strictly smaller win than the TTL already discards on its own
//     timescale, for a feature this package's own anti-autotune stance
//     (see the package doc) already treats as advisory rather than
//     authoritative.
//  3. The feature ships DARK by default (Config.Enabled defaults to false —
//     see Config's own doc). No production deployment depends today on
//     drift continuity surviving a rollout, so paying a durable sink's
//     ongoing cost now — a background flush goroutine, a buffered channel
//     sized against burst writes, a JSONL or CH-table schema, boot wiring in
//     cmd/cerberus, and restart-recovery test coverage, all mirroring
//     optcorpus's own nontrivial machinery built for a fundamentally
//     heavier join-and-batch problem — would buy correctness insurance for a
//     value nobody is yet relying on. RecordActual is additionally called
//     synchronously from the query-response hot path
//     (internal/chclient/progress.go's flush()); any sink added later MUST
//     preserve that non-blocking discipline (an internal buffered channel
//     plus a background flush goroutine, exactly like optcorpus.Sink's own
//     writers), never a synchronous write on that path.
//
// This is a deliberate scope boundary, not an oversight: were Config.EntryTTL
// ever raised well past today's 30m default, or the feature to graduate out
// of dark-by-default with operators reporting real pain from losing
// alerting state across routine deploys, a Tracker-specific sink mirroring
// optcorpus.Sink's buffered-channel-plus-background-flush shape would earn
// its cost at that point — never a synchronous write retrofitted into
// RecordActual.
type Tracker struct {
	cfg Config

	mu     sync.Mutex
	states map[string]*state

	now func() time.Time // overridable by tests
}

// NewTracker constructs a Tracker from cfg. Every method on a nil *Tracker is
// a safe no-op (mirrors routememo's own "nil behaves like off" convention
// for every optional Engine-level mechanism), so callers do not need a
// separate nil check before every call site.
func NewTracker(cfg Config) *Tracker {
	return &Tracker{
		cfg:    cfg,
		states: make(map[string]*state),
		now:    time.Now,
	}
}

// SetNowForTest overrides the Tracker's time source — production code must
// never call this. Mirrors routememo.Memo.SetNowForTest's own doc.
func (t *Tracker) SetNowForTest(now func() time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.now = now
}

// RecordPredicted records shapeID's most recent advisory row prediction
// (from EXPLAIN ESTIMATE or the cardinality pre-probe — see
// solver.ScanEstimate.Rows's own doc for which producer supplied it). A
// later call OVERWRITES the previous prediction for the same shape rather
// than averaging: unlike the actual side, a fresh prediction is a
// synchronous re-derivation of the SAME advisory signal classify() is about
// to consume for THIS request, not a noisy repeated sample to smooth. No-op
// on a nil Tracker or an empty shapeID.
func (t *Tracker) RecordPredicted(shapeID string, rows uint64) {
	if t == nil || shapeID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.getOrCreateLocked(shapeID)
	st.hasPredicted = true
	st.predictedRows = rows
}

// RecordActual records one dispatch's measured resource usage for shapeID,
// updating the bounded EMA (Config.EMAAlpha) and returning the resulting
// DriftReport. No-op (zero DriftReport, false) on a nil Tracker or an empty
// shapeID.
func (t *Tracker) RecordActual(shapeID string, a Actual, source Source) (DriftReport, bool) {
	if t == nil || shapeID == "" {
		return DriftReport{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.getOrCreateLocked(shapeID)
	if !st.hasActual {
		st.emaRows = float64(a.ReadRows)
	} else {
		st.emaRows += t.cfg.EMAAlpha * (float64(a.ReadRows) - st.emaRows)
	}
	st.hasActual = true
	st.observations++
	st.lastSource = source
	st.lastObserved = t.now()
	return t.reportLocked(shapeID, st), true
}

// CalibrationFactor returns a BOUNDED multiplier (see
// minCalibrationFactor/maxCalibrationFactor) for shapeID's advisory row
// prediction, derived from how far the tracked actual EMA has drifted from
// the last prediction, or ok=false when there is not enough evidence yet
// (no prediction recorded, no actual recorded, or Observations below
// Config.MinObservations — the SAME corroboration floor DriftReport.Alerting
// uses, so a factor is never handed out on less evidence than an alert
// would need). Consumed by internal/engine's calibrateEstimate (Hook 1: the
// solver's carrier-geometry cost-model calibration) to nudge a FUTURE
// request's EXPLAIN ESTIMATE-derived row count toward what this shape has
// actually been costing, bounded so one noisy shape can never swing a
// single request's K clamp by more than 2x.
func (t *Tracker) CalibrationFactor(shapeID string) (factor float64, ok bool) {
	if t == nil || shapeID == "" {
		return 0, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st, exists := t.states[shapeID]
	if !exists || t.expiredLocked(st) {
		return 0, false
	}
	if !st.hasPredicted || !st.hasActual || st.observations < t.cfg.MinObservations {
		return 0, false
	}
	if st.predictedRows == 0 {
		return 0, false
	}
	factor = st.emaRows / float64(st.predictedRows)
	if factor < minCalibrationFactor {
		factor = minCalibrationFactor
	}
	if factor > maxCalibrationFactor {
		factor = maxCalibrationFactor
	}
	return factor, true
}

// Snapshot returns shapeID's current DriftReport, or ok=false when no live
// (unexpired) state exists for it.
func (t *Tracker) Snapshot(shapeID string) (DriftReport, bool) {
	if t == nil || shapeID == "" {
		return DriftReport{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st, exists := t.states[shapeID]
	if !exists || t.expiredLocked(st) {
		return DriftReport{}, false
	}
	return t.reportLocked(shapeID, st), true
}

// Stats is a point-in-time snapshot of the Tracker's resident state, for
// observability.
type Stats struct {
	Entries  int
	Alerting int
}

// Stats returns a snapshot of the Tracker's current size and how many
// tracked shapes are currently drift-alerting. Expired entries are excluded
// from both counts without being evicted (eviction is lazy, on next touch —
// mirroring routememo.Memo.getLiveLocked's own posture).
func (t *Tracker) Stats() Stats {
	if t == nil {
		return Stats{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var s Stats
	for id, st := range t.states {
		if t.expiredLocked(st) {
			continue
		}
		s.Entries++
		if t.reportLocked(id, st).Alerting {
			s.Alerting++
		}
	}
	return s
}

// getOrCreateLocked returns shapeID's state, creating (or resetting, if
// expired) it as needed and evicting one arbitrary entry at capacity —
// coarser than a true LRU, mirroring internal/engine's own
// ScanEstimateAdvisor.store / PerRungAdmissionLearner.record: the cost of
// evicting the wrong entry here is trivial (one shape re-learns its
// calibration a little sooner), so a full LRU buys nothing worth its own
// bookkeeping. Caller holds t.mu.
func (t *Tracker) getOrCreateLocked(shapeID string) *state {
	st, exists := t.states[shapeID]
	if exists && !t.expiredLocked(st) {
		return st
	}
	if !exists && len(t.states) >= trackerCapacity {
		for k := range t.states {
			delete(t.states, k)
			break
		}
	}
	st = &state{}
	t.states[shapeID] = st
	return st
}

// expiredLocked reports whether st has aged past Config.EntryTTL. A state
// with no actual observation yet (freshly seeded by RecordPredicted alone)
// never expires on that basis — LastObserved is the zero time.Time until
// the first RecordActual, so treat it as fresh; the predicted-only window
// between an advisory estimate and the request's own dispatch is always far
// shorter than any reasonable TTL. Caller holds t.mu.
func (t *Tracker) expiredLocked(st *state) bool {
	if !st.hasActual {
		return false
	}
	return t.now().Sub(st.lastObserved) >= t.cfg.EntryTTL
}

// reportLocked builds the DriftReport for shapeID's current state. Caller
// holds t.mu.
func (t *Tracker) reportLocked(shapeID string, st *state) DriftReport {
	r := DriftReport{
		ShapeID:       shapeID,
		HasPredicted:  st.hasPredicted,
		PredictedRows: st.predictedRows,
		ActualEMARows: st.emaRows,
		Observations:  st.observations,
		LastSource:    st.lastSource,
		LastObserved:  st.lastObserved,
	}
	r.Ratio, r.Alerting = t.driftLocked(st)
	return r
}

// driftLocked computes the ratio + alerting verdict for st. Caller holds
// t.mu.
//
// A zero PredictedRows (the index analysis pruned the scan to nothing) is
// deliberately NEVER alerting regardless of the actual: EXPLAIN ESTIMATE's
// own doc (internal/chclient/explain_estimate.go) already treats a
// near-zero estimate as strong, one-directional evidence — a scan-side
// upper bound of zero says the scan reads nothing, and if the actual EMA
// disagrees the interesting fact IS the ratio being undefined (division by
// zero), not a number this function should invent. A caller needing to flag
// "predicted zero, actual nonzero" reads HasPredicted + PredictedRows == 0
// directly.
func (t *Tracker) driftLocked(st *state) (ratio float64, alerting bool) {
	if !st.hasPredicted || !st.hasActual || st.predictedRows == 0 {
		return 0, false
	}
	if st.observations < t.cfg.MinObservations {
		return st.emaRows / float64(st.predictedRows), false
	}
	ratio = st.emaRows / float64(st.predictedRows)
	alerting = ratio < t.cfg.DriftLowerRatio || ratio > t.cfg.DriftUpperRatio
	return ratio, alerting
}
