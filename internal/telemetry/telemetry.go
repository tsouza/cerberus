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

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otellog "go.opentelemetry.io/otel/log"
	lognoop "go.opentelemetry.io/otel/log/noop"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdklog "go.opentelemetry.io/otel/sdk/log"
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

	// ExportInterval controls how often the metric PeriodicReader
	// flushes accumulated points to the OTLP endpoint. Zero falls back
	// to the OTel SDK default (60s); cerberus's runtime config picks a
	// shorter quickstart-friendly default (10s) via
	// CERBERUS_OTLP_EXPORT_INTERVAL so panels populate within ~30s of
	// stack startup. Production deployments can dial it back up to
	// reduce collector load.
	ExportInterval time.Duration

	ServiceName    string
	ServiceVersion string
}

// Providers bundles the trace, meter, and logger providers cerberus
// installed as globals plus a Shutdown closure that flushes all three.
// Shutdown is safe to call multiple times; the first call wins.
type Providers struct {
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider
	LoggerProvider otellog.LoggerProvider

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
			LoggerProvider: lognoop.NewLoggerProvider(),
		}, nil
	}

	res, err := buildResource(ctx, cfg, os.Hostname)
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

	lp, logShutdown, err := newLoggerProvider(ctx, cfg, res)
	if err != nil {
		_ = traceShutdown(ctx)
		_ = metricShutdown(ctx)
		return nil, fmt.Errorf("log exporter: %w", err)
	}

	return &Providers{
		TracerProvider: tp,
		MeterProvider:  mp,
		LoggerProvider: lp,
		shutdown: func(ctx context.Context) error {
			// Best-effort: try all three, return the first error so
			// the operator still sees a signal but none blocks the
			// others.
			tErr := traceShutdown(ctx)
			mErr := metricShutdown(ctx)
			lErr := logShutdown(ctx)
			if tErr != nil {
				return tErr
			}
			if mErr != nil {
				return mErr
			}
			return lErr
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
	readerOpts := []sdkmetric.PeriodicReaderOption{}
	if cfg.ExportInterval > 0 {
		readerOpts = append(readerOpts, sdkmetric.WithInterval(cfg.ExportInterval))
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp, readerOpts...)),
		sdkmetric.WithResource(res),
	)
	return mp, mp.Shutdown, nil
}

// newLoggerProvider builds the OTLP gRPC logger provider that gives
// the third o11y pillar (logs) the same wire-level treatment as traces
// and metrics. The slog handler bridge (see `bridges/otelslog`) wraps
// this provider in `cmd/cerberus/main.go` so every record emitted via
// `slog.Default()` lands in the collector's otel_logs pipeline
// alongside the trace and metric streams. Without this, cerberus would
// rely on the k8s container-log → filelog-receiver path — which (a)
// requires a sidecar/DaemonSet, (b) round-trips slog records through
// text format losing structured attributes, and (c) wouldn't work in
// non-k8s deployments.
func newLoggerProvider(ctx context.Context, cfg Config, res *resource.Resource) (
	*sdklog.LoggerProvider, func(context.Context) error, error,
) {
	opts := []otlploggrpc.Option{
		otlploggrpc.WithEndpoint(cfg.Endpoint),
	}
	if cfg.Insecure {
		opts = append(opts, otlploggrpc.WithInsecure())
	}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlploggrpc.WithHeaders(cfg.Headers))
	}
	if cfg.Timeout > 0 {
		opts = append(opts, otlploggrpc.WithTimeout(cfg.Timeout))
	}
	exp, err := otlploggrpc.New(ctx, opts...)
	if err != nil {
		return nil, nil, err
	}
	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)),
		sdklog.WithResource(res),
	)
	return lp, lp.Shutdown, nil
}

// hostnameFunc resolves the hostname used for service.instance.id. The
// production path is os.Hostname; tests inject a deterministic stub to
// avoid host-environment dependence (scratch containers, sandboxed CI
// runners and Linux netns-isolated test pods can all return empty or
// errored hostnames).
type hostnameFunc func() (string, error)

// buildResource composes the resource attached to every exported span
// and metric. Attribute fallbacks:
//
//   - service.name        ← cfg.ServiceName  (defaults to "cerberus" if blank)
//   - service.version     ← cfg.ServiceVersion (defaults to "dev" if blank)
//   - service.instance.id ← hostname()        (falls back to a random 16-byte
//     hex string when hostname lookup errors out — common in scratch
//     containers)
//
// hostname is parameterised for testability — production callers pass
// os.Hostname; tests supply a stub. Nil is treated as os.Hostname.
func buildResource(ctx context.Context, cfg Config, hostname hostnameFunc) (*resource.Resource, error) {
	name := cfg.ServiceName
	if name == "" {
		name = "cerberus"
	}
	version := cfg.ServiceVersion
	if version == "" {
		version = "dev"
	}
	if hostname == nil {
		hostname = os.Hostname
	}
	instance, err := hostname()
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
