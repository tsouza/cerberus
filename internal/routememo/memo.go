package routememo

import (
	"container/list"
	"sync"
	"time"
)

// Outcome buckets a dispatch's terminal result into exactly one of three
// evidence classes. Only OutcomeSuccess and OutcomeResourceFailure are ever
// written into memo state (see Memo.Observe) — OutcomeNoEvidence means
// "this dispatch's outcome says nothing about whether route B helps route
// A's failure mode", so it never touches a verdict.
type Outcome int

const (
	// OutcomeNoEvidence covers every error that is not a typed resource
	// rejection: a solver wall-clock deadline, an open circuit breaker, a
	// gate/context cancellation, a broken connection already retried and
	// exhausted inside chclient, the sample budget, or a pre-flight SQL-emit
	// failure. None of these say anything about whether sharding helps —
	// recording them would poison the memo with unrelated evidence
	// (Major-6).
	OutcomeNoEvidence Outcome = iota
	// OutcomeSuccess is a clean dispatch completion.
	OutcomeSuccess
	// OutcomeResourceFailure is a typed resource-exhaustion rejection — the
	// only NEGATIVE evidence the memo ever learns from.
	OutcomeResourceFailure
)

// LookupState is the result of a Lookup: whether prior evidence exists for
// a Key and, for a positive verdict, whether it must be re-validated before
// being trusted.
type LookupState int

const (
	// Unknown: no verdict recorded for this Key (or it aged out, or route A
	// has not yet failed enough times to be probe-eligible). The caller
	// takes the normal route-A path; a route-A outcome should still be
	// Observed so corroboration bookkeeping progresses.
	Unknown LookupState = iota
	// PreferB: route A has failed on this Key at least
	// minCorroboratingFailures times with no intervening Success, and a
	// route-B probe subsequently succeeded. The caller MAY memo-hit route B
	// — subject to re-checking every structural/freshness/admission gate
	// itself; Lookup makes no promise about any of those.
	PreferB
	// BothFail: route B was tried for this Key and itself failed with a
	// resource failure. The caller stays on route A; no further probing is
	// warranted until the entry ages out at memoEntryTTL.
	BothFail
)

// Named, meaning-bearing constants — see docs/solver.md for the full
// rationale of each.
const (
	// memoMaxEntries bounds the memo's resident size. A bounded LRU evicts
	// the least-recently-touched entry once this is exceeded, so the memo
	// cannot grow without bound under high key cardinality.
	memoMaxEntries = 4096

	// memoEntryTTL is how long any verdict is trusted at all, counted from
	// creation. It is never refreshed by a lookup, nor by a plain
	// confirming route-A success on an Unknown/bookkeeping entry — only a
	// route-A ResourceFailure against a PreferB entry (the stale
	// re-validation path) or a fresh route-B Observe restamps it.
	memoEntryTTL = 30 * time.Minute

	// reValidationFraction places re-validation at the TTL midpoint — a
	// fixed relationship to memoEntryTTL rather than a second freestanding
	// duration.
	reValidationFraction = 2

	// maxConcurrentRoutedDispatches bounds every non-baseline dispatch —
	// probe, retry, AND memo-hit alike — behind one process-wide,
	// non-blocking semaphore. Route B can therefore never be the thing that
	// queues or stampedes the shared connection gate: the worst case for a
	// PreferB key under contention degrades to route-A traffic, which is
	// exactly today's (pre-memo) behavior.
	maxConcurrentRoutedDispatches = 4

	// minCorroboratingFailures is how many consecutive route-A
	// ResourceFailure observations (with no intervening Success) a Key
	// needs before it becomes probe-eligible. A single transient resource
	// rejection cannot, by itself, teach the memo anything.
	minCorroboratingFailures = 2

	// pressureFailureThreshold is how many DISTINCT keys must show a fresh
	// ResourceFailure within the pressure window before the environment is
	// flagged under pressure. Defined relative to minCorroboratingFailures
	// so a single hot key's own corroboration count can never trip it
	// alone — it takes failures spread across more keys than any one key
	// could contribute.
	pressureFailureThreshold = minCorroboratingFailures + 1
)

