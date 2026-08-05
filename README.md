<div align="center">

<picture>
  <source media="(prefers-color-scheme: light)" srcset="https://cerberus.foo/assets/brand/readme-banner-light-1280x640.png">
  <img src="https://cerberus.foo/assets/brand/readme-banner-1280x640.png" alt="cerberus — three query languages, one backend" width="100%">
</picture>

<br>

### Drop-in Prometheus / Loki / Tempo HTTP gateway for ClickHouse

_Keep Grafana, alerting, and your CLI tooling. Swap the backend._

<sub>
<a href="#why-cerberus">Why cerberus</a> &nbsp;·&nbsp;
<a href="#quick-start">Quick start</a> &nbsp;·&nbsp;
<a href="#how-it-works">How it works</a> &nbsp;·&nbsp;
<a href="#migrating-from-prometheus">Migrating</a> &nbsp;·&nbsp;
<a href="#compatibility">Compatibility</a> &nbsp;·&nbsp;
<a href="#documentation">Docs</a>
</sub>

<br><br>

[![License: Apache 2.0](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Go Reference](https://pkg.go.dev/badge/github.com/tsouza/cerberus.svg)](https://pkg.go.dev/github.com/tsouza/cerberus)
[![Ask DeepWiki](https://deepwiki.com/badge.svg)](https://deepwiki.com/tsouza/cerberus)

[![PromQL compat](https://img.shields.io/endpoint?url=https%3A%2F%2Fraw.githubusercontent.com%2Ftsouza%2Fcerberus%2Fcompat-scores%2Fbadges%2Fprometheus.json)](https://github.com/tsouza/cerberus/actions/workflows/compatibility.yml)
[![LogQL compat](https://img.shields.io/endpoint?url=https%3A%2F%2Fraw.githubusercontent.com%2Ftsouza%2Fcerberus%2Fcompat-scores%2Fbadges%2Floki.json)](https://github.com/tsouza/cerberus/actions/workflows/compatibility.yml)
[![TraceQL compat](https://img.shields.io/endpoint?url=https%3A%2F%2Fraw.githubusercontent.com%2Ftsouza%2Fcerberus%2Fcompat-scores%2Fbadges%2Ftempo.json)](https://github.com/tsouza/cerberus/actions/workflows/compatibility.yml)

</div>

Cerberus lets you keep your metrics, logs, and traces in **ClickHouse**
and go on querying them from **Grafana** with **PromQL, LogQL, and
TraceQL** — as if Prometheus, Loki, and Tempo were still doing the work.

It is a **read-only query gateway**. You add it to Grafana as three
datasources (one Prometheus, one Loki, one Tempo). When you run a query,
cerberus translates it into ClickHouse SQL, runs it, and hands back the
normal Prometheus / Loki / Tempo response. Grafana can't tell the
difference, so your existing dashboards and alerts keep working unchanged.

<div align="center">
<table><tr><td><pre>
      WRITE SIDE                      READ SIDE
                             (PromQL · LogQL · TraceQL)
  ┌────────────────┐         ┌──────────┐    ┌─────────┐
  │ OTel Collector │         │ cerberus │◀───│ Grafana │
  └───────┬────────┘         └────┬─────┘    └─────────┘
          │ writes                │ reads
          │    ┌─────────────┐    │
          └───▶│ ClickHouse  │◀───┘
               └─────────────┘
</pre></td></tr></table>
</div>

**Cerberus never ingests or stores anything.** Your OpenTelemetry Collector
already writes telemetry into ClickHouse through its ClickHouse exporter, and
cerberus only reads it back. Writers keep pointing at ClickHouse exactly as
they do today — never at cerberus.

> [!NOTE]
> **1.0 — stable wire API, young project.** The Prometheus / Loki / Tempo
> HTTP surfaces are a versioned 1.0 contract, but the project itself is
> young and moving fast. Try it against your own data before you rely on it
> in production. See [`CHANGELOG.md`](CHANGELOG.md) for what has landed.

---

## Why cerberus?

Metrics, logs, and traces rarely share a store. The usual answer is
Prometheus + Loki + Tempo: three systems, three retention policies, three
storage bills — for what is largely the same OpenTelemetry data sliced
three ways. ClickHouse is a great single store for all three signals.
Cerberus supplies the missing **query side**.

- **No Grafana plugin.** Cerberus speaks each upstream HTTP API verbatim
  (`/api/v1/query_range`, `/loki/api/v1/query_range`, `/api/search`, …),
  so Grafana sees three ordinary datasources.
- **No new query language.** PromQL, LogQL, and TraceQL — exactly as your
  dashboards and alerts already write them.
- **Faithful parsers.** If upstream parses a query, so does cerberus.
  PromQL goes through the upstream Apache `prometheus/promql/parser`; LogQL
  and TraceQL go through cerberus's own clean-room Apache reimplementations
  of the published grammars, checked against the real Grafana parsers in
  testing — so no AGPL-licensed code is linked into the binary.

## How it works

Your telemetry lives in the **standard OpenTelemetry ClickHouse schema** —
one table per signal, not one giant table: `otel_traces`, `otel_logs`, and
metrics split by type across `otel_metrics_gauge`, `otel_metrics_sum`,
`otel_metrics_histogram`, `otel_metrics_exponential_histogram`, and
`otel_metrics_summary`. That is what the Collector's ClickHouse exporter
writes by default. Cerberus reads those tables and never creates or writes
them. Different column layout? Point cerberus at it with the
[`CERBERUS_SCHEMA_*` overrides](docs/configuration.md#schema-overrides-and-prometheus-resource-labels).

Every query takes the same path:

**parse → lower to a shared plan → optimize → ClickHouse SQL → stream back
in the upstream wire format**

One pipeline sits behind all three heads, so a new optimization costs one
implementation instead of three. The full breakdown is in
[`docs/engine.md`](docs/engine.md), the performance strategy in
[`docs/performance.md`](docs/performance.md).

> **Rate-over-range is exact by default.** `rate(…)` range queries match
> reference Prometheus bit-for-bit and stay sub-second at realistic scale.
> For million-row queries an experimental native ClickHouse path
> (`timeSeriesRateToGrid`) trades a sub-observable last-bit rounding
> difference for flat memory and an order-of-magnitude speed-up — see the
> [exactness-vs-scale guide](docs/performance.md#native-rate-exactness-vs-scale-should-i-enable-it).

## Quick start

Kick the tyres locally, no ClickHouse of your own required:

```sh
git clone https://github.com/tsouza/cerberus.git && cd cerberus
docker compose up --wait
open http://localhost:3000   # Grafana (auto-login as admin); cerberus on :8080
```

That builds cerberus, boots a single-node ClickHouse, loads a deterministic
OTel fixture (logs / traces / metrics), and brings up Grafana
pre-provisioned with cerberus as three datasources. A fresh dashboard
populates in ~30s; `docker compose down -v` wipes the volume.

### Install a release

Cerberus is one stateless binary, configured from a `cerberus.yaml` or from
`CERBERUS_*` environment variables — the same settings either way (see
[`docs/configuration.md`](docs/configuration.md)). The runtime contract
around it — lifecycle, scaling, the solver and experimental knobs in
context — is in [`docs/operations.md`](docs/operations.md).

**Docker.** Pin an explicit tag; `:latest` only moves with stable releases.

```sh
docker pull ghcr.io/tsouza/cerberus:<tag>
docker run --rm -p 8080:8080 -e CERBERUS_CH_ADDR=clickhouse:9000 \
  ghcr.io/tsouza/cerberus:<tag>
```

**Homebrew** (macOS and Linuxbrew). The quickest way to get the
`cerberus migrate` CLI onto the machine holding your rules and dashboards —
see [migrating to cerberus](docs/migration.md#step-1-install-the-binary).
Only stable releases publish a cask, so `brew` never hands you a prerelease.

```sh
brew install --cask tsouza/tap/cerberus
```

**Binaries.** linux / darwin × amd64 / arm64 on the
[release page](https://github.com/tsouza/cerberus/releases), each with a
[SLSA build provenance](https://slsa.dev) attestation:

```sh
gh attestation verify cerberus_*_linux_amd64.tar.gz --owner tsouza --repo cerberus
```

**Helm.** A production chart lives in
[`deploy/helm/cerberus`](deploy/helm/cerberus), published as a
cosign-signed OCI artifact with SLSA provenance:

```sh
helm install cerberus oci://ghcr.io/tsouza/cerberus/charts/cerberus --version <x.y.z> \
  --set clickhouse.addr='{clickhouse:9000}' \
  --set clickhouse.existingSecret=ch-creds
```

It is stateless and secure-by-default, with typed
ClickHouse / OTLP / schema / admit blocks plus full escape hatches
(`extraEnv`, sidecars, affinity, ingress, HPA, PDB, NetworkPolicy). The
[chart README](deploy/helm/cerberus/README.md) has the complete values
reference and a production HA example.

## Migrating from Prometheus

Cerberus replaces Prometheus's **storage and query engine — not your
dashboards, alerts, or ruler.** It has no rule engine and never ingests, so
your recording and alerting rules keep being evaluated by whatever evaluates
them today, and the move is ultimately a datasource **URL swap**. You just
have to earn that swap by proving, query by query, that cerberus returns the
same numbers on your own data.

The `migrate` subcommand in the release binary is built for exactly that. It

- **harvests** your real PromQL, LogQL, and TraceQL from rule files and
  exported dashboards,
- **previews** the ClickHouse SQL and schema offline,
- **replays** every metric query against its head's reference backend
  (Prometheus, Loki, Tempo) _and_ against cerberus, then diffs the results,
- **gates** the cutover on all of it, refusing the flip while a single query
  still diverges. Nothing is allow-listed; the gate earns the flip.

**→ Follow the step-by-step operator playbook in
[`docs/migration.md`](docs/migration.md).**

## Version requirements

Two things decide whether a deployment is compatible: the **ClickHouse
server version** cerberus queries, and the **schema shape** your telemetry
was written in.

| Component            | Minimum                        | Notes                                                               |
| -------------------- | ------------------------------ | ------------------------------------------------------------------- |
| ClickHouse           | **24.8**                       | The supported floor — the SQL cerberus emits is correct down to it. |
| OTel exporter schema | **clickhouseexporter 0.152.0** | A table layout, not a binary version — any matching writer works.   |

**ClickHouse 24.8+.** A query that runs on 24.8 runs on every newer server
too, and the differential harnesses execute on ClickHouse 26.5, so the
validated SQL is exercised well forward of the floor.

You do not have to tune for your server version. The optimization
auto-picker (`CERBERUS_CH_OPTIMIZATIONS=auto`, the default) probes it once
at startup and turns on the result-equivalent optimizations it finds:

- `aggregation_in_order` — 24.8+
- `condition_cache` — 25.3+
- the native `timeSeries*ToGrid` aggregates — 25.9+, so an eligible
  `rate(<counter>[range])` range query lowers to the compiled
  `timeSeriesRateToGrid` aggregate there and emits the 24.8-safe SQL
  unchanged below it. Validated result-correct at flat memory, still
  labelled experimental.
- `columnar_result_decode` — opt-in only; a perf tradeoff `auto` never
  selects for you.

[`docs/clickhouse-optimizations.md`](docs/clickhouse-optimizations.md) and
[the runtime contract](docs/operations.md#native-rate-timeseriesratetogrid--auto-enabled-on-259)
have the details.

**The schema shape, not the exporter.** Cerberus reads the standard
OpenTelemetry ClickHouse schema, pinned to the `clickhouseexporter`
**v0.152.0** table layout (via the `tsouza/…:cerberus-ddl` fork in
[`go.mod`](go.mod)). What matters is the column names, types, and `Map`
shapes — not which binary wrote them — so any exporter, collector pipeline,
or other path that produces that layout works. If yours differs, point
cerberus at it with the
[`CERBERUS_SCHEMA_*` overrides](docs/configuration.md#schema-overrides-and-prometheus-resource-labels).

## Compatibility

The three `*QL compat` badges at the top are **parity scores**:
`passed / total` cases where cerberus returned the same answer as a real
Prometheus / Loki / Tempo on the same seeded data. Each head has a
**differential harness** that answers one corpus with both engines and diffs
the responses case-for-case — pinning observed behaviour on real ClickHouse
against an upstream oracle, not just the emitted SQL.

| Head    | Reference + corpus                                                  | Required check             | Conformance leg                           |
| ------- | ------------------------------------------------------------------- | -------------------------- | ----------------------------------------- |
| PromQL  | real `prom/prometheus` vs `prometheus/compliance` (PromLabs / CNCF) | `compatibility/prometheus` | third-party conformance suite (strongest) |
| LogQL   | real Loki vs `grafana/loki:pkg/logql/bench` corpus                  | `compatibility/loki`       | real-backend diff, Grafana bench corpus   |
| TraceQL | real Tempo vs cerberus-owned TXTAR corpus                           | `compatibility/tempo`      | author-written corpus (lightest)          |

PromQL is the strongest leg: the third-party **PromQL Compliance Tester**
(PromLabs / CNCF Prometheus Conformance Program tooling) against a real
`prom/prometheus`, seeded identically on both sides via remote-write —
**745/745 cases pass, no allow-list.** LogQL is solid but measured on a
Grafana bench corpus rather than a standardised conformance suite. TraceQL
is the lightest leg: no third-party TraceQL conformance suite exists, so its
corpus is author-written TXTAR and its numerical confidence is
correspondingly lower than PromQL's.

```sh
just compat-all          # or compat-promql / compat-logql / compat-traceql
```

**No allow-lists.** Every diff against the reference is a real bug to fix at
the source, not an exception to suppress. The full playbook — per-head
drivers, local reproduction, rejection parity, the sole pinned
`upstream-skip-baseline` contract — is in
[`docs/compatibility.md`](docs/compatibility.md).

<details>
<summary><b>How those badges double as a merge gate</b></summary>

<br>

The three `compatibility/<head>` checks run on every PR in two layers.

The harness itself is _scored_
([#503](https://github.com/tsouza/cerberus/pull/503)): it accumulates
per-case results into `report.json` / `compat-score.json` plus a per-case
roster in `compat-cases.json`, and exits 0 even when a case diverges — so
the harness step alone reddens the job only on infrastructure breakage
(stack won't boot, seed fails, report unparseable).

The gate is the step after it.
[`compat-ratchet.mjs`](.github/scripts/compat-ratchet.mjs) compares the
run's roster against the committed one in
[`compatibility/parity-baseline.json`](compatibility/parity-baseline.json)
and **fails the required job on any case that moved**: a recorded case that
now diverges, one that stopped running, or a new case that either diverges
on arrival or passes without being recorded. Gating on case identity rather
than a count means a regression cannot hide behind an unrelated case that
started passing in the same run, and moving a roster is a deliberate
same-PR edit to the baseline file.

`compatibility/prometheus-forced-route` goes further: `FAIL_ON_DIFF=1`
hard-fails inside the harness on _any_ per-case diff, which is what proves
the sharded solver route is byte-identical to reference Prometheus over the
whole corpus.

So each head badge is both a continuously re-measured conformance score and
— through the ratchet floor — a merge gate.
[`docs/compatibility.md`](docs/compatibility.md#parity-regression-ratchet-the-gate)
is the canonical reference.

</details>

## Testing

Cerberus is tested at 14 layers: parser and plan checks, emitted-SQL
goldens, query roundtrips on real ClickHouse, the differential harnesses
above, end-to-end Grafana flows, chaos and leak detectors, performance
guards, and an oracle-based property framework. `just test` runs the core
lanes; [`docs/test-strategy.md`](docs/test-strategy.md) is the canonical
layer map and CI-gate inventory.

## Documentation

### Using cerberus

| Doc                                                          | What's in it                                                                                                                |
| ------------------------------------------------------------ | --------------------------------------------------------------------------------------------------------------------------- |
| [`migration.md`](docs/migration.md)                          | Operator playbook for moving off Prometheus: assess your queries, verify parity against both backends, then flip over.      |
| [`migration-reference.md`](docs/migration-reference.md)      | The contract behind that playbook: every `migrate` flag, what each comparator calls equal, what each gate blocks on.        |
| [`configuration.md`](docs/configuration.md)                  | Every setting, grouped by area, with types and defaults — as a `CERBERUS_*` variable or the equivalent `cerberus.yaml` key. |
| [`operations.md`](docs/operations.md)                        | Runtime contract: lifecycle, scaling, the solver and experimental knobs in context.                                         |
| [`coverage.md`](docs/coverage.md)                            | Per-function / per-construct support status across PromQL / LogQL / TraceQL.                                                |
| [`observability.md`](docs/observability.md)                  | Self-observability across logs / metrics / traces (OTLP export).                                                            |
| [`health.md`](docs/health.md)                                | `/readyz` / `/healthz` probe semantics.                                                                                     |

### How it works inside

| Doc                                                               | What's in it                                                                                                                  |
| ----------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------- |
| [`engine.md`](docs/engine.md)                                     | The shared query pipeline, the `Lang` contract, and the per-stage breakdown.                                                  |
| [`performance.md`](docs/performance.md)                           | The compute-fan-out strategy, per-layer optimisations, and how they're held against regression.                               |
| [`clickhouse-optimizations.md`](docs/clickhouse-optimizations.md) | The ClickHouse-optimization suite: feature registry, version gating, the runtime probe, the query_log corpus reconciler.      |
| [`solver.md`](docs/solver.md)                                     | The sharded-pushdown solver: eligibility, slicing, execution, and the cancellation contract.                                  |
| [`router-rules.md`](docs/router-rules.md)                         | The offline router-rules catalog: generic drivers in the repo, per-deployment thresholds resolved from the corpus at runtime. |
| [`native-clickhouse.md`](docs/native-clickhouse.md)               | What native ClickHouse capability cerberus uses today, and why we don't upstream aggregates.                                  |
| [`benchmarks.md`](docs/benchmarks.md)                             | Benchmark methodology and the recorded numbers (regenerable).                                                                 |

### Working on cerberus

| Doc                                                        | What's in it                                                                                          |
| ---------------------------------------------------------- | ----------------------------------------------------------------------------------------------------- |
| [`test-strategy.md`](docs/test-strategy.md)                | The 14-layer test map and CI-gate inventory.                                                          |
| [`compatibility.md`](docs/compatibility.md)                | The differential-harness playbook for all three heads.                                                |
| [`optimization-rules.md`](docs/optimization-rules.md)      | The standing optimizer-design rules (feature-registry single-source-of-truth, clone-less-not-faster). |
| [`upstream-forks.md`](docs/upstream-forks.md)              | The `tsouza/*` parser-fork + Dependabot-watch flow.                                                   |
| [`forbid-skip.md`](docs/forbid-skip.md)                    | The forbidden-pattern reference for the `forbid-skip` gate.                                           |

## Contributing

Smaller PRs (a new optimizer rule, a TXTAR fixture, a parser-dep bump)
are welcome any time; open an issue or discussion before a large one. The
local-dev and end-to-end commands live in
[`CONTRIBUTING.md`](CONTRIBUTING.md).

## License

[Apache 2.0](LICENSE) © Thiago Souza.

Cerberus's LogQL and TraceQL parsers are clean-room reimplementations of the
published language grammars, API-compatible with Grafana Loki / Tempo but not
derived from their AGPLv3 source. Third-party attributions and the clean-room
statement are in [`NOTICE`](NOTICE). Cerberus is not affiliated with or endorsed
by Grafana Labs, Prometheus, or the OpenTelemetry project.
