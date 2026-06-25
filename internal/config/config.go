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
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/common/model"
	"github.com/spf13/viper"
	otellog "go.opentelemetry.io/otel/log"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/telemetry"
)

// Config is the cerberus runtime configuration.
type Config struct {
	HTTPAddr   string
	HTTPServer HTTPServerConfig
	ClickHouse chclient.Config
	Schema     schema.Metrics

	// DebugPProf, when true, mounts the net/http/pprof debug handlers
	// (/debug/pprof/…) on the main HTTP listener. Default false — the
	// profiling surface stays OFF in production so it is never reachable
	// unless an operator explicitly opts in via CERBERUS_DEBUG_PPROF=true.
	// It exists so a live heap/CPU profile can be captured from a pod that
	// is mid-incident (the rc.5 e2e OOM investigation needed exactly this:
	// a `wget /debug/pprof/heap` against the running cerberus container
	// before teardown). Gated, not always-on, because the endpoints expose
	// process internals + add an attack surface no production deploy wants
	// open by default.
	DebugPProf bool

	// LokiTailWriteTimeout bounds a single /tail WebSocket write before a
	// slow / dead client is torn down. Promoted from the hardcoded 10s in
	// internal/api/loki/tail.go via CERBERUS_LOKI_TAIL_WRITE_TIMEOUT.
	LokiTailWriteTimeout time.Duration
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

	// AutoCreateDatabase controls whether the auto-create hook also creates
	// the configured database (CREATE DATABASE IF NOT EXISTS), in addition to
	// the tables. It defaults to AutoCreateSchema's value: enabling schema
	// auto-create creates the database too. Set CERBERUS_AUTO_CREATE_DATABASE
	// explicitly to false when the database is provisioned externally — e.g. a
	// Replicated database created by cluster tooling with specific Keeper
	// paths — so cerberus creates only the tables inside it. Because the
	// configured database may not exist yet when cerberus connects (the
	// session's default database), the CREATE DATABASE is issued over a
	// bootstrap connection bound to ClickHouse's always-present `default`
	// database; the fully-qualified table creates work from there too.
	AutoCreateDatabase bool

	// SchemaProvisioning carries the DDL knobs the auto-create hook
	// (CERBERUS_AUTO_CREATE_SCHEMA) uses when it creates the OTel schema.
	// Every field is a no-op unless AutoCreateSchema is true. The zero value
	// is cerberus's single-node shape: an Atomic database, MergeTree tables,
	// no ON CLUSTER, no TTL. The table names are NOT here — auto-create
	// reuses the same Schema / Logs / Traces table names the query heads
	// read, so a CERBERUS_SCHEMA_*_TABLE override creates and reads the same
	// table.
	SchemaProvisioning SchemaProvisioning

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

	// LogCommentShape, when true, lets the engine stamp ClickHouse
	// `log_comment` with a COMPACT, literal-free cerberus shape id
	// (emit-root plan node kind + key modifiers, never any literal values)
	// so operators with query_log enabled can cluster system.query_log rows
	// by normalized_query_hash + log_comment. log_comment is a free-form
	// annotation ignored by execution, so stamping it is result-neutral and
	// version-safe.
	//
	// Default false: it ships DARK behind CERBERUS_LOG_COMMENT_SHAPE.
	LogCommentShape bool

	// CHOptimizations is the raw CERBERUS_CH_OPTIMIZATIONS value ("auto" |
	// "off" | comma-separated feature ids). It is the auto-picker's selection;
	// the actual EnabledSet is resolved ONCE in cmd/cerberus AFTER the runtime
	// version probe (the probe needs a live connection, which FromEnv does not
	// have), so FromEnv carries only the raw string here.
	CHOptimizations string

	// CHOptimizationsMode is the parsed CERBERUS_CH_OPTIMIZATIONS_MODE
	// (enforcing | permissive, default enforcing). It governs how the resolver
	// treats an explicitly-requested feature the connected server is too old
	// for: FATAL (enforcing, default) vs WARN + skip (permissive). Ignored
	// under auto/off.
	CHOptimizationsMode chopt.Mode

	// LegacyTSGridFlag carries the tri-state deprecated
	// CERBERUS_EXPERIMENTAL_TS_GRID_RANGE alias (unset vs explicit true/false).
	// cmd/cerberus passes it into chopt.Resolve so the legacy flag is re-routed
	// through the resolver rather than read directly by the lowering/engine.
	LegacyTSGridFlag chopt.LegacyFlag

	// CHOptCorpus configures the async system.query_log performance-corpus
	// reconciler (disabled by default; production-only — chDB has no
	// query_log).
	CHOptCorpus CHOptCorpusConfig

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

	// Admit configures per-handler admission control. When Admit.Disabled
	// is true cerberus skips the admission middleware entirely and every
	// request is admitted. Otherwise each per-head toggle (Admit.Prom /
	// Loki / Tempo) enables a counted semaphore for that API head at its
	// conservative default cap — requests above the cap are rejected with
	// HTTP 503 + Retry-After: 1 so well-behaved clients back off and CH
	// stays out of overload. A falsy per-head toggle leaves that head
	// unlimited.
	Admit AdmitConfig

	// EnabledHeads is the set of query heads (prom / loki / tempo) this
	// process serves, parsed from the comma-separated CERBERUS_ENABLED_HEADS.
	// Default is all three (full backward compatibility — an unset value
	// behaves exactly as a process that serves every head). A subset lets a
	// deployment run a single head in its own process/cgroup so one head
	// OOMing can no longer sever the others (the feasibility gate for
	// splitting cerberus into per-head Kubernetes deployments). The /healthz
	// + /readyz probes are unconditional regardless of this set. Consult it
	// through HeadEnabled, never by reaching into the map directly.
	EnabledHeads EnabledHeads
}

// Head identifies one of cerberus's three query heads. The string values
// are the tokens CERBERUS_ENABLED_HEADS accepts (case-insensitive) and the
// labels other config / log surfaces already use for these heads.
type Head string

const (
	HeadProm  Head = "prom"
	HeadLoki  Head = "loki"
	HeadTempo Head = "tempo"
)

// defaultEnabledHeads is the CERBERUS_ENABLED_HEADS default: all three heads.
// An unset value therefore serves prom + loki + tempo exactly as cerberus
// always has.
const defaultEnabledHeads = "prom,loki,tempo"

// EnabledHeads is the resolved set of heads a process serves. The zero value
// is an empty set (no heads) — FromEnv always populates it, defaulting to all
// three, so an empty set only ever arises from an explicit, validated
// CERBERUS_ENABLED_HEADS that listed nothing, which FromEnv rejects.
type EnabledHeads map[Head]struct{}

// HeadEnabled reports whether this process serves head. cmd/cerberus gates
// each head's handler/client/limiter build + route Mount (and the Tempo gRPC
// service) on it; the health probes never consult it.
func (c Config) HeadEnabled(head Head) bool {
	_, ok := c.EnabledHeads[head]
	return ok
}

// headFromToken maps a single CERBERUS_ENABLED_HEADS token to its Head,
// case-insensitively and whitespace-trimmed. An unknown token is rejected so
// a typo (e.g. "promql", "traces") fails startup loudly instead of silently
// disabling a head.
func headFromToken(tok string) (Head, bool) {
	switch Head(strings.ToLower(strings.TrimSpace(tok))) {
	case HeadProm:
		return HeadProm, true
	case HeadLoki:
		return HeadLoki, true
	case HeadTempo:
		return HeadTempo, true
	default:
		return "", false
	}
}

// enabledHeadsFromEnv parses the comma-separated CERBERUS_ENABLED_HEADS into
// a validated set. The default (all three heads) preserves full backward
// compatibility. Each token must name a known head (prom / loki / tempo,
// case-insensitive); an unknown token or an effectively-empty list (after
// trimming) is rejected fail-fast so a misconfiguration trips startup rather
// than silently serving no heads.
func enabledHeadsFromEnv(v *viper.Viper) (EnabledHeads, error) {
	raw := getString(v, envEnabledHeads)
	if raw == "" {
		raw = defaultEnabledHeads
	}
	set := EnabledHeads{}
	for _, tok := range strings.Split(raw, ",") {
		if strings.TrimSpace(tok) == "" {
			continue
		}
		head, ok := headFromToken(tok)
		if !ok {
			return nil, fmt.Errorf("%s: unknown head %q: want a comma-separated subset of prom,loki,tempo", envEnabledHeads, strings.TrimSpace(tok))
		}
		set[head] = struct{}{}
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("%s: no heads enabled: want a comma-separated subset of prom,loki,tempo", envEnabledHeads)
	}
	return set, nil
}

// CHOptCorpusConfig configures the async system.query_log performance-corpus
// reconciler (internal/optcorpus). The reconciler keeps a bounded ring of
// recently-dispatched cerberus query_ids, periodically joins them back to
// system.query_log for their server-side cost, and appends the
// (shape-id, enabled-opts, timings) tuples to a durable JSONL sink. It is
// disabled by default and production-only: chDB (the parity test substrate)
// has no system.query_log, so the reconciler is never started there.
type CHOptCorpusConfig struct {
	// Enabled gates the whole reconciler (CERBERUS_CH_OPT_CORPUS_ENABLED).
	Enabled bool
	// Interval is how often the reconciler reconciles recent query_ids against
	// system.query_log (CERBERUS_CH_OPT_CORPUS_INTERVAL, default 60s).
	Interval time.Duration
	// SinkPath is the JSONL sink path (CERBERUS_CH_OPT_CORPUS_SINK_PATH). Empty
	// disables the file sink.
	SinkPath string
	// RingCapacity bounds the in-memory ring of recently-dispatched query_ids
	// the reconciler tracks (CERBERUS_CH_OPT_CORPUS_RING, default 4096). It caps
	// the reconciler's memory and the size of each per-interval IN(...) join; the
	// ring drops the oldest id when full. A non-positive value falls back to the
	// optcorpus default.
	RingCapacity int
	// SinkMode selects the durable sink (CERBERUS_CH_OPT_CORPUS_SINK_MODE):
	// "jsonl" (default) appends rows to the SinkPath file; "chtable" writes them
	// to the cerberus_router_corpus MergeTree the operator queries with the
	// go/no-go analysis SQL. The CH-table sink needs no SinkPath. Any
	// unrecognised value falls back to the JSONL sink.
	SinkMode string
}

