package tempo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/grafana/tempo/pkg/traceql"
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
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/telemetry"
)

// tracer emits the `parse` pipeline-stage span before the TraceQL
// parser runs.
var tracer = otel.Tracer("github.com/tsouza/cerberus/internal/api/tempo")

// parseExpr wraps traceql.Parse in a `parse` pipeline-stage span. The
// QL identifier and the (truncated) query string land on the span as
// `cerberus.ql` + `cerberus.query`.
func parseExpr(ctx context.Context, query string) (*traceql.RootExpr, error) {
	_, span := tracer.Start(ctx, cerbtrace.SpanParse,
		trace.WithAttributes(cerbtrace.ParseAttrs("traceql", query)...))
	defer span.End()
	expr, err := traceql.Parse(query)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	return expr, nil
}

// Querier is the subset of *chclient.Client the Handler needs. Stub
// shape mirrors api/prom + api/loki for the same test reasons.
//
// QueryStrings backs the /api/search/tags + /api/search/tag/<name>/values
// endpoints, which expect a single-string-column result rather than the
// chclient.Sample shape.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) ([]chclient.Sample, error)
	QueryStrings(ctx context.Context, sql string, args ...any) ([]string, error)
}

// Handler implements the Tempo HTTP API endpoints cerberus speaks.
// Mount it via Handler.Mount(mux). The current surface covers
// /api/echo, /api/status/version, /api/search, /api/search/recent,
// /api/search/tags, /api/search/tag/{name}/values (plus the V2
// variants under /api/v2/), and /api/traces/{id}.
type Handler struct {
	Client    Querier
	Schema    schema.Traces
	Optimizer *optimizer.Driver
	Logger    *slog.Logger
	Version   string

	// Engine runs the shared parse → wrap-projection → optimize →
	// emit → execute pipeline for /api/search and /api/traces/{id}.
	// The string-result endpoints (/api/search/tags etc.) still call
	// Client.QueryStrings directly because the engine surface is
	// row-oriented today.
	Engine *engine.Engine
	// lang is the TraceQL adapter Engine calls for parse + wrap.
	lang *traceqlLang

	// Limiter caps in-flight Tempo API requests. nil disables the
	// admission middleware. Wired from CERBERUS_ADMIT_TEMPO.
	Limiter *admit.Limiter
}

// New constructs a Handler with the seed optimizer wired in.
func New(client Querier, s schema.Traces, version string, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	opt := optimizer.Default()
	return &Handler{
		Client:    client,
		Schema:    s,
		Optimizer: opt,
		Logger:    logger,
		Version:   version,
		Engine:    &engine.Engine{Optimizer: opt, Client: client},
		lang:      &traceqlLang{schema: s},
	}
}

// Mount registers the Tempo-compatible endpoints under /api/ on mux.
// /api/echo + /api/status/version satisfy Grafana's datasource health
// check; /api/search runs a TraceQL query; /api/traces/{id} fetches a
// single trace by ID.
func (h *Handler) Mount(mux *http.ServeMux) {
	// Every Tempo endpoint flows through the cerberus.queries.* counter
	// + duration middleware. /api/echo and /api/status/version
	// are health-checks and will dominate ResultOK volume — that's fine,
	// dashboards can split them out via cerberus.route if needed.
	register := func(pattern string, hf http.HandlerFunc) {
		// admit.Middleware (outer) → telemetry.QueryMiddleware (inner)
		// — same layering as the Prom + Loki heads. Rejections land
		// on cerberus.admit.rejected_total, not cerberus.queries.*.
		mux.Handle(pattern, h.Limiter.Middleware(1, telemetry.QueryMiddleware("traceql", hf)))
	}
	register("GET /api/echo", h.handleEcho)
	register("GET /api/status/version", h.handleVersion)
	register("GET /api/search", h.handleSearch)
	register("GET /api/search/recent", h.handleSearchRecent)
	register("GET /api/search/tags", h.handleSearchTags)
	register("GET /api/search/tag/{name}/values", h.handleSearchTagValues)
	register("GET /api/v2/search/tags", h.handleSearchTagsV2)
	register("GET /api/v2/search/tag/{name}/values", h.handleSearchTagValuesV2)
	register("GET /api/traces/{id}", h.handleTraceByID)
}

