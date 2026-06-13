// Package config loads cerberus runtime configuration from environment
// variables (the seed source — YAML loading lands when there's an actual
// need for nested config).
package config

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	otellog "go.opentelemetry.io/otel/log"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/telemetry"
)

// Config is the cerberus runtime configuration.
type Config struct {
	HTTPAddr   string
	ClickHouse chclient.Config
	Schema     schema.Metrics
	// Logs is the OTel logs schema (table + columns the Loki API reads).
	// Defaults to schema.DefaultOTelLogs() with any CERBERUS_SCHEMA_LOGS_*
	// env overrides applied.
	Logs schema.Logs
	// Traces is the OTel traces schema (table + columns the Tempo API
	// reads). Defaults to schema.DefaultOTelTraces() with any
	// CERBERUS_SCHEMA_TRACES_* env overrides applied.
	Traces schema.Traces

	// AutoCreateSchema, when true, instructs cerberus to run the OTel
	// ClickHouse Exporter DDL (via internal/schema/ddl) against the
	// configured ClickHouse connection at startup, before HTTP serving
	// begins. Default is false — production deploys stay explicit and
	// keep the operator-runs-DDL contract. The DDL itself is idempotent
	// (every statement carries CREATE TABLE IF NOT EXISTS) so enabling
	// the flag on an already-populated ClickHouse is a no-op.
	AutoCreateSchema bool

	// ExperimentalTSGridRange, when true, makes the PromQL lowering emit
	// ClickHouse-native `timeSeriesRateToGrid` for eligible
	// `rate(<counter>[<range>])` query_range expressions instead of the
	// default arrayJoin fan-out. The native operator is a compiled C++
	// aggregate that computes the per-grid-point rate directly, closing
	// the execution-layer gap the SQL array machinery leaves at high
	// cardinality.
	//
	// Default is false — and MUST stay false out of the box. The family
	// was introduced in ClickHouse v25.6.0; the compose / e2e /
	// compatibility lanes all run ClickHouse 24.8, which lacks the
	// function entirely (a native-path query 500s there with
	// UNKNOWN_FUNCTION). The experimental setting
	// `allow_experimental_time_series_aggregate_functions=1` is sent only
	// on the queries that actually use the native node (see
	// internal/engine), so unrelated queries on a 24.8 server are never
	// touched. First cut is rate-only; increase / delta stay on the
	// fan-out until a dedicated chDB differential sweep proves the
	// timeSeriesDeltaToGrid mapping.
	ExperimentalTSGridRange bool

	// Log configures cerberus's own structured logging (stdlib log/slog).
	// See LogConfig for the env-var contract.
	Log LogConfig

	// OTLP is the OpenTelemetry exporter configuration. When
	// OTLP.Endpoint is empty cerberus installs no-op trace and meter
	// providers (zero-collector-dependency binary). When set, cerberus
	// builds gRPC OTLP exporters that ship spans + self-metrics to that
	// endpoint. Standard OTEL_EXPORTER_OTLP_* env vars also work — the
	// OTel Go SDK reads them by default and they merge with whatever
	// these cerberus-specific values resolve to.
	OTLP OTLPConfig

	// Admit configures per-handler concurrency caps. When Admit.Disabled
	// is true cerberus skips the admission middleware entirely and every
	// request is admitted. When false, each API head (Prom / Loki /
	// Tempo) gets its own counted semaphore — requests above the cap
	// are rejected with HTTP 503 + Retry-After: 1 so well-behaved
	// clients back off and CH stays out of overload.
	Admit AdmitConfig
}

// AdmitConfig holds the per-handler concurrency cap knobs.
//
// Defaults are deliberately conservative — easy to lift via env, hard
// to accidentally deny-of-service legitimate traffic with the
// out-of-the-box values. Tempo's default is half of Prom/Loki because
// trace queries are typically the heaviest per-call (full trace span
// fetches + tag-value scans across wide column sets).
type AdmitConfig struct {
	// Disabled, when true, removes admission control entirely. Handy
	// for local development where artificial caps mask real
	// concurrency bugs. Default false (admission control enabled).
	Disabled bool

	// MaxInflightProm caps simultaneous in-flight Prom API requests.
	MaxInflightProm int

	// MaxInflightLoki caps simultaneous in-flight Loki API requests.
	MaxInflightLoki int

	// MaxInflightTempo caps simultaneous in-flight Tempo API requests.
	MaxInflightTempo int
}

