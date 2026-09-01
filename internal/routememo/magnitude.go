package routememo

import "time"

// magnitude.go closes issue #2789's route-memo consumer, hook 2: refresh the
// route memo's priors with REAL MAGNITUDES, not just the success/failure
// bits Verdict already carries. The memo's existing state (Verdict.state,
// Verdict.corroboration) answers "should route B be tried for this Key" —
// a pure outcome bit — and says nothing about HOW MUCH data a successful
// route-B dispatch for that Key actually moved. RecordActualMagnitude adds
// that second, purely OBSERVATIONAL axis without touching the outcome
// axis at all: it never creates, deletes, or changes a Key's LookupState,
// eviction order, or TTL, and no existing method's behavior changes by one
// bit. Deliberately a THIN hook (this file's own doc names it as such,
// mirroring issue #2789's own "the others can be thinner initial hooks"
// allowance for its three non-primary consumers): the magnitude is recorded
// for observability today (Stats-style callers, an operator dashboard,
// internal/engine's actuals wiring), and is architecture the memo already
// supports for a future routing use — but this file itself makes no routing
// decision based on it.
//
// Only a LIVE (unexpired) entry is updated — RecordActualMagnitude never
// CREATES an entry, unlike every state-transition method in memo.go. A
// magnitude reading is meaningless without an existing verdict to annotate:
// there is no routing state for it to describe otherwise.

// magnitudeEMAAlpha bounds the per-observation influence of a fresh
// magnitude reading on a Key's tracked average rows, mirroring
// internal/actuals.Config's own EMAAlpha default (0.2) and its
// bounded-influence rationale (issue #2789's anti-autotune stance,
// restated here because this is a second, independent implementation of
// the same bound rather than a shared helper — routememo must not import
// internal/actuals, see .go-arch-lint.yml).
const magnitudeEMAAlpha = 0.2

// RecordActualMagnitude records outputRows as one fresh observation of how
// much data a route-B dispatch for k actually produced, updating a bounded
// exponential moving average — the same bounded-influence shape
// internal/actuals.Tracker.RecordActual uses for its own EMA, applied here
// to route memo's Key granularity instead of a plan-shape-id string. No-op
// when k has no live (unexpired) entry: see this file's own doc for why a
// magnitude never creates one.
func (m *Memo) RecordActualMagnitude(k Key, outputRows uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, live := m.getLiveLocked(k, m.now())
	if !live {
		return
	}
	if v.magnitudeObservations == 0 {
		v.magnitudeEMARows = float64(outputRows)
	} else {
		v.magnitudeEMARows += magnitudeEMAAlpha * (float64(outputRows) - v.magnitudeEMARows)
	}
	v.magnitudeObservations++
	v.magnitudeObservedAt = m.now()
}

// MagnitudeFor returns k's tracked actual-magnitude EMA, or ok=false when
// k has no live entry or no magnitude has ever been recorded for it (the
// overwhelmingly common case today: RecordActualMagnitude has exactly one
// caller, per_rung_admission.go's perRungObservingCursor, so most Keys
// carry no magnitude reading at all).
func (m *Memo) MagnitudeFor(k Key) (rows float64, observations int, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, live := m.getLiveLocked(k, m.now())
	if !live || v.magnitudeObservations == 0 {
		return 0, 0, false
	}
	return v.magnitudeEMARows, v.magnitudeObservations, true
}

// magnitudeObservedAtFor is a test-only accessor for
// Verdict.magnitudeObservedAt, so magnitude_test.go can assert the
// bookkeeping timestamp without exporting the field itself.
func (m *Memo) magnitudeObservedAtFor(k Key) time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, live := m.getLiveLocked(k, m.now())
	if !live {
		return time.Time{}
	}
	return v.magnitudeObservedAt
}
