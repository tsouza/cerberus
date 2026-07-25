package routememo

import "time"

// pressureTracker is a bounded, pruned set of (Key, lastResourceFailureTime)
// entries feeding the cluster-wide pressure discriminator: simultaneous
// resource failures across unrelated keys is the signature of a shared
// external cause (e.g. a noisy neighbour saturating the ClickHouse cluster),
// not a per-query one, and needs no signal beyond correlating what already
// flows through the same classifyRouteOutcome seam every dispatch already
// passes through.
//
// pressureTracker is NOT independently synchronized — every method is
// called only while the owning Memo holds its own mutex, so it stays a
// plain map instead of duplicating locking.
type pressureTracker struct {
	lastFailure map[Key]time.Time
}

func newPressureTracker() *pressureTracker {
	return &pressureTracker{lastFailure: make(map[Key]time.Time)}
}

// record timestamps a fresh ResourceFailure observation for k, from either
// route — a route-B resource failure is just as much a sign of shared
// pressure as a route-A one.
func (p *pressureTracker) record(k Key, now time.Time) {
	p.lastFailure[k] = now
}

// countFresh prunes every entry older than window and returns the number of
// DISTINCT keys with a resource failure still inside it.
func (p *pressureTracker) countFresh(now time.Time, window time.Duration) int {
	for k, t := range p.lastFailure {
		if now.Sub(t) >= window {
			delete(p.lastFailure, k)
		}
	}
	return len(p.lastFailure)
}
