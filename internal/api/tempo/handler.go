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
	"time"

	"github.com/gogo/protobuf/jsonpb"
	"github.com/gogo/protobuf/proto"
	"github.com/grafana/tempo/pkg/tempopb"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	traceql "github.com/tsouza/cerberus/internal/traceql/ast"

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
	traceql_lower "github.com/tsouza/cerberus/internal/traceql"
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

// Lang exposes the TraceQL engine.Lang adapter wired into the Handler.
// Sibling surfaces (the gRPC StreamingQuerier in internal/api/tempo/grpc)
// reuse this so the parse + lower + wrap-projection pipeline is shared
// byte-for-byte with the HTTP /api/search path — the only divergence is
// the response serialisation.
func (h *Handler) Lang() engine.Lang { return h.lang }

// ToTraceSummaries is the package-level export of the toTraceSummaries
// shaper used by /api/search. The gRPC Search RPC (see
// internal/api/tempo/grpc/search.go) calls it to translate the cursor's
// chclient.Sample stream into the per-trace summary shape that maps
// onto tempopb.TraceSearchMetadata.
//
// Returns (summaries, missingRootTraceIDs); see toTraceSummaries for
// the per-row grouping + root-resolution semantics. Equivalent to
// calling toTraceSummaries directly inside the tempo package; the
// re-export exists purely so external callers don't depend on an
// unexported helper.
//
// spss caps the spans collected into each summary's SpanSet (Tempo's
// `spss` / SpansPerSpanSet request param); <= 0 applies
// DefaultSpansPerSpanSet.
func ToTraceSummaries(samples []chclient.Sample, spss int) ([]TraceSummary, []string) {
	return toTraceSummaries(samples, spss)
}

// ResolveMissingRoots issues the follow-up CH lookup that recovers root-
// span identity + trace-wide duration for traces whose result set
// lacked a true root row (typical for structural-join queries and
// `{ status = error }` style predicates that only match child spans),
// then patches the affected summaries in place. Mirrors the HTTP
// handler's post-search resolution step so the gRPC Search RPC produces
// byte-equivalent trace metadata for the same TraceQL input.
//
// `missing` may be nil/empty (no-op fast path). Returns a non-nil error
// only when the follow-up CH query fails — callers typically log + ignore
// (the earliest-span fallback in summaries stays in place), matching the
// HTTP handler's "best-effort" semantics.
func (h *Handler) ResolveMissingRoots(ctx context.Context, summaries []TraceSummary, missing []string) error {
	if len(missing) == 0 {
		return nil
	}
	roots, err := h.resolveTraceRoots(ctx, missing)
	if err != nil {
		return err
	}
	applyRootMetadata(summaries, roots)
	return nil
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
		mux.Handle(pattern, h.Limiter.Middleware(1, telemetry.QueryMiddleware("traceql", panicEnvelope, hf)))
	}
	register("GET /api/echo", h.handleEcho)
	register("GET /api/status/version", h.handleVersion)
	// Grafana's Tempo datasource probes /api/status/buildinfo on every
	// page load to gate streaming-search + metrics-summary feature
	// flags. Tempo OSS responds with the same shape /status/version
	// emits — share the handler so cerberus answers both consistently.
	register("GET /api/status/buildinfo", h.handleVersion)
	register("GET /api/search", h.handleSearch)
	register("GET /api/search/recent", h.handleSearchRecent)
	register("GET /api/search/tags", h.handleSearchTags)
	register("GET /api/search/tag/{name}/values", h.handleSearchTagValues)
	register("GET /api/v2/search/tags", h.handleSearchTagsV2)
	register("GET /api/v2/search/tag/{name}/values", h.handleSearchTagValuesV2)
	register("GET /api/traces/{id}", h.handleTraceByID)
	// /api/v2/traces/{id} is the modern Grafana Tempo datasource path —
	// the default in Grafana 11.x+ whenever the datasource's
	// `tempoApiVersion` setting is >= v2 (which is the out-of-the-box
	// default for new datasources). Unlike the v1 endpoint, reference
	// Tempo's v2 returns the response ENVELOPED in a
	// tempopb.TraceByIDResponse (`{trace, metrics, status, message}`),
	// not a bare tempopb.Trace — see upstream
	// `modules/frontend/combiner/trace_by_id_v2.go` (proto + jsonpb
	// marshal of the envelope) vs `trace_by_id.go` (bare Trace).
	// Grafana 12.x's Tempo plugin proto.Unmarshal-s the v2 body as
	// TraceByIDResponse before converting to OTLP; serving the bare v1
	// bytes on this URL misaligns the decode one message level deep and
	// dies with `proto: KeyValue: wiretype end group for non-group`
	// inside Grafana ("An error occurred within the plugin").
	register("GET /api/v2/traces/{id}", h.handleTraceByIDV2)
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

// DefaultSearchLimit / DefaultSpansPerSpanSet mirror reference Tempo's
// /api/search request defaults (pkg/api ParseSearchRequest): at most 20
// trace summaries per response, at most 3 spans per spanset. Exported so
// the gRPC StreamingQuerier Search RPC (internal/api/tempo/grpc) applies
// the same defaults when tempopb.SearchRequest leaves Limit /
// SpansPerSpanSet unset.
const (
	DefaultSearchLimit     = 20
	DefaultSpansPerSpanSet = 3
)

// MaxSearchLimit caps the `limit` request param on /api/search (and the gRPC
// Search RPC). A client-supplied limit above this is clamped rather than
// rejected so a misbehaving caller can't make the response buffer the spans of
// an unbounded number of traces. The inner SQL LIMIT bounds the trace COUNT,
// not the per-trace span count, so the response shaper still materialises every
// span of every matched trace before truncation — this cap bounds that buffer.
// Exported so the gRPC Search callsite applies the same ceiling.
const MaxSearchLimit = 1000

// DefaultSearchLookback bounds a windowless /api/search (no start/end
// params) to the most recent hour, so the trace-limit pushdown's inner
// GROUP BY TraceId scans a window instead of the whole table. Matches
// reference Tempo's "recent data" default for a search with no range.
const DefaultSearchLookback = time.Hour