// LogConfig controls the slog setup applied at startup.
//
//   - Format is the slog handler kind. "text" produces a human-readable
//     stream suited to local development; "json" produces newline-delimited
//     JSON suited to log aggregators (Loki / ECS / GCP).
//   - Level is the minimum level recorded; lower-severity records are
//     dropped at the handler. Supported: "debug", "info" (default),
//     "warn", "error".
type LogConfig struct {
	Format string
	Level  slog.Level
}

// OTLPConfig holds OTLP gRPC exporter settings shared by the trace and
// metric exporters. An empty Endpoint disables exporters entirely.
type OTLPConfig struct {
	// Endpoint is the gRPC target, e.g. "otel-collector.observability.svc:4317".
	// Empty disables the exporters (noop providers installed).
	Endpoint string

	// Insecure, when true, dials the endpoint without TLS (handy for
	// local dev / k3d where the collector listens on plain gRPC).
	Insecure bool

	// Headers are passed to every OTLP request as gRPC metadata
	// (typically used for auth bearer tokens).
	Headers map[string]string

	// Timeout caps a single OTLP request roundtrip. Applies to both
	// the trace and metric exporters.
	Timeout time.Duration

	// ExportInterval is how often the SDK PeriodicReader flushes
	// accumulated metric points to the OTLP endpoint. The OTel SDK
	// default is 60s, which is fine for steady-state production but
	// adds ~minute of latency before fresh data appears in dashboards
	// after a stack restart. Cerberus's default (10s) trades a small
	// amount of collector load for a noticeably tighter
	// time-to-visibility on the Docker Compose quickstart. Operators
	// running at scale should raise it via CERBERUS_OTLP_EXPORT_INTERVAL.
	ExportInterval time.Duration
}

