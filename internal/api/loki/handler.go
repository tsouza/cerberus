package loki

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/grafana/loki/v3/pkg/logql/syntax"

	"github.com/tsouza/cerberus/internal/api/admit"
	"github.com/tsouza/cerberus/internal/api/format"
	"github.com/tsouza/cerberus/internal/api/httperr"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/telemetry"
)

// Querier is the subset of *chclient.Client that the Handler needs for
// the non-engine endpoints (labels / series / index-stats / volume /
// detected-fields / tail). The /query and /query_range data-plane
// endpoints now run through engine.Engine, which carries its own
// narrower Querier — Handler still owns this broader interface so
// the metadata endpoints can stub it in tests.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) ([]chclient.Sample, error)
	QueryStrings(ctx context.Context, sql string, args ...any) ([]string, error)
	QueryIndexStats(ctx context.Context, sql string, args ...any) (chclient.IndexStatsRow, error)
	QueryIndexVolume(ctx context.Context, sql string, args ...any) ([]chclient.IndexVolumeRow, error)
	QueryLabelSets(ctx context.Context, sql string, args ...any) ([]map[string]string, error)
}

// Handler implements the Loki HTTP API endpoints cerberus speaks. Mount
// it via Handler.Mount(mux). The current vertical slice covers
// /loki/api/v1/query, /loki/api/v1/query_range, /loki/api/v1/index/stats
// + /index/volume (RC2 P0.3), and — as of this PR — the remaining RC2
// metadata endpoints /labels, /label/<name>/values, /series,
// /detected_fields, /patterns (the last stubbed pending its own
// pattern-discovery workstream).
type Handler struct {
	Client    Querier
	Schema    schema.Logs
	Optimizer *optimizer.Driver
	Logger    *slog.Logger

	// Engine drives the /query and /query_range data-plane endpoints
	// (parse → lower → wrap-projection → optimize → emit → execute).
	// Constructed lazily in New so callers don't need to wire it
	// explicitly; the engine.Engine instance shares this handler's
	// Optimizer + Client.
	Engine *engine.Engine

	// Lang is a long-lived template adapter — its Schema is the
	// canonical handle the metadata endpoints share. The /query and
	// /query_range handlers DO NOT reuse this instance: they construct
	// a fresh *logql.Lang per request via langForRequest so the
	// request's [start, end] window threads into the lowering as a
	// `Timestamp BETWEEN ?` predicate. Held as a pointer so the
	// metadata path can pivot on the same schema without re-allocating.
	Lang *logql.Lang

	// Limiter caps in-flight Loki API requests. nil disables the
	// admission middleware. Wired from CERBERUS_ADMIT_LOKI.
	Limiter *admit.Limiter
}

// New constructs a Handler with the seed optimizer wired in.
func New(client Querier, s schema.Logs, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	opt := optimizer.Default()
	return &Handler{
		Client:    client,
		Schema:    s,
		Optimizer: opt,
		Logger:    logger,
		Engine:    &engine.Engine{Optimizer: opt, Client: client},
		Lang:      &logql.Lang{Schema: s},
	}
}

// Mount registers the Loki-compatible endpoints under /loki/api/v1/ on
// mux. Query + range + index/stats + index/volume cover the data-plane;
// the metadata endpoints (/labels, /label/{name}/values, /series,
// /detected_fields, /patterns) cover what Grafana's logs UI queries to
// populate label autocomplete, the streams chooser, and the patterns
// panel.
func (h *Handler) Mount(mux *http.ServeMux) {
	// Route every endpoint through the cerberus.queries.* counter +
	// duration middleware. WebSocket /tail is included — a
	// long-lived tail counts as one query for the purposes of total
	// volume bookkeeping; its duration will skew toward the long tail
	// of the histogram, which is what dashboards want to see anyway.
	register := func(pattern string, hf http.HandlerFunc) {
		// admit.Middleware (outer) → telemetry.QueryMiddleware (inner)
		// — rejections are accounted on cerberus.admit.rejected_total,
		// not on cerberus.queries.*. See prom.Handler.Mount for the
		// full layering note.
		mux.Handle(pattern, h.Limiter.Middleware(1, telemetry.QueryMiddleware("logql", hf)))
	}
	register("GET /loki/api/v1/query", h.handleQuery)
	register("POST /loki/api/v1/query", h.handleQuery)
	register("GET /loki/api/v1/query_range", h.handleQueryRange)
	register("POST /loki/api/v1/query_range", h.handleQueryRange)
	register("GET /loki/api/v1/index/stats", h.handleIndexStats)
	register("POST /loki/api/v1/index/stats", h.handleIndexStats)
	register("GET /loki/api/v1/index/volume", h.handleIndexVolume)
	register("POST /loki/api/v1/index/volume", h.handleIndexVolume)
	register("GET /loki/api/v1/labels", h.handleLabels)
	register("POST /loki/api/v1/labels", h.handleLabels)
	register("GET /loki/api/v1/label/{name}/values", h.handleLabelValues)
	register("POST /loki/api/v1/label/{name}/values", h.handleLabelValues)
	register("GET /loki/api/v1/series", h.handleSeries)
	register("POST /loki/api/v1/series", h.handleSeries)
	register("GET /loki/api/v1/detected_fields", h.handleDetectedFields)
	register("POST /loki/api/v1/detected_fields", h.handleDetectedFields)
	register("GET /loki/api/v1/patterns", h.handlePatterns)
	register("POST /loki/api/v1/patterns", h.handlePatterns)
	// /tail is WebSocket-upgrade only; no POST counterpart in upstream Loki.
	register("GET /loki/api/v1/tail", h.handleTail)
}

