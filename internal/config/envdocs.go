package config

import (
	"fmt"
	"os"
	"time"
)

// EnvDoc is one row of the generated configuration reference. Key is the
// literal CERBERUS_* environment-variable name (identical to the viper key);
// Type and Desc are the hand-authored, code-reviewed documentation prose;
// Group names the section the key is rendered under. The Default column is
// NOT stored here on purpose - it is read LIVE from the viper loader by
// DocDefault so the documented default can never disagree with the runtime
// one. Key/Type/Desc/Group are reviewed once in code; that is what keeps the
// generated docs/configuration.md readable while its structure and defaults
// stay generated.
type EnvDoc struct {
	// Key is the CERBERUS_* env-var name (and viper key - they are the same
	// string by design).
	Key string
	// Type is the human-facing value type shown in the docs table
	// (string, bool, int, int64, duration, enum, int | bool, ...). It is the
	// documentation type, not the Go type, so it can carry the same nuance the
	// hand-written table did (e.g. an admit cap that accepts an int or a bool).
	Type string
	// Group is the section heading the key renders under. Keys sharing a Group
	// are emitted as one markdown table, in envDocs order.
	Group string
	// Desc is the one-cell description. It is plain markdown (inline code, bold)
	// and must stay single-line so the generated table row is well-formed.
	Desc string
}

// envDocGroup is a section of the generated reference: a Group name (matched
// against EnvDoc.Group) plus an optional Intro paragraph rendered above the
// table. The order of envDocGroups is the order sections appear in the
// generated document. Every EnvDoc.Group MUST appear here exactly once, and
// every group here MUST own at least one EnvDoc - TestEnvDocsCoverAllKeys and
// the generator both enforce that, so a new key with an unknown group fails
// before the doc gate.
type envDocGroup struct {
	Name  string
	Intro string
}

