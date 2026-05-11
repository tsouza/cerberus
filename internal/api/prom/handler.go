package prom

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// Querier is the subset of *chclient.Client that Handler needs. Stubbing
// it makes unit tests possible without a live ClickHouse.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) ([]chclient.Sample, error)
	QueryStrings(ctx context.Context, sql string, args ...any) ([]string, error)
	QueryLabelSets(ctx context.Context, sql string, args ...any) ([]map[string]string, error)
}

// Handler implements the Prometheus HTTP API endpoints cerberus speaks.
// Mount it via Handler.Mount(mux).
type Handler struct {
	Client    Querier
	Schema    schema.Metrics
	Optimizer *optimizer.Driver
	Logger    *slog.Logger

	parser promparser.Parser
}

// New constructs a Handler with the seed optimizer wired in.
func New(client Querier, s schema.Metrics, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		Client:    client,
		Schema:    s,
		Optimizer: optimizer.Default(),
		Logger:    logger,
		parser:    promparser.NewParser(promparser.Options{}),
	}
}

// Mount registers the Prom-compatible endpoints under /api/v1/ on mux.
func (h *Handler) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/query", h.handleQuery)
	mux.HandleFunc("GET /api/v1/query_range", h.handleQueryRange)
	mux.HandleFunc("POST /api/v1/query", h.handleQuery)
	mux.HandleFunc("POST /api/v1/query_range", h.handleQueryRange)
	mux.HandleFunc("GET /api/v1/labels", h.handleLabels)
	mux.HandleFunc("POST /api/v1/labels", h.handleLabels)
	mux.HandleFunc("GET /api/v1/label/{name}/values", h.handleLabelValues)
	mux.HandleFunc("GET /api/v1/series", h.handleSeries)
	mux.HandleFunc("POST /api/v1/series", h.handleSeries)
}

func (h *Handler) handleQuery(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("query")
	if q == "" {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("missing query parameter"))
		return
	}
	ts, err := parseTime(r.URL.Query().Get("time"), time.Now())
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}

	samples, err := h.executeInstant(r.Context(), q)
	if err != nil {
		h.respondError(w, err)
		return
	}

	result := toVector(samples, ts)
	writeJSON(w, http.StatusOK, Response{
		Status: "success",
		Data:   &QueryData{ResultType: "vector", Result: result},
	})
}

func (h *Handler) handleQueryRange(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("query")
	if q == "" {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("missing query parameter"))
		return
	}
	end, err := parseTime(r.URL.Query().Get("end"), time.Now())
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}

	// v0.1 range-query lowering returns the same row set as an instant
	// query, then projects every sample at the `end` timestamp. Real per-
	// step bucketing lands when the RangeWindow chplan node grows full
	// emission semantics (post-PR8 follow-up).
	samples, err := h.executeInstant(r.Context(), q)
	if err != nil {
		h.respondError(w, err)
		return
	}

	result := toMatrix(samples, end)
	writeJSON(w, http.StatusOK, Response{
		Status: "success",
		Data:   &QueryData{ResultType: "matrix", Result: result},
	})
}

// executeInstant lowers a PromQL string to chplan, wraps with a Project
// that selects exactly the four columns chclient.Sample expects, optimizes,
// emits SQL, and runs the query.
func (h *Handler) executeInstant(ctx context.Context, query string) ([]chclient.Sample, error) {
	expr, err := h.parser.ParseExpr(query)
	if err != nil {
		return nil, &apiError{kind: ErrBadData, err: err, status: http.StatusBadRequest}
	}
	plan, err := promql.Lower(expr, h.Schema)
	if err != nil {
		return nil, &apiError{kind: ErrExecution, err: err, status: http.StatusUnprocessableEntity}
	}

	plan = wrapWithSampleProjection(plan, h.Schema)
	plan = h.Optimizer.Run(plan)

	sql, args, err := chsql.Emit(plan)
	if err != nil {
		return nil, &apiError{kind: ErrInternal, err: err, status: http.StatusInternalServerError}
	}
	h.Logger.Debug("cerberus query", "promql", query, "sql", sql, "args", args)

	samples, err := h.Client.Query(ctx, sql, args...)
	if err != nil {
		return nil, &apiError{kind: ErrInternal, err: err, status: http.StatusBadGateway}
	}
	return samples, nil
}

