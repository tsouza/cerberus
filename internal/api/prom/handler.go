package prom

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"sort"
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
	QueryExemplars(ctx context.Context, sql string, args ...any) ([]chclient.ExemplarRow, error)
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

	// Version is the cerberus build identifier surfaced via
	// `/api/v1/status/buildinfo`. Wired from cmd/cerberus's build-time
	// `Version` var so Grafana's Prom datasource per-page probe sees a
	// real value; left empty in tests (the buildinfo handler still
	// returns 200 with empty-string fields, matching upstream Prom's
	// behaviour when build metadata is unset).
	Version string

	// Lowerers is the BOOT-WIRED polymorphic dispatch table for the
	// ClickHouse-native timeSeries*ToGrid family (native rate +
	// native staleness). It is threaded into the query_range lowering so
	// eligible shapes lower to the native nodes (chplan.RangeWindowNative /
	// chplan.RangeWindowResample) instead of the generic SQL fan-out. Built
	// ONCE at boot in cmd/cerberus from the resolved chopt.EnabledSet
	// (per-function; native rate and native staleness are independent). The
	// zero value (nil strategy fields) is the all-fan-out default. Only the
	// range-streaming path consults it — instant and metadata queries never
	// produce a rate / range-staleness query_range grid, so they are
	// unaffected.
	Lowerers promql.RangeLowerers

	// QueryTimeout is the configured default per-query wall-clock cap
	// (CERBERUS_QUERY_TIMEOUT). It is the ceiling the standard Prometheus
	// `?timeout=<duration>` query param min's against per request
	// (Prometheus uses the smaller of `query.timeout` and `?timeout=`):
	// the resolved value is threaded onto the request ctx as BOTH a
	// context deadline AND chclient.WithQueryTimeout (the ClickHouse
	// max_execution_time override), so route A gets a server-side
	// deadline matching the client-visible one. Wired from
	// Config.ClickHouse.QueryTimeout in cmd/cerberus; 0 leaves the cap
	// at whatever the Client default is and applies no per-request
	// override (?timeout= still narrows the Client default via
	// chclient.WithQueryTimeout). Tests leave it zero.
	QueryTimeout time.Duration

	// parser is the single PromQL parser instance the handler uses for
	// every parse path. The handler-side classification parse
	// (parseExpr — scalar fold / string literal / expression type
	// gate) AND the engine path (executeInstant /
	// executeRangeStreaming, which construct lang values with
	// `Parser: h.parser`) share this same interface value, so the two
	// paths cannot drift on promparser.Options. New(...) is the only
	// construction site; lang values borrow the field by interface
	// identity. The invariant is pinned by
	// TestHandlerLang_ParserOptionsAligned.
	parser promparser.Parser

	// onRangeDrain, when non-nil, is invoked once per /api/v1/query_range
	// request with the number of rows the handler pulled off the streaming
	// cursor (cursor.Inspected() after matrixFromCursor drains it) — the
	// streaming-path drain count, the buffer that OOMed the gateway. It is
	// the test-observable hook the boundsdrain harness reads to assert the
	// range matrix pivot stays O(output) = O(series × step) rather than
	// O(rows scanned) as the dataset / window / cardinality axis grows.
	// Production leaves it nil (the range handler skips the call), so the
	// hot path is byte-unchanged; only tests install it.
	onRangeDrain func(int64)
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
		handler := promHeadersMiddleware(telemetry.QueryMiddleware("promql", panicEnvelope, hf))
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
	// Alerting / recording-rules probe endpoints. cerberus has no rule
	// engine — return the canonical empty envelope an unconfigured
	// upstream Prometheus would, so Grafana's per-page probe is quiet.
	register("GET /api/v1/rules", h.handleRules)
	register("GET /api/v1/alerts", h.handleAlerts)
	// Build-info probe. Grafana's Prom datasource hits this on every
	// page load to gate feature flags (PromQL editor capabilities,
	// remote-write receiver presence). The body is the upstream
	// PrometheusVersion shape wrapped in the {status, data} envelope.
	register("GET /api/v1/status/buildinfo", h.handleBuildInfo)
}

// handleBuildInfo implements `/api/v1/status/buildinfo`. Returns the
// upstream PrometheusVersion shape (version / revision / branch /
// buildUser / buildDate / goVersion) wrapped in the standard
// {status, data} envelope. Grafana parses this body to decide which
// PromQL features to enable in the query editor.
func (h *Handler) handleBuildInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Response{
		Status: "success",
		Data: BuildInfo{
			Version:   h.Version,
			GoVersion: runtime.Version(),
		},
	})
}