// envDocGroups defines the section order and per-section prose for the
// generated configuration reference. The Intro text is migrated verbatim from
// the previously hand-written docs/configuration.md so nothing readable is
// lost; the per-key tables below it are generated.
var envDocGroups = []envDocGroup{
	{
		Name: "HTTP server",
		Intro: "A single listener serves all three upstream APIs plus the health probes; there\n" +
			"is no separate admin port. See [`operations.md`](operations.md#port-binding)\n" +
			"for the port-binding contract (including h2c + gRPC on the same socket).\n\n" +
			"The timeout knobs map 1:1 to `http.Server` fields. `ReadTimeout` and\n" +
			"`WriteTimeout` default to `0` (unlimited) deliberately: the Loki `/tail`\n" +
			"WebSocket and long `query_range` matrix responses stream for an unbounded\n" +
			"duration and a non-zero server-side write deadline would sever them\n" +
			"mid-response. `ReadHeaderTimeout` (the promoted 5s) still bounds slow-header\n" +
			"attacks; `IdleTimeout` reclaims idle keep-alive connections.",
	},
	{
		Name: "ClickHouse connection",
		Intro: "ClickHouse is the only mandatory backing service, reached exclusively through\n" +
			"these connection inputs. `CERBERUS_CH_ADDR` accepts a comma-separated list of\n" +
			"hosts for a replicated / sharded cluster; with more than one host the driver\n" +
			"selects per `CERBERUS_CH_CONN_OPEN_STRATEGY`. The protocol defaults to the\n" +
			"native binary protocol (port 9000); set `CERBERUS_CH_PROTOCOL=http` for the\n" +
			"HTTP protocol (port 8123) when only 8123 is reachable. Every knob below is\n" +
			"unset-by-default to the exact connection cerberus has always opened - setting\n" +
			"none of them is byte-identical to the pre-knob behaviour.",
	},
	{
		Name: "ClickHouse TLS / mTLS",
		Intro: "Set `CERBERUS_CH_TLS_ENABLED=true` to dial ClickHouse over TLS. The TLS\n" +
			"sub-knobs are inert (and **rejected at startup**) unless TLS is enabled - a\n" +
			"silently-ignored TLS config is a security footgun. For mutual TLS supply both\n" +
			"`_TLS_CERT_FILE` and `_TLS_KEY_FILE` (a lone one is rejected). `_TLS_CA_FILE`\n" +
			"pins a custom CA bundle; `_TLS_SERVER_NAME` overrides the verified hostname\n" +
			"(SNI). `_TLS_INSECURE_SKIP_VERIFY=true` disables certificate verification\n" +
			"entirely and is **rejected in combination with** `_TLS_CA_FILE` or\n" +
			"`_TLS_SERVER_NAME` (skip-verify ignores both - the combo is incoherent).",
	},
	{
		Name: "ClickHouse HTTP-protocol knobs",
		Intro: "Consulted only under `CERBERUS_CH_PROTOCOL=http`; setting any of them under\n" +
			"`native` is **rejected at startup** (they would be silently ignored).",
	},
	{
		Name: "Connection pool",
		Intro: "The connection-count defaults reproduce clickhouse-go/v2's previously-implicit\n" +
			"pool sizing verbatim, made explicit so the sharded-pushdown solver can raise the\n" +
			"ceiling for fan-out rather than inherit a hidden driver default. The\n" +
			"connection-lifetime default departs deliberately (see the row below) so a stale\n" +
			"conn to a restarted ClickHouse backend is recycled fast. When the pool is\n" +
			"exhausted an acquire blocks up to `CERBERUS_CH_DIAL_TIMEOUT` and then fails with\n" +
			"a breaker-neutral acquire-timeout (a local pool-sizing signal, not a\n" +
			"ClickHouse-health failure).\n\n" +
			"TCP keepalive (`CERBERUS_CH_KEEPALIVE_*`, on by default) is the primary\n" +
			"recovery mechanism after a ClickHouse restart: the kernel probes idle sockets\n" +
			"and tears down a connection to a force-killed pod within roughly\n" +
			"`IDLE + INTERVAL x COUNT` (~25s at the defaults), so the next query fails fast\n" +
			"with a broken-conn error that is retried and evicted instead of blocking on a\n" +
			"half-open socket. Probes fire only on idle connections, so long streaming\n" +
			"queries are never interrupted. `CERBERUS_CH_CONN_MAX_LIFETIME` is the\n" +
			"age-eviction backstop if keepalive is disabled.",
	},
	{
		Name:  "Query limits and memory",
		Intro: "Per-query wall-clock, memory, and sample budgets. A query crossing any cap gets a breaker-neutral typed rejection rather than holding an admit slot and pooled connection unbounded.",
	},
	{
		Name: "Circuit breaker",
		Intro: "Every ClickHouse-touching call is guarded by a per-`Client` circuit breaker.\n" +
			"The defaults reproduce the pre-tunable hardcoded values exactly, so\n" +
			"out-of-the-box behaviour is unchanged. Pool-acquire timeouts,\n" +
			"`MEMORY_LIMIT_EXCEEDED` rejections, and client-cancelled requests are treated\n" +
			"as breaker-neutral and never advance the failure count. Set\n" +
			"`CERBERUS_CH_BREAKER_ENABLED=false` to disable the breaker entirely. See\n" +
			"[`operations.md`](operations.md#clickhouse-circuit-breaker) for the full state\n" +
			"machine.",
	},
	{
		Name: "Admission control",
		Intro: "Each of the three API heads can be fronted by a counted semaphore that caps\n" +
			"simultaneous in-flight requests. Requests above the cap are rejected with HTTP\n" +
			"503 + `Retry-After: 1` so well-behaved clients back off and ClickHouse stays out\n" +
			"of overload. Tempo's cap is half of Prom / Loki because trace queries are\n" +
			"typically the heaviest per-call.\n\n" +
			"`CERBERUS_ADMIT_{PROM,LOKI,TEMPO}` each accept **either** a non-negative\n" +
			"integer cap **or** a boolean alias: `true` enables the head at its conservative\n" +
			"default cap, `false` (or `0`) leaves that head unlimited, and a positive\n" +
			"integer pins an exact cap. A negative or unparseable value is rejected at\n" +
			"startup. `CERBERUS_ADMIT_DISABLED` is a separate master switch that turns every\n" +
			"head off at once.",
	},
	{
		Name:  "Logging",
		Intro: "Cerberus's own structured logging (stdlib `log/slog`). The same records that print to stderr also bridge to OTLP when self-telemetry is enabled (see below).",
	},
	{
		Name: "Self-telemetry (OTLP export)",
		Intro: "The OpenTelemetry exporter configuration. When `CERBERUS_OTLP_ENDPOINT` is empty\n" +
			"cerberus installs no-op trace, meter, and logger providers and runs as a\n" +
			"zero-collector-dependency binary. Standard `OTEL_EXPORTER_OTLP_*` env vars are\n" +
			"also honored by the OTel Go SDK and merge with these. See\n" +
			"[`observability.md`](observability.md) for the full self-observability contract.",
	},
	{
		Name: "Schema provisioning",
		Intro: "The auto-create hook (off by default) runs the idempotent OTel-CH exporter DDL\n" +
			"at startup before HTTP serving begins. Every knob below shapes that DDL and is\n" +
			"a no-op unless `CERBERUS_AUTO_CREATE_SCHEMA=true`. `CERBERUS_REQUIREMENTS_CHECK`\n" +
			"gates the boot-time version + schema-shape preflight that runs after the\n" +
			"auto-create step. The schema-shape table-name overrides\n" +
			"(`CERBERUS_SCHEMA_*_TABLE`) and the Prometheus resource-label allowlist\n" +
			"(`CERBERUS_PROM_RESOURCE_LABELS`) are resolved by `internal/schema` rather than\n" +
			"this loader and are documented in\n" +
			"[`observability.md`](observability.md#schema-shape-overrides).",
	},
	{
		Name: "ClickHouse optimizations",
		Intro: "The ClickHouse-optimization suite: an auto-picker that enables the stable,\n" +
			"server-supported optimizations for the connected ClickHouse version, an\n" +
			"enforcing/permissive policy for explicitly-requested features, a per-query\n" +
			"instrumentation layer, and an async performance-corpus reconciler. The\n" +
			"canonical spec lives in\n" +
			"[`clickhouse-optimizations.md`](clickhouse-optimizations.md); this section is\n" +
			"the env-var reference. Individual optimizations (e.g. `aggregation_in_order`,\n" +
			"`condition_cache`, `columnar_result_decode`) have no standalone env var - they\n" +
			"are reached only through the `CERBERUS_CH_OPTIMIZATIONS` list.",
	},
	{
		Name: "Experimental flags",
		Intro: "`CERBERUS_EXPERIMENTAL_TS_GRID_RANGE` is soft-deprecated in favour of\n" +
			"`CERBERUS_CH_OPTIMIZATIONS` (list `ts_grid_range`). It is re-routed through the\n" +
			"optimization resolver and remains honoured for backward compatibility:\n" +
			"explicit `true` force-enables `ts_grid_range` (subject to version + mode),\n" +
			"explicit `false` force-disables it, unset has no effect. It **requires\n" +
			"ClickHouse >= 25.6**. See\n" +
			"[`clickhouse-optimizations.md`](clickhouse-optimizations.md#legacy-alias-cerberus_experimental_ts_grid_range).",
	},
	{
		Name:  "Loki streaming",
		Intro: "",
	},
}

