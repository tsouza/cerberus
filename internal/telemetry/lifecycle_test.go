package telemetry

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
)

// TestBuildResource_DefaultsServiceName confirms an empty ServiceName
// in Config still produces "cerberus" on the resource. Anchors the
// invariant cerberus dashboards rely on (service.name=cerberus).
func TestBuildResource_DefaultsServiceName(t *testing.T) {
	res, err := buildResource(t.Context(), Config{})
	if err != nil {
		t.Fatalf("buildResource: %v", err)
	}
	if got := attr(res, "service.name"); got != "cerberus" {
		t.Errorf("service.name = %q; want cerberus", got)
	}
}

// TestBuildResource_DefaultsServiceVersion confirms an empty
// ServiceVersion in Config falls back to "dev".
func TestBuildResource_DefaultsServiceVersion(t *testing.T) {
	res, err := buildResource(t.Context(), Config{})
	if err != nil {
		t.Fatalf("buildResource: %v", err)
	}
	if got := attr(res, "service.version"); got != "dev" {
		t.Errorf("service.version = %q; want dev", got)
	}
}

// TestBuildResource_AppliesConfiguredVersion confirms a populated
// ServiceVersion surfaces on the resource — this is how goreleaser-built
// binaries propagate their version to the OTLP backend.
func TestBuildResource_AppliesConfiguredVersion(t *testing.T) {
	res, err := buildResource(t.Context(), Config{
		ServiceName:    "cerberus",
		ServiceVersion: "v1.2.3",
	})
	if err != nil {
		t.Fatalf("buildResource: %v", err)
	}
	if got := attr(res, "service.version"); got != "v1.2.3" {
		t.Errorf("service.version = %q; want v1.2.3", got)
	}
}

// TestBuildResource_IncludesServiceInstanceID confirms the
// service.instance.id attribute is always present (so per-pod metrics
// disambiguate). The value is either the hostname or a random fallback
// — both non-empty.
func TestBuildResource_IncludesServiceInstanceID(t *testing.T) {
	res, err := buildResource(t.Context(), Config{})
	if err != nil {
		t.Fatalf("buildResource: %v", err)
	}
	if got := attr(res, "service.instance.id"); got == "" {
		t.Errorf("service.instance.id = empty; want non-empty")
	}
}

// TestBuildResource_InstanceIDMatchesHostnameWhenAvailable: when
// os.Hostname succeeds, the resource attribute should match.
func TestBuildResource_InstanceIDMatchesHostnameWhenAvailable(t *testing.T) {
	host, err := os.Hostname()
	if err != nil || host == "" {
		t.Skip("hostname unavailable on this host; skipping")
	}
	res, err := buildResource(t.Context(), Config{})
	if err != nil {
		t.Fatalf("buildResource: %v", err)
	}
	if got := attr(res, "service.instance.id"); got != host {
		t.Errorf("service.instance.id = %q; want %q", got, host)
	}
}

// TestRandomInstanceID_NotEmpty pins the fallback path — even when
// rand.Read failed we should still get a non-panicking sentinel.
func TestRandomInstanceID_NotEmpty(t *testing.T) {
	v := randomInstanceID()
	if v == "" {
		t.Error("randomInstanceID = empty; want sentinel or hex string")
	}
}

// TestNew_NoopShutdownIdempotent: the noop providers' Shutdown is safe
// to call multiple times. Cerberus's main wraps the shutdown into a
// defer + a signal-driven path; both can hit it.
func TestNew_NoopShutdownIdempotent(t *testing.T) {
	providers, err := New(t.Context(), Config{Endpoint: ""})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := providers.Shutdown(t.Context()); err != nil {
			t.Errorf("Shutdown #%d: %v", i, err)
		}
	}
}

// TestNew_SDKShutdownHonorsContextDeadline: a populated endpoint pointing
// at nothing must still let Shutdown return when the context expires.
// Critical for the main.go path that bounds shutdown to 10s.
func TestNew_SDKShutdownHonorsContextDeadline(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	providers, err := New(t.Context(), Config{
		Endpoint:       addr,
		Insecure:       true,
		Timeout:        50 * time.Millisecond,
		ServiceName:    "cerberus",
		ServiceVersion: "test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = providers.Shutdown(ctx)
		close(done)
	}()
	select {
	case <-done:
		// Shutdown returned within the test envelope — good.
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return inside the deadline")
	}
}

// TestProviders_HandsOutUsableTracer confirms the TracerProvider's
// Tracer() call returns a non-nil tracer that can mint a span without
// erroring. Sanity check that the SDK plumbing is wired correctly.
func TestProviders_HandsOutUsableTracer(t *testing.T) {
	providers, err := New(t.Context(), Config{Endpoint: ""})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tr := providers.TracerProvider.Tracer("test")
	if tr == nil {
		t.Fatal("Tracer = nil")
	}
	_, span := tr.Start(t.Context(), "noop-span")
	span.End()
}

// TestProviders_HandsOutUsableMeter mirrors the tracer sanity above for
// the metric path. Even on noop providers, Meter() must hand out a
// working meter.
func TestProviders_HandsOutUsableMeter(t *testing.T) {
	providers, err := New(t.Context(), Config{Endpoint: ""})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m := providers.MeterProvider.Meter("test")
	if m == nil {
		t.Fatal("Meter = nil")
	}
	if _, err := m.Int64Counter("smoke"); err != nil {
		t.Errorf("Int64Counter on noop: %v", err)
	}
}

// TestNew_PropagatesHeadersToExporter: when CERBERUS_OTLP_HEADERS is
// translated into Config.Headers and an endpoint is set, the SDK
// provider should still construct without error. The exporter respects
// the headers via grpc.WithMetadata internally — we can't inspect it
// from outside, but we can guard the no-panic contract.
func TestNew_PropagatesHeadersToExporter(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	providers, err := New(t.Context(), Config{
		Endpoint: addr,
		Insecure: true,
		Headers:  map[string]string{"authorization": "Bearer abc", "x-tenant": "ut"},
		Timeout:  500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = providers.Shutdown(ctx)
	})
	if providers.TracerProvider == nil {
		t.Error("TracerProvider nil after Headers config")
	}
	if providers.MeterProvider == nil {
		t.Error("MeterProvider nil after Headers config")
	}
}

// TestNew_SDKConstructorWithoutTimeout: zero Timeout must not be a
// fatal error — the SDK still works, it just uses its own default.
func TestNew_SDKConstructorWithoutTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	providers, err := New(t.Context(), Config{
		Endpoint: addr,
		Insecure: true,
		Timeout:  0,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = providers.Shutdown(ctx)
	})
}

// attr returns the string value of the named attribute on res, or ""
// when missing. Resource attributes don't expose a direct lookup — we
// scan once.
func attr(res *resource.Resource, key string) string {
	for _, kv := range res.Attributes() {
		if string(kv.Key) == key {
			if kv.Value.Type() == attribute.STRING {
				return kv.Value.AsString()
			}
		}
	}
	return ""
}