func (h *Handler) handleQuery(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("query")
	if q == "" {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("missing query parameter"))
		return
	}
	ts, err := format.ParseTimeLoki(r.URL.Query().Get("time"), time.Now())
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}

	// Instant /query: collapse the window onto a single point. Per
	// upstream Loki contract the evaluation lookback is the previous
	// 5 minutes (the same instant-lookback PromQL uses). Threading
	// [ts - 5m, ts] keeps the Scan filtered to that envelope so the
	// SQL doesn't return every matching log in the table.
	const instantLookback = 5 * time.Minute
	res, err := h.Engine.Query(r.Context(), h.langForRequest(ts.Add(-instantLookback), ts), q)
	if err != nil {
		h.respondError(w, classifyEngineErr(err))
		return
	}
	expr, _ := res.Meta.Extra["expr"].(syntax.Expr)
	h.Logger.Debug("cerberus loki query", "logql", q, "sql", res.SQL, "args", res.Args)

	data, err := buildInstantData(expr, res.Samples, ts, h.Schema)
	if err != nil {
		h.respondError(w, err)
		return
	}

	writeEngineHeaders(w, res.Headers)
	writeJSON(w, http.StatusOK, Response{
		Status: "success",
		Data:   data,
	})
}

func (h *Handler) handleQueryRange(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("query")
	if q == "" {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("missing query parameter"))
		return
	}
	start, err := format.ParseTimeLoki(r.URL.Query().Get("start"), time.Time{})
	if err != nil || start.IsZero() {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("missing or invalid 'start' parameter"))
		return
	}
	end, err := format.ParseTimeLoki(r.URL.Query().Get("end"), time.Time{})
	if err != nil || end.IsZero() {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("missing or invalid 'end' parameter"))
		return
	}
	step, err := format.ParseDuration(r.URL.Query().Get("step"))
	if err != nil {
		// Loki allows missing step (auto-resolves); cerberus requires it for
		// metric queries. Default to 1 minute when absent.
		step = time.Minute
	}
	if !end.After(start) {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("'end' must be after 'start'"))
		return
	}

	res, err := h.Engine.Query(r.Context(), h.langForRequest(start, end), q)
	if err != nil {
		h.respondError(w, classifyEngineErr(err))
		return
	}
	expr, _ := res.Meta.Extra["expr"].(syntax.Expr)
	h.Logger.Debug("cerberus loki query_range", "logql", q, "sql", res.SQL, "args", res.Args)

	data, err := buildRangeData(expr, res.Samples, start, end, step, h.Schema)
	if err != nil {
		h.respondError(w, err)
		return
	}

	writeEngineHeaders(w, res.Headers)
	writeJSON(w, http.StatusOK, Response{
		Status: "success",
		Data:   data,
	})
}

// langForRequest builds a per-request *logql.Lang carrying the request's
// [start, end] window. The engine threads Start / End down through
// logql.LowerAt so every Scan(LogsTable) gains a
// `Timestamp BETWEEN start AND end` predicate at the SQL layer — the
// fix for the wire-format contract violation where /query and
// /query_range used to return every matching log row regardless of the
// requested window.
func (h *Handler) langForRequest(start, end time.Time) *logql.Lang {
	return &logql.Lang{Schema: h.Schema, Start: start, End: end}
}

// writeEngineHeaders stamps the X-Cerberus-* response headers populated
// by engine.Engine.Query / QueryPlan onto w before the response body
// fires. Safe to call with a nil / empty map (no-op).
//
// Each handler calls this once per successful query — the engine
// populates the canonical bag (Strategy / Plan-Nodes / CH-Millis) so
// adding a new engine-level header (e.g. SQL-Length) requires no
// per-handler change.
func writeEngineHeaders(w http.ResponseWriter, hdr map[string]string) {
	for k, v := range hdr {
		w.Header().Set(k, v)
	}
}