// handleFormatQuery implements `/api/v1/format_query`. Takes a `query`
// param, parses it, and returns the pretty-printed string. Grafana's
// query editor uses this to format on save.
func (h *Handler) handleFormatQuery(w http.ResponseWriter, r *http.Request) {
	// r.FormValue merges URL query params with POST form-encoded body
	// (auto-calling ParseForm). Matches the consistent surface used by
	// handleQuery / handleQueryRange.
	q := r.FormValue("query")
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
// inline syntax check.
func (h *Handler) handleParseQuery(w http.ResponseWriter, r *http.Request) {
	// r.FormValue merges URL query params with POST form-encoded body
	// (auto-calling ParseForm). Matches the consistent surface used by
	// handleQuery / handleQueryRange.
	q := r.FormValue("query")
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

	ctx, cancel, ok := h.applyQueryTimeout(w, r)
	if !ok {
		return
	}
	defer cancel()
	ctx = h.applyChaosSleep(ctx, r)

	// Classify the expression once up front: scalar folds and string
	// literals are answered in Go (no ClickHouse round-trip), and a
	// matrix-typed expression (`up[5m]`, `up[5m:1m]`) selects the
	// instant-matrix pivot below — reference Prometheus answers all
	// four result types on /api/v1/query.
	expr, err := h.parseExpr(ctx, q)
	if err != nil {
		h.respondError(w, &apiError{Kind: ErrBadData, Err: err, Status: http.StatusBadRequest})
		return
	}

	// Scalar-only PromQL (`1+1`, `42`) — Grafana's datasource health
	// probe path. Evaluate in Go and return the canonical scalar
	// envelope.
	if value, ok := promql.TryFoldScalar(expr); ok {
		writeJSON(w, http.StatusOK, Response{
			Status: "success",
			Data:   &QueryData{ResultType: "scalar", Result: scalarPoint(ts, value)},
		})
		return
	}

	// String literal (`"a string literal"`, parens included) — the
	// reference wire shape is resultType "string" with the same
	// [<ts>, <value>] pair layout the scalar envelope uses.
	if lit, ok := tryStringLiteralExpr(expr); ok {
		writeJSON(w, http.StatusOK, Response{
			Status: "success",
			Data:   &QueryData{ResultType: "string", Result: Sample{float64(ts.UnixMilli()) / 1e3, lit}},
		})
		return
	}

	samples, hdr, err := h.executeInstant(ctx, q, ts, ts)
	if err != nil {
		h.respondError(w, err)
		return
	}

	writeEngineHeaders(w, hdr)
	if expr.Type() == promparser.ValueTypeMatrix {
		// Top-level range-vector / subquery expression: resultType
		// "matrix" with every returned sample at its own timestamp,
		// grouped per series — the SQL window bound owns sample
		// membership.
		writeJSON(w, http.StatusOK, Response{
			Status: "success",
			Data:   &QueryData{ResultType: "matrix", Result: matrixFromSamples(samples)},
		})
		return
	}
	result := toVector(samples, ts)
	writeJSON(w, http.StatusOK, Response{
		Status: "success",
		Data:   &QueryData{ResultType: "vector", Result: result},
	})
}

// tryStringLiteralExpr unwraps a (possibly parenthesised) top-level
// PromQL string literal. Only /api/v1/query accepts string-typed
// expressions; /api/v1/query_range rejects them via the expression
// type gate (mirroring upstream).
func tryStringLiteralExpr(expr promparser.Expr) (string, bool) {
	for {
		p, ok := expr.(*promparser.ParenExpr)
		if !ok {
			break
		}
		expr = p.Expr
	}
	if lit, ok := expr.(*promparser.StringLiteral); ok {
		return lit.Val, true
	}
	return "", false
}

// matrixFromSamples is the instant-query sibling of matrixFromCursor:
// group rows per canonical series key and emit each sample at its own
// timestamp, ascending. Used for matrix-typed expressions on
// /api/v1/query (`up[5m]`), where the lowered SQL's window predicate
// already owns sample membership — no clipping or step grid applies.
func matrixFromSamples(samples []chclient.Sample) []MatrixSample {
	type seriesState struct {
		labels map[string]string
		rows   []chclient.Sample
	}
	bySeries := map[string]*seriesState{}
	order := make([]string, 0)
	memo := newLabelMemo(len(samples))
	for _, s := range samples {
		labels := memo.normalize(s)
		key := format.CanonicalKey(labels)
		st, ok := bySeries[key]
		if !ok {
			st = &seriesState{labels: labels}
			bySeries[key] = st
			order = append(order, key)
		}
		st.rows = append(st.rows, s)
	}
	sort.Strings(order)

	out := make([]MatrixSample, 0, len(bySeries))
	for _, key := range order {
		st := bySeries[key]
		sort.Slice(st.rows, func(i, j int) bool { return st.rows[i].Timestamp.Before(st.rows[j].Timestamp) })
		ms := MatrixSample{Metric: st.labels}
		for _, r := range st.rows {
			stamp := float64(r.Timestamp.UnixMilli()) / 1e3
			ms.Values = append(ms.Values, Sample{stamp, strconv.FormatFloat(r.Value, 'f', -1, 64)})
		}
		out = append(out, ms)
	}
	return out
}

// maxResolutionPoints caps the returned points per timeseries on a range
// query: (end-start)/step must not exceed it. Aliases the shared
// format.MaxResolutionPoints so the Prom, Loki, and Tempo heads enforce one
// ceiling. Mirrors upstream Prometheus's web/api/v1.queryRange (sufficient for
// 60s resolution over a week, or 1h over a year), so Prom clients see
// identical behaviour.
const maxResolutionPoints = format.MaxResolutionPoints

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
	// For safety, limit the number of returned points per timeseries.
	// This is sufficient for 60s resolution for a week or 1h resolution
	// for a year. Mirrors upstream Prometheus web/api/v1.queryRange —
	// same condition, same errorType, same message — so clients that
	// already handle Prom's resolution cap see identical behaviour.
	// Placed before the scalar fold so `1+1`-style queries are capped
	// too (upstream rejects them as well; the check runs before the
	// engine is consulted).
	if end.Sub(start)/step > maxResolutionPoints {
		// Pre-parse cap rejection: no CH query runs, so query_log can never
		// reflect it. Record a decision-only "rejected" corpus row.
		h.Engine.ObserveCapRejection("promql")
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New(format.ResolutionCapMessage))
		return
	}

	ctx, cancel, ok := h.applyQueryTimeout(w, r)
	if !ok {
		return
	}
	defer cancel()
	ctx = h.applyChaosSleep(ctx, r)

	// Parse up front: the expression type gate below and the scalar
	// fold both need the AST. Mirrors upstream Prometheus's
	// web/api/v1.queryRange ordering (parse → type check → engine).
	expr, err := h.parseExpr(ctx, q)
	if err != nil {
		h.respondError(w, &apiError{Kind: ErrBadData, Err: err, Status: http.StatusBadRequest})
		return
	}

	// Range queries accept only Scalar / instant Vector expressions —
	// matrix- and string-typed expressions are rejected with the
	// upstream error shape (web/api/v1's invalidExprError). Without
	// this gate a top-level `up[5m]` would lower fine (the instant
	// matrix path) and silently return rows upstream refuses.
	if t := expr.Type(); t != promparser.ValueTypeVector && t != promparser.ValueTypeScalar {
		writeError(w, http.StatusBadRequest, ErrBadData,
			fmt.Errorf("invalid expression type %q for range query, must be Scalar or instant Vector", promparser.DocumentedType(t)))
		return
	}

	// Scalar-only PromQL → matrix of one series at every step holding
	// the folded constant. Matches Prom's `1+1` over query_range
	// (single series, no labels, every step bucket populated).
	if value, ok := promql.TryFoldScalar(expr); ok {
		writeJSON(w, http.StatusOK, Response{
			Status: "success",
			Data:   &QueryData{ResultType: "matrix", Result: scalarMatrix(value, start, end, step)},
		})
		return
	}

	cursor, hdr, queryID, err := h.executeRangeStreaming(ctx, q, start, end, step)
	if err != nil {
		h.respondError(w, err)
		return
	}
	defer func() {
		if err := cursor.Close(); err != nil {
			h.Logger.Warn("cerberus prom: cursor close failed", "err", err)
		}
	}()

	result, err := matrixFromCursor(cursor, start, end, step)
	if err != nil {
		// A cerberus-side outcome surfaced during the drain. A sample-budget 422
		// fires after a clean CH finish (query_log shows ok with real cost), so it
		// is stamped onto the dispatch record (cost retained, exit overridden); a
		// memory-cap abort is recorded terminally so the corpus does not depend on
		// the query_log join landing a row.
		h.Engine.ObserveDrainOutcome(queryID, "promql", err)
		h.respondError(w, classifyDrainError(err))
		return
	}
	// Surface the streaming-path drain count (rows pulled off the cursor =
	// the buffer matrixFromCursor accumulated) to the test-observable hook
	// when one is installed. Mirrors the eager path's Result.Inspected /
	// Tempo's SearchMetrics.InspectedTraces; nil in production.
	if h.onRangeDrain != nil {
		h.onRangeDrain(cursor.Inspected())
	}
	writeEngineHeaders(w, hdr)
	writeJSON(w, http.StatusOK, Response{
		Status: "success",
		Data:   &QueryData{ResultType: "matrix", Result: result},
	})
}

