package info

import (
	"encoding/json"
	"net/http"
)

// AutotuneStatus is the JSON body of GET /info/autotune: the self-driving
// solver's live decision state. It is defined here (rather than imported from
// internal/autotune) so the info layer stays decoupled from the solver stack;
// cmd/ maps the autotune snapshot into this shape.
type AutotuneStatus struct {
	// Enabled reflects the CERBERUS_SOLVER_AUTOTUNE toggle. Active is true only
	// when the loop is actually running (enabled AND auto mode AND the corpus CH
	// table is available). Reason explains Active: "active" | "disabled" |
	// "not-auto-mode" | "corpus-unavailable".
	Enabled bool   `json:"enabled"`
	Active  bool   `json:"active"`
	Reason  string `json:"reason"`

	IntervalSeconds     float64 `json:"intervalSeconds,omitempty"`
	CorpusWindowSeconds float64 `json:"corpusWindowSeconds,omitempty"`

	// Configured is the shipped gate; Live is what the Planner routes with right
	// now (equal to Configured until the loop lowers it). The delta shows exactly
	// what the loop has done.
	Configured ThresholdInfo `json:"configured"`
	Live       ThresholdInfo `json:"live"`

	// Stats aggregates the loop's own behavior (process health); Outcome is the
	// rolling-window efficacy signal from the corpus.
	Stats   AutotuneStats   `json:"stats"`
	Outcome AutotuneOutcome `json:"outcome"`
}

// ThresholdInfo is one (MinFanout, MinAnchorPairs) auto-gate pair.
type ThresholdInfo struct {
	MinFanout      int `json:"minFanout"`
	MinAnchorPairs int `json:"minAnchorPairs"`
}

// AutotuneStats aggregates the loop's behavior across ticks. Convergence reads as
// a high ticksSinceChange with a small, stable appliedTicks; trouble reads as
// climbing errorTicks, or signalTicks == 0 while route-A OOMs persist.
type AutotuneStats struct {
	Ticks            int64  `json:"ticks"`
	SignalTicks      int64  `json:"signalTicks"`
	AppliedTicks     int64  `json:"appliedTicks"`
	ErrorTicks       int64  `json:"errorTicks"`
	TicksSinceChange int64  `json:"ticksSinceChange"`
	LastChangeAt     string `json:"lastChangeAt,omitempty"` // RFC 3339
	LastError        string `json:"lastError,omitempty"`
	LastErrorAt      string `json:"lastErrorAt,omitempty"` // RFC 3339
}

// AutotuneOutcome is the rolling-window efficacy signal (over corpusWindowSeconds).
// Good: routeBOoms == 0 (the safe path isn't itself OOMing), routeAOoms trending
// to 0 (unprotected OOMs eliminated) with routeBExecutions > 0 (volume protected).
// Bad: routeBOoms > 0, or routeAOoms persistently high.
type AutotuneOutcome struct {
	At               string `json:"at,omitempty"` // RFC 3339, most recent tick
	HasSignal        bool   `json:"hasSignal"`
	OOMMinFanout     int    `json:"oomMinFanout"`
	OOMMinAnchors    int    `json:"oomMinAnchors"`
	RouteAOoms       int64  `json:"routeAOoms"`
	RouteBExecutions int64  `json:"routeBExecutions"`
	RouteBOoms       int64  `json:"routeBOoms"`
}

// handleAutotune serves GET /info/autotune. It is unauthenticated and read-only,
// like GET /info. When autotune introspection is not wired (nil func) or reports
// unavailable, it returns 404.
func (h *Handler) handleAutotune(w http.ResponseWriter, _ *http.Request) {
	if h.autotune == nil {
		http.Error(w, "autotune introspection not configured", http.StatusNotFound)
		return
	}
	st, ok := h.autotune()
	if !ok {
		http.Error(w, "autotune introspection unavailable", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(st)
}
