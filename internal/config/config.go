// Package config loads cerberus runtime configuration. The value source
// is a github.com/spf13/viper instance (per-loader, not the global
// singleton) wired with the CERBERUS_ env prefix; an optional
// `cerberus.yaml` config file may supply values, but environment
// variables always win over the file and explicit defaults sit beneath
// both. The CERBERUS_* environment-variable contract — names, Go types,
// defaults, and fail-fast validation — is unchanged from the prior
// hand-rolled env parser; viper is a mechanism swap, not a redesign.
package config

import (
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"
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

	// RequirementsCheck, when true (the default), runs the boot-time
	// requirements check after the schema-create step: it inspects the
	// connected ClickHouse server version against the config-derived
	// minimum (CH 25.8 base, raised to max(base, native-rate floor) when
	// CERBERUS_EXPERIMENTAL_TS_GRID_RANGE is enabled) AND validates the
	// deployed schema shape (the configured tables' essential columns and
	// the attribute-map column types) via system.columns. A FATAL finding —
	// a too-old/unreadable server or a table that EXISTS but is wrong-shape —
	// exits the process non-zero with an aggregated message, instead of
	// letting it surface as an opaque query-time error later. A schema that is
	// ENTIRELY ABSENT (not yet provisioned — the cerberus + collector startup
	// race) is NOT fatal: cerberus boots, reports NOT READY on /readyz, and
	// re-probes until an external writer creates the schema (no restart).
	// Setting CERBERUS_REQUIREMENTS_CHECK=false skips both gates.
	RequirementsCheck bool

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
	// compatibility lanes now all run ClickHouse 25.8 (matching the chDB
	// test substrate, chdb-go v1.11.0 = 25.8.2.1-lts), so the function
	// exists everywhere — but the path stays experimental because it
	// depends on the experimental setting
	// `allow_experimental_time_series_aggregate_functions=1`, sent only
	// on the queries that actually use the native node (see
	// internal/engine), so unrelated queries are never touched. First cut
	// is rate-only; increase / delta stay on the fan-out until a dedicated
	// chDB differential sweep proves the timeSeriesDeltaToGrid mapping.
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

// Environment-variable keys. Centralised so the viper SetDefault /
// BindEnv wiring and the per-field reads reference the exact same
// strings — the CERBERUS_* contract is load-bearing (docs + surface
// tests pin these names).
const (
	envHTTPAddr            = "CERBERUS_HTTP_ADDR"
	envCHAddr              = "CERBERUS_CH_ADDR"
	envCHDatabase          = "CERBERUS_CH_DATABASE"
	envCHUsername          = "CERBERUS_CH_USERNAME"
	envCHPassword          = "CERBERUS_CH_PASSWORD"
	envCHDialTimeout       = "CERBERUS_CH_DIAL_TIMEOUT"
	envCHMaxOpenConns      = "CERBERUS_CH_MAX_OPEN_CONNS"
	envCHMaxIdleConns      = "CERBERUS_CH_MAX_IDLE_CONNS"
	envCHConnMaxLifetime   = "CERBERUS_CH_CONN_MAX_LIFETIME"
	envQueryMaxSamples     = "CERBERUS_QUERY_MAX_SAMPLES"
	envQueryTimeout        = "CERBERUS_QUERY_TIMEOUT"
	envCHQueryMaxMemory    = "CERBERUS_CH_QUERY_MAX_MEMORY"
	envCHBreakerEnabled    = "CERBERUS_CH_BREAKER_ENABLED"
	envCHBreakerThreshold  = "CERBERUS_CH_BREAKER_THRESHOLD"
	envCHBreakerWindow     = "CERBERUS_CH_BREAKER_WINDOW"
	envCHBreakerOpenIntrvl = "CERBERUS_CH_BREAKER_OPEN_INTERVAL"
	envAutoCreateSchema    = "CERBERUS_AUTO_CREATE_SCHEMA"
	envRequirementsCheck   = "CERBERUS_REQUIREMENTS_CHECK"
	envExperimentalTSGrid  = "CERBERUS_EXPERIMENTAL_TS_GRID_RANGE"
	envLogFormat           = "CERBERUS_LOG_FORMAT"
	envLogLevel            = "CERBERUS_LOG_LEVEL"
	envOTLPEndpoint        = "CERBERUS_OTLP_ENDPOINT"
	envOTLPInsecure        = "CERBERUS_OTLP_INSECURE"
	envOTLPHeaders         = "CERBERUS_OTLP_HEADERS"
	envOTLPTimeout         = "CERBERUS_OTLP_TIMEOUT"
	envOTLPExportInterval  = "CERBERUS_OTLP_EXPORT_INTERVAL"
	envAdmitDisabled       = "CERBERUS_ADMIT_DISABLED"
	envAdmitProm           = "CERBERUS_ADMIT_PROM"
	envAdmitLoki           = "CERBERUS_ADMIT_LOKI"
	envAdmitTempo          = "CERBERUS_ADMIT_TEMPO"
)

// configFileBaseName is the base name (without extension) viper looks
// for when probing for an optional config file: cerberus.yaml.
const configFileBaseName = "cerberus"

// FromEnv reads configuration via a per-call viper loader. Values are
// resolved with viper's standard precedence — environment variable >
// config file > built-in default — so the CERBERUS_* environment
// contract always wins. An optional `cerberus.yaml` in the working
// directory (or /etc/cerberus) supplies file-level defaults; its
// absence is not an error.
//
//	CERBERUS_HTTP_ADDR             default ":8080"
//	CERBERUS_CH_ADDR               default "localhost:9000"
//	CERBERUS_CH_DATABASE           default "otel"
//	CERBERUS_CH_USERNAME           default "default"
//	CERBERUS_CH_PASSWORD           default ""
//	CERBERUS_CH_DIAL_TIMEOUT       default "5s"
//	CERBERUS_CH_MAX_OPEN_CONNS     default 10 (total pooled conns, busy + idle)
//	CERBERUS_CH_MAX_IDLE_CONNS     default 5  (idle conns kept warm for reuse)
//	CERBERUS_CH_CONN_MAX_LIFETIME  default "30s" (max age before a conn is recycled; short so a stale conn to a restarted CH pod ages out in seconds — the effective restart heal bound)
//	CERBERUS_QUERY_MAX_SAMPLES     default 50000000 (0 disables the budget)
//	CERBERUS_QUERY_TIMEOUT         default "2m" — per-query wall-clock cap stamped
//	    as ClickHouse max_execution_time (timeout_overflow_mode=throw); the
//	    standard Prometheus ?timeout= param min's with it per request; 0 disables
//	CERBERUS_CH_QUERY_MAX_MEMORY   default 1073741824 bytes = 1GiB (0 = don't set)
//	CERBERUS_CH_BREAKER_ENABLED       default "true"  (false → breaker never trips)
//	CERBERUS_CH_BREAKER_THRESHOLD     default 5   (consecutive failures to trip OPEN)
//	CERBERUS_CH_BREAKER_WINDOW        default "10s" (rolling failure window)
//	CERBERUS_CH_BREAKER_OPEN_INTERVAL default "5s"  (OPEN-state backoff before a probe)
//	CERBERUS_AUTO_CREATE_SCHEMA    default "false"
//	CERBERUS_REQUIREMENTS_CHECK     default "true" — run the boot-time
//	    requirements check (CH server version >= the config-derived minimum
//	    AND deployed schema shape) AFTER the schema-create step; any unmet
//	    requirement fails startup non-zero with an aggregated message.
//	    Set to "false" to skip both gates.
//	CERBERUS_EXPERIMENTAL_TS_GRID_RANGE default "false" — emit ClickHouse-native
//	    timeSeriesRateToGrid for eligible rate query_range; requires ClickHouse
//	    >= 25.6 (prod / compose / e2e are on 25.8, so this floor is met by
//	    default); on older servers the native query 500s with UNKNOWN_FUNCTION
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
//	CERBERUS_SCHEMA_TRACES_TS_LOOKUP            default off (opt-in trace_id_ts window prune)
func FromEnv() (Config, error) {
	v := newLoader()

	dial, err := getDuration(v, envCHDialTimeout)
	if err != nil {
		return Config{}, err
	}
	autoCreate, err := getBool(v, envAutoCreateSchema)
	if err != nil {
		return Config{}, err
	}
	requirementsCheck, err := getBool(v, envRequirementsCheck)
	if err != nil {
		return Config{}, err
	}
	tsGridRange, err := getBool(v, envExperimentalTSGrid)
	if err != nil {
		return Config{}, err
	}
	maxOpenConns, err := getInt(v, envCHMaxOpenConns)
	if err != nil {
		return Config{}, err
	}
	if maxOpenConns <= 0 {
		return Config{}, fmt.Errorf("%s: must be > 0, got %d", envCHMaxOpenConns, maxOpenConns)
	}
	maxIdleConns, err := getInt(v, envCHMaxIdleConns)
	if err != nil {
		return Config{}, err
	}
	if maxIdleConns <= 0 {
		return Config{}, fmt.Errorf("%s: must be > 0, got %d", envCHMaxIdleConns, maxIdleConns)
	}
	connMaxLifetime, err := getDuration(v, envCHConnMaxLifetime)
	if err != nil {
		return Config{}, err
	}
	if connMaxLifetime <= 0 {
		return Config{}, fmt.Errorf("%s: must be > 0, got %s", envCHConnMaxLifetime, connMaxLifetime)
	}
	maxSamples, err := getInt64(v, envQueryMaxSamples)
	if err != nil {
		return Config{}, err
	}
	if maxSamples < 0 {
		return Config{}, fmt.Errorf("%s: must be >= 0, got %d", envQueryMaxSamples, maxSamples)
	}
	maxMemory, err := getInt64(v, envCHQueryMaxMemory)
	if err != nil {
		return Config{}, err
	}
	if maxMemory < 0 {
		return Config{}, fmt.Errorf("%s: must be >= 0, got %d", envCHQueryMaxMemory, maxMemory)
	}
	queryTimeout, err := getDuration(v, envQueryTimeout)
	if err != nil {
		return Config{}, err
	}
	if queryTimeout < 0 {
		return Config{}, fmt.Errorf("%s: must be >= 0, got %s", envQueryTimeout, queryTimeout)
	}
	breaker, err := breakerFromEnv(v)
	if err != nil {
		return Config{}, err
	}
	logCfg, err := envLog(v)
	if err != nil {
		return Config{}, err
	}
	otlp, err := otlpFromEnv(v)
	if err != nil {
		return Config{}, err
	}
	admit, err := admitFromEnv(v)
	if err != nil {
		return Config{}, err
	}
	return Config{
		HTTPAddr: v.GetString(envHTTPAddr),
		ClickHouse: chclient.Config{
			Addr:                v.GetString(envCHAddr),
			Database:            v.GetString(envCHDatabase),
			Username:            v.GetString(envCHUsername),
			Password:            v.GetString(envCHPassword),
			DialTimeout:         dial,
			MaxOpenConns:        maxOpenConns,
			MaxIdleConns:        maxIdleConns,
			ConnMaxLifetime:     connMaxLifetime,
			MaxQuerySamples:     maxSamples,
			MaxQueryMemoryBytes: maxMemory,
			QueryTimeout:        queryTimeout,
			BreakerThreshold:    breaker.Threshold,
			BreakerWindow:       breaker.Window,
			BreakerOpenInterval: breaker.OpenInterval,
			BreakerDisabled:     breaker.Disabled,
		},
		Schema:                  schema.DefaultOTelMetricsFromEnv(),
		Logs:                    schema.DefaultOTelLogsFromEnv(),
		Traces:                  schema.DefaultOTelTracesFromEnv(),
		AutoCreateSchema:        autoCreate,
		RequirementsCheck:       requirementsCheck,
		ExperimentalTSGridRange: tsGridRange,
		Log:                     logCfg,
		OTLP:                    otlp,
		Admit:                   admit,
	}, nil
}

// allEnvKeys is every CERBERUS_* var the loader resolves. Each is both
// the viper key and the literal environment-variable name — they are
// identical by design so the historical CERBERUS_* contract is byte-exact.
var allEnvKeys = []string{
	envHTTPAddr,
	envCHAddr,
	envCHDatabase,
	envCHUsername,
	envCHPassword,
	envCHDialTimeout,
	envCHMaxOpenConns,
	envCHMaxIdleConns,
	envCHConnMaxLifetime,
	envQueryMaxSamples,
	envQueryTimeout,
	envCHQueryMaxMemory,
	envCHBreakerEnabled,
	envCHBreakerThreshold,
	envCHBreakerWindow,
	envCHBreakerOpenIntrvl,
	envAutoCreateSchema,
	envRequirementsCheck,
	envExperimentalTSGrid,
	envLogFormat,
	envLogLevel,
	envOTLPEndpoint,
	envOTLPInsecure,
	envOTLPHeaders,
	envOTLPTimeout,
	envOTLPExportInterval,
	envAdmitDisabled,
	envAdmitProm,
	envAdmitLoki,
	envAdmitTempo,
}

// newLoader builds the per-call viper instance: a fresh viper.New()
// (never the package-global singleton, so the loader is testable and
// embeddable), with every CERBERUS_* key explicitly bound to its
// identically-named environment variable via BindEnv(key, key) and seeded
// with the exact historical default. Per-key BindEnv (rather than
// SetEnvPrefix + AutomaticEnv) is deliberate: our viper keys are already
// the full CERBERUS_<NAME> strings, and AutomaticEnv would re-apply the
// prefix and look up CERBERUS_CERBERUS_<NAME>, breaking the contract.
// An optional `cerberus.yaml` config file is merged in beneath env vars;
// its absence is silently tolerated. Precedence is viper's native
// ordering — env var > config file > default — so a CERBERUS_* env var
// always wins over a file value.
func newLoader() *viper.Viper {
	v := viper.New()
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	for _, key := range allEnvKeys {
		// Two-arg BindEnv binds the viper key to the literal env-var
		// name with no prefix munging — key and env var are the same
		// CERBERUS_<NAME> string. BindEnv only errors on an empty key,
		// which is impossible here.
		_ = v.BindEnv(key, key)
	}

	// Defaults — the exact historical values. Durations/bools are stored
	// as their typed Go values so viper's getters and config-file
	// unmarshalling agree.
	v.SetDefault(envHTTPAddr, defaultHTTPAddr)
	v.SetDefault(envCHAddr, defaultCHAddr)
	v.SetDefault(envCHDatabase, defaultCHDatabase)
	v.SetDefault(envCHUsername, defaultCHUsername)
	v.SetDefault(envCHPassword, defaultCHPassword)
	v.SetDefault(envCHDialTimeout, defaultCHDialTimeout.String())
	v.SetDefault(envCHMaxOpenConns, defaultCHMaxOpenConns)
	v.SetDefault(envCHMaxIdleConns, defaultCHMaxIdleConns)
	v.SetDefault(envCHConnMaxLifetime, defaultCHConnMaxLifetime.String())
	v.SetDefault(envQueryMaxSamples, defaultQueryMaxSamples)
	v.SetDefault(envQueryTimeout, defaultQueryTimeout.String())
	v.SetDefault(envCHQueryMaxMemory, defaultCHQueryMaxMemory)
	v.SetDefault(envCHBreakerEnabled, defaultCHBreakerEnabled)
	v.SetDefault(envCHBreakerThreshold, defaultCHBreakerThreshold)
	v.SetDefault(envCHBreakerWindow, defaultCHBreakerWindow.String())
	v.SetDefault(envCHBreakerOpenIntrvl, defaultCHBreakerOpenInterval.String())
	v.SetDefault(envAutoCreateSchema, defaultAutoCreateSchema)
	v.SetDefault(envRequirementsCheck, defaultRequirementsCheck)
	v.SetDefault(envExperimentalTSGrid, defaultExperimentalTSGrid)
	v.SetDefault(envLogFormat, defaultLogFormat)
	v.SetDefault(envLogLevel, defaultLogLevel)
	v.SetDefault(envOTLPEndpoint, defaultOTLPEndpoint)
	v.SetDefault(envOTLPInsecure, defaultOTLPInsecure)
	v.SetDefault(envOTLPHeaders, defaultOTLPHeaders)
	v.SetDefault(envOTLPTimeout, defaultOTLPTimeout.String())
	v.SetDefault(envOTLPExportInterval, defaultOTLPExportInterval.String())
	v.SetDefault(envAdmitDisabled, defaultAdmitDisabled)
	v.SetDefault(envAdmitProm, defaultAdmitProm)
	v.SetDefault(envAdmitLoki, defaultAdmitLoki)
	v.SetDefault(envAdmitTempo, defaultAdmitTempo)

	// Optional config file: cerberus.yaml in the working directory or
	// /etc/cerberus. Env vars always win (viper precedence: explicit
	// Set > env > config file > default), so the file is purely additive
	// and never overrides an operator's environment. A missing file is
	// not an error; a malformed file is tolerated here and surfaces later
	// only if a value fails the same fail-fast validation env values get.
	v.SetConfigName(configFileBaseName)
	v.SetConfigType("yaml")
	v.AddConfigPath(".")
	v.AddConfigPath("/etc/cerberus")
	// Every ReadInConfig error is tolerated, not just file-not-found: the
	// CERBERUS_* env contract is the source of truth, and a missing OR
	// malformed cerberus.yaml must never take cerberus down. Values still
	// resolve from env vars and built-in defaults, and each one is run
	// through the same fail-fast typed validation regardless of source.
	_ = v.ReadInConfig()
	return v
}

// Built-in defaults, kept as named constants so newLoader's SetDefault
// calls and the doc comment can't drift. String/bool defaults that have
// no other natural home live here; the int / duration budget defaults
// keep their original homes below (they carry longer rationale comments).
const (
	defaultHTTPAddr           = ":8080"
	defaultCHAddr             = "localhost:9000"
	defaultCHDatabase         = "otel"
	defaultCHUsername         = "default"
	defaultCHPassword         = ""
	defaultAutoCreateSchema   = false
	defaultRequirementsCheck  = true
	defaultExperimentalTSGrid = false
	defaultLogFormat          = "text"
	defaultLogLevel           = "info"
	defaultOTLPEndpoint       = ""
	defaultOTLPInsecure       = false
	defaultOTLPHeaders        = ""
	defaultCHBreakerEnabled   = true
	defaultAdmitDisabled      = false
)

const (
	defaultCHDialTimeout      time.Duration = 5 * time.Second
	defaultOTLPTimeout        time.Duration = 10 * time.Second
	defaultOTLPExportInterval time.Duration = 10 * time.Second
)

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

// defaultQueryTimeout is the default per-query wall-clock execution cap:
// 2 minutes, mirroring upstream Prometheus's `--query.timeout` default
// so Grafana / Prom clients see the budget they already expect. It is
// stamped on the DEFAULT route-A data-plane path as ClickHouse's
// per-query `max_execution_time` (with timeout_overflow_mode=throw), so
// a pathological query is aborted server-side with TIMEOUT_EXCEEDED
// (code 159) instead of holding a pooled connection + admit slot for an
// unbounded duration. This is deliberately looser than the solver's
// 60s CERBERUS_SOLVER_TIMEOUT (which guards only the dark route-B
// fan-out): route A is the common single-statement path and a 2m
// ceiling matches the wall-clock budget Prom operators tune against,
// while still capping the unbounded hold the gap left open. The
// standard Prometheus ?timeout= query param min's with this default per
// request. 0 disables the cap (ClickHouse server defaults apply).
const defaultQueryTimeout time.Duration = 2 * time.Minute

// defaultCHQueryMaxMemory is the default ClickHouse per-query memory
// cap (the `max_memory_usage` setting chclient stamps on every
// data-plane query): 1 GiB. Chosen so a single over-broad query (the
// 24h/15s matrix tuple from k3d run 27277793810 demanded 2.12 GiB)
// gets a deterministic resource-exhausted rejection instead of racing
// ClickHouse's server-total cap mid-stream and 502-ing. 0 disables the
// setting entirely (ClickHouse server defaults apply).
const defaultCHQueryMaxMemory int64 = 1 << 30 // 1073741824 bytes

// ClickHouse connection-pool defaults (#81). MaxIdleConns / MaxOpenConns
// reproduce clickhouse-go/v2's previously-implicit defaults verbatim so the
// non-sharded path stays behaviour-compatible (the driver defaulted
// MaxIdleConns to 5, MaxOpenConns to MaxIdleConns+5 = 10). Cerberus sets
// them explicitly here — the ONE place pool sizing is derived — so the
// sharded-pushdown solver can raise the ceiling for fan-out by bumping these
// (or the matching CERBERUS_CH_MAX_OPEN_CONNS / CERBERUS_CH_MAX_IDLE_CONNS /
// CERBERUS_CH_CONN_MAX_LIFETIME env vars) rather than inheriting an implicit
// driver default. When the pool is exhausted an acquire blocks up to
// DialTimeout and then fails with clickhouse.ErrAcquireConnTimeout, which the
// circuit breaker treats neutrally (local pool-sizing signal, not CH-health
// failure).
//
// ConnMaxLifetime DEPARTS from the driver's 1h default: it is the
// recovery-speed CEILING for a RESTARTED ClickHouse backend (ch-pod-kill
// recovery, run 27509796946 — then re-flaked at the 5m value, run
// 27572XXXXXX). clickhouse-go v2.46.0 exposes NO idle-health knob, and a
// force-killed pod's socket can stay ESTABLISHED (no FIN/RST), so the driver's
// per-acquire socket check (conn_check.go) passes the dead conn through as
// healthy. The driver DOES evict a conn the moment a query against it errors
// (clickhouse.release(conn, err) → conn.close()), but the error only surfaces
// AFTER the socket read on the dead peer unblocks — and an idle-but-not-noticed
// stale conn's read blocks for the driver's full ReadTimeout (300s default) or
// the request's ctx budget (the prom head's 2m QueryTimeout). So neither the
// transport-retry nor the per-query eviction fires in seconds: recovery is
// bounded by whichever ages the stale conn out first. ConnMaxLifetime is the
// ONLY age-eviction lever — isBad() retires any conn older than it at acquire,
// and the pool's background drain ticker fires on the same period — so it
// directly sets the worst-case heal window. 5m bounded recovery at ~5m (the
// observed flake: one replica's breaker stayed OPEN until exactly
// process-start + 5m). 30s bounds it to ~30s + one breaker probe interval
// (well under the chaos BREAKER_CLOSE_DEADLINE_MS=300s, and under 60s),
// deterministically and on EVERY replica, because age eviction is
// unconditional. CH native conns are stateless (no session temp tables) and
// cheap to redial, so recycling the idle pool every 30s is negligible churn —
// the steady-state trade-off the short window costs.
const (
	defaultCHMaxOpenConns                  = 10
	defaultCHMaxIdleConns                  = 5
	defaultCHConnMaxLifetime time.Duration = 30 * time.Second
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

// breakerFromEnv reads the CERBERUS_CH_BREAKER_* knobs from the viper
// loader. Unset values use the defaults above, which reproduce the
// pre-#95 hardcoded breaker constants exactly (so defaults are
// byte-unchanged). CERBERUS_CH_BREAKER_ENABLED=false disables the breaker
// entirely (always-allow, never trips); when disabled the threshold /
// window / interval knobs are still validated so a typo doesn't pass
// silently, but they have no runtime effect.
//
// Fail-fast validation: threshold must be >= 1, window > 0, interval > 0.
func breakerFromEnv(v *viper.Viper) (breakerConfig, error) {
	enabled, err := getBool(v, envCHBreakerEnabled)
	if err != nil {
		return breakerConfig{}, err
	}
	threshold, err := getInt(v, envCHBreakerThreshold)
	if err != nil {
		return breakerConfig{}, err
	}
	if threshold < 1 {
		return breakerConfig{}, fmt.Errorf("%s: must be >= 1, got %d", envCHBreakerThreshold, threshold)
	}
	window, err := getDuration(v, envCHBreakerWindow)
	if err != nil {
		return breakerConfig{}, err
	}
	if window <= 0 {
		return breakerConfig{}, fmt.Errorf("%s: must be > 0, got %s", envCHBreakerWindow, window)
	}
	openInterval, err := getDuration(v, envCHBreakerOpenIntrvl)
	if err != nil {
		return breakerConfig{}, err
	}
	if openInterval <= 0 {
		return breakerConfig{}, fmt.Errorf("%s: must be > 0, got %s", envCHBreakerOpenIntrvl, openInterval)
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

// admitFromEnv reads CERBERUS_ADMIT_* knobs from the viper loader.
// Unset values use the conservative defaults above. Setting any cap
// to 0 disables admission control for that head specifically (a
// finer-grained alternative to CERBERUS_ADMIT_DISABLED, which kills
// every head). Negative caps are rejected — they almost certainly
// mean a typo.
func admitFromEnv(v *viper.Viper) (AdmitConfig, error) {
	disabled, err := getBool(v, envAdmitDisabled)
	if err != nil {
		return AdmitConfig{}, err
	}
	prom, err := getInt(v, envAdmitProm)
	if err != nil {
		return AdmitConfig{}, err
	}
	loki, err := getInt(v, envAdmitLoki)
	if err != nil {
		return AdmitConfig{}, err
	}
	tempo, err := getInt(v, envAdmitTempo)
	if err != nil {
		return AdmitConfig{}, err
	}
	for name, val := range map[string]int{
		envAdmitProm:  prom,
		envAdmitLoki:  loki,
		envAdmitTempo: tempo,
	} {
		if val < 0 {
			return AdmitConfig{}, fmt.Errorf("%s: must be >= 0, got %d", name, val)
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

// otlpFromEnv parses the CERBERUS_OTLP_* knobs from the viper loader.
// Empty endpoint is the documented "disabled" state and not an error —
// the caller installs noop providers in that case.
func otlpFromEnv(v *viper.Viper) (OTLPConfig, error) {
	timeout, err := getDuration(v, envOTLPTimeout)
	if err != nil {
		return OTLPConfig{}, err
	}
	insecure, err := getBool(v, envOTLPInsecure)
	if err != nil {
		return OTLPConfig{}, err
	}
	headers, err := parseHeaders(v.GetString(envOTLPHeaders))
	if err != nil {
		return OTLPConfig{}, fmt.Errorf("%s: %w", envOTLPHeaders, err)
	}
	exportInterval, err := getDuration(v, envOTLPExportInterval)
	if err != nil {
		return OTLPConfig{}, err
	}
	return OTLPConfig{
		Endpoint:       strings.TrimSpace(v.GetString(envOTLPEndpoint)),
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
		val := strings.TrimSpace(part[eq+1:])
		if k == "" {
			return nil, fmt.Errorf("entry %q: empty key", part)
		}
		out[k] = val
	}
	return out, nil
}

// getString returns the resolved string value for key (env > file >
// default), trimmed of surrounding whitespace so a pasted newline /
// space is treated the same as the historical os.Getenv-based parser.
func getString(v *viper.Viper, key string) string {
	return strings.TrimSpace(v.GetString(key))
}

// getInt resolves key and parses it as a base-10 int, preserving the
// historical fail-fast contract: a non-integer value is rejected with
// an error that names the offending env var. An empty resolved value
// falls back to the parsed default (viper SetDefault guarantees a
// non-empty default exists for every int key).
func getInt(v *viper.Viper, key string) (int, error) {
	raw := getString(v, key)
	if raw == "" {
		return 0, fmt.Errorf("%s: missing value", key)
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid integer %q: %w", key, raw, err)
	}
	return n, nil
}

// getInt64 resolves key and parses it as a base-10 int64 with the same
// fail-fast, env-var-naming contract as getInt.
func getInt64(v *viper.Viper, key string) (int64, error) {
	raw := getString(v, key)
	if raw == "" {
		return 0, fmt.Errorf("%s: missing value", key)
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid integer %q: %w", key, raw, err)
	}
	return n, nil
}

// getBool resolves key and parses it with the standard strconv.ParseBool
// vocabulary ("1"/"0", "t"/"f", "true"/"false", case-insensitive). A
// value that fails to parse is rejected with an error naming the env
// var — preserving the historical fail-fast-on-misconfiguration contract.
func getBool(v *viper.Viper, key string) (bool, error) {
	raw := getString(v, key)
	if raw == "" {
		return false, fmt.Errorf("%s: missing value", key)
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s: invalid boolean %q: %w", key, raw, err)
	}
	return b, nil
}

// getDuration resolves key and parses it with time.ParseDuration,
// rejecting a malformed value with an error naming the env var.
func getDuration(v *viper.Viper, key string) (time.Duration, error) {
	raw := getString(v, key)
	if raw == "" {
		return 0, fmt.Errorf("%s: missing value", key)
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}

// envLog parses CERBERUS_LOG_FORMAT + CERBERUS_LOG_LEVEL from the viper
// loader into a LogConfig. Unset values default to "text" / "info";
// invalid values fail fast at startup so a typo never silently downgrades
// observability.
func envLog(v *viper.Viper) (LogConfig, error) {
	format := strings.ToLower(getString(v, envLogFormat))
	switch format {
	case "text", "json":
	default:
		return LogConfig{}, fmt.Errorf("%s: invalid value %q (want \"text\" or \"json\")", envLogFormat, format)
	}
	levelStr := strings.ToLower(getString(v, envLogLevel))
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
		return LogConfig{}, fmt.Errorf("%s: invalid value %q (want \"debug\", \"info\", \"warn\", or \"error\")", envLogLevel, levelStr)
	}
	return LogConfig{Format: format, Level: level}, nil
}