func (h *Handler) handleEcho(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("echo"))
}

func (h *Handler) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, VersionResponse{
		Version:   h.Version,
		GoVersion: runtime.Version(),
	})
}

func (h *Handler) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		// Grafana sometimes pings /api/search with no query as a
		// health-check; return an empty result rather than an error.
		writeJSON(w, http.StatusOK, SearchResponse{Traces: []TraceSummary{}})
		return
	}

	ctx := r.Context()
	// Engine.Query runs parse → lower → wrap-projection → optimize →
	// emit → execute. The TraceQL adapter (h.lang) owns the parser
	// dispatch + wrap-projection so the post-engine response pivot
	// stays handler-local.
	res, err := h.Engine.Query(ctx, h.lang, q)
	if err != nil {
		writeError(w, classifySearchErr(err), "", "", err)
		return
	}
	h.Logger.Debug("cerberus tempo search", "traceql", q, "sql", res.SQL, "args", res.Args)

	writeEngineHeaders(w, res.Headers)
	writeJSON(w, http.StatusOK, SearchResponse{
		Traces:  toTraceSummaries(res.Samples),
		Metrics: SearchMetrics{InspectedTraces: len(res.Samples)},
	})
}

// writeEngineHeaders stamps the X-Cerberus-* response headers populated
// by engine.Engine.Query / QueryPlan onto w before the response body
// fires. Safe to call with a nil / empty map (no-op). See the matching
// helper in internal/api/loki/handler.go for the full rationale.
func writeEngineHeaders(w http.ResponseWriter, hdr map[string]string) {
	for k, v := range hdr {
		w.Header().Set(k, v)
	}
}

// classifySearchErr maps an engine.Query error to the HTTP status the
// inline handler used to return: parse failures → 400, lower failures
// → 422, emit failures → 500, execute failures → 502. The Lang
// adapter tags parse vs lower errors with errParseStage / errLowerStage
// so errors.Is recovers the precise stage even though the engine
// collapses both into its outer `engine: parse:` wrapper.
func classifySearchErr(err error) int {
	if err == nil {
		return http.StatusInternalServerError
	}
	switch {
	case errors.Is(err, errParseStage):
		return http.StatusBadRequest
	case errors.Is(err, errLowerStage):
		return http.StatusUnprocessableEntity
	case strings.Contains(err.Error(), "engine: emit:"):
		return http.StatusInternalServerError
	case strings.Contains(err.Error(), "engine: execute:"):
		return http.StatusBadGateway
	}
	return http.StatusInternalServerError
}

// handleSearchRecent implements `GET /api/search/recent`. Returns the
// most-recent N traces (per the seeded Timestamp) without applying a
// TraceQL filter. Grafana's Tempo Search UI calls this on first page
// load to populate the trace list.
//
// Honors `?limit=N` (default 20, max 200); ignores `start` / `end` for
// now (the emitter doesn't thread them through OrderBy + Limit).
func (h *Handler) handleSearchRecent(w http.ResponseWriter, r *http.Request) {
	limit := int64(20)
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			if n > 200 {
				n = 200
			}
			limit = n
		}
	}

	s := h.Schema
	// Plan: Limit(OrderBy(Scan(otel_traces) ORDER BY Timestamp DESC) LIMIT N)
	// — the engine applies wrap-projection + optimizer + emit + execute.
	plan := chplan.Node(&chplan.Scan{Table: s.SpansTable})
	plan = &chplan.OrderBy{
		Input: plan,
		Keys: []chplan.OrderKey{
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Desc: true},
		},
	}
	plan = &chplan.Limit{Input: plan, Count: limit}

	ctx := r.Context()
	// /search/recent doesn't go through a parser, so use QueryPlan
	// directly. IsTraceByID stays false: the OrderBy+Limit shape
	// benefits from the seed optimizer's projection-pushdown pass.
	res, err := h.Engine.QueryPlan(ctx, h.lang, plan, engine.Meta{ResponseShape: "tempo-trace"})
	if err != nil {
		writeError(w, classifySearchErr(err), "", "", err)
		return
	}
	h.Logger.Debug("cerberus tempo search/recent", "limit", limit, "sql", res.SQL, "args", res.Args)

	writeEngineHeaders(w, res.Headers)
	writeJSON(w, http.StatusOK, SearchResponse{
		Traces:  toTraceSummaries(res.Samples),
		Metrics: SearchMetrics{InspectedTraces: len(res.Samples)},
	})
}

