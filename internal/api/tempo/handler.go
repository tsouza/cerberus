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
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) ([]chclient.Sample, error)
}

// Handler implements the Tempo HTTP API endpoints cerberus speaks.
// Mount it via Handler.Mount(mux). The current vertical slice covers
// /api/echo, /api/status/version, /api/search, and /api/traces/<id>.
// /api/search/tags + /api/search/tag/<n>/values defer to RC6's
// sqlbuilder integration so the new SQL avoids Sprintf.
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

// wrapWithSampleProjection adds a Project on top of plan that selects
// the columns the chclient.Sample scanner expects positionally:
// MetricName (used for SpanName here), Attributes (ResourceAttributes),
// TimeUnix (Timestamp), Value (Duration as float64-castable). Tempo
// queries reuse the same Sample shape for transport; the response
// formatters re-derive richer span data from row context.
func wrapWithSampleProjection(plan chplan.Node, s schema.Traces) chplan.Node {
	return &chplan.Project{
		Input: plan,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.SpanNameColumn}, Alias: "MetricName"},
			{Expr: &chplan.ColumnRef{Name: s.ResourceAttributesColumn}, Alias: "Attributes"},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: "TimeUnix"},
			// Duration is Int64 (nanoseconds) in OTel-CH; chclient.Sample.Value
			// is float64 and clickhouse-go's Scan refuses Int64→float64 without
			// a cast. toFloat64 keeps the wire shape lossless within the
			// 53-bit mantissa range (a 100-day duration in ns still fits).
			{Expr: &chplan.FuncCall{Name: "toFloat64", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.DurationColumn}}}, Alias: "Value"},
		},
	}
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
