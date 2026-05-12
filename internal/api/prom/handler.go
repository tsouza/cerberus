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
	QueryMetricMeta(ctx context.Context, sql, metricType string, args ...any) ([]chclient.MetricMetaRow, error)
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
// Each route is wrapped with promHeadersMiddleware so responses carry
// `X-Prometheus-API-Version` and `X-Cerberus-CH-Millis`.
func (h *Handler) Mount(mux *http.ServeMux) {
	register := func(pattern string, hf http.HandlerFunc) {
		mux.Handle(pattern, promHeadersMiddleware(hf))
	}
	register("GET /api/v1/query", h.handleQuery)
	register("GET /api/v1/query_range", h.handleQueryRange)
	register("POST /api/v1/query", h.handleQuery)
	register("POST /api/v1/query_range", h.handleQueryRange)
	register("GET /api/v1/labels", h.handleLabels)
	register("POST /api/v1/labels", h.handleLabels)
	register("GET /api/v1/label/{name}/values", h.handleLabelValues)
	register("GET /api/v1/series", h.handleSeries)
	register("POST /api/v1/series", h.handleSeries)
	register("GET /api/v1/metadata", h.handleMetadata)
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
	start, err := parseTime(r.URL.Query().Get("start"), time.Time{})
	if err != nil || start.IsZero() {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("missing or invalid 'start' parameter"))
		return
	}
	end, err := parseTime(r.URL.Query().Get("end"), time.Time{})
	if err != nil || end.IsZero() {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("missing or invalid 'end' parameter"))
		return
	}
	step, err := parseDuration(r.URL.Query().Get("step"))
	if err != nil || step <= 0 {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("missing or invalid 'step' parameter"))
		return
	}
	if end.Before(start) {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("'end' must be after 'start'"))
		return
	}

	samples, err := h.executeInstant(r.Context(), q)
	if err != nil {
		h.respondError(w, err)
		return
	}

	result := toMatrixStepGrid(samples, start, end, step)
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

	samples, err := timeCH(ctx, func() ([]chclient.Sample, error) {
		return h.Client.Query(ctx, sql, args...)
	})
	if err != nil {
		return nil, &apiError{kind: ErrInternal, err: err, status: http.StatusBadGateway}
	}
	return samples, nil
}

// wrapWithSampleProjection adds a Project on top of plan that emits
// the canonical chclient.Sample shape — (MetricName, Attributes,
// TimeUnix, Value) — adapted to whatever the inner plan's output
// schema actually exposes. Two distinct shapes are recognised:
//
//  1. Scan / Filter(Scan) — the otel_metrics_* columns are available
//     directly; project MetricName / Attributes / TimeUnix / Value.
//  2. RangeWindow / Aggregate / Filter(Aggregate) — derived shapes
//     whose inner SELECT exposes only (group-keys…, value). The
//     canonical MetricName and TimeUnix don't exist in that scope;
//     synthesise them as empty string + now64() respectively. The
//     value column comes from the RangeWindow / Aggregate output
//     (always lowercase `value` by chsql convention).
func wrapWithSampleProjection(plan chplan.Node, s schema.Metrics) chplan.Node {
	projections := []chplan.Projection{
		{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}},
		{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}},
		{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}},
		{Expr: &chplan.ColumnRef{Name: s.ValueColumn}},
	}
	if isDerivedShape(plan) {
		// TimeUnix source: matrix-shape RangeWindow exposes a real
		// per-row `anchor_ts` (one row per anchor across the subquery's
		// outer range); the instant case has to synthesise via now64().
		var tsExpr chplan.Expr
		if isMatrixRangeWindow(plan) {
			tsExpr = &chplan.ColumnRef{Name: "anchor_ts"}
		} else {
			tsExpr = synthesizedAnchor()
		}
		projections = []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: tsExpr, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: "value"}, Alias: s.ValueColumn},
		}
	}
	return &chplan.Project{Input: plan, Projections: projections}
}

// isMatrixRangeWindow reports whether the plan root is a matrix-shape
// RangeWindow — i.e., one that emits N rows per series (one per anchor
// across [End-OuterRange, End] spaced by Step) and exposes `anchor_ts`
// as a per-row column. Set by PromQL subquery lowering (P0 4.5+).
func isMatrixRangeWindow(plan chplan.Node) bool {
	rw, ok := plan.(*chplan.RangeWindow)
	return ok && rw.OuterRange > 0
}

