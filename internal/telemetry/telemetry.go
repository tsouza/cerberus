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
// The OTel Go SDK also reads the standard `OTEL_EXPORTER_OTLP_*` env
// vars on its own, and buildResource wires in the standard resource
// detector so `OTEL_SERVICE_NAME` / `OTEL_RESOURCE_ATTRIBUTES` are
// honoured too; cerberus's CERBERUS_OTLP_* knobs apply on top of those.
package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"

	"go.opentelemetry.io/otel/attribute"
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
// cfg.ServiceVersion, the standard OTel resource env vars, and the local
// hostname (with a random fallback) — see buildResource for precedence.
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

	return newProviders(ctx, cfg, res, defaultProviderBuilders())
}

// shutdownFunc tears one provider down, flushing whatever it still holds.
type shutdownFunc func(context.Context) error

// providerBuilders names the three SDK provider constructors newProviders
// drives, in the order it drives them. New always supplies the real ones;
// the indirection exists because the OTLP gRPC exporters do not dial
// eagerly, so a real constructor practically never returns an error and
// the rollback paths below would otherwise be unreachable from a test.
type providerBuilders struct {
	tracer func(context.Context, Config, *resource.Resource) (trace.TracerProvider, shutdownFunc, error)
	meter  func(context.Context, Config, *resource.Resource) (metric.MeterProvider, shutdownFunc, error)
	logger func(context.Context, Config, *resource.Resource) (otellog.LoggerProvider, shutdownFunc, error)
}

// defaultProviderBuilders is the production wiring: real gRPC OTLP
// exporters behind the SDK providers.
func defaultProviderBuilders() providerBuilders {
	return providerBuilders{
		tracer: newTracerProvider,
		meter:  newMeterProvider,
		logger: newLoggerProvider,
	}
}

// rollbackProviders tears down the providers built before a later stage
// failed and folds every teardown failure into cause. A rollback that
// itself fails leaves running the very goroutine the rollback exists to
// stop, so that failure has to reach the caller rather than vanish
// beside the error that triggered it.
func rollbackProviders(ctx context.Context, cause error, built ...shutdownFunc) error {
	errs := make([]error, 1, len(built)+1)
	errs[0] = cause
	for _, shutdown := range built {
		if err := shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("rollback: %w", err))
		}
	}
	return errors.Join(errs...)
}

// newProviders builds the three providers in order, rolling back the ones
// already built as soon as a later one fails so a half-built set never
// leaks its exporter goroutines.
func newProviders(
	ctx context.Context, cfg Config, res *resource.Resource, build providerBuilders,
) (*Providers, error) {
	tp, traceShutdown, err := build.tracer(ctx, cfg, res)
	if err != nil {
		return nil, fmt.Errorf("trace exporter: %w", err)
	}

	mp, metricShutdown, err := build.meter(ctx, cfg, res)
	if err != nil {
		return nil, rollbackProviders(ctx, fmt.Errorf("metric exporter: %w", err), traceShutdown)
	}

	lp, logShutdown, err := build.logger(ctx, cfg, res)
	if err != nil {
		return nil, rollbackProviders(
			ctx, fmt.Errorf("log exporter: %w", err), traceShutdown, metricShutdown,
		)
	}

	return &Providers{
		TracerProvider: tp,
		MeterProvider:  mp,
		LoggerProvider: lp,
		shutdown: func(ctx context.Context) error {
			// Best-effort: run all three so one failure never blocks
			// the others, and join every error so a second or third
			// failure isn't masked by the first.
			return errors.Join(
				traceShutdown(ctx),
				metricShutdown(ctx),
				logShutdown(ctx),
			)
		},
	}, nil
}

func newTracerProvider(ctx context.Context, cfg Config, res *resource.Resource) (
	trace.TracerProvider, shutdownFunc, error,
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
	metric.MeterProvider, shutdownFunc, error,
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
		sdkmetric.WithView(queryDurationNativeHistogramView),
	)
	return mp, mp.Shutdown, nil
}

// queryDurationExpoHistogramMaxSize and …MaxScale are the OTel exponential
// histogram spec's own documented defaults (the same values every major
// OTel SDK ships when a caller doesn't override them): up to 160 buckets,
// starting at the maximum resolution scale (20) and auto-narrowing as
// measurements arrive. sdkmetric.AggregationBase2ExponentialHistogram's own
// zero value is NOT a usable "use the SDK default" sentinel — its err()
// rejects MaxSize <= 0, and sdkmetric.NewView silently drops (logs via
// go.opentelemetry.io/otel's global error handler, otherwise unnoticed) an
// Aggregation that fails validation, falling back to the classic default
// this View exists to override. An unvalidated zero-value struct here would
// make queryDurationNativeHistogramView a silent no-op.
const (
	queryDurationExpoHistogramMaxSize  = 160
	queryDurationExpoHistogramMaxScale = 20
)

