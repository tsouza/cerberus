# cerberus

**Drop-in Prometheus / Loki / Tempo HTTP gateway for ClickHouse.**
Keep Grafana, alerting, and your CLI tooling. Swap the backend.

> [!WARNING]
> **RELEASE CANDIDATE — NOT YET GA.** Cerberus is on the `v1.0.0` release-
> candidate line (latest tag `v1.0.0-RC3`): the surface is feature-complete
> for 1.0 and the differential harnesses gate every merge, but correctness,
> performance, and operational hardening are still being burned down.
> Evaluate it against your own corpus before standing it in for a running
> Prom / Loki / Tempo deployment, and expect breaking changes between
> release candidates. See [`CHANGELOG.md`](CHANGELOG.md) for the per-release
> breakdown.

[![CI](https://github.com/tsouza/cerberus/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/tsouza/cerberus/actions/workflows/ci.yml)
[![Mutation](https://github.com/tsouza/cerberus/actions/workflows/mutation.yml/badge.svg?branch=main)](https://github.com/tsouza/cerberus/actions/workflows/mutation.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Go Reference](https://pkg.go.dev/badge/github.com/tsouza/cerberus.svg)](https://pkg.go.dev/github.com/tsouza/cerberus)
[![Go Report Card](https://goreportcard.com/badge/github.com/tsouza/cerberus)](https://goreportcard.com/report/github.com/tsouza/cerberus)
[![PromQL compat](https://img.shields.io/endpoint?url=https%3A%2F%2Fraw.githubusercontent.com%2Ftsouza%2Fcerberus%2Fcompat-scores%2Fbadges%2Fprometheus.json)](https://github.com/tsouza/cerberus/actions/workflows/compatibility.yml)
[![LogQL compat](https://img.shields.io/endpoint?url=https%3A%2F%2Fraw.githubusercontent.com%2Ftsouza%2Fcerberus%2Fcompat-scores%2Fbadges%2Floki.json)](https://github.com/tsouza/cerberus/actions/workflows/compatibility.yml)
[![TraceQL compat](https://img.shields.io/endpoint?url=https%3A%2F%2Fraw.githubusercontent.com%2Ftsouza%2Fcerberus%2Fcompat-scores%2Fbadges%2Ftempo.json)](https://github.com/tsouza/cerberus/actions/workflows/compatibility.yml)

The three `*QL compat` badges are **differential parity scores** —
`passed / total` cases where cerberus matched a reference Prometheus /
Loki / Tempo on the same seeded corpus ([details](#compatibility)).

---

## Why cerberus?

Metrics, logs, and traces rarely share a store — the usual answer is
Prometheus + Loki + Tempo, three retention policies and storage bills for
what is largely the same OTLP data sliced three ways. ClickHouse is a
great single store for all three signals; cerberus supplies the missing
**query side**. Point Grafana at it as three datasources and your
existing PromQL / LogQL / TraceQL keeps working, translated to ClickHouse
SQL underneath.

- **No Grafana plugin.** Cerberus speaks each upstream HTTP API verbatim
  (`/api/v1/query_range`, `/loki/api/v1/query_range`, `/api/search`, …).
  Grafana sees three normal datasources.
- **No custom QL.** PromQL, LogQL, TraceQL — exactly as your dashboards
  and alerts already use them.
- **No reinvented parsers.** Cerberus imports `prometheus/promql/parser`,
  `grafana/loki/v3/pkg/logql/syntax`, and `grafana/tempo/pkg/traceql`
  directly. If upstream parses it, cerberus parses it.

## Version requirements

Two axes decide whether a deployment is compatible: the **ClickHouse server
version** cerberus queries, and the **OTel schema shape** the data was written
in.

| Component            | Minimum                        | Notes                                                                                                                               |
| -------------------- | ------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------- |
| ClickHouse           | **24.8**                       | The supported floor — the SQL cerberus emits is correct down to it. Enabling the experimental native rate requires 25.6 (below).    |
| OTel exporter schema | **clickhouseexporter 0.152.0** | A **schema shape**, not a binary version — see below.                                                                               |

**ClickHouse.** 24.8 is the lowest version cerberus's emitted SQL is correct on:
the 24.8 empty-input / parse-unit / filter-path quirks are all worked around
unconditionally, so a query that runs on 24.8 runs on every newer server too.
The differential compatibility harnesses — the source of truth for all three
heads — execute on ClickHouse 25.8, so the validated SQL is exercised forward of
the floor as well. Enabling the experimental native-rate path
(`CERBERUS_EXPERIMENTAL_TS_GRID_RANGE`, **default off**) **raises** the floor to
**25.6**: it lowers eligible `rate(<counter>[range])` range queries to the
compiled `timeSeriesRateToGrid` aggregate, which exists only from ClickHouse
25.6. With the flag off, 24.8 is sufficient. See
[`docs/operations.md`](docs/operations.md#experimental-native-rate-timeseriesratetogrid)
for the runtime contract and the experimental-setting details.

**OTel schema — the shape, not the exporter.** Cerberus reads the
**OpenTelemetry ClickHouse schema shape** pinned to `clickhouseexporter`
**v0.152.0** (via the `tsouza/…:cerberus-ddl` fork in
[`go.mod`](go.mod)). What matters is the **table layout** — column names,
types, and `Map` shapes — not which binary produced it. Any exporter, collector
pipeline, or ingestion path that writes tables in that shape works; the exporter
binary version itself is irrelevant. If your layout deviates from the exporter
defaults, point cerberus at it with the `CERBERUS_SCHEMA_*` overrides — see
[`docs/configuration.md`](docs/configuration.md#schema).

## Quick start

```sh
git clone https://github.com/tsouza/cerberus.git && cd cerberus
docker compose up --wait
open http://localhost:3000   # Grafana (auto-login as admin); cerberus on :8080
```

That builds cerberus, boots single-node ClickHouse, loads a deterministic
OTel fixture (logs / traces / metrics), and brings up Grafana
pre-provisioned with cerberus as three datasources. A fresh dashboard
populates in ~30s; `docker compose down -v` wipes the volume.

### From a published release

Cerberus is one stateless binary configured via environment variables.
Pin an explicit tag — `:latest` only moves with stable releases:

```sh
docker pull ghcr.io/tsouza/cerberus:<tag>
docker run --rm -p 8080:8080 -e CERBERUS_CH_ADDR=clickhouse:9000 \
  ghcr.io/tsouza/cerberus:<tag>
```

Prebuilt binaries (linux / darwin × amd64 / arm64) are on the
[release page](https://github.com/tsouza/cerberus/releases); each release
ships a [SLSA build provenance](https://slsa.dev) attestation:

```sh
gh attestation verify cerberus_*_linux_amd64.tar.gz --owner tsouza --repo cerberus
```

Cerberus is configured **entirely** through `CERBERUS_*` environment
variables — see the full [configuration reference](docs/configuration.md).
The surrounding runtime contract (lifecycle, scaling, the solver and
experimental knobs in context) lives in
[`docs/operations.md`](docs/operations.md).

## Architecture

Cerberus has **one** query pipeline, not three. Each head parses with its
reference upstream parser and lowers to a shared plan IR
([`internal/chplan`](internal/chplan)); a rule-based optimiser rewrites
it; the closed typed-Frag [`internal/chsql`](internal/chsql) emitter
produces parameterised, escape-free ClickHouse SQL; and the engine
streams results. The three HTTP heads plug in as thin `Lang` adapters
over [`internal/engine`](internal/engine), so the optimiser and emitter
never know which head produced a plan — **new optimisations cost one
implementation, not three**.

See [`docs/engine.md`](docs/engine.md) for the `Lang` contract, the
request lifecycle, and the per-stage breakdown (IR algebra, optimiser
rules, the typed-SQL emitter, the OTel schema). For how cerberus keeps
queries fast — the compute-fan-out strategy and per-layer optimisations —
see [`docs/performance.md`](docs/performance.md).

> **Rate-over-range is exact by default.** `rate(…)` range queries match
> reference Prometheus bit-for-bit and stay sub-second at realistic scale.
> For million-row queries an experimental native ClickHouse path
> (`timeSeriesRateToGrid`) trades a sub-observable last-bit rounding
> difference for flat memory and an order-of-magnitude speed-up — see the
> [exactness-vs-scale tradeoff guide](docs/performance.md#native-rate-exactness-vs-scale-should-i-enable-it).

## Compatibility

Each query language is gated by a **differential harness**: cerberus and
a reference engine answer the same corpus against the same seeded data,
and the responses are diffed case-for-case — pinning *observed semantics
on real ClickHouse* against an upstream oracle, not just emitted SQL.

| Head    | Reference + corpus                                          | Required gate              |
| ------- | ----------------------------------------------------------- | -------------------------- |
| PromQL  | bundled Prometheus engine vs `prometheus/compliance` corpus | `compatibility/prometheus` |
| LogQL   | reference Loki vs `grafana/loki:pkg/logql/bench` corpus     | `compatibility/loki`       |
| TraceQL | reference Tempo vs cerberus-owned TXTAR corpus              | `compatibility/tempo`      |

```sh
just compat-all          # or compat-promql / compat-logql / compat-traceql
```

**No allow-lists** — every diff against the reference is a real bug to
fix at the source. The full playbook (per-head drivers, local
reproduction, rejection parity, the sole pinned `upstream-skip-baseline`
contract) is in [`docs/compatibility.md`](docs/compatibility.md).

## Testing

Cerberus is tested in a 13-layer map spanning AST-shape pinning, plan-IR
invariants, optimiser properties, emitted-SQL goldens, chDB roundtrips,
function-surface parity, HTTP wire conformance, differential harnesses,
Playwright UX flows, deterministic chaos / goleak, perf benchmarks +
compute-fan-out guards, live-stack chaos against the k3d deployment, and
an oracle-based property framework.
`just test` runs the core lanes; see
[`docs/test-strategy.md`](docs/test-strategy.md) for the canonical layer
map, the CI-gate inventory, and the gremlins rollout.

## Documentation

| Doc                                                | What's in it                                                                                    |
| -------------------------------------------------- | ----------------------------------------------------------------------------------------------- |
| [`docs/engine.md`](docs/engine.md)                 | The shared query pipeline, the `Lang` contract, and the per-stage breakdown.                    |
| [`docs/coverage.md`](docs/coverage.md)             | Per-function / per-construct support status across PromQL / LogQL / TraceQL.                    |
| [`docs/configuration.md`](docs/configuration.md)   | The full `CERBERUS_*` environment-variable reference, grouped by area, with types and defaults. |
| [`docs/operations.md`](docs/operations.md)         | Runtime contract: lifecycle, scaling, the solver and experimental knobs in context.             |
| [`docs/performance.md`](docs/performance.md)       | The compute-fan-out strategy, per-layer optimisations, and how they're held against regression. |
| [`docs/solver.md`](docs/solver.md)                 | The sharded-pushdown solver: eligibility, slicing, execution, and the cancellation contract.    |
| [`docs/benchmarks.md`](docs/benchmarks.md)         | Benchmark methodology and the recorded numbers (regenerable).                                   |
| [`docs/compatibility.md`](docs/compatibility.md)   | The differential-harness playbook for all three heads.                                          |
| [`docs/test-strategy.md`](docs/test-strategy.md)   | The 13-layer test map and CI-gate inventory.                                                    |
| [`docs/observability.md`](docs/observability.md)   | Self-observability across logs / metrics / traces (OTLP export).                                |
| [`docs/health.md`](docs/health.md)                 | `/readyz` / `/healthz` probe semantics.                                                         |
| [`docs/upstream-forks.md`](docs/upstream-forks.md) | The `tsouza/*` parser-fork + Dependabot-watch flow.                                             |
| [`docs/forbid-skip.md`](docs/forbid-skip.md)       | The forbidden-pattern reference for the `forbid-skip` gate.                                     |

## Contributing

Smaller PRs (a new optimizer rule, a TXTAR fixture, a parser-dep bump)
are welcome any time; open an issue or discussion before a large one. The
local-dev and end-to-end commands live in
[`CONTRIBUTING.md`](CONTRIBUTING.md).

## License

[MIT](LICENSE) © Thiago Souza.
