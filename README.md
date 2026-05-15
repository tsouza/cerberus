# cerberus

> Drop-in **Prometheus / Loki / Tempo** HTTP gateway for **ClickHouse**.
> Keep Grafana, alerting, and your CLI tooling. Swap the backend.

[![CI](https://github.com/tsouza/cerberus/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/tsouza/cerberus/actions/workflows/ci.yml)
[![Mutation](https://github.com/tsouza/cerberus/actions/workflows/mutation.yml/badge.svg?branch=main)](https://github.com/tsouza/cerberus/actions/workflows/mutation.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Go Reference](https://pkg.go.dev/badge/github.com/tsouza/cerberus.svg)](https://pkg.go.dev/github.com/tsouza/cerberus)
[![Go Report Card](https://goreportcard.com/badge/github.com/tsouza/cerberus)](https://goreportcard.com/report/github.com/tsouza/cerberus)

---

## Why cerberus?

If you've ever shipped metrics, logs, and traces to three different stores just to satisfy three different query languages — Prom/Loki/Tempo — you know the operational tax: three retention policies, three index strategies, three storage bills, three on-call playbooks.

ClickHouse is a great single store for all three signals. The only thing missing has been: **the existing Grafana ecosystem doesn't speak SQL.** Cerberus closes that gap. Point Grafana at cerberus as three datasources (Prometheus, Loki, Tempo) and the queries just work — translated into optimized ClickHouse SQL underneath.

- **No Grafana plugin.** Cerberus speaks the upstream HTTP APIs verbatim.
- **No custom QL.** PromQL, LogQL, TraceQL — exactly as your dashboards and alerts already use them.
- **No reinvented parsers.** Cerberus imports `prometheus/promql/parser`, `grafana/loki/v3/pkg/logql/syntax`, and `grafana/tempo/pkg/traceql` directly. If the upstream parses it, cerberus parses it.

## Status

> **`v1.0.0` candidate.** Cerberus has shipped the full RC1–RC8 backlog: PromQL / LogQL / TraceQL parse + lowering, the pattern-based optimizer (transposes, PREWHERE promotion, late materialisation, MV substitution), the typed `chsql` SQL emitter, the shared `internal/engine` pipeline, full self-observability (`slog` + OTel traces + OTLP-exported metrics), 12-factor packaging (`/readyz`, admission control, `docker compose up` for one-command local dev), and the chDB-backed round-trip + property test suites. Upstream parser shims are retired via the [`tsouza/tempo:cerberus-accessors`](https://github.com/tsouza/tempo/tree/cerberus-accessors) fork; the schema source of truth is [`tsouza/opentelemetry-collector-contrib:cerberus-ddl`](https://github.com/tsouza/opentelemetry-collector-contrib/tree/cerberus-ddl). See [`CHANGELOG.md`](CHANGELOG.md) for the per-release breakdown.

## Architecture

```text
   PromQL                LogQL                TraceQL
     │                     │                     │
     ▼                     ▼                     ▼
prometheus/        grafana/loki/v3/         grafana/tempo/
promql/parser      pkg/logql/syntax         pkg/traceql
     │                     │                     │
     │     per-QL high-level IR (parser AST)     │
     │                     │                     │
     ▼                     ▼                     ▼
 ┌──────────────────────────────────────────────────┐
 │            internal/chplan — shared IR           │
 │ Scan • Filter • Project • Aggregate •            │
 │ RangeWindow • Limit + expression tree            │
 └──────────────────────────────────────────────────┘
                       │
                       ▼
            ┌────────────────────────┐
            │  internal/optimizer    │  rule-based, fixpoint
            │  • predicate pushdown  │
            │  • filter fusion       │
            │  • constant folding    │
            └────────────────────────┘
                       │
                       ▼
            ┌────────────────────────┐
            │  internal/chsql        │  emit parameterised SQL
            └────────────────────────┘
                       │
                       ▼
                  ClickHouse
```

Schema defaults to the [OpenTelemetry ClickHouse Exporter](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/exporter/clickhouseexporter) layout (`otel_metrics_*`, `otel_logs`, `otel_traces`). A thin YAML override config supports SigNoz schemas and custom column layouts.

The three heads share one query pipeline — `internal/engine/`. Each
head plugs in as a `Lang` adapter (parser + lowering + per-language
projection); the engine owns the parse → optimize → emit → execute
loop and the telemetry around it. See [`docs/engine.md`](docs/engine.md)
for the contract, request lifecycle, and extension points.

## Quick start

Cerberus is a [12-factor app](docs/12factor.md): one stateless binary,
configured entirely via environment variables, treating ClickHouse and
the optional OTel collector as attached resources. The same image runs
unchanged under Docker Compose, Kubernetes, or a bare-metal supervisor.

### Docker Compose (one-command local dev)

```sh
git clone https://github.com/tsouza/cerberus.git && cd cerberus
docker compose up --wait
open http://localhost:3000   # Grafana (admin/admin); cerberus on :8080
```

The stack builds cerberus from the repo, boots a single-node ClickHouse,
loads the deterministic OTel fixture (logs / traces / metrics), and brings
up Grafana pre-provisioned with cerberus as three datasources (Prom +
Loki + Tempo). ClickHouse data persists in a named volume; use
`docker compose down -v` to wipe it.

### From a published release

Pull the container image (`:latest` is **only** advanced by stable
releases; RC / alpha / beta tags don't move it):

```sh
docker pull ghcr.io/tsouza/cerberus:v1.0.0
docker run --rm -p 8080:8080 \
  -e CERBERUS_CH_ADDR=clickhouse:9000 \
  ghcr.io/tsouza/cerberus:v1.0.0
```

Or download a prebuilt binary from the [release page](https://github.com/tsouza/cerberus/releases)
(linux / darwin × amd64 / arm64). Each release also ships a
[SLSA build provenance](https://slsa.dev) attestation; verify with:

```sh
gh attestation verify cerberus_*_linux_amd64.tar.gz \
  --owner tsouza --repo cerberus
```

### Local dev

```sh
direnv allow           # loads .envrc (puts Go on PATH, GOTOOLCHAIN=auto)
just install-tools     # one-time: golangci-lint, gofumpt, goimports, gremlins
just ci                # lint + test + build
just build && ./bin/cerberus --help
```

End-to-end against a real ClickHouse + Grafana in k3d:

```sh
just e2e-up            # boot k3d cluster, deploy CH / Grafana / cerberus
just e2e-seed          # ingest sample OTel data
just e2e-run           # Go E2E tests + Grafana playwright smoke
just e2e-down          # tear down
```

## Testing

Cerberus is tested in 12 layers — AST shape pinning, plan-IR
invariants, optimizer properties, emitted-SQL goldens, chDB-backed
roundtrips, HTTP wire conformance, system lifecycle, differential
shadow harness, Playwright UX flows, chaos / goleak, perf benchmarks
with alloc regressions, and an oracle-based property framework. See
[`docs/test-strategy.md`](docs/test-strategy.md) for the canonical
layer map, CI-gate inventory, gremlins phased rollout, and per-layer
recipes for adding a new test.

Quick reference:

| Layer family     | What it covers                                                                    | How to run                                          |
| ---------------- | --------------------------------------------------------------------------------- | --------------------------------------------------- |
| **Unit**         | Per-package logic, `Equal` contracts, optimizer rule kernels, Frag goldens        | `just test`                                         |
| **Spec (TXTAR)** | `<QL> → expected SQL` + chplan IR snapshots + optional chDB roundtrip             | `just test`; `just spec-chdb` for roundtrip lane    |
| **Property**     | Oracle-based property tests with `rapid` shrinking and chDB execution             | `go test -tags chdb ./test/property/...`            |
| **Integration**  | `chclient` against a real ClickHouse via testcontainers                           | `go test -tags=integration ./internal/chclient/...` |
| **E2E**          | k3d cluster with CH + Grafana + cerberus; Grafana Playwright smoke                | `just e2e`                                          |
| **Compat**       | `prometheus/compliance` differential harness                                      | `just compatibility`                                |
| **Mutation**     | Gremlins matrix — see `docs/test-strategy.md` § "Gremlins phased rollout" for bar | `just mutate` (slow, nightly in CI)                 |

## Project structure

```text
cmd/cerberus/            # main entrypoint
internal/
  api/{prom,loki,tempo}/ # HTTP handlers per upstream API
  promql/                # PromQL head: parse + lower
  logql/                 # LogQL head (stub)
  traceql/               # TraceQL head (stub)
  chplan/                # shared plan IR
  chsql/                 # plan → CH SQL emitter
  optimizer/             # rule + driver + rule implementations
  chclient/              # CH driver wrapper
  schema/                # OTel schema defaults + overrides
  config/                # runtime config
test/
  spec/                  # TXTAR fixture-driven tests
  e2e/                   # k3d + playwright
  e2e/k3s/               # k3d manifests consumed by the smoke
  e2e/grafana/           # provisioned datasources + dashboards
```

## Contributing

Open an issue or a discussion before opening a large PR — the seed is opinionated and the architecture lockdown is recent. Smaller PRs (a new optimizer rule, a new TXTAR fixture, a parser-dep bump) are welcome any time. See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[MIT](LICENSE) © Thiago Souza.