// FromEnv reads configuration from environment variables, falling back to
// reasonable defaults for local development.
//
//	CERBERUS_HTTP_ADDR             default ":8080"
//	CERBERUS_CH_ADDR               default "localhost:9000"
//	CERBERUS_CH_DATABASE           default "otel"
//	CERBERUS_CH_USERNAME           default "default"
//	CERBERUS_CH_PASSWORD           default ""
//	CERBERUS_CH_DIAL_TIMEOUT       default "5s"
//	CERBERUS_CH_MAX_OPEN_CONNS     default 10 (total pooled conns, busy + idle)
//	CERBERUS_CH_MAX_IDLE_CONNS     default 5  (idle conns kept warm for reuse)
//	CERBERUS_CH_CONN_MAX_LIFETIME  default "1h" (max age before a conn is recycled)
//	CERBERUS_QUERY_MAX_SAMPLES     default 50000000 (0 disables the budget)
//	CERBERUS_CH_QUERY_MAX_MEMORY   default 1073741824 bytes = 1GiB (0 = don't set)
//	CERBERUS_CH_BREAKER_ENABLED       default "true"  (false → breaker never trips)
//	CERBERUS_CH_BREAKER_THRESHOLD     default 5   (consecutive failures to trip OPEN)
//	CERBERUS_CH_BREAKER_WINDOW        default "10s" (rolling failure window)
//	CERBERUS_CH_BREAKER_OPEN_INTERVAL default "5s"  (OPEN-state backoff before a probe)
//	CERBERUS_AUTO_CREATE_SCHEMA    default "false"
//	CERBERUS_EXPERIMENTAL_TS_GRID_RANGE default "false" — emit ClickHouse-native
//	    timeSeriesRateToGrid for eligible rate query_range; requires ClickHouse
//	    >= 25.6; on older servers the native query 500s with UNKNOWN_FUNCTION
//	CERBERUS_LOG_FORMAT            default "text"  ("text" | "json")
//	CERBERUS_LOG_LEVEL             default "info"  ("debug" | "info" | "warn" | "error")
//	CERBERUS_OTLP_ENDPOINT         default ""   (empty → exporters disabled)
//	CERBERUS_OTLP_INSECURE         default "false"
//	CERBERUS_OTLP_HEADERS          default ""   ("k=v,k2=v2" comma-separated)
//	CERBERUS_OTLP_TIMEOUT          default "10s"
//	CERBERUS_OTLP_EXPORT_INTERVAL  default "10s" (metric PeriodicReader flush interval)
//	CERBERUS_ADMIT_DISABLED        default "false"
//	CERBERUS_ADMIT_PROM            default 64
//	CERBERUS_ADMIT_LOKI            default 64
//	CERBERUS_ADMIT_TEMPO           default 32
//
// Standard OTEL_EXPORTER_OTLP_* env vars are also honored by the OTel
// Go SDK and complement these — see docs/observability.md.
//
// Schema-shape overrides (see internal/schema for the full env-var list):
//
//	CERBERUS_SCHEMA_METRICS_GAUGE_TABLE         default "otel_metrics_gauge"
//	CERBERUS_SCHEMA_METRICS_SUM_TABLE           default "otel_metrics_sum"
//	CERBERUS_SCHEMA_METRICS_HISTOGRAM_TABLE     default "otel_metrics_histogram"
//	CERBERUS_SCHEMA_METRICS_EXP_HISTOGRAM_TABLE default "otel_metrics_exp_histogram"
//	CERBERUS_SCHEMA_METRICS_SUMMARY_TABLE       default "otel_metrics_summary"
//	CERBERUS_SCHEMA_LOGS_TABLE                  default "otel_logs"
//	CERBERUS_SCHEMA_TRACES_TABLE                default "otel_traces"
func FromEnv() (Config, error) {
	dial, err := time.ParseDuration(envDefault("CERBERUS_CH_DIAL_TIMEOUT", "5s"))
	if err != nil {
		return Config{}, fmt.Errorf("CERBERUS_CH_DIAL_TIMEOUT: %w", err)
	}
	autoCreate, err := envBool("CERBERUS_AUTO_CREATE_SCHEMA", false)
	if err != nil {
		return Config{}, fmt.Errorf("CERBERUS_AUTO_CREATE_SCHEMA: %w", err)
	}
	tsGridRange, err := envBool("CERBERUS_EXPERIMENTAL_TS_GRID_RANGE", false)
	if err != nil {
		return Config{}, fmt.Errorf("CERBERUS_EXPERIMENTAL_TS_GRID_RANGE: %w", err)
	}
	maxOpenConns, err := envInt("CERBERUS_CH_MAX_OPEN_CONNS", defaultCHMaxOpenConns)
	if err != nil {
		return Config{}, fmt.Errorf("CERBERUS_CH_MAX_OPEN_CONNS: %w", err)
	}
	if maxOpenConns <= 0 {
		return Config{}, fmt.Errorf("CERBERUS_CH_MAX_OPEN_CONNS: must be > 0, got %d", maxOpenConns)
	}
	maxIdleConns, err := envInt("CERBERUS_CH_MAX_IDLE_CONNS", defaultCHMaxIdleConns)
	if err != nil {
		return Config{}, fmt.Errorf("CERBERUS_CH_MAX_IDLE_CONNS: %w", err)
	}
	if maxIdleConns <= 0 {
		return Config{}, fmt.Errorf("CERBERUS_CH_MAX_IDLE_CONNS: must be > 0, got %d", maxIdleConns)
	}
	connMaxLifetime, err := time.ParseDuration(envDefault("CERBERUS_CH_CONN_MAX_LIFETIME", defaultCHConnMaxLifetime.String()))
	if err != nil {
		return Config{}, fmt.Errorf("CERBERUS_CH_CONN_MAX_LIFETIME: %w", err)
	}
	if connMaxLifetime <= 0 {
		return Config{}, fmt.Errorf("CERBERUS_CH_CONN_MAX_LIFETIME: must be > 0, got %s", connMaxLifetime)
	}
	maxSamples, err := envInt64("CERBERUS_QUERY_MAX_SAMPLES", defaultQueryMaxSamples)
	if err != nil {
		return Config{}, fmt.Errorf("CERBERUS_QUERY_MAX_SAMPLES: %w", err)
	}
	if maxSamples < 0 {
		return Config{}, fmt.Errorf("CERBERUS_QUERY_MAX_SAMPLES: must be >= 0, got %d", maxSamples)
	}
	maxMemory, err := envInt64("CERBERUS_CH_QUERY_MAX_MEMORY", defaultCHQueryMaxMemory)
	if err != nil {
		return Config{}, fmt.Errorf("CERBERUS_CH_QUERY_MAX_MEMORY: %w", err)
	}
	if maxMemory < 0 {
		return Config{}, fmt.Errorf("CERBERUS_CH_QUERY_MAX_MEMORY: must be >= 0, got %d", maxMemory)
	}
	breaker, err := breakerFromEnv()
	if err != nil {
		return Config{}, err
	}
	logCfg, err := envLog()
	if err != nil {
		return Config{}, err
	}
	otlp, err := otlpFromEnv()
	if err != nil {
		return Config{}, err
	}
	admit, err := admitFromEnv()
	if err != nil {
		return Config{}, err
	}
	return Config{
		HTTPAddr: envDefault("CERBERUS_HTTP_ADDR", ":8080"),
		ClickHouse: chclient.Config{
			Addr:                envDefault("CERBERUS_CH_ADDR", "localhost:9000"),
			Database:            envDefault("CERBERUS_CH_DATABASE", "otel"),
			Username:            envDefault("CERBERUS_CH_USERNAME", "default"),
			Password:            envDefault("CERBERUS_CH_PASSWORD", ""),
			DialTimeout:         dial,
			MaxOpenConns:        maxOpenConns,
			MaxIdleConns:        maxIdleConns,
			ConnMaxLifetime:     connMaxLifetime,
			MaxQuerySamples:     maxSamples,
			MaxQueryMemoryBytes: maxMemory,
			BreakerThreshold:    breaker.Threshold,
			BreakerWindow:       breaker.Window,
			BreakerOpenInterval: breaker.OpenInterval,
			BreakerDisabled:     breaker.Disabled,
		},
		Schema:                  schema.DefaultOTelMetricsFromEnv(),
		Logs:                    schema.DefaultOTelLogsFromEnv(),
		Traces:                  schema.DefaultOTelTracesFromEnv(),
		AutoCreateSchema:        autoCreate,
		ExperimentalTSGridRange: tsGridRange,
		Log:                     logCfg,
		OTLP:                    otlp,
		Admit:                   admit,
	}, nil
}