// Verdict is the memo's per-Key state. Unexported: callers only ever see it
// through Lookup's (LookupState, stale) projection.
type Verdict struct {
	state LookupState
	// createdAt is the TTL/re-validation clock. Stamped once per verdict
	// transition (a new PreferB, a new BothFail, or a stale-PreferB
	// re-confirmed by another route-A failure); never touched by a lookup
	// or by a plain confirming success.
	createdAt time.Time
	// corroboration is the consecutive route-A ResourceFailure count with
	// no intervening Success, for an Unknown (not-yet-probed) entry only.
	corroboration int
}

// Memo is the bounded, in-process failure-driven route memo (docs/solver.md
// §"Failure-driven route memo"). It stores routing DECISIONS only — never
// result rows, query text, or any per-request literal — and is safe for
// concurrent use. The zero value is not usable; construct with New.
type Memo struct {
	mu       sync.Mutex
	entries  map[Key]*Verdict
	lruList  *list.List
	lruIndex map[Key]*list.Element

	dispatchTokens chan struct{} // non-blocking semaphore, size maxConcurrentRoutedDispatches

	pressureWindow time.Duration
	pressure       *pressureTracker

	now func() time.Time // overridable by tests
}

// New builds an empty Memo. pressureWindow is the horizon the cluster-wide
// pressure damper correlates distinct-key resource failures over — callers
// pass the same horizon their dispatch deadline uses, since that is how
// long a single fan-out's resource pressure stays live server-side.
func New(pressureWindow time.Duration) *Memo {
	return &Memo{
		entries:        make(map[Key]*Verdict),
		lruList:        list.New(),
		lruIndex:       make(map[Key]*list.Element),
		dispatchTokens: make(chan struct{}, maxConcurrentRoutedDispatches),
		pressureWindow: pressureWindow,
		pressure:       newPressureTracker(),
		now:            time.Now,
	}
}

// Lookup reports the recorded verdict for k. stale is true only for a
// PreferB verdict that has crossed its re-validation midpoint: the caller
// must NOT memo-hit route B for a stale verdict — it routes through A as if
// the Key were unknown, and that attempt's Observe'd outcome decides the
// entry's fate (Major-5).
func (m *Memo) Lookup(k Key) (state LookupState, stale bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	v, live := m.getLiveLocked(k, m.now())
	if !live {
		return Unknown, false
	}
	if v.state == PreferB {
		reValidateAt := v.createdAt.Add(memoEntryTTL / reValidationFraction)
		if !m.now().Before(reValidateAt) {
			return PreferB, true
		}
	}
	return v.state, false
}

// AdmitDispatch is the single, process-wide, non-blocking admission
// semaphore covering EVERY non-baseline dispatch — probe, retry, and
// memo-hit alike. On success, release MUST be called exactly once when the
// dispatch finishes (success or failure). On denial, ok is false and
// release is a no-op — callers must not call it, but it is safe to.
func (m *Memo) AdmitDispatch() (release func(), ok bool) {
	select {
	case m.dispatchTokens <- struct{}{}:
		return m.releaseToken, true
	default:
		return noopRelease, false
	}
}

func (m *Memo) releaseToken() { <-m.dispatchTokens }

func noopRelease() {}

// BeginProbe atomically checks probe-eligibility (corroboration met, not
// under cluster-wide pressure) and reserves an AdmitDispatch token, inside
// one critical section, for a route-A-failed Key that has never been
// probed (Unknown, no verdict yet). It is the single-flight admission path
// a caller uses right after observing route-A's Nth consecutive resource
// failure. On success, release must be called exactly once when the probe
// dispatch finishes.
func (m *Memo) BeginProbe(k Key) (release func(), ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	if m.pressure.countFresh(now, m.pressureWindow) > pressureFailureThreshold {
		return noopRelease, false
	}
	v, live := m.getLiveLocked(k, now)
	if !live || v.state != Unknown || v.corroboration < minCorroboratingFailures {
		return noopRelease, false
	}
	select {
	case m.dispatchTokens <- struct{}{}:
		return m.releaseToken, true
	default:
		return noopRelease, false
	}
}

// UnderPressure reports whether more than pressureFailureThreshold distinct
// Keys have shown a resource failure within the pressure window. While
// true, callers must not admit a probe or a memo-hit route-B dispatch (both
// fall back to route A), and Observe writes no memo state — a cluster-wide
// event can only suppress action, never mass-teach bad verdicts.
func (m *Memo) UnderPressure() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pressure.countFresh(m.now(), m.pressureWindow) > pressureFailureThreshold
}

