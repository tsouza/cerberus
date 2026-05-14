package loki

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/grafana/loki/v3/pkg/logql/syntax"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/tsouza/cerberus/internal/api/admit"
	"github.com/tsouza/cerberus/internal/api/format"
	"github.com/tsouza/cerberus/internal/cerbtrace"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/telemetry"
)

// tracer emits the `parse` pipeline-stage span before the LogQL parser
// runs. The subsequent lower / optimize / emit / execute stages carry
// their own tracers from their owning packages.
var tracer = otel.Tracer("github.com/tsouza/cerberus/internal/api/loki")

// parseExpr wraps syntax.ParseExpr in a `parse` pipeline-stage span.
// The QL identifier and the (truncated) query string land on the span
// as `cerberus.ql` + `cerberus.query`.
func parseExpr(ctx context.Context, query string) (syntax.Expr, error) {
	_, span := tracer.Start(ctx, cerbtrace.SpanParse,
		trace.WithAttributes(cerbtrace.ParseAttrs("logql", query)...))
	defer span.End()
	expr, err := syntax.ParseExpr(query)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	return expr, nil
}

// Querier is the subset of *chclient.Client that the Handler needs.
// Mirrors the api/prom Querier interface for the same stub-in-tests
// reasons.
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

	// Limiter caps in-flight Loki API requests. nil disables the
	// admission middleware. Wired from CERBERUS_ADMIT_LOKI.
	Limiter *admit.Limiter
}

// New constructs a Handler with the seed optimizer wired in.
func New(client Querier, s schema.Logs, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		Client:    client,
		Schema:    s,
		Optimizer: optimizer.Default(),
		Logger:    logger,
	}
}

// Mount registers the Loki-compatible endpoints under /loki/api/v1/ on
// mux. Query + range + index/stats + index/volume cover the data-plane;
// the metadata endpoints (/labels, /label/{name}/values, /series,
// /detected_fields, /patterns) cover what Grafana's logs UI queries to
// populate label autocomplete, the streams chooser, and the patterns
// panel.
func (h *Handler) Mount(mux *http.ServeMux) {
	// RC4 R4.4: route every endpoint through the cerberus.queries.*
	// counter + duration middleware. WebSocket /tail is included — a
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

	parseT := telemetry.ObserveStage(telemetry.StageParse)
	expr, err := parseExpr(r.Context(), q)
	parseT.Done(r.Context())
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}

	samples, err := h.execute(r.Context(), expr)
	if err != nil {
		h.respondError(w, err)
		return
	}

	data, err := buildInstantData(expr, samples, ts, h.Schema)
	if err != nil {
		h.respondError(w, err)
		return
	}

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

	parseT := telemetry.ObserveStage(telemetry.StageParse)
	expr, err := parseExpr(r.Context(), q)
	parseT.Done(r.Context())
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}

	samples, err := h.execute(r.Context(), expr)
	if err != nil {
		h.respondError(w, err)
		return
	}

	data, err := buildRangeData(expr, samples, start, end, step, h.Schema)
	if err != nil {
		h.respondError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, Response{
		Status: "success",
		Data:   data,
	})
}

// execute lowers a parsed LogQL expression, wraps with a sample-shape
// projection, optimizes, emits SQL, and runs the query. Each stage is
// timed onto the cerberus.pipeline.stage.duration.seconds histogram
// (RC4 R4.4); parse already happened in the caller.
func (h *Handler) execute(ctx context.Context, expr syntax.Expr) ([]chclient.Sample, error) {
	lowerT := telemetry.ObserveStage(telemetry.StageLower)
	plan, err := logql.Lower(ctx, expr, h.Schema)
	lowerT.Done(ctx)
	if err != nil {
		return nil, &apiError{kind: ErrExecution, err: err, status: http.StatusUnprocessableEntity}
	}

	plan = wrapWithLogSampleProjection(plan, h.Schema, expr)
	optT := telemetry.ObserveStage(telemetry.StageOptimize)
	plan = h.Optimizer.Run(ctx, plan)
	optT.Done(ctx)

	emitT := telemetry.ObserveStage(telemetry.StageEmit)
	sqlStr, args, err := chsql.Emit(ctx, plan)
	emitT.Done(ctx)
	if err != nil {
		return nil, &apiError{kind: ErrInternal, err: err, status: http.StatusInternalServerError}
	}
	h.Logger.Debug("cerberus loki query", "logql", expr.String(), "sql", sqlStr, "args", args)

	execT := telemetry.ObserveStage(telemetry.StageExecute)
	samples, err := h.Client.Query(chclient.WithProgressFor(ctx, "logql"), sqlStr, args...)
	execT.Done(ctx)
	if err != nil {
		h.Logger.Error("cerberus loki CH query failed", "err", err, "sql", sqlStr)
		return nil, &apiError{kind: ErrInternal, err: err, status: http.StatusBadGateway}
	}
	return samples, nil
}

