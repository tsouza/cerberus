package prom

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/tsouza/cerberus/internal/api/admit"
	"github.com/tsouza/cerberus/internal/api/format"
	"github.com/tsouza/cerberus/internal/api/httperr"
	"github.com/tsouza/cerberus/internal/cerbtrace"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/telemetry"
)

// tracer emits the `parse` pipeline-stage span before the PromQL parser
// runs. The subsequent lower / optimize / emit / execute stages carry
// their own tracers from their owning packages.
var tracer = otel.Tracer("github.com/tsouza/cerberus/internal/api/prom")

// Querier is the subset of *chclient.Client that Handler needs. Stubbing
// it makes unit tests possible without a live ClickHouse.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) ([]chclient.Sample, error)
	QueryCursor(ctx context.Context, sql string, args ...any) (chclient.Cursor, error)
	QueryStrings(ctx context.Context, sql string, args ...any) ([]string, error)
	QueryLabelSets(ctx context.Context, sql string, args ...any) ([]map[string]string, error)
	QueryMetricMeta(ctx context.Context, sql, metricType string, args ...any) ([]chclient.MetricMetaRow, error)
}

// Handler implements the Prometheus HTTP API endpoints cerberus speaks.
// Mount it via Handler.Mount(mux).
type Handler struct {
	Client Querier
	Schema schema.Metrics
	// Engine runs the shared parse → lower → optimize → emit → execute
	// pipeline. Wired by New from the same Client + Optimizer the
	// handler holds; the indirection keeps the per-request pipeline
	// orchestration in one place across the three API heads.
	Engine    *engine.Engine
	Optimizer *optimizer.Driver
	Logger    *slog.Logger

	// Limiter caps in-flight Prom API requests. nil disables the
	// admission middleware (every request flows through). main wires
	// this from CERBERUS_ADMIT_PROM; tests leave it nil for
	// unconstrained behaviour.
	Limiter *admit.Limiter

	parser promparser.Parser
}

// New constructs a Handler with the seed optimizer wired in plus a
// matching engine.Engine. The engine + handler share the same Client
// and Optimizer; the engine owns the pipeline loop, the handler owns
// HTTP routing + the per-API wire-format pivot.
func New(client Querier, s schema.Metrics, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	opt := optimizer.Default()
	return &Handler{
		Client:    client,
		Schema:    s,
		Engine:    &engine.Engine{Optimizer: opt, Client: client},
		Optimizer: opt,
		Logger:    logger,
		parser:    promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true}),
	}
}

// Mount registers the Prom-compatible endpoints under /api/v1/ on mux.
// Each route is wrapped with promHeadersMiddleware so responses carry
// `X-Prometheus-API-Version` and `X-Cerberus-CH-Millis`.
func (h *Handler) Mount(mux *http.ServeMux) {
	register := func(pattern string, hf http.HandlerFunc) {
		// Layering, outermost → innermost:
		//   admit.Middleware       — reject early when at the cap so
		//                            the slot is freed before any
		//                            request-shaped work runs.
		//   promHeadersMiddleware  — sets Prom response headers + the
		//                            CH-millis counter on r.Context().
		//   telemetry.QueryMiddleware — counts every admitted request
		//                            on cerberus.queries.* with the
		//                            matched route label.
		//   hf                     — the actual handler.
		// Rejections are not counted by QueryMiddleware (the inner
		// handler never runs); they show up on
		// cerberus.admit.rejected_total instead.
		handler := promHeadersMiddleware(telemetry.QueryMiddleware("promql", hf))
		mux.Handle(pattern, h.Limiter.Middleware(1, handler))
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
	register("GET /api/v1/format_query", h.handleFormatQuery)
	register("POST /api/v1/format_query", h.handleFormatQuery)
	register("GET /api/v1/parse_query", h.handleParseQuery)
	register("POST /api/v1/parse_query", h.handleParseQuery)
	register("GET /api/v1/query_exemplars", h.handleQueryExemplars)
	register("POST /api/v1/query_exemplars", h.handleQueryExemplars)
}

// handleFormatQuery implements `/api/v1/format_query`. Takes a `query`
// param, parses it, and returns the pretty-printed string. Grafana's
// query editor uses this to format on save.
func (h *Handler) handleFormatQuery(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("query")
	if q == "" && r.Method == http.MethodPost {
		_ = r.ParseForm()
		q = r.PostForm.Get("query")
	}
	if q == "" {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("missing query parameter"))
		return
	}
	expr, err := h.parseExpr(r.Context(), q)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}
	writeJSON(w, http.StatusOK, Response{
		Status: "success",
		Data:   expr.String(),
	})
}