// classifyEngineErr maps the error chains engine.Engine returns onto
// the Loki HTTP error vocabulary. Parse-stage errors arrive already
// wrapped in *apiError by [logql.Lang.Parse] (400 bad_data for parser
// failures, 422 execution for lower failures), so errors.As pulls them
// out unchanged. Engine-wrapped emit / execute errors are bare wrapped
// strings — we sniff the stage prefix to map emit → 500 and execute →
// 502 with the right Loki errorType.
func classifyEngineErr(err error) error {
	if err == nil {
		return nil
	}
	// Circuit-breaker fast-fail short-circuit: when the chclient
	// breaker is OPEN, surface 503 + Retry-After: 5 directly. See
	// internal/api/prom for the rationale.
	if errors.Is(err, chclient.ErrCircuitOpen) {
		return &apiError{
			Kind:              ErrUnavailable,
			Err:               err,
			Status:            http.StatusServiceUnavailable,
			RetryAfterSeconds: 5,
		}
	}
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		return apiErr
	}
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "engine: execute:"):
		return &apiError{Kind: ErrInternal, Err: err, Status: http.StatusBadGateway}
	case strings.HasPrefix(msg, "engine: emit:"):
		return &apiError{Kind: ErrInternal, Err: err, Status: http.StatusInternalServerError}
	default:
		return &apiError{Kind: ErrInternal, Err: err, Status: http.StatusInternalServerError}
	}
}

// buildInstantData turns the sample stream into a Loki instant-query
// data body. Metric queries produce a vector; log queries produce
// streams.
func buildInstantData(expr syntax.Expr, samples []chclient.Sample, ts time.Time, _ schema.Logs) (*QueryData, error) {
	if logql.IsMetricQuery(expr) {
		return &QueryData{
			ResultType: "vector",
			Result:     toVector(samples, ts),
		}, nil
	}
	tx, err := postProcessExtract(expr)
	if err != nil {
		return nil, &apiError{Kind: ErrBadData, Err: err, Status: http.StatusBadRequest}
	}
	return &QueryData{
		ResultType: "streams",
		Result:     toStreamsWithTransform(samples, tx),
	}, nil
}

// buildRangeData turns the sample stream into a Loki range-query data
// body. Metric queries produce a matrix (per-step latest value per
// series). Log queries produce streams.
func buildRangeData(expr syntax.Expr, samples []chclient.Sample, start, end time.Time, step time.Duration, _ schema.Logs) (*QueryData, error) {
	if logql.IsMetricQuery(expr) {
		return &QueryData{
			ResultType: "matrix",
			Result:     toMatrixStepGrid(samples, start, end, step),
		}, nil
	}
	tx, err := postProcessExtract(expr)
	if err != nil {
		return nil, &apiError{Kind: ErrBadData, Err: err, Status: http.StatusBadRequest}
	}
	return &QueryData{
		ResultType: "streams",
		Result:     toStreamsWithTransform(samples, tx),
	}, nil
}

// toVector groups samples by label set, picks the latest per series.
func toVector(samples []chclient.Sample, ts time.Time) []VectorSample {
	type latest struct {
		labels map[string]string
		ts     time.Time
		value  float64
	}
	bySeries := map[string]latest{}
	for _, s := range samples {
		key := format.CanonicalKey(s.Labels)
		cur, ok := bySeries[key]
		if !ok || s.Timestamp.After(cur.ts) {
			bySeries[key] = latest{labels: s.Labels, ts: s.Timestamp, value: s.Value}
		}
	}
	out := make([]VectorSample, 0, len(bySeries))
	stamp := float64(ts.UnixMilli()) / 1e3
	for _, l := range bySeries {
		out = append(out, VectorSample{
			Metric: l.labels,
			Value:  [2]any{stamp, strconv.FormatFloat(l.value, 'f', -1, 64)},
		})
	}
	return out
}

