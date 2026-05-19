package tempo

import (
	"context"
	"encoding/json"
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
// single trace by ID; /api/metrics/query_range evaluates a TraceQL
// metrics pipeline (`| rate()`, `| count_over_time()`, `| *_over_time(...)`)
// against the spans table and returns the matrix in Tempo's
// series-of-samples envelope (the shape Grafana's service-graph and
// metrics dashboards consume); /api/metrics/query is the instant
// variant of the same pipeline — single-bucket evaluation over
// [start, end] returning one (labels, value) per series, matching
// Tempo's translateQueryRangeToInstant wire shape.
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
	register("GET /api/metrics/query_range", h.handleMetricsQueryRange)
	register("GET /api/metrics/query", h.handleMetricsQueryInstant)
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
//
// Circuit-breaker fast-fail: when the chclient breaker is OPEN the
// underlying error chains down to chclient.ErrCircuitOpen and we
// surface HTTP 503 — the Retry-After: 5 stamp lives on the handler
// side (see handleSearch / handleTraceByID).
func classifySearchErr(err error) int {
	if err == nil {
		return http.StatusInternalServerError
	}
	switch {
	case errors.Is(err, chclient.ErrCircuitOpen):
		return http.StatusServiceUnavailable
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
	if errors.Is(err, chclient.ErrCircuitOpen) {
		return http.StatusServiceUnavailable
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

// Reserved keys used by wrapWithSampleProjection to smuggle trace-detail
// fields (TraceId, SpanId, ParentSpanId, SpanKind, StatusCode, +
// SpanAttributes JSON) through the canonical chclient.Sample.Labels
// map for the /api/traces/{id} path. groupBatches splits them back
// out into the SpanEntry fields Grafana's trace-view consumes.
//
// The keys are namespaced under `__cerberus_*` so they cannot collide
// with real OTel attribute keys (which by spec are reserved namespaces
// like `service.*`, `http.*`, etc., never `__*`).
//
// searchKeyTraceID reuses the same `__cerberus_traceID` slot on the
// /api/search path so toTraceSummaries can group spans by real TraceID
// rather than synthesising a key from (SpanName + Timestamp).
const (
	traceByIDKeyTraceID       = "__cerberus_traceID"
	traceByIDKeySpanID        = "__cerberus_spanID"
	traceByIDKeyParentSpanID  = "__cerberus_parentSpanID"
	traceByIDKeySpanKind      = "__cerberus_spanKind"
	traceByIDKeyStatusCode    = "__cerberus_statusCode"
	traceByIDKeySpanAttrsJSON = "__cerberus_spanAttrsJSON"

	// searchKeyTraceID is the reserved Labels key that carries the
	// hex-encoded TraceId on /api/search responses. Same constant value
	// as traceByIDKeyTraceID — the two paths never overlap (search keeps
	// the slot for the trace-id, trace-by-id keeps it for the same).
	searchKeyTraceID = traceByIDKeyTraceID
)

// wrapWithSampleProjection adds a Project on top of plan that emits
// the canonical chclient.Sample shape — (MetricName, Attributes,
// TimeUnix, Value) — adapted to whatever the inner plan's output
// schema actually exposes. Four distinct shapes are recognised:
//
//  1. Scan / Filter(Scan) — the otel_traces columns are available
//     directly; project SpanName / ResourceAttributes / Timestamp /
//     toFloat64(Duration). When meta.IsTraceByID is set, the
//     Attributes map is enriched with span-detail fields the
//     trace-view UI consumes (see traceByIDProjections).
//  2. StructuralJoin — the emitter renders `SELECT R.* FROM (L) AS L
//     INNER JOIN (R) AS R ON …`. CH's outer-scope column resolution
//     depends on the L subquery's column set, so the wrap branches
//     on the structural Op (see the case body for the per-op
//     rationale).
//  3. Aggregate or Filter(Aggregate) — the inner SELECT is just
//     `<group-keys>, <agg-funcs>`; SpanName / Timestamp / Duration
//     don't exist in that scope. Synthesise MetricName="" and
//     TimeUnix=now64(); read Value from the Aggregate's alias.
//  4. Project — lowerSelect for `| select(...)` produces a user-shaped
//     projection (TraceId, SpanId, Timestamp, <selected-attrs>) that
//     doesn't expose SpanName / Duration / ResourceAttributes. The
//     /api/search response envelope only needs the canonical Sample
//     fields, so we strip the user Project and re-wrap the underlying
//     Scan / Filter(Scan) with the canonical projection. The
//     user-selected attrs are dropped from the HTTP search response
//     (they were never surfaced in the trace-summary shape); the spec
//     fixtures that pin the lowerSelect shape go through chDB without
//     wrap-projection so they're unaffected.
func wrapWithSampleProjection(plan chplan.Node, s schema.Traces, meta engine.Meta) chplan.Node {
	switch {
	case isStructuralJoin(plan):
		// CH's column-resolution at the outer SELECT layer depends on
		// the L side of the inner `SELECT R.* FROM (L) AS L INNER JOIN
		// (R) AS R ON ...`:
		//
		//   - For direct ops (>, <, ~), L emits `SELECT * FROM
		//     otel_traces WHERE ...` (all columns); CH leaves R's
		//     columns reachable as `R.<col>` in the outer scope but
		//     refuses bare `<col>` because L and R share names.
		//   - For recursive ops (>>, <<), L is a `WITH RECURSIVE`
		//     closure CTE that only selects (TraceId, SpanId); CH
		//     narrows R-side resolution to those two columns and
		//     refuses `R.<other-col>`, but accepts bare `<col>`
		//     references (which then reach R's full output).
		//
		// Both regressions were caught by tempo_ux.spec.ts (the L10
		// UX flows nightly run). Until the emitter wraps the join in
		// a uniform re-projection layer, branch the wrap projection
		// on Op to use whichever form CH accepts for this shape.
		sj := plan.(*chplan.StructuralJoin)
		// Negated variants (`!>`, `!<`, `!~`, `!>>`, `!<<`) and union
		// variants (`&>`, `&<`, `&~`, `&>>`, `&<<`) share their
		// positive cousin's outer column-resolution shape: the
		// emitter rewrites the SELECT for negated forms (R becomes
		// the FROM source, closure is anti-joined) and emits two
		// INNER-JOIN arms glued by UNION DISTINCT for union forms.
		// In both cases the outer scope's bare-column ambiguity (or
		// lack thereof) mirrors the positive base relation, so we
		// branch on `Positive()` rather than the raw Op.
		switch sj.Op.Positive() {
		case chplan.StructuralDescendant, chplan.StructuralAncestor:
			return &chplan.Project{Input: plan, Projections: canonicalSampleProjections(s)}
		default:
			return &chplan.Project{Input: plan, Projections: rQualifiedSampleProjections(s)}
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
		return &chplan.Project{Input: plan, Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: "MetricName"},
			{Expr: emptyAttrsMap(), Alias: "Attributes"},
			{Expr: &chplan.FuncCall{Name: "now64", Args: []chplan.Expr{&chplan.LitInt{V: 9}}}, Alias: "TimeUnix"},
			{Expr: &chplan.FuncCall{Name: "toFloat64", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}}, Alias: "Value"},
		}}
	case isProjectShape(plan):
		// `| select(...)` lowering produces Project(Filter?(Scan)) with
		// a user-defined column list. The HTTP search envelope only
		// needs canonical Sample columns, so replace the user Project
		// with one rooted in the same input.
		inner := plan.(*chplan.Project).Input
		return &chplan.Project{Input: inner, Projections: canonicalSampleProjections(s)}
	}

	// Default shape — Scan / Filter(Scan) — canonical columns available.
	if meta.IsTraceByID {
		return &chplan.Project{Input: plan, Projections: traceByIDProjections(s)}
	}
	return &chplan.Project{Input: plan, Projections: canonicalSampleProjections(s)}
}

// canonicalSampleProjections returns the standard (MetricName,
// Attributes, TimeUnix, Value) projection over otel_traces canonical
// columns. Used for /api/search and the recursive structural-join
// (`>>` / `<<`) wrap path, where the inner SELECT exposes R's columns
// unqualified to the outer scope.
//
// The Attributes map merges ResourceAttributes with a single
// reserved-key entry (`__cerberus_traceID` → hex TraceId) so
// toTraceSummaries can group spans into per-trace summaries by real
// TraceID rather than synthesising a key from (SpanName + Timestamp).
// Same mapConcat pattern as traceByIDProjections; the resource keys
// (`service.*`, `k8s.*`, …) never collide with the `__cerberus_*`
// namespace so no precedence surprises.
func canonicalSampleProjections(s schema.Traces) []chplan.Projection {
	traceIDMap := &chplan.FuncCall{Name: "map", Args: []chplan.Expr{
		&chplan.LitString{V: searchKeyTraceID},
		&chplan.ColumnRef{Name: s.TraceIDColumn},
	}}
	mergedAttrs := &chplan.FuncCall{Name: "mapConcat", Args: []chplan.Expr{
		&chplan.ColumnRef{Name: s.ResourceAttributesColumn},
		traceIDMap,
	}}
	return []chplan.Projection{
		{Expr: &chplan.ColumnRef{Name: s.SpanNameColumn}, Alias: "MetricName"},
		{Expr: mergedAttrs, Alias: "Attributes"},
		{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: "TimeUnix"},
		// Duration is Int64 (nanoseconds) in OTel-CH; chclient.Sample.Value
		// is float64 and clickhouse-go's Scan refuses Int64→float64 without
		// a cast. toFloat64 keeps the wire shape lossless within the
		// 53-bit mantissa range (a 100-day duration in ns still fits).
		{Expr: &chplan.FuncCall{Name: "toFloat64", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.DurationColumn}}}, Alias: "Value"},
	}
}