func (h *Handler) handleTraceByID(w http.ResponseWriter, r *http.Request) {
	traceID := r.PathValue("id")
	if traceID == "" {
		writeError(w, http.StatusBadRequest, "", "", fmt.Errorf("missing trace id"))
		return
	}

	// Build the lowered query: { traceID = "<id>" }.
	plan, err := h.lowerTraceByID(traceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "", "", err)
		return
	}

	// /traces/{id} is the canonical engine.QueryPlan + IsTraceByID
	// caller: the plan is hand-built (no parser), and the row-by-id
	// fetch has no rewrites worth running — IsTraceByID = true tells
	// the engine to skip the optimizer pass entirely (engine.go's
	// `if !meta.IsTraceByID` branch). The wrap-projection still runs
	// via Lang.ProjectSamples so the chclient.Sample shape is
	// canonical before emit.
	ctx := r.Context()
	res, err := h.Engine.QueryPlan(ctx, h.lang, plan, engine.Meta{
		IsTraceByID:   true,
		ResponseShape: "tempo-trace",
	})
	if err != nil {
		h.Logger.Error("cerberus tempo traceByID CH query failed", "err", err, "trace_id", traceID)
		writeError(w, classifyTraceByIDErr(err), traceID, "", err)
		return
	}
	h.Logger.Debug("cerberus tempo traceByID", "trace_id", traceID, "sql", res.SQL, "args", res.Args)

	writeEngineHeaders(w, res.Headers)
	if len(res.Samples) == 0 {
		// Tempo's "trace not found" shape — Grafana renders the right UI.
		writeJSON(w, http.StatusNotFound, ErrorResponse{
			TraceID: traceID, SpanID: "", Error: true,
			Message: fmt.Sprintf("trace not found: %s", traceID),
		})
		return
	}

	writeJSON(w, http.StatusOK, TraceByIDResponse{
		Batches: groupBatches(res.Samples),
	})
}

// classifyTraceByIDErr is the trace-by-ID counterpart to
// classifySearchErr. Only emit (500) and execute (502) errors are
// reachable today since the plan is hand-built and IsTraceByID skips
// the optimizer; the helper keeps the same shape for consistency
// with the search-path classifier.
func classifyTraceByIDErr(err error) int {
	if err == nil {
		return http.StatusInternalServerError
	}
	if strings.Contains(err.Error(), "engine: execute:") {
		return http.StatusBadGateway
	}
	return http.StatusInternalServerError
}

// lowerTraceByID builds a chplan tree equivalent to the TraceQL
// `{ traceID = "<id>" }` query without going through the parser.
// Cheaper than re-parsing for every trace lookup.
func (h *Handler) lowerTraceByID(traceID string) (chplan.Node, error) {
	pred := &chplan.Binary{
		Op:    chplan.OpEq,
		Left:  &chplan.ColumnRef{Name: h.Schema.TraceIDColumn},
		Right: &chplan.LitString{V: traceID},
	}
	return &chplan.Filter{
		Input:     &chplan.Scan{Table: h.Schema.SpansTable},
		Predicate: pred,
	}, nil
}

