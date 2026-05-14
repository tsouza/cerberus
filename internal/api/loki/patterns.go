package loki

import (
	"errors"
	"net/http"
)

// PatternsData is the body of a /loki/api/v1/patterns response. Each
// pattern is a {pattern, samples} object where samples is a slice of
// (unix_ms, count) tuples — matching Loki's documented schema.
//
// Cerberus's first cut returns an empty pattern list (see handler
// docstring for the deferral rationale). The type is concrete so we can
// fill it in without changing the wire shape later.
type PatternsData struct {
	Patterns []Pattern `json:"patterns"`
}

// Pattern is one detected log-line template plus its time-bucketed
// sample counts.
type Pattern struct {
	Pattern string     `json:"pattern"`
	Samples [][2]int64 `json:"samples"`
}

// handlePatterns implements GET /loki/api/v1/patterns.
//
// Upstream Loki 3.x exposes a pattern-discovery subsystem (drain3-style
// log template extraction) on this endpoint. Cerberus does not run a
// pattern-discovery algorithm itself today — the algorithm is its own
// beast and Grafana's pattern panel renders an empty result gracefully.
// The endpoint exists so Grafana's panel doesn't 404; it validates the
// caller's parameters (so a broken `query` still returns 400) and then
// emits {status:"success", data:{patterns:[]}}. A future release can
// run a drain3-equivalent over the same peek-window used by
// /detected_fields if there's demand.
func (h *Handler) handlePatterns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("query")
	if q == "" {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("missing query parameter"))
		return
	}
	if _, _, err := parseStartEnd(r); err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}
	if _, err := selectorMatchers(q); err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}

	writeJSON(w, http.StatusOK, Response{
		Status: "success",
		Data:   PatternsData{Patterns: []Pattern{}},
	})
}