// Observe records one dispatch's classified outcome for k, on route. Only
// OutcomeSuccess and OutcomeResourceFailure ever touch memo state;
// OutcomeNoEvidence is a pure no-op. While UnderPressure(), no write
// happens at all (pressure tracking itself still records the failure, so
// the window can naturally decay once traffic quiets).
func (m *Memo) Observe(k Key, route Route, outcome Outcome) {
	if outcome == OutcomeNoEvidence {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	if outcome == OutcomeResourceFailure {
		m.pressure.record(k, now)
	}
	if m.pressure.countFresh(now, m.pressureWindow) > pressureFailureThreshold {
		return
	}

	switch route {
	case RouteA:
		m.observeRouteALocked(k, now, outcome)
	case RouteB:
		m.observeRouteBLocked(k, now, outcome)
	}
}

func (m *Memo) observeRouteALocked(k Key, now time.Time, outcome Outcome) {
	v, live := m.getLiveLocked(k, now)

	switch outcome {
	case OutcomeSuccess:
		// A route-A success always contradicts "A always fails here" —
		// drop any recorded state (positive verdict or bare corroboration
		// bookkeeping) for this Key immediately.
		if live {
			m.deleteLocked(k)
		}
	case OutcomeResourceFailure:
		if !live {
			m.entries[k] = &Verdict{state: Unknown, createdAt: now, corroboration: 1}
			m.touchLRULocked(k)
			m.evictIfNeededLocked()
			return
		}
		switch v.state {
		case PreferB:
			// Stale re-validation path: A was routed through instead of a
			// memo-hit (Lookup reported stale=true), and it failed again —
			// the failing premise was just re-confirmed directly, without
			// re-probing B. Refresh the TTL clock; do not re-probe.
			v.createdAt = now
		case BothFail:
			// No positive result to protect; it simply expires from its
			// own creation at memoEntryTTL.
		case Unknown:
			if v.corroboration < minCorroboratingFailures {
				v.corroboration++
			}
		}
	}
}

func (m *Memo) observeRouteBLocked(k Key, now time.Time, outcome Outcome) {
	switch outcome {
	case OutcomeSuccess:
		m.entries[k] = &Verdict{state: PreferB, createdAt: now}
	case OutcomeResourceFailure:
		m.entries[k] = &Verdict{state: BothFail, createdAt: now}
	default:
		return
	}
	m.touchLRULocked(k)
	m.evictIfNeededLocked()
}

// getLiveLocked returns k's Verdict, evicting and reporting absent if it
// has crossed memoEntryTTL.
func (m *Memo) getLiveLocked(k Key, now time.Time) (*Verdict, bool) {
	v, ok := m.entries[k]
	if !ok {
		return nil, false
	}
	if now.Sub(v.createdAt) >= memoEntryTTL {
		m.deleteLocked(k)
		return nil, false
	}
	return v, true
}

func (m *Memo) deleteLocked(k Key) {
	delete(m.entries, k)
	if el, ok := m.lruIndex[k]; ok {
		m.lruList.Remove(el)
		delete(m.lruIndex, k)
	}
}

func (m *Memo) touchLRULocked(k Key) {
	if el, ok := m.lruIndex[k]; ok {
		m.lruList.MoveToBack(el)
		return
	}
	m.lruIndex[k] = m.lruList.PushBack(k)
}

func (m *Memo) evictIfNeededLocked() {
	for len(m.entries) > memoMaxEntries {
		front := m.lruList.Front()
		if front == nil {
			return
		}
		k, _ := front.Value.(Key)
		m.lruList.Remove(front)
		delete(m.lruIndex, k)
		delete(m.entries, k)
	}
}

// Stats is a point-in-time snapshot of the memo's resident state, for
// observability (cerberus_solver_route_memo_* gauges).
type Stats struct {
	Entries  int
	PreferB  int
	BothFail int
}

// Stats returns a snapshot of the memo's current size and verdict mix.
func (m *Memo) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()

	s := Stats{Entries: len(m.entries)}
	for _, v := range m.entries {
		switch v.state {
		case PreferB:
			s.PreferB++
		case BothFail:
			s.BothFail++
		}
	}
	return s
}