// wrapWithSampleProjection adds a Project on top of plan that emits
// the canonical chclient.Sample shape — (MetricName, Attributes,
// TimeUnix, Value) — adapted to whatever the inner plan's output
// schema actually exposes. Three distinct shapes are recognised:
//
//  1. Scan / Filter(Scan) — the otel_traces columns are available
//     directly; project SpanName / ResourceAttributes / Timestamp /
//     toFloat64(Duration).
//  2. StructuralJoin — the emitter renders `SELECT R.* FROM (L)
//     INNER JOIN (R) ON …`, which exposes R's columns as
//     R-prefixed names (not unqualified). The wrap-projection uses
//     ColumnRef.Qualifier="R" to reach them.
//  3. Aggregate or Filter(Aggregate) — the inner SELECT is just
//     `<group-keys>, <agg-funcs>`; SpanName / Timestamp / Duration
//     don't exist in that scope. Synthesise MetricName="" and
//     TimeUnix=now64(); read Value from the Aggregate's alias.
func wrapWithSampleProjection(plan chplan.Node, s schema.Traces) chplan.Node {
	// Default shape — Scan / Filter(Scan) — canonical columns available.
	projections := []chplan.Projection{
		{Expr: &chplan.ColumnRef{Name: s.SpanNameColumn}, Alias: "MetricName"},
		{Expr: &chplan.ColumnRef{Name: s.ResourceAttributesColumn}, Alias: "Attributes"},
		{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: "TimeUnix"},
		// Duration is Int64 (nanoseconds) in OTel-CH; chclient.Sample.Value
		// is float64 and clickhouse-go's Scan refuses Int64→float64 without
		// a cast. toFloat64 keeps the wire shape lossless within the
		// 53-bit mantissa range (a 100-day duration in ns still fits).
		{Expr: &chplan.FuncCall{Name: "toFloat64", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.DurationColumn}}}, Alias: "Value"},
	}

	switch {
	case isStructuralJoin(plan):
		// CH exposes the right side of a self-join as R.<col>. Use the
		// qualifier so the outer SELECT can reach those columns.
		projections = []chplan.Projection{
			{Expr: &chplan.ColumnRef{Qualifier: "R", Name: s.SpanNameColumn}, Alias: "MetricName"},
			{Expr: &chplan.ColumnRef{Qualifier: "R", Name: s.ResourceAttributesColumn}, Alias: "Attributes"},
			{Expr: &chplan.ColumnRef{Qualifier: "R", Name: s.TimestampColumn}, Alias: "TimeUnix"},
			{Expr: &chplan.FuncCall{Name: "toFloat64", Args: []chplan.Expr{&chplan.ColumnRef{Qualifier: "R", Name: s.DurationColumn}}}, Alias: "Value"},
		}
	case isAggregateShape(plan):
		// Aggregate output is just (group-keys, agg-func-aliases). SpanName
		// and Duration aren't in scope; synthesise the missing pieces.
		// The aggregate's alias is "Value" by convention (set by
		// internal/traceql/aggregate.go); other code shapes that emit a
		// different alias would need to thread it through. count() has
		// no GROUP BY so ResourceAttributes isn't in scope either — use
		// an empty Map(String,String) CAST, same pattern as PromQL's
		// emptyAttrsMap helper.
		projections = []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: "MetricName"},
			{Expr: emptyAttrsMap(), Alias: "Attributes"},
			{Expr: &chplan.FuncCall{Name: "now64", Args: []chplan.Expr{&chplan.LitInt{V: 9}}}, Alias: "TimeUnix"},
			{Expr: &chplan.FuncCall{Name: "toFloat64", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}}, Alias: "Value"},
		}
	}

	return &chplan.Project{Input: plan, Projections: projections}
}

// emptyAttrsMap returns a CH expression for an empty Map(String,String),
// used when an Aggregate's GROUP BY is empty (e.g. bare `count()`) and
// ResourceAttributes isn't in the inner scope. Mirrors the helper in
// internal/promql/lower.go and internal/logql/vector_aggregation.go so
// the rendered SQL has the same shape across the three QL surfaces.
func emptyAttrsMap() chplan.Expr {
	return &chplan.FuncCall{
		Name: "CAST",
		Args: []chplan.Expr{
			&chplan.FuncCall{Name: "map", Args: nil},
			&chplan.LitString{V: "Map(String,String)"},
		},
	}
}

// isStructuralJoin reports whether the plan root is a StructuralJoin
// (or a thin wrapper over one). Today only the bare form needs the
// R-qualifier handling; nested wrappers would need recursive shape
// inspection — defer until a real call-site needs it.
func isStructuralJoin(plan chplan.Node) bool {
	_, ok := plan.(*chplan.StructuralJoin)
	return ok
}