// toMatrixStepGrid mirrors api/prom's per-step bucketing. Walks
// [start, end] at `step`, each series emits one Sample per step =
// the latest at-or-before that step (5-min lookback).
func toMatrixStepGrid(samples []chclient.Sample, start, end time.Time, step time.Duration) []MatrixSample {
	const lookback = 5 * time.Minute

	type seriesState struct {
		labels map[string]string
		rows   []chclient.Sample
	}
	bySeries := map[string]*seriesState{}
	for _, s := range samples {
		key := format.CanonicalKey(s.Labels)
		st, ok := bySeries[key]
		if !ok {
			st = &seriesState{labels: s.Labels}
			bySeries[key] = st
		}
		st.rows = append(st.rows, s)
	}
	for _, st := range bySeries {
		sort.Slice(st.rows, func(i, j int) bool { return st.rows[i].Timestamp.Before(st.rows[j].Timestamp) })
	}

	out := make([]MatrixSample, 0, len(bySeries))
	for _, st := range bySeries {
		ms := MatrixSample{Metric: st.labels}
		cursor := 0
		for t := start; !t.After(end); t = t.Add(step) {
			for cursor < len(st.rows) && !st.rows[cursor].Timestamp.After(t) {
				cursor++
			}
			if cursor == 0 {
				continue
			}
			latest := st.rows[cursor-1]
			if t.Sub(latest.Timestamp) > lookback {
				continue
			}
			stamp := float64(t.UnixMilli()) / 1e3
			ms.Values = append(ms.Values, [2]any{stamp, strconv.FormatFloat(latest.Value, 'f', -1, 64)})
		}
		if len(ms.Values) > 0 {
			out = append(out, ms)
		}
	}
	return out
}

// toStreamsWithTransform pivots samples into Loki's "streams" result shape
// and optionally runs a per-row transform (line_format / decolorize /
// label_format) before grouping. Each distinct *output* label set
// becomes one Stream; values are sorted by ts ascending. Nil tx is
// the identity transform.
//
// When the transform mutates labels (e.g., `| label_format`), the
// grouping reflects the post-format label set — two rows that differ
// only on a dropped label collapse into a single stream. Conversely,
// two rows that share the original labels but diverge after a
// template-set stay in distinct streams.
//
// Note: the synthesized projection writes the log Body string into
// chclient.Sample.MetricName (since Sample.Value is float64). This is a
// short-term hack — the proper fix is a new chclient row decoder for
// log-stream output, which lands with the stream-aware decoder PR.
func toStreamsWithTransform(samples []chclient.Sample, tx lineTransform) []Stream {
	type acc struct {
		labels map[string]string
		values [][2]string
	}
	bySeries := map[string]*acc{}
	for _, s := range samples {
		line := s.MetricName
		labels := s.Labels
		if tx != nil {
			line, labels = tx(line, labels)
		}
		key := format.CanonicalKey(labels)
		a, ok := bySeries[key]
		if !ok {
			a = &acc{labels: labels}
			bySeries[key] = a
		}
		a.values = append(a.values, [2]string{
			strconv.FormatInt(s.Timestamp.UnixNano(), 10),
			line,
		})
	}
	out := make([]Stream, 0, len(bySeries))
	for _, a := range bySeries {
		sort.Slice(a.values, func(i, j int) bool { return a.values[i][0] < a.values[j][0] })
		out = append(out, Stream{Stream: a.labels, Values: a.values})
	}
	return out
}

// apiError is a package-local alias for the shared [httperr.Error]
// carrier so the existing in-package callsites can stay literal.
type apiError = httperr.Error

func (h *Handler) respondError(w http.ResponseWriter, err error) {
	// Circuit-breaker fast-fail short-circuit applies regardless of
	// whether the callsite pre-wrapped the chclient error in its own
	// *apiError — many Loki sub-handlers do (`&apiError{Status: 502,
	// Err: chclient.QueryX...Err}`), and the inner ErrCircuitOpen
	// would otherwise be masked by the outer wrap. We sniff via
	// errors.Is so both shapes (bare and wrapped) get the 503 +
	// Retry-After treatment.
	if errors.Is(err, chclient.ErrCircuitOpen) {
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, ErrUnavailable, err)
		return
	}
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		if apiErr.RetryAfterSeconds > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(apiErr.RetryAfterSeconds))
		}
		writeError(w, apiErr.Status, apiErr.Kind, apiErr.Err)
		return
	}
	writeError(w, http.StatusInternalServerError, ErrInternal, err)
}

// writeJSON wraps [httperr.WriteJSON] so package-local callsites stay
// unqualified. The shared helper handles Content-Type + status + body
// encoding identically across all three handlers.
func writeJSON(w http.ResponseWriter, status int, body any) {
	httperr.WriteJSON(w, status, body)
}

// writeError emits the Loki JSON envelope `{status, errorType, error}`.
// Loki and Prom share the same wire format here; the envelope is still
// shaped per-handler so each package owns its own `Response` type.
func writeError(w http.ResponseWriter, status int, kind string, err error) {
	httperr.WriteJSON(w, status, Response{
		Status:    "error",
		ErrorType: kind,
		Error:     err.Error(),
	})
}