// defaultQueryMaxSamples is the default per-query sample budget,
// mirroring upstream Prometheus's --query.max-samples default
// (50,000,000). A query whose result-set drain crosses the budget is
// aborted (Prom head: 422 errorType=execution with Prometheus's exact
// wire message) instead of materialising an unbounded matrix in
// process memory. Note Prometheus sizes its default around ~16-byte
// in-memory samples; cerberus rows carry label maps (interned
// per-series, but still heavier), so memory-constrained deploys
// should set CERBERUS_QUERY_MAX_SAMPLES well below the default — the
// k3d e2e stack runs at 5,000,000.
const defaultQueryMaxSamples int64 = 50_000_000

// defaultCHQueryMaxMemory is the default ClickHouse per-query memory
// cap (the `max_memory_usage` setting chclient stamps on every
// data-plane query): 1 GiB. Chosen so a single over-broad query (the
// 24h/15s matrix tuple from k3d run 27277793810 demanded 2.12 GiB)
// gets a deterministic resource-exhausted rejection instead of racing
// ClickHouse's server-total cap mid-stream and 502-ing. 0 disables the
// setting entirely (ClickHouse server defaults apply).
const defaultCHQueryMaxMemory int64 = 1 << 30 // 1073741824 bytes

// ClickHouse connection-pool defaults (#81). These reproduce
// clickhouse-go/v2's previously-implicit defaults verbatim so the
// non-sharded path stays behaviour-compatible: the driver defaulted
// MaxIdleConns to 5, MaxOpenConns to MaxIdleConns+5 (= 10), and
// ConnMaxLifetime to 1h. Cerberus now sets them explicitly here — the
// ONE place pool sizing is derived — so the sharded-pushdown solver can
// raise the ceiling for fan-out by bumping these (or the matching
// CERBERUS_CH_MAX_OPEN_CONNS / CERBERUS_CH_MAX_IDLE_CONNS /
// CERBERUS_CH_CONN_MAX_LIFETIME env vars) rather than inheriting an
// implicit driver default. When the pool is exhausted an acquire blocks
// up to DialTimeout and then fails with clickhouse.ErrAcquireConnTimeout,
// which the circuit breaker treats neutrally (local pool-sizing signal,
// not CH-health failure).
const (
	defaultCHMaxOpenConns                  = 10
	defaultCHMaxIdleConns                  = 5
	defaultCHConnMaxLifetime time.Duration = time.Hour
)