// rQualifiedSampleProjections is preserved for direct structural ops
// (`>` / `<` / `~`) where the lowering wraps the join in an inner
// `SELECT R.TraceId AS TraceId, R.SpanId AS SpanId, R.ParentSpanId AS
// ParentSpanId, R.* EXCEPT (TraceId, SpanId, ParentSpanId) FROM
// (otel_traces) L INNER JOIN (otel_traces) R ON …` (see chsql
// structural-join wrap landed in PR #489). After that wrap, R's
// columns are exposed at the inner subquery's output without any
// qualifier, so the outer projections must use bare names — adding
// `R.` would produce `Unknown expression identifier 'R.SpanName' in
// scope` because the outer FROM is the un-aliased wrap subquery.
//
// This function is the bare-column equivalent of canonicalSample
// Projections; the call-site branch on `Op.Positive()` is preserved
// for parity with the historical descendant/ancestor split, but both
// arms now emit identical projections.
func rQualifiedSampleProjections(s schema.Traces) []chplan.Projection {
	traceIDMap := &chplan.FuncCall{Name: "map", Args: []chplan.Expr{
		&chplan.LitString{V: searchKeyTraceID},
		&chplan.ColumnRef{Name: s.TraceIDColumn},
	}}
	mergedAttrs := &chplan.FuncCall{Name: "mapConcat", Args: []chplan.Expr{
		&chplan.ColumnRef{Name: s.ResourceAttributesColumn},
		traceIDMap,
	}}
	return []chplan.Projection{
		{Expr: &chplan.ColumnRef{Name: s.SpanNameColumn}, Alias: "MetricName"},
		{Expr: mergedAttrs, Alias: "Attributes"},
		{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: "TimeUnix"},
		{Expr: &chplan.FuncCall{Name: "toFloat64", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.DurationColumn}}}, Alias: "Value"},
	}
}

