package loki

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
	QueryDetectedFieldRows(ctx context.Context, sql string, args ...any) ([]chclient.DetectedFieldRow, error)
	QueryTimestampedLines(ctx context.Context, sql string, args ...any) ([]chclient.TimestampedLine, error)
	QueryIndexStats(ctx context.Context, sql string, args ...any) (chclient.IndexStatsRow, error)
	QueryIndexVolume(ctx context.Context, sql string, args ...any) ([]chclient.IndexVolumeRow, error)
	QueryLabelSets(ctx context.Context, sql string, args ...any) ([]map[string]string, error)
}

// Handler implements the Loki HTTP API endpoints cerberus speaks. Mount
// it via Handler.Mount(mux). The current vertical slice covers
// /loki/api/v1/query, /loki/api/v1/query_range, /loki/api/v1/index/stats
// + /index/volume, plus the metadata endpoints /labels,
// /label/<name>/values, /series, /detected_fields, /patterns.
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

	// Version is the cerberus build identifier surfaced via
	// `/loki/api/v1/status/buildinfo`. Wired from cmd/cerberus's
	// build-time `Version` var so Grafana's Loki datasource per-page
	// probe sees a real value; left empty in tests (the buildinfo
	// handler still returns 200 with empty-string fields, matching
	// upstream Loki's behaviour when build metadata is unset).
	Version string

	// QueryTimeout is the configured default per-query wall-clock cap
	// (CERBERUS_QUERY_TIMEOUT). Loki's query API accepts a `timeout`
	// param; when present it min's against this default (the smaller
	// wins) and the result is threaded onto the request ctx as both a
	// context deadline and chclient.WithQueryTimeout (the ClickHouse
	// max_execution_time override). Wired from
	// Config.ClickHouse.QueryTimeout in cmd/cerberus; 0 applies no
	// per-request override (the Client default still caps every query).
	QueryTimeout time.Duration

	// TailWriteTimeout bounds a single /tail WebSocket write before a slow /
	// dead client is torn down. Wired from CERBERUS_LOKI_TAIL_WRITE_TIMEOUT
	// (Config.LokiTailWriteTimeout) in cmd/cerberus. Zero falls back to the
	// package default (defaultTailWriteTimeout) so tests that build a bare
	// Handler keep the historical 10s bound.
	TailWriteTimeout time.Duration
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
// panel. /patterns trains a drain template miner over the peek window.
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
		mux.Handle(pattern, h.Limiter.Middleware(1, telemetry.QueryMiddleware("logql", panicEnvelope, hf)))
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
	register("GET /loki/api/v1/detected_labels", h.handleDetectedLabels)
	register("POST /loki/api/v1/detected_labels", h.handleDetectedLabels)
	register("GET /loki/api/v1/patterns", h.handlePatterns)
	register("POST /loki/api/v1/patterns", h.handlePatterns)
	// /tail is WebSocket-upgrade only; no POST counterpart in upstream Loki.
	register("GET /loki/api/v1/tail", h.handleTail)
	// Format-query + build-info probes. Grafana's Loki datasource hits
	// /status/buildinfo on every page load to gate feature flags
	// (LogQL editor capabilities, label-browser presence) and uses
	// /format_query when the query-editor's "Format query" button is
	// pressed. Neither endpoint touches ClickHouse.
	register("GET /loki/api/v1/format_query", h.handleFormatQuery)
	register("POST /loki/api/v1/format_query", h.handleFormatQuery)
	register("GET /loki/api/v1/status/buildinfo", h.handleBuildInfo)
	// Drilldown-limits config probe — Grafana's first-party Logs
	// Drilldown app (preinstalled in 12.x) fetches this on boot to
	// gate the Patterns tab / volume panels / level filtering.
	// Upstream registers GET only (pkg/loki/loki.go).
	register("GET /loki/api/v1/drilldown-limits", h.handleDrilldownLimits)

	// JSON-shaped 404 fallback for unmatched /loki/api/v1/* routes. Without
	// this, http.ServeMux serves Go's plain-text "404 page not found"
	// body, which trips Grafana 11.2+ — the Loki datasource probes feature
	// endpoints (e.g. /detected_labels on older cerberus) and expects a
	// JSON envelope even on misses. Registering the catch-all on the
	// prefix keeps unrelated routes (/, /healthz, /metrics, the Prom and
	// Tempo paths) untouched.
	mux.HandleFunc("/loki/api/", h.handleLokiNotFound)
}