// envDocs is the hand-authored metadata for every CERBERUS_* key the viper
// loader resolves. There is exactly one entry per allEnvKeys member (enforced
// by TestEnvDocsCoverAllKeys). The Type/Group/Desc are migrated from the
// previously hand-written docs/configuration.md so the generated document
// reads as well as the hand-maintained one did; the Default column and the
// table structure are generated, so the documented default can never drift
// from the runtime default.
var envDocs = []EnvDoc{
	// --- HTTP server ---
	{envHTTPAddr, "string", "HTTP server", "HTTP listen address for the Prom / Loki / Tempo APIs and the `/healthz` / `/readyz` probes."},
	{envHTTPReadTimeout, "duration", "HTTP server", "Whole-request read deadline (headers + body). `0` = unlimited (streaming-safe)."},
	{envHTTPReadHdrTimeout, "duration", "HTTP server", "Request-header read deadline. Must be `<=` `CERBERUS_HTTP_READ_TIMEOUT` when that is `> 0`."},
	{envHTTPWriteTimeout, "duration", "HTTP server", "Response write deadline. `0` = unlimited - required so `/tail` + long matrices stream uninterrupted."},
	{envHTTPIdleTimeout, "duration", "HTTP server", "Idle keep-alive connection lifetime."},
	{envHTTPMaxHeaderBytes, "size", "HTTP server", "Max request header size in bytes. Accepts a raw byte integer (e.g. `1048576`) **or** a humanized size (`1Mi`, `512Ki`, `1M`); the raw-integer form is unchanged for backward compatibility. `0` leaves Go's 1 MiB default."},
	{envHTTPMaxBodyBytes, "size", "HTTP server", "Max inbound HTTP request body size, applied via `http.MaxBytesReader` on the Prom / Loki / Tempo HTTP paths (the gRPC path is unaffected). Accepts a raw byte integer **or** a humanized size (`4Mi`, `1M`). Default `4Mi`; `0` disables the cap."},
	{envDebugPProf, "bool", "HTTP server", "Mount the `net/http/pprof` debug endpoints (`/debug/pprof/*`) on the HTTP listener. **Off by default** - opt-in only, so the profiling surface never ships open in production."},
	{envEnabledHeads, "string", "HTTP server", "Comma-separated subset of query heads this process serves: `prom`, `loki`, `tempo` (case-insensitive). Default (all three) preserves full backward compatibility. A subset skips building **and** mounting the disabled heads' handler/client/limiter (and the Tempo gRPC service) so one head can run isolated in its own process/cgroup; `/healthz` + `/readyz` stay served in every mode. An unknown head or an empty list fails startup."},

	// --- ClickHouse connection ---
	{envCHAddr, "string", "ClickHouse connection", "ClickHouse endpoint(s). Comma-separated for multiple hosts (each trimmed; at least one required)."},
	{envCHDatabase, "string", "ClickHouse connection", "ClickHouse database name. Matches the upstream OTel ClickHouse exporter default; `AUTO_CREATE_SCHEMA` creates it (idempotently) if absent."},
	{envCHUsername, "string", "ClickHouse connection", "ClickHouse user."},
	{envCHPassword, "string", "ClickHouse connection", "ClickHouse password. Source from a secret, never commit."},
	{envCHDialTimeout, "duration", "ClickHouse connection", "ClickHouse dial timeout (`time.ParseDuration` syntax)."},
	{envCHProtocol, "enum", "ClickHouse connection", "Wire protocol: `native` (port 9000) or `http` (port 8123). The HTTP-only knobs below require `http`."},
	{envCHConnOpenStrategy, "enum", "ClickHouse connection", "Multi-host selection: `in_order` (try hosts in order) or `round_robin` (rotate). Pointless but benign with a single host."},
	{envCHReadTimeout, "duration", "ClickHouse connection", "Socket read ceiling. Unset derives it from `CERBERUS_QUERY_TIMEOUT`. When set must be `>=` `CERBERUS_QUERY_TIMEOUT`. clickhouse-go has no write-timeout knob."},
	{envCHCompression, "enum", "ClickHouse connection", "Wire compression: `none`, `lz4`, or `zstd`."},
	{envCHCompressionLevel, "int", "ClickHouse connection", "Compression level. `0` = method default. Requires a method. lz4: `0..12`; zstd: `1..22`."},
	{envCHBlockBufferSize, "int", "ClickHouse connection", "Per-connection block buffer count (`0` -> driver default 2; valid `1..255`)."},
	{envCHMaxComprBuffer, "size", "ClickHouse connection", "Compression buffer cap in bytes. Accepts a raw byte integer **or** a humanized size (`16Mi`, `10M`); the raw-integer form is unchanged for backward compatibility. `0` -> driver default 10 MiB; otherwise `> 0`."},
	{envCHFreeBufOnRelease, "bool", "ClickHouse connection", "Drop the preserved memory buffer after each query (lower steady-state memory, less buffer reuse)."},
	{envCHDebug, "bool", "ClickHouse connection", "clickhouse-go legacy stdout debug logging. Noisy; local diagnosis only."},

	// --- ClickHouse TLS / mTLS ---
	{envCHTLSEnabled, "bool", "ClickHouse TLS / mTLS", "Dial ClickHouse over TLS. Required for any other TLS sub-knob."},
	{envCHTLSCAFile, "string", "ClickHouse TLS / mTLS", "PEM CA bundle path. A set-but-unreadable path fails fast."},
	{envCHTLSCertFile, "string", "ClickHouse TLS / mTLS", "Client certificate (mTLS). Must be set together with the key file."},
	{envCHTLSKeyFile, "string", "ClickHouse TLS / mTLS", "Client private key (mTLS). Must be set together with the cert file."},
	{envCHTLSServerName, "string", "ClickHouse TLS / mTLS", "Verified server hostname / SNI override."},
	{envCHTLSSkipVerify, "bool", "ClickHouse TLS / mTLS", "Skip certificate verification. Incompatible with CA / server-name knobs."},

	// --- ClickHouse HTTP-protocol knobs ---
	{envCHHTTPHeaders, "string", "ClickHouse HTTP-protocol knobs", "Extra HTTP request headers, `k=v,k2=v2` (e.g. multi-tenant IDs)."},
	{envCHHTTPURLPath, "string", "ClickHouse HTTP-protocol knobs", "Extra URL path prefix for HTTP requests."},
	{envCHHTTPMaxConns, "int", "ClickHouse HTTP-protocol knobs", "`http.Transport` per-host connection cap (`0` -> driver default)."},
	{envCHHTTPProxyURL, "string", "ClickHouse HTTP-protocol knobs", "HTTP proxy URL (absolute, with scheme + host)."},

	// --- Connection pool ---
	{envCHMaxOpenConns, "int", "Connection pool", "Total pooled ClickHouse connections (busy + idle). Must be > 0."},
	{envCHMaxIdleConns, "int", "Connection pool", "Idle ClickHouse connections kept warm for reuse. Must be > 0."},
	{envCHConnMaxLifetime, "duration", "Connection pool", "Max age of a pooled connection before it is recycled. Age-eviction backstop for a stale conn to a restarted backend (keepalive is the primary mechanism). Must be > 0."},
	{envCHKeepAliveEnabled, "bool", "Connection pool", "Enable TCP keepalive on ClickHouse connection sockets so the kernel detects a dead peer after a restart."},
	{envCHKeepAliveIdle, "duration", "Connection pool", "Idle time before the first keepalive probe. Must be > 0 when keepalive is enabled."},
	{envCHKeepAliveInterval, "duration", "Connection pool", "Gap between successive keepalive probes. Must be > 0 when keepalive is enabled."},
	{envCHKeepAliveCount, "int", "Connection pool", "Unanswered keepalive probes before the socket is declared dead. Must be > 0 when keepalive is enabled."},

	// --- Query limits and memory ---
	{envCHQueryMaxMemory, "size", "Query limits and memory", "Per-query ClickHouse memory cap (`max_memory_usage` on every data-plane query; DDL exempt). Accepts a raw byte integer (e.g. `1073741824`) **or** a humanized size (`2Gi`, `512Mi`, `1G`); the raw-integer form is unchanged for backward compatibility. 1 GiB default. `0` leaves it unset. A query over the cap gets a breaker-neutral resource-exhausted rejection (Prom 422 / Loki 400 / Tempo 422)."},
	{envQueryMaxSamples, "int64", "Query limits and memory", "Per-query sample budget, mirroring Prometheus `--query.max-samples`. Bounds cerberus-process memory by aborting a result-set drain that crosses the budget. `0` disables."},
	{envQueryTimeout, "duration", "Query limits and memory", "Per-query wall-clock cap, stamped as ClickHouse `max_execution_time` (with `timeout_overflow_mode=throw`) on every data-plane query; DDL exempt. Mirrors Prometheus `--query.timeout`. The `?timeout=` query param min's against this per request. Also derives the driver-level socket `ReadTimeout`. `0` disables both."},

	// --- Circuit breaker ---
	{envCHBreakerEnabled, "bool", "Circuit breaker", "Master switch. `false` makes the breaker a no-op (always-allow, never trips); a dead CH then surfaces as ordinary errors."},
	{envCHBreakerThreshold, "int", "Circuit breaker", "Consecutive CH-health failures within the window that trip the breaker CLOSED -> OPEN. Must be >= 1."},
	{envCHBreakerWindow, "duration", "Circuit breaker", "Rolling window over which the threshold failures must occur. Must be > 0."},
	{envCHBreakerOpenIntrvl, "duration", "Circuit breaker", "OPEN-state backoff before the breaker admits a single HALF-OPEN probe. Must be > 0."},

	// --- Admission control ---
	{envAdmitDisabled, "bool", "Admission control", "Disable admission control entirely on every head (handy for local development)."},
	{envAdmitProm, "int | bool", "Admission control", "Prom API in-flight cap. Integer caps the head; `true` = default cap 64; `false`/`0` = unlimited."},
	{envAdmitLoki, "int | bool", "Admission control", "Loki API in-flight cap. Integer caps the head; `true` = default cap 64; `false`/`0` = unlimited."},
	{envAdmitTempo, "int | bool", "Admission control", "Tempo API in-flight cap. Integer caps the head; `true` = default cap 32; `false`/`0` = unlimited."},

	// --- Logging ---
	{envLogFormat, "string", "Logging", "slog handler kind: `text` (human-readable) or `json` (aggregators)."},
	{envLogLevel, "string", "Logging", "Minimum slog level: `debug`, `info`, `warn`, or `error`."},

	// --- Self-telemetry (OTLP export) ---
	{envOTLPEndpoint, "string", "Self-telemetry (OTLP export)", "gRPC OTLP target for self-telemetry (e.g. `otel-collector.observability.svc:4317`). Empty disables the exporters."},
	{envOTLPInsecure, "bool", "Self-telemetry (OTLP export)", "Dial the OTLP endpoint without TLS (handy for local dev / k3d)."},
	{envOTLPHeaders, "string", "Self-telemetry (OTLP export)", "Comma-separated `key=value` gRPC metadata sent on every OTLP request (typically auth bearer tokens)."},
	{envOTLPTimeout, "duration", "Self-telemetry (OTLP export)", "Per-request OTLP roundtrip timeout (applies to both the trace and metric exporters)."},
	{envOTLPExportInterval, "duration", "Self-telemetry (OTLP export)", "Metric `PeriodicReader` flush interval. The quickstart default is tuned for time-to-first-panel; deployments at scale should raise it (e.g. `60s`) to cut collector load."},

	// --- Schema provisioning ---
	{envAutoCreateSchema, "bool", "Schema provisioning", "When `true`, run the idempotent OTel-CH exporter DDL at startup before HTTP serving begins. The knobs below shape that DDL - all are no-ops unless this is `true`."},
	{envAutoCreateDatabase, "bool", "Schema provisioning", "Whether the hook also creates the database (`CREATE DATABASE IF NOT EXISTS`) over a bootstrap connection to the always-present `default` db. Defaults to the value of `CERBERUS_AUTO_CREATE_SCHEMA`. Set `false` to create only the tables when the database is provisioned externally."},
	{envSchemaCluster, "string", "Schema provisioning", "Render an `ON CLUSTER <name>` clause into the auto-create DDL (classic distributed-DDL clusters). Mutually exclusive with `CERBERUS_SCHEMA_DATABASE_REPLICATED`."},
	{envSchemaTableEngine, "string", "Schema provisioning", "Override the table engine. Empty renders `MergeTree()` - or, when `CERBERUS_SCHEMA_DATABASE_REPLICATED=true`, the bare `ReplicatedMergeTree` (no args). Set this only to pin some other non-default engine."},
	{envSchemaTTL, "duration", "Schema provisioning", "Global default retention for every signal's tables (no TTL clause when `0`). Accepts the Prometheus/Grafana duration syntax (`90d`, `2w`, `1y`, or the Go `2160h` form). Per-signal overrides below take precedence."},
	{envSchemaTTLMetrics, "duration", "Schema provisioning", "Retention for the five metrics tables. A non-zero value overrides the global default for metrics; `0` inherits `CERBERUS_SCHEMA_TTL`."},
	{envSchemaTTLLogs, "duration", "Schema provisioning", "Retention for the logs table; `0` inherits `CERBERUS_SCHEMA_TTL`."},
	{envSchemaTTLTraces, "duration", "Schema provisioning", "Retention for the spans + `trace_id_ts` tables; `0` inherits `CERBERUS_SCHEMA_TTL`."},
	{envSchemaDBReplicated, "bool", "Schema provisioning", "Create the database with `ENGINE = Replicated(...)` so DDL auto-replicates across replicas (no `ON CLUSTER` needed). Emits a bare `ReplicatedMergeTree` table engine to replicate the data."},
	{envSchemaDBReplPath, "string", "Schema provisioning", "ZooKeeper/Keeper path the Replicated engine coordinates on (e.g. `/clickhouse/databases/otel`). **Required** when `CERBERUS_SCHEMA_DATABASE_REPLICATED=true`."},
	{envSchemaDBReplShard, "string", "Schema provisioning", "Shard name for the Replicated engine - defaults to the ClickHouse server `{shard}` macro."},
	{envSchemaDBReplReplica, "string", "Schema provisioning", "Replica name for the Replicated engine - defaults to the ClickHouse server `{replica}` macro."},
	{envSchemaStoragePolicy, "string", "Schema provisioning", "Typed shorthand for the MergeTree `storage_policy` setting on every auto-created table (the S3 / tiered-storage knob). Appended FIRST to the SETTINGS tail. Empty appends nothing. Mutually exclusive with a `storage_policy` key in `CERBERUS_SCHEMA_SETTINGS` (set it in exactly one)."},
	{envSchemaSettings, "string", "Schema provisioning", "Generic MergeTree-SETTINGS escape hatch: an ordered `k=v,k2=v2` list appended to every auto-created table's SETTINGS tail (e.g. `min_bytes_for_wide_part=0`). Numeric / boolean values render bare, others single-quoted. Empty appends nothing (byte-identical default DDL)."},
	{envRequirementsCheck, "bool", "Schema provisioning", "Run the boot-time requirements check (version + schema-shape gate) after the schema-create step. Fails startup on a fatal finding; an absent (not-yet-provisioned) schema instead boots NOT READY and re-probes."},

	// --- ClickHouse optimizations ---
	{envCHOptimizations, "string", "ClickHouse optimizations", "`auto` (enable every **stable** feature the probed server supports), `off` (enable nothing), or a comma-separated list of feature ids. `auto` may itself appear in the list to add an opt-in feature on top of the auto-selected set, e.g. `auto,columnar_result_decode`. `off` is absolute and cannot be combined."},
	{envCHOptimizationsMode, "string", "ClickHouse optimizations", "`enforcing` (an explicitly-requested but unsupported feature is a FATAL startup error) or `permissive` (it is skipped with a `WARN`). Ignored under `auto`/`off`."},
	{envLogCommentShape, "bool", "ClickHouse optimizations", "Stamp ClickHouse `log_comment` with a compact, literal-free cerberus shape id (`cerb:<root>[;mod...]`) so `system.query_log` rows cluster by `normalized_query_hash`."},
	{envCHOptCorpusEnabled, "bool", "ClickHouse optimizations", "Enable the async `system.query_log` performance-corpus reconciler (needs `system.query_log` access; production-only - chDB has none)."},
	{envCHOptCorpusInterval, "duration", "ClickHouse optimizations", "How often the reconciler joins recently-dispatched query_ids back to `system.query_log`."},
	{envCHOptCorpusSinkPath, "string", "ClickHouse optimizations", "JSONL sink path for the `(shape-id, opts, timings)` corpus. Empty disables the file sink."},
	{envCHOptCorpusRing, "int", "ClickHouse optimizations", "Ring capacity for tracked query_ids; caps memory + the per-interval `IN(...)`."},
	{envCHOptCorpusSinkMode, "string", "ClickHouse optimizations", "Corpus sink: `jsonl` (default, writes the sink-path file) or `chtable` (writes the `cerberus_router_corpus` MergeTree for the route A/B go/no-go analysis)."},

	// --- Experimental flags ---
	{envExperimentalTSGrid, "bool", "Experimental flags", "Soft-deprecated alias for `CERBERUS_CH_OPTIMIZATIONS=ts_grid_range`. Emit ClickHouse-native `timeSeriesRateToGrid` for eligible `rate(<counter>[range])` query_range instead of the default arrayJoin fan-out. Requires ClickHouse >= 25.6."},

	// --- Loki streaming ---
	{envLokiTailWriteTO, "duration", "Loki streaming", "Bound on a single `/loki/api/v1/tail` WebSocket write before a slow / dead client is torn down. `> 0`."},
}

