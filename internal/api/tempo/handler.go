package tempo

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/grafana/tempo/pkg/traceql"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/schema"
	traceql_lower "github.com/tsouza/cerberus/internal/traceql"
)

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
}

// New constructs a Handler with the seed optimizer wired in.
func New(client Querier, s schema.Traces, version string, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		Client:    client,
		Schema:    s,
		Optimizer: optimizer.Default(),
		Logger:    logger,
		Version:   version,
	}
}

// Mount registers the Tempo-compatible endpoints under /api/ on mux.
// /api/echo + /api/status/version satisfy Grafana's datasource health
// check; /api/search runs a TraceQL query; /api/traces/{id} fetches a
// single trace by ID.
func (h *Handler) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/echo", h.handleEcho)
	mux.HandleFunc("GET /api/status/version", h.handleVersion)
	mux.HandleFunc("GET /api/search", h.handleSearch)
	mux.HandleFunc("GET /api/search/recent", h.handleSearchRecent)
	mux.HandleFunc("GET /api/search/tags", h.handleSearchTags)
	mux.HandleFunc("GET /api/search/tag/{name}/values", h.handleSearchTagValues)
	mux.HandleFunc("GET /api/v2/search/tags", h.handleSearchTagsV2)
	mux.HandleFunc("GET /api/v2/search/tag/{name}/values", h.handleSearchTagValuesV2)
	mux.HandleFunc("GET /api/traces/{id}", h.handleTraceByID)
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

	expr, err := traceql.Parse(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, "", "", err)
		return
	}

	plan, err := traceql_lower.Lower(expr, h.Schema)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "", "", err)
		return
	}

	plan = wrapWithSampleProjection(plan, h.Schema)
	plan = h.Optimizer.Run(plan)

	sqlStr, args, err := chsql.Emit(plan)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "", "", err)
		return
	}
	h.Logger.Debug("cerberus tempo search", "traceql", q, "sql", sqlStr, "args", args)

	samples, err := h.Client.Query(r.Context(), sqlStr, args...)
	if err != nil {
		h.Logger.Warn("cerberus tempo search CH query failed", "err", err.Error(), "sql", sqlStr)
		writeError(w, http.StatusBadGateway, "", "", err)
		return
	}

	writeJSON(w, http.StatusOK, SearchResponse{
		Traces:  toTraceSummaries(samples),
		Metrics: SearchMetrics{InspectedTraces: len(samples)},
	})
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
	plan := chplan.Node(&chplan.Scan{Table: s.SpansTable})
	plan = &chplan.OrderBy{
		Input: plan,
		Keys: []chplan.OrderKey{
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Desc: true},
		},
	}
	plan = &chplan.Limit{Input: plan, Count: limit}
	plan = wrapWithSampleProjection(plan, s)
	plan = h.Optimizer.Run(plan)

	sqlStr, args, err := chsql.Emit(plan)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "", "", err)
		return
	}
	h.Logger.Debug("cerberus tempo search/recent", "limit", limit, "sql", sqlStr, "args", args)

	samples, err := h.Client.Query(r.Context(), sqlStr, args...)
	if err != nil {
		h.Logger.Warn("cerberus tempo search/recent CH query failed", "err", err.Error(), "sql", sqlStr)
		writeError(w, http.StatusBadGateway, "", "", err)
		return
	}

	writeJSON(w, http.StatusOK, SearchResponse{
		Traces:  toTraceSummaries(samples),
		Metrics: SearchMetrics{InspectedTraces: len(samples)},
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
	plan = wrapWithSampleProjection(plan, h.Schema)
	plan = h.Optimizer.Run(plan)

	sqlStr, args, err := chsql.Emit(plan)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "", "", err)
		return
	}
	h.Logger.Debug("cerberus tempo traceByID", "trace_id", traceID, "sql", sqlStr, "args", args)

	samples, err := h.Client.Query(r.Context(), sqlStr, args...)
	if err != nil {
		h.Logger.Warn("cerberus tempo traceByID CH query failed", "err", err.Error(), "trace_id", traceID, "sql", sqlStr)
		writeError(w, http.StatusBadGateway, traceID, "", err)
		return
	}
	if len(samples) == 0 {
		// Tempo's "trace not found" shape — Grafana renders the right UI.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(ErrorResponse{
			TraceID: traceID, SpanID: "", Error: true,
			Message: fmt.Sprintf("trace not found: %s", traceID),
		})
		return
	}

	writeJSON(w, http.StatusOK, TraceByIDResponse{
		Batches: groupBatches(samples),
	})
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
		key := canonicalKey(s.Labels)
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

func canonicalKey(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
		b.WriteByte(0)
	}
	return b.String()
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError returns Tempo's distinct error shape so Grafana renders
// the right UI (vs generic JSON error).
func writeError(w http.ResponseWriter, status int, traceID, spanID string, err error) {
	writeJSON(w, status, ErrorResponse{
		TraceID: traceID, SpanID: spanID, Error: true,
		Message: err.Error(),
	})
}