// positiveIntParam parses an integer query param, returning def when the
// param is absent, malformed, or non-positive — mirroring reference
// Tempo's lenient ParseSearchRequest handling (a bad `limit` falls back
// to the default rather than erroring the search).
func positiveIntParam(r *http.Request, name string, def int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// TruncateSummaries enforces Tempo's `limit` request param on a
// toTraceSummaries result: keeps the first `limit` summaries (the slice
// arrives sorted startTimeUnixNano-desc, so these are the newest traces
// — Tempo's ordering) and filters the missing-root TraceID list down to
// the kept traces so the follow-up root lookup never queries traces the
// response won't include. limit <= 0 applies DefaultSearchLimit.
// Exported for the gRPC Search RPC, which shares the HTTP path's
// summary pipeline.
func TruncateSummaries(summaries []TraceSummary, missing []string, limit int) ([]TraceSummary, []string) {
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	if len(summaries) <= limit {
		return summaries, missing
	}
	summaries = summaries[:limit]
	if len(missing) == 0 {
		return summaries, missing
	}
	kept := make(map[string]struct{}, len(summaries))
	for _, t := range summaries {
		kept[t.TraceID] = struct{}{}
	}
	filtered := missing[:0]
	for _, id := range missing {
		if _, ok := kept[id]; ok {
			filtered = append(filtered, id)
		}
	}
	return summaries, filtered
}

func (h *Handler) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		// Grafana sometimes pings /api/search with no query as a
		// health-check; return an empty result rather than an error.
		writeJSON(w, http.StatusOK, SearchResponse{Traces: []TraceSummary{}})
		return
	}
	// Tempo's /api/search honours `limit` (max trace summaries, default
	// 20) and `spss` (max spans per spanset, default 3). Grafana's
	// Traces Drilldown sends both (limit=200&spss=10 for the trace
	// list); ignoring them used to return every matching trace
	// (observed live: 4937 summaries / ~755KB body for limit=200).
	limit := positiveIntParam(r, "limit", DefaultSearchLimit)
	if limit > MaxSearchLimit {
		limit = MaxSearchLimit
	}
	spss := positiveIntParam(r, "spss", DefaultSpansPerSpanSet)
	// The request time window bounds the plain-search scan so /api/search
	// drains only the matching traces in [start, end] rather than the whole
	// table (the summaries-drain OOM). An invalid bound is a 400, matching
	// Tempo's own rejection of malformed start/end.
	start, end, err := parseTempoStartEnd(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "", "", err)
		return
	}
	// Bound a windowless search (hand-rolled q={} with no time range) so the
	// trace-limit pushdown's inner GROUP BY TraceId never aggregates over the
	// whole table server-side. Grafana's Traces Drilldown always sends both
	// bounds, so this only tightens the degenerate path. Mirrors reference
	// Tempo, which restricts a windowless search to recent data. A one-sided
	// window is a deliberate open-ended bound and is left as-is.
	if start.IsZero() && end.IsZero() {
		end = time.Now().UTC()
		start = end.Add(-DefaultSearchLookback)
	}

	ctx := r.Context()
	// Thread the response trace limit into lowering so the nested-set
	// numbering walk only numbers the traces this response will keep —
	// without it the structure-tab `select(nestedSet*)` query numbers
	// every matched trace and peaks past the per-query memory cap (#103).
	// No-op for queries that don't carry a nested-set select.
	ctx = traceql_lower.WithSearchTraceLimit(ctx, limit)
	// Thread the request window so stampSearchTraceLimit folds it into the
	// bounded plain-search scan. No-op when both bounds are absent.
	ctx = traceql_lower.WithSearchWindow(ctx, start, end)
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

	summaries, missingRoots := toTraceSummaries(res.Samples, spss)
	summaries, missingRoots = TruncateSummaries(summaries, missingRoots, limit)
	if len(missingRoots) > 0 {
		// The result set lacked a root row for some traces — typical
		// for structural-join queries (the join projects only matched
		// children) and filter predicates that never match the root
		// (`{ status = error }`, `{ kind = consumer }`). Issue a
		// follow-up lookup against otel_traces filtered to root spans
		// of the affected TraceIDs and patch RootServiceName /
		// RootTraceName before responding.
		//
		// A lookup failure WARN-degrades (the earliest-span fallback
		// stays in place) instead of failing the search — mirroring
		// reference Tempo, where root metadata is best-effort by
		// design: a search whose root span is unavailable still
		// returns 200 with placeholder root fields
		// (modules/frontend/combiner/search.go sets
		// search.RootSpanNotYetReceivedText), never an error.
		roots, lookupErr := h.resolveTraceRoots(ctx, missingRoots)
		if lookupErr != nil {
			h.Logger.Warn("cerberus tempo root-span lookup failed",
				"err", lookupErr, "missing", len(missingRoots))
		} else {
			applyRootMetadata(summaries, roots)
		}
	}
	writeEngineHeaders(w, res.Headers)
	writeJSON(w, http.StatusOK, SearchResponse{
		Traces:  summaries,
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
	// Sample-budget exceedance (CERBERUS_QUERY_MAX_SAMPLES) is an
	// over-broad query, not a transport failure — surface 422 like the
	// other "query is valid TraceQL but cannot be evaluated" rejections
	// instead of a breaker-adjacent 5xx. The error body carries the
	// chclient budget message including the configured limit.
	case errors.Is(err, chclient.ErrTooManySamples):
		return http.StatusUnprocessableEntity
	// ClickHouse memory-limit abort (code 241) — the server-side
	// sibling of the sample budget: a per-query resource rejection,
	// not a transport failure. 422 like the budget; the error body
	// carries the chclient message naming the configured cap.
	case errors.Is(err, chclient.ErrMemoryLimitExceeded):
		return http.StatusUnprocessableEntity
	// Wall-clock timeout (CERBERUS_QUERY_TIMEOUT → ClickHouse
	// max_execution_time, code 159, or the request ctx deadline): a
	// per-query cap doing its job, not a transport failure. 503 like the
	// breaker-open case so clients back off, rather than a generic 5xx —
	// ClickHouse is healthy when it aborts an over-long query (the
	// chclient breaker treats code 159 as a success for the same reason).
	case errors.Is(err, chclient.ErrQueryTimeout), errors.Is(err, context.DeadlineExceeded):
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

// search/recent page-size bounds: the Tempo Search UI's first-page
// trace list. defaultSearchRecentLimit is one screen's worth when the
// client sends no `?limit`; maxSearchRecentLimit caps a client-supplied
// limit so a single request can't drain an unbounded scan.
const (
	defaultSearchRecentLimit = 20
	maxSearchRecentLimit     = 200
)

// handleSearchRecent implements `GET /api/search/recent`. Returns the
// most-recent N traces (per the seeded Timestamp) without applying a
// TraceQL filter. Grafana's Tempo Search UI calls this on first page
// load to populate the trace list.
//
// Honors `?limit=N` (default 20, max 200); ignores `start` / `end` for
// now (the emitter doesn't thread them through OrderBy + Limit).
func (h *Handler) handleSearchRecent(w http.ResponseWriter, r *http.Request) {
	limit := int64(defaultSearchRecentLimit)
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			if n > maxSearchRecentLimit {
				n = maxSearchRecentLimit
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

	summaries, missingRoots := toTraceSummaries(res.Samples, DefaultSpansPerSpanSet)
	if len(missingRoots) > 0 {
		roots, lookupErr := h.resolveTraceRoots(ctx, missingRoots)
		if lookupErr != nil {
			h.Logger.Warn("cerberus tempo root-span lookup failed",
				"err", lookupErr, "missing", len(missingRoots))
		} else {
			applyRootMetadata(summaries, roots)
		}
	}
	writeEngineHeaders(w, res.Headers)
	writeJSON(w, http.StatusOK, SearchResponse{
		Traces:  summaries,
		Metrics: SearchMetrics{InspectedTraces: len(res.Samples)},
	})
}

// handleTraceByID serves `GET /api/traces/{id}` — the v1 wire shape:
// a bare *tempopb.Trace under Accept: protobuf, the flattened batches
// JSON otherwise. Mirrors upstream Tempo's
// `modules/frontend/combiner/trace_by_id.go` (proto.Marshal(trace) /
// tempopb.MarshalToJSONV1).
func (h *Handler) handleTraceByID(w http.ResponseWriter, r *http.Request) {
	h.serveTraceByID(w, r, false)
}

// handleTraceByIDV2 serves `GET /api/v2/traces/{id}` — the v2 wire
// shape: a tempopb.TraceByIDResponse ENVELOPE in both encodings.
// Mirrors upstream Tempo's
// `modules/frontend/combiner/trace_by_id_v2.go` finalize +
// `combiner/common.go` internalMarshalAs (proto.Marshal(envelope) /
// jsonpb.Marshaler{}.MarshalToString(envelope)).
func (h *Handler) handleTraceByIDV2(w http.ResponseWriter, r *http.Request) {
	h.serveTraceByID(w, r, true)
}

// serveTraceByID is the shared trace-by-id core. The lookup, trace-id
// validation, engine plan, and batch assembly are identical between
// the v1 and v2 endpoints; only the response envelope differs (v2
// wraps the trace in tempopb.TraceByIDResponse — see
// handleTraceByIDV2). The inner trace bytes stay deterministic and
// identical across both endpoints (groupTraceBatches owns the
// ordering contract), so the v1 body and the v2 envelope's `trace`
// field can never drift on content.
func (h *Handler) serveTraceByID(w http.ResponseWriter, r *http.Request, v2 bool) {
	traceID := r.PathValue("id")
	if traceID == "" {
		writeError(w, http.StatusBadRequest, "", "", fmt.Errorf("missing trace id"))
		return
	}

	// Reference Tempo's `/api/traces/{id}` rejects malformed IDs with
	// 400 (`invalid trace id`) before any storage lookup: only 16-char
	// (span-id-sized, retained for compatibility with older clients) or
	// 32-char hex strings are valid trace IDs. Without this gate, a
	// `curl /api/traces/ZZZZ` would surface as 404 ("trace not found")
	// and Grafana's Tempo plugin would render the wrong UX. Lower-case
	// up front so mixed-case input matches `^[0-9a-f]{16,32}$`.
	traceID = strings.ToLower(traceID)
	if !isValidTraceID(traceID) {
		writeError(w, http.StatusBadRequest, "", "", fmt.Errorf("invalid trace id"))
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

	// Grafana 11.x's Tempo datasource plugin sends
	// `Accept: application/protobuf` and proto.Unmarshal-s the response
	// body into a *tempopb.Trace; a JSON body surfaces on the Grafana
	// side as "proto: illegal wireType …" and the "click trace" UX
	// fails. Negotiate up-front so both the empty (404) and the
	// happy-path (200) branches emit the right wire format. See
	// handler_trace_proto.go for the Accept-header allow-list and the
	// proto sibling of groupBatches.
	proto := negotiateTraceByIDProto(r.Header.Get("Accept"))

	if len(res.Samples) == 0 {
		// Tempo's "trace not found" shape. Reference Tempo returns 404
		// on both endpoints (v1: the TraceByIDCombiner starts at 404
		// and stays there when every shard 404s; v2: the
		// genericCombiner relays the downstream 404 via
		// erroredResponse — see upstream
		// modules/frontend/combiner/{trace_by_id.go,common.go}).
		// Under Accept: protobuf we emit an empty message (an empty
		// *tempopb.Trace and an empty *tempopb.TraceByIDResponse both
		// marshal to zero bytes, so the two endpoints coincide here);
		// under Accept: json we keep Tempo's error envelope so Grafana
		// renders the "trace not found" UI.
		if proto {
			writeTraceProto(w, http.StatusNotFound, &tempopb.Trace{})
			return
		}
		writeJSON(w, http.StatusNotFound, ErrorResponse{
			TraceID: traceID, SpanID: "", Error: true,
			Message: fmt.Sprintf("trace not found: %s", traceID),
		})
		return
	}

	if v2 {
		// v2 envelope — tempopb.TraceByIDResponse{trace, metrics}.
		// Metrics is always non-nil with honest zero counters: the
		// reference frontend initialises the metrics combiner with
		// &TraceByIDMetrics{} and unconditionally assigns it in
		// finalize (modules/frontend/combiner/trace_by_id_v2.go +
		// response_metrics.go), so the field is present on the wire
		// even when no bytes-inspected accounting exists. Status stays
		// COMPLETE (zero) and Message empty: cerberus never truncates
		// a trace, so the PARTIAL path (upstream's maxBytes overflow)
		// is unreachable.
		writeTraceByIDV2(w, http.StatusOK, &tempopb.TraceByIDResponse{
			Trace:   groupBatchesProto(res.Samples),
			Metrics: &tempopb.TraceByIDMetrics{},
		}, proto)
		return
	}

	if proto {
		writeTraceProto(w, http.StatusOK, groupBatchesProto(res.Samples))
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
	// Sample-budget exceedance → 422, mirroring classifySearchErr; see
	// the rationale there.
	if errors.Is(err, chclient.ErrTooManySamples) {
		return http.StatusUnprocessableEntity
	}
	// CH memory-limit abort (code 241) → 422, mirroring
	// classifySearchErr; see the rationale there.
	if errors.Is(err, chclient.ErrMemoryLimitExceeded) {
		return http.StatusUnprocessableEntity
	}
	// Wall-clock timeout (code 159 / ctx deadline) → 503, mirroring
	// classifySearchErr; see the rationale there.
	if errors.Is(err, chclient.ErrQueryTimeout) || errors.Is(err, context.DeadlineExceeded) {
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
//
// The OTel-CH exporter writes TraceId as a 32-char lowercase hex string
// (see compatibility/tempo/driver/seeder.go: `hex.EncodeToString(s.TraceId)`),
// but Tempo's wire format strips leading zeros from trace IDs, and most
// Grafana installations forward the stripped form back on /api/traces/{id}.
// normaliseTraceID pads the inbound traceID back to the 32-char shape so
// the WHERE comparison matches the column value regardless of whether the
// caller sent the stripped or padded form.
func (h *Handler) lowerTraceByID(traceID string) (chplan.Node, error) {
	id := normaliseTraceID(traceID)
	pred := chplan.Expr(&chplan.Binary{
		Op:    chplan.OpEq,
		Left:  &chplan.ColumnRef{Name: h.Schema.TraceIDColumn},
		Right: &chplan.LitString{V: id},
	})
	if h.Schema.TraceIDTsEnabled {
		pred = &chplan.Binary{Op: chplan.OpAnd, Left: pred, Right: h.traceIDTsWindow(id)}
	}
	return &chplan.Filter{
		Input:     &chplan.Scan{Table: h.Schema.SpansTable},
		Predicate: pred,
	}, nil
}

// traceIDTsEndPadSeconds compensates the trace_id_ts MV storing
// End as a DateTime (1-second precision) computed as `max(Timestamp)`
// over spans whose Timestamp is DateTime64(9). The DateTime64→DateTime
// cast floors toward second granularity, so `End` is the second-truncated
// max; a naive `Timestamp <= End` upper bound would drop every span in the
// trace's final fractional second. Padding the bound by exactly one second
// makes it a strict superset of the true max Timestamp (the dropped
// sub-second carry is < 1s), so no in-trace span can fall above the window.
// The value is the DateTime-vs-DateTime64 granularity, not a tuning knob.
const traceIDTsEndPadSeconds = 1

// traceIDTsWindow builds the correctness-preserving Timestamp-window
// SUPERSET predicate read from the `<spans>_trace_id_ts` lookup MV for the
// already-normalised trace id. It AND-folds two bounds onto the spans scan
// so ClickHouse can Partition/PrimaryKey/MinMax-prune the spans table to
// the trace's day-partition + sort-key range instead of scanning every
// part to apply the bloom filter:
//
//	Timestamp >= (SELECT min(Start) FROM <ts> WHERE TraceId = id)
//	Timestamp <= addSeconds((SELECT max(End) FROM <ts> WHERE TraceId = id), 1)
//
// The lower bound is inherently safe: `Start = floor(min(Timestamp))`
// already floors DOWNWARD. The upper bound is padded (see
// traceIDTsEndPadSeconds). Each bound is a scalar subquery over a no-GROUP
// Aggregate, which projects exactly one column and one row — satisfying
// chplan.ScalarSubquery's contract. The exact `TraceId = id` Eq stays
// ANDed in lowerTraceByID, so even a stale/empty MV row only ever
// over-includes by the window, never under-includes by TraceId.
func (h *Handler) traceIDTsWindow(id string) chplan.Expr {
	scalar := func(agg, col string) chplan.Expr {
		return &chplan.ScalarSubquery{Input: &chplan.Aggregate{
			Input: &chplan.Filter{
				Input: &chplan.Scan{Table: h.Schema.TraceIDTsTable},
				Predicate: &chplan.Binary{
					Op:    chplan.OpEq,
					Left:  &chplan.ColumnRef{Name: h.Schema.TraceIDColumn},
					Right: &chplan.LitString{V: id},
				},
			},
			AggFuncs: []chplan.AggFunc{{
				Name:  agg,
				Args:  []chplan.Expr{&chplan.ColumnRef{Name: col}},
				Alias: col,
			}},
		}}
	}
	lower := &chplan.Binary{
		Op:    chplan.OpGe,
		Left:  &chplan.ColumnRef{Name: h.Schema.TimestampColumn},
		Right: scalar("min", h.Schema.TraceIDTsStartColumn),
	}
	upper := &chplan.Binary{
		Op:   chplan.OpLe,
		Left: &chplan.ColumnRef{Name: h.Schema.TimestampColumn},
		Right: &chplan.FuncCall{
			Name: "addSeconds",
			Args: []chplan.Expr{
				scalar("max", h.Schema.TraceIDTsEndColumn),
				&chplan.LitInt{V: traceIDTsEndPadSeconds},
			},
		},
	}
	return &chplan.Binary{Op: chplan.OpAnd, Left: lower, Right: upper}
}

// normaliseTraceID returns the canonical 32-char lowercase-hex form of
// the trace-id Tempo's wire format uses on storage (see OTel-CH
// exporter's `hex.EncodeToString`). Tempo's response shaper strips
// leading zeros (so `0000…ab` becomes `ab`), and Grafana echoes that
// stripped form back on /api/traces/{id}/. We restore the padding here
// so the WHERE comparison in lowerTraceByID hits the column value.
//
// Input shape is permissive: any length up to 32 hex chars, padded with
// leading zeros on the left; anything longer is returned unchanged so a
// caller's already-canonical 32-char form round-trips byte-for-byte and
// a malformed/longer id surfaces via the downstream CH not-found path
// rather than getting silently rewritten here. Lowercases the input so
// callers that uppercased their hex still resolve.
func normaliseTraceID(s string) string {
	if len(s) >= 32 {
		return strings.ToLower(s)
	}
	return strings.Repeat("0", 32-len(s)) + strings.ToLower(s)
}

// stripLeadingHexZeros historically emitted `replaceRegexpOne(col,
// '^0+([0-9a-f])', '\\1')` to mirror reference Tempo's habit of
// stripping leading zeros from trace IDs and span IDs on the wire.
// That behaviour violates the OTel / Tempo spec: a TraceId is a
// fixed-width 16-byte value and its hex representation MUST be 32
// lowercase-hex chars; a SpanId is 8 bytes / 16 hex chars. Reference
// Tempo's own zero-stripping was a long-standing wire-format defect
// that clients had to work around — cerberus shouldn't propagate it
// (issue #209).
//
// The function is retained as a passthrough so every existing call
// site keeps compiling without churn. The OTel-CH exporter writes
// TraceId / SpanId / ParentSpanId as canonical fixed-width lowercase-
// hex strings (`hex.EncodeToString`), so returning the bare column
// ref yields the spec-compliant 32-/16-char form verbatim on the wire.
//
// Input parsing on the `/api/traces/{id}` path still accepts both the
// padded and the leading-zero-stripped form (see normaliseTraceID) so
// clients that round-trip via legacy reference-Tempo wire payloads
// continue to resolve.
func stripLeadingHexZeros(col string) chplan.Expr {
	return &chplan.ColumnRef{Name: col}
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
//
// searchKeyParentSpanID reuses the same `__cerberus_parentSpanID` slot
// on the /api/search path so toTraceSummaries can identify the per-trace
// root span — the row where ParentSpanId is empty (legacy / stub
// queriers) or `"0"` (the post-strip form of `0000000000000000`, the
// on-disk shape for a true root span; stripLeadingHexZeros always
// retains ≥ 1 hex digit). Without this, the shaper anchors
// RootServiceName / RootTraceName on whichever span CH happens to
// return first, which for multi-span traces is typically a child span
// (Tempo's wire spec says rootTraceName is the name of the span at the
// top of the trace tree). See toTraceSummaries for the resolution
// logic and the fallback rules for broken or truncated traces.
const (
	traceByIDKeyTraceID       = "__cerberus_traceID"
	traceByIDKeySpanID        = "__cerberus_spanID"
	traceByIDKeyParentSpanID  = "__cerberus_parentSpanID"
	traceByIDKeySpanKind      = "__cerberus_spanKind"
	traceByIDKeyStatusCode    = "__cerberus_statusCode"
	traceByIDKeySpanAttrsJSON = "__cerberus_spanAttrsJSON"
	// traceByIDKeyScopeName / traceByIDKeyScopeVersion carry the
	// instrumentation-scope identity columns (ScopeName /
	// ScopeVersion in the OTel-CH schema). groupBatchesProto buckets
	// spans into one ScopeSpans per distinct (name, version) pair and
	// emits a non-nil *commonv1.InstrumentationScope — Grafana 12's
	// server-side trace transform dereferences ils.Scope.Name with no
	// nil check (pkg/tsdb/tempo/trace_transform.go:137), so a nil
	// Scope panics the whole /api/ds/query trace-detail request.
	// Reference Tempo always rehydrates a non-nil scope
	// (tempodb/encoding/vparquet4/schema.go
	// parquetToProtoInstrumentationScope), so cerberus matching that
	// is also the OTLP-correct shape.
	traceByIDKeyScopeName    = "__cerberus_scopeName"
	traceByIDKeyScopeVersion = "__cerberus_scopeVersion"

	// searchKeyTraceID is the reserved Labels key that carries the
	// hex-encoded TraceId on /api/search responses. Same constant value
	// as traceByIDKeyTraceID — the two paths never overlap (search keeps
	// the slot for the trace-id, trace-by-id keeps it for the same).
	searchKeyTraceID = traceByIDKeyTraceID
	// searchKeyParentSpanID is the reserved Labels key carrying the
	// hex-encoded ParentSpanId on /api/search responses. Reuses the
	// traceByIDKeyParentSpanID slot — same constant value, same
	// namespace; the two paths never overlap.
	searchKeyParentSpanID = traceByIDKeyParentSpanID
	// searchKeySpanID is the reserved Labels key carrying the
	// hex-encoded SpanId on /api/search responses. Reuses the
	// traceByIDKeySpanID slot. toTraceSummaries groups the matched
	// rows into per-trace SpanSets from it — Tempo's wire spec
	// (tempopb.TraceSearchMetadata.spanSets) lists each matched span's
	// spanID, and Grafana's tableType='spans' transform renders zero
	// rows for a summary without them.
	searchKeySpanID = traceByIDKeySpanID
	// searchKeyTraceDurationNs is the reserved Labels key carrying
	// the **whole-trace** wall-clock duration in nanoseconds. The
	// spanset-aggregate wrap-projection populates it as
	// `TraceEndNs - TraceStartNs` (see
	// spansetAggregateSampleProjections); toTraceSummaries reads it
	// and surfaces durationMs = ns / 1e6 so the response matches
	// Tempo's wire spec, which reports the trace-wide span — not the
	// matched row's own Duration column. Non-aggregate paths leave
	// the slot empty and fall back to chclient.Sample.Value (the
	// per-row Duration), preserving the historical shape.
	searchKeyTraceDurationNs = "__cerberus_traceDurationNs"

	// searchKeySelIntPrefix / searchKeySelStrPrefix carry user-selected
	// `| select(...)` attribute values through the canonical Labels map
	// on the /api/search path. The wrap projection
	// (selectedAttrKVPairs) emits one `<prefix><attrKey>` entry per
	// selected attribute; toTraceSummaries / observeSpan strip the
	// prefix and surface the value inside SpanSetSpan.Attributes as the
	// OTLP AnyValue type the prefix names — intValue for the nested-set
	// intrinsics (proto3 JSON renders int64 as a decimal string),
	// stringValue for everything else (the OTel-CH Map(String,String)
	// carriers erase the original attribute type; reference Tempo's
	// typed parquet columns don't — a pre-existing schema-level
	// divergence for non-string span attributes).
	searchKeySelIntPrefix = "__cerberus_sel_int_"
	searchKeySelStrPrefix = "__cerberus_sel_str_"
	// searchKeySelName carries the span name when `| select(name)`
	// requested it. Reference Tempo populates tempopb.Span.Name (NOT an
	// attribute) for selected names — pkg/traceql/engine.go
	// asTraceSearchMetadata reads the IntrinsicName attr into span.name
	// and skips it from the attribute list — so the shaper mirrors that
	// placement.
	searchKeySelName = "__cerberus_sel_name"
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
	case isSpansetAggregateShape(plan):
		// `| count()` / `| avg()` / `| sum()` / `| min()` / `| max()`
		// — second-stage spanset aggregates lowered by
		// internal/traceql/aggregate.go. The Aggregate groups by
		// TraceId so the per-trace search envelope is preserved (one
		// row per matching trace). lowerAggregate piggybacks the
		// envelope columns onto the AggFuncs list (`any(SpanName) AS
		// MetricName`, `any(ResourceAttributes) AS ResourceAttrs`,
		// `min(Timestamp) AS TimeUnix`) so the wrap projection just
		// reads them out + merges the TraceId into Attributes via
		// mapConcat (same `__cerberus_traceID` reserved-key pattern as
		// canonicalSampleProjections).
		return &chplan.Project{Input: plan, Projections: spansetAggregateSampleProjections()}
	case isAggregateShape(plan):
		// MetricsAggregate / MetricsSecondStage output is just
		// (group-keys, agg-func-aliases). SpanName / Timestamp /
		// Duration aren't in scope; synthesise the missing pieces.
		// The aggregate's alias is "Value" by convention. The metrics
		// paths bypass wrapWithSampleProjection (see
		// metrics_query_range.go / metrics_query_instant.go), so this
		// branch only fires when a metrics-pipeline query somehow
		// lands on /api/search; the empty-MetricName synthesis keeps
		// the response shape sane.
		return &chplan.Project{Input: plan, Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: "MetricName"},
			{Expr: emptyAttrsMap(), Alias: "Attributes"},
			{Expr: chplan.NowNano(), Alias: "TimeUnix"},
			{Expr: &chplan.FuncCall{Name: "toFloat64", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}}, Alias: "Value"},
		}}
	case isProjectShape(plan):
		// `| select(...)` lowering produces Project(Filter?(Scan)) —
		// or Project(NestedSetAnnotate(...)) when nested-set intrinsics
		// were selected — with a user-defined column list. The HTTP
		// search envelope needs the canonical Sample columns PLUS the
		// user-selected attribute values (reference Tempo surfaces them
		// inside spanSets[].spans[].attributes; Grafana's Traces
		// Drilldown structure tab hard-fails without nestedSetLeft /
		// nestedSetRight there). Replace the user Project with the
		// canonical one rooted in the same input, smuggling each
		// selected value through a reserved `__cerberus_sel_*` Labels
		// key that toTraceSummaries pivots into SpanSetSpan.Attributes.
		p := plan.(*chplan.Project)
		return &chplan.Project{
			Input:       p.Input,
			Projections: sampleProjectionsWithSelected(s, selectedAttrKVPairs(p, s)),
		}
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
// The Attributes map merges ResourceAttributes with two reserved-key
// entries:
//   - `__cerberus_traceID` → leading-zero-stripped hex TraceId — so
//     toTraceSummaries can group spans into per-trace summaries by real
//     TraceID rather than synthesising a key from (SpanName + Timestamp).
//   - `__cerberus_parentSpanID` → leading-zero-stripped hex ParentSpanId
//     — so toTraceSummaries can resolve the root span per trace (Tempo's
//     wire spec anchors rootServiceName / rootTraceName on the span where
//     ParentSpanId is empty, not the first span returned by the
//     underlying engine).
//
// Same mapConcat pattern as traceByIDProjections; the resource keys
// (`service.*`, `k8s.*`, …) never collide with the `__cerberus_*`
// namespace so no precedence surprises.
func canonicalSampleProjections(s schema.Traces) []chplan.Projection {
	return sampleProjectionsWithSelected(s, nil)
}

// sampleProjectionsWithSelected is canonicalSampleProjections with
// extra (LitString key, String-typed value expr) pairs appended to the
// reserved map — the `__cerberus_sel_*` entries that carry user-
// selected `| select(...)` attribute values to the response shaper.
func sampleProjectionsWithSelected(s schema.Traces, selectedKVs []chplan.Expr) []chplan.Projection {
	mapArgs := []chplan.Expr{
		&chplan.LitString{V: searchKeyTraceID},
		stripLeadingHexZeros(s.TraceIDColumn),
		&chplan.LitString{V: searchKeyParentSpanID},
		stripLeadingHexZeros(s.ParentSpanIDColumn),
		&chplan.LitString{V: searchKeySpanID},
		stripLeadingHexZeros(s.SpanIDColumn),
	}
	mapArgs = append(mapArgs, selectedKVs...)
	reservedMap := &chplan.FuncCall{Name: "map", Args: mapArgs}
	mergedAttrs := &chplan.FuncCall{Name: "mapConcat", Args: []chplan.Expr{
		&chplan.ColumnRef{Name: s.ResourceAttributesColumn},
		reservedMap,
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

// selectedAttrKVPairs converts a user `| select(...)` Project's
// aliased projections into reserved-key (LitString, value-expr) pairs
// for the canonical Attributes map. The identity projections lowerSelect
// prepends (TraceId / SpanId / Timestamp) carry no alias and are
// skipped; each aliased projection is classified by its lowered
// expression shape:
//
//   - nested-set synthetic columns → `__cerberus_sel_int_<name>` with
//     the Int64 value stringified (the shaper re-types it as an OTLP
//     intValue, matching reference Tempo's TypeInt statics).
//   - SpanName → `__cerberus_sel_name` (populates SpanSetSpan.Name,
//     reference Tempo's placement for selected names).
//   - StatusCode / SpanKind → `__cerberus_sel_str_<name>` lowercased:
//     OTel-CH stores the TitleCase enum words ("Error", "Server")
//     while reference Tempo's wire encoding is lowercase ("error",
//     "server" — traceql.Static.EncodeToString).
//   - StatusMessage / ScopeName / ScopeVersion → plain string entries.
//   - Map lookups (span./resource./instrumentation-scoped attributes)
//     → plain string entries under the attribute key (reference uses
//     the scope-less Attribute.Name as the wire key too).
//   - TraceId / SpanId / Duration / Timestamp / ParentSpanId
//     projections are dropped: reference Tempo skips those intrinsics
//     in the per-span attribute list (engine.go asTraceSearchMetadata)
//     because they already ride dedicated SpanSetSpan fields.
func selectedAttrKVPairs(p *chplan.Project, s schema.Traces) []chplan.Expr {
	var kvs []chplan.Expr
	for _, pr := range p.Projections {
		if pr.Alias == "" {
			continue
		}
		key, val, ok := classifySelectedProjection(pr, s)
		if !ok {
			continue
		}
		kvs = append(kvs, &chplan.LitString{V: key}, val)
	}
	return kvs
}

// classifySelectedProjection maps one aliased select() projection onto
// its reserved Labels key + String-typed value expression. ok=false
// means the projection has no per-span attribute representation (see
// selectedAttrKVPairs for the rationale per shape).
func classifySelectedProjection(pr chplan.Projection, s schema.Traces) (string, chplan.Expr, bool) {
	toStr := func(e chplan.Expr) chplan.Expr {
		return &chplan.FuncCall{Name: "toString", Args: []chplan.Expr{e}}
	}
	switch e := pr.Expr.(type) {
	case *chplan.ColumnRef:
		switch e.Name {
		case chplan.NestedSetLeftColumn, chplan.NestedSetRightColumn, chplan.NestedSetParentColumn:
			return searchKeySelIntPrefix + pr.Alias, toStr(e), true
		case s.SpanNameColumn:
			return searchKeySelName, toStr(e), true
		case s.StatusCodeColumn, s.SpanKindColumn:
			return searchKeySelStrPrefix + pr.Alias,
				toStr(&chplan.FuncCall{Name: "lower", Args: []chplan.Expr{e}}), true
		case s.StatusMessageColumn, s.ScopeNameColumn, s.ScopeVersionColumn:
			return searchKeySelStrPrefix + pr.Alias, toStr(e), true
		}
		return "", nil, false
	case *chplan.FieldAccess:
		return searchKeySelStrPrefix + pr.Alias, e, true
	}
	return "", nil, false
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
	reservedMap := &chplan.FuncCall{Name: "map", Args: []chplan.Expr{
		&chplan.LitString{V: searchKeyTraceID},
		stripLeadingHexZeros(s.TraceIDColumn),
		&chplan.LitString{V: searchKeyParentSpanID},
		stripLeadingHexZeros(s.ParentSpanIDColumn),
		&chplan.LitString{V: searchKeySpanID},
		stripLeadingHexZeros(s.SpanIDColumn),
	}}
	mergedAttrs := &chplan.FuncCall{Name: "mapConcat", Args: []chplan.Expr{
		&chplan.ColumnRef{Name: s.ResourceAttributesColumn},
		reservedMap,
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
		stripLeadingHexZeros(s.TraceIDColumn),
		&chplan.LitString{V: traceByIDKeySpanID},
		stripLeadingHexZeros(s.SpanIDColumn),
		&chplan.LitString{V: traceByIDKeyParentSpanID},
		stripLeadingHexZeros(s.ParentSpanIDColumn),
		&chplan.LitString{V: traceByIDKeySpanKind},
		// SpanKind / StatusCode are LowCardinality(String); cast to
		// String so the mapConcat homogeneous-value-type requirement
		// is met across the merged Map(String,String).
		&chplan.FuncCall{Name: "toString", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.SpanKindColumn}}},
		&chplan.LitString{V: traceByIDKeyStatusCode},
		&chplan.FuncCall{Name: "toString", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.StatusCodeColumn}}},
		&chplan.LitString{V: traceByIDKeyScopeName},
		// ScopeName / ScopeVersion are String in the upstream OTel-CH
		// traces DDL, but custom schemas may declare them
		// LowCardinality(String); toString keeps the map() literal's
		// value type homogeneous either way (same rationale as
		// SpanKind / StatusCode above).
		&chplan.FuncCall{Name: "toString", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ScopeNameColumn}}},
		&chplan.LitString{V: traceByIDKeyScopeVersion},
		&chplan.FuncCall{Name: "toString", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ScopeVersionColumn}}},
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

// isSpansetAggregateShape reports whether the plan's root is the
// per-trace spanset aggregate shape produced by
// internal/traceql/aggregate.go: a *chplan.Aggregate whose AggFuncs
// list carries the envelope-column aliases (MetricName /
// ResourceAttrs / TimeUnix) alongside Value. The scalar-filter HAVING
// wrap (`| count() > 0`) puts a Filter on top — recurse through
// Filter so both `| count()` and `| count() > 0` hit this branch.
// MetricsAggregate / MetricsSecondStage (metrics-pipeline output)
// deliberately fall through; the metrics search-envelope shape is
// distinct (no TraceId group key, no `__cerberus_traceID` thread).
func isSpansetAggregateShape(plan chplan.Node) bool {
	switch v := plan.(type) {
	case *chplan.Aggregate:
		return aggregateCarriesSpansetEnvelope(v)
	case *chplan.Filter:
		return isSpansetAggregateShape(v.Input)
	}
	return false
}

// aggregateCarriesSpansetEnvelope reports whether a *chplan.Aggregate
// has the envelope-column aliases (MetricName, ResourceAttrs,
// TimeUnix) in its AggFuncs list — the marker that
// internal/traceql/aggregate.go's lowerAggregate produced it (vs e.g.
// a hand-rolled aggregate from a future call-site). The check keys
// off alias presence rather than a structural type so the wrap
// projection stays decoupled from the lowering layer.
func aggregateCarriesSpansetEnvelope(a *chplan.Aggregate) bool {
	wanted := map[string]bool{
		"MetricName":    false,
		"ResourceAttrs": false,
		"TimeUnix":      false,
	}
	for _, af := range a.AggFuncs {
		if _, ok := wanted[af.Alias]; ok {
			wanted[af.Alias] = true
		}
	}
	for _, found := range wanted {
		if !found {
			return false
		}
	}
	return true
}

// spansetAggregateSampleProjections returns the wrap-projection used
// when the plan root is the per-trace spanset Aggregate shape
// produced by internal/traceql/aggregate.go. The inner Aggregate
// already emits the per-trace envelope columns (MetricName,
// ResourceAttrs, TimeUnix) alongside (TraceId, Value, TraceStartNs,
// TraceEndNs); this outer Project merges TraceId into Attributes via
// the `__cerberus_traceID` reserved-key pattern, threads the derived
// whole-trace duration `(TraceEndNs - TraceStartNs)` via
// `__cerberus_traceDurationNs`, and casts Value to float64 — same
// wire shape as canonicalSampleProjections but reading from the
// Aggregate's projected columns rather than the raw spans-table
// columns.
//
// The `TraceId` column the inner Aggregate exposes is the raw
// fixed-width OTel-CH value (32 lowercase-hex chars); Tempo's wire
// format strips leading zeros, so we route it through
// stripLeadingHexZeros — same treatment canonicalSampleProjections
// applies to the scan-level TraceId column. Without the strip, the
// compat differ pairs cerberus rows by `00af…66b` against Tempo's
// `af…66b`, generating spurious missing_in_a / missing_in_b reasons
// across the spanset-aggregate compat cases (e.g.
// `avg_duration_per_trace_status_ok`).
//
// Tempo's /api/search wire spec reports `durationMs` as the
// **whole-trace** wall-clock span (max-end minus min-start across
// every span in the trace), not the matched span's per-row Duration.
// Surfacing the derived `TraceEndNs - TraceStartNs` via the reserved
// `__cerberus_traceDurationNs` slot lets toTraceSummaries report the
// trace-wide duration while keeping the non-aggregate path's
// per-row-Duration semantics intact (the slot is absent there, so
// the shaper falls back to Sample.Value).
func spansetAggregateSampleProjections() []chplan.Projection {
	traceDurationNs := &chplan.Binary{
		Op:    chplan.OpSub,
		Left:  &chplan.ColumnRef{Name: "TraceEndNs"},
		Right: &chplan.ColumnRef{Name: "TraceStartNs"},
	}
	traceIDMap := &chplan.FuncCall{Name: "map", Args: []chplan.Expr{
		&chplan.LitString{V: searchKeyTraceID},
		stripLeadingHexZeros("TraceId"),
		&chplan.LitString{V: searchKeyTraceDurationNs},
		// toString keeps the merged Map(String,String) homogeneous;
		// the shaper parses the int back out on the Go side.
		&chplan.FuncCall{Name: "toString", Args: []chplan.Expr{traceDurationNs}},
	}}
	mergedAttrs := &chplan.FuncCall{Name: "mapConcat", Args: []chplan.Expr{
		&chplan.ColumnRef{Name: "ResourceAttrs"},
		traceIDMap,
	}}
	return []chplan.Projection{
		{Expr: &chplan.ColumnRef{Name: "MetricName"}, Alias: "MetricName"},
		{Expr: mergedAttrs, Alias: "Attributes"},
		{Expr: &chplan.ColumnRef{Name: "TimeUnix"}, Alias: "TimeUnix"},
		{Expr: &chplan.FuncCall{Name: "toFloat64", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}}, Alias: "Value"},
	}
}

// toTraceSummaries pivots samples into the per-trace summary shape
// Tempo's /api/search returns. Each unique TraceID becomes one
// summary; StartTimeUnixNano is the earliest span timestamp seen and
// DurationMs reports the **whole-trace** wall-clock span (max-end
// minus min-start across every span in the trace) for the
// spanset-aggregate path, falling back to the max per-row Sample.Value
// when the reserved `__cerberus_traceDurationNs` slot is absent (non-
// aggregate /api/search rows, older fixtures, stub queriers).
//
// chclient.Sample's MetricName carries SpanName here (per the wrap
// projection above) and Attributes carries ResourceAttributes plus
// three reserved entries:
//   - `__cerberus_traceID` (searchKeyTraceID) — the grouping key so
//     multi-span traces collapse into one summary row.
//   - `__cerberus_parentSpanID` (searchKeyParentSpanID) — used to
//     identify the per-trace root span (Tempo's wire spec anchors
//     rootServiceName / rootTraceName on the span at the top of the
//     trace tree, where ParentSpanId is empty/unset, not whichever
//     span the underlying engine happens to return first).
//   - `__cerberus_traceDurationNs` (searchKeyTraceDurationNs) — the
//     derived whole-trace duration (`max(span.end) - min(span.start)`)
//     in nanoseconds, populated by spansetAggregateSampleProjections.
//     Matches Tempo's /api/search wire spec for durationMs: a
//     trace-wide span, not the matched row's own Duration.
//
// Root-span resolution:
//   - Prefer the row where ParentSpanId == "" (the actual root). Among
//     multiple roots (broken trace), the earliest by start time wins —
//     same fallback Tempo uses internally.
//   - When no row in the matched set is a root (truncated trace; the
//     matcher only hit children, the common case for structural-join
//     queries and `{ status = error }` against fixtures where only
//     child spans satisfy the predicate), the second return value
//     surfaces the missing-root TraceIDs so the caller can issue a
//     follow-up root-lookup query (see resolveTraceRoots) and patch
//     RootServiceName / RootTraceName before responding. Until that
//     follow-up resolves, the summary anchors on the earliest-span
//     fallback so a response is always emitted.
//
// Defensive: samples missing the reserved trace-ID key (older fixtures,
// stub queriers in tests) fall back to (SpanName | Timestamp) so
// partial data still surfaces a row rather than silently dropping.
// Samples missing the parent-span-id key default to a non-root
// classification (since we can't tell — older fixtures pre-date the
// reserved slot); the trace's RootService/RootTraceName then anchor
// on the earliest-span fallback path.
//
// SpanSets: rows that carry the reserved `__cerberus_spanID` slot
// (populated by canonicalSampleProjections / rQualifiedSampleProjections)
// are additionally collected into one SpanSet per trace — Tempo's wire
// spec lists each matched span (spanID, name, startTimeUnixNano,
// durationNanos) and Grafana's tableType='spans' transform (the Traces
// Drilldown trace list) renders rows exclusively from
// trace.spanSets[].spans. The set is capped at `spss` spans (Tempo's
// spans-per-spanset request param, default 3) with Matched carrying the
// uncapped total; the kept spans are the earliest by start time
// (spanID tie-break) so the cap is deterministic. Rows without the
// spanID slot (legacy stub queriers, spanset-aggregate projections that
// collapse spans into one row per trace) contribute no SpanSet and the
// summary omits the fields — same wire shape as before.
//
// Output ordering matches Tempo's /api/search: traces sort by
// startTimeUnixNano descending (newest first), TraceID ascending as
// the deterministic tie-break. Callers enforce the request's `limit`
// by truncating the returned slice.
//
// Returns (summaries, missingRootTraceIDs). `missingRootTraceIDs`
// contains the stripped-zero-form TraceID of every trace whose result
// set lacked a true root span — the structural-join SQL only returns
// the join's right-side rows (no root row by construction), and many
// filter queries (`{ status = error }`, `{ kind = consumer }`) only
// match child spans. resolveTraceRoots fetches the real root from
// otel_traces and patches the affected summaries.
func toTraceSummaries(samples []chclient.Sample, spss int) ([]TraceSummary, []string) {
	if spss <= 0 {
		spss = DefaultSpansPerSpanSet
	}
	byTrace := map[string]*summaryAcc{}
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
			a = &summaryAcc{traceID: traceID}
			byTrace[key] = a
		}
		ns := s.Timestamp.UnixNano()
		if a.startNS == 0 || ns < a.startNS {
			a.startNS = ns
		}
		a.observeDuration(s)
		a.observeSpan(s, ns)
		a.observeRoot(s, ns)
	}
	out := make([]TraceSummary, 0, len(byTrace))
	var missing []string
	for k, a := range byTrace {
		// Emit the real TraceID when the projection supplied one;
		// otherwise surface the synthetic key (back-compat for stub
		// queriers + older fixtures that never threaded it through).
		tid := a.traceID
		if tid == "" {
			tid = k
		}
		summary := TraceSummary{
			TraceID:           tid,
			RootServiceName:   a.serviceName,
			RootTraceName:     a.traceName,
			StartTimeUnixNano: strconv.FormatInt(a.startNS, 10),
			DurationMs:        int(a.durationNS / 1_000_000),
		}
		if set := a.buildSpanSet(spss); set != nil {
			// Both fields carry the same set: spanSets is what Grafana's
			// tableType='spans' transform consumes, spanSet is the legacy
			// single-set field Tempo still emits for older readers.
			summary.SpanSets = []SpanSet{*set}
			summary.SpanSet = set
		}
		out = append(out, summary)
		// Only flag traces where we actually have a real TraceID, the
		// projection populated the __cerberus_parentSpanID slot for at
		// least one row (so we can trust "no root present"), and no
		// row in the result set was a root span. The synthetic-key
		// fallback path (no real TraceID) can't be looked up; the
		// no-parent-slot path is the legacy fixture / stub-querier
		// shape and stays on the earliest-span anchor.
		if a.traceID != "" && a.sawParentSlot && !a.hasRoot {
			missing = append(missing, a.traceID)
		}
	}
	sortSummariesStartDesc(out)
	sort.Strings(missing)
	return out, missing
}

// summaryAcc accumulates one trace's worth of /api/search rows while
// toTraceSummaries groups the flat sample stream by TraceID.
type summaryAcc struct {
	traceID     string
	serviceName string
	traceName   string
	startNS     int64
	durationNS  int64
	// rootStartNS pins the timestamp of the best root-span candidate
	// seen so far (smallest start time wins ties). 0 means "no
	// root span seen yet"; we anchor on the earliest-span fallback
	// in that case.
	rootStartNS int64
	// earliestStartNS pins the earliest-by-timestamp span seen,
	// regardless of root status. Used as the fallback anchor for
	// truncated traces where no root is in the matched set.
	earliestStartNS int64
	// hasRoot is true once we've seen a span with ParentSpanId == "".
	hasRoot bool
	// sawParentSlot is true once any of the trace's rows supplied
	// the reserved `__cerberus_parentSpanID` slot. When false the
	// shaper can't distinguish root from child (e.g. legacy fixtures
	// + stub queriers that never populate the slot), so the
	// missing-root follow-up lookup is suppressed and the
	// earliest-span fallback handles the trace alone.
	sawParentSlot bool
	// hasTraceDurationNs is set the first time a row supplies the
	// `__cerberus_traceDurationNs` reserved-key entry. Once true,
	// `durationNS` carries the trace-wide span and per-row Sample.
	// Value contributions are ignored — they would otherwise pull
	// the value back to the max single-span duration.
	hasTraceDurationNs bool
	// spans collects the matched-span rows (those carrying the
	// reserved __cerberus_spanID slot) for the trace's SpanSet.
	// Uncapped during accumulation; buildSpanSet sorts by
	// (startTimeUnixNano, spanID) and truncates to spss.
	spans []SpanSetSpan
	// matched counts every matched-span row, including the ones
	// the spss cap drops — surfaces as SpanSet.Matched.
	matched int
}

// observeDuration folds one row into the trace-wide duration. The
// whole-trace duration takes precedence when the wrap-projection
// surfaced one (`__cerberus_traceDurationNs`). The spanset-aggregate
// path emits a single row per trace so we just overwrite; the guard
// against per-row-Duration fallback ensures a mixed-shape stream
// (older fixture rows + aggregate rows on the same trace) doesn't pull
// the value down to a single span's max.
func (a *summaryAcc) observeDuration(s chclient.Sample) {
	if v, ok := s.Labels[searchKeyTraceDurationNs]; ok && v != "" {
		if ns, err := strconv.ParseInt(v, 10, 64); err == nil && ns >= 0 {
			a.durationNS = ns
			a.hasTraceDurationNs = true
		}
		return
	}
	if !a.hasTraceDurationNs && int64(s.Value) > a.durationNS {
		a.durationNS = int64(s.Value)
	}
}

// observeSpan collects the matched span for the trace's SpanSet. Only
// rows that carry the reserved __cerberus_spanID slot qualify —
// spanset-aggregate rows (one collapsed row per trace) and legacy
// stub-querier rows don't, and their summaries omit spanSets entirely
// rather than fabricating a span list.
//
// Name stays unset on plain spanset-filter queries: reference Tempo
// emits `name: ""` there (verified live against grafana/tempo by the
// compatibility differ — populating it from SpanName produced a
// per-span field_mismatch on every matched span). The one exception
// mirrors reference too: `| select(name)` carries the span name in the
// reserved searchKeySelName slot and reference Tempo populates
// tempopb.Span.Name from the selected IntrinsicName attribute
// (pkg/traceql/engine.go asTraceSearchMetadata).
//
// Attributes carries the user-selected `| select(...)` values smuggled
// through the `__cerberus_sel_*` reserved Labels keys — Tempo's wire
// spec puts them in spanSets[].spans[].attributes and Grafana's Traces
// Drilldown structure tab hard-fails (`nestedSetLeft not found!`)
// when its selected nested-set bounds are missing.
func (a *summaryAcc) observeSpan(s chclient.Sample, ns int64) {
	spanID, ok := s.Labels[searchKeySpanID]
	if !ok || spanID == "" {
		return
	}
	a.matched++
	// DurationNanos: proto3 JSON encodes the uint64 as a decimal
	// string and omits zero values; Sample.Value carries the per-row
	// Duration column in nanoseconds on this path.
	var durationNanos string
	if d := int64(s.Value); d > 0 {
		durationNanos = strconv.FormatInt(d, 10)
	}
	a.spans = append(a.spans, SpanSetSpan{
		SpanID:            spanID,
		Name:              s.Labels[searchKeySelName],
		StartTimeUnixNano: strconv.FormatInt(ns, 10),
		DurationNanos:     durationNanos,
		Attributes:        selectedSpanAttributes(s.Labels),
	})
}

// selectedSpanAttributes pivots the reserved `__cerberus_sel_int_*` /
// `__cerberus_sel_str_*` Labels entries into the OTLP KeyValue list
// for one SpanSetSpan. Int entries always surface (the nested-set
// synthetic columns default to 0 for unnumbered spans, and reference
// Tempo returns those zeros verbatim when the intrinsics are
// explicitly selected); string entries with an empty value are dropped
// — the OTel-CH Map(String,String) subscript returns ” for absent
// keys, and reference Tempo simply omits attributes the span doesn't
// carry. Keys sort alphabetically for deterministic output (reference
// Tempo's order is Go-map iteration order, i.e. unspecified).
func selectedSpanAttributes(labels map[string]string) []KeyValue {
	var out []KeyValue
	for k, v := range labels {
		switch {
		case strings.HasPrefix(k, searchKeySelIntPrefix):
			val := v
			out = append(out, KeyValue{
				Key:   strings.TrimPrefix(k, searchKeySelIntPrefix),
				Value: AnyValue{IntValue: &val},
			})
		case strings.HasPrefix(k, searchKeySelStrPrefix):
			if v == "" {
				continue
			}
			val := v
			out = append(out, KeyValue{
				Key:   strings.TrimPrefix(k, searchKeySelStrPrefix),
				Value: AnyValue{StringValue: &val},
			})
		}
	}
	if len(out) == 0 {
		return nil
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// observeRoot resolves root-span metadata for one row. The reserved
// __cerberus_parentSpanID slot carries the parent ID; an empty value
// (`""`), a single `"0"`, or the full 16-zero hex string
// (`"0000000000000000"`) all mean this row is the root span. The
// OTel-CH exporter writes ParentSpanId as a 16-char lowercase-hex
// string; the canonical/r-qualified projections now surface that
// column verbatim (post-#209 fix: no more leading-zero stripping on
// the OUTPUT side), so a true root span arrives as the full 16-char
// zero hex. The `""` / `"0"` forms are accepted for back-compat with
// legacy fixtures, stub queriers, and the pre-fix stripped projection
// variant respectively.
//
// When the slot is missing (older fixtures / stub queriers), treat
// every row as a non-root candidate so the fallback path runs —
// `parentID, hasParentSlot := ...` captures the "did we get the
// projection" signal independently of the emptiness check.
func (a *summaryAcc) observeRoot(s chclient.Sample, ns int64) {
	parentID, hasParentSlot := s.Labels[searchKeyParentSpanID]
	if hasParentSlot {
		a.sawParentSlot = true
	}
	isRoot := hasParentSlot && (parentID == "" || parentID == "0" || parentID == "0000000000000000")
	svc := s.Labels["service.name"]
	switch {
	case isRoot && (!a.hasRoot || ns < a.rootStartNS):
		// First root, or an earlier root (broken trace with
		// multiple ParentSpanId="" spans — Tempo picks the
		// earliest by start time, we mirror that).
		a.hasRoot = true
		a.rootStartNS = ns
		a.serviceName = svc
		a.traceName = s.MetricName
	case !a.hasRoot && (a.earliestStartNS == 0 || ns < a.earliestStartNS):
		// No root seen yet (truncated trace) — anchor on the
		// earliest-by-timestamp child as a graceful fallback.
		a.earliestStartNS = ns
		a.serviceName = svc
		a.traceName = s.MetricName
	}
}

// buildSpanSet materialises the trace's SpanSet: a deterministic spss
// cap keeps the earliest spans by start time, spanID as the
// total-order tie-break, so two runs over the same data (or two CH row
// orders) emit the same set. Returns nil when no row carried the
// reserved spanID slot (legacy / aggregate shapes) so the summary
// omits the spanSet fields entirely.
func (a *summaryAcc) buildSpanSet(spss int) *SpanSet {
	if len(a.spans) == 0 {
		return nil
	}
	sort.Slice(a.spans, func(i, j int) bool {
		// StartTimeUnixNano is FormatInt of UnixNano — compare
		// numerically, not lexically (string compare breaks across
		// digit-count boundaries).
		ti, _ := strconv.ParseInt(a.spans[i].StartTimeUnixNano, 10, 64)
		tj, _ := strconv.ParseInt(a.spans[j].StartTimeUnixNano, 10, 64)
		if ti != tj {
			return ti < tj
		}
		return a.spans[i].SpanID < a.spans[j].SpanID
	})
	spans := a.spans
	if len(spans) > spss {
		spans = spans[:spss]
	}
	return &SpanSet{Spans: spans, Matched: a.matched}
}

// sortSummariesStartDesc applies Tempo's /api/search ordering:
// startTimeUnixNano descending (newest trace first); upstream leaves
// ties unspecified, so TraceID ascending is the deterministic
// tie-break.
func sortSummariesStartDesc(out []TraceSummary) {
	sort.Slice(out, func(i, j int) bool {
		si, _ := strconv.ParseInt(out[i].StartTimeUnixNano, 10, 64)
		sj, _ := strconv.ParseInt(out[j].StartTimeUnixNano, 10, 64)
		if si != sj {
			return si > sj
		}
		return out[i].TraceID < out[j].TraceID
	})
}

// traceBatch is the wire-format-agnostic intermediate the trace-by-ID
// assemblers consume: one entry per distinct ResourceAttributes set, in
// deterministic order, with the per-span rows already sorted. Both
// groupBatches (JSON) and groupBatchesProto (proto) map this shape onto
// their wire types, so batch / span ordering can never diverge between
// the two Accept-negotiated paths — or between two sequential calls
// (the unit + e2e determinism pins fetch the same trace via v1 and v2
// and require the v2 envelope's inner trace to be byte-identical to
// the bare v1 trace).
type traceBatch struct {
	resourceAttrs map[string]string
	spans         []traceSpanRow
}

// traceSpanRow carries one sample plus its splitTraceByIDLabels
// partition (span attributes + reserved-key metadata).
type traceSpanRow struct {
	sample    chclient.Sample
	spanAttrs map[string]string
	meta      map[string]string
}

// groupTraceBatches buckets a flat span list by resource-attribute set
// and returns the batches in a fully deterministic order, independent
// of both Go map iteration and the row order ClickHouse happened to
// return:
//
//   - Batches sort by resource service.name first (the field Grafana's
//     "Processes" tab leads with), then by the canonical resource-attr
//     string (format.CanonicalKey) as the total-order tie-break.
//   - Spans within a batch sort by StartTimeUnixNano, then by SpanID.
//
// Without this, the bucket map's iteration order (JSON path) and the
// CH result-row order (span order, both paths) made two sequential
// fetches of the same trace intermittently differ — see the retry-
// masked flake in k3d e2e run 27284868985.
func groupTraceBatches(samples []chclient.Sample) []traceBatch {
	bucket := map[string]*traceBatch{}
	keys := make([]string, 0)
	for _, s := range samples {
		resourceAttrs, spanAttrs, meta := splitTraceByIDLabels(s.Labels)
		// Group by resource-attribute set so Grafana's "Processes" tab
		// gets one batch per service.
		key := format.CanonicalKey(resourceAttrs)
		b, ok := bucket[key]
		if !ok {
			b = &traceBatch{resourceAttrs: resourceAttrs}
			bucket[key] = b
			keys = append(keys, key)
		}
		b.spans = append(b.spans, traceSpanRow{sample: s, spanAttrs: spanAttrs, meta: meta})
	}
	sort.Slice(keys, func(i, j int) bool {
		si := bucket[keys[i]].resourceAttrs["service.name"]
		sj := bucket[keys[j]].resourceAttrs["service.name"]
		if si != sj {
			return si < sj
		}
		return keys[i] < keys[j]
	})
	out := make([]traceBatch, 0, len(keys))
	for _, k := range keys {
		b := bucket[k]
		sort.Slice(b.spans, func(i, j int) bool {
			ti := b.spans[i].sample.Timestamp.UnixNano()
			tj := b.spans[j].sample.Timestamp.UnixNano()
			if ti != tj {
				return ti < tj
			}
			return b.spans[i].meta[traceByIDKeySpanID] < b.spans[j].meta[traceByIDKeySpanID]
		})
		out = append(out, *b)
	}
	return out
}

// groupBatches converts a flat span list into Tempo's `batches` shape
// (one batch per distinct ResourceAttributes set). The wrap-projection
// for /api/traces/{id} (see traceByIDProjections) smuggles span-detail
// fields (TraceId / SpanId / ParentSpanId / SpanKind / StatusCode plus
// the SpanAttributes map) inside chclient.Sample.Labels under reserved
// keys; groupTraceBatches splits them back out (via
// splitTraceByIDLabels) and owns the deterministic batch / span order;
// this helper only maps the intermediate onto the SpanEntry fields
// Grafana's trace-view consumes, keeping ResourceAttributes
// (un-prefixed entries) on the Resource.Attributes map.
func groupBatches(samples []chclient.Sample) []ResourceSpans {
	groups := groupTraceBatches(samples)
	out := make([]ResourceSpans, 0, len(groups))
	for _, g := range groups {
		rs := ResourceSpans{Resource: Resource{Attributes: g.resourceAttrs}}
		for _, row := range g.spans {
			rs.Spans = append(rs.Spans, SpanEntry{
				TraceID:           row.meta[traceByIDKeyTraceID],
				SpanID:            row.meta[traceByIDKeySpanID],
				ParentSpanID:      row.meta[traceByIDKeyParentSpanID],
				Name:              row.sample.MetricName,
				Kind:              row.meta[traceByIDKeySpanKind],
				StartTimeUnixNano: strconv.FormatInt(row.sample.Timestamp.UnixNano(), 10),
				DurationNanos:     int64(row.sample.Value),
				Status:            SpanStatus{Code: row.meta[traceByIDKeyStatusCode]},
				Attributes:        row.spanAttrs,
			})
		}
		out = append(out, rs)
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
			traceByIDKeySpanKind, traceByIDKeyStatusCode,
			traceByIDKeyScopeName, traceByIDKeyScopeVersion:
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

// writeTraceProto marshals a *tempopb.Trace and writes it with
// Content-Type: application/protobuf so Grafana's Tempo plugin can
// proto.Unmarshal the body directly. Mirrors reference Tempo's
// `/api/traces/{id}` Accept-protobuf branch (see
// grafana/tempo:cmd/tempo/app/...querier.go) — same Content-Type,
// same payload shape (root *tempopb.Trace), same status semantics.
//
// On marshal failure we fall back to a JSON error envelope rather
// than half-writing a malformed proto body: a partial proto blob
// surfaces in Grafana as a generic "illegal wireType" message that
// hides the underlying cause, while the JSON envelope at least lets
// the operator see what the marshal error was.
func writeTraceProto(w http.ResponseWriter, status int, t *tempopb.Trace) {
	b, err := proto.Marshal(t)
	if err != nil {
		httperr.WriteJSON(w, http.StatusInternalServerError, ErrorResponse{
			Error: true, Message: fmt.Sprintf("proto marshal: %v", err),
		})
		return
	}
	w.Header().Set("Content-Type", "application/protobuf")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// writeTraceByIDV2 emits the `/api/v2/traces/{id}` envelope in the
// negotiated encoding, mirroring upstream Tempo's
// `modules/frontend/combiner/common.go` internalMarshalAs:
//
//   - Accept: protobuf → proto.Marshal(*tempopb.TraceByIDResponse),
//     Content-Type: application/protobuf. This is what Grafana 12.x's
//     Tempo plugin unmarshals before its OTLP conversion.
//   - otherwise → gogo jsonpb (the proto3 JSON mapping: camelCase
//     field names, base64 bytes, zero-valued fields omitted), i.e.
//     `{"trace":{"resourceSpans":[…]},"metrics":{}}` — NOT the bare
//     v1 `{"batches":[…]}` shape, and NOT MarshalToJSONV1's
//     `batches` rename (that helper is v1-only upstream).
//
// On marshal failure both branches fall back to the JSON error
// envelope, same rationale as writeTraceProto.
func writeTraceByIDV2(w http.ResponseWriter, status int, resp *tempopb.TraceByIDResponse, asProto bool) {
	var (
		b   []byte
		err error
	)
	contentType := "application/json"
	if asProto {
		contentType = "application/protobuf"
		b, err = proto.Marshal(resp)
	} else {
		var s string
		s, err = new(jsonpb.Marshaler).MarshalToString(resp)
		b = []byte(s)
	}
	if err != nil {
		httperr.WriteJSON(w, http.StatusInternalServerError, ErrorResponse{
			Error: true, Message: fmt.Sprintf("trace-by-id v2 marshal: %v", err),
		})
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// isValidTraceID mirrors reference Tempo's trace-id grammar on the
// `/api/traces/{id}` (v1 + v2) wire: a lowercase hex string of length
// 16 or 32. Callers must `strings.ToLower` the input first so
// upper-case IDs are accepted (Grafana sometimes emits upper-case);
// the length check here is on the lowered string so it stays
// allocation-free in the hot path. Anything else — non-hex bytes,
// off-by-one length, empty — is a 400 ("invalid trace id") before
// the engine lookup, matching upstream behaviour.
func isValidTraceID(id string) bool {
	switch len(id) {
	case 16, 32:
	default:
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
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

// panicEnvelope is the [telemetry.PanicRenderer] for the Tempo head:
// when QueryMiddleware recovers a handler panic before any response was
// committed, it renders Tempo's distinct error shape
// (`{traceID, spanID, error, message}`) with a 500 status so Grafana's
// Tempo datasource sees a clean error instead of a dropped connection.
// The recovered value + stack are logged by the middleware via the OTLP
// slog bridge; this only shapes the wire response.
func panicEnvelope(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusInternalServerError, "", "",
		errors.New("internal server error"))
}