// traceByIDProjections returns the wrap-projection used for
// /api/traces/{id}. It mirrors the canonical shape but enriches the
// Attributes map with the span-detail fields Grafana's trace-view
// renders: TraceId / SpanId / ParentSpanId / SpanKind / StatusCode +
// the full SpanAttributes map (JSON-encoded under a single reserved
// key, since the flat chclient.Sample.Labels shape only carries one
// homogeneously-typed Map(String,String) — encoding SpanAttributes
// as JSON lets us round-trip arbitrary OTel attribute keys without
// adding a chplan.Lambda for arrayMap key-rewriting). groupBatches
// pivots the reserved-key entries into the SpanEntry fields and
// json-parses the SpanAttributes blob back into a map.
func traceByIDProjections(s schema.Traces) []chplan.Projection {
	// Build the per-row metadata map(k1, v1, k2, v2, ...) of trace /
	// span identity fields + the JSON-encoded SpanAttributes. All
	// values are String so the mapConcat with ResourceAttributes
	// satisfies CH's homogeneous-value-type requirement.
	metaKVPairs := []chplan.Expr{
		&chplan.LitString{V: traceByIDKeyTraceID},
		&chplan.ColumnRef{Name: s.TraceIDColumn},
		&chplan.LitString{V: traceByIDKeySpanID},
		&chplan.ColumnRef{Name: s.SpanIDColumn},
		&chplan.LitString{V: traceByIDKeyParentSpanID},
		&chplan.ColumnRef{Name: s.ParentSpanIDColumn},
		&chplan.LitString{V: traceByIDKeySpanKind},
		// SpanKind / StatusCode are LowCardinality(String); cast to
		// String so the mapConcat homogeneous-value-type requirement
		// is met across the merged Map(String,String).
		&chplan.FuncCall{Name: "toString", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.SpanKindColumn}}},
		&chplan.LitString{V: traceByIDKeyStatusCode},
		&chplan.FuncCall{Name: "toString", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.StatusCodeColumn}}},
		&chplan.LitString{V: traceByIDKeySpanAttrsJSON},
		// CH `toJSONString(<Map>)` renders a JSON object like
		// {"http.method":"GET","http.status_code":"200"}. The Go
		// handler json-decodes it back into a Go map[string]string.
		&chplan.FuncCall{Name: "toJSONString", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}}},
	}
	metaMap := &chplan.FuncCall{Name: "map", Args: metaKVPairs}

	// Final Attributes = mapConcat(ResourceAttributes, metaMap).
	// Resource attribute keys (service.*, k8s.*, host.*, ...) never
	// collide with the __cerberus_* reserved keys so no precedence
	// surprises.
	mergedAttrs := &chplan.FuncCall{Name: "mapConcat", Args: []chplan.Expr{
		&chplan.ColumnRef{Name: s.ResourceAttributesColumn},
		metaMap,
	}}

	return []chplan.Projection{
		{Expr: &chplan.ColumnRef{Name: s.SpanNameColumn}, Alias: "MetricName"},
		{Expr: mergedAttrs, Alias: "Attributes"},
		{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: "TimeUnix"},
		{Expr: &chplan.FuncCall{Name: "toFloat64", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.DurationColumn}}}, Alias: "Value"},
	}
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