// SchemaProvisioning carries the DDL-shaping knobs the auto-create hook
// applies when CERBERUS_AUTO_CREATE_SCHEMA=true. They mirror the typed
// internal/schema/ddl Config surface; the zero value is the single-node
// shape cerberus ships by default (Atomic database, MergeTree tables, no
// cluster, no TTL).
type SchemaProvisioning struct {
	// Cluster (CERBERUS_SCHEMA_CLUSTER) renders an ON CLUSTER clause into
	// every CREATE statement — the classic distributed-DDL model. Mutually
	// exclusive with DatabaseReplicated (a Replicated database replicates
	// DDL itself); leave it empty for a single-node or Replicated-database
	// deployment.
	Cluster string

	// TableEngine (CERBERUS_SCHEMA_TABLE_ENGINE) overrides the table engine.
	// Empty renders the upstream default `MergeTree()` — or, when
	// DatabaseReplicated is set, the bare `ReplicatedMergeTree` (no args): a
	// Replicated database does NOT auto-convert MergeTree, so the tables need a
	// replicated engine to replicate their DATA, and inside a Replicated
	// database the engine's path/replica are supplied automatically (explicit
	// args are rejected, code 36). Set this only to pin a non-default engine —
	// e.g. a classic ON CLUSTER cluster needing an explicit
	// `ReplicatedMergeTree('/path', '{replica}')`.
	TableEngine string

	// TTL (CERBERUS_SCHEMA_TTL) is the DEFAULT retention applied to every
	// signal's tables (e.g. `2160h` = 90 days). Zero (the default) leaves
	// retention to the operator — no TTL clause is emitted. The per-signal
	// overrides below take precedence when set.
	TTL time.Duration

	// TTLMetrics / TTLLogs / TTLTraces (CERBERUS_SCHEMA_TTL_METRICS / _LOGS /
	// _TRACES) override TTL for a single signal. Observability retention is
	// conventionally per-signal — logs are voluminous and short-lived,
	// metrics long-lived — so each signal can diverge from the global
	// default. A zero value inherits TTL; a non-zero value overrides it.
	TTLMetrics time.Duration
	TTLLogs    time.Duration
	TTLTraces  time.Duration

	// DatabaseReplicated (CERBERUS_SCHEMA_DATABASE_REPLICATED) creates the
	// database with `ENGINE = Replicated(...)`. A Replicated database
	// auto-replicates all DDL across replicas, so no ON CLUSTER clause is
	// needed — but it does NOT auto-convert MergeTree tables to
	// ReplicatedMergeTree, so cerberus emits a bare `ReplicatedMergeTree` table
	// engine (no args — the database supplies the Keeper coordinates) to
	// replicate the DATA. Requires DatabaseReplicatedPath.
	DatabaseReplicated bool

	// DatabaseReplicatedPath (CERBERUS_SCHEMA_DATABASE_REPLICATED_PATH) is
	// the ZooKeeper/Keeper path the Replicated engine coordinates on, e.g.
	// `/clickhouse/databases/otel`. Required when DatabaseReplicated is true.
	DatabaseReplicatedPath string

	// DatabaseReplicatedShard / DatabaseReplicatedReplica
	// (CERBERUS_SCHEMA_DATABASE_REPLICATED_SHARD / _REPLICA) name the shard
	// and replica the engine identifies this node by. Empty falls back to
	// the ClickHouse server macros `{shard}` / `{replica}` (the conventional
	// cluster setup), resolved in internal/schema/ddl.
	DatabaseReplicatedShard   string
	DatabaseReplicatedReplica string

	// StoragePolicy (CERBERUS_SCHEMA_STORAGE_POLICY) is the typed shorthand
	// for the MergeTree `storage_policy` setting on every auto-created table —
	// the common S3 / tiered-storage knob. Empty (the default) appends no
	// storage_policy. When set it is folded into the SETTINGS tail PINNED
	// FIRST (before Settings) so the emitted DDL is deterministic. Setting it
	// AND also putting a `storage_policy` key in Settings is rejected at
	// startup (one way to set it).
	StoragePolicy string

	// Settings (CERBERUS_SCHEMA_SETTINGS, `k=v,k2=v2`) is the generic
	// MergeTree-SETTINGS escape hatch: an ORDERED list of extra settings
	// appended to every auto-created table's SETTINGS tail (e.g.
	// `min_bytes_for_wide_part=0`). Order is preserved for deterministic DDL.
	// Empty (the default) appends nothing — strict backward compatibility.
	Settings []schema.KV
}

// AdmitConfig holds the per-handler admission-control concurrency caps.
//
// CERBERUS_ADMIT_{PROM,LOKI,TEMPO} set the per-head in-flight concurrency
// cap. Each accepts EITHER an explicit non-negative integer cap OR a
// boolean spelling, so one knob serves both an operator pinning an exact
// cap and the Helm chart rendering a plain YAML bool:
//
//   - a positive integer N -> cap the head at N concurrent requests
//   - "true"  / "t"        -> enable at the head's conservative default
//     cap (DefaultAdmitProm / Loki / Tempo)
//   - "false" / "f" / "0"  -> no limiter for that head (unlimited)
//   - a negative or otherwise unparseable value -> rejected (fail fast)
//
// "1"/"0" are read as the integer caps 1 and 0 (and 0 == unlimited), so
// the four canonical booleans 1/0/true/false all behave intuitively while
// an exact cap like 2 is still honoured. Tempo's default cap is half of
// Prom/Loki because trace queries are typically the heaviest per-call
// (full trace span fetches + tag-value scans across wide column sets).
//
// CERBERUS_ADMIT_DISABLED is a separate plain-bool master switch that
// removes admission control on every head at once, independent of the
// per-head caps below.
type AdmitConfig struct {
	// Disabled, when true, removes admission control entirely. Handy
	// for local development where artificial caps mask real
	// concurrency bugs. Default false (admission control enabled).
	Disabled bool

	// Prom caps simultaneous in-flight Prom API requests. 0 leaves the
	// head unlimited. Default DefaultAdmitProm (CERBERUS_ADMIT_PROM
	// unset or "true").
	Prom int

	// Loki caps simultaneous in-flight Loki API requests. 0 leaves the
	// head unlimited. Default DefaultAdmitLoki (CERBERUS_ADMIT_LOKI
	// unset or "true").
	Loki int

	// Tempo caps simultaneous in-flight Tempo API requests. 0 leaves the
	// head unlimited. Default DefaultAdmitTempo (CERBERUS_ADMIT_TEMPO
	// unset or "true").
	Tempo int
}

