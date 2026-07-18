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

```text
      WRITE SIDE                      READ SIDE
                             (PromQL · LogQL · TraceQL)
  ┌────────────────┐         ┌──────────┐    ┌─────────┐
  │ OTel Collector │         │ cerberus │◀───│ Grafana │
  └───────┬────────┘         └────┬─────┘    └─────────┘
          │ writes                │ reads
          │    ┌─────────────┐    │
          └───▶│ ClickHouse  │◀───┘
               └─────────────┘
```

**Cerberus does not ingest or store anything.** Your OpenTelemetry
Collector already writes telemetry into ClickHouse through its ClickHouse
exporter; cerberus only reads it back. So you do **not** point Promtail, an
OTel agent, or any other writer at cerberus — those keep writing straight
to ClickHouse exactly as they do now. Cerberus sits on the read side only.

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
Cerberus supplies the missing **query side**, so you get one backend
without giving up the query languages and tooling you already use.

- **No Grafana plugin.** Cerberus speaks each upstream HTTP API verbatim
  (`/api/v1/query_range`, `/loki/api/v1/query_range`, `/api/search`, …),
  so Grafana sees three ordinary datasources.
- **No new query language.** PromQL, LogQL, and TraceQL — exactly as your
  dashboards and alerts already write them.
- **Faithful parsers.** PromQL is parsed by the upstream Apache
  `prometheus/promql/parser`. LogQL and TraceQL are parsed by cerberus's
  own clean-room Apache reimplementations of the published grammars
  (`internal/logql/lsyntax`, `internal/traceql/ast`), checked against the
  real Grafana parsers in testing. If upstream parses a query, so does
  cerberus — without linking Grafana's AGPL-licensed code into the binary.

## How it works

ClickHouse holds your telemetry in the **standard OpenTelemetry ClickHouse
schema** — one table per signal, not one giant table:

- traces in `otel_traces`,
- logs in `otel_logs`,
- metrics split by type across `otel_metrics_gauge`, `otel_metrics_sum`,
  `otel_metrics_histogram`, `otel_metrics_exponential_histogram`, and
  `otel_metrics_summary`.

