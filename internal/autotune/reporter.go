package autotune

import (
	"sync/atomic"
	"time"

	"github.com/tsouza/cerberus/internal/routerrules"
)

// Status is a point-in-time snapshot of the self-driving loop's decision state,
// exposed for introspection (e.g. the /info/autotune endpoint). It is a plain
// value — the Reporter hands out copies, never a live pointer.
type Status struct {
	// Enabled reflects CERBERUS_SOLVER_AUTOTUNE. Active is true only when the
	// loop is actually running (enabled AND auto mode AND the corpus CH table is
	// available). Reason explains the Active state: "active" | "disabled" |
	// "not-auto-mode" | "corpus-unavailable".
	Enabled bool
	Active  bool
	Reason  string

	IntervalSeconds     float64
	CorpusWindowSeconds float64

	// Configured is the gate the deployment shipped with; Live is what the
	// Planner is using right now (Configured until the loop lowers it).
	Configured routerrules.Thresholds
	Live       routerrules.Thresholds

	// Ticks counts completed fit cycles; LastFit is the most recent one (nil
	// until the first tick).
	Ticks   int64
	LastFit *FitReport
}

// FitReport is the outcome of one fit cycle.
type FitReport struct {
	At            time.Time
	Reason        string
	Changed       bool
	HasOOMSignal  bool
	OOMMinFanout  int
	OOMMinAnchors int
	Candidate     routerrules.Thresholds
	Error         string
}

// Reason values for Status.Reason.
const (
	ReasonStatusActive            = "active"
	ReasonStatusDisabled          = "disabled"
	ReasonStatusNotAutoMode       = "not-auto-mode"
	ReasonStatusCorpusUnavailable = "corpus-unavailable"
)

// Reporter is a thread-safe holder of the latest Status. main/ seeds it with the
// initial (pre-first-tick) status; the loop goroutine updates it after each tick
// (single writer). The /info handler reads Snapshot concurrently.
type Reporter struct {
	v atomic.Pointer[Status]
}

// NewReporter builds a Reporter seeded with initial.
func NewReporter(initial Status) *Reporter {
	r := &Reporter{}
	r.v.Store(&initial)
	return r
}

// Set replaces the reported status.
func (r *Reporter) Set(s Status) { r.v.Store(&s) }

// Snapshot returns a copy of the current status.
func (r *Reporter) Snapshot() Status { return *r.v.Load() }