// handleLokiNotFound serves a JSON-shaped 404 for unmatched /loki/* paths.
// Mirrors the wire envelope writeError emits on real 400/500s so Grafana
// parses the body uniformly regardless of which route missed.
func (h *Handler) handleLokiNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, ErrBadData, errLokiPathNotFound{path: r.URL.Path})
}

// errLokiPathNotFound is a tiny typed error so the 404 body carries the
// request path back to the operator (Grafana surfaces the `error` field
// in datasource health checks). Implementing error keeps writeError's
// signature untouched.
type errLokiPathNotFound struct{ path string }

func (e errLokiPathNotFound) Error() string {
	return "unknown loki endpoint: " + e.path
}

// handleFormatQuery implements `/loki/api/v1/format_query`. Takes a
// `query` parameter, parses it with the upstream LogQL syntax parser,
// and returns the pretty-printed string. Grafana's logs query editor
// uses this to format on save / on the explicit "Format query" button.
// Wrapped in the standard {status, data} envelope so the Loki
// datasource decodes it identically to /labels and /series.
func (h *Handler) handleFormatQuery(w http.ResponseWriter, r *http.Request) {
	q := r.FormValue("query")
	if q == "" {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("missing query parameter"))
		return
	}
	expr, err := syntax.ParseExpr(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}
	writeJSON(w, http.StatusOK, Response{
		Status: "success",
		Data:   expr.String(),
	})
}

// handleBuildInfo implements `/loki/api/v1/status/buildinfo`. Returns
// the upstream Loki BuildInfo shape (version / revision / branch /
// buildUser / buildDate / goVersion) as a flat top-level JSON object —
// the Loki API documents this endpoint's body as the BuildInfo struct
// directly, NOT wrapped in the {status, data} envelope the rest of
// the v1 surface uses. Grafana parses this body to decide which LogQL
// features to enable in the query editor.
func (h *Handler) handleBuildInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, BuildInfo{
		Version:   h.Version,
		GoVersion: runtime.Version(),
	})
}

