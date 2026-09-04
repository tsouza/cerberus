package routememo

import "time"

// magnitude.go closes issue #2789's route-memo consumer, hook 2: refresh the
// route memo's priors with REAL MAGNITUDES, not just the success/failure
// bits Verdict already carries. The memo's existing state (Verdict.state,
// Verdict.corroboration) answers "should route B be tried for this Key" —
// a pure outcome bit — and says nothing about HOW MUCH data a successful
// route-B dispatch for that Key actually moved. RecordActualMagnitude adds
// that second axis without touching the outcome axis at all: it never
// creates, deletes, or changes a Key's LookupState, eviction order, or TTL,
// and no existing state-transition method's behavior changes by one bit —
// this file writes only the magnitudeEMA*/magnitudeObservations fields.
//
// Originally a purely OBSERVATIONAL hook (issue #2789's own "the others can
// be thinner initial hooks" allowance for its non-primary consumers): the
// magnitude was recorded for observability only, with the memo's own doc
// noting it "makes no routing decision based on it". Issue #3035 (split
// from #2853) closed that gap on both ends: RecordActualMagnitude gained
// production callers beyond per_rung_admission.go's narrow, single-route
// perRungObservingCursor — internal/engine/route_memo_wiring.go now feeds it
// from every clean route-B drain (memo-hit, first probe, and stale-PreferB
// re-validation alike), and MagnitudeFor gained a real routing consumer
// there too: a stale-PreferB re-validation whose tracked magnitude is
// well-corroborated (MinCorroboratingFailures readings) AND trivially small
// declines the automatic rescue-probe admission, corroboration-gated the
// same way every other verdict transition in this package is. See that
// file's own doc for the gate's exact shape and reasoning.
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
// k has no live entry or no magnitude has ever been recorded for it — still
// the common case for a Key that has never once completed a clean route-B
// drain (every fresh Unknown key, every BothFail key, and any PreferB key
// whose only route-B dispatches so far were mid-drain failures).
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
