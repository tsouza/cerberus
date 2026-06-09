package telemetry

import (
	"bufio"
	"errors"
	"net"
	"net/http"
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
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
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
	return h.Hijack()
}

// Flush forwards to http.Flusher when supported; harmless when not.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// QueryMiddleware wraps next so every request increments
// cerberus.queries.total and records cerberus.queries.duration.seconds
// labelled with the QL identifier, the matched route, and the outcome.
//
// ql is the constant for the API head — "promql" for prom.Handler,
// "logql" for loki.Handler, "traceql" for tempo.Handler. The route
// label is r.Pattern (Go 1.22+ exposes the matched http.ServeMux
// pattern); when it's empty (404 / unmatched) we fall back to the
// request URL's path prefix so the cardinality stays bounded.
//
// Outcome bucketing: any status >= 400 maps to ResultError, anything
// else (including the implicit-200 path where the handler never calls
// WriteHeader) is ResultOK. 4xx is counted as an error because the
// `cerberus_queries_total{result}` series is a query-outcome metric,
// not an HTTP-SLO metric: a 400 parse rejection / 422 lower rejection
// IS a failed query from the caller's point of view, and the
// "Error rate by language" dashboard panel reads this bucket to show
// users how many of their queries failed. The HTTP-layer SLO (which
// would legitimately treat 4xx as "gateway behaved correctly")
// rides the separate `http.server.*` instrument set.
func QueryMiddleware(ql string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := r.Pattern
		if route == "" {
			route = "HTTP " + strings.ToUpper(r.Method)
		}
		t := ObserveQuery(ql, route)
		sr := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(sr, r)
		result := ResultOK
		if sr.status >= 400 {
			result = ResultError
		}
		t.Done(r.Context(), result)
	})
}