// wrapWithSampleProjection adds a Project on top of plan that selects
// exactly MetricName, Attributes, TimeUnix, Value (in that order) so the
// chclient.Sample scanner can decode rows positionally.
func wrapWithSampleProjection(plan chplan.Node, s schema.Metrics) chplan.Node {
	return &chplan.Project{
		Input: plan,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}},
		},
	}
}

// toVector groups samples by label set, picks the latest value per series,
// and emits a Prom-shaped vector result. ts is the eval timestamp the
// caller asked for; we stamp every sample with it (Prometheus convention).
func toVector(samples []chclient.Sample, ts time.Time) []VectorSample {
	type latest struct {
		labels map[string]string
		ts     time.Time
		value  float64
	}

	bySeries := map[string]latest{}
	for _, s := range samples {
		labels := withMetricName(s.Labels, s.MetricName)
		key := canonicalKey(labels)
		cur, ok := bySeries[key]
		if !ok || s.Timestamp.After(cur.ts) {
			bySeries[key] = latest{labels: labels, ts: s.Timestamp, value: s.Value}
		}
	}

	out := make([]VectorSample, 0, len(bySeries))
	stamp := float64(ts.UnixMilli()) / 1e3
	for _, l := range bySeries {
		out = append(out, VectorSample{
			Metric: l.labels,
			Value:  Sample{stamp, strconv.FormatFloat(l.value, 'f', -1, 64)},
		})
	}
	return out
}

// toMatrix is the v0.1 placeholder range result — one point per series at
// the end timestamp. Full per-step rendering follows when RangeWindow
// emission lands.
func toMatrix(samples []chclient.Sample, end time.Time) []MatrixSample {
	type latest struct {
		labels map[string]string
		ts     time.Time
		value  float64
	}
	bySeries := map[string]latest{}
	for _, s := range samples {
		labels := withMetricName(s.Labels, s.MetricName)
		key := canonicalKey(labels)
		cur, ok := bySeries[key]
		if !ok || s.Timestamp.After(cur.ts) {
			bySeries[key] = latest{labels: labels, ts: s.Timestamp, value: s.Value}
		}
	}

	stamp := float64(end.UnixMilli()) / 1e3
	out := make([]MatrixSample, 0, len(bySeries))
	for _, l := range bySeries {
		out = append(out, MatrixSample{
			Metric: l.labels,
			Values: []Sample{{stamp, strconv.FormatFloat(l.value, 'f', -1, 64)}},
		})
	}
	return out
}

func withMetricName(labels map[string]string, name string) map[string]string {
	out := make(map[string]string, len(labels)+1)
	for k, v := range labels {
		out[k] = v
	}
	if name != "" {
		out["__name__"] = name
	}
	return out
}

// canonicalKey is a deterministic string form of a label set, used as a
// map key for grouping. Sorted by key ASCII to match what Prometheus does.
func canonicalKey(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	// Inline insertion sort — labels are typically small (<20).
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	var b []byte
	for _, k := range keys {
		b = append(b, k...)
		b = append(b, '=')
		b = append(b, labels[k]...)
		b = append(b, 0)
	}
	return string(b)
}

// parseTime parses a Prom-API time parameter — a Unix-seconds float or an
// RFC3339 timestamp. An empty string falls back to def.
func parseTime(raw string, def time.Time) (time.Time, error) {
	if raw == "" {
		return def, nil
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		return time.Unix(int64(f), int64((f-float64(int64(f)))*1e9)).UTC(), nil
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, errors.New("time parameter must be Unix seconds or RFC3339")
	}
	return t.UTC(), nil
}

// apiError carries the Prom errorType + an HTTP status code through the
// internal error path.
type apiError struct {
	kind   string
	err    error
	status int
}

func (e *apiError) Error() string { return e.err.Error() }
func (e *apiError) Unwrap() error { return e.err }

func (h *Handler) respondError(w http.ResponseWriter, err error) {
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		writeError(w, apiErr.status, apiErr.kind, apiErr.err)
		return
	}
	writeError(w, http.StatusInternalServerError, ErrInternal, err)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, kind string, err error) {
	writeJSON(w, status, Response{
		Status:    "error",
		ErrorType: kind,
		Error:     err.Error(),
	})
}
