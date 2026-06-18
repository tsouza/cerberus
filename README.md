# cerberus

**Drop-in Prometheus / Loki / Tempo HTTP gateway for ClickHouse.**
Keep Grafana, alerting, and your CLI tooling. Swap the backend.

> [!NOTE]
> **1.0.0 — stable wire API, young project.** The Prometheus / Loki /
> Tempo HTTP surfaces cerberus serves are its 1.0 compatibility contract
> and follow semantic versioning from here. Behaviour is held to reference
> Prometheus / Loki / Tempo by differential harnesses on every merge
> (PromQL at **574/574** on the CNCF compliance tester). It's a confident
> 1.0 on behaviour — but a young, actively-developed project, so evaluate
> it against your own corpus before production, and expect TraceQL to carry
> the lightest conformance confidence of the three heads
> ([details](#compatibility)). See [`CHANGELOG.md`](CHANGELOG.md) for what
> has landed.

[![CI](https://github.com/tsouza/cerberus/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/tsouza/cerberus/actions/workflows/ci.yml)
[![Mutation](https://github.com/tsouza/cerberus/actions/workflows/mutation.yml/badge.svg?branch=main)](https://github.com/tsouza/cerberus/actions/workflows/mutation.yml)
[![License: Apache 2.0](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Go Reference](https://pkg.go.dev/badge/github.com/tsouza/cerberus.svg)](https://pkg.go.dev/github.com/tsouza/cerberus)
[![Go Report Card](https://goreportcard.com/badge/github.com/tsouza/cerberus)](https://goreportcard.com/report/github.com/tsouza/cerberus)
[![PromQL compat](https://img.shields.io/endpoint?url=https%3A%2F%2Fraw.githubusercontent.com%2Ftsouza%2Fcerberus%2Fcompat-scores%2Fbadges%2Fprometheus.json)](https://github.com/tsouza/cerberus/actions/workflows/compatibility.yml)
[![LogQL compat](https://img.shields.io/endpoint?url=https%3A%2F%2Fraw.githubusercontent.com%2Ftsouza%2Fcerberus%2Fcompat-scores%2Fbadges%2Floki.json)](https://github.com/tsouza/cerberus/actions/workflows/compatibility.yml)
[![TraceQL compat](https://img.shields.io/endpoint?url=https%3A%2F%2Fraw.githubusercontent.com%2Ftsouza%2Fcerberus%2Fcompat-scores%2Fbadges%2Ftempo.json)](https://github.com/tsouza/cerberus/actions/workflows/compatibility.yml)

The three `*QL compat` badges are **differential parity scores** —
`passed / total` cases where cerberus matched a reference Prometheus /
Loki / Tempo on the same seeded corpus ([details](#compatibility)). The
PromQL leg runs the third-party
[PromLabs / CNCF **PromQL Compliance Tester**](https://github.com/prometheus/compliance)
(`prometheus/compliance`) — the same tool the CNCF Prometheus Conformance
Program uses — at **574/574 cases passing, no allow-list**, against a real
`prom/prometheus`. The scores are tracked, not gated: see
[Compatibility](#compatibility) for exactly what the CI checks enforce.

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
the floor as well. The ClickHouse-optimization auto-picker
(`CERBERUS_CH_OPTIMIZATIONS=auto`, the default) probes the connected server's
version once at startup and enables the stable, result-equivalent optimizations
it supports — `aggregation_in_order` (24.8+) and `condition_cache` (25.3+) —
while never auto-enabling experimental paths. Enabling the experimental
native-rate path (list `ts_grid_range`, or the deprecated
`CERBERUS_EXPERIMENTAL_TS_GRID_RANGE` alias, **default off**) **raises** the
floor to **25.6**: it lowers eligible `rate(<counter>[range])` range queries to
the compiled `timeSeriesRateToGrid` aggregate, which exists only from ClickHouse
25.6. With it off, 24.8 is sufficient. See
[`docs/clickhouse-optimizations.md`](docs/clickhouse-optimizations.md) for the
auto-picker and
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

### Helm (Kubernetes)

A production Helm chart lives in
[`deploy/helm/cerberus`](deploy/helm/cerberus) and is published as an OCI
artifact (cosign-signed, with SLSA provenance):

```sh
helm install cerberus oci://ghcr.io/tsouza/cerberus/charts/cerberus --version <x.y.z> \
  --set clickhouse.addr='{clickhouse:9000}' \
  --set clickhouse.existingSecret=ch-creds
```

The chart is stateless and secure-by-default, with typed
ClickHouse / OTLP / schema / admit blocks plus full escape hatches
(`extraEnv`, sidecars, affinity, ingress, HPA, PDB, NetworkPolicy). See
the [chart README](deploy/helm/cerberus/README.md) for the complete values
reference and a production HA example.

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

Each query language has a **differential harness**: cerberus and a
reference engine answer the same corpus against the same seeded data, and
the responses are diffed case-for-case — pinning *observed semantics on
real ClickHouse* against an upstream oracle, not just emitted SQL.

The strongest leg is **PromQL**, which runs the third-party **PromQL
Compliance Tester** (`prometheus/compliance`, the PromLabs / CNCF
Prometheus Conformance Program tooling) against a real `prom/prometheus`,
seeded identically on both sides via remote-write. **574/574 cases pass,
no allow-list.** LogQL diffs against a real Loki on Grafana's own
`pkg/logql/bench` corpus — solid, but a Grafana bench corpus rather than a
standardised conformance suite. TraceQL is the lighter leg: there is **no
third-party TraceQL conformance suite**, so its corpus is cerberus-owned
(author-written TXTAR), and its numerical confidence is correspondingly
lower than PromQL's.

| Head    | Reference + corpus                                                  | Required check             | Conformance leg                           |
| ------- | ------------------------------------------------------------------- | -------------------------- | ----------------------------------------- |
| PromQL  | real `prom/prometheus` vs `prometheus/compliance` (PromLabs / CNCF) | `compatibility/prometheus` | third-party conformance suite (strongest) |
| LogQL   | real Loki vs `grafana/loki:pkg/logql/bench` corpus                  | `compatibility/loki`       | real-backend diff, Grafana bench corpus   |
| TraceQL | real Tempo vs cerberus-owned TXTAR corpus                           | `compatibility/tempo`      | author-written corpus (lightest)          |

```sh
just compat-all          # or compat-promql / compat-logql / compat-traceql
```

**What the required checks enforce.** The three `compatibility/<head>`
checks run on every PR and **fail on infrastructure breakage** (stack
won't boot, seed fails, report unparseable). Per-case **parity drift is
report-only** by design ([#503](https://github.com/tsouza/cerberus/pull/503)):
it is recorded in `report.json` and rendered into the live `compat-score.json`
badge, but does not turn the required check red. The one lane that
*hard-fails on any parity diff* is `compatibility/prometheus-forced-route`
(`FAIL_ON_DIFF=1`, proving the sharded solver route is byte-identical to
reference Prometheus over the whole corpus) — that lane is informational,
not a required check. The honest reading: the badges are a continuously
re-measured conformance score, not a merge gate on numeric correctness.

**No allow-lists** — every diff against the reference is a real bug to
fix at the source, not an exception to suppress. The full playbook
(per-head drivers, local reproduction, rejection parity, the sole pinned
`upstream-skip-baseline` contract) is in
[`docs/compatibility.md`](docs/compatibility.md).

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

| Doc                                                                      | What's in it                                                                                                                 |
| ------------------------------------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------- |
| [`docs/engine.md`](docs/engine.md)                                       | The shared query pipeline, the `Lang` contract, and the per-stage breakdown.                                                 |
| [`docs/coverage.md`](docs/coverage.md)                                   | Per-function / per-construct support status across PromQL / LogQL / TraceQL.                                                 |
| [`docs/configuration.md`](docs/configuration.md)                         | The full `CERBERUS_*` environment-variable reference, grouped by area, with types and defaults.                              |
| [`docs/operations.md`](docs/operations.md)                               | Runtime contract: lifecycle, scaling, the solver and experimental knobs in context.                                          |
| [`docs/performance.md`](docs/performance.md)                             | The compute-fan-out strategy, per-layer optimisations, and how they're held against regression.                              |
| [`docs/optimization-rules.md`](docs/optimization-rules.md)               | The standing optimizer-design rules (feature-registry single-source-of-truth, clone-less-not-faster).                        |
| [`docs/clickhouse-optimizations.md`](docs/clickhouse-optimizations.md)   | The ClickHouse-optimization suite: feature registry, version gating, the runtime probe, and the query_log corpus reconciler. |
| [`docs/solver.md`](docs/solver.md)                                       | The sharded-pushdown solver: eligibility, slicing, execution, and the cancellation contract.                                 |
| [`docs/native-clickhouse-roadmap.md`](docs/native-clickhouse-roadmap.md) | The research roadmap for shipping heavy lowerings as native `timeSeries*ToGrid` ClickHouse aggregates.                       |
| [`docs/benchmarks.md`](docs/benchmarks.md)                               | Benchmark methodology and the recorded numbers (regenerable).                                                                |
| [`docs/compatibility.md`](docs/compatibility.md)                         | The differential-harness playbook for all three heads.                                                                       |
| [`docs/test-strategy.md`](docs/test-strategy.md)                         | The 13-layer test map and CI-gate inventory.                                                                                 |
| [`docs/observability.md`](docs/observability.md)                         | Self-observability across logs / metrics / traces (OTLP export).                                                             |
| [`docs/health.md`](docs/health.md)                                       | `/readyz` / `/healthz` probe semantics.                                                                                      |
| [`docs/upstream-forks.md`](docs/upstream-forks.md)                       | The `tsouza/*` parser-fork + Dependabot-watch flow.                                                                          |
| [`docs/forbid-skip.md`](docs/forbid-skip.md)                             | The forbidden-pattern reference for the `forbid-skip` gate.                                                                  |

## Contributing

Smaller PRs (a new optimizer rule, a TXTAR fixture, a parser-dep bump)
are welcome any time; open an issue or discussion before a large one. The
local-dev and end-to-end commands live in
[`CONTRIBUTING.md`](CONTRIBUTING.md).

## License

[Apache 2.0](LICENSE) © Thiago Souza.