// HTTPServerConfig holds the cerberus HTTP server's net/http timeout knobs
// (cmd/cerberus's http.Server). Every field maps 1:1 to an http.Server field.
//
// Defaults preserve today's behaviour: ReadHeaderTimeout is the promoted 5s
// that was previously hardcoded; ReadTimeout and WriteTimeout default to 0
// (unlimited) so the Loki /tail WebSocket and long query_range matrix streams
// are never severed mid-response by a server-side deadline; IdleTimeout bounds
// an otherwise-leaked keep-alive connection; MaxHeaderBytes 0 leaves Go's
// 1 MiB default.
type HTTPServerConfig struct {
	// ReadTimeout caps the time to read the ENTIRE request including the body
	// (http.Server.ReadTimeout). 0 = unlimited. A streaming-safe default.
	ReadTimeout time.Duration

	// ReadHeaderTimeout caps the time to read request HEADERS
	// (http.Server.ReadHeaderTimeout). Default 5s (the promoted hardcoded
	// value). Must be <= ReadTimeout when ReadTimeout > 0.
	ReadHeaderTimeout time.Duration

	// WriteTimeout caps the time to write the response
	// (http.Server.WriteTimeout). 0 = unlimited — required so the /tail
	// WebSocket and long matrix responses are not cut mid-stream.
	WriteTimeout time.Duration

	// IdleTimeout caps how long an idle keep-alive connection is held open
	// (http.Server.IdleTimeout). Default 120s.
	IdleTimeout time.Duration

	// MaxHeaderBytes caps request header size (http.Server.MaxHeaderBytes).
	// 0 leaves Go's 1 MiB default.
	MaxHeaderBytes int

	// MaxBodyBytes caps an inbound HTTP request body (applied via
	// http.MaxBytesReader in cmd/cerberus's HTTP dispatcher). Default 4 MiB;
	// 0 disables the cap. The gRPC path is unaffected.
	MaxBodyBytes int64
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
	envCHKeepAliveEnabled  = "CERBERUS_CH_KEEPALIVE_ENABLED"
	envCHKeepAliveIdle     = "CERBERUS_CH_KEEPALIVE_IDLE"
	envCHKeepAliveInterval = "CERBERUS_CH_KEEPALIVE_INTERVAL"
	envCHKeepAliveCount    = "CERBERUS_CH_KEEPALIVE_COUNT"
	envQueryMaxSamples     = "CERBERUS_QUERY_MAX_SAMPLES"
	envQueryTimeout        = "CERBERUS_QUERY_TIMEOUT"
	envCHQueryMaxMemory    = "CERBERUS_CH_QUERY_MAX_MEMORY"
	envCHBreakerEnabled    = "CERBERUS_CH_BREAKER_ENABLED"
	envCHBreakerThreshold  = "CERBERUS_CH_BREAKER_THRESHOLD"
	envCHBreakerWindow     = "CERBERUS_CH_BREAKER_WINDOW"
	envCHBreakerOpenIntrvl = "CERBERUS_CH_BREAKER_OPEN_INTERVAL"
	envCHProtocol          = "CERBERUS_CH_PROTOCOL"
	envCHConnOpenStrategy  = "CERBERUS_CH_CONN_OPEN_STRATEGY"
	envCHReadTimeout       = "CERBERUS_CH_READ_TIMEOUT"
	envCHCompression       = "CERBERUS_CH_COMPRESSION"
	envCHCompressionLevel  = "CERBERUS_CH_COMPRESSION_LEVEL"
	envCHBlockBufferSize   = "CERBERUS_CH_BLOCK_BUFFER_SIZE"
	envCHMaxComprBuffer    = "CERBERUS_CH_MAX_COMPRESSION_BUFFER"
	envCHFreeBufOnRelease  = "CERBERUS_CH_FREE_BUF_ON_CONN_RELEASE"
	envCHDebug             = "CERBERUS_CH_DEBUG"
	envCHTLSEnabled        = "CERBERUS_CH_TLS_ENABLED"
	envCHTLSCAFile         = "CERBERUS_CH_TLS_CA_FILE"
	envCHTLSCertFile       = "CERBERUS_CH_TLS_CERT_FILE"
	envCHTLSKeyFile        = "CERBERUS_CH_TLS_KEY_FILE"
	envCHTLSServerName     = "CERBERUS_CH_TLS_SERVER_NAME"
	envCHTLSSkipVerify     = "CERBERUS_CH_TLS_INSECURE_SKIP_VERIFY"
	envCHHTTPHeaders       = "CERBERUS_CH_HTTP_HEADERS"
	envCHHTTPURLPath       = "CERBERUS_CH_HTTP_URL_PATH"
	envCHHTTPMaxConns      = "CERBERUS_CH_HTTP_MAX_CONNS_PER_HOST"
	envCHHTTPProxyURL      = "CERBERUS_CH_HTTP_PROXY_URL"
	envHTTPReadTimeout     = "CERBERUS_HTTP_READ_TIMEOUT"
	envHTTPReadHdrTimeout  = "CERBERUS_HTTP_READ_HEADER_TIMEOUT"
	envHTTPWriteTimeout    = "CERBERUS_HTTP_WRITE_TIMEOUT" //nolint:gosec // env-var name, not a credential
	envHTTPIdleTimeout     = "CERBERUS_HTTP_IDLE_TIMEOUT"
	envHTTPMaxHeaderBytes  = "CERBERUS_HTTP_MAX_HEADER_BYTES"
	envHTTPMaxBodyBytes    = "CERBERUS_HTTP_MAX_BODY_BYTES"
	envLokiTailWriteTO     = "CERBERUS_LOKI_TAIL_WRITE_TIMEOUT"
	envDebugPProf          = "CERBERUS_DEBUG_PPROF"
	envAutoCreateSchema    = "CERBERUS_AUTO_CREATE_SCHEMA"
	envAutoCreateDatabase  = "CERBERUS_AUTO_CREATE_DATABASE"
	envSchemaCluster       = "CERBERUS_SCHEMA_CLUSTER"
	envSchemaTableEngine   = "CERBERUS_SCHEMA_TABLE_ENGINE"
	envSchemaTTL           = "CERBERUS_SCHEMA_TTL"
	envSchemaTTLMetrics    = "CERBERUS_SCHEMA_TTL_METRICS"
	envSchemaTTLLogs       = "CERBERUS_SCHEMA_TTL_LOGS"
	envSchemaTTLTraces     = "CERBERUS_SCHEMA_TTL_TRACES"
	envSchemaDBReplicated  = "CERBERUS_SCHEMA_DATABASE_REPLICATED"
	envSchemaDBReplPath    = "CERBERUS_SCHEMA_DATABASE_REPLICATED_PATH"
	envSchemaDBReplShard   = "CERBERUS_SCHEMA_DATABASE_REPLICATED_SHARD"
	envSchemaDBReplReplica = "CERBERUS_SCHEMA_DATABASE_REPLICATED_REPLICA"
	envSchemaStoragePolicy = "CERBERUS_SCHEMA_STORAGE_POLICY"
	envSchemaSettings      = "CERBERUS_SCHEMA_SETTINGS"
	envRequirementsCheck   = "CERBERUS_REQUIREMENTS_CHECK"
	envExperimentalTSGrid  = "CERBERUS_EXPERIMENTAL_TS_GRID_RANGE"
	envLogCommentShape     = "CERBERUS_LOG_COMMENT_SHAPE"
	envCHOptimizations     = "CERBERUS_CH_OPTIMIZATIONS"
	envCHOptimizationsMode = "CERBERUS_CH_OPTIMIZATIONS_MODE"
	envCHOptCorpusEnabled  = "CERBERUS_CH_OPT_CORPUS_ENABLED"
	envCHOptCorpusInterval = "CERBERUS_CH_OPT_CORPUS_INTERVAL"
	envCHOptCorpusSinkPath = "CERBERUS_CH_OPT_CORPUS_SINK_PATH"
	envCHOptCorpusRing     = "CERBERUS_CH_OPT_CORPUS_RING"
	envCHOptCorpusSinkMode = "CERBERUS_CH_OPT_CORPUS_SINK_MODE"
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
	envEnabledHeads        = "CERBERUS_ENABLED_HEADS"
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
//	CERBERUS_CH_DATABASE           default "default"
//	CERBERUS_CH_USERNAME           default "default"
//	CERBERUS_CH_PASSWORD           default ""
//	CERBERUS_CH_DIAL_TIMEOUT       default "5s"
//	CERBERUS_CH_MAX_OPEN_CONNS     default 10 (total pooled conns, busy + idle)
//	CERBERUS_CH_MAX_IDLE_CONNS     default 5  (idle conns kept warm for reuse)
//	CERBERUS_CH_CONN_MAX_LIFETIME  default "30s" (max age before a pooled conn is recycled)
//	CERBERUS_CH_KEEPALIVE_ENABLED  default "true" (TCP keepalive on CH sockets; bounds dead-peer detection after a restart)
//	CERBERUS_CH_KEEPALIVE_IDLE     default "10s"  (idle before the first keepalive probe)
//	CERBERUS_CH_KEEPALIVE_INTERVAL default "5s"   (gap between keepalive probes)
//	CERBERUS_CH_KEEPALIVE_COUNT    default 3      (unanswered probes before the socket is declared dead)
//	CERBERUS_QUERY_MAX_SAMPLES     default 5000000 (0 disables the budget)
//	CERBERUS_QUERY_TIMEOUT         default "2m" — per-query wall-clock cap stamped
//	    as ClickHouse max_execution_time (timeout_overflow_mode=throw); the
//	    standard Prometheus ?timeout= param min's with it per request; 0 disables
//	CERBERUS_CH_QUERY_MAX_MEMORY   default 1073741824 bytes = 1GiB (0 = don't set)
//	CERBERUS_CH_BREAKER_ENABLED       default "true"  (false → breaker never trips)
//	CERBERUS_CH_BREAKER_THRESHOLD     default 5   (consecutive failures to trip OPEN)
//	CERBERUS_CH_BREAKER_WINDOW        default "10s" (rolling failure window)
//	CERBERUS_CH_BREAKER_OPEN_INTERVAL default "5s"  (OPEN-state backoff before a probe)
//	CERBERUS_CH_PROTOCOL           default "native" ("native" | "http")
//	CERBERUS_CH_CONN_OPEN_STRATEGY default "in_order" ("in_order" | "round_robin")
//	CERBERUS_CH_READ_TIMEOUT       default "" (empty → derived from CERBERUS_QUERY_TIMEOUT;
//	    when set must be >= CERBERUS_QUERY_TIMEOUT; clickhouse-go has NO write-timeout knob)
//	CERBERUS_CH_COMPRESSION        default "none" ("none" | "lz4" | "zstd")
//	CERBERUS_CH_COMPRESSION_LEVEL  default 0 (unset; lz4 0..12, zstd 1..22; requires a method)
//	CERBERUS_CH_BLOCK_BUFFER_SIZE  default 0 (unset → driver 2; valid 1..255)
//	CERBERUS_CH_MAX_COMPRESSION_BUFFER default 0 (unset → driver 10 MiB; bytes, > 0)
//	CERBERUS_CH_FREE_BUF_ON_CONN_RELEASE default "false"
//	CERBERUS_CH_DEBUG              default "false" (clickhouse-go legacy stdout debug)
//	CERBERUS_CH_TLS_ENABLED        default "false" (dial CH over TLS; sub-knobs require this)
//	CERBERUS_CH_TLS_CA_FILE        default "" (PEM CA bundle path)
//	CERBERUS_CH_TLS_CERT_FILE      default "" (client cert for mTLS; pairs with KEY_FILE)
//	CERBERUS_CH_TLS_KEY_FILE       default "" (client key for mTLS; pairs with CERT_FILE)
//	CERBERUS_CH_TLS_SERVER_NAME    default "" (SNI / cert-verify hostname override)
//	CERBERUS_CH_TLS_INSECURE_SKIP_VERIFY default "false" (skip cert verify; incompatible with CA/SERVER_NAME)
//	CERBERUS_CH_HTTP_HEADERS       default "" (HTTP-protocol only; "k=v,k2=v2")
//	CERBERUS_CH_HTTP_URL_PATH      default "" (HTTP-protocol only)
//	CERBERUS_CH_HTTP_MAX_CONNS_PER_HOST default 0 (HTTP-protocol only; unset → driver default)
//	CERBERUS_CH_HTTP_PROXY_URL     default "" (HTTP-protocol only; absolute URL)
//	CERBERUS_HTTP_READ_TIMEOUT     default "0s" (whole-request read; 0 = unlimited / streaming-safe)
//	CERBERUS_HTTP_READ_HEADER_TIMEOUT default "5s" (header read; <= READ_TIMEOUT when that is > 0)
//	CERBERUS_HTTP_WRITE_TIMEOUT    default "0s" (response write; 0 = unlimited / streaming-safe)
//	CERBERUS_HTTP_IDLE_TIMEOUT     default "120s" (idle keep-alive connection)
//	CERBERUS_HTTP_MAX_HEADER_BYTES default 0 (0 → Go's 1 MiB default)
//	CERBERUS_LOKI_TAIL_WRITE_TIMEOUT default "10s" (single /tail WebSocket write bound; > 0)
//	CERBERUS_AUTO_CREATE_SCHEMA    default "false"
//	CERBERUS_AUTO_CREATE_DATABASE  default = CERBERUS_AUTO_CREATE_SCHEMA — also
//	    create the database (over a bootstrap connection to the always-present
//	    `default` db). Set false to create only the tables (externally-managed db).
//	CERBERUS_SCHEMA_CLUSTER        default "" — ON CLUSTER clause for auto-create
//	    DDL (classic distributed-DDL clusters). Mutually exclusive with
//	    CERBERUS_SCHEMA_DATABASE_REPLICATED.
//	CERBERUS_SCHEMA_TABLE_ENGINE   default "" → MergeTree(), or the bare
//	    ReplicatedMergeTree (no args) when CERBERUS_SCHEMA_DATABASE_REPLICATED=true
//	    (a Replicated database does NOT auto-convert MergeTree, and explicit
//	    engine args are rejected there); set only to pin some other non-default
//	    engine — e.g. a classic ON CLUSTER ReplicatedMergeTree('/path','{replica}')
//	CERBERUS_SCHEMA_TTL            default "0s" — global default retention for all
//	    signals (no TTL clause when 0; e.g. "2160h" = 90d)
//	CERBERUS_SCHEMA_TTL_METRICS   default inherits CERBERUS_SCHEMA_TTL; per-signal override
//	CERBERUS_SCHEMA_TTL_LOGS      default inherits CERBERUS_SCHEMA_TTL; per-signal override
//	CERBERUS_SCHEMA_TTL_TRACES    default inherits CERBERUS_SCHEMA_TTL; per-signal override
//	CERBERUS_SCHEMA_DATABASE_REPLICATED default "false" — create the database with
//	    ENGINE = Replicated(...) so DDL auto-replicates across replicas (no ON
//	    CLUSTER needed); cerberus emits bare ReplicatedMergeTree tables to
//	    replicate the DATA (a Replicated database does NOT auto-convert MergeTree)
//	CERBERUS_SCHEMA_DATABASE_REPLICATED_PATH default "" — Keeper path, required when
//	    CERBERUS_SCHEMA_DATABASE_REPLICATED=true (e.g. "/clickhouse/databases/otel")
//	CERBERUS_SCHEMA_DATABASE_REPLICATED_SHARD   default "{shard}" server macro
//	CERBERUS_SCHEMA_DATABASE_REPLICATED_REPLICA default "{replica}" server macro
//	CERBERUS_REQUIREMENTS_CHECK     default "true" — run the boot-time
//	    requirements check (CH server version >= the config-derived minimum
//	    AND deployed schema shape) AFTER the schema-create step; any unmet
//	    requirement fails startup non-zero with an aggregated message.
//	    Set to "false" to skip both gates.
//	CERBERUS_EXPERIMENTAL_TS_GRID_RANGE default "false" — emit ClickHouse-native
//	    timeSeriesRateToGrid for eligible rate query_range; requires ClickHouse
//	    >= 25.6 (prod / compose / e2e are on 25.8, so this floor is met by
//	    default); on older servers the native query 500s with UNKNOWN_FUNCTION
//	CERBERUS_LOG_COMMENT_SHAPE     default "false" — stamp a compact, literal-
//	    free cerberus shape id into ClickHouse log_comment so query_log rows
//	    cluster by normalized_query_hash + log_comment. Result-neutral, safe
//	    on CH 24.8; DARK by default.
//	CERBERUS_LOG_FORMAT            default "text"  ("text" | "json")
//	CERBERUS_LOG_LEVEL             default "info"  ("debug" | "info" | "warn" | "error")
//	CERBERUS_OTLP_ENDPOINT         default ""   (empty → exporters disabled)
//	CERBERUS_OTLP_INSECURE         default "false"
//	CERBERUS_OTLP_HEADERS          default ""   ("k=v,k2=v2" comma-separated)
//	CERBERUS_OTLP_TIMEOUT          default "10s"
//	CERBERUS_OTLP_EXPORT_INTERVAL  default "10s" (metric PeriodicReader flush interval)
//	CERBERUS_ADMIT_DISABLED        default "false" (master off switch, bool)
//	CERBERUS_ADMIT_PROM            default "64"  (Prom cap; int N, or true/false)
//	CERBERUS_ADMIT_LOKI            default "64"  (Loki cap; int N, or true/false)
//	CERBERUS_ADMIT_TEMPO           default "32"  (Tempo cap; int N, or true/false)
//	CERBERUS_ENABLED_HEADS         default "prom,loki,tempo" — comma-separated
//	    subset of query heads this process serves. Unset = all three (full
//	    backward compatibility). A subset (e.g. "prom") skips building AND
//	    mounting the other heads' handler/client/limiter so one head's process
//	    can be isolated per-deployment; /healthz + /readyz stay served in every
//	    mode. An unknown head or an empty list fails startup.
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
//	CERBERUS_PROM_RESOURCE_LABELS              default "" (empty = project ALL OTel
//	                                           ResourceAttributes keys as Prometheus
//	                                           labels) — comma-separated allowlist of
//	                                           resource-attribute keys (dotted OTel form)
//	                                           to surface; sanitized dot->underscore on
//	                                           the wire
func FromEnv() (Config, error) {
	v := newLoader()

	dial, err := getDuration(v, envCHDialTimeout)
	if err != nil {
		return Config{}, err
	}
	flags, err := bootFlagsFromEnv(v)
	if err != nil {
		return Config{}, err
	}
	schemaProvisioning, err := schemaProvisioningFromEnv(v)
	if err != nil {
		return Config{}, err
	}
	maxOpenConns, err := getPositiveInt(v, envCHMaxOpenConns)
	if err != nil {
		return Config{}, err
	}
	maxIdleConns, err := getPositiveInt(v, envCHMaxIdleConns)
	if err != nil {
		return Config{}, err
	}
	connMaxLifetime, err := getDuration(v, envCHConnMaxLifetime)
	if err != nil {
		return Config{}, err
	}
	if connMaxLifetime <= 0 {
		return Config{}, fmt.Errorf("%s: must be > 0, got %s", envCHConnMaxLifetime, connMaxLifetime)
	}
	keepAliveEnabled, err := getBool(v, envCHKeepAliveEnabled)
	if err != nil {
		return Config{}, err
	}
	keepAliveIdle, err := getDuration(v, envCHKeepAliveIdle)
	if err != nil {
		return Config{}, err
	}
	keepAliveInterval, err := getDuration(v, envCHKeepAliveInterval)
	if err != nil {
		return Config{}, err
	}
	keepAliveCount, err := getInt(v, envCHKeepAliveCount)
	if err != nil {
		return Config{}, err
	}
	// Validate the keepalive timing knobs only when keepalive is enabled —
	// a degenerate idle/interval/count would arm a useless or never-firing
	// probe schedule. When disabled the values are inert, so they are not
	// gated (mirrors how the breaker knobs are inert when the breaker is off).
	if keepAliveEnabled {
		if keepAliveIdle <= 0 {
			return Config{}, fmt.Errorf("%s: must be > 0, got %s", envCHKeepAliveIdle, keepAliveIdle)
		}
		if keepAliveInterval <= 0 {
			return Config{}, fmt.Errorf("%s: must be > 0, got %s", envCHKeepAliveInterval, keepAliveInterval)
		}
		if keepAliveCount <= 0 {
			return Config{}, fmt.Errorf("%s: must be > 0, got %d", envCHKeepAliveCount, keepAliveCount)
		}
	}
	maxSamples, err := getInt64(v, envQueryMaxSamples)
	if err != nil {
		return Config{}, err
	}
	if maxSamples < 0 {
		return Config{}, fmt.Errorf("%s: must be >= 0, got %d", envQueryMaxSamples, maxSamples)
	}
	// CERBERUS_CH_QUERY_MAX_MEMORY is a byte size: it accepts BOTH the
	// historical raw-integer-of-bytes form (exact BWC) AND a humanized
	// Kubernetes-style size like 2Gi / 500Mi / 1G. getByteSize already rejects
	// a negative value, so no extra >= 0 guard is needed here.
	maxMemory, err := getByteSize(v, envCHQueryMaxMemory)
	if err != nil {
		return Config{}, err
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
	enabledHeads, err := enabledHeadsFromEnv(v)
	if err != nil {
		return Config{}, err
	}
	// Full-surface ClickHouse connection knobs (protocol, multi-host, TLS,
	// compression, buffers, HTTP-only) + the HTTP server timeouts + the Loki
	// tail write timeout, all parsed and cross-validated in one helper so
	// FromEnv stays readable. The CROSS-setting dependency matrix is enforced
	// inside surfaceFromEnv, where the query / pool knobs are threaded in.
	// The idle<=open rule fires only when the operator EXPLICITLY set idle:
	// lowering only MAX_OPEN_CONNS below the default idle of 5 is a common,
	// coherent idiom (clickhouse-go clamps idle to open internally), so a
	// defaulted idle must not be punished. viper.IsSet can't distinguish a
	// SetDefault from a real override, so explicit-set is detected from the
	// value source directly (env var or config-file key present).
	pools := poolKnobs{
		maxOpen:      maxOpenConns,
		maxIdle:      maxIdleConns,
		idleExplicit: explicitlySet(v, envCHMaxIdleConns),
	}
	surface, err := surfaceFromEnv(v, queryTimeout, pools)
	if err != nil {
		return Config{}, err
	}
	chCfg := assembleCHConfig(chConfigInputs{
		database:        getString(v, envCHDatabase),
		username:        getString(v, envCHUsername),
		password:        v.GetString(envCHPassword),
		dial:            dial,
		maxOpen:         maxOpenConns,
		maxIdle:         maxIdleConns,
		connMaxLifetime: connMaxLifetime,
		keepAlive: keepAliveInputs{
			enabled:  keepAliveEnabled,
			idle:     keepAliveIdle,
			interval: keepAliveInterval,
			probes:   keepAliveCount,
		},
		maxSamples:   maxSamples,
		maxMemory:    maxMemory,
		queryTimeout: queryTimeout,
		breaker:      breaker,
		extra:        surface.ch,
	})
	return Config{
		HTTPAddr:             getString(v, envHTTPAddr),
		HTTPServer:           surface.httpServer,
		LokiTailWriteTimeout: surface.lokiTailWriteTimeout,
		ClickHouse:           chCfg,
		Schema:               schema.DefaultOTelMetricsFromEnv(),
		Logs:                 schema.DefaultOTelLogsFromEnv(),
		Traces:               schema.DefaultOTelTracesFromEnv(),
		AutoCreateSchema:     flags.AutoCreate,
		AutoCreateDatabase:   flags.AutoCreateDatabase,
		SchemaProvisioning:   schemaProvisioning,
		RequirementsCheck:    flags.RequirementsCheck,
		DebugPProf:           flags.DebugPProf,
		LogCommentShape:      flags.LogCommentShape,
		CHOptimizations:      flags.CHOptimizations,
		CHOptimizationsMode:  flags.CHOptimizationsMode,
		LegacyTSGridFlag:     flags.TSGrid,
		CHOptCorpus:          flags.CHOptCorpus,
		Log:                  logCfg,
		OTLP:                 otlp,
		Admit:                admit,
		EnabledHeads:         enabledHeads,
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
	envCHKeepAliveEnabled,
	envCHKeepAliveIdle,
	envCHKeepAliveInterval,
	envCHKeepAliveCount,
	envQueryMaxSamples,
	envQueryTimeout,
	envCHQueryMaxMemory,
	envCHBreakerEnabled,
	envCHBreakerThreshold,
	envCHBreakerWindow,
	envCHBreakerOpenIntrvl,
	envCHProtocol,
	envCHConnOpenStrategy,
	envCHReadTimeout,
	envCHCompression,
	envCHCompressionLevel,
	envCHBlockBufferSize,
	envCHMaxComprBuffer,
	envCHFreeBufOnRelease,
	envCHDebug,
	envCHTLSEnabled,
	envCHTLSCAFile,
	envCHTLSCertFile,
	envCHTLSKeyFile,
	envCHTLSServerName,
	envCHTLSSkipVerify,
	envCHHTTPHeaders,
	envCHHTTPURLPath,
	envCHHTTPMaxConns,
	envCHHTTPProxyURL,
	envHTTPReadTimeout,
	envHTTPReadHdrTimeout,
	envHTTPWriteTimeout,
	envHTTPIdleTimeout,
	envHTTPMaxHeaderBytes,
	envHTTPMaxBodyBytes,
	envLokiTailWriteTO,
	envDebugPProf,
	envAutoCreateSchema,
	envAutoCreateDatabase,
	envSchemaCluster,
	envSchemaTableEngine,
	envSchemaTTL,
	envSchemaTTLMetrics,
	envSchemaTTLLogs,
	envSchemaTTLTraces,
	envSchemaDBReplicated,
	envSchemaDBReplPath,
	envSchemaDBReplShard,
	envSchemaDBReplReplica,
	envSchemaStoragePolicy,
	envSchemaSettings,
	envRequirementsCheck,
	envExperimentalTSGrid,
	envLogCommentShape,
	envCHOptimizations,
	envCHOptimizationsMode,
	envCHOptCorpusEnabled,
	envCHOptCorpusInterval,
	envCHOptCorpusSinkPath,
	envCHOptCorpusRing,
	envCHOptCorpusSinkMode,
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
	envEnabledHeads,
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
	v.SetDefault(envCHKeepAliveEnabled, defaultCHKeepAliveEnabled)
	v.SetDefault(envCHKeepAliveIdle, defaultCHKeepAliveIdle.String())
	v.SetDefault(envCHKeepAliveInterval, defaultCHKeepAliveInterval.String())
	v.SetDefault(envCHKeepAliveCount, defaultCHKeepAliveCount)
	v.SetDefault(envQueryMaxSamples, defaultQueryMaxSamples)
	v.SetDefault(envQueryTimeout, defaultQueryTimeout.String())
	v.SetDefault(envCHQueryMaxMemory, defaultCHQueryMaxMemory)
	v.SetDefault(envCHBreakerEnabled, defaultCHBreakerEnabled)
	v.SetDefault(envCHBreakerThreshold, defaultCHBreakerThreshold)
	v.SetDefault(envCHBreakerWindow, defaultCHBreakerWindow.String())
	v.SetDefault(envCHBreakerOpenIntrvl, defaultCHBreakerOpenInterval.String())
	v.SetDefault(envCHProtocol, defaultCHProtocol)
	v.SetDefault(envCHConnOpenStrategy, defaultCHConnOpenStrategy)
	v.SetDefault(envCHReadTimeout, defaultCHReadTimeout)
	v.SetDefault(envCHCompression, defaultCHCompression)
	v.SetDefault(envCHCompressionLevel, defaultCHCompressionLevel)
	v.SetDefault(envCHBlockBufferSize, defaultCHBlockBufferSize)
	v.SetDefault(envCHMaxComprBuffer, defaultCHMaxCompressionBuffer)
	v.SetDefault(envCHFreeBufOnRelease, defaultCHFreeBufOnConnRelease)
	v.SetDefault(envCHDebug, defaultCHDebug)
	v.SetDefault(envCHTLSEnabled, defaultCHTLSEnabled)
	v.SetDefault(envCHTLSCAFile, "")
	v.SetDefault(envCHTLSCertFile, "")
	v.SetDefault(envCHTLSKeyFile, "")
	v.SetDefault(envCHTLSServerName, "")
	v.SetDefault(envCHTLSSkipVerify, defaultCHTLSSkipVerify)
	v.SetDefault(envCHHTTPHeaders, "")
	v.SetDefault(envCHHTTPURLPath, "")
	v.SetDefault(envCHHTTPMaxConns, defaultCHHTTPMaxConns)
	v.SetDefault(envCHHTTPProxyURL, "")
	v.SetDefault(envHTTPReadTimeout, defaultHTTPReadTimeout.String())
	v.SetDefault(envHTTPReadHdrTimeout, defaultHTTPReadHeaderTimeout.String())
	v.SetDefault(envHTTPWriteTimeout, defaultHTTPWriteTimeout.String())
	v.SetDefault(envHTTPIdleTimeout, defaultHTTPIdleTimeout.String())
	v.SetDefault(envHTTPMaxHeaderBytes, defaultHTTPMaxHeaderBytes)
	v.SetDefault(envHTTPMaxBodyBytes, defaultHTTPMaxBodyBytes)
	v.SetDefault(envLokiTailWriteTO, defaultLokiTailWriteTimeout.String())
	// pprof is OFF by default — the profiling surface is opt-in only.
	v.SetDefault(envDebugPProf, false)
	v.SetDefault(envAutoCreateSchema, defaultAutoCreateSchema)
	// Schema-provisioning bool + duration knobs need a non-empty default so
	// the getBool / getDuration parsers don't reject an unset value. The
	// string knobs (cluster, table engine, database replicated
	// path/shard/replica) resolve "" via getString and need none —
	// internal/schema/ddl supplies the {shard}/{replica} macro fallbacks and
	// the bare ReplicatedMergeTree engine when the database is Replicated.
	v.SetDefault(envSchemaDBReplicated, defaultSchemaDBReplicated)
	v.SetDefault(envSchemaTTL, defaultSchemaTTL)
	v.SetDefault(envSchemaTTLMetrics, defaultSchemaTTL)
	v.SetDefault(envSchemaTTLLogs, defaultSchemaTTL)
	v.SetDefault(envSchemaTTLTraces, defaultSchemaTTL)
	v.SetDefault(envRequirementsCheck, defaultRequirementsCheck)
	v.SetDefault(envExperimentalTSGrid, defaultExperimentalTSGrid)
	v.SetDefault(envLogCommentShape, defaultLogCommentShape)
	setCHOptDefaults(v)
	v.SetDefault(envLogFormat, defaultLogFormat)
	v.SetDefault(envLogLevel, defaultLogLevel)
	v.SetDefault(envOTLPEndpoint, defaultOTLPEndpoint)
	v.SetDefault(envOTLPInsecure, defaultOTLPInsecure)
	v.SetDefault(envOTLPHeaders, defaultOTLPHeaders)
	v.SetDefault(envOTLPTimeout, defaultOTLPTimeout.String())
	v.SetDefault(envOTLPExportInterval, defaultOTLPExportInterval.String())
	v.SetDefault(envAdmitDisabled, defaultAdmitDisabled)
	v.SetDefault(envAdmitProm, DefaultAdmitProm)
	v.SetDefault(envAdmitLoki, DefaultAdmitLoki)
	v.SetDefault(envAdmitTempo, DefaultAdmitTempo)
	v.SetDefault(envEnabledHeads, defaultEnabledHeads)

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

// setCHOptDefaults seeds the CERBERUS_CH_OPTIMIZATIONS* and
// CERBERUS_CH_OPT_CORPUS_* defaults. Extracted from newLoader so the
// auto-picker + corpus knobs do not inflate newLoader's statement count; the
// defaults are the exact values documented in docs/clickhouse-optimizations.md.
func setCHOptDefaults(v *viper.Viper) {
	v.SetDefault(envCHOptimizations, defaultCHOptimizations)
	v.SetDefault(envCHOptimizationsMode, defaultCHOptimizationsMode)
	v.SetDefault(envCHOptCorpusEnabled, defaultCHOptCorpusEnabled)
	v.SetDefault(envCHOptCorpusInterval, defaultCHOptCorpusInterval.String())
	v.SetDefault(envCHOptCorpusSinkPath, defaultCHOptCorpusSinkPath)
	v.SetDefault(envCHOptCorpusRing, defaultCHOptCorpusRing)
	v.SetDefault(envCHOptCorpusSinkMode, defaultCHOptCorpusSinkMode)
}

// Built-in defaults, kept as named constants so newLoader's SetDefault
// calls and the doc comment can't drift. String/bool defaults that have
// no other natural home live here; the int / duration budget defaults
// keep their original homes below (they carry longer rationale comments).
const (
	defaultHTTPAddr           = ":8080"
	defaultCHAddr             = "localhost:9000"
	defaultCHDatabase         = "default"
	defaultCHUsername         = "default"
	defaultCHPassword         = ""
	defaultAutoCreateSchema   = false
	defaultSchemaDBReplicated = false
	defaultSchemaTTL          = "0s"
	defaultRequirementsCheck  = true
	defaultExperimentalTSGrid = false
	defaultLogCommentShape    = false
	// defaultCHOptimizations is "auto": enable every stable feature the
	// connected server supports, never an experimental one. This preserves the
	// historical experimental-off-by-default posture while turning on the
	// 24.8-safe stable wins (aggregation_in_order) and any newer stable feature
	// (condition_cache on >= 25.3) automatically.
	defaultCHOptimizations = "auto"
	// defaultCHOptimizationsMode is "enforcing": an explicitly-requested but
	// unsupported feature ABORTS startup (FATAL). `auto`/`off` already cover the
	// graceful paths, so naming an explicit feature list means "I require these".
	// Set CERBERUS_CH_OPTIMIZATIONS_MODE=permissive to WARN-and-skip instead.
	defaultCHOptimizationsMode = "enforcing"
	// defaultCHOptCorpusEnabled is false: the query_log performance-corpus
	// reconciler is opt-in (it needs system.query_log access and is
	// production-only; chDB has no query_log).
	defaultCHOptCorpusEnabled = false
	// defaultCHOptCorpusSinkPath is empty: no JSONL sink unless an operator
	// supplies a path.
	defaultCHOptCorpusSinkPath = ""
	// defaultCHOptCorpusSinkMode is the JSONL file sink; "chtable" selects the
	// cerberus_router_corpus MergeTree instead.
	defaultCHOptCorpusSinkMode = "jsonl"
	// defaultCHOptCorpusRing is the reconciler ring capacity when the operator
	// does not override it. It mirrors optcorpus's own internal default; the
	// reconciler clamps a non-positive value to the same floor.
	defaultCHOptCorpusRing    = 4096
	defaultLogFormat          = "text"
	defaultLogLevel           = "info"
	defaultOTLPEndpoint       = ""
	defaultOTLPInsecure       = false
	defaultOTLPHeaders        = ""
	defaultCHBreakerEnabled   = true
	defaultCHKeepAliveEnabled = true
	defaultAdmitDisabled      = false
	// The per-head ADMIT_{PROM,LOKI,TEMPO} defaults are the numeric caps
	// DefaultAdmitProm / Loki / Tempo (registered directly as the viper
	// defaults): admission control is ON out of the box at each head's
	// conservative cap. An explicit env value (an integer, or true/false)
	// overrides; see getAdmitCap.
)

const (
	defaultCHDialTimeout      time.Duration = 5 * time.Second
	defaultOTLPTimeout        time.Duration = 10 * time.Second
	defaultOTLPExportInterval time.Duration = 10 * time.Second
)

// ClickHouse protocol / strategy / compression enum vocabularies. The
// defaults reproduce today's behaviour exactly: native protocol, in-order
// host selection, and no wire compression — so an operator who sets none of
// these knobs gets the same connection cerberus has always opened.
const (
	chProtocolNative = "native"
	chProtocolHTTP   = "http"

	chConnOpenInOrder    = "in_order"
	chConnOpenRoundRobin = "round_robin"

	chCompressionNone = "none"
	chCompressionLZ4  = "lz4"
	chCompressionZSTD = "zstd"

	defaultCHProtocol         = chProtocolNative
	defaultCHConnOpenStrategy = chConnOpenInOrder
	defaultCHCompression      = chCompressionNone
	// defaultCHReadTimeout is empty so a socket ReadTimeout is DERIVED from
	// CERBERUS_QUERY_TIMEOUT (the deterministic restart-recovery ceiling, see
	// chclient.buildOptions). A non-empty CERBERUS_CH_READ_TIMEOUT overrides
	// the derivation. Empty (not "0s") keeps the value "unset" so the
	// derivation path stays in effect rather than forcing ReadTimeout to 0.
	defaultCHReadTimeout = ""
	// defaultCHCompressionLevel is 0: "no explicit level". Sending a level
	// while CERBERUS_CH_COMPRESSION=none is rejected (see the dependency
	// matrix); 0 means the driver's per-method default level applies.
	defaultCHCompressionLevel = 0
	// defaultCHBlockBufferSize / defaultCHMaxCompressionBuffer / the HTTP
	// per-host cap are all 0 = "leave the driver default" (2 / 10 MiB / Go's
	// default), so an unset knob is byte-identical to before it existed.
	defaultCHBlockBufferSize      = 0
	defaultCHMaxCompressionBuffer = 0
	defaultCHHTTPMaxConns         = 0
	defaultCHFreeBufOnConnRelease = false
	defaultCHDebug                = false
	defaultCHTLSEnabled           = false
	defaultCHTLSSkipVerify        = false
)

// Compression level bounds. clickhouse-go consumes Compression.Level
// differently per method (lz4hc honours it; the SpeedDefault-pinned zstd
// writer ignores it), but cerberus still validates the level against the
// method's documented range so a typo fails fast at startup instead of
// silently doing nothing. lz4: 0..12 (0 = driver default = LZ4HC level 9;
// 12 = compress.LevelLZ4HCMax). zstd: 1..22, the conventional ZSTD range
// that ClickHouse's server-side network_zstd_compression_level accepts.
const (
	chCompressionLZ4MinLevel  = 0
	chCompressionLZ4MaxLevel  = 12
	chCompressionZSTDMinLevel = 1
	chCompressionZSTDMaxLevel = 22
)

// chBlockBufferMax is the inclusive upper bound on CERBERUS_CH_BLOCK_BUFFER_SIZE
// — the driver field is a uint8, so 255 is the hard ceiling. The lower bound
// is 1 (0 means "unset / driver default 2", handled before this check).
const chBlockBufferMax = 255

// HTTP server timeout defaults (cmd/cerberus's http.Server). The header
// timeout is the promoted 5s that was previously hardcoded; read / write
// default to 0 (unlimited) so streaming responses (Loki /tail WebSocket,
// long query_range matrices) are never cut mid-stream; idle bounds an
// otherwise-leaked keep-alive connection. MaxHeaderBytes 0 = Go's 1 MiB
// default.
const (
	defaultHTTPReadTimeout       time.Duration = 0
	defaultHTTPReadHeaderTimeout time.Duration = 5 * time.Second
	defaultHTTPWriteTimeout      time.Duration = 0
	defaultHTTPIdleTimeout       time.Duration = 120 * time.Second
	defaultHTTPMaxHeaderBytes                  = 0
)

// defaultHTTPMaxBodyBytes caps an inbound HTTP request body (4 MiB). It bounds
// ParseForm/FormValue reads on the Prom/Loki/Tempo HTTP paths so an
// unauthenticated client cannot stream an unbounded body into process memory.
// The gRPC path is unaffected (it has its own framing). 0 disables the cap.
const defaultHTTPMaxBodyBytes int64 = 4 << 20

// defaultLokiTailWriteTimeout promotes the previously-hardcoded
// tailWriteTimeout in internal/api/loki/tail.go: the bound on a single
// /tail WebSocket write before a slow / dead client is torn down.
const defaultLokiTailWriteTimeout time.Duration = 10 * time.Second

// defaultCHOptCorpusInterval is how often the query_log performance-corpus
// reconciler reconciles recently-dispatched query_ids against
// system.query_log. 60s keeps the query_log read rate-limited (one batch per
// interval) while staying fresh enough to capture a recent shape's cost.
const defaultCHOptCorpusInterval time.Duration = 60 * time.Second

// defaultQueryMaxSamples is the default-on per-query sample budget. A
// query whose result-set drain crosses the budget is aborted (all
// three heads map it to HTTP 422 — Prom: errorType=execution with
// Prometheus's exact --query.max-samples wire message) instead of
// materialising an unbounded matrix in process memory. This is the
// backstop for the matrixFromCursor OOM class: prod hit it with the
// budget effectively unset (the old default was Prometheus's
// 50,000,000, which on cerberus's heavier label-carrying rows is no
// real cap on a ~2Gi pod). 5,000,000 is the top of the
// empirically-safe range for a 2Gi heap — it is what the k3d e2e stack
// runs, and is high enough that realistic Grafana query_range fan-outs
// over OTel data stay well under it — while bounding the blast radius
// of a runaway drain. Cerberus DELIBERATELY departs from upstream
// Prometheus's 50M default here: parity on the rejection *contract*
// (the 422 + exact message) is what matters; parity on a default sized
// for 16-byte in-memory samples does not. Set CERBERUS_QUERY_MAX_SAMPLES
// to raise it for big deployments, or 0 to disable the budget entirely.
const defaultQueryMaxSamples int64 = 5_000_000

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
// ConnMaxLifetime departs from the driver's 1h default to 30s: it is the
// age-eviction backstop that bounds recovery after a ClickHouse restart even
// if TCP keepalive (see below) is disabled. CH native conns are stateless and
// cheap to redial, so recycling the idle pool every 30s is negligible churn.
const (
	defaultCHMaxOpenConns                  = 10
	defaultCHMaxIdleConns                  = 5
	defaultCHConnMaxLifetime time.Duration = 30 * time.Second
)

// TCP keepalive defaults — the ROOT-CAUSE fix for slow breaker recovery after
// a ClickHouse restart. clickhouse-go v2.46.0 exposes NO idle-health knob, and
// a force-killed pod's socket can stay ESTABLISHED (no FIN/RST), so the
// driver's per-acquire socket check passes the dead conn through as healthy.
// The next query then blocks on a read until the driver's ReadTimeout (300s)
// or the request's ctx budget — surfacing as context.DeadlineExceeded, which
// is NOT a broken-conn error, so withTransportRetry neither fast-retries nor
// evicts it; the stale conn lingers and re-trips every breaker probe until
// something AGES it out. With keepalive armed, the kernel probes the idle
// socket and, after defaultCHKeepAliveCount unanswered probes, declares the
// peer dead; the blocked read returns ECONNRESET fast, which isBrokenConnError
// classifies → withTransportRetry evicts + redials. Idle(10s) +
// Interval(5s)*Count(3) ≈ 25s worst-case dead-peer detection — well under the
// chaos BREAKER_CLOSE_DEADLINE_MS=300s and under 60s, deterministically and on
// EVERY replica. Probes fire only on IDLE sockets, so a long streaming query
// is never interrupted. ConnMaxLifetime remains the age-eviction backstop if
// keepalive is turned off.
const (
	defaultCHKeepAliveIdle     time.Duration = 10 * time.Second
	defaultCHKeepAliveInterval time.Duration = 5 * time.Second
	defaultCHKeepAliveCount                  = 3
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

// DefaultAdmitProm, DefaultAdmitLoki and DefaultAdmitTempo are the
// per-head concurrency caps applied when CERBERUS_ADMIT_{PROM,LOKI,TEMPO}
// is unset or set to "true"; an explicit integer in that env overrides
// the default. cmd/cerberus reads the resolved cap to size each limiter.
// Tempo gets a smaller cap because trace queries (search + tag-value
// scans + per-trace span fetches) are heavier than Prom/Loki metric
// queries.
const (
	DefaultAdmitProm  = 64
	DefaultAdmitLoki  = 64
	DefaultAdmitTempo = 32
)

// admitFromEnv reads CERBERUS_ADMIT_* knobs from the viper loader.
// CERBERUS_ADMIT_DISABLED is a plain bool (getBool). The per-head
// CERBERUS_ADMIT_{PROM,LOKI,TEMPO} are concurrency caps read through
// getAdmitCap, which accepts an explicit non-negative integer OR a
// boolean spelling: "true" -> the head's default cap, "false"/"0" ->
// unlimited, a positive N -> cap N, negative/garbage -> rejected. This
// keeps the 1/0/true/false ergonomics the Helm chart relies on while
// still letting an operator pin an exact cap (e.g. 2). Unset values fall
// back to the registered per-head defaults (DefaultAdmitProm / Loki /
// Tempo).
func admitFromEnv(v *viper.Viper) (AdmitConfig, error) {
	disabled, err := getBool(v, envAdmitDisabled)
	if err != nil {
		return AdmitConfig{}, err
	}
	prom, err := getAdmitCap(v, envAdmitProm, DefaultAdmitProm)
	if err != nil {
		return AdmitConfig{}, err
	}
	loki, err := getAdmitCap(v, envAdmitLoki, DefaultAdmitLoki)
	if err != nil {
		return AdmitConfig{}, err
	}
	tempo, err := getAdmitCap(v, envAdmitTempo, DefaultAdmitTempo)
	if err != nil {
		return AdmitConfig{}, err
	}
	return AdmitConfig{
		Disabled: disabled,
		Prom:     prom,
		Loki:     loki,
		Tempo:    tempo,
	}, nil
}

// getAdmitCap resolves a per-head admission cap env var. It accepts both
// an explicit non-negative integer cap and the boolean spellings, so the
// four canonical values 1/0/true/false all behave intuitively AND an
// exact cap like 2 is honoured:
//
//	"true" / "t"   -> defaultCap (enable at the head's default)
//	"false" / "f"  -> 0 (unlimited; no limiter for this head)
//	a non-negative integer N (incl. 0 and 1) -> cap N (0 == unlimited)
//	negative or otherwise unparseable        -> error (fail fast)
//
// The boolean spellings are case-insensitive. A failing value is rejected
// with an error naming the env var, preserving the fail-fast contract a
// misconfiguration must trip at startup rather than silently mis-size.
func getAdmitCap(v *viper.Viper, key string, defaultCap int) (int, error) {
	raw := getString(v, key)
	if raw == "" {
		return 0, fmt.Errorf("%s: missing value", key)
	}
	trimmed := strings.TrimSpace(raw)
	switch strings.ToLower(trimmed) {
	case "true", "t":
		return defaultCap, nil
	case "false", "f":
		return 0, nil
	}
	n, err := strconv.Atoi(trimmed)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid admission cap %q: want a non-negative integer or true/false", key, raw)
	}
	if n < 0 {
		return 0, fmt.Errorf("%s: invalid admission cap %q: must be >= 0", key, raw)
	}
	return n, nil
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
	return slog.New(telemetry.NewSlogHandler(local, cfg.Level, lp))
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

// explicitlySet reports whether key was supplied by the operator — via the
// environment OR the optional config file — as opposed to resolving from a
// built-in SetDefault. viper.IsSet conflates a registered default with a real
// override, so it can't answer this; we inspect the two operator-controlled
// sources directly. A present-but-empty env value counts as NOT set (it is the
// same "unset" the rest of the loader treats it as via getString's trim).
func explicitlySet(v *viper.Viper, key string) bool {
	if raw, ok := os.LookupEnv(key); ok && strings.TrimSpace(raw) != "" {
		return true
	}
	return v.InConfig(key)
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

// getPositiveInt resolves key as an int and additionally rejects a
// non-positive value with an error naming the env var. It folds the repeated
// "parse then require > 0" pattern (the connection-pool size knobs) into one
// fail-fast helper.
func getPositiveInt(v *viper.Viper, key string) (int, error) {
	n, err := getInt(v, key)
	if err != nil {
		return 0, err
	}
	if n <= 0 {
		return 0, fmt.Errorf("%s: must be > 0, got %d", key, n)
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

// parseBool is the single shared boolean parser for every CERBERUS_*
// boolean config knob. It is backed by strconv.ParseBool, so it accepts
// the full vocabulary "1"/"0", "t"/"f"/"T"/"F", "true"/"false"/"TRUE"/
// "FALSE" (case-insensitive) interchangeably. Surrounding whitespace is
// trimmed first so a pasted newline / space parses the same as the bare
// token. A value outside the accepted vocabulary is rejected.
//
// Routing every boolean field through this one function guarantees a
// uniform convention across the whole config surface: AUTO_CREATE_*,
// OTLP_INSECURE, REQUIREMENTS_CHECK, SCHEMA_DATABASE_REPLICATED and the
// ADMIT_* toggles all parse identically. This is what lets the Helm
// chart render a plain YAML bool (`true`) into CERBERUS_ADMIT_PROM
// without the binary crash-looping on `strconv.Atoi("true")`.
func parseBool(s string) (bool, error) {
	return strconv.ParseBool(strings.TrimSpace(s))
}

// getBool resolves key and parses it through the shared parseBool helper
// (the standard strconv.ParseBool vocabulary: "1"/"0", "t"/"f",
// "true"/"false", case-insensitive). A value that fails to parse is
// rejected with an error naming the env var — preserving the historical
// fail-fast-on-misconfiguration contract.
func getBool(v *viper.Viper, key string) (bool, error) {
	raw := getString(v, key)
	if raw == "" {
		return false, fmt.Errorf("%s: missing value", key)
	}
	b, err := parseBool(raw)
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

// getRetentionDuration resolves key as a retention duration using the
// Prometheus/Grafana duration syntax operators already type for retention
// windows — `90d`, `2w`, `1y`, as well as the `2160h` Go form. It is a
// superset of getDuration (time.ParseDuration stops at hours), so existing
// hour-based values keep working. Units d/w/y are fixed (d=24h, w=7d,
// y=365d); calendar months/years are intentionally unsupported — they can't
// round-trip through a time.Duration (see docs/configuration.md). Note the
// Prometheus convention that compound values list units in descending order
// (`1w2d` is valid, `2d1w` is not).
func getRetentionDuration(v *viper.Viper, key string) (time.Duration, error) {
	raw := getString(v, key)
	if raw == "" {
		return 0, fmt.Errorf("%s: missing value", key)
	}
	d, err := model.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return time.Duration(d), nil
}

// bootFlags groups the boolean boot-time toggles FromEnv resolves: the
// auto-create-schema hook, the auto-create-database sub-toggle, the
// requirements preflight, and the experimental native-rate path. Grouping
// them keeps the per-flag parse + fail-fast error checks out of FromEnv's
// body.
type bootFlags struct {
	AutoCreate         bool
	AutoCreateDatabase bool
	RequirementsCheck  bool
	// TSGrid is the tri-state legacy CERBERUS_EXPERIMENTAL_TS_GRID_RANGE flag
	// (unset vs explicit true/false). It is no longer a plain bool: the alias
	// is re-routed through the chopt resolver in cmd/cerberus, which needs to
	// distinguish "unset" (no effect) from an explicit value (force
	// enable/disable ts_grid_range). Config.ExperimentalTSGridRange is then
	// back-filled from the resolved EnabledSet, not from this flag directly.
	TSGrid          chopt.LegacyFlag
	DebugPProf      bool
	LogCommentShape bool
	// CHOptimizations / CHOptimizationsMode / CHOptCorpus carry the
	// CERBERUS_CH_OPTIMIZATIONS* + CERBERUS_CH_OPT_CORPUS_* knobs. They live on
	// bootFlags (rather than a separate parse in FromEnv) so all the boot-time
	// toggle parsing — each fail-fast on a malformed value — stays in one
	// place. The raw optimizations string is resolved against the probed server
	// version in cmd/cerberus, not here.
	CHOptimizations     string
	CHOptimizationsMode chopt.Mode
	CHOptCorpus         CHOptCorpusConfig
}

// bootFlagsFromEnv parses the boolean boot toggles, failing fast on a
// malformed value exactly as the inline parses did. AutoCreateDatabase
// defaults to AutoCreate's value when CERBERUS_AUTO_CREATE_DATABASE is not
// explicitly set: enabling schema auto-create also creates the database, but
// an operator whose database is managed externally (e.g. a Replicated
// database provisioned by their cluster tooling) can set it to false to
// create only the tables.
func bootFlagsFromEnv(v *viper.Viper) (bootFlags, error) {
	autoCreate, err := getBool(v, envAutoCreateSchema)
	if err != nil {
		return bootFlags{}, err
	}
	autoCreateDatabase := autoCreate
	if explicitlySet(v, envAutoCreateDatabase) {
		autoCreateDatabase, err = getBool(v, envAutoCreateDatabase)
		if err != nil {
			return bootFlags{}, err
		}
	}
	requirementsCheck, err := getBool(v, envRequirementsCheck)
	if err != nil {
		return bootFlags{}, err
	}
	// Legacy ts-grid alias as a tri-state: Set distinguishes an explicit value
	// (force enable/disable ts_grid_range through the resolver) from unset (no
	// effect). The bool still parses through the same getBool vocabulary, so an
	// explicit malformed value still fails fast. When unset, getBool resolves
	// the seeded default (false), but Set=false makes the value irrelevant.
	tsGridSet := explicitlySet(v, envExperimentalTSGrid)
	tsGridValue, err := getBool(v, envExperimentalTSGrid)
	if err != nil {
		return bootFlags{}, err
	}
	debugPProf, err := getBool(v, envDebugPProf)
	if err != nil {
		return bootFlags{}, err
	}
	logCommentShape, err := getBool(v, envLogCommentShape)
	if err != nil {
		return bootFlags{}, err
	}
	chOpt, err := chOptFromEnv(v)
	if err != nil {
		return bootFlags{}, err
	}
	return bootFlags{
		AutoCreate:          autoCreate,
		AutoCreateDatabase:  autoCreateDatabase,
		RequirementsCheck:   requirementsCheck,
		TSGrid:              chopt.LegacyFlag{Set: tsGridSet, Value: tsGridValue},
		DebugPProf:          debugPProf,
		LogCommentShape:     logCommentShape,
		CHOptimizations:     chOpt.Optimizations,
		CHOptimizationsMode: chOpt.Mode,
		CHOptCorpus:         chOpt.Corpus,
	}, nil
}

// chOptParsed groups the CERBERUS_CH_OPTIMIZATIONS* parse results so FromEnv
// resolves them in a single call (keeping FromEnv's statement count down): the
// raw selection string, the parsed enforcing/permissive mode, and the corpus
// reconciler config.
type chOptParsed struct {
	Optimizations string
	Mode          chopt.Mode
	Corpus        CHOptCorpusConfig
}

// chOptFromEnv parses the CERBERUS_CH_OPTIMIZATIONS, CERBERUS_CH_OPTIMIZATIONS_
// MODE, and CERBERUS_CH_OPT_CORPUS_* knobs. The mode fails fast on an invalid
// value; the raw optimizations string is carried verbatim (it is resolved
// against the probed server version in cmd/cerberus, not here). Extracted from
// FromEnv so the parses live in one place.
func chOptFromEnv(v *viper.Viper) (chOptParsed, error) {
	mode, err := chopt.ParseMode(getString(v, envCHOptimizationsMode))
	if err != nil {
		return chOptParsed{}, fmt.Errorf("%s: %w", envCHOptimizationsMode, err)
	}
	corpus, err := chOptCorpusFromEnv(v)
	if err != nil {
		return chOptParsed{}, err
	}
	return chOptParsed{
		Optimizations: getString(v, envCHOptimizations),
		Mode:          mode,
		Corpus:        corpus,
	}, nil
}

// chOptCorpusFromEnv parses the CERBERUS_CH_OPT_CORPUS_* knobs into a
// CHOptCorpusConfig. The enable bool, interval duration, and ring capacity each
// fail fast on a malformed value; the sink path resolves via getString (empty is
// valid and simply disables the file sink). Extracted from FromEnv so the parses
// live in one place.
func chOptCorpusFromEnv(v *viper.Viper) (CHOptCorpusConfig, error) {
	enabled, err := getBool(v, envCHOptCorpusEnabled)
	if err != nil {
		return CHOptCorpusConfig{}, err
	}
	interval, err := getDuration(v, envCHOptCorpusInterval)
	if err != nil {
		return CHOptCorpusConfig{}, err
	}
	ring, err := getInt(v, envCHOptCorpusRing)
	if err != nil {
		return CHOptCorpusConfig{}, err
	}
	return CHOptCorpusConfig{
		Enabled:      enabled,
		Interval:     interval,
		SinkPath:     getString(v, envCHOptCorpusSinkPath),
		RingCapacity: ring,
		SinkMode:     getString(v, envCHOptCorpusSinkMode),
	}, nil
}

// schemaProvisioningFromEnv parses the CERBERUS_SCHEMA_* auto-create knobs
// into a SchemaProvisioning. Extracted from FromEnv so the boolean +
// duration parses (each fail-fast on a malformed value) live in one place
// rather than inflating FromEnv's branch count. The string knobs resolve via
// getString (empty is valid); internal/schema/ddl supplies the
// {shard}/{replica} macro fallbacks when the database is Replicated.
func schemaProvisioningFromEnv(v *viper.Viper) (SchemaProvisioning, error) {
	replicated, err := getBool(v, envSchemaDBReplicated)
	if err != nil {
		return SchemaProvisioning{}, err
	}
	ttl, err := getRetentionDuration(v, envSchemaTTL)
	if err != nil {
		return SchemaProvisioning{}, err
	}
	ttlMetrics, err := getRetentionDuration(v, envSchemaTTLMetrics)
	if err != nil {
		return SchemaProvisioning{}, err
	}
	ttlLogs, err := getRetentionDuration(v, envSchemaTTLLogs)
	if err != nil {
		return SchemaProvisioning{}, err
	}
	ttlTraces, err := getRetentionDuration(v, envSchemaTTLTraces)
	if err != nil {
		return SchemaProvisioning{}, err
	}
	settings, err := getKVList(v, envSchemaSettings)
	if err != nil {
		return SchemaProvisioning{}, err
	}
	return SchemaProvisioning{
		Cluster:                   getString(v, envSchemaCluster),
		TableEngine:               getString(v, envSchemaTableEngine),
		TTL:                       ttl,
		TTLMetrics:                ttlMetrics,
		TTLLogs:                   ttlLogs,
		TTLTraces:                 ttlTraces,
		DatabaseReplicated:        replicated,
		DatabaseReplicatedPath:    getString(v, envSchemaDBReplPath),
		DatabaseReplicatedShard:   getString(v, envSchemaDBReplShard),
		DatabaseReplicatedReplica: getString(v, envSchemaDBReplReplica),
		StoragePolicy:             getString(v, envSchemaStoragePolicy),
		Settings:                  settings,
	}, nil
}

// getKVList resolves a `k=v,k2=v2` env var (via viper) into the ordered
// schema.KV slice the schema-provisioning escape hatch consumes. Unset / empty
// returns nil; a token with no `=` is a fail-fast error (a silent drop would
// hide a misconfigured setting). The parse + type inference lives in
// schema.ParseKVList so the env-shape discipline sits next to the other schema
// env helpers.
func getKVList(v *viper.Viper, key string) ([]schema.KV, error) {
	kvs, err := schema.ParseKVList(getString(v, key))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", key, err)
	}
	return kvs, nil
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
