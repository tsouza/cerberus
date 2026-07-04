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

	// Stats aggregates the loop's own behavior; Outcome is the rolling-window
	// efficacy signal from the corpus. Together they answer "is it doing good?".
	Stats   Stats
	Outcome Outcome
}

// Stats aggregates the loop's behavior across ticks (process health): whether it
// has converged (TicksSinceChange high, AppliedTicks small and stable), whether
// it is finding signal, and whether it is erroring.
type Stats struct {
	Ticks            int64
	SignalTicks      int64
	AppliedTicks     int64
	ErrorTicks       int64
	TicksSinceChange int64
	LastChangeAt     time.Time
	LastError        string
	LastErrorAt      time.Time
}

// Outcome is the rolling-window efficacy signal from the corpus, refreshed each
// tick. RouteAOomCount is the eligible OOM population the loop targets (good =
// trending to 0 as it protects them); RouteBExecutions is the volume it routes to
// the safe path; RouteBOomCount is route-B OOMs — the mitigation itself failing,
// which should stay 0 (nonzero = bad). OOMMin* is the last observed floor.
type Outcome struct {
	At               time.Time
	HasSignal        bool
	OOMMinFanout     int
	OOMMinAnchors    int
	RouteAOomCount   int64
	RouteBExecutions int64
	RouteBOomCount   int64
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
