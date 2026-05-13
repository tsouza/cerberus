// Package telemetry builds the OpenTelemetry tracer- and meter-provider
// pair cerberus installs as the OTel process globals.
//
// When the supplied endpoint is empty, telemetry returns noop providers
// — the zero-collector-dependency default that keeps cerberus runnable
// without any OTel infrastructure. When the endpoint is set, telemetry
// builds gRPC OTLP exporters (one for traces, one for metrics), wraps
// them in the SDK trace/metric providers, and tags every export with a
// resource carrying `service.name`, `service.version`, and
// `service.instance.id`.
//
// The OTel Go SDK also reads standard `OTEL_EXPORTER_OTLP_*` env vars on
// its own; cerberus's CERBERUS_OTLP_* knobs apply on top of those.
package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// Config holds the runtime OTLP settings cerberus passes into the
// provider builders. Mirrors internal/config.OTLPConfig but stays
// dependency-free so this package can be reused by tests.
type Config struct {
	Endpoint string
	Insecure bool
	Headers  map[string]string
	Timeout  time.Duration

	ServiceName    string
	ServiceVersion string
}

// Providers bundles the trace + meter providers cerberus installed as
// globals plus a Shutdown closure that flushes both. Shutdown is safe
// to call multiple times; the first call wins.
type Providers struct {
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider

	shutdown func(context.Context) error
}

// Shutdown flushes any pending spans / metric batches and tears down
// the providers. Always returns nil for the noop case.
func (p *Providers) Shutdown(ctx context.Context) error {
	if p == nil || p.shutdown == nil {
		return nil
	}
	return p.shutdown(ctx)
}

// New builds the OTel providers cerberus uses at runtime. An empty
// cfg.Endpoint returns noop providers and a no-op Shutdown — the safe
// "OTel disabled" default. A non-empty endpoint builds real gRPC OTLP
// exporters; resource attributes are filled from cfg.ServiceName,
// cfg.ServiceVersion, and the local hostname (with a random fallback).
func New(ctx context.Context, cfg Config) (*Providers, error) {
	if cfg.Endpoint == "" {
		return &Providers{
			TracerProvider: tracenoop.NewTracerProvider(),
			MeterProvider:  metricnoop.NewMeterProvider(),
		}, nil
	}

	res, err := buildResource(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("build resource: %w", err)
	}

	tp, traceShutdown, err := newTracerProvider(ctx, cfg, res)
	if err != nil {
		return nil, fmt.Errorf("trace exporter: %w", err)
	}

	mp, metricShutdown, err := newMeterProvider(ctx, cfg, res)
	if err != nil {
		// Roll back the trace provider so we don't leak a goroutine
		// when only one of the two could be built.
		_ = traceShutdown(ctx)
		return nil, fmt.Errorf("metric exporter: %w", err)
	}

	return &Providers{
		TracerProvider: tp,
		MeterProvider:  mp,
		shutdown: func(ctx context.Context) error {
			// Best-effort: try both, return the first error so the
			// operator still sees a signal but neither half blocks
			// the other.
			tErr := traceShutdown(ctx)
			mErr := metricShutdown(ctx)
			if tErr != nil {
				return tErr
			}
			return mErr
		},
	}, nil
}

func newTracerProvider(ctx context.Context, cfg Config, res *resource.Resource) (
	*sdktrace.TracerProvider, func(context.Context) error, error,
) {
	opts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
	}
	if cfg.Insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlptracegrpc.WithHeaders(cfg.Headers))
	}
	if cfg.Timeout > 0 {
		opts = append(opts, otlptracegrpc.WithTimeout(cfg.Timeout))
	}
	client := otlptracegrpc.NewClient(opts...)
	exp, err := otlptrace.New(ctx, client)
	if err != nil {
		return nil, nil, err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	return tp, tp.Shutdown, nil
}

func newMeterProvider(ctx context.Context, cfg Config, res *resource.Resource) (
	*sdkmetric.MeterProvider, func(context.Context) error, error,
) {
	opts := []otlpmetricgrpc.Option{
		otlpmetricgrpc.WithEndpoint(cfg.Endpoint),
	}
	if cfg.Insecure {
		opts = append(opts, otlpmetricgrpc.WithInsecure())
	}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlpmetricgrpc.WithHeaders(cfg.Headers))
	}
	if cfg.Timeout > 0 {
		opts = append(opts, otlpmetricgrpc.WithTimeout(cfg.Timeout))
	}
	exp, err := otlpmetricgrpc.New(ctx, opts...)
	if err != nil {
		return nil, nil, err
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)),
		sdkmetric.WithResource(res),
	)
	return mp, mp.Shutdown, nil
}

// buildResource composes the resource attached to every exported span
// and metric. Attribute fallbacks:
//
//   - service.name        ← cfg.ServiceName  (defaults to "cerberus" if blank)
//   - service.version     ← cfg.ServiceVersion (defaults to "dev" if blank)
//   - service.instance.id ← os.Hostname()    (falls back to a random 16-byte
//     hex string when hostname lookup errors out — common in scratch
//     containers)
func buildResource(ctx context.Context, cfg Config) (*resource.Resource, error) {
	name := cfg.ServiceName
	if name == "" {
		name = "cerberus"
	}
	version := cfg.ServiceVersion
	if version == "" {
		version = "dev"
	}
	instance, err := os.Hostname()
	if err != nil || instance == "" {
		instance = randomInstanceID()
	}
	return resource.New(
		ctx,
		resource.WithAttributes(
			semconv.ServiceName(name),
			semconv.ServiceVersion(version),
			semconv.ServiceInstanceID(instance),
		),
	)
}

func randomInstanceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}