// handleParseQuery implements `/api/v1/parse_query`. Takes a `query`
// param, parses it, and returns the AST. Upstream Prometheus returns
// a rich nested-node tree; cerberus returns a minimal shape that
// signals "parsed OK" via the Type field — enough for Grafana's
// inline syntax check. Full nested-AST serialization is a follow-up.
func (h *Handler) handleParseQuery(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("query")
	if q == "" && r.Method == http.MethodPost {
		_ = r.ParseForm()
		q = r.PostForm.Get("query")
	}
	if q == "" {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("missing query parameter"))
		return
	}
	expr, err := h.parseExpr(r.Context(), q)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}
	writeJSON(w, http.StatusOK, Response{
		Status: "success",
		Data: map[string]any{
			"type": fmt.Sprintf("%T", expr),
			"node": expr.String(),
		},
	})
}

func (h *Handler) handleQuery(w http.ResponseWriter, r *http.Request) {
	// r.FormValue merges URL query params with POST form-encoded body
	// (auto-calling ParseForm). Upstream Prometheus accepts `query=...`
	// in either; Grafana + the prometheus/client_golang library POST
	// with application/x-www-form-urlencoded for instant queries.
	q := r.FormValue("query")
	if q == "" {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("missing query parameter"))
		return
	}
	ts, err := format.ParseTimeProm(r.FormValue("time"), time.Now())
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}

	// Scalar-only PromQL (`1+1`, `42`) — Grafana's datasource health
	// probe path. Evaluate in Go and return the canonical scalar
	// envelope; no ClickHouse round-trip.
	if value, ok, err := h.tryScalarFold(r.Context(), q); err != nil {
		h.respondError(w, err)
		return
	} else if ok {
		writeJSON(w, http.StatusOK, Response{
			Status: "success",
			Data:   &QueryData{ResultType: "scalar", Result: scalarPoint(ts, value)},
		})
		return
	}

	samples, hdr, err := h.executeInstant(r.Context(), q, ts, ts)
	if err != nil {
		h.respondError(w, err)
		return
	}

	result := toVector(samples, ts)
	writeEngineHeaders(w, hdr)
	writeJSON(w, http.StatusOK, Response{
		Status: "success",
		Data:   &QueryData{ResultType: "vector", Result: result},
	})
}

func (h *Handler) handleQueryRange(w http.ResponseWriter, r *http.Request) {
	// r.FormValue merges URL query params with POST form-encoded body
	// (auto-calling ParseForm). Upstream Prometheus accepts these in
	// either form, and the prometheus/client_golang library defaults
	// to POST application/x-www-form-urlencoded for range queries
	// (see DoGetFallback in client_golang/api/prometheus/v1/api.go).
	q := r.FormValue("query")
	if q == "" {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("missing query parameter"))
		return
	}
	start, err := format.ParseTimeProm(r.FormValue("start"), time.Time{})
	if err != nil || start.IsZero() {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("missing or invalid 'start' parameter"))
		return
	}
	end, err := format.ParseTimeProm(r.FormValue("end"), time.Time{})
	if err != nil || end.IsZero() {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("missing or invalid 'end' parameter"))
		return
	}
	step, err := format.ParseDuration(r.FormValue("step"))
	if err != nil || step <= 0 {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("missing or invalid 'step' parameter"))
		return
	}
	if end.Before(start) {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("'end' must be after 'start'"))
		return
	}

	// Scalar-only PromQL → matrix of one series at every step holding
	// the folded constant. Matches Prom's `1+1` over query_range
	// (single series, no labels, every step bucket populated).
	if value, ok, err := h.tryScalarFold(r.Context(), q); err != nil {
		h.respondError(w, err)
		return
	} else if ok {
		writeJSON(w, http.StatusOK, Response{
			Status: "success",
			Data:   &QueryData{ResultType: "matrix", Result: scalarMatrix(value, start, end, step)},
		})
		return
	}

	cursor, hdr, err := h.executeRangeStreaming(r.Context(), q, start, end)
	if err != nil {
		h.respondError(w, err)
		return
	}
	defer func() {
		_ = cursor.Close()
	}()

	result, err := matrixFromCursor(cursor, start, end, step)
	if err != nil {
		h.respondError(w, &apiError{Kind: ErrInternal, Err: err, Status: http.StatusBadGateway})
		return
	}
	writeEngineHeaders(w, hdr)
	writeJSON(w, http.StatusOK, Response{
		Status: "success",
		Data:   &QueryData{ResultType: "matrix", Result: result},
	})
}

