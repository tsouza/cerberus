package routememo

import (
	"container/list"
	"time"
)

// pressureTracker is a bounded, pruned set of (Key, lastResourceFailureTime)
// entries feeding the cluster-wide pressure discriminator: simultaneous
// resource failures across unrelated keys is the signature of a shared
// external cause (e.g. a noisy neighbour saturating the ClickHouse cluster),
// not a per-query one, and needs no signal beyond correlating what already
// flows through the same classifyRouteOutcome seam every dispatch already
// passes through.
//
// Bounded on two independent axes, matching the resident-size discipline
// Memo already applies to its own verdict map: TIME (countFresh prunes any
// entry older than the caller's window on every call) and SIZE (record
// evicts the single stalest entry once pressureTrackerMaxEntries is
// exceeded, a hard backstop for a burst of distinct keys arriving faster
// than the time-based prune can retire them). Ordering the entries oldest-
// to-newest by record time — record always moves a key to the back — lets
// countFresh prune from the front and STOP at the first still-fresh entry,
// so the common case (most entries are fresh) costs O(entries actually
// pruned this call), not O(all entries), even while holding the owning
// Memo's mutex for the duration.
//
// pressureTracker is NOT independently synchronized — every method is
// called only while the owning Memo holds its own mutex, so it stays a
// plain map instead of duplicating locking.
type pressureTracker struct {
	lastFailure map[Key]time.Time
	order       *list.List // oldest-recorded at Front, newest at Back
	index       map[Key]*list.Element
}

// pressureTrackerMaxEntries bounds the tracker's resident size independently
// of the time-based prune in countFresh. Reuses memoMaxEntries's rationale
// verbatim: the same order-of-magnitude bound on distinct in-flight Key
// cardinality this process is sized for, not a fresh number to justify.
const pressureTrackerMaxEntries = memoMaxEntries

func newPressureTracker() *pressureTracker {
	return &pressureTracker{
		lastFailure: make(map[Key]time.Time),
		order:       list.New(),
		index:       make(map[Key]*list.Element),
	}
}

// record timestamps a fresh ResourceFailure observation for k, from either
// route — a route-B resource failure is just as much a sign of shared
// pressure as a route-A one. A re-record of an existing key moves it to the
// back of the recency order, same as Memo's own touchLRULocked.
func (p *pressureTracker) record(k Key, now time.Time) {
	p.lastFailure[k] = now
	if el, ok := p.index[k]; ok {
		p.order.MoveToBack(el)
	} else {
		p.index[k] = p.order.PushBack(k)
	}
	p.evictOldestIfOverCap()
}

// countFresh prunes every entry older than window — walking from the front
// (oldest) and stopping at the first entry still inside window, since
// record's back-insertion keeps the list ordered oldest-to-newest by
// (approximate) record time — and returns the number of DISTINCT keys with
// a resource failure still inside it.
func (p *pressureTracker) countFresh(now time.Time, window time.Duration) int {
	for {
		front := p.order.Front()
		if front == nil {
			break
		}
		k, _ := front.Value.(Key)
		if now.Sub(p.lastFailure[k]) < window {
			break
		}
		p.removeLocked(k, front)
	}
	return len(p.lastFailure)
}

// evictOldestIfOverCap drops the single stalest entry when the tracker
// exceeds pressureTrackerMaxEntries, independent of whether that entry has
// aged out of any caller's window yet — the size cap is a resource-safety
// backstop, not a freshness judgement.
func (p *pressureTracker) evictOldestIfOverCap() {
	if len(p.lastFailure) <= pressureTrackerMaxEntries {
		return
	}
	front := p.order.Front()
	if front == nil {
		return
	}
	k, _ := front.Value.(Key)
	p.removeLocked(k, front)
}

func (p *pressureTracker) removeLocked(k Key, el *list.Element) {
	p.order.Remove(el)
	delete(p.index, k)
	delete(p.lastFailure, k)
}