// Circuit-breaker defaults (#95). These reproduce the previously-
// hardcoded constants in internal/chclient/breaker.go verbatim
// (threshold 5, window 10s, open-interval 5s, enabled) so out-of-the-box
// breaker behaviour is byte-unchanged when none of the CERBERUS_CH_BREAKER_*
// env vars are set. cmd/cerberus threads these through chclient.Config
// into the per-Client breaker; a zero field there resolves back to the
// matching constant inside the breaker, so the two default sources can
// never drift apart silently.
const (
	defaultCHBreakerThreshold                  = 5
	defaultCHBreakerWindow       time.Duration = 10 * time.Second
	defaultCHBreakerOpenInterval time.Duration = 5 * time.Second
)

// breakerConfig is the parsed CERBERUS_CH_BREAKER_* knob set. It is an
// internal carrier between breakerFromEnv and FromEnv — the fields land
// flat on chclient.Config (the breaker lives in chclient, so there is no
// separate public breaker struct to expose).
type breakerConfig struct {
	Disabled     bool
	Threshold    int
	Window       time.Duration
	OpenInterval time.Duration
}

// breakerFromEnv reads the CERBERUS_CH_BREAKER_* env vars into a
// breakerConfig. Unset values use the defaults above, which reproduce the
// pre-#95 hardcoded breaker constants exactly (so defaults are
// byte-unchanged). CERBERUS_CH_BREAKER_ENABLED=false disables the breaker
// entirely (always-allow, never trips); when disabled the threshold /
// window / interval knobs are still validated so a typo doesn't pass
// silently, but they have no runtime effect.
//
// Fail-fast validation: threshold must be >= 1, window > 0, interval > 0.
func breakerFromEnv() (breakerConfig, error) {
	enabled, err := envBool("CERBERUS_CH_BREAKER_ENABLED", true)
	if err != nil {
		return breakerConfig{}, fmt.Errorf("CERBERUS_CH_BREAKER_ENABLED: %w", err)
	}
	threshold, err := envInt("CERBERUS_CH_BREAKER_THRESHOLD", defaultCHBreakerThreshold)
	if err != nil {
		return breakerConfig{}, fmt.Errorf("CERBERUS_CH_BREAKER_THRESHOLD: %w", err)
	}
	if threshold < 1 {
		return breakerConfig{}, fmt.Errorf("CERBERUS_CH_BREAKER_THRESHOLD: must be >= 1, got %d", threshold)
	}
	window, err := time.ParseDuration(envDefault("CERBERUS_CH_BREAKER_WINDOW", defaultCHBreakerWindow.String()))
	if err != nil {
		return breakerConfig{}, fmt.Errorf("CERBERUS_CH_BREAKER_WINDOW: %w", err)
	}
	if window <= 0 {
		return breakerConfig{}, fmt.Errorf("CERBERUS_CH_BREAKER_WINDOW: must be > 0, got %s", window)
	}
	openInterval, err := time.ParseDuration(envDefault("CERBERUS_CH_BREAKER_OPEN_INTERVAL", defaultCHBreakerOpenInterval.String()))
	if err != nil {
		return breakerConfig{}, fmt.Errorf("CERBERUS_CH_BREAKER_OPEN_INTERVAL: %w", err)
	}
	if openInterval <= 0 {
		return breakerConfig{}, fmt.Errorf("CERBERUS_CH_BREAKER_OPEN_INTERVAL: must be > 0, got %s", openInterval)
	}
	return breakerConfig{
		Disabled:     !enabled,
		Threshold:    threshold,
		Window:       window,
		OpenInterval: openInterval,
	}, nil
}

// Default per-handler concurrency caps. Tempo gets a smaller cap
// because trace queries (search + tag-value scans + per-trace span
// fetches) are heavier than Prom/Loki metric queries.
const (
	defaultAdmitProm  = 64
	defaultAdmitLoki  = 64
	defaultAdmitTempo = 32
)

