# Configuration

Cerberus is a stateless 12-factor binary configured **entirely** through
`CERBERUS_*` environment variables — there is no YAML, INI, or TOML file to
load. ClickHouse and the (optional) OpenTelemetry collector are attached
resources reached through env-var connection inputs, so swapping a local
single-node ClickHouse for a managed cluster, or a sidecar collector for a
SaaS ingest URL, is a matter of flipping env vars and restarting. Every
default below is the value `internal/config/config.go` ships out of the box.

Misconfigured values fail fast: an unparseable duration, an out-of-range
integer, an unknown log level, or a malformed OTLP header list aborts startup
with a clear error rather than silently downgrading behaviour. Secrets (the
ClickHouse password, OTLP bearer tokens) live in this same env-var namespace
and are sourced from a Kubernetes `Secret`, a Docker `secrets:` mount, or a
vault-injecting init container — never committed.

For how these knobs interact with the running service — lifecycle, readiness,
deployment, scaling — see [`operations.md`](operations.md).

## HTTP server

| Variable             | Type   | Default | Description                                                                                 |
| -------------------- | ------ | ------- | ------------------------------------------------------------------------------------------- |
| `CERBERUS_HTTP_ADDR` | string | `:8080` | HTTP listen address for the Prom / Loki / Tempo APIs and the `/healthz` / `/readyz` probes. |

A single listener serves all three upstream APIs plus the health probes; there
is no separate admin port. See [`operations.md`](operations.md#port-binding)
for the port-binding contract (including h2c + gRPC on the same socket).

## ClickHouse connection

ClickHouse is the only mandatory backing service, reached exclusively through
these connection inputs.

| Variable                   | Type     | Default          | Description                                            |
| -------------------------- | -------- | ---------------- | ------------------------------------------------------ |
| `CERBERUS_CH_ADDR`         | string   | `localhost:9000` | ClickHouse native-protocol endpoint.                   |
| `CERBERUS_CH_DATABASE`     | string   | `otel`           | ClickHouse database name.                              |
| `CERBERUS_CH_USERNAME`     | string   | `default`        | ClickHouse user.                                       |
| `CERBERUS_CH_PASSWORD`     | string   | (empty)          | ClickHouse password.                                   |
| `CERBERUS_CH_DIAL_TIMEOUT` | duration | `5s`             | ClickHouse dial timeout (`time.ParseDuration` syntax). |

## Connection pool

These reproduce clickhouse-go/v2's previously-implicit pool defaults verbatim,
made explicit so the sharded-pushdown solver can raise the ceiling for fan-out
rather than inherit a hidden driver default. When the pool is exhausted an
acquire blocks up to `CERBERUS_CH_DIAL_TIMEOUT` and then fails with a
breaker-neutral acquire-timeout (a local pool-sizing signal, not a
ClickHouse-health failure).

| Variable                        | Type     | Default | Description                                                        |
| ------------------------------- | -------- | ------- | ------------------------------------------------------------------ |
| `CERBERUS_CH_MAX_OPEN_CONNS`    | int      | `10`    | Total pooled ClickHouse connections (busy + idle). Must be > 0.    |
| `CERBERUS_CH_MAX_IDLE_CONNS`    | int      | `5`     | Idle ClickHouse connections kept warm for reuse. Must be > 0.      |
| `CERBERUS_CH_CONN_MAX_LIFETIME` | duration | `1h`    | Max age of a pooled connection before it is recycled. Must be > 0. |

## Query limits and memory

| Variable                       | Type     | Default      | Description                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| ------------------------------ | -------- | ------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `CERBERUS_CH_QUERY_MAX_MEMORY` | int64    | `1073741824` | Per-query ClickHouse memory cap in bytes (`max_memory_usage` on every data-plane query; DDL exempt). 1 GiB default. `0` leaves it unset (CH server defaults apply). A query over the cap gets a breaker-neutral resource-exhausted rejection (Prom 422 / Loki 400 / Tempo 422).                                                                                                                                                                                                                                                                                                                                                                                                         |
| `CERBERUS_QUERY_MAX_SAMPLES`   | int64    | `50000000`   | Per-query sample budget, mirroring Prometheus `--query.max-samples`. Bounds cerberus-process memory by aborting a result-set drain that crosses the budget. `0` disables. Memory-constrained deploys should set this well below the default.                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| `CERBERUS_QUERY_TIMEOUT`       | duration | `2m`         | Per-query wall-clock cap, stamped as ClickHouse `max_execution_time` (with `timeout_overflow_mode=throw`) on every data-plane query; DDL exempt. Mirrors Prometheus `--query.timeout`, so a pathological query gets a server-side deadline instead of holding an admit slot + pooled connection unbounded. The standard Prometheus `?timeout=<duration>` query param (and Loki's `timeout`) min's against this default per request (the smaller wins) and is threaded to both the request ctx deadline and `max_execution_time`. A query over the cap gets a breaker-neutral timeout rejection (Prom/Loki 503 `errorType=timeout`, Tempo 503). `0` disables (CH server defaults apply). |

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

| Variable                      | Type | Default | Description                                                                                 |
| ----------------------------- | ---- | ------- | ------------------------------------------------------------------------------------------- |
| `CERBERUS_AUTO_CREATE_SCHEMA` | bool | `false` | When `true`, run the idempotent OTel-CH exporter DDL at startup before HTTP serving begins. |

Schema-shape overrides — the table names cerberus reads when the ClickHouse
layout deviates from the OTel-CH exporter defaults (`CERBERUS_SCHEMA_METRICS_*`,
`CERBERUS_SCHEMA_LOGS_TABLE`, `CERBERUS_SCHEMA_TRACES_TABLE`) — are documented in
[`observability.md`](observability.md#schema-shape-overrides).

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
run 25.8, so the floor is met there). The native operator computes the same
Prometheus `extrapolatedRate` inside the engine, closing the execution-layer gap
the SQL array machinery leaves at high cardinality. Default off is byte-for-byte
the established fan-out. See [`performance.md`](performance.md#the-durable-answer)
for the why and [`benchmarks.md`](benchmarks.md) for the recorded numbers.