// isProjectShape reports whether the plan root is a chplan.Project.
// Today the only producer at HTTP layer is internal/traceql.lowerSelect
// (the `| select(...)` pipeline element), which projects user-chosen
// columns that don't match the canonical chclient.Sample shape; the
// wrap drops the user Project and re-projects the underlying Scan to
// keep the search-result envelope intact. See the
// wrapWithSampleProjection switch comment for the rationale.
func isProjectShape(plan chplan.Node) bool {
	_, ok := plan.(*chplan.Project)
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
	case *chplan.MetricsSecondStage:
		// TraceQL second-stage transforms (topk / bottomk / threshold)
		// preserve the inner aggregate's SELECT-list shape — recurse.
		return isAggregateShape(v.Input)
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
// summary; StartTimeUnixNano is the earliest span timestamp seen and
// DurationMs is the max span duration (a coarse proxy until per-trace
// aggregate plumbing lands).
//
// chclient.Sample's MetricName carries SpanName here (per the wrap
// projection above) and Attributes carries ResourceAttributes plus a
// reserved `__cerberus_traceID` entry (searchKeyTraceID); we use the
// reserved entry as the grouping key so spans share a row when they
// share a real trace, and derive RootServiceName from
// Attributes['service.name']. The reserved entry is stripped from
// the returned RootServiceName lookup since it's namespaced under
// `__cerberus_*` and never collides with OTel-spec attribute keys.
//
// Defensive: samples missing the reserved key (older fixtures, stub
// queriers in tests) fall back to (SpanName | Timestamp) so partial
// data still surfaces a row rather than silently dropping.
func toTraceSummaries(samples []chclient.Sample) []TraceSummary {
	type acc struct {
		traceID     string
		serviceName string
		traceName   string
		startNS     int64
		durationNS  int64
	}
	byTrace := map[string]*acc{}
	for _, s := range samples {
		traceID, hasID := s.Labels[searchKeyTraceID]
		// Group key prefers the real TraceID so multi-span traces
		// collapse into one row; fall back to the legacy synthetic
		// shape only when the projection didn't supply it.
		key := traceID
		if !hasID || key == "" {
			key = s.MetricName + "|" + s.Timestamp.Format("20060102150405.000000000")
		}
		a, ok := byTrace[key]
		if !ok {
			a = &acc{traceID: traceID, traceName: s.MetricName}
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
		// Emit the real TraceID when the projection supplied one;
		// otherwise surface the synthetic key (back-compat for stub
		// queriers + older fixtures that never threaded it through).
		tid := a.traceID
		if tid == "" {
			tid = k
		}
		out = append(out, TraceSummary{
			TraceID:           tid,
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
// (one batch per distinct ResourceAttributes set). The wrap-projection
// for /api/traces/{id} (see traceByIDProjections) smuggles span-detail
// fields (TraceId / SpanId / ParentSpanId / SpanKind / StatusCode plus
// the SpanAttributes map) inside chclient.Sample.Labels under reserved
// keys; we split them back out into the SpanEntry fields Grafana's
// trace-view consumes and keep ResourceAttributes (un-prefixed entries)
// on the Resource.Attributes map.
func groupBatches(samples []chclient.Sample) []ResourceSpans {
	bucket := map[string]*ResourceSpans{}
	for _, s := range samples {
		resourceAttrs, spanAttrs, meta := splitTraceByIDLabels(s.Labels)
		// Group by resource-attribute set so Grafana's "Processes" tab
		// gets one batch per service.
		key := format.CanonicalKey(resourceAttrs)
		rs, ok := bucket[key]
		if !ok {
			rs = &ResourceSpans{Resource: Resource{Attributes: resourceAttrs}}
			bucket[key] = rs
		}
		rs.Spans = append(rs.Spans, SpanEntry{
			TraceID:           meta[traceByIDKeyTraceID],
			SpanID:            meta[traceByIDKeySpanID],
			ParentSpanID:      meta[traceByIDKeyParentSpanID],
			Name:              s.MetricName,
			Kind:              meta[traceByIDKeySpanKind],
			StartTimeUnixNano: strconv.FormatInt(s.Timestamp.UnixNano(), 10),
			DurationNanos:     int64(s.Value),
			Status:            SpanStatus{Code: meta[traceByIDKeyStatusCode]},
			Attributes:        spanAttrs,
		})
	}
	out := make([]ResourceSpans, 0, len(bucket))
	for _, rs := range bucket {
		out = append(out, *rs)
	}
	return out
}

// splitTraceByIDLabels partitions the chclient.Sample.Labels map into
// (resourceAttrs, spanAttrs, meta) using the reserved-key constants
// established by traceByIDProjections:
//
//   - traceByIDKeySpanAttrsJSON's value is a JSON-encoded map; we
//     json-decode it into spanAttrs.
//   - Keys that exactly match one of the traceByIDKey* metadata
//     constants go into meta.
//   - Everything else stays in resourceAttrs (so Grafana's "Processes"
//     tab still sees ResourceAttributes verbatim).
//
// On the /api/search path (non-trace-by-id), no reserved keys are
// present and the function returns labels as resourceAttrs unchanged.
func splitTraceByIDLabels(labels map[string]string) (resourceAttrs, spanAttrs, meta map[string]string) {
	resourceAttrs = make(map[string]string, len(labels))
	for k, v := range labels {
		switch k {
		case traceByIDKeySpanAttrsJSON:
			if v == "" || v == "{}" {
				continue
			}
			var parsed map[string]string
			if err := json.Unmarshal([]byte(v), &parsed); err != nil {
				// Defensive: malformed JSON should never happen
				// (CH toJSONString is canonical), but if it does we
				// drop the span-attrs rather than failing the whole
				// trace view.
				continue
			}
			spanAttrs = parsed
		case traceByIDKeyTraceID, traceByIDKeySpanID, traceByIDKeyParentSpanID,
			traceByIDKeySpanKind, traceByIDKeyStatusCode:
			if meta == nil {
				meta = map[string]string{}
			}
			meta[k] = v
		default:
			resourceAttrs[k] = v
		}
	}
	return resourceAttrs, spanAttrs, meta
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
//
// When the underlying error chain contains chclient.ErrCircuitOpen
// (the GA-default downstream-CH circuit breaker is OPEN) the writer
// stamps `Retry-After: 5` on the response so well-behaved clients
// back off for the breaker's recovery window. The 503 status is
// supplied by classifySearchErr / classifyTraceByIDErr; the header
// is set here so all Tempo error paths get it without each call site
// repeating the boilerplate.
func writeError(w http.ResponseWriter, status int, traceID, spanID string, err error) {
	if errors.Is(err, chclient.ErrCircuitOpen) {
		w.Header().Set("Retry-After", "5")
	}
	httperr.WriteJSON(w, status, ErrorResponse{
		TraceID: traceID, SpanID: spanID, Error: true,
		Message: err.Error(),
	})
}
