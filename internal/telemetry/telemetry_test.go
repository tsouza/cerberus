package telemetry

import (
	"context"
	"net"
	"testing"
	"time"

	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// TestNew_NoopWhenEndpointEmpty pins the zero-collector-dependency
// default: an empty endpoint installs noop providers and Shutdown is a
// no-op. This is the production-safe path that ships when an operator
// hasn't pointed cerberus at a collector yet.
func TestNew_NoopWhenEndpointEmpty(t *testing.T) {
	providers, err := New(t.Context(), Config{Endpoint: ""})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, isSDK := providers.TracerProvider.(*sdktrace.TracerProvider); isSDK {
		t.Errorf("TracerProvider is SDK provider; want noop")
	}
	if _, isSDK := providers.MeterProvider.(*sdkmetric.MeterProvider); isSDK {
		t.Errorf("MeterProvider is SDK provider; want noop")
	}
	// Smoke: both providers can still hand out a tracer/meter.
	_ = providers.TracerProvider.Tracer("test")
	_ = providers.MeterProvider.Meter("test")

	if err := providers.Shutdown(t.Context()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

// TestNew_BuildsSDKProvidersWhenEndpointSet verifies that a populated
// endpoint produces SDK-backed providers (real exporters) instead of
// the noop pair. We don't dial the endpoint here — gRPC client creation
// is lazy, so New returns synchronously with a working provider even
// without a listener. Shutdown afterward must not block indefinitely;
// we bound it with a short context.
func TestNew_BuildsSDKProvidersWhenEndpointSet(t *testing.T) {
	// Reserve a port nothing is listening on. We immediately close the
	// listener so the OTLP client's eventual connect attempt fails fast,
	// but the SDK provider construction itself doesn't care.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	providers, err := New(t.Context(), Config{
		Endpoint:       addr,
		Insecure:       true,
		Timeout:        1 * time.Second,
		ServiceName:    "cerberus",
		ServiceVersion: "test",
		Headers:        map[string]string{"x-tenant": "ut"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := providers.TracerProvider.(*sdktrace.TracerProvider); !ok {
		t.Errorf("TracerProvider type = %T; want *sdktrace.TracerProvider", providers.TracerProvider)
	}
	if _, ok := providers.MeterProvider.(*sdkmetric.MeterProvider); !ok {
		t.Errorf("MeterProvider type = %T; want *sdkmetric.MeterProvider", providers.MeterProvider)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := providers.Shutdown(ctx); err != nil {
		// Shutdown may report a connection error since nothing's
		// listening — that's fine, the contract is "best effort flush
		// + tear down, return what you saw". The test just guards
		// against a panic or hang.
		t.Logf("Shutdown error (expected when no listener): %v", err)
	}
}

// TestProviders_ShutdownNil makes sure the nil-receiver path doesn't
// panic — exercised by callers that bail out before New succeeds.
func TestProviders_ShutdownNil(t *testing.T) {
	var p *Providers
	if err := p.Shutdown(t.Context()); err != nil {
		t.Errorf("nil Shutdown: %v", err)
	}
}

// TestProviders_NoopInterfaceSatisfied confirms the noop providers we
// hand back implement the OTel interfaces the rest of cerberus depends
// on (otel.SetTracerProvider / otel.SetMeterProvider expect those
// exact interface types).
func TestProviders_NoopInterfaceSatisfied(t *testing.T) {
	providers, err := New(t.Context(), Config{Endpoint: ""})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := providers.TracerProvider.(tracenoop.TracerProvider); !ok {
		t.Errorf("TracerProvider not noop type: %T", providers.TracerProvider)
	}
	if _, ok := providers.MeterProvider.(metricnoop.MeterProvider); !ok {
		t.Errorf("MeterProvider not noop type: %T", providers.MeterProvider)
	}
}