// queryDurationNativeHistogramView collects cerberus_queries_duration_exp_hist
// (internal/telemetry/metrics.go's QueryDuration) as a native/exponential
// histogram instead of the classic explicit-bucket-boundary aggregation its
// own WithExplicitBucketBoundaries construction would otherwise get by
// default (cerberus issue #3170). A classic histogram stores one physical
// row per (series, `le` rung) — that storage shape, not this metric's own
// cardinality, is what let the nightly e2e stack's own self-monitoring
// dashboard trip the unrelated RangeBucketGridNative density guard once
// before (range_bucket_grid_native_bound.go's "Issue #2522 recalibration"
// section, querying this exact metric at a 24h/15s window). Native
// histograms store the whole distribution as one compact row per (series,
// timestamp), so that class of risk does not apply to them at all — see
// cerberus issue #3165's investigation for the full comparison.
//
// The instrument's own registered name carries the `_exp_hist` suffix —
// not just its aggregation — because cerberus's own PromQL read path has
// no wire-format way to tell a native histogram from a classic one by
// type; it routes purely on that suffix
// (schema.Metrics.ExpHistogramSuffix, internal/schema/otel.go). Overriding
// only the Aggregation here while leaving the instrument named
// `cerberus_queries_duration_seconds` would collect the data correctly
// but leave every PromQL query against it silently resolving to nothing,
// since the read path would still look for a `_bucket` series that no
// longer exists.
//
// StageDuration and the other histograms in metrics.go stay classic for now
// — this metric is the one with a real prior incident and the one queried
// in test/e2e/grafana/dashboards/cerberus.json, so it is the one worth the
// dashboard-query-shape migration (native histogram PromQL has no `_bucket`
// series or `le` label) that came with this change.
var queryDurationNativeHistogramView = sdkmetric.NewView(
	sdkmetric.Instrument{Name: "cerberus_queries_duration_exp_hist"},
	sdkmetric.Stream{
		Aggregation: sdkmetric.AggregationBase2ExponentialHistogram{
			MaxSize:  queryDurationExpoHistogramMaxSize,
			MaxScale: queryDurationExpoHistogramMaxScale,
		},
	},
)

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
	otellog.LoggerProvider, shutdownFunc, error,
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

// Fallback resource values used when neither the cerberus config nor the
// standard OTel environment supplies one.
const (
	defaultServiceName    = "cerberus"
	defaultServiceVersion = "dev"
)

// buildResource composes the resource attached to every exported span,
// metric, and log record. Three precedence tiers are merged in order,
// each one overriding the tier before it (resource.New merges detectors
// left-to-right, and Merge lets the right-hand resource win):
//
//  1. Derived defaults — service.name "cerberus", service.version "dev",
//     and service.instance.id from hostname() (a random 16-byte hex
//     string when hostname lookup errors out or returns empty, common in
//     scratch containers). These are guesses, so anything explicit beats
//     them.
//  2. The standard OTel environment — OTEL_SERVICE_NAME and
//     OTEL_RESOURCE_ATTRIBUTES, via resource.WithFromEnv. This is the
//     only channel through which a deployment can attach resource
//     attributes cerberus has no config knob for (service.namespace,
//     deployment.environment, k8s.*), which is what every other OTLP
//     producer in a stack does and what docs/observability.md documents.
//  3. Explicit cerberus config — CERBERUS_OTLP_SERVICE_NAME /
//     _SERVICE_VERSION. Applied last so the documented "when both are
//     set, the CERBERUS_* value wins for that field" contract holds.
//     Blank config fields contribute nothing, leaving tiers 1-2 intact.
//
// hostname is parameterised for testability — production callers pass
// os.Hostname; tests supply a stub. Nil is treated as os.Hostname.
func buildResource(ctx context.Context, cfg Config, hostname hostnameFunc) (*resource.Resource, error) {
	if hostname == nil {
		hostname = os.Hostname
	}
	instance, err := hostname()
	if err != nil || instance == "" {
		instance = randomInstanceID()
	}

	explicit := make([]attribute.KeyValue, 0, 2)
	if cfg.ServiceName != "" {
		explicit = append(explicit, semconv.ServiceName(cfg.ServiceName))
	}
	if cfg.ServiceVersion != "" {
		explicit = append(explicit, semconv.ServiceVersion(cfg.ServiceVersion))
	}

	return resource.New(
		ctx,
		resource.WithAttributes(
			semconv.ServiceName(defaultServiceName),
			semconv.ServiceVersion(defaultServiceVersion),
			semconv.ServiceInstanceID(instance),
		),
		resource.WithFromEnv(),
		resource.WithAttributes(explicit...),
	)
}

func randomInstanceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}
