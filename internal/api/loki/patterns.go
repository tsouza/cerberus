package loki

import (
	"errors"
	"net/http"
)

// Pattern is one detected log-line template plus its time-bucketed
// sample counts. The upstream Loki contract (verified against
// `pkg/util/marshal/marshal.go:WriteQueryPatternsResponseJSON`) emits
// each sample as a `[unix_seconds, count]` 2-tuple — the timestamp is
// `sample.Timestamp.Unix()`, which strips the millisecond component.
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
// emits `{status:"success", data:[]}`. A drain3-equivalent pattern
// miner could run over the same peek-window used by /detected_fields if
// there's demand (see `docs/loki-patterns-impl-plan.md` PR B).
//
// Note the wire shape: `data` is a top-level array of pattern series,
// NOT a `{patterns: [...]}` wrapper. This matches upstream Loki's
// `WriteQueryPatternsResponseJSON` exactly so Grafana's pattern panel
// decodes cerberus responses without any client-side compatibility shim.
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
		Data:   []Pattern{},
	})
}