// applyQueryTimeout derives the request context every query handler runs
// under, honouring the standard Prometheus `?timeout=<duration>` query
// param. It resolves the effective per-query wall-clock budget — the
// configured default (h.QueryTimeout) min'd with the request's ?timeout=
// (Prometheus uses the smaller of the two), treating 0 on either side as
// "no cap from that source" — and, when the budget is positive, threads
// it onto the returned context as BOTH:
//
//   - a context deadline (context.WithTimeout), so a query that hangs
//     past the budget unblocks the handler and releases its admit slot +
//     pooled connection even if the server-side cap somehow doesn't fire;
//     and
//   - chclient.WithQueryTimeout, so the data-plane query's ClickHouse
//     max_execution_time is narrowed to the same budget and the server
//     aborts the query with TIMEOUT_EXCEEDED (code 159) → *QueryTimeoutError.
//
// The returned cancel MUST be deferred by the caller (it is a no-op when
// no deadline was installed). A malformed ?timeout= is a 400 bad_data
// (matching upstream Prometheus); ok=false signals the caller already
// wrote the error and must return.
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
// non-positive value as "unbounded" (so it never wins the min). When
// both are non-positive the result is 0 (no cap). This mirrors
// Prometheus's effective-timeout rule: the engine uses the smaller of
// the global query.timeout and the per-request ?timeout=, and a disabled
// (zero) cap on either side does not clamp the other.
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