// wrapWithLogSampleProjection adds a Project on top of plan so the
// chclient.Sample scanner can decode rows positionally. For metric
// queries (rate, count_over_time, sum(...)) the lowered plan already
// produces (MetricName, Attributes, TimeUnix, Value) — the projection
// is an explicit pass-through. For raw log-stream queries
// ({selector} ...) the projection synthesises an empty MetricName, the
// Body column as a synthetic stringified Value (decoded as a string by
// the streams formatter), and the per-record Timestamp.
func wrapWithLogSampleProjection(plan chplan.Node, s schema.Logs, expr syntax.Expr) chplan.Node {
	if isMetricQuery(expr) {
		// Metric queries lower to RangeWindow / Aggregate / Filter(Aggregate),
		// whose output is just (group-keys…, value). MetricName + TimeUnix
		// don't exist in that scope — synthesise them so the chclient
		// Sample scanner has the four positional columns it expects.
		return &chplan.Project{
			Input: plan,
			Projections: []chplan.Projection{
				{Expr: &chplan.LitString{V: ""}, Alias: "MetricName"},
				{Expr: &chplan.ColumnRef{Name: s.ResourceAttributesColumn}, Alias: "Attributes"},
				// now64(9) - 5s buffer; see prom handler's synthesizedAnchor
				// docstring. Avoids toMatrixStepGrid dropping the only row
				// when CH-now > client-end.
				{Expr: &chplan.Binary{
					Op:    chplan.OpSub,
					Left:  &chplan.FuncCall{Name: "now64", Args: []chplan.Expr{&chplan.LitInt{V: 9}}},
					Right: &chplan.FuncCall{Name: "toIntervalNanosecond", Args: []chplan.Expr{&chplan.LitInt{V: 5_000_000_000}}},
				}, Alias: "TimeUnix"},
				{Expr: &chplan.ColumnRef{Name: "value"}, Alias: "Value"},
			},
		}
	}
	// Log-stream query: chclient.Sample is (MetricName, Attributes, Timestamp,
	// Value) where Value is float64. The log line `Body` is a String, so it
	// can't ride in Value — instead we put it in MetricName (also a String)
	// and write a 0.0 placeholder into Value. toStreamsWithTransform reads
	// back from Sample.MetricName as the line content.
	return &chplan.Project{
		Input: plan,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.BodyColumn}, Alias: "MetricName"},
			{Expr: &chplan.ColumnRef{Name: s.ResourceAttributesColumn}, Alias: "Attributes"},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: "TimeUnix"},
			// Wrap the placeholder zero in toFloat64 so CH returns the column
			// as Float64; without the cast a bare `0` literal becomes UInt8
			// and clickhouse-go's Scan rejects UInt8 → *float64.
			{Expr: &chplan.FuncCall{Name: "toFloat64", Args: []chplan.Expr{&chplan.LitFloat{V: 0}}}, Alias: "Value"},
		},
	}
}

// isMetricQuery reports whether the parsed LogQL expression produces a
// numeric series (rate / count_over_time / aggregations) versus a raw
// log-line stream.
func isMetricQuery(expr syntax.Expr) bool {
	switch expr.(type) {
	case *syntax.RangeAggregationExpr, *syntax.VectorAggregationExpr,
		*syntax.LiteralExpr, *syntax.BinOpExpr, *syntax.LabelReplaceExpr:
		return true
	}
	return false
}

// buildInstantData turns the sample stream into a Loki instant-query
// data body. Metric queries produce a vector; log queries produce
// streams.
func buildInstantData(expr syntax.Expr, samples []chclient.Sample, ts time.Time, _ schema.Logs) (*QueryData, error) {
	if isMetricQuery(expr) {
		return &QueryData{
			ResultType: "vector",
			Result:     toVector(samples, ts),
		}, nil
	}
	tx, err := postProcessExtract(expr)
	if err != nil {
		return nil, &apiError{kind: ErrBadData, err: err, status: http.StatusBadRequest}
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
	if isMetricQuery(expr) {
		return &QueryData{
			ResultType: "matrix",
			Result:     toMatrixStepGrid(samples, start, end, step),
		}, nil
	}
	tx, err := postProcessExtract(expr)
	if err != nil {
		return nil, &apiError{kind: ErrBadData, err: err, status: http.StatusBadRequest}
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

// apiError carries the Loki errorType + an HTTP status code through the
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
