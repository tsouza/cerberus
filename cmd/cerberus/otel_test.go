package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// TestWrapWithOTel_EmitsRouteSpan exercises the full R4.2 wiring path: a
// real http.ServeMux with registered patterns, wrapped via wrapWithOTel,
// served through httptest, and verified via an in-memory span recorder.
// The assertion is purposefully tight: one span, named after the matched
// pattern. This is the contract R4.3 depends on when it attaches child
// spans for parse / lower / optimize / emit.
func TestWrapWithOTel_EmitsRouteSpan(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })

	// Install our recording provider as the process global — otelhttp
	// pulls the tracer from there. installOTel also re-installs the
	// W3C+Baggage propagator (idempotent across tests).
	installOTel(tp)
	t.Cleanup(func() { installOTel(nil) })

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/query", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := wrapWithOTel(mux, "cerberus")

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/query?query=up")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("span count: got %d want 1; spans=%v", len(spans), spans)
	}
	got := spans[0].Name
	want := "GET /api/v1/query"
	if got != want {
		t.Errorf("span name: got %q want %q", got, want)
	}
}

// TestWrapWithOTel_FallbackSpanName covers the 404 / unmatched-route case:
// http.ServeMux leaves r.Pattern empty, so our formatter must fall back
// to "HTTP <METHOD>" rather than leak the full URL (high-cardinality
// trap that would blow up the Tempo backend).
func TestWrapWithOTel_FallbackSpanName(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })
	installOTel(tp)
	t.Cleanup(func() { installOTel(nil) })

	mux := http.NewServeMux() // no routes registered
	handler := wrapWithOTel(mux, "cerberus")

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/no/such/route")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("span count: got %d want 1", len(spans))
	}
	if got, want := spans[0].Name, "HTTP GET"; got != want {
		t.Errorf("span name: got %q want %q", got, want)
	}
}

// TestWrapWithOTel_PropagatesTraceContext verifies the second half of
// R4.2: incoming `traceparent` headers are honored so the handler's
// http.Request.Context() carries the inbound trace ID. Grafana sets
// traceparent on every datasource query — without this, cerberus spans
// would be orphaned from the user-initiated trace.
func TestWrapWithOTel_PropagatesTraceContext(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })
	installOTel(tp)
	t.Cleanup(func() { installOTel(nil) })

	mux := http.NewServeMux()
	mux.HandleFunc("GET /ping", func(w http.ResponseWriter, r *http.Request) {
		// The handler must see a context whose span context derives
		// from the inbound traceparent header.
		sc := trace.SpanContextFromContext(r.Context())
		if !sc.IsValid() {
			t.Errorf("handler context: span context invalid; want propagated trace ID")
		}
		if got, want := sc.TraceID().String(), "0af7651916cd43dd8448eb211c80319c"; got != want {
			t.Errorf("trace ID: got %s want %s", got, want)
		}
		w.WriteHeader(http.StatusOK)
	})
	handler := wrapWithOTel(mux, "cerberus")

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/ping", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	// W3C traceparent: version-traceid-spanid-flags.
	req.Header.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
}

// TestInstallOTel_InstallsPropagator pins the propagator contract: after
// installOTel returns, otel.GetTextMapPropagator must list traceparent +
// baggage as recognized fields. Guards against an accidental regression
// that drops one of the two.
func TestInstallOTel_InstallsPropagator(t *testing.T) {
	installOTel(nil)
	t.Cleanup(func() {
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())
	})

	prop := otel.GetTextMapPropagator()
	fields := prop.Fields()
	joined := strings.Join(fields, ",")
	if !strings.Contains(joined, "traceparent") {
		t.Errorf("propagator fields missing traceparent: %v", fields)
	}
	if !strings.Contains(joined, "baggage") {
		t.Errorf("propagator fields missing baggage: %v", fields)
	}
}
