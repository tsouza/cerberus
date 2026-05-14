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

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
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
//	CERBERUS_AUTO_CREATE_SCHEMA    default "false"
//	CERBERUS_LOG_FORMAT            default "text"  ("text" | "json")
//	CERBERUS_LOG_LEVEL             default "info"  ("debug" | "info" | "warn" | "error")
//	CERBERUS_OTLP_ENDPOINT         default ""   (empty → exporters disabled)
//	CERBERUS_OTLP_INSECURE         default "false"
//	CERBERUS_OTLP_HEADERS          default ""   ("k=v,k2=v2" comma-separated)
//	CERBERUS_OTLP_TIMEOUT          default "10s"
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
			Addr:        envDefault("CERBERUS_CH_ADDR", "localhost:9000"),
			Database:    envDefault("CERBERUS_CH_DATABASE", "otel"),
			Username:    envDefault("CERBERUS_CH_USERNAME", "default"),
			Password:    envDefault("CERBERUS_CH_PASSWORD", ""),
			DialTimeout: dial,
		},
		Schema:           schema.DefaultOTelMetricsFromEnv(),
		Logs:             schema.DefaultOTelLogsFromEnv(),
		Traces:           schema.DefaultOTelTracesFromEnv(),
		AutoCreateSchema: autoCreate,
		Log:              logCfg,
		OTLP:             otlp,
		Admit:            admit,
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
func NewLogger(w io.Writer, cfg LogConfig) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.Level}
	var h slog.Handler
	switch cfg.Format {
	case "json":
		h = slog.NewJSONHandler(w, opts)
	default:
		h = slog.NewTextHandler(w, opts)
	}
	return slog.New(h)
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
	return OTLPConfig{
		Endpoint: strings.TrimSpace(os.Getenv("CERBERUS_OTLP_ENDPOINT")),
		Insecure: insecure,
		Headers:  headers,
		Timeout:  timeout,
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