// tryScalarFold parses the query and, if it's a scalar-only expression
// (NumberLiteral, ParenExpr around scalars, scalar arithmetic), returns
// the folded constant. The bool reports whether folding succeeded; the
// error covers parse failures (the same `bad_data` 400 envelope the
// normal path would return).
func (h *Handler) tryScalarFold(ctx context.Context, query string) (float64, bool, error) {
	expr, err := h.parseExpr(ctx, query)
	if err != nil {
		return 0, false, &apiError{Kind: ErrBadData, Err: err, Status: http.StatusBadRequest}
	}
	val, ok := promql.TryFoldScalar(expr)
	return val, ok, nil
}

// parseExpr wraps the prom parser in a `parse` pipeline-stage span. The
// QL identifier and the (truncated) query string land on the span as
// `cerberus.ql` + `cerberus.query`.
func (h *Handler) parseExpr(ctx context.Context, query string) (promparser.Expr, error) {
	_, span := tracer.Start(ctx, cerbtrace.SpanParse,
		trace.WithAttributes(cerbtrace.ParseAttrs("promql", query)...))
	defer span.End()
	expr, err := h.parser.ParseExpr(query)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	return expr, nil
}

// scalarPoint renders the [<unix_seconds_float>, "<value_string>"]
// pair Prometheus uses for both scalar and matrix sample wire shapes,
// matching the format toVector + the matrix pivot already use.
func scalarPoint(ts time.Time, v float64) Sample {
	return Sample{float64(ts.UnixMilli()) / 1e3, strconv.FormatFloat(v, 'f', -1, 64)}
}

// scalarMatrix builds the matrix shape for a scalar evaluated over a
// range: one series with no labels, the folded value repeated at every
// step bucket between start and end (inclusive).
func scalarMatrix(v float64, start, end time.Time, step time.Duration) []MatrixSample {
	if step <= 0 {
		return nil
	}
	values := make([]Sample, 0)
	for ts := start; !ts.After(end); ts = ts.Add(step) {
		values = append(values, scalarPoint(ts, v))
	}
	return []MatrixSample{{Metric: map[string]string{}, Values: values}}
}

// executeRangeStreaming is the streaming counterpart to executeInstant
// used by /api/v1/query_range. The pipeline body (parse → lower →
// project → optimize → emit → execute) runs through engine.QueryCursor;
// the handler retains responsibility for the chclient.Cursor →
// response-shape pivot. For a wide-range / fine-step query this is the
// difference between O(rows) and O(rows-per-series) resident memory.
func (h *Handler) executeRangeStreaming(
	ctx context.Context,
	query string,
	start, end time.Time,
) (chclient.Cursor, map[string]string, error) {
	l := &lang{Parser: h.parser, Schema: h.Schema, Start: start, End: end}
	// Time the entire QueryCursor entry so the cursor-open round-trip
	// is billed to X-Cerberus-CH-Millis the same way timeCH did pre-
	// port. The execute span the engine opens internally covers the
	// same wall-clock; this counter is the handler-side header
	// surface, separate from the OTel span.
	chStart := time.Now()
	res, err := h.Engine.QueryCursor(ctx, l, query)
	if c := ctxCounter(ctx); c != nil {
		c.add(time.Since(chStart))
	}
	if err != nil {
		return nil, nil, classifyEngineError(err)
	}
	h.Logger.Debug("cerberus query_range (stream)", "promql", query, "sql", res.SQL, "args", res.Args)
	return res.Cursor, res.Headers, nil
}

// executeInstant runs a PromQL query through engine.Engine and returns
// the row slice. start / end are the query's evaluation-range bookends,
// threaded into the Lang adapter so `@ start()` / `@ end()` modifiers
// resolve against them. For instant queries the caller passes
// start == end == ts.
func (h *Handler) executeInstant(ctx context.Context, query string, start, end time.Time) ([]chclient.Sample, map[string]string, error) {
	l := &lang{Parser: h.parser, Schema: h.Schema, Start: start, End: end}
	res, err := h.Engine.Query(ctx, l, query)
	if err != nil {
		return nil, nil, classifyEngineError(err)
	}
	h.Logger.Debug("cerberus query", "promql", query, "sql", res.SQL, "args", res.Args)
	// Engine times the execute stage uniformly; surface that to the
	// per-request chMillisCounter so the X-Cerberus-CH-Millis header
	// keeps reporting CH wall-clock. The middleware-driven counter is
	// retained alongside the engine's per-Result.Headers stamp so the
	// error-path response (no engine.Result) still gets a sensible
	// (zero) X-Cerberus-CH-Millis stamped by the middleware.
	if c := ctxCounter(ctx); c != nil {
		c.add(time.Duration(res.CHMillis) * time.Millisecond)
	}
	return res.Samples, res.Headers, nil
}