func (h *Handler) handleQuery(w http.ResponseWriter, r *http.Request) {
	// r.FormValue merges URL query params with a POST form-encoded body
	// (auto-calling ParseForm). Grafana's Loki datasource POSTs queries as
	// an application/x-www-form-urlencoded body, so reading r.URL.Query()
	// alone returns an empty `query` → 400. FormValue keeps GET behaviour
	// byte-identical while making the POST routes (registered in Mount)
	// actually work.
	q := r.FormValue("query")
	if q == "" {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("missing query parameter"))
		return
	}
	ts, err := format.ParseTimeLoki(r.FormValue("time"), time.Now())
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}
	limit, err := parseLogLimit(r.FormValue("limit"))
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}
	dir := parseLogDirection(r.FormValue("direction"))

	ctx, cancel, ok := h.applyQueryTimeout(w, r)
	if !ok {
		return
	}
	defer cancel()

	// Instant /query: collapse the window onto a single point. Per
	// upstream Loki contract the evaluation lookback is the previous
	// 5 minutes (the same instant-lookback PromQL uses). Threading
	// [ts - 5m, ts] keeps the Scan filtered to that envelope so the
	// SQL doesn't return every matching log in the table.
	const instantLookback = 5 * time.Minute
	res, err := h.Engine.Query(ctx, h.langForRequest(ts.Add(-instantLookback), ts), q)
	if err != nil {
		h.respondError(w, classifyEngineErr(err))
		return
	}
	expr, _ := res.Meta.Extra["expr"].(syntax.Expr)
	h.Logger.Debug("cerberus loki query", "logql", q, "sql", res.SQL, "args", res.Args)

	data, err := buildInstantData(expr, res.Samples, ts, h.Schema, limit, dir, wantsCategorizedLabels(r))
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
	// r.FormValue merges URL query params with a POST form-encoded body
	// (auto-calling ParseForm). Grafana's Loki datasource POSTs queries as
	// an application/x-www-form-urlencoded body; reading r.URL.Query() alone
	// returns an empty `query` → 400. FormValue keeps GET byte-identical.
	q := r.FormValue("query")
	if q == "" {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("missing query parameter"))
		return
	}
	start, err := format.ParseTimeLoki(r.FormValue("start"), time.Time{})
	if err != nil || start.IsZero() {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("missing or invalid 'start' parameter"))
		return
	}
	end, err := format.ParseTimeLoki(r.FormValue("end"), time.Time{})
	if err != nil || end.IsZero() {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("missing or invalid 'end' parameter"))
		return
	}
	step, err := format.ParseDuration(r.FormValue("step"))
	if err != nil {
		// Loki allows missing step (auto-resolves); cerberus requires it for
		// metric queries. Default to 1 minute when absent.
		step = time.Minute
	}
	if !end.After(start) {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("'end' must be after 'start'"))
		return
	}
	// Cap the returned points per timeseries (end-start)/step, mirroring the
	// Prom head's resolution ceiling. Without it an unauthenticated client can
	// force an arbitrarily wide matrix fan-out (compute-DoS). Uses the shared
	// format.MaxResolutionPoints so all three heads reject the same shape.
	if end.Sub(start)/step > format.MaxResolutionPoints {
		// Pre-parse cap rejection: no CH query runs, so query_log can never
		// reflect it. Record a decision-only "rejected" corpus row.
		h.Engine.ObserveCapRejection("logql")
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New(format.ResolutionCapMessage))
		return
	}
	limit, err := parseLogLimit(r.FormValue("limit"))
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}
	dir := parseLogDirection(r.FormValue("direction"))

	ctx, cancel, ok := h.applyQueryTimeout(w, r)
	if !ok {
		return
	}
	defer cancel()

	res, err := h.Engine.Query(ctx, h.langForRangeRequest(start, end, step), q)
	if err != nil {
		h.respondError(w, classifyEngineErr(err))
		return
	}
	expr, _ := res.Meta.Extra["expr"].(syntax.Expr)
	h.Logger.Debug("cerberus loki query_range", "logql", q, "sql", res.SQL, "args", res.Args)

	data, err := buildRangeData(expr, res.Samples, start, end, step, h.Schema, limit, dir, wantsCategorizedLabels(r))
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

