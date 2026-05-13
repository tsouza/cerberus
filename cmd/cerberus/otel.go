// otel.go wires the bare-minimum OpenTelemetry plumbing cerberus needs at
// RC4 R4.2: a no-op tracer-provider (real exporters land in R4.5), the
// W3C / Baggage propagator pair so incoming `traceparent` headers are
// honored, and a helper that wraps an HTTP handler with otelhttp so every
// request becomes a server span whose name derives from the matched
// http.ServeMux pattern.
package main

import (
	"fmt"
	"net/http"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// installOTel installs the default propagator (W3C TraceContext + Baggage)
// and the supplied tracer provider as process globals. Passing nil for tp
// installs a no-op provider — the safe default until R4.5 wires real
// OTLP exporters.
//
// Idempotent: safe to call multiple times (e.g. from a test setup helper
// alongside main's call), the last writer wins. Tests that want to
// observe spans pass their own recording provider.
func installOTel(tp trace.TracerProvider) {
	if tp == nil {
		tp = tracenoop.NewTracerProvider()
	}
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
}

// wrapWithOTel wraps next with otelhttp so every request gets a server
// span. The span name is `<METHOD> <route>` where route is the matched
// http.ServeMux pattern (Go 1.22+ exposes it via http.Request.Pattern).
// Falls back to "HTTP <METHOD>" when no pattern matched (e.g. 404), so
// span cardinality stays bounded by the route set rather than the URL
// space.
//
// `service` becomes the otelhttp operation name — kept stable across all
// routes; per-route disambiguation lives in the formatter.
func wrapWithOTel(next http.Handler, service string) http.Handler {
	return otelhttp.NewHandler(next, service,
		otelhttp.WithSpanNameFormatter(spanNameFromPattern),
	)
}

// spanNameFromPattern derives a clean span name from the matched mux
// pattern. http.ServeMux populates r.Pattern with the registered string
// (e.g. "GET /api/v1/query"); when no pattern matched (404) it's empty
// and we fall back to "HTTP <METHOD>" so we never leak full URLs.
func spanNameFromPattern(_ string, r *http.Request) string {
	if r == nil {
		return "HTTP"
	}
	if p := strings.TrimSpace(r.Pattern); p != "" {
		return p
	}
	if r.Method != "" {
		return fmt.Sprintf("HTTP %s", r.Method)
	}
	return "HTTP"
}
