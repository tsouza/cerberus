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

> 🚧 **`v1.0.0-RC1` candidate** — M1 (PromQL) + M2 (Prom HTTP API) + M3 (LogQL) + M4 (TraceQL + Tempo HTTP API) all merged. TraceQL TXTAR corpus expanded 8 → 26, chsql 15 → 29; Playwright covers Loki + Tempo + richer Prom against Grafana; cerberus-side HTTP integration tests cover every shipped surface. The RC1 tag is the only step left — see [`CHANGELOG.md`](CHANGELOG.md) for the full Unreleased entry, and [`docs/roadmap.md`](docs/roadmap.md) for the path to v1.0.0.

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

## Quick start

### From a published release

Pull the container image (pin to a specific RC — `:latest` is **only**
advanced by stable releases; RC / alpha / beta tags don't move it):

```sh
docker pull ghcr.io/tsouza/cerberus:v1.0.0-RC1
docker run --rm -p 8080:8080 \
  -e CERBERUS_CH_ADDR=clickhouse:9000 \
  ghcr.io/tsouza/cerberus:v1.0.0-RC1
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

## Testing layers

| Layer            | What it covers                                                                    | How to run                                          |
| ---------------- | --------------------------------------------------------------------------------- | --------------------------------------------------- |
| **Unit**         | Per-package logic, `Equal` contracts, optimizer rule kernels                      | `just test`                                         |
| **Spec (TXTAR)** | `<QL> → expected SQL` and `plan → optimized plan` golden tests under `test/spec/` | `just test`; `just update-golden` to regenerate     |
| **Integration**  | `chclient` against a real ClickHouse via testcontainers                           | `go test -tags=integration ./internal/chclient/...` |
| **E2E**          | k3d cluster with CH + Grafana + cerberus; Grafana playwright smoke                | `just e2e`                                          |
| **Mutation**     | Gremlins mutation testing on `internal/` logic packages                           | `just mutate` (slow, nightly in CI)                 |

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
  e2e/                   # k3d + playwright (PR8)
deploy/k3s/              # Kubernetes manifests
deploy/grafana/          # provisioned datasources + dashboards
```

## Contributing

Open an issue or a discussion before opening a large PR — the seed is opinionated and the architecture lockdown is recent. Smaller PRs (a new optimizer rule, a new TXTAR fixture, a parser-dep bump) are welcome any time. See [CONTRIBUTING.md](CONTRIBUTING.md) (lands in PR9).

## License

[MIT](LICENSE) © Thiago Souza.
