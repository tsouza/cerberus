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
	// now (equal to Configured until the loop lowers it).
	Configured ThresholdInfo `json:"configured"`
	Live       ThresholdInfo `json:"live"`

	// Ticks counts completed fit cycles; LastFit is the most recent one (absent
	// until the first tick).
	Ticks   int64        `json:"ticks"`
	LastFit *AutotuneFit `json:"lastFit,omitempty"`
}

// ThresholdInfo is one (MinFanout, MinAnchorPairs) auto-gate pair.
type ThresholdInfo struct {
	MinFanout      int `json:"minFanout"`
	MinAnchorPairs int `json:"minAnchorPairs"`
}

// AutotuneFit is the outcome of one fit cycle.
type AutotuneFit struct {
	At            string        `json:"at"` // RFC 3339
	Reason        string        `json:"reason"`
	Changed       bool          `json:"changed"`
	HasOOMSignal  bool          `json:"hasOomSignal"`
	OOMMinFanout  int           `json:"oomMinFanout"`
	OOMMinAnchors int           `json:"oomMinAnchors"`
	Candidate     ThresholdInfo `json:"candidate"`
	Error         string        `json:"error,omitempty"`
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