// parseExpr wraps the prom parser in a `parse` pipeline-stage span. The
// QL identifier and the (truncated) query string land on the span as
// `cerberus.ql` + `cerberus.query`.
//
// Before parsing, the query passes through normalizeDottedSelectors,
// which rewrites OTel-style dotted metric names (e.g.
// `http.server.request.duration`) to the explicit `{__name__="..."}`
// form. PromQL's parser only accepts ASCII identifiers in selector
// position; the rewrite lets users keep typing the OTel name they see
// in Grafana's metric picker without a 400 parse error.
func (h *Handler) parseExpr(ctx context.Context, query string) (promparser.Expr, error) {
	_, span := tracer.Start(ctx, cerbtrace.SpanParse,
		trace.WithAttributes(cerbtrace.ParseAttrs("promql", query)...))
	defer span.End()
	expr, err := h.parser.ParseExpr(normalizeDottedSelectors(query))
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
//
// step is threaded through to the PromQL lang adapter so the
// "no-driving-vector" lowerings (`time()`, `vector(N)`, zero-arg date
// fns, `absent(...)`) emit one sample per step across [start, end]
// instead of a single row at the eval anchor. Without this threading
// the matrix pivot below would drop those rows outside the 5-minute
// lookback window, producing the empty-matrix shape Pool-O/Pool-S2
// surfaced as 54 compat-lane diffs.
func (h *Handler) executeRangeStreaming(
	ctx context.Context,
	query string,
	start, end time.Time,
	step time.Duration,
) (chclient.Cursor, map[string]string, string, error) {
	l := &lang{
		Parser:   h.parser,
		Schema:   h.Schema,
		Start:    start,
		End:      end,
		Step:     step,
		Lowerers: h.Lowerers,
	}
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
		return nil, nil, "", classifyEngineError(err)
	}
	h.Logger.Debug("cerberus query_range (stream)", "promql", query, "sql", res.SQL, "args", res.Args)
	return res.Cursor, res.Headers, res.QueryID, nil
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

// promMaxSamplesMessage is upstream Prometheus's exact wire message for
// a query that crosses --query.max-samples: the promql engine raises
// ErrTooManySamples(env) with env = "query execution" (see
// promql/engine.go in the pinned tsouza/prometheus fork), and the v1
// API maps it to HTTP 422 errorType=execution. Cerberus mirrors the
// message verbatim so clients that already parse Prom's budget
// rejection see identical behaviour.
const promMaxSamplesMessage = "query processing would load too many samples into memory in query execution"

// tooManySamplesAPIError is the Prometheus-parity rejection for a
// sample-budget exceedance: HTTP 422, errorType "execution",
// upstream's exact wire message. Shared by classifyEngineError (eager
// drain inside engine.Query) and classifyDrainError (handler-side
// cursor drain).
func tooManySamplesAPIError() *apiError {
	return &apiError{
		Kind:   ErrExecution,
		Err:    errors.New(promMaxSamplesMessage),
		Status: http.StatusUnprocessableEntity,
	}
}

// promMemoryLimitMessage is the CH-side sibling of
// promMaxSamplesMessage: the wire message for a query ClickHouse
// aborted with MEMORY_LIMIT_EXCEEDED (code 241). There is no upstream
// Prometheus message to mirror verbatim here (Prometheus has no
// server-side SQL engine), so the message keeps Prometheus's
// resource-exhausted phrasing style and honestly names the ClickHouse
// per-query memory cap that fired. When no per-query cap is configured
// (CERBERUS_CH_QUERY_MAX_MEMORY=0) the rejection came from a ClickHouse
// server-side limit and the message says so without inventing a cap.
func promMemoryLimitMessage(limit int64) string {
	if limit > 0 {
		return fmt.Sprintf(
			"query processing would use too much memory in query execution (ClickHouse memory limit exceeded; per-query cap %d bytes)",
			limit,
		)
	}
	return "query processing would use too much memory in query execution (ClickHouse memory limit exceeded)"
}

// memoryLimitAPIError is the resource-exhausted rejection for a
// ClickHouse memory-limit abort: HTTP 422, errorType "execution" —
// the exact wire shape of the sample-budget rejection (#746), because
// the two are the same class of error (per-query resource cap, CH
// healthy, breaker-neutral). Shared by classifyEngineError (open-time
// 241 surfacing through engine.Query) and classifyDrainError
// (mid-stream 241 surfacing via cursor.Err() — k3d run 27277793810).
func memoryLimitAPIError(e *chclient.MemoryLimitError) *apiError {
	return &apiError{
		Kind:   ErrExecution,
		Err:    errors.New(promMemoryLimitMessage(e.Limit)),
		Status: http.StatusUnprocessableEntity,
	}
}

// queryTimeoutAPIError is the head-idiomatic rejection for a query that
// exceeded its wall-clock budget — HTTP 503, errorType "timeout",
// mirroring upstream Prometheus's web/api/v1 handling of a query that
// hits `query.timeout` (ErrQueryTimeout → 503 errorType=timeout). It
// covers both the ClickHouse server-side abort (TIMEOUT_EXCEEDED, code
// 159 → *QueryTimeoutError) and a context-deadline expiry on the
// handler-installed budget (context.DeadlineExceeded). It is NOT a 5xx
// server fault: the query ran exactly as long as it was allowed to, then
// the cap fired — CH is healthy, so this is breaker-neutral too.
func queryTimeoutAPIError(err error) *apiError {
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

// isQueryTimeout reports whether err is a per-query wall-clock timeout —
// either the ClickHouse server-side TIMEOUT_EXCEEDED rejection
// (chclient.ErrQueryTimeout) or the handler's own context-deadline
// expiry (context.DeadlineExceeded), the latter being how a query that
// hangs past the budget without the server-side cap firing surfaces. A
// plain context.Canceled (client walked away) is deliberately NOT
// treated as a timeout — that maps to the canceled wire shape elsewhere.
func isQueryTimeout(err error) bool {
	return errors.Is(err, chclient.ErrQueryTimeout) || errors.Is(err, context.DeadlineExceeded)
}

// classifyDrainError maps errors surfaced while draining a query_range
// cursor (matrixFromCursor → cursor.Err()). The sample-budget sentinel
// becomes the Prometheus-parity 422; the wall-clock timeout becomes a
// 503 errorType=timeout; everything else keeps the transport-failure 502
// shape. These errors occur AFTER the cursor open succeeded, so they are
// never recorded against the chclient circuit breaker — this mapping is
// purely a wire-shape concern.
func classifyDrainError(err error) error {
	if errors.Is(err, chclient.ErrTooManySamples) {
		return tooManySamplesAPIError()
	}
	var memLimit *chclient.MemoryLimitError
	if errors.As(err, &memLimit) {
		return memoryLimitAPIError(memLimit)
	}
	if isQueryTimeout(err) {
		return queryTimeoutAPIError(err)
	}
	return &apiError{Kind: ErrInternal, Err: err, Status: http.StatusBadGateway}
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
	// Sample-budget exceedance (instant path: engine.Query drains the
	// cursor inside chclient.Client.Query, so the sentinel arrives
	// wrapped as `engine: execute: ...`). Prometheus parity: 422
	// errorType=execution with the upstream wire message.
	if errors.Is(err, chclient.ErrTooManySamples) {
		return tooManySamplesAPIError()
	}
	// ClickHouse memory-limit abort (code 241): the same
	// resource-exhausted class as the sample budget, surfaced from the
	// server side instead of the client-side drain. 422
	// errorType=execution; never a 5xx — CH is healthy when it
	// enforces a cap.
	var memLimit *chclient.MemoryLimitError
	if errors.As(err, &memLimit) {
		return memoryLimitAPIError(memLimit)
	}
	// Wall-clock timeout (open path): the data-plane query hit its
	// max_execution_time cap (TIMEOUT_EXCEEDED → *QueryTimeoutError) or
	// the handler's context deadline fired before the cursor opened. 503
	// errorType=timeout, mirroring upstream Prometheus; never a 5xx — the
	// query ran exactly as long as it was allowed to.
	if isQueryTimeout(err) {
		return queryTimeoutAPIError(err)
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
//     it as empty string. The matrix RangeWindow exposes the per-row
//     anchor as the literal column `anchor_ts` (no inner alias to
//     s.TimestampColumn — emitWindowedArrayMatrix emits it raw); this
//     Project renames `anchor_ts` → s.TimestampColumn on the way out via
//     the projection's own Alias. The instant case has to synthesise
//     via now64().
//
// Project transparency: PromQL lowerings like `projectValueOverInner`
// (clamp / abs / unary minus / `quantile_over_time(out-of-range, ...)`
// Inf-Value fold) wrap a RangeWindow / Aggregate with a Project whose
// projections are the same (group-keys, Value) shape the inner already
// exposes — i.e., the Project replaces only the Value expression and
// does NOT widen the column set. Such Projects pass the derived-shape
// gate through; otherwise the canonical-shape branch would generate
// `SELECT MetricName, TimeUnix, ... FROM (<two-column derived>)` and
// real CH 24.x rejects the missing-column reference as 502. The
// projectionExposesCanonical check distinguishes these "value-rewrite"
// Projects from the canonical-shape Projects upstream lowerings (LWR,
// instant fns over `temperature`, etc.) emit.
func wrapWithSampleProjection(plan chplan.Node, s schema.Metrics) chplan.Node {
	projections := []chplan.Projection{
		{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}},
		{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}},
		{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}},
		{Expr: &chplan.ColumnRef{Name: s.ValueColumn}},
	}
	if isDerivedShape(plan, s) {
		// TimeUnix source: a matrix-shape RangeWindow exposes a real per-row
		// timestamp under the literal column `anchor_ts`, which the emitter
		// keeps offset-SHIFTED (the window/rate math keys off the shifted
		// window edge). PromQL's `offset` shifts only WHICH samples a reducing
		// window reads, not the timestamp the result is reported at, so for a
		// reducing rate/increase/*_over_time window with a non-zero offset the
		// reported timestamp is anchor_ts + Offset (the unshifted request
		// grid); reading raw `anchor_ts` here re-shifted every offset matrix
		// query's output past this handler's projection, invisible to the spec
		// goldens (which never wrap in this projection). A raw range vector /
		// subquery (Identity) reports each sample at its ACTUAL, offset-shifted
		// time, so it is left un-relabeled. The instant case synthesises via
		// now64().
		var tsExpr chplan.Expr
		if isMatrixRangeWindow(plan) {
			tsExpr = &chplan.ColumnRef{Name: chplan.RangeWindowAnchorColumn}
			if off, relabel := matrixWindowOffset(plan); relabel {
				tsExpr = &chplan.Binary{
					Op:   chplan.OpAdd,
					Left: &chplan.ColumnRef{Name: chplan.RangeWindowAnchorColumn},
					Right: &chplan.FuncCall{
						Name: "toIntervalNanosecond",
						Args: []chplan.Expr{&chplan.LitInt{V: off.Nanoseconds()}},
					},
				}
			}
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
// as a per-row column. Set by PromQL subquery lowering.
//
// "Plan root" here is "after walking past any value-rewrite Projects"
// — `projectValueOverInner` (RangeWindow case) drops a Project on top
// that keeps the same `[Attributes, ..., Value]` shape, including the
// `anchor_ts` passthrough when the inner is matrix-shape. The outer
// Project's projections still reference `anchor_ts` by name, so the
// `wrapWithSampleProjection` matrix branch can keep doing the same.
// matrixWindowOffset walks to the matrix window at plan's root and reports its
// PromQL offset and whether that offset shifts the REPORTED timestamp. Only a
// reducing (non-identity) RangeWindow / RangeWindowNative reports on the
// unshifted request grid, so its shifted anchor_ts must be un-shifted by adding
// Offset back. A raw range vector / subquery (RangeWindow.Identity) reports each
// sample at its actual, offset-shifted time and is left alone; a zero offset
// needs no relabel. Mirrors the emitter's gridAnchorFrag so the bare-selector
// (wrapWithSampleProjection) and wrapping-aggregate (grid-keyed) paths agree.
func matrixWindowOffset(plan chplan.Node) (offset time.Duration, relabel bool) {
	switch v := plan.(type) {
	case *chplan.RangeWindow:
		return v.Offset, v.Offset != 0 && !v.Identity
	case *chplan.RangeWindowNative:
		return v.Offset, v.Offset != 0
	case *chplan.Project:
		return matrixWindowOffset(v.Input)
	case *chplan.Filter:
		return matrixWindowOffset(v.Input)
	}
	return 0, false
}

func isMatrixRangeWindow(plan chplan.Node) bool {
	switch v := plan.(type) {
	case *chplan.RangeWindow:
		return v.OuterRange > 0
	case *chplan.RangeWindowNative:
		// The native timeSeriesRateToGrid path is always matrix-shape: it
		// explodes the grid into one row per anchor and surfaces the
		// per-row `anchor_ts` column, exactly like the fan-out matrix
		// RangeWindow. The TimeUnix source is therefore that column, not
		// the now64() instant synthesis.
		return true
	case *chplan.Project:
		return isMatrixRangeWindow(v.Input)
	case *chplan.Filter:
		return isMatrixRangeWindow(v.Input)
	}
	return false
}

// synthesizedAnchor returns the CH expression cerberus stamps on
// rate / count_over_time / … sample rows for query_range bucketing.
// Equivalent to `now64(9) - toIntervalNanosecond(5e9)` — 5 seconds
// before CH-now. See the docstring on wrapWithSampleProjection's
// derived-shape branch for the rationale.
func synthesizedAnchor() chplan.Expr {
	return chplan.NowNanoMinusStaleness()
}

// isDerivedShape reports whether the plan's output schema lacks the
// canonical Sample columns (MetricName / TimeUnix / Value as-is) and
// has only the (group-keys…, value) shape produced by RangeWindow,
// Aggregate, or a Filter on top of one of those.
//
// A Project on top of a derived shape stays derived UNLESS its own
// projections name all four canonical Sample columns as outputs —
// that's the LWR `Project [MetricName, Attributes, TimeUnix, Value]`
// shape lowered for canonical-shape consumers. The
// projectValueOverInner Project (clamp / abs / instant fn over
// RangeWindow, plus the quantile_over_time out-of-range fold from
// PR #322) carries only `[Attributes, ..., Value]` over a derived
// inner, and must not be classified as canonical because the inner
// scope doesn't carry MetricName / TimeUnix — real CH 24.x rejects
// the missing-column reference with a 502 on `query_range`.
func isDerivedShape(plan chplan.Node, s schema.Metrics) bool {
	switch v := plan.(type) {
	case *chplan.RangeWindow, *chplan.Aggregate, *chplan.RangeWindowNative:
		// RangeWindowNative emits the same derived (group-keys…, anchor_ts,
		// value) shape as the fan-out RangeWindow — MetricName never exists
		// in that scope, so it must be synthesised as the empty string
		// rather than referenced as a bare column (CH would 502 with
		// "Unknown expression identifier MetricName").
		return true
	case *chplan.Filter:
		return isDerivedShape(v.Input, s)
	case *chplan.Project:
		if projectionExposesCanonical(v, s) {
			return false
		}
		return isDerivedShape(v.Input, s)
	}
	return false
}

// projectionExposesCanonical reports whether p's projections name all
// four canonical Sample column outputs (MetricName / Attributes /
// TimeUnix / Value). An output is "named" when either Projection.Alias
// matches, or the Projection.Expr is a bare ColumnRef to the canonical
// column name with no Alias rewrite (the canonical column passes
// through under its own name).
//
// We only treat this as canonical when ALL four names are present —
// `projectValueOverInner` (RangeWindow case) emits a two-output
// Project (`Attributes`, `Value`) over a derived inner, so missing
// MetricName / TimeUnix correctly disqualifies it.
func projectionExposesCanonical(p *chplan.Project, s schema.Metrics) bool {
	needed := map[string]bool{
		s.MetricNameColumn: false,
		s.AttributesColumn: false,
		s.TimestampColumn:  false,
		s.ValueColumn:      false,
	}
	for _, proj := range p.Projections {
		name := projectionOutputName(proj)
		if _, ok := needed[name]; ok {
			needed[name] = true
		}
	}
	for _, ok := range needed {
		if !ok {
			return false
		}
	}
	return true
}

// projectionOutputName returns the column name a Projection exposes:
// the explicit Alias when set, otherwise the bare-ColumnRef name when
// the Expr is a column reference. Computed Exprs without an Alias
// return "" — the caller treats that as "no canonical column exposed
// at this slot", which is the conservative answer for the
// projectExposesCanonical check.
func projectionOutputName(p chplan.Projection) string {
	if p.Alias != "" {
		return p.Alias
	}
	if cr, ok := p.Expr.(*chplan.ColumnRef); ok {
		return cr.Name
	}
	return ""
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
	memo := newLabelMemo(len(samples))
	for _, s := range samples {
		labels := memo.normalize(s)
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

// matrixFromCursor drains the cursor row-by-row and emits one Matrix
// sample per row at the row's TimeUnix.
//
// The Pool-AK range-mode rework makes every PromQL `query_range`
// plan emit one row per (series, step anchor) on the SQL side —
// each row already carries the per-step LWR-resolved value at the
// correct anchor timestamp. The matrix-shape RangeWindow path (rate
// / *_over_time / subquery) follows the same per-anchor contract.
// So the matrix pivot is now a trivial row → sample copy keyed by
// canonical series key, with no step-grid iteration or carry-forward
// dance.
//
// Empty-window anchors are dropped by the SQL itself (the per-step
// LWR yields no row when no sample falls in `(anchor-5m, anchor]`;
// the matrix RangeWindow's `length(window_vals) >= N` predicate
// drops empty rate / *_over_time windows). The pivot mirrors that
// behaviour by simply not emitting a sample at an anchor for which
// no row was returned — preserving Prom's "no sample for an empty
// staleness window" rule end-to-end.
//
// Rows whose Timestamp falls outside `[start, end]` are clipped so a
// drifted server-side `now64(9)` (legacy bare-selector instant
// shapes) never lands a stray point past the request window.
//
// Memory complexity: O(rows) total in the per-series buffers. The
// eventual fully-streaming variant (one series at a time, flushed on
// canonicalKey boundary changes) requires the SQL emission to
// ORDER BY (series_key, ts).
func matrixFromCursor(
	cursor chclient.Cursor,
	start, end time.Time,
	_ time.Duration,
) ([]MatrixSample, error) {
	type seriesState struct {
		labels map[string]string
		rows   []chclient.Sample
	}

	bySeries := map[string]*seriesState{}
	// order records first-seen canonical keys so the output series order
	// is deterministic; it is sorted below so the matrix is emitted in
	// canonical label order. Reference Prometheus returns range-query
	// series sorted by labels, and the prometheus/compliance differential
	// tester compares the two `model.Matrix` slices ORDER-SENSITIVELY
	// (cmp.Diff, no pre-sort) — so an unsorted matrix (Go map iteration
	// order) diffs against reference even when every series + sample is
	// identical. The instant sibling matrixFromSamples already sorts; this
	// path must match. (Compat query `{job="demo", __name__!~"..."}`
	// diverged purely on series order before this.)
	order := make([]string, 0)
	// Memoise the per-row label normalisation by interned-map identity so
	// a series with K samples normalises once, not K times — see labelMemo.
	memo := newLabelMemo(0)
	for cursor.Next() {
		s := cursor.Sample()
		labels := memo.normalize(s)
		key := format.CanonicalKey(labels)
		st, ok := bySeries[key]
		if !ok {
			st = &seriesState{labels: labels}
			bySeries[key] = st
			order = append(order, key)
		}
		st.rows = append(st.rows, s)
	}
	if err := cursor.Err(); err != nil {
		return nil, err
	}
	sort.Strings(order)

	out := make([]MatrixSample, 0, len(bySeries))
	for _, key := range order {
		st := bySeries[key]
		// Inline insertion sort by Timestamp ascending — rows are
		// typically already nearly sorted from CH but the CrossJoin
		// + Aggregate plan shapes the rework introduces do not
		// guarantee row order across (series, anchor) pairs.
		for i := 1; i < len(st.rows); i++ {
			for j := i; j > 0 && st.rows[j-1].Timestamp.After(st.rows[j].Timestamp); j-- {
				st.rows[j-1], st.rows[j] = st.rows[j], st.rows[j-1]
			}
		}

		ms := MatrixSample{Metric: st.labels}
		for _, r := range st.rows {
			if r.Timestamp.Before(start) || r.Timestamp.After(end) {
				continue
			}
			stamp := float64(r.Timestamp.UnixMilli()) / 1e3
			ms.Values = append(ms.Values, Sample{stamp, strconv.FormatFloat(r.Value, 'f', -1, 64)})
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
	// A bare per-query resource-limit rejection — the sample budget (now
	// enforced on metadata drains too, chclient.drainBudgetExceeded) or the CH
	// memory cap — maps to the same Prom 422 the matrix path gives via
	// classifyDrainError, rather than the generic 500 below. A callsite that
	// pre-wrapped its own *apiError already returned above, so this only
	// reclassifies the raw sentinels.
	if errors.Is(err, chclient.ErrTooManySamples) {
		ae := tooManySamplesAPIError()
		writeError(w, ae.Status, ae.Kind, ae.Err)
		return
	}
	var memLimit *chclient.MemoryLimitError
	if errors.As(err, &memLimit) {
		ae := memoryLimitAPIError(memLimit)
		writeError(w, ae.Status, ae.Kind, ae.Err)
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

// panicEnvelope is the [telemetry.PanicRenderer] for the Prom head: when
// QueryMiddleware recovers a handler panic before any response was
// committed, it renders the canonical Prom 500 envelope so Grafana sees
// `{status:"error", errorType:"internal"}` instead of a dropped
// connection. The recovered value + stack are logged by the middleware
// via the OTLP slog bridge; this only shapes the wire response.
func panicEnvelope(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusInternalServerError, ErrInternal,
		errors.New("internal server error"))
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
