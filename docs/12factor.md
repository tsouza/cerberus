# Cerberus and the 12-Factor App

Cerberus is built as a long-lived HTTP server that holds no operational
state and treats every dependency as an attached resource. That shape
maps cleanly onto Adam Wiggins' [12-Factor App](https://12factor.net/)
methodology, which is why a single binary can be dropped into Docker
Compose, Kubernetes, Nomad, or a bare-metal supervisor without
configuration changes.

This page enumerates the twelve factors and describes how cerberus
complies with each, using concrete environment variables, files, and
commands from the current codebase.

## Factor I — Codebase

> One codebase tracked in revision control, many deploys.

Cerberus is a single Go module — `github.com/tsouza/cerberus` — tracked
in [one Git repository](https://github.com/tsouza/cerberus). There are
no sibling repositories, no vendored sub-projects, and no separate
"agent" or "controller" deliverables: the same `cmd/cerberus` binary
serves every Prom/Loki/Tempo HTTP request. A single tag in the repo
maps to a single set of release artefacts (container image on GHCR,
prebuilt binaries on the GitHub release page) and the same artefact is
re-used unchanged across dev, staging, and production deployments.

Upstream parser forks (e.g. [`tsouza/tempo:cerberus-accessors`](https://github.com/tsouza/tempo/tree/cerberus-accessors)
and [`tsouza/opentelemetry-collector-contrib:cerberus-ddl`](https://github.com/tsouza/opentelemetry-collector-contrib/tree/cerberus-ddl))
are pulled in via `go.mod` `replace` directives — they are dependencies,
not parallel codebases.

## Factor II — Dependencies

> Explicitly declare and isolate dependencies.

Every Go dependency is pinned in `go.mod` / `go.sum`. The toolchain
itself is pinned: `go.mod` carries a `go 1.x.y` directive and
`GOTOOLCHAIN=auto` (the default in modern Go) silently fetches the
correct toolchain into the module cache when a developer's system Go
is older. No `GOPATH` writes, no system-wide installs.

Container builds use a multi-stage Dockerfile that builds inside a
golang base image and copies the static binary into a distroless final
stage. The resulting image has no shell, no package manager, and no
implicit reliance on host libraries: it inherits exactly what was
declared at build time.

External binaries needed during development (`golangci-lint`,
`gofumpt`, `goimports`, `gremlins`) are installed via `just install-tools`,
each pinned to a specific version inside the `Justfile`. CI installs
the same versions via first-party GitHub Actions
(`actions/setup-go@v6`, `golangci/golangci-lint-action@v7`) so the
build environment is reproducible.

## Factor III — Config

> Store config in the environment.

Every runtime knob is an environment variable read at startup by
`internal/config/config.go` — no YAML, INI, or TOML files are loaded.
The canonical list:

| Variable                      | Default           | Meaning                                                              |
| ----------------------------- | ----------------- | -------------------------------------------------------------------- |
| `CERBERUS_HTTP_ADDR`          | `:8080`           | HTTP listen address for the Prom/Loki/Tempo APIs and health probes.  |
| `CERBERUS_CH_ADDR`            | `localhost:9000`  | ClickHouse native-protocol endpoint.                                 |
| `CERBERUS_CH_DATABASE`        | `otel`            | ClickHouse database name.                                            |
| `CERBERUS_CH_USERNAME`        | `default`         | ClickHouse user.                                                     |
| `CERBERUS_CH_PASSWORD`        | (empty)           | ClickHouse password.                                                 |
| `CERBERUS_CH_DIAL_TIMEOUT`    | `5s`              | ClickHouse dial timeout (`time.ParseDuration` syntax).               |
| `CERBERUS_AUTO_CREATE_SCHEMA` | `false`           | When `true`, apply the OTel-CH DDL at startup before serving.        |
| `CERBERUS_LOG_FORMAT`         | `text`            | slog handler kind (`text` or `json`).                                |
| `CERBERUS_LOG_LEVEL`          | `info`            | Minimum slog level (`debug` / `info` / `warn` / `error`).            |
| `CERBERUS_OTLP_ENDPOINT`      | (empty)           | gRPC OTLP target for self-telemetry. Empty disables exporters.       |
| `CERBERUS_OTLP_INSECURE`      | `false`           | Dial OTLP endpoint without TLS.                                      |
| `CERBERUS_OTLP_HEADERS`       | (empty)           | Comma-separated `key=value` gRPC metadata (e.g. auth tokens).        |
| `CERBERUS_OTLP_TIMEOUT`       | `10s`             | Per-request OTLP roundtrip timeout.                                  |

Misconfigured values fail fast: an unparseable duration, an unknown
log level, or a malformed OTLP header list aborts startup with a clear
error rather than silently downgrading behaviour. Secrets (CH password,
OTLP bearer tokens) live in the same env-var namespace and are sourced
from Kubernetes `Secret` / Docker `secrets:` / a vault-injecting init
container — never committed.

## Factor IV — Backing services

> Treat backing services as attached resources.

ClickHouse is the only mandatory backing service, and it is reached
exclusively through the connection inputs above. Swapping a local
single-node CH for a managed ClickHouse Cloud cluster is a matter of
flipping `CERBERUS_CH_ADDR` / `CERBERUS_CH_USERNAME` / `CERBERUS_CH_PASSWORD`
and restarting the process — there is no code path that knows or
cares whether the resource is local, in-cluster, or remote.

The optional OTLP collector for self-telemetry is treated the same
way: `CERBERUS_OTLP_ENDPOINT` may point at a sidecar, a cluster-local
collector, or a SaaS ingest URL. When unset, cerberus installs no-op
trace and meter providers and runs as a zero-collector-dependency
binary.

## Factor V — Build, release, run

> Strictly separate build and run stages.

- **Build** — `goreleaser` produces release artefacts (binaries +
  container images) from a Git tag. Source code is compiled, the
  binary is statically linked (`CGO_ENABLED=0` in release builds),
  and the version string is injected via `-ldflags` so `Version` in
  `cmd/cerberus/main.go` reflects the tag.
- **Release** — the build output is combined with the deployment
  configuration. In Kubernetes that means a specific image SHA in
  `deploy/k3s/cerberus.yaml` (or the operator's chart) plus the
  `cerberus-config` ConfigMap. The release is immutable: rolling
  back means redeploying the previous tag, not editing files in
  place.
- **Run** — the container is started; the process reads its
  configuration from the environment and binds its HTTP listener.
  No build-time work happens at run time; no `go run`, no `make` in
  the final image.

The distroless image enforces this separation by construction: it
ships only the compiled binary and root CA bundle.

## Factor VI — Processes

> Execute the app as one or more stateless processes.

Cerberus holds no operational state. There is no in-process query
cache, plan cache, result cache, or session store — every HTTP
request goes through parse → lower → optimize → emit → execute
against ClickHouse from a clean slate. The only in-process memory
that survives a request is:

- The ClickHouse driver connection pool (`internal/chclient`).
- The schema configuration (`internal/schema`, immutable after
  startup).
- A 2-second TTL cache inside the readiness probe handler
  (`internal/api/health`) so probe traffic does not amplify into
  ClickHouse pings.

None of these survive a process restart, and none are shared across
replicas. ClickHouse is the durable store; cerberus is a stateless
translation layer in front of it.

## Factor VII — Port binding

> Export services via port binding.

Cerberus binds a single HTTP listener on `CERBERUS_HTTP_ADDR`
(default `:8080`). All three upstream APIs (Prometheus, Loki, Tempo)
plus the `/healthz` and `/readyz` probes are mounted on that one
listener — there is no separate admin port, no Unix socket, no
embedded TLS terminator. A reverse proxy or a Kubernetes `Service`
publishes the port to the outside world; cerberus itself only knows
how to bind and serve.

The same binding semantics apply in every environment: `docker compose
up` exposes `8080:8080`, `deploy/k3s/cerberus.yaml` declares a
`NodePort` on `30080 → 8080`, and a local `./cerberus` run from
source listens on `:8080`. No env-var translation is needed between
deployment targets.

## Factor VIII — Concurrency

> Scale out via the process model.

Cerberus scales horizontally by adding replicas. Because the process
is stateless (Factor VI), an N-replica deployment behind a round-robin
load balancer (Kubernetes `Service`, an external L4/L7 LB, or HAProxy)
distributes load without any coordination between cerberus instances.
ClickHouse handles the actual heavy lifting — parallel query
execution, distributed table sharding, result merging — so cerberus
horizontal scaling is bounded only by ClickHouse capacity, not by
cerberus's own CPU.

A single cerberus process is itself concurrent: the standard
`net/http` server multiplexes goroutines per request, and the
ClickHouse driver pool serves them from a shared connection set.
There is no `--workers` flag because there is no need for one.

## Factor IX — Disposability

> Maximize robustness with fast startup and graceful shutdown.

**Startup** is fast: the only blocking operations are reading env
vars, dialling ClickHouse (capped by `CERBERUS_CH_DIAL_TIMEOUT`,
default `5s`), and — if `CERBERUS_AUTO_CREATE_SCHEMA=true` — applying
the idempotent OTel-CH DDL. The HTTP listener is up within seconds
of the container starting. A reasonable Kubernetes
`initialDelaySeconds: 2` on the readiness probe is enough (see
`deploy/k3s/cerberus.yaml`).

**Shutdown** is graceful and signal-driven. `cmd/cerberus/main.go`
installs a `signal.NotifyContext` for `SIGINT` and `SIGTERM`. On
signal:

1. The HTTP server stops accepting new connections (`http.Server.Shutdown`).
2. In-flight requests are drained inside a 10-second shutdown context.
3. Pending OTLP trace + metric batches are flushed
   (`telemetry.Providers.Shutdown`) so the last few spans do not
   vanish.

If the OTLP collector is unreachable during shutdown, the exporter
logs the error and returns — cerberus still exits cleanly rather
than hanging.

The detailed health-probe contract (which Kubernetes uses to decide
when to send `SIGTERM`) is documented in [`health.md`](health.md).

## Factor X — Dev/prod parity

> Keep development, staging, and production as similar as possible.

The repo ships two deployment surfaces that intentionally mirror each
other:

- **Docker Compose** (`docker-compose.yml` at the repo root) — boots
  ClickHouse, cerberus, a one-shot OTel-fixture seeder, and Grafana
  pre-provisioned with cerberus as three datasources. `docker compose
  up --wait` and the local stack is queryable.
- **Kubernetes / k3d** (`deploy/k3s/`) — the same image is built with
  `just e2e-up`, imported into a k3d cluster, and run alongside the
  same ClickHouse + Grafana topology. The shipped manifests
  (`cerberus.yaml`, `clickhouse.yaml`, `grafana.yaml`,
  `otel-collector.yaml`) are real-world starting points, not toy
  examples.

Both stacks consume the same `CERBERUS_*` env vars in the same
shapes, the same container image, and the same OTel-CH schema. A
dashboard that works locally works in k3d; a dashboard that works in
k3d works in production. The E2E test suite (`just e2e-up && just
e2e-seed && just e2e-run`) exercises the parity end-to-end via a
Playwright smoke against Grafana.

## Factor XI — Logs

> Treat logs as event streams.

Cerberus writes structured logs to `stderr` via the standard library
`log/slog` package — never to a file, never to a syslog socket, never
to a rotated log directory. The process is oblivious to the log
sink; whatever runs cerberus (Docker, Kubernetes, systemd, a
developer's terminal) decides what happens to the stream.

Two env vars steer the handler:

- `CERBERUS_LOG_FORMAT` — `text` (default, human-readable) or
  `json` (machine-parseable, recommended for any deployment with a
  log aggregator).
- `CERBERUS_LOG_LEVEL` — `debug` / `info` / `warn` / `error`.

In Docker Compose the stream is collected by the Docker logging
driver; in Kubernetes it is collected by the node-level log agent
(typically Fluent Bit or the OTel Collector's `filelog` receiver)
and forwarded to a downstream store. Cerberus itself remains
write-only.

The full logging contract (level vocabulary, attribute keys, slog
attribute conventions) is documented in [`observability.md`](observability.md#logging-r41-shipped).

## Factor XII — Admin processes

> Run admin/management tasks as one-off processes.

Cerberus exposes no in-process admin interface (no `/admin` HTTP
namespace, no `cerberus admin` subcommand that talks to a running
instance). Operational tasks run as separate, short-lived processes
against the same backing ClickHouse:

- **Schema bootstrap** — the OTel-CH DDL lives in
  `internal/schema/ddl` and is applied either as a startup hook
  inside cerberus (`CERBERUS_AUTO_CREATE_SCHEMA=true`) or as a
  one-shot job from CI / an operator's terminal. The DDL is
  idempotent (`CREATE TABLE IF NOT EXISTS`), so re-running it is
  safe.
- **Seed data** — `go run ./test/e2e/seed/cmd/seed` (also run as the
  `seed` service in `docker-compose.yml`) inserts the deterministic
  OTel fixture into ClickHouse and exits. It uses the same
  ClickHouse connection inputs as cerberus does, so a single set of
  credentials drives both the long-lived server and the one-shot
  task.
- **Compatibility harness** — `just compatibility` boots cerberus
  alongside upstream Prometheus and diffs the two against a shared
  fixture. Again, a one-shot process; cerberus stays unchanged.

Tasks like these run as the same binary or as adjacent helpers from
the same repository (`cmd/`, `test/e2e/seed/cmd/`), never as separate
long-running services with their own deployment lifecycle.
