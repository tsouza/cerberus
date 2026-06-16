# Configuration

Cerberus is a stateless 12-factor binary configured primarily through
`CERBERUS_*` environment variables. An optional `cerberus.yaml`
([below](#configuration-file-optional)) may supply file-level defaults, but the
environment contract always wins — env vars are the source of truth.
ClickHouse and the (optional) OpenTelemetry collector are attached resources
reached through env-var connection inputs, so swapping a local single-node
ClickHouse for a managed cluster, or a sidecar collector for a SaaS ingest URL,
is a matter of flipping env vars and restarting. Every default below is the
value `internal/config/config.go` ships out of the box.

Misconfigured values fail fast: an unparseable duration, an out-of-range
integer, an unknown log level, or a malformed OTLP header list aborts startup
with a clear error rather than silently downgrading behaviour. Secrets (the
ClickHouse password, OTLP bearer tokens) live in this same env-var namespace
and are sourced from a Kubernetes `Secret`, a Docker `secrets:` mount, or a
vault-injecting init container — never committed.

For how these knobs interact with the running service — lifecycle, readiness,
deployment, scaling — see [`operations.md`](operations.md).

## Configuration file (optional)

Cerberus loads configuration through a [viper](https://github.com/spf13/viper)
loader, so an **optional** `cerberus.yaml` may supply values alongside the
environment. The resolution order is:

1. **Environment variable** (`CERBERUS_*`) — always wins.
2. **Config file** (`cerberus.yaml`) — fills in anything the environment leaves
   unset.
3. **Built-in default** — the value `internal/config/config.go` ships.

The loader probes two paths for `cerberus.yaml`, in order: the **working
directory** (`.`) and **`/etc/cerberus`**. The file is **purely additive** — it
can only supply a value the operator hasn't set in the environment; it can never
override an env var. That keeps the 12-factor contract intact (the deployment's
environment remains the source of truth) while giving a baked-image or
bare-metal deployment a place to pin defaults without a long `-e` list.

The file is **optional and best-effort**: a **missing** `cerberus.yaml` is not
an error, and a **malformed** one is tolerated at load time rather than crashing
startup — values still resolve from the environment and the built-in defaults.
Each resolved value, whatever its source, is then run through the **same
fail-fast typed validation** an env value gets: an unparseable duration or an
out-of-range integer supplied by the file aborts startup with the same clear
error it would from an env var. So a file *value* that is present but invalid
still fails fast; only an absent or structurally-broken file is silently
skipped.

The keys are the literal `CERBERUS_*` names (the loader binds each viper key to
its environment variable). A minimal example pinning the ClickHouse endpoint and
log format:

```yaml
# /etc/cerberus/cerberus.yaml — defaults; any CERBERUS_* env var overrides these
CERBERUS_CH_ADDR: clickhouse.observability.svc:9000
CERBERUS_CH_DATABASE: otel
CERBERUS_LOG_FORMAT: json
CERBERUS_ADMIT_TEMPO: 24
```

Secrets (the ClickHouse password, OTLP bearer tokens) are best left **out** of
the file and injected through the environment from a Kubernetes `Secret` or a
vault sidecar, exactly as without a config file — the file is for non-secret
defaults.

## HTTP server

A single listener serves all three upstream APIs plus the health probes; there
is no separate admin port. See [`operations.md`](operations.md#port-binding)
for the port-binding contract (including h2c + gRPC on the same socket).

The timeout knobs map 1:1 to `http.Server` fields. `ReadTimeout` and
`WriteTimeout` default to `0` (unlimited) deliberately: the Loki `/tail`
WebSocket and long `query_range` matrix responses stream for an unbounded
duration and a non-zero server-side write deadline would sever them
mid-response. `ReadHeaderTimeout` (the promoted 5s) still bounds slow-header
attacks; `IdleTimeout` reclaims idle keep-alive connections.

| Variable                            | Type     | Default | Description                                                                                                                                                                                                                                                                                                      |
| ----------------------------------- | -------- | ------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `CERBERUS_HTTP_ADDR`                | string   | `:8080` | HTTP listen address for the Prom / Loki / Tempo APIs and the `/healthz` / `/readyz` probes.                                                                                                                                                                                                                      |
| `CERBERUS_HTTP_READ_TIMEOUT`        | duration | `0s`    | Whole-request read deadline (headers + body). `0` = unlimited (streaming-safe).                                                                                                                                                                                                                                  |
| `CERBERUS_HTTP_READ_HEADER_TIMEOUT` | duration | `5s`    | Request-header read deadline. Must be `<=` `CERBERUS_HTTP_READ_TIMEOUT` when that is `> 0`.                                                                                                                                                                                                                      |
| `CERBERUS_HTTP_WRITE_TIMEOUT`       | duration | `0s`    | Response write deadline. `0` = unlimited — required so `/tail` + long matrices stream uninterrupted.                                                                                                                                                                                                             |
| `CERBERUS_HTTP_IDLE_TIMEOUT`        | duration | `120s`  | Idle keep-alive connection lifetime.                                                                                                                                                                                                                                                                             |
| `CERBERUS_HTTP_MAX_HEADER_BYTES`    | int      | `0`     | Max request header size. `0` leaves Go's 1 MiB default.                                                                                                                                                                                                                                                          |
| `CERBERUS_DEBUG_PPROF`              | bool     | `false` | Mount the `net/http/pprof` debug endpoints (`/debug/pprof/*`) on the HTTP listener. **Off by default** — opt-in only, so the profiling surface never ships open in production. Enable to capture a live heap / CPU profile from a running pod mid-incident (e.g. `wget http://localhost:8080/debug/pprof/heap`). |

## ClickHouse connection

ClickHouse is the only mandatory backing service, reached exclusively through
these connection inputs. `CERBERUS_CH_ADDR` accepts a comma-separated list of
hosts for a replicated / sharded cluster; with more than one host the driver
selects per `CERBERUS_CH_CONN_OPEN_STRATEGY`. The protocol defaults to the
native binary protocol (port 9000); set `CERBERUS_CH_PROTOCOL=http` for the
HTTP protocol (port 8123) when only 8123 is reachable. Every knob below is
unset-by-default to the exact connection cerberus has always opened — setting
none of them is byte-identical to the pre-knob behaviour.

| Variable                               | Type     | Default          | Description                                                                                                                                                  |
| -------------------------------------- | -------- | ---------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `CERBERUS_CH_ADDR`                     | string   | `localhost:9000` | ClickHouse endpoint(s). Comma-separated for multiple hosts (each trimmed; at least one required).                                                            |
| `CERBERUS_CH_DATABASE`                 | string   | `default`        | ClickHouse database name. Matches the upstream OTel ClickHouse exporter default; `AUTO_CREATE_SCHEMA` creates it (idempotently) if absent.                   |
| `CERBERUS_CH_USERNAME`                 | string   | `default`        | ClickHouse user.                                                                                                                                             |
| `CERBERUS_CH_PASSWORD`                 | string   | (empty)          | ClickHouse password.                                                                                                                                         |
| `CERBERUS_CH_DIAL_TIMEOUT`             | duration | `5s`             | ClickHouse dial timeout (`time.ParseDuration` syntax).                                                                                                       |
| `CERBERUS_CH_PROTOCOL`                 | enum     | `native`         | Wire protocol: `native` (port 9000) or `http` (port 8123). The HTTP-only knobs below require `http`.                                                         |
| `CERBERUS_CH_CONN_OPEN_STRATEGY`       | enum     | `in_order`       | Multi-host selection: `in_order` (try hosts in order) or `round_robin` (rotate). Pointless but benign with a single host.                                    |
| `CERBERUS_CH_READ_TIMEOUT`             | duration | (derived)        | Socket read ceiling. Unset → derived from `CERBERUS_QUERY_TIMEOUT`. When set must be `>=` `CERBERUS_QUERY_TIMEOUT`. clickhouse-go has no write-timeout knob. |
| `CERBERUS_CH_COMPRESSION`              | enum     | `none`           | Wire compression: `none`, `lz4`, or `zstd`.                                                                                                                  |
| `CERBERUS_CH_COMPRESSION_LEVEL`        | int      | `0`              | Compression level. `0` = method default. Requires a method. lz4: `0..12`; zstd: `1..22`.                                                                     |
| `CERBERUS_CH_BLOCK_BUFFER_SIZE`        | int      | `0`              | Per-connection block buffer count (`0` → driver default 2; valid `1..255`).                                                                                  |
| `CERBERUS_CH_MAX_COMPRESSION_BUFFER`   | int      | `0`              | Compression buffer cap in bytes (`0` → driver default 10 MiB; otherwise `> 0`).                                                                              |
| `CERBERUS_CH_FREE_BUF_ON_CONN_RELEASE` | bool     | `false`          | Drop the preserved memory buffer after each query (lower steady-state memory, less buffer reuse).                                                            |
| `CERBERUS_CH_DEBUG`                    | bool     | `false`          | clickhouse-go legacy stdout debug logging. Noisy; local diagnosis only.                                                                                      |

### TLS / mTLS

Set `CERBERUS_CH_TLS_ENABLED=true` to dial ClickHouse over TLS. The TLS
sub-knobs are inert (and **rejected at startup**) unless TLS is enabled — a
silently-ignored TLS config is a security footgun. For mutual TLS supply both
`_TLS_CERT_FILE` and `_TLS_KEY_FILE` (a lone one is rejected). `_TLS_CA_FILE`
pins a custom CA bundle; `_TLS_SERVER_NAME` overrides the verified hostname
(SNI). `_TLS_INSECURE_SKIP_VERIFY=true` disables certificate verification
entirely and is **rejected in combination with** `_TLS_CA_FILE` or
`_TLS_SERVER_NAME` (skip-verify ignores both — the combo is incoherent).

| Variable                               | Type   | Default | Description                                                              |
| -------------------------------------- | ------ | ------- | ------------------------------------------------------------------------ |
| `CERBERUS_CH_TLS_ENABLED`              | bool   | `false` | Dial ClickHouse over TLS. Required for any other TLS sub-knob.           |
| `CERBERUS_CH_TLS_CA_FILE`              | string | (empty) | PEM CA bundle path. A set-but-unreadable path fails fast.                |
| `CERBERUS_CH_TLS_CERT_FILE`            | string | (empty) | Client certificate (mTLS). Must be set together with the key file.       |
| `CERBERUS_CH_TLS_KEY_FILE`             | string | (empty) | Client private key (mTLS). Must be set together with the cert file.      |
| `CERBERUS_CH_TLS_SERVER_NAME`          | string | (empty) | Verified server hostname / SNI override.                                 |
| `CERBERUS_CH_TLS_INSECURE_SKIP_VERIFY` | bool   | `false` | Skip certificate verification. Incompatible with CA / server-name knobs. |

### HTTP-protocol knobs

Consulted only under `CERBERUS_CH_PROTOCOL=http`; setting any of them under
`native` is **rejected at startup** (they would be silently ignored).

| Variable                              | Type   | Default | Description                                                       |
| ------------------------------------- | ------ | ------- | ----------------------------------------------------------------- |
| `CERBERUS_CH_HTTP_HEADERS`            | string | (empty) | Extra HTTP request headers, `k=v,k2=v2` (e.g. multi-tenant IDs).  |
| `CERBERUS_CH_HTTP_URL_PATH`           | string | (empty) | Extra URL path prefix for HTTP requests.                          |
| `CERBERUS_CH_HTTP_MAX_CONNS_PER_HOST` | int    | `0`     | `http.Transport` per-host connection cap (`0` → driver default).  |
| `CERBERUS_CH_HTTP_PROXY_URL`          | string | (empty) | HTTP proxy URL (absolute, with scheme + host).                    |

## Connection pool

The connection-count defaults reproduce clickhouse-go/v2's previously-implicit
pool sizing verbatim, made explicit so the sharded-pushdown solver can raise the
ceiling for fan-out rather than inherit a hidden driver default. The
connection-lifetime default departs deliberately (see the row below) so a stale
conn to a restarted ClickHouse backend is recycled fast. When the pool is
exhausted an acquire blocks up to `CERBERUS_CH_DIAL_TIMEOUT` and then fails with
a breaker-neutral acquire-timeout (a local pool-sizing signal, not a
ClickHouse-health failure).

TCP keepalive (`CERBERUS_CH_KEEPALIVE_*`, on by default) is the primary
recovery mechanism after a ClickHouse restart: the kernel probes idle sockets
and tears down a connection to a force-killed pod within roughly
`IDLE + INTERVAL × COUNT` (≈25s at the defaults), so the next query fails fast
with a broken-conn error that is retried and evicted instead of blocking on a
half-open socket. Probes fire only on idle connections, so long streaming
queries are never interrupted. `CERBERUS_CH_CONN_MAX_LIFETIME` is the
age-eviction backstop if keepalive is disabled. The driver-level socket
`ReadTimeout` (derived from `CERBERUS_QUERY_TIMEOUT`; see the query-limits
table) is the hard ceiling: even if keepalive probes do not fire, a read on a
stale half-open socket cannot block past the per-query budget, so breaker
recovery is bounded by `CERBERUS_QUERY_TIMEOUT` rather than the driver's 300s
default.

| Variable                         | Type     | Default | Description                                                                                                                                                                                                                                                     |
| -------------------------------- | -------- | ------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `CERBERUS_CH_MAX_OPEN_CONNS`     | int      | `10`    | Total pooled ClickHouse connections (busy + idle). Must be > 0.                                                                                                                                                                                                 |
| `CERBERUS_CH_MAX_IDLE_CONNS`     | int      | `5`     | Idle ClickHouse connections kept warm for reuse. Must be > 0.                                                                                                                                                                                                   |
| `CERBERUS_CH_CONN_MAX_LIFETIME`  | duration | `30s`   | Max age of a pooled connection before it is recycled. Age-eviction backstop for a stale conn to a restarted backend (keepalive is the primary mechanism). Must be > 0.                                                                                          |
| `CERBERUS_CH_KEEPALIVE_ENABLED`  | bool     | `true`  | Enable TCP keepalive on ClickHouse connection sockets so the kernel detects a dead peer after a restart.                                                                                                                                                        |
| `CERBERUS_CH_KEEPALIVE_IDLE`     | duration | `10s`   | Idle time before the first keepalive probe. Must be > 0 when keepalive is enabled.                                                                                                                                                                              |
| `CERBERUS_CH_KEEPALIVE_INTERVAL` | duration | `5s`    | Gap between successive keepalive probes. Must be > 0 when keepalive is enabled.                                                                                                                                                                                 |
| `CERBERUS_CH_KEEPALIVE_COUNT`    | int      | `3`     | Unanswered keepalive probes before the socket is declared dead. Must be > 0 when keepalive is enabled.                                                                                                                                                          |

## Query limits and memory

| Variable                       | Type     | Default      | Description                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| ------------------------------ | -------- | ------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `CERBERUS_CH_QUERY_MAX_MEMORY` | int64    | `1073741824` | Per-query ClickHouse memory cap in bytes (`max_memory_usage` on every data-plane query; DDL exempt). 1 GiB default. `0` leaves it unset (CH server defaults apply). A query over the cap gets a breaker-neutral resource-exhausted rejection (Prom 422 / Loki 400 / Tempo 422).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| `CERBERUS_QUERY_MAX_SAMPLES`   | int64    | `50000000`   | Per-query sample budget, mirroring Prometheus `--query.max-samples`. Bounds cerberus-process memory by aborting a result-set drain that crosses the budget. `0` disables. Memory-constrained deploys should set this well below the default.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| `CERBERUS_QUERY_TIMEOUT`       | duration | `2m`         | Per-query wall-clock cap, stamped as ClickHouse `max_execution_time` (with `timeout_overflow_mode=throw`) on every data-plane query; DDL exempt. Mirrors Prometheus `--query.timeout`, so a pathological query gets a server-side deadline instead of holding an admit slot + pooled connection unbounded. The standard Prometheus `?timeout=<duration>` query param (and Loki's `timeout`) min's against this default per request (the smaller wins) and is threaded to both the request ctx deadline and `max_execution_time`. A query over the cap gets a breaker-neutral timeout rejection (Prom/Loki 503 `errorType=timeout`, Tempo 503). It also derives the driver-level socket `ReadTimeout` (clickhouse-go would otherwise default to 300s), so a stale half-open connection to a force-killed ClickHouse pod fails within ~`CERBERUS_QUERY_TIMEOUT` instead of blocking the breaker for 300s. `0` disables both (CH server defaults + the driver's 300s ReadTimeout apply). |

## Circuit breaker

Every ClickHouse-touching call is guarded by a per-`Client` circuit breaker
(`internal/chclient/breaker.go`). The defaults reproduce the pre-tunable
hardcoded values exactly, so out-of-the-box behaviour is unchanged. Pool-acquire
timeouts, `MEMORY_LIMIT_EXCEEDED` rejections, and client-cancelled requests are
treated as breaker-neutral and never advance the failure count. Set
`CERBERUS_CH_BREAKER_ENABLED=false` to disable the breaker entirely — useful
when an external proxy or service mesh already owns ClickHouse fail-fast. See
[`operations.md`](operations.md#clickhouse-circuit-breaker) for the full state
machine.

| Variable                            | Type     | Default | Description                                                                                                               |
| ----------------------------------- | -------- | ------- | ------------------------------------------------------------------------------------------------------------------------- |
| `CERBERUS_CH_BREAKER_ENABLED`       | bool     | `true`  | Master switch. `false` makes the breaker a no-op (always-allow, never trips); a dead CH then surfaces as ordinary errors. |
| `CERBERUS_CH_BREAKER_THRESHOLD`     | int      | `5`     | Consecutive CH-health failures within the window that trip the breaker CLOSED → OPEN. Must be >= 1.                       |
| `CERBERUS_CH_BREAKER_WINDOW`        | duration | `10s`   | Rolling window over which the threshold failures must occur. Must be > 0.                                                 |
| `CERBERUS_CH_BREAKER_OPEN_INTERVAL` | duration | `5s`    | OPEN-state backoff before the breaker admits a single HALF-OPEN probe. Must be > 0.                                       |

## Admission control

Each of the three API heads is fronted by a counted semaphore that caps
simultaneous in-flight requests. Requests above the cap are rejected with HTTP
503 + `Retry-After: 1` so well-behaved clients back off and ClickHouse stays out
of overload. Tempo's cap is half of Prom / Loki because trace queries are
typically the heaviest per-call. Setting an individual cap to `0` disables
admission control for that head specifically.

| Variable                  | Type | Default | Description                                                                  |
| ------------------------- | ---- | ------- | ---------------------------------------------------------------------------- |
| `CERBERUS_ADMIT_DISABLED` | bool | `false` | Disable per-handler concurrency caps entirely (handy for local development). |
| `CERBERUS_ADMIT_PROM`     | int  | `64`    | Max simultaneous in-flight Prom API requests. `0` disables for this head.    |
| `CERBERUS_ADMIT_LOKI`     | int  | `64`    | Max simultaneous in-flight Loki API requests. `0` disables for this head.    |
| `CERBERUS_ADMIT_TEMPO`    | int  | `32`    | Max simultaneous in-flight Tempo API requests. `0` disables for this head.   |

## Logging

Cerberus's own structured logging (stdlib `log/slog`). The same records that
print to stderr also bridge to OTLP when self-telemetry is enabled (see below).

| Variable              | Type   | Default | Description                                                         |
| --------------------- | ------ | ------- | ------------------------------------------------------------------- |
| `CERBERUS_LOG_FORMAT` | string | `text`  | slog handler kind: `text` (human-readable) or `json` (aggregators). |
| `CERBERUS_LOG_LEVEL`  | string | `info`  | Minimum slog level: `debug`, `info`, `warn`, or `error`.            |

## Self-telemetry (OTLP export)

The OpenTelemetry exporter configuration. When `CERBERUS_OTLP_ENDPOINT` is empty
cerberus installs no-op trace, meter, and logger providers and runs as a
zero-collector-dependency binary. Standard `OTEL_EXPORTER_OTLP_*` env vars are
also honored by the OTel Go SDK and merge with these. See
[`observability.md`](observability.md) for the full self-observability contract.

| Variable                        | Type     | Default | Description                                                                                                                                                               |
| ------------------------------- | -------- | ------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `CERBERUS_OTLP_ENDPOINT`        | string   | (empty) | gRPC OTLP target for self-telemetry (e.g. `otel-collector.observability.svc:4317`). Empty disables the exporters.                                                         |
| `CERBERUS_OTLP_INSECURE`        | bool     | `false` | Dial the OTLP endpoint without TLS (handy for local dev / k3d).                                                                                                           |
| `CERBERUS_OTLP_HEADERS`         | string   | (empty) | Comma-separated `key=value` gRPC metadata sent on every OTLP request (typically auth bearer tokens).                                                                      |
| `CERBERUS_OTLP_TIMEOUT`         | duration | `10s`   | Per-request OTLP roundtrip timeout (applies to both the trace and metric exporters).                                                                                      |
| `CERBERUS_OTLP_EXPORT_INTERVAL` | duration | `10s`   | Metric `PeriodicReader` flush interval. The quickstart default is tuned for time-to-first-panel; deployments at scale should raise it (e.g. `60s`) to cut collector load. |

## Schema

| Variable                                      | Type     | Default                             | Description                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| --------------------------------------------- | -------- | ----------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `CERBERUS_AUTO_CREATE_SCHEMA`                 | bool     | `false`                             | When `true`, run the idempotent OTel-CH exporter DDL at startup before HTTP serving begins. The knobs below shape that DDL — all are no-ops unless this is `true`.                                                                                                                                                                                                                                                                                                                                   |
| `CERBERUS_AUTO_CREATE_DATABASE`               | bool     | = `CERBERUS_AUTO_CREATE_SCHEMA`     | Whether the hook also creates the database (`CREATE DATABASE IF NOT EXISTS`), issued over a bootstrap connection to ClickHouse's always-present `default` db (the configured db may not exist yet, and ClickHouse rejects every statement on a session whose default db is absent). Set `false` to create only the tables when the database is provisioned externally (e.g. a Replicated database managed by cluster tooling).                                                                       |
| `CERBERUS_SCHEMA_CLUSTER`                     | string   | (empty)                             | Render an `ON CLUSTER <name>` clause into the auto-create DDL (classic distributed-DDL clusters). Mutually exclusive with `CERBERUS_SCHEMA_DATABASE_REPLICATED`.                                                                                                                                                                                                                                                                                                                                     |
| `CERBERUS_SCHEMA_TABLE_ENGINE`                | string   | `MergeTree()`                       | Override the table engine. Empty renders `MergeTree()` — or, when `CERBERUS_SCHEMA_DATABASE_REPLICATED=true`, the **bare `ReplicatedMergeTree`** (no args): a Replicated database does **not** auto-convert `MergeTree`, and inside one the engine's Keeper path/replica are supplied automatically (explicit args are rejected, code 36). Set this only to pin some other non-default engine — e.g. a classic `ON CLUSTER` cluster needing an explicit `ReplicatedMergeTree('/path', '{replica}')`. |
| `CERBERUS_SCHEMA_TTL`                         | duration | `0s`                                | Global default retention for every signal's tables (no TTL clause when `0`). Accepts the Prometheus/Grafana duration syntax — `90d`, `2w`, `1y`, or the Go `2160h` form (`d`/`w`/`y` are fixed: 24h/7d/365d; no calendar months; compound values list units descending, e.g. `1w2d`). Per-signal overrides below take precedence.                                                                                                                                                                    |
| `CERBERUS_SCHEMA_TTL_METRICS`                 | duration | (inherits `CERBERUS_SCHEMA_TTL`)    | Retention for the five metrics tables. A non-zero value overrides the global default for metrics.                                                                                                                                                                                                                                                                                                                                                                                                    |
| `CERBERUS_SCHEMA_TTL_LOGS`                    | duration | (inherits `CERBERUS_SCHEMA_TTL`)    | Retention for the logs table.                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| `CERBERUS_SCHEMA_TTL_TRACES`                  | duration | (inherits `CERBERUS_SCHEMA_TTL`)    | Retention for the spans + `trace_id_ts` tables.                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| `CERBERUS_SCHEMA_DATABASE_REPLICATED`         | bool     | `false`                             | Create the database with `ENGINE = Replicated(...)` so DDL auto-replicates across replicas (no `ON CLUSTER` needed). A Replicated database does **not** auto-convert `MergeTree` tables, so cerberus emits a **bare `ReplicatedMergeTree`** table engine (no args — the database supplies the Keeper coordinates) to replicate the DATA.                                                                                                                                                             |
| `CERBERUS_SCHEMA_DATABASE_REPLICATED_PATH`    | string   | (empty)                             | ZooKeeper/Keeper path the Replicated engine coordinates on (e.g. `/clickhouse/databases/otel`). **Required** when `CERBERUS_SCHEMA_DATABASE_REPLICATED=true`.                                                                                                                                                                                                                                                                                                                                        |
| `CERBERUS_SCHEMA_DATABASE_REPLICATED_SHARD`   | string   | `{shard}`                           | Shard name for the Replicated engine — defaults to the ClickHouse server macro.                                                                                                                                                                                                                                                                                                                                                                                                                      |
| `CERBERUS_SCHEMA_DATABASE_REPLICATED_REPLICA` | string   | `{replica}`                         | Replica name for the Replicated engine — defaults to the ClickHouse server macro.                                                                                                                                                                                                                                                                                                                                                                                                                    |
| `CERBERUS_REQUIREMENTS_CHECK`                 | bool     | `true`                              | Run the boot-time requirements check after the schema-create step. Fails startup on a fatal finding (too-old server, wrong-shape table); an absent (not-yet-provisioned) schema instead boots NOT READY and re-probes.                                                                                                                                                                                                                                                                               |

> The auto-create hook reuses the same table names the query heads read
> (the `CERBERUS_SCHEMA_*_TABLE` overrides), so a renamed table is created
> **and** queried consistently. Retention is keyed per **signal**, not per
> individual table — the five metrics tables share one TTL, etc. — because
> that matches how observability retention is set (logs short, metrics long).
> A deployment needing genuinely per-table retention runs the DDL itself.

The preflight runs two gates, both parameterised by the active
(override-resolved) configuration:

- **Version gate.** `SELECT version()` is compared against
  `max(base, applicable-feature-floors)`. The base floor is **ClickHouse
  24.8** — the minimum cerberus supports, with the 24.8 empty-input /
  parse-unit / filter-path emit workarounds shipped unconditionally so the
  SQL is correct on it. Enabling `CERBERUS_EXPERIMENTAL_TS_GRID_RANGE` adds the
  native-rate floor (CH 25.6); the effective requirement is the maximum, so
  the flag **raises** the floor from 24.8 to 25.6, and a future feature floor
  raises it further. An unreadable or unparseable version is a failure, never
  a silent pass.
- **Schema-shape gate.** The configured tables are introspected via
  `system.columns` and validated to carry the essential columns the emitters
  read, with the attribute-map columns (`Attributes` /
  `ResourceAttributes` / `ScopeAttributes`) typed `Map(String, String)`
  (a `Map(String, LowCardinality(String))` value type is accepted). Every
  table and column name comes from the override-resolved schema, so
  `CERBERUS_SCHEMA_*` renames are respected. The gate distinguishes two
  cases:
  - A table that **exists but is wrong-shape** (missing column / wrong
    attribute-map type) is a misconfiguration that never self-heals → it is a
    **fatal** finding (startup fails).
  - A table that is **entirely absent** (zero columns reported) is the
    not-yet-provisioned startup race → it is **not** fatal. Cerberus boots,
    reports NOT READY on `/readyz` with a precise reason (`schema not yet
    provisioned: table otel_logs absent`), and **re-probes** on the
    auto-create retry cadence; readiness flips green once an external writer
    (the otel-collector, or `CERBERUS_AUTO_CREATE_SCHEMA`) creates the schema
    — **no restart**. `/healthz` stays 200 throughout. An introspection
    *error* (as opposed to a clean zero-row absence) remains fatal.

The check runs after the auto-create step on purpose: on a fresh database
cerberus has just created the tables, so introspecting them before the
create would fail the schema gate against tables that don't exist yet. When
a **fatal** gate fails, the error **aggregates every** unmet requirement, e.g.:

```text
requirements check failed:
  - clickhouse version 24.3.1 is below the required minimum 24.8 (native rate disabled)
  - table otel_metrics_gauge column Attributes: expected Map(String,String), found JSON
  - table otel_traces: missing required column ServiceName
```

Schema-shape overrides — the table names cerberus reads when the ClickHouse
layout deviates from the OTel-CH exporter defaults (`CERBERUS_SCHEMA_METRICS_*`,
`CERBERUS_SCHEMA_LOGS_TABLE`, `CERBERUS_SCHEMA_TRACES_TABLE`) — are documented in
[`observability.md`](observability.md#schema-shape-overrides).

### Prometheus resource-attribute labels

The Prometheus head projects each metric row's OTel `ResourceAttributes`
map as Prometheus labels alongside the per-datapoint `Attributes` map, so
fleet metrics can be filtered and grouped by resource-level keys
(`k8s.namespace.name`, `deployment.environment.name`, `k8s.pod.name`, …).
Keys are sanitized dot→underscore for Prometheus legality
(`k8s.namespace.name` → `k8s_namespace_name`) on the wire, and a matcher
like `{k8s_namespace_name="prod"}` reverses the sanitized name through the
existing dot↔underscore candidate chain to filter the stored dotted key.

**Precedence:** on a key collision the per-datapoint `Attributes` value
wins over the `ResourceAttributes` value (the Prometheus convention that a
datapoint label overrides a resource label). The dedicated
`service.name`/`service_name` → `ServiceName`-column handling is preserved
and keeps its existing precedence.

| Variable                         | Type            | Default          | Description                                                                                                                                                                                                                                                                                                            |
| -------------------------------- | --------------- | ---------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `CERBERUS_PROM_RESOURCE_LABELS`  | CSV (`a,b,c`)   | (empty = all)    | Allowlist of OTel `ResourceAttributes` keys (in their **original dotted** form, e.g. `k8s.namespace.name`) projected as Prometheus labels. **Empty / unset promotes EVERY resource key** — the allowlist is opt-IN narrowing, not opt-in enabling. Matching is on the dotted key; the wire emits the sanitized form.   |

The allowlist gates every read surface in lock-step: the bare-selector
projection, `sum`/`avg by(...)`/`without(...)` aggregations, the matcher
`WHERE`, `/api/v1/series`, `/api/v1/labels`, and
`/api/v1/label/<name>/values`. A custom schema that clears the
`ResourceAttributes` column disables the resource arm entirely regardless
of this knob.

**Cardinality.** Promote-all is unbounded by design: high-churn resource keys
(`k8s.pod.name`, `k8s.pod.uid`, `host.id`) become labels and multiply
active-series cardinality. Set the allowlist to the keys you actually query on
at scale to bound it.

**Non-dot sanitization caveat.** Resource-attribute keys containing characters
*other* than dots that sanitize to underscore (e.g. `-`, `/`, `:`) are surfaced
on `/api/v1/labels` and the bare-selector projection, but are **not** addressable
by an underscored matcher or `/api/v1/label/<name>/values` — the candidate chain
only reverses underscore→dot, so it can't reconstruct the original `-`/`/`/`:`
key from the underscored form (the same caveat as the leading-digit case).

## Execution / solver

The sharded-pushdown solver (`internal/solver`, [`solver.md`](solver.md))
handles the one query class route A cannot bound: high **anchor fan-out**
(`F = Range/Step`), where one statement's peak intermediate cardinality exceeds
the ClickHouse memory cap. It is **on by default** (`CERBERUS_EVAL_ROUTE=auto`)
and fails toward route A: only eligible plans that clear the cost thresholds
take route B; everything else stays byte-identical on route A.

| Variable                               | Type     | Default   | Description                                                                                                                                                                                                                                 |
| -------------------------------------- | -------- | --------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `CERBERUS_EVAL_ROUTE`                  | string   | `auto`    | Solver mode: `auto` routes eligible above-threshold plans; `single` disables routing (every request runs route A); `sharded` forces every eligible plan to route B (used by the forced-route compatibility lane, not a production setting). |
| `CERBERUS_SHARD_MIN_FANOUT`            | int      | `16`      | `Fmin` — minimum anchor fan-out `F = max(Range/Step)` a plan must reach to be worth slicing (auto mode).                                                                                                                                    |
| `CERBERUS_SHARD_MIN_ANCHOR_PAIRS`      | int      | `4000`    | Minimum expanded `(sample, anchor)` pair count `N×F` a plan must reach (auto mode).                                                                                                                                                         |
| `CERBERUS_SHARD_MAX_K`                 | int      | `8`       | Caps the shard count `K`. Must be >= 2.                                                                                                                                                                                                     |
| `CERBERUS_SHARD_MIN_ANCHORS_PER_SLICE` | int      | `16`      | Grid quantum — each slice owns at least this many anchors (never fewer than 2).                                                                                                                                                             |
| `CERBERUS_SHARD_PARALLEL`              | int      | `3`       | `P` — per-request shard concurrency. Must be >= 1.                                                                                                                                                                                          |
| `CERBERUS_SOLVER_TIMEOUT`              | duration | `60s`     | End-to-end bound on a routed request. Must be > 0.                                                                                                                                                                                          |
| `CERBERUS_SHARD_MAX_OUTPUT_ROWS`       | int64    | `2000000` | Caps composed per-request output rows; an overrun is a typed `422`. Must be > 0.                                                                                                                                                            |
| `CERBERUS_SHARD_MEMORY_APPORTION`      | bool     | `false`   | When `true`, per-shard `max_memory_usage` is `cap/P` (256 MiB floor), holding total exposure at the single-query cap.                                                                                                                       |

## Experimental flags

| Variable                              | Type | Default | Description                                                                                                                                                     |
| ------------------------------------- | ---- | ------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `CERBERUS_EXPERIMENTAL_TS_GRID_RANGE` | bool | `false` | Emit ClickHouse-native `timeSeriesRateToGrid` for eligible `rate(<counter>[range])` query_range instead of the default arrayJoin fan-out. Scope is `rate` only. |

`CERBERUS_EXPERIMENTAL_TS_GRID_RANGE` **requires ClickHouse ≥ 25.6** — the
`timeSeries*ToGrid` family was introduced in CH v25.6.0; on any older server a
native-path query errors with `UNKNOWN_FUNCTION`, so the flag **must stay off**
unless the target ClickHouse is ≥ 25.6 (the compose / e2e / compatibility lanes
all run 25.8, so the floor is met where the flag is exercised). The native
operator computes the same
Prometheus `extrapolatedRate` inside the engine, closing the execution-layer gap
the SQL array machinery leaves at high cardinality. Default off is byte-for-byte
the established fan-out. See [`performance.md`](performance.md#the-durable-answer)
for the why and [`benchmarks.md`](benchmarks.md) for the recorded numbers.

## Loki streaming

| Variable                           | Type     | Default | Description                                                                                            |
| ---------------------------------- | -------- | ------- | ------------------------------------------------------------------------------------------------------ |
| `CERBERUS_LOKI_TAIL_WRITE_TIMEOUT` | duration | `10s`   | Bound on a single `/loki/api/v1/tail` WebSocket write before a slow / dead client is torn down. `> 0`. |

## Dependency matrix

Most knobs are validated in isolation (unknown enum, out-of-range buffer,
malformed URL, non-positive where positive is required). Some knobs, however,
only make sense in combination — an individually-valid value can be incoherent
next to another. Cerberus rejects these **combinations** at startup with an
error that names both knobs, rather than silently ignoring or downgrading one
of them. The full set of cross-setting rules:

| Rule                                                | Knobs involved                                                                                                           | Why it fails fast                                                                                                                                                                                          |
| --------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| TLS cert/key are both-or-neither                    | `_TLS_CERT_FILE`, `_TLS_KEY_FILE`                                                                                        | A lone cert or key cannot form an mTLS client key pair.                                                                                                                                                    |
| TLS sub-knobs require enable                        | `_TLS_ENABLED` vs `_TLS_CA_FILE` / `_TLS_CERT_FILE` / `_TLS_KEY_FILE` / `_TLS_SERVER_NAME` / `_TLS_INSECURE_SKIP_VERIFY` | Silently-ignored TLS config is a security footgun.                                                                                                                                                         |
| skip-verify contradicts CA / server-name            | `_TLS_INSECURE_SKIP_VERIFY` vs `_TLS_CA_FILE` / `_TLS_SERVER_NAME`                                                       | skip-verify ignores both — pinning a CA or hostname alongside it is incoherent.                                                                                                                            |
| HTTP-protocol knobs require `http`                  | `CERBERUS_CH_PROTOCOL` vs `_HTTP_HEADERS` / `_HTTP_URL_PATH` / `_HTTP_MAX_CONNS_PER_HOST` / `_HTTP_PROXY_URL`            | Under `native` they would be silently dropped.                                                                                                                                                             |
| Compression level requires a method                 | `CERBERUS_CH_COMPRESSION` vs `CERBERUS_CH_COMPRESSION_LEVEL`                                                             | A level with `none` does nothing; a level must also sit in the method's range (lz4 `0..12`, zstd `1..22`).                                                                                                 |
| Read timeout ≥ query timeout                        | `CERBERUS_CH_READ_TIMEOUT` vs `CERBERUS_QUERY_TIMEOUT`                                                                   | A socket read shorter than the query budget would kill legitimate long queries.                                                                                                                            |
| Idle conns ≤ open conns                             | `CERBERUS_CH_MAX_IDLE_CONNS` vs `CERBERUS_CH_MAX_OPEN_CONNS`                                                             | More idle than total pooled connections is a degenerate pool. Fires only when idle is **explicitly set** — lowering only `MAX_OPEN_CONNS` below the default idle is fine (the driver clamps idle to open). |
| Server header timeout ≤ read timeout                | `CERBERUS_HTTP_READ_HEADER_TIMEOUT` vs `CERBERUS_HTTP_READ_TIMEOUT`                                                      | A header deadline longer than the whole-request deadline can never fire.                                                                                                                                   |

Benign-but-pointless combinations are **not** hard errors — they are noted here
rather than rejected:

- `CERBERUS_CH_CONN_OPEN_STRATEGY=round_robin` with a single `CERBERUS_CH_ADDR`
  host: the strategy has nothing to rotate over, but it is harmless.
- Keepalive timing sub-knobs (`CERBERUS_CH_KEEPALIVE_IDLE` / `_INTERVAL` /
  `_COUNT`) while `CERBERUS_CH_KEEPALIVE_ENABLED=false`: inert (the kernel never
  arms a probe schedule), so a degenerate value is accepted, not rejected, when
  keepalive is disabled.