// admitFromEnv reads CERBERUS_ADMIT_* env vars into an AdmitConfig.
// Unset values use the conservative defaults above. Setting any cap
// to 0 disables admission control for that head specifically (a
// finer-grained alternative to CERBERUS_ADMIT_DISABLED, which kills
// every head). Negative caps are rejected — they almost certainly
// mean a typo.
func admitFromEnv() (AdmitConfig, error) {
	disabled, err := envBool("CERBERUS_ADMIT_DISABLED", false)
	if err != nil {
		return AdmitConfig{}, fmt.Errorf("CERBERUS_ADMIT_DISABLED: %w", err)
	}
	prom, err := envInt("CERBERUS_ADMIT_PROM", defaultAdmitProm)
	if err != nil {
		return AdmitConfig{}, fmt.Errorf("CERBERUS_ADMIT_PROM: %w", err)
	}
	loki, err := envInt("CERBERUS_ADMIT_LOKI", defaultAdmitLoki)
	if err != nil {
		return AdmitConfig{}, fmt.Errorf("CERBERUS_ADMIT_LOKI: %w", err)
	}
	tempo, err := envInt("CERBERUS_ADMIT_TEMPO", defaultAdmitTempo)
	if err != nil {
		return AdmitConfig{}, fmt.Errorf("CERBERUS_ADMIT_TEMPO: %w", err)
	}
	for name, v := range map[string]int{
		"CERBERUS_ADMIT_PROM":  prom,
		"CERBERUS_ADMIT_LOKI":  loki,
		"CERBERUS_ADMIT_TEMPO": tempo,
	} {
		if v < 0 {
			return AdmitConfig{}, fmt.Errorf("%s: must be >= 0, got %d", name, v)
		}
	}
	return AdmitConfig{
		Disabled:         disabled,
		MaxInflightProm:  prom,
		MaxInflightLoki:  loki,
		MaxInflightTempo: tempo,
	}, nil
}

// NewLogger builds a *slog.Logger from a LogConfig writing to w. The
// caller is responsible for installing the result as the slog default
// (e.g. via slog.SetDefault) if global propagation is desired. Accepting
// io.Writer keeps the helper trivially testable (a *bytes.Buffer drops
// straight in).
//
// This builds the **stderr-only** logger used during startup, before
// telemetry providers exist. Once `telemetry.New` returns, the caller
// should replace the slog default with `NewTelemetryLogger`, which
// adds the OTLP-log bridge while preserving the same stderr stream
// shape.
func NewLogger(w io.Writer, cfg LogConfig) *slog.Logger {
	return slog.New(newLocalHandler(w, cfg))
}

// NewTelemetryLogger builds the post-startup logger that fans every
// record out to (a) the stderr handler this function would have
// returned via NewLogger (text or json per LogConfig), AND (b) an
// OTel slog bridge backed by `provider`. When `provider` is the no-op
// LoggerProvider (telemetry disabled), the result is functionally
// identical to NewLogger — every record still hits stderr, nothing
// is exported.
//
// The fan-out gives cerberus the third o11y pillar over OTLP: the
// same records that print to `kubectl logs` also land in the
// collector's `otel_logs` table next to its traces and metrics.
//
// The provider parameter takes `any` to avoid an import cycle with
// `internal/telemetry`; the actual value must satisfy
// `go.opentelemetry.io/otel/log.LoggerProvider`. A nil provider
// returns a stderr-only logger.
func NewTelemetryLogger(w io.Writer, cfg LogConfig, provider any) *slog.Logger {
	local := newLocalHandler(w, cfg)
	if provider == nil {
		return slog.New(local)
	}
	lp, ok := provider.(otellog.LoggerProvider)
	if !ok {
		// Defensive: a non-LoggerProvider argument means the
		// caller's import wiring is broken; fall back to stderr.
		return slog.New(local)
	}
	return slog.New(telemetry.NewSlogHandler(local, lp))
}

func newLocalHandler(w io.Writer, cfg LogConfig) slog.Handler {
	opts := &slog.HandlerOptions{Level: cfg.Level}
	switch cfg.Format {
	case "json":
		return slog.NewJSONHandler(w, opts)
	default:
		return slog.NewTextHandler(w, opts)
	}
}