This is the layout the OpenTelemetry Collector's ClickHouse exporter writes
by default. Cerberus reads those tables; it never creates or writes them.
If your column layout differs from the exporter defaults, point cerberus at
it with `CERBERUS_SCHEMA_*` overrides — see the
[configuration reference](docs/configuration.md#schema-overrides-and-prometheus-resource-labels).

Each query takes one path: the head parses its language and lowers it to a
shared plan, a small optimizer rewrites that plan, and cerberus emits
parameterised ClickHouse SQL and streams the result back in the upstream
wire format. There is **one** pipeline behind all three heads, so a new
optimization costs one implementation, not three. The full breakdown is in
[`docs/engine.md`](docs/engine.md); the performance strategy is in
[`docs/performance.md`](docs/performance.md).

> **Rate-over-range is exact by default.** `rate(…)` range queries match
> reference Prometheus bit-for-bit and stay sub-second at realistic scale.
> For million-row queries an experimental native ClickHouse path
> (`timeSeriesRateToGrid`) trades a sub-observable last-bit rounding
> difference for flat memory and an order-of-magnitude speed-up — see the
> [exactness-vs-scale tradeoff guide](docs/performance.md#native-rate-exactness-vs-scale-should-i-enable-it).

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

## Version requirements

Two things decide whether a deployment is compatible: the **ClickHouse
server version** cerberus queries, and the **schema shape** your telemetry
was written in.

| Component            | Minimum                        | Notes                                                               |
| -------------------- | ------------------------------ | ------------------------------------------------------------------- |
| ClickHouse           | **24.8**                       | The supported floor — the SQL cerberus emits is correct down to it. |
| OTel exporter schema | **clickhouseexporter 0.152.0** | A table layout, not a binary version — any matching writer works.   |

**ClickHouse.** 24.8 is the lowest version cerberus's emitted SQL is
correct on; a query that runs on 24.8 runs on every newer server too. The
differential compatibility harnesses execute on ClickHouse 25.8, so the
validated SQL is exercised forward of the floor as well. On modern
ClickHouse, the optimization auto-picker
(`CERBERUS_CH_OPTIMIZATIONS=auto`, the default) probes the server version
once at startup and turns on result-equivalent optimizations it supports —
`aggregation_in_order` (24.8+), `condition_cache` (25.3+), and the native
`timeSeries*ToGrid` aggregates (25.9+). Those native aggregates are
validated result-correct at flat memory but kept behind an "experimental"
label; `auto` selects them by version, so eligible
`rate(<counter>[range])` range queries lower to the compiled
`timeSeriesRateToGrid` aggregate on 25.9+ and emit the 24.8-safe SQL
unchanged below it. The one opt-in-only feature is
`columnar_result_decode` (a perf tradeoff `auto` never selects). See
[`docs/clickhouse-optimizations.md`](docs/clickhouse-optimizations.md) and
[`docs/operations.md`](docs/operations.md#native-rate-timeseriesratetogrid--auto-enabled-on-259)
for the runtime contract.

**OTel schema — the shape, not the exporter.** Cerberus reads the standard
OpenTelemetry ClickHouse schema, pinned to the `clickhouseexporter`
**v0.152.0** table layout (via the `tsouza/…:cerberus-ddl` fork in
[`go.mod`](go.mod)). What matters is the column names, types, and `Map`
shapes — not which binary wrote them. Any exporter, collector pipeline, or
other path that produces that layout works. If yours differs, point
cerberus at it with the `CERBERUS_SCHEMA_*` overrides — see the
[configuration reference](docs/configuration.md#schema-overrides-and-prometheus-resource-labels).

## Compatibility

The three `*QL compat` badges at the top are **parity scores** —
`passed / total` cases where cerberus returned the same answer as a
reference Prometheus / Loki / Tempo on the same seeded data. Here is how
they are measured.

Each query language has a **differential harness**: cerberus and a
reference engine answer the same corpus against the same seeded data, and
the responses are diffed case-for-case — pinning observed behaviour on
real ClickHouse against an upstream oracle, not just the emitted SQL.

The strongest leg is **PromQL**, which runs the third-party **PromQL
Compliance Tester** (`prometheus/compliance`, the PromLabs / CNCF
Prometheus Conformance Program tooling) against a real `prom/prometheus`,
seeded identically on both sides via remote-write. **718/718 cases pass,
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
_hard-fails on any parity diff_ is `compatibility/prometheus-forced-route`
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

Cerberus is tested at 13 layers, from parser and plan checks through
emitted-SQL goldens and query roundtrips on real ClickHouse, the
differential harnesses above, end-to-end Grafana flows, chaos and
leak detectors, performance guards, and an oracle-based property
framework.
`just test` runs the core lanes; see
[`docs/test-strategy.md`](docs/test-strategy.md) for the canonical layer
map, the CI-gate inventory, and the gremlins rollout.

## Documentation

| Doc                                                                      | What's in it                                                                                                                  |
| ------------------------------------------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------- |
| [`docs/engine.md`](docs/engine.md)                                       | The shared query pipeline, the `Lang` contract, and the per-stage breakdown.                                                  |
| [`docs/coverage.md`](docs/coverage.md)                                   | Per-function / per-construct support status across PromQL / LogQL / TraceQL.                                                  |
| [`docs/configuration.md`](docs/configuration.md)                         | The full `CERBERUS_*` environment-variable reference, grouped by area, with types and defaults.                               |
| [`docs/operations.md`](docs/operations.md)                               | Runtime contract: lifecycle, scaling, the solver and experimental knobs in context.                                           |
| [`docs/performance.md`](docs/performance.md)                             | The compute-fan-out strategy, per-layer optimisations, and how they're held against regression.                               |
| [`docs/optimization-rules.md`](docs/optimization-rules.md)               | The standing optimizer-design rules (feature-registry single-source-of-truth, clone-less-not-faster).                         |
| [`docs/clickhouse-optimizations.md`](docs/clickhouse-optimizations.md)   | The ClickHouse-optimization suite: feature registry, version gating, the runtime probe, and the query_log corpus reconciler.  |
| [`docs/solver.md`](docs/solver.md)                                       | The sharded-pushdown solver: eligibility, slicing, execution, and the cancellation contract.                                  |
| [`docs/router-rules.md`](docs/router-rules.md)                           | The offline router-rules catalog: generic drivers in the repo, per-deployment thresholds resolved from the corpus at runtime. |
| [`docs/native-clickhouse.md`](docs/native-clickhouse.md)                 | What native ClickHouse capability cerberus uses today, and why we don't upstream aggregates (the upstream positioning).       |
| [`docs/benchmarks.md`](docs/benchmarks.md)                               | Benchmark methodology and the recorded numbers (regenerable).                                                                 |
| [`docs/compatibility.md`](docs/compatibility.md)                         | The differential-harness playbook for all three heads.                                                                        |
| [`docs/test-strategy.md`](docs/test-strategy.md)                         | The 13-layer test map and CI-gate inventory.                                                                                  |
| [`docs/observability.md`](docs/observability.md)                         | Self-observability across logs / metrics / traces (OTLP export).                                                              |
| [`docs/health.md`](docs/health.md)                                       | `/readyz` / `/healthz` probe semantics.                                                                                       |
| [`docs/upstream-forks.md`](docs/upstream-forks.md)                       | The `tsouza/*` parser-fork + Dependabot-watch flow.                                                                           |
| [`docs/forbid-skip.md`](docs/forbid-skip.md)                             | The forbidden-pattern reference for the `forbid-skip` gate.                                                                   |

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