// EnvDocs returns the documentation metadata for every CERBERUS_* key the
// viper loader resolves, in document order. The returned slice is a copy so
// callers (the cmd/config-docs generator, tests) cannot mutate the package
// state.
func EnvDocs() []EnvDoc {
	out := make([]EnvDoc, len(envDocs))
	copy(out, envDocs)
	return out
}

// EnvDocGroup mirrors envDocGroup for external (generator/test) consumers.
type EnvDocGroup struct {
	Name  string
	Intro string
}

// EnvDocGroups returns the ordered sections (name + intro prose) the generated
// document renders, as a copy.
func EnvDocGroups() []EnvDocGroup {
	out := make([]EnvDocGroup, len(envDocGroups))
	for i, g := range envDocGroups {
		out[i] = EnvDocGroup(g)
	}
	return out
}

// AllEnvKeys returns the literal CERBERUS_* keys the viper loader resolves, in
// loader order. It is the authoritative key set the documentation must cover
// 1:1; TestEnvDocsCoverAllKeys asserts envDocs matches it exactly.
func AllEnvKeys() []string {
	out := make([]string, len(allEnvKeys))
	copy(out, allEnvKeys)
	return out
}

// DocDefaults returns the rendered default value for every CERBERUS_* key,
// read LIVE from a freshly-built viper loader with no CERBERUS_* environment
// variable set. Reading the default from the same loader the running binary
// uses is what makes the generated docs/configuration.md unable to disagree
// with the runtime default. The returned strings are presentation-ready (the
// same `(empty)` / duration / inherits forms the hand-written doc used).
func DocDefaults() map[string]string {
	// Snapshot and clear every CERBERUS_* env var so an ambient value in the
	// generator's environment cannot leak into the documented default, then
	// restore the environment before returning. The loader reads BindEnv'd
	// values ahead of SetDefault, so an un-cleared CERBERUS_CH_ADDR would make
	// the doc claim that operator's host as "the default".
	saved := make(map[string]*string, len(allEnvKeys))
	for _, key := range allEnvKeys {
		if v, ok := os.LookupEnv(key); ok {
			val := v
			saved[key] = &val
		} else {
			saved[key] = nil
		}
		_ = os.Unsetenv(key)
	}
	defer func() {
		for key, v := range saved {
			if v == nil {
				_ = os.Unsetenv(key)
			} else {
				_ = os.Setenv(key, *v)
			}
		}
	}()

	v := newLoader()
	out := make(map[string]string, len(allEnvKeys))
	for _, key := range allEnvKeys {
		out[key] = renderDefault(key, v.Get(key))
	}
	return out
}

// renderDefault formats a raw viper default into the presentation form used in
// docs/configuration.md. It keeps the few special cases the hand-written doc
// carried (empty strings render as `(empty)`, the two derived/inherited knobs
// keep their explanatory placeholder) so the generated table reads the same.
func renderDefault(key string, raw any) string {
	switch key {
	case envCHReadTimeout:
		// Derived from CERBERUS_QUERY_TIMEOUT when unset; the loader stores "".
		return "(derived)"
	case envAutoCreateDatabase:
		// No SetDefault: resolves to CERBERUS_AUTO_CREATE_SCHEMA at boot.
		return "= `CERBERUS_AUTO_CREATE_SCHEMA`"
	case envSchemaTTLMetrics, envSchemaTTLLogs, envSchemaTTLTraces:
		return "(inherits `CERBERUS_SCHEMA_TTL`)"
	}

	switch t := raw.(type) {
	case nil:
		return "(empty)"
	case string:
		if t == "" {
			return "(empty)"
		}
		return fmt.Sprintf("`%s`", t)
	case bool:
		return fmt.Sprintf("`%t`", t)
	case time.Duration:
		return fmt.Sprintf("`%s`", t)
	default:
		return fmt.Sprintf("`%v`", t)
	}
}