// writeEngineHeaders stamps the X-Cerberus-* response headers populated
// by engine.Engine.Query / QueryCursor onto w before the response body
// fires. Safe to call with a nil / empty map (no-op).
//
// The middleware-driven X-Cerberus-CH-Millis stamp still runs after the
// handler returns (see middleware.go); when the engine populated
// res.Headers[X-Cerberus-CH-Millis] we Set the same key here first and
// the middleware's later Set is a no-op overwrite with the equivalent
// value (engine CH timing is also written into the per-request counter
// in executeInstant). The middleware path stays as a safety net for
// error responses where the engine never produced a Result.
func writeEngineHeaders(w http.ResponseWriter, hdr map[string]string) {
	for k, v := range hdr {
		w.Header().Set(k, v)
	}
}

// classifyEngineError maps engine.Query / engine.QueryCursor errors to
// the per-stage apiError shape the Prom handler exposes via
// respondError. The engine wraps each stage's error with an
// "engine: <stage>:" prefix (parse / emit / execute); the Lang
// adapter further tags parse-vs-lower failures via parseStageError so
// the wire-level errorType / HTTP status mirror the pre-port
// classification.
func classifyEngineError(err error) error {
	if err == nil {
		return nil
	}
	// Circuit-breaker fast-fail short-circuit: when the chclient
	// breaker is OPEN, surface 503 + Retry-After: 5 directly without
	// dressing it as a 5xx "execute" failure. This is the wire
	// signal Grafana / Prom clients honour to back off rather than
	// hammer the gateway during an upstream CH outage.
	if errors.Is(err, chclient.ErrCircuitOpen) {
		return &apiError{
			Kind:              ErrUnavailable,
			Err:               err,
			Status:            http.StatusServiceUnavailable,
			RetryAfterSeconds: 5,
		}
	}
	var ps *parseStageError
	if errors.As(err, &ps) {
		switch ps.stage {
		case "parse":
			return &apiError{Kind: ErrBadData, Err: err, Status: http.StatusBadRequest}
		case "lower":
			return &apiError{Kind: ErrExecution, Err: err, Status: http.StatusUnprocessableEntity}
		}
	}
	msg := err.Error()
	switch {
	case errContainsStage(msg, "emit"):
		return &apiError{Kind: ErrInternal, Err: err, Status: http.StatusInternalServerError}
	case errContainsStage(msg, "execute"):
		return &apiError{Kind: ErrInternal, Err: err, Status: http.StatusBadGateway}
	}
	return &apiError{Kind: ErrInternal, Err: err, Status: http.StatusInternalServerError}
}

// errContainsStage reports whether msg starts with `engine: <stage>:`.
// Kept narrow (prefix match against the engine's wrapping format) so a
// downstream error message that happens to contain "execute" doesn't
// get mis-classified.
func errContainsStage(msg, stage string) bool {
	prefix := "engine: " + stage + ":"
	return len(msg) >= len(prefix) && msg[:len(prefix)] == prefix
}

