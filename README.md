# cerberus

> Three-headed query gateway: **PromQL**, **LogQL**, and **TraceQL** → ClickHouse SQL.

Cerberus lets you keep a single observability backend — ClickHouse — while continuing to point Grafana, Alertmanager, and CLI tooling at Prometheus, Loki, and Tempo. It parses each upstream query language with the project's own reference parser, lowers the result into a shared plan IR, applies a rule-based optimizer, and emits ClickHouse SQL.

**Status:** v0.1 seed in progress.

## Quick start

```sh
direnv allow                # loads .envrc (Go toolchain on PATH)
just install-tools          # one-time: golangci-lint, gofumpt, goimports
just ci                     # lint + test + build
```

## Architecture (one paragraph)

```
<QL>  →  upstream parser  →  per-QL HL IR  →  shared chplan IR
                                                      ↓
                                               rule-based optimizer
                                                      ↓
                                               ClickHouse SQL  →  CH
```

Each head (PromQL, LogQL, TraceQL) imports the canonical Go parser maintained by its upstream project. All three lower into one shared `internal/chplan` package — a tree of relational operators — over which a small set of rewrite rules runs to a fixpoint. The `internal/chsql` emitter walks the optimized plan and serializes ClickHouse SQL. The HTTP layer emulates the Prom/Loki/Tempo APIs so Grafana sees cerberus as three drop-in datasources.

Schema defaults to the [OpenTelemetry ClickHouse Exporter](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/exporter/clickhouseexporter) layout (`otel_metrics_*`, `otel_logs`, `otel_traces`), with a thin YAML config to override table and column names for SigNoz or custom setups.

## License

MIT — see [LICENSE](LICENSE).