// otlpFromEnv parses the CERBERUS_OTLP_* env vars into an OTLPConfig.
// Empty endpoint is the documented "disabled" state and not an error —
// the caller installs noop providers in that case.
func otlpFromEnv() (OTLPConfig, error) {
	timeout, err := time.ParseDuration(envDefault("CERBERUS_OTLP_TIMEOUT", "10s"))
	if err != nil {
		return OTLPConfig{}, fmt.Errorf("CERBERUS_OTLP_TIMEOUT: %w", err)
	}
	insecure, err := envBool("CERBERUS_OTLP_INSECURE", false)
	if err != nil {
		return OTLPConfig{}, fmt.Errorf("CERBERUS_OTLP_INSECURE: %w", err)
	}
	headers, err := parseHeaders(os.Getenv("CERBERUS_OTLP_HEADERS"))
	if err != nil {
		return OTLPConfig{}, fmt.Errorf("CERBERUS_OTLP_HEADERS: %w", err)
	}
	exportInterval, err := time.ParseDuration(envDefault("CERBERUS_OTLP_EXPORT_INTERVAL", "10s"))
	if err != nil {
		return OTLPConfig{}, fmt.Errorf("CERBERUS_OTLP_EXPORT_INTERVAL: %w", err)
	}
	return OTLPConfig{
		Endpoint:       strings.TrimSpace(os.Getenv("CERBERUS_OTLP_ENDPOINT")),
		Insecure:       insecure,
		Headers:        headers,
		Timeout:        timeout,
		ExportInterval: exportInterval,
	}, nil
}

// parseHeaders splits a "k=v,k2=v2" string into a map. Empty input
// returns nil, mirroring the noop default. Whitespace around keys and
// values is trimmed. Entries without "=" are rejected so a typo doesn't
// silently drop an auth header.
func parseHeaders(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	out := map[string]string{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		eq := strings.IndexByte(part, '=')
		if eq < 0 {
			return nil, fmt.Errorf("entry %q: missing '='", part)
		}
		k := strings.TrimSpace(part[:eq])
		v := strings.TrimSpace(part[eq+1:])
		if k == "" {
			return nil, fmt.Errorf("entry %q: empty key", part)
		}
		out[k] = v
	}
	return out, nil
}

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envInt parses an integer env var. An unset or empty value returns
// def. Anything that fails strconv.Atoi is rejected with an error so
// misconfiguration fails fast at startup.
func envInt(key string, def int) (int, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid integer %q: %w", v, err)
	}
	return n, nil
}

// envInt64 parses a 64-bit integer env var. An unset or empty value
// returns def. Anything that fails strconv.ParseInt is rejected with
// an error so misconfiguration fails fast at startup.
func envInt64(key string, def int64) (int64, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid integer %q: %w", v, err)
	}
	return n, nil
}

// envBool parses a boolean env var. Accepts the standard strconv.ParseBool
// vocabulary: "1"/"0", "t"/"f", "true"/"false", "TRUE"/"FALSE",
// case-insensitive. An unset or empty value returns def. Anything else is
// rejected with an error so misconfiguration fails fast at startup.
func envBool(key string, def bool) (bool, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("invalid boolean %q: %w", v, err)
	}
	return b, nil
}

// envLog parses CERBERUS_LOG_FORMAT + CERBERUS_LOG_LEVEL into a LogConfig.
// Unset values default to "text" / "info"; invalid values fail fast at
// startup so a typo never silently downgrades observability.
func envLog() (LogConfig, error) {
	format := strings.ToLower(strings.TrimSpace(envDefault("CERBERUS_LOG_FORMAT", "text")))
	switch format {
	case "text", "json":
	default:
		return LogConfig{}, fmt.Errorf("CERBERUS_LOG_FORMAT: invalid value %q (want \"text\" or \"json\")", format)
	}
	levelStr := strings.ToLower(strings.TrimSpace(envDefault("CERBERUS_LOG_LEVEL", "info")))
	var level slog.Level
	switch levelStr {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return LogConfig{}, fmt.Errorf("CERBERUS_LOG_LEVEL: invalid value %q (want \"debug\", \"info\", \"warn\", or \"error\")", levelStr)
	}
	return LogConfig{Format: format, Level: level}, nil
}
