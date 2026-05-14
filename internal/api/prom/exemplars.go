package prom

import (
	"errors"
	"net/http"
	"time"

	"github.com/tsouza/cerberus/internal/api/format"
)

// ExemplarSeries is the wire shape for one element of the
// `/api/v1/query_exemplars` data array — one series identified by its
// label set with the matched exemplars grouped under it.
type ExemplarSeries struct {
	SeriesLabels map[string]string `json:"seriesLabels"`
	Exemplars    []Exemplar        `json:"exemplars"`
}

// Exemplar is one exemplar inside an ExemplarSeries. `Value` is a float
// (Prom's exemplar JSON keeps it as a number, unlike Sample which
// stringifies for precision). `Timestamp` is unix seconds with fractional
// nanos.
type Exemplar struct {
	Labels    map[string]string `json:"labels"`
	Value     float64           `json:"value"`
	Timestamp float64           `json:"timestamp"`
}

// handleQueryExemplars implements `/api/v1/query_exemplars`.
//
// Upstream contract:
// https://prometheus.io/docs/prometheus/latest/querying/api/#querying-exemplars
//
// Required params: `query` (PromQL string), `start` and `end` (RFC3339 or
// unix seconds). The response is the canonical Prom envelope with `data`
// shaped as []ExemplarSeries.
//
// Implementation status (RC2): cerberus's metrics schema does not yet
// surface OTel exemplars — the OTel ClickHouse Exporter v0.x writes
// exemplars only into the `otel_traces_*` tables, and `schema.Metrics`
// has no Exemplars column to point at. We still validate inputs (parse
// the PromQL, range-check `start`/`end`) so Grafana receives the right
// error envelopes on bad probes, then return `{status:"success",
// data:[]}` so the exemplars probe doesn't crash the dashboard.
//
// TODO(RC2/RC3 schema): once `internal/schema/otel.go` exposes an
// exemplars column (or a sibling-table join target), wire the SQL via
// `internal/chsql.Builder` — extract VectorSelector matchers from the
// parsed PromQL expression, build a SELECT against the metric's
// exemplars source with the matcher predicates in WHERE, time-bounded
// on `[start, end]`, and shape the result rows into ExemplarSeries.
// See `docs/roadmap.md § RC2 PromQL HTTP APIs`.
func (h *Handler) handleQueryExemplars(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
	}
	q := r.URL.Query().Get("query")
	if q == "" && r.Method == http.MethodPost {
		q = r.PostForm.Get("query")
	}
	if q == "" {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("missing query parameter"))
		return
	}
	if _, err := h.parseExpr(r.Context(), q); err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}

	startRaw := r.URL.Query().Get("start")
	endRaw := r.URL.Query().Get("end")
	if startRaw == "" && r.Method == http.MethodPost {
		startRaw = r.PostForm.Get("start")
	}
	if endRaw == "" && r.Method == http.MethodPost {
		endRaw = r.PostForm.Get("end")
	}
	start, err := format.ParseTimeProm(startRaw, time.Time{})
	if err != nil || start.IsZero() {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("missing or invalid 'start' parameter"))
		return
	}
	end, err := format.ParseTimeProm(endRaw, time.Time{})
	if err != nil || end.IsZero() {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("missing or invalid 'end' parameter"))
		return
	}
	if end.Before(start) {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("'end' must be after 'start'"))
		return
	}

	// Empty-data path — see the schema TODO on the function docstring.
	// Return `[]ExemplarSeries{}` (not nil) so the JSON envelope renders
	// as `"data":[]` rather than `"data":null`; Grafana's exemplars probe
	// distinguishes the two.
	writeJSON(w, http.StatusOK, Response{
		Status: "success",
		Data:   []ExemplarSeries{},
	})
}