// langForRangeRequest builds a per-request *logql.Lang carrying the
// request's [start, end, step] for metric queries. The engine threads
// Step alongside Start / End so range-aggregation lowerings switch to
// the matrix RangeWindow shape (one row per anchor across [start, end]
// spaced by step). Without this, metric queries whose seeded data
// lies outside the last 5 minutes of wall-clock return an empty matrix
// because the windowed-array filter anchors at `now64(9)`. Log queries
// also use this entry point — Step is harmless on the non-metric path
// since lowerCtx.rangeMode() only fires inside the range-aggregation
// lowering.
func (h *Handler) langForRangeRequest(start, end time.Time, step time.Duration) *logql.Lang {
	return &logql.Lang{Schema: h.Schema, Start: start, End: end, Step: step}
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
// applyQueryTimeout derives the request context the /query and
// /query_range handlers run under, honouring Loki's `timeout` query
// param. It resolves the effective wall-clock budget — the configured
// default (h.QueryTimeout) min'd with the request's ?timeout= (the
// smaller wins; 0 on either side means "no cap from that source") — and,
// when positive, threads it onto the returned context as both a context
// deadline AND chclient.WithQueryTimeout (the ClickHouse
// max_execution_time override). The caller MUST defer the returned
// cancel (a no-op when no deadline was installed). A malformed ?timeout=
// is a 400 bad_data; ok=false signals the caller already wrote the error
// and must return.
func (h *Handler) applyQueryTimeout(w http.ResponseWriter, r *http.Request) (context.Context, context.CancelFunc, bool) {
	ctx := r.Context()
	budget := h.QueryTimeout
	if raw := r.FormValue("timeout"); raw != "" {
		reqTimeout, err := format.ParseDuration(raw)
		if err != nil || reqTimeout < 0 {
			writeError(w, http.StatusBadRequest, ErrBadData,
				fmt.Errorf("invalid parameter 'timeout': %w", err))
			return ctx, func() {}, false
		}
		budget = minPositiveDuration(budget, reqTimeout)
	}
	if budget <= 0 {
		return ctx, func() {}, true
	}
	ctx = chclient.WithQueryTimeout(ctx, budget)
	ctx, cancel := context.WithTimeout(ctx, budget)
	return ctx, cancel, true
}

// minPositiveDuration returns the smaller of a and b, treating a
// non-positive value as "unbounded" so it never wins the min; when both
// are non-positive the result is 0 (no cap). Mirrors the effective-
// timeout rule shared with the Prom head.
func minPositiveDuration(a, b time.Duration) time.Duration {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

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
	// Sample-budget exceedance: the per-query CERBERUS_QUERY_MAX_SAMPLES
	// cap aborted the result-set drain inside chclient. Surface it the
	// way upstream Loki reports query-limit violations — HTTP 400 with a
	// "maximum ... reached for a single query" message — NOT a 5xx: the
	// query is over-broad, ClickHouse is healthy (the breaker never sees
	// drain errors, see chclient.QueryCursor).
	var tooMany *chclient.TooManySamplesError
	if errors.As(err, &tooMany) {
		return &apiError{
			Kind: ErrBadData,
			Err: fmt.Errorf(
				"maximum number of samples (%d) reached for a single query; consider reducing the query range or resolution",
				tooMany.Limit,
			),
			Status: http.StatusBadRequest,
		}
	}
	// ClickHouse memory-limit abort (code 241, MEMORY_LIMIT_EXCEEDED):
	// the server-side sibling of the sample budget above — the
	// per-query `max_memory_usage` cap (CERBERUS_CH_QUERY_MAX_MEMORY)
	// or a CH server-side limit rejected the query. Same Loki-style
	// limit rejection: HTTP 400 bad_data with a "maximum ... reached
	// for a single query" message — NOT a 5xx, ClickHouse is healthy
	// when it enforces a cap (the chclient breaker treats code 241 as
	// a success for the same reason).
	var memLimit *chclient.MemoryLimitError
	if errors.As(err, &memLimit) {
		msg := "maximum memory usage reached for a single query; consider reducing the query range or resolution"
		if memLimit.Limit > 0 {
			msg = fmt.Sprintf(
				"maximum memory usage (%d bytes) reached for a single query; consider reducing the query range or resolution",
				memLimit.Limit,
			)
		}
		return &apiError{
			Kind:   ErrBadData,
			Err:    errors.New(msg),
			Status: http.StatusBadRequest,
		}
	}
	// Wall-clock timeout: the data-plane query hit its max_execution_time
	// cap (TIMEOUT_EXCEEDED → *QueryTimeoutError) or the handler's
	// context deadline fired. Upstream Loki surfaces a query-timeout as
	// HTTP 503 errorType=timeout (queryrange's "context deadline
	// exceeded" path), NOT a 5xx fault — the query ran exactly as long as
	// it was allowed to; ClickHouse is healthy (the chclient breaker
	// treats code 159 as a success for the same reason). A plain
	// context.Canceled (client gone) is deliberately not caught here.
	if errors.Is(err, chclient.ErrQueryTimeout) || errors.Is(err, context.DeadlineExceeded) {
		msg := "query timed out"
		var qt *chclient.QueryTimeoutError
		if errors.As(err, &qt) && qt.Timeout > 0 {
			msg = fmt.Sprintf("query timed out after %s", qt.Timeout)
		}
		return &apiError{
			Kind:   ErrTimeout,
			Err:    errors.New(msg),
			Status: http.StatusServiceUnavailable,
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
// streams. The limit + direction control how many log entries to
// surface and in what order (Loki applies `limit` to the TOTAL entry
// count across all streams, not per-stream).
func buildInstantData(expr syntax.Expr, samples []chclient.Sample, ts time.Time, _ schema.Logs, limit int, dir logDirection, categorize bool) (*QueryData, error) {
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
	samples = clampLogSamples(samples, limit, dir)
	streams := toStreamsWithTransform(samples, tx, categorize)
	return &QueryData{
		ResultType:    "streams",
		EncodingFlags: encodingFlagsFor(categorize, streams),
		Result:        streams,
	}, nil
}

// buildRangeData turns the sample stream into a Loki range-query data
// body. Metric queries produce a matrix (per-step latest value per
// series). Log queries produce streams; limit + direction follow the
// same `total entries across all streams` rule as [buildInstantData].
func buildRangeData(expr syntax.Expr, samples []chclient.Sample, start, end time.Time, step time.Duration, _ schema.Logs, limit int, dir logDirection, categorize bool) (*QueryData, error) {
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
	samples = clampLogSamples(samples, limit, dir)
	streams := toStreamsWithTransform(samples, tx, categorize)
	return &QueryData{
		ResultType:    "streams",
		EncodingFlags: encodingFlagsFor(categorize, streams),
		Result:        streams,
	}, nil
}

// wantsCategorizedLabels reports whether the request asked for the
// categorized-labels response encoding via the
// `X-Loki-Response-Encoding-Flags` header. Grafana's Loki datasource
// always sends `categorize-labels`; plain clients (the loki-compat
// harness, curl) omit it and get the default two-element value shape.
// The header is comma-separated; cerberus honours the single flag it
// supports.
func wantsCategorizedLabels(r *http.Request) bool {
	for _, raw := range r.Header.Values("X-Loki-Response-Encoding-Flags") {
		for _, flag := range strings.Split(raw, ",") {
			if strings.TrimSpace(flag) == encodingFlagCategorizeLabels {
				return true
			}
		}
	}
	return false
}

// encodingFlagsFor returns the `encodingFlags` array to advertise on a
// streams response. The decision is REQUEST-driven, not data-driven: it is
// `["categorize-labels"]` exactly when the client requested categorization
// (`categorize` true), independent of whether any value carries structured
// metadata. This MUST stay in lock-step with [StreamValue.MarshalJSON],
// which emits the three-element categorized tuple for every value when
// `Categorize` is set: the advertised flag and the per-value arity are two
// halves of one contract. Grafana's parser switches on the flag alone
// (`readResult`: `slices.Contains(encodingFlags, "categorize-labels")`),
// then `readCategorizedStream` reads a mandatory third element from EVERY
// value — so advertising the flag while emitting a two-element value (the
// old "metadata-free → no flag" gate) is precisely the mismatch that 400'd
// `[explore:loki] POST /api/ds/query`. Returning nil leaves the field
// absent (omitempty), keeping a non-categorize client on the plain
// `readStream` branch with byte-identical two-element values.
func encodingFlagsFor(categorize bool, _ []Stream) []string {
	if !categorize {
		return nil
	}
	return []string{encodingFlagCategorizeLabels}
}

// logDirection is the wire-format direction parameter — "backward"
// (most-recent-first, the default) or "forward". Used to order log
// entries when applying the `limit` clamp so the surfaced subset
// matches reference Loki's truncation rule (latest N for backward,
// earliest N for forward). Metric queries ignore this field; their
// matrix / vector emitters carry their own per-anchor ordering.
type logDirection int

const (
	// directionBackward returns the most-recent N entries — Loki's
	// default and the only direction the loki-bench harness exercises
	// for log queries (forward log queries are unsupported by Loki's
	// v2 engine, see fast/basic-selectors.yaml).
	directionBackward logDirection = iota
	directionForward
)

// Loki's documented limit defaults. The default + ceiling mirror the
// upstream `pkg/loghttp/params.go::defaultQueryLimit` /
// `maxQueryLimit` constants — a request with no `limit` parameter
// returns up to 100 entries; values above 5000 are clamped to 5000
// rather than rejected so a misbehaving client doesn't trigger
// runaway memory growth.
const (
	defaultLogQueryLimit = 100
	maxLogQueryLimit     = 5000
)

// parseLogLimit reads the URL's `limit` query parameter, clamping at
// [maxLogQueryLimit] and defaulting to [defaultLogQueryLimit] when
// absent. Non-positive or non-numeric values fail with a 400-mapped
// error so a Grafana / loki-bench client that passes garbage sees
// the same diagnostic shape upstream Loki emits.
func parseLogLimit(raw string) (int, error) {
	if raw == "" {
		return defaultLogQueryLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, errors.New("'limit' must be a positive integer")
	}
	if n > maxLogQueryLimit {
		n = maxLogQueryLimit
	}
	return n, nil
}

// parseLogDirection maps the URL's `direction` query parameter onto
// the [logDirection] enum. Unknown / empty values fall back to
// backward (the Loki documented default — the loki-bench harness
// also sends explicit "backward" for every log case).
func parseLogDirection(raw string) logDirection {
	if strings.EqualFold(raw, "forward") {
		return directionForward
	}
	return directionBackward
}

// clampLogSamples sorts samples by timestamp (direction-aware) and
// truncates to limit. Loki's wire contract applies `limit` to the
// TOTAL entry count across all streams — not per-stream — so we sort
// the flat sample slice first and let [toStreamsWithTransform] group
// the surviving subset into Streams by labelset. Without this clamp,
// a query whose underlying SQL returns more rows than limit would
// over-surface the response: reference Loki would return e.g. 1000
// streams (one per unique post-parser labelset for the latest 1000
// entries) where cerberus returned every matching row as its own
// stream (the loki-compat `regression/drilldown-patterns.yaml#Basic
// drilldown with json and logfmt parsing` case surfaced as `streams
// length: expected=1000 actual=1440`).
//
// Samples come back from the engine in CH's natural ORDER BY-free
// order; sorting here is authoritative for the wire-format response
// since the chsql emitter for log Scans doesn't currently project an
// ORDER BY clause. Sorting before truncation keeps the chosen subset
// stable across CH execution-plan changes.
//
// limit <= 0 is treated as "no clamp" so test callers that don't
// care about the cap can pass 0; the production handler always
// passes a positive value out of [parseLogLimit].
func clampLogSamples(samples []chclient.Sample, limit int, dir logDirection) []chclient.Sample {
	if len(samples) == 0 {
		return samples
	}
	if dir == directionBackward {
		sort.SliceStable(samples, func(i, j int) bool {
			return samples[i].Timestamp.After(samples[j].Timestamp)
		})
	} else {
		sort.SliceStable(samples, func(i, j int) bool {
			return samples[i].Timestamp.Before(samples[j].Timestamp)
		})
	}
	if limit > 0 && len(samples) > limit {
		samples = samples[:limit]
	}
	return samples
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
			Metric: format.NormalizeLabelMap(l.labels),
			Value:  [2]any{stamp, strconv.FormatFloat(l.value, 'f', -1, 64)},
		})
	}
	return out
}

// toMatrixStepGrid pivots the per-anchor sample stream into Loki's
// "matrix" wire shape: one MatrixSample per distinct series, each
// carrying one (timestamp, value) point per row the SQL returned for
// that series. The matrix-shape RangeWindow emitter (see
// internal/chsql/range_window.go::emitWindowedArrayMatrix) already
// fans the per-step grid out as one row per (series, anchor) and
// drops empty-window anchors via `WHERE length(window_vals) >= N`;
// the pivot is a trivial row → sample copy keyed by canonical series
// key, with NO step-grid iteration or carry-forward.
//
// Mirrors api/prom's matrixFromCursor (post Pool-AK rework): the
// previous last-value-forward implementation with a 5-minute lookback
// over-counted anchors at the request boundary. With sparse
// `by (level)` series — where the inner SQL legitimately drops empty
// 5m windows — the LVF would back-fill the dropped anchors with the
// previous sample's value, inflating the per-series point count
// (~1345 emitted vs ~1071 expected against reference Loki on the
// loki-compat 24h/1m/5m matrix queries).
//
// Rows whose Timestamp falls outside `[start, end]` are clipped so a
// drifted server-side anchor (e.g. instant fallback to now64(9))
// never lands a stray point past the request window.
//
// The `step` argument is preserved for signature symmetry with the
// pre-PR call shape but is unused: the per-row anchor timestamp is
// authoritative.
func toMatrixStepGrid(samples []chclient.Sample, start, end time.Time, _ time.Duration) []MatrixSample {
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
		ms := MatrixSample{Metric: format.NormalizeLabelMap(st.labels)}
		for _, r := range st.rows {
			if r.Timestamp.Before(start) || r.Timestamp.After(end) {
				continue
			}
			stamp := float64(r.Timestamp.UnixMilli()) / 1e3
			ms.Values = append(ms.Values, [2]any{stamp, strconv.FormatFloat(r.Value, 'f', -1, 64)})
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
func toStreamsWithTransform(samples []chclient.Sample, tx lineTransform, categorize bool) []Stream {
	type acc struct {
		labels map[string]string
		values []StreamValue
	}
	bySeries := map[string]*acc{}
	for _, s := range samples {
		line := s.MetricName
		labels := s.Labels
		if tx != nil {
			line, labels = tx(line, s.Timestamp.UnixNano(), labels)
		}
		key := format.CanonicalKey(labels)
		a, ok := bySeries[key]
		if !ok {
			a = &acc{labels: labels}
			bySeries[key] = a
		}
		a.values = append(a.values, StreamValue{
			Timestamp: strconv.FormatInt(s.Timestamp.UnixNano(), 10),
			Line:      line,
			// Per-line structured metadata (the OTel-CH LogAttributes map).
			// When the client requested `categorize-labels` it rides as the
			// categorized `{"structuredMetadata": {...}}` third tuple element
			// (see [StreamValue.MarshalJSON]); an empty map still rides as
			// `{}` so the categorized tuple stays three-element. Without the
			// flag the value marshals to the two-element shape reference Loki
			// returns. Keys are normalised to the Loki/Prom grammar so the
			// Drilldown app renders them as well-formed columns; empty-valued
			// keys are dropped so a blank column never surfaces.
			Metadata:   normalizeMetadata(s.Metadata),
			Categorize: categorize,
		})
	}
	out := make([]Stream, 0, len(bySeries))
	for _, a := range bySeries {
		sort.Slice(a.values, func(i, j int) bool { return a.values[i].Timestamp < a.values[j].Timestamp })
		// NormalizeLabelMap rewrites every OTel-dotted key to Loki's
		// (and Prom's) `[a-zA-Z_][a-zA-Z0-9_]*` grammar; the
		// already-underscored sibling wins on collisions. The
		// WHERE-clause matching path stays bound to raw ResourceAttributes
		// at the SQL layer so dotted-form selectors (`{service.name="x"}`)
		// keep matching.
		out = append(out, Stream{Stream: format.NormalizeLabelMap(a.labels), Values: a.values})
	}
	return out
}

// normalizeMetadata rewrites a per-line structured-metadata map through
// the Loki/Prom label grammar (via [format.NormalizeLabelMap]) and drops
// any entry whose value is empty. The empty-drop mirrors reference Loki,
// which only attaches structured-metadata keys that carry a value, and
// is a defence-in-depth backstop to the SQL-side mapFilter — so a blank
// `_method`-style column can never surface regardless of which path
// populated the map. A nil / all-empty input returns nil so
// [StreamValue.MarshalJSON] falls back to the two-element tuple.
func normalizeMetadata(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	nonEmpty := make(map[string]string, len(in))
	for k, v := range in {
		if v != "" {
			nonEmpty[k] = v
		}
	}
	return format.NormalizeLabelMap(nonEmpty)
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

// panicEnvelope is the [telemetry.PanicRenderer] for the Loki head: when
// QueryMiddleware recovers a handler panic before any response was
// committed, it renders the canonical Loki 500 envelope so Grafana sees
// `{status:"error", errorType:"internal"}` instead of a dropped
// connection. The recovered value + stack are logged by the middleware
// via the OTLP slog bridge; this only shapes the wire response.
func panicEnvelope(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusInternalServerError, ErrInternal,
		errors.New("internal server error"))
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