// synthesizedAnchor returns the CH expression cerberus stamps on
// rate / count_over_time / … sample rows for query_range bucketing.
// Equivalent to `now64(9) - toIntervalNanosecond(5e9)` — 5 seconds
// before CH-now. See the docstring on wrapWithSampleProjection's
// derived-shape branch for the rationale.
func synthesizedAnchor() chplan.Expr {
	return &chplan.Binary{
		Op:   chplan.OpSub,
		Left: &chplan.FuncCall{Name: "now64", Args: []chplan.Expr{&chplan.LitInt{V: 9}}},
		Right: &chplan.FuncCall{
			Name: "toIntervalNanosecond",
			Args: []chplan.Expr{&chplan.LitInt{V: 5_000_000_000}},
		},
	}
}

// isDerivedShape reports whether the plan's output schema lacks the
// canonical Sample columns (MetricName / TimeUnix / Value as-is) and
// has only the (group-keys…, value) shape produced by RangeWindow,
// Aggregate, or a Filter on top of one of those.
func isDerivedShape(plan chplan.Node) bool {
	switch v := plan.(type) {
	case *chplan.RangeWindow, *chplan.Aggregate:
		return true
	case *chplan.Filter:
		return isDerivedShape(v.Input)
	case *chplan.Project:
		return false
	}
	return false
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

// toMatrixStepGrid pivots the sample stream into the Prom matrix shape.
// For each evaluation step T in [start, end] (inclusive, spaced by step),
// each series emits one Sample: the value of the latest input row whose
// timestamp <= T (the standard PromQL latest-sample-at-eval-time
// semantics). Series with no eligible samples for a given step are
// represented as a stale-marker gap (the step is simply skipped in the
// Values slice — Prometheus permits this).
//
// Lookback delta: a hard 5-minute lookback by default; older samples are
// considered stale. Future work threads the API's `lookback_delta`
// param through here.
func toMatrixStepGrid(samples []chclient.Sample, start, end time.Time, step time.Duration) []MatrixSample {
	const lookback = 5 * time.Minute

	type seriesState struct {
		labels map[string]string
		// rows holds the sorted (ascending) timestamps + values for this
		// series; we walk the row cursor forward as we advance through
		// the step grid.
		rows []chclient.Sample
	}

	bySeries := map[string]*seriesState{}
	for _, s := range samples {
		labels := withMetricName(s.Labels, s.MetricName)
		key := canonicalKey(labels)
		st, ok := bySeries[key]
		if !ok {
			st = &seriesState{labels: labels}
			bySeries[key] = st
		}
		st.rows = append(st.rows, s)
	}
	for _, st := range bySeries {
		// Inline insertion sort by Timestamp ascending — samples are
		// typically already nearly sorted from CH.
		for i := 1; i < len(st.rows); i++ {
			for j := i; j > 0 && st.rows[j-1].Timestamp.After(st.rows[j].Timestamp); j-- {
				st.rows[j-1], st.rows[j] = st.rows[j], st.rows[j-1]
			}
		}
	}

	out := make([]MatrixSample, 0, len(bySeries))
	for _, st := range bySeries {
		ms := MatrixSample{Metric: st.labels}
		cursor := 0
		for t := start; !t.After(end); t = t.Add(step) {
			// Advance cursor as long as the next row's ts <= t.
			for cursor < len(st.rows) && !st.rows[cursor].Timestamp.After(t) {
				cursor++
			}
			if cursor == 0 {
				continue // no row at-or-before this step yet
			}
			latest := st.rows[cursor-1]
			if t.Sub(latest.Timestamp) > lookback {
				continue // older than the lookback window; stale
			}
			stamp := float64(t.UnixMilli()) / 1e3
			ms.Values = append(ms.Values, Sample{stamp, strconv.FormatFloat(latest.Value, 'f', -1, 64)})
		}
		if len(ms.Values) > 0 {
			out = append(out, ms)
		}
	}
	return out
}

// parseDuration parses a Prom-style step / range duration. Accepts plain
// floats (seconds) or Go-style durations like `30s`, `5m`, `1h`.
func parseDuration(raw string) (time.Duration, error) {
	if raw == "" {
		return 0, errors.New("missing duration")
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		return time.Duration(f * float64(time.Second)), nil
	}
	return time.ParseDuration(raw)
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