// isAggregateShape reports whether the plan's output schema is just
// the Aggregate's projected columns (group-keys + agg-func aliases),
// i.e. the otel_traces canonical columns aren't available. Covers
// bare Aggregate, Filter(Aggregate) (TraceQL scalar-filter HAVING
// shape), and Project(Aggregate) (potential future shape).
func isAggregateShape(plan chplan.Node) bool {
	switch v := plan.(type) {
	case *chplan.Aggregate:
		return true
	case *chplan.MetricsAggregate:
		// TraceQL metrics-pipeline output: same SELECT-list shape as
		// chplan.Aggregate (group-keys + Value alias).
		return true
	case *chplan.Filter:
		return isAggregateShape(v.Input)
	case *chplan.Project:
		// Explicit Project — assume the user took responsibility for
		// the output columns.
		return false
	}
	return false
}

// toTraceSummaries pivots samples into the per-trace summary shape
// Tempo's /api/search returns. Each unique TraceID becomes one
// summary; duration is the max span duration seen for that trace
// (a coarse proxy until the per-trace aggregate plumbing lands).
//
// Note: chclient.Sample's MetricName carries SpanName here (per the
// projection above) and Attributes carries ResourceAttributes; we
// derive RootServiceName from Attributes['service.name'].
func toTraceSummaries(samples []chclient.Sample) []TraceSummary {
	type acc struct {
		serviceName string
		traceName   string
		startNS     int64
		durationNS  int64
	}
	byTrace := map[string]*acc{}
	for _, s := range samples {
		// The TraceID lives in the row's labels under a synthetic key —
		// for the search projection we don't pull it out (handler hits
		// `/search/tags` defer); instead each unique sample is one span,
		// and we use the MetricName + Timestamp to summarise.
		// TODO(M4.5 follow-up): include TraceId in projection.
		key := s.MetricName + "|" + s.Timestamp.Format("20060102150405.000000000")
		a, ok := byTrace[key]
		if !ok {
			a = &acc{traceName: s.MetricName}
			byTrace[key] = a
		}
		if svc, ok := s.Labels["service.name"]; ok {
			a.serviceName = svc
		}
		ns := s.Timestamp.UnixNano()
		if a.startNS == 0 || ns < a.startNS {
			a.startNS = ns
		}
		if int64(s.Value) > a.durationNS {
			a.durationNS = int64(s.Value)
		}
	}
	out := make([]TraceSummary, 0, len(byTrace))
	for k, a := range byTrace {
		out = append(out, TraceSummary{
			TraceID:           k, // synthetic until we project TraceId
			RootServiceName:   a.serviceName,
			RootTraceName:     a.traceName,
			StartTimeUnixNano: strconv.FormatInt(a.startNS, 10),
			DurationMs:        int(a.durationNS / 1_000_000),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TraceID < out[j].TraceID })
	return out
}

// groupBatches converts a flat span list into Tempo's `batches` shape
// (one batch per distinct ResourceAttributes set).
func groupBatches(samples []chclient.Sample) []ResourceSpans {
	bucket := map[string]*ResourceSpans{}
	for _, s := range samples {
		key := format.CanonicalKey(s.Labels)
		rs, ok := bucket[key]
		if !ok {
			rs = &ResourceSpans{Resource: Resource{Attributes: s.Labels}}
			bucket[key] = rs
		}
		rs.Spans = append(rs.Spans, SpanEntry{
			Name:              s.MetricName,
			StartTimeUnixNano: strconv.FormatInt(s.Timestamp.UnixNano(), 10),
			DurationNanos:     int64(s.Value),
		})
	}
	out := make([]ResourceSpans, 0, len(bucket))
	for _, rs := range bucket {
		out = append(out, *rs)
	}
	return out
}

// writeJSON wraps [httperr.WriteJSON] so package-local callsites stay
// unqualified. The shared helper handles Content-Type + status + body
// encoding identically across all three handlers.
func writeJSON(w http.ResponseWriter, status int, body any) {
	httperr.WriteJSON(w, status, body)
}

// writeError emits Tempo's distinct error envelope
// `{traceID, spanID, error, message}` (vs Prom's
// `{status, errorType, error}`) so Grafana renders the right UI. The
// envelope shape is wire-format invariant — it stays per-handler rather
// than living in httperr.
func writeError(w http.ResponseWriter, status int, traceID, spanID string, err error) {
	httperr.WriteJSON(w, status, ErrorResponse{
		TraceID: traceID, SpanID: spanID, Error: true,
		Message: err.Error(),
	})
}