// wrapWithSampleProjection adds a Project on top of plan that emits
// the canonical chclient.Sample shape — (MetricName, Attributes,
// TimeUnix, Value) — adapted to whatever the inner plan's output
// schema actually exposes. Two distinct shapes are recognised:
//
//  1. Scan / Filter(Scan) — the otel_metrics_* columns are available
//     directly; project MetricName / Attributes / TimeUnix / Value.
//  2. RangeWindow / Aggregate / Filter(Aggregate) — derived shapes
//     whose inner SELECT exposes only (group-keys…, s.ValueColumn).
//     The canonical MetricName doesn't exist in that scope; synthesise
//     it as empty string. The matrix RangeWindow projects anchor_ts AS
//     s.TimestampColumn on its outermost SELECT so the per-row timestamp
//     flows through under the canonical name; the instant case has to
//     synthesise via now64().
func wrapWithSampleProjection(plan chplan.Node, s schema.Metrics) chplan.Node {
	projections := []chplan.Projection{
		{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}},
		{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}},
		{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}},
		{Expr: &chplan.ColumnRef{Name: s.ValueColumn}},
	}
	if isDerivedShape(plan) {
		// TimeUnix source: matrix-shape RangeWindow exposes a real
		// per-row timestamp under s.TimestampColumn (one row per anchor
		// across the subquery's outer range); the instant case has to
		// synthesise via now64().
		var tsExpr chplan.Expr
		if isMatrixRangeWindow(plan) {
			tsExpr = &chplan.ColumnRef{Name: s.TimestampColumn}
		} else {
			tsExpr = synthesizedAnchor()
		}
		projections = []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: tsExpr, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
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
		labels := format.WithMetricName(s.Labels, s.MetricName)
		key := format.CanonicalKey(labels)
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

// matrixFromCursor is the streaming pivot from the cursor: it
// drains the cursor row-by-row instead of consuming a pre-materialised
// []chclient.Sample slice. Per-series buffers (timestamps + values) live
// in a map keyed by canonicalKey; once the cursor is fully drained we
// pivot each buffer onto the step grid using the same latest-at-step
// semantics PromQL uses (latest sample at-or-before step, within lookback delta).
//
// Memory complexity: O(rows) total in the per-series buffers — but with
// the master []chclient.Sample slice gone, peak resident bytes shrink
// roughly 2x (no parallel copy) and the gc churn from growing one big
// slice disappears. The eventual fully-streaming variant (one series at
// a time, flushed on canonicalKey boundary changes) requires the SQL
// emission to ORDER BY (series_key, ts) — that lands as a separate
// follow-up so this PR stays scoped to the API surface change.
func matrixFromCursor(
	cursor chclient.Cursor,
	start, end time.Time,
	step time.Duration,
) ([]MatrixSample, error) {
	type seriesState struct {
		labels map[string]string
		rows   []chclient.Sample
	}

	bySeries := map[string]*seriesState{}
	for cursor.Next() {
		s := cursor.Sample()
		labels := format.WithMetricName(s.Labels, s.MetricName)
		key := format.CanonicalKey(labels)
		st, ok := bySeries[key]
		if !ok {
			st = &seriesState{labels: labels}
			bySeries[key] = st
		}
		st.rows = append(st.rows, s)
	}
	if err := cursor.Err(); err != nil {
		return nil, err
	}

	const lookback = 5 * time.Minute
	out := make([]MatrixSample, 0, len(bySeries))
	for _, st := range bySeries {
		// Inline insertion sort by Timestamp ascending — rows are
		// typically already nearly sorted from CH.
		for i := 1; i < len(st.rows); i++ {
			for j := i; j > 0 && st.rows[j-1].Timestamp.After(st.rows[j].Timestamp); j-- {
				st.rows[j-1], st.rows[j] = st.rows[j], st.rows[j-1]
			}
		}

		ms := MatrixSample{Metric: st.labels}
		row := 0
		for t := start; !t.After(end); t = t.Add(step) {
			for row < len(st.rows) && !st.rows[row].Timestamp.After(t) {
				row++
			}
			if row == 0 {
				continue
			}
			latest := st.rows[row-1]
			if t.Sub(latest.Timestamp) > lookback {
				continue
			}
			stamp := float64(t.UnixMilli()) / 1e3
			ms.Values = append(ms.Values, Sample{stamp, strconv.FormatFloat(latest.Value, 'f', -1, 64)})
		}
		if len(ms.Values) > 0 {
			out = append(out, ms)
		}
	}
	return out, nil
}

// apiError is a package-local alias for the shared [httperr.Error]
// carrier so the existing in-package callsites can stay literal.
type apiError = httperr.Error

func (h *Handler) respondError(w http.ResponseWriter, err error) {
	// Circuit-breaker fast-fail short-circuit applies regardless of
	// whether the callsite pre-wrapped the chclient error in its own
	// *apiError. The inner ErrCircuitOpen would otherwise be masked
	// by the outer wrap (`&apiError{Status: 502, Err: chclient...}`);
	// errors.Is rescues both shapes (bare and wrapped) for the
	// 503 + Retry-After treatment.
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

// writeError emits the Prom JSON envelope `{status, errorType, error}`.
// The envelope shape is wire-format invariant — Grafana parses it
// directly — so it stays per-handler rather than living in httperr.
func writeError(w http.ResponseWriter, status int, kind string, err error) {
	httperr.WriteJSON(w, status, Response{
		Status:    "error",
		ErrorType: kind,
		Error:     err.Error(),
	})
}
