package telemetry

import (
	"bufio"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
)

// statusRecorder is a minimal ResponseWriter wrapper that captures the
// final status code so the middleware can decide ok-vs-error after the
// handler returned without forcing handlers to expose that decision.
//
// Hijack() is implemented so the wrapper is transparent to WebSocket
// upgrades — Loki's /tail endpoint hijacks the underlying connection,
// and a recorder that didn't forward Hijack would force a 500 with
// "websocket: hijack: feature not supported" on every tail request.
// Flush() is implemented similarly so streaming handlers can chunk
// without losing the http.Flusher interface contract.
type statusRecorder struct {
	http.ResponseWriter
	status int
	// wrote is set the moment any byte (status line or body) has gone to
	// the client. The panic-recovery path reads it to decide whether it
	// can still synthesize a clean 500 error envelope: if the handler
	// already committed a status line (or streamed body bytes) before
	// panicking, the wire is past the point of no return and the
	// recovery can only log + count, not re-render.
	wrote bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.wrote = true
	r.ResponseWriter.WriteHeader(code)
}

// Write marks the response as committed (an implicit 200 if the handler
// streamed body bytes without an explicit WriteHeader) so the recovery
// path knows the envelope can no longer be rendered.
func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	r.wrote = true
	return r.ResponseWriter.Write(b)
}

// Hijack delegates to the underlying ResponseWriter when it implements
// http.Hijacker; once hijacked, the recorder marks the request as ok
// (status=200) so the post-handler classification doesn't trip the
// error bucket on a clean WebSocket upgrade.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("telemetry: ResponseWriter does not implement http.Hijacker")
	}
	if r.status == 0 {
		r.status = http.StatusSwitchingProtocols
	}
	r.wrote = true
	return h.Hijack()
}

// Flush forwards to http.Flusher when supported; harmless when not.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// PanicRenderer renders a head's own 500 error envelope (the Prom /
// Loki / Tempo JSON shape) when a handler in the request path panics
// before it had a chance to write any response. It is invoked by
// [QueryMiddleware] only when nothing has been committed to the wire
// yet, so it owns the full response: it must call WriteHeader(500) and
// write the body. Each head passes the closure that knows its own
// envelope shape — telemetry stays decoupled from per-head wire formats.
type PanicRenderer func(w http.ResponseWriter, r *http.Request)

// QueryMiddleware wraps next so every request increments
// cerberus.queries.total and records cerberus.queries.duration.seconds,
// and so a panic in the request path produces a clean per-head 500
// error envelope instead of a dropped TCP connection.
//
// ql is the constant for the API head — "promql" for prom.Handler,
// "logql" for loki.Handler, "traceql" for tempo.Handler. The route
// label is r.Pattern (Go 1.22+ exposes the matched http.ServeMux
// pattern); when it's empty (404 / unmatched) we fall back to the
// request method so the cardinality stays bounded.
//
// Outcome bucketing: any status >= 400 maps to ResultError, anything
// else (including the implicit-200 path where the handler never calls
// WriteHeader) is ResultOK. 4xx is counted as an error because the
// `cerberus_queries_total{result}` series is a query-outcome metric,
// not an HTTP-SLO metric: a 400 parse rejection / 422 lower rejection
// IS a failed query from the caller's point of view, and the
// "Error rate by language" dashboard panel reads this bucket to show
// users how many of their queries failed. The HTTP-layer SLO (which
// would legitimately treat 4xx as "gateway behaved correctly") rides
// the separate `http.server.*` instrument set.
//
// renderPanic is the head's own 500-envelope writer (see [PanicRenderer]).
// When a panic unwinds through next.ServeHTTP, the deferred recovery:
//
//  1. logs the recovered value + stack at ERROR via slog.Default()
//     (the OTLP slog bridge ships it to the collector's otel_logs);
//  2. renders the head's 500 envelope via renderPanic — but only if the
//     handler hadn't already committed a status line / body, in which
//     case the wire is past the point of re-rendering and we only log;
//  3. records the query on cerberus.queries.* as ResultError.
//
// Step 3 runs in its own deferred call that always fires — even on
// panic or early return — so the failed query is never silently dropped
// from cerberus_queries_total / cerberus_query_duration. Before this,
// t.Done() ran inline after ServeHTTP and a panic skipped it entirely,
// leaving the metrics unbalanced and the OTLP outcome unrecorded.
func QueryMiddleware(ql string, renderPanic PanicRenderer, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := r.Pattern
		if route == "" {
			route = "HTTP " + strings.ToUpper(r.Method)
		}
		t := ObserveQuery(ql, route)
		sr := &statusRecorder{ResponseWriter: w}
		// panicked is set by the recover defer so the metric defer can
		// bucket the query as an error even when the handler had already
		// committed a 2xx status line before panicking (a truncated
		// success is still a failed query from the caller's point of view).
		panicked := false

		// Outermost defer: classify + record the outcome on
		// cerberus.queries.*. Deferred so it fires on the normal return,
		// on an early return inside next, AND on a panic that the inner
		// defer recovered (the recover re-runs the deferred chain). Any
		// status >= 400 — including the synthesized 500 from a recovered
		// panic — buckets as ResultError, keeping the {result} label
		// cardinality fixed at {ok, error}. Without the defer the panic
		// would skip t.Done() entirely and the failed query would never
		// land on cerberus_queries_total / cerberus_query_duration.
		defer func() {
			result := ResultOK
			if panicked || sr.status >= 400 {
				result = ResultError
			}
			t.Done(r.Context(), result)
		}()

		// Inner defer: recover a handler panic, log it via the OTLP slog
		// bridge, and synthesize the head's 500 envelope. Registered after
		// the metric defer, so it runs first on unwind and the panicked
		// flag it sets is visible to the metric classification above.
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			panicked = true
			slog.Default().Error(
				"cerberus handler panic recovered",
				"cerberus.ql", ql,
				"cerberus.route", route,
				"panic", rec,
				"stack", string(debug.Stack()),
			)
			// If the handler already committed a status / body, the wire
			// is past re-rendering; we've logged + flagged the error, so
			// just unwind.
			if sr.wrote {
				return
			}
			sr.status = http.StatusInternalServerError
			renderPanic(sr, r)
		}()

		next.ServeHTTP(sr, r)
	})
}
