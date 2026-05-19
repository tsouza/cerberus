# cerberus

> Drop-in **Prometheus / Loki / Tempo** HTTP gateway for **ClickHouse**.
> Keep Grafana, alerting, and your CLI tooling. Swap the backend.

> [!WARNING]
> ## ALPHA — NOT PRODUCTION-READY
>
> Cerberus is **alpha quality**. `v1.0.0-RC3` is the latest tag; **RC4
> through the rest of the `v1.0.0` roadmap is still owed** —
> correctness, performance, and operational hardening. Two of the three
> compatibility harnesses (LogQL, TraceQL) are **mid-rollout and not yet
> gating PRs**. Treat this as a preview for evaluation and experiments,
> **not** a drop-in replacement for a running Prom / Loki / Tempo
> deployment. Expect breaking changes between RC tags.

[![CI](https://github.com/tsouza/cerberus/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/tsouza/cerberus/actions/workflows/ci.yml)
[![Mutation](https://github.com/tsouza/cerberus/actions/workflows/mutation.yml/badge.svg?branch=main)](https://github.com/tsouza/cerberus/actions/workflows/mutation.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Go Reference](https://pkg.go.dev/badge/github.com/tsouza/cerberus.svg)](https://pkg.go.dev/github.com/tsouza/cerberus)
[![Go Report Card](https://goreportcard.com/badge/github.com/tsouza/cerberus)](https://goreportcard.com/report/github.com/tsouza/cerberus)
[![PromQL compat](https://img.shields.io/endpoint?url=https%3A%2F%2Fraw.githubusercontent.com%2Ftsouza%2Fcerberus%2Fcompat-scores%2Fbadges%2Fprometheus.json)](https://github.com/tsouza/cerberus/actions/workflows/compatibility.yml)
[![LogQL compat](https://img.shields.io/endpoint?url=https%3A%2F%2Fraw.githubusercontent.com%2Ftsouza%2Fcerberus%2Fcompat-scores%2Fbadges%2Floki.json)](https://github.com/tsouza/cerberus/actions/workflows/compatibility.yml)
[![TraceQL compat](https://img.shields.io/endpoint?url=https%3A%2F%2Fraw.githubusercontent.com%2Ftsouza%2Fcerberus%2Fcompat-scores%2Fbadges%2Ftempo.json)](https://github.com/tsouza/cerberus/actions/workflows/compatibility.yml)

The three `*QL compat` badges above are **differential parity scores** —
`passed / total` cases where cerberus's response matched a reference
Prometheus / Loki / Tempo on the same seeded corpus. See
[Compatibility harnesses](#compatibility-harnesses) for what each
corpus is and what each score actually measures.

---

## Why cerberus?

Metrics, logs, and traces almost never live in one store. The usual
answer is Prometheus + Loki + Tempo — three retention policies, three
index strategies, three storage bills, three on-call playbooks. The
duplication is real and it doesn't buy anything: the OTLP payload going
into each store is largely the same data, just sliced for a different
query language.

ClickHouse is a great single store for all three signals. The piece
that has been missing is the **query side**: dashboards, alerts, and
CLIs speak PromQL / LogQL / TraceQL, not SQL. Cerberus closes that
gap. Point Grafana at cerberus as three datasources (Prometheus, Loki,
Tempo) and the queries you already have keep working — translated into
optimised ClickHouse SQL underneath.

- **No Grafana plugin.** Cerberus speaks each upstream HTTP API verbatim
  (`/api/v1/query_range`, `/loki/api/v1/query_range`, `/api/search`,
  `/api/traces/<id>`, …). Grafana sees three normal datasources.
- **No custom QL.** PromQL, LogQL, TraceQL — exactly as your dashboards
  and alerts already use them.
- **No reinvented parsers.** Cerberus imports
  `prometheus/promql/parser`, `grafana/loki/v3/pkg/logql/syntax`, and
  `grafana/tempo/pkg/traceql` directly. If upstream parses it, cerberus
  parses it.

## Status

**Alpha — `v1.0.0-RC3` is the latest tag.** RC1, RC2, and RC3 have
shipped: PromQL / LogQL / TraceQL parse + lowering, the pattern-based
optimizer (transposes, PREWHERE promotion, late materialisation, MV
substitution), the typed `chsql` SQL emitter, the shared
`internal/engine` pipeline, full self-observability (`slog` + OTel
traces + OTLP-exported metrics), 12-factor packaging (`/readyz`,
admission control, `docker compose up` for one-command local dev), and
the chDB-backed round-trip + property test suites. Upstream parser
shims are retired via the
[`tsouza/tempo:cerberus-accessors`](https://github.com/tsouza/tempo/tree/cerberus-accessors)
fork; the schema source of truth is
[`tsouza/opentelemetry-collector-contrib:cerberus-ddl`](https://github.com/tsouza/opentelemetry-collector-contrib/tree/cerberus-ddl).

**RC4 and the rest of the v1.0.0 roadmap are still owed** — see
[`docs/roadmap.md`](docs/roadmap.md) for what's left and
[`CHANGELOG.md`](CHANGELOG.md) for the per-release breakdown.

## Architecture

Cerberus has **one** query pipeline, not three. Each query language has
its own parser and its own response shape, but the work in the middle —
lowering to a shared plan IR, optimising, emitting ClickHouse SQL,
streaming results — is identical. The three HTTP heads plug in as thin
`Lang` adapters; the engine owns the parse → optimize → emit → execute
loop and the telemetry around it.

```text
   PromQL                LogQL                TraceQL
     │                     │                     │
     ▼                     ▼                     ▼
prometheus/        grafana/loki/v3/         grafana/tempo/
promql/parser      pkg/logql/syntax         pkg/traceql        ← reference upstream parsers, imported directly
     │                     │                     │
     │      per-QL lowering (head → chplan)      │
     ▼                     ▼                     ▼
 ┌──────────────────────────────────────────────────┐
 │           internal/chplan — shared IR            │   Scan • Filter • Project •
 │  one algebra; the optimiser and the emitter      │   Aggregate • RangeWindow •
 │  don't know which head produced the plan         │   Limit • expression tree
 └──────────────────────────────────────────────────┘
                       │
                       ▼
 ┌──────────────────────────────────────────────────┐
 │          internal/optimizer — rule-based         │   Catalyst-style batches:
 │  Analyzer (semantic, idempotent) → Once          │   • constant fold (semantic + heuristic)
 │  (heuristic, idempotent) → FixedPoint(n)         │   • predicate pushdown + filter fusion
 │  (rules that unlock each other; iterates until   │   • filter ↔ project / aggregate /
 │  no rule fires)                                  │     range-window transposes
 │                                                  │   • projection pushdown (late materialisation)
 │                                                  │   • MV substitution (rollup view rewrite)
 └──────────────────────────────────────────────────┘
                       │
                       ▼
 ┌──────────────────────────────────────────────────┐
 │           internal/chsql — typed emitter         │   • parameterised, escape-free
 │  QueryBuilder slots + typed Frag constructors;   │   • PREWHERE promotion on Filter(Scan)
 │  no raw SQL by construction (writeSQL is         │   • sort-key-aware predicate ordering
 │  unexported, chsql.Raw / Concat are gone)        │   • streaming clickhouse-go/v2 cursor
 └──────────────────────────────────────────────────┘
                       │
                       ▼
                  ClickHouse
```

The three heads share one engine — [`internal/engine/`](internal/engine).
Each head implements `Lang` (`Parse` + `ProjectSamples`); the engine
holds the optimiser, the ClickHouse `Querier`, and the OTel spans
around each pipeline stage. See [`docs/engine.md`](docs/engine.md) for
the contract, the request lifecycle, and the extension points.

### One IR for three languages — `internal/chplan`

A small algebra (`Scan`, `Filter`, `Project`, `Aggregate`,
`RangeWindow`, `Limit` + an expression tree) is the meeting point of
all three heads. The optimiser, the SQL emitter, and the engine work
over this IR; they don't know which head produced the plan. **New
optimisations cost one implementation, not three.**

### A real rule-based optimiser — `internal/optimizer`

Catalyst- and DataFusion-style: rules are grouped into batches with
three strategies — `Analyzer` (semantic, must-run, idempotent — panics
on contract violation), `Once` (idempotent heuristics, single pass),
and `FixedPoint(n)` (rules that unlock each other; iterates until no
rule reports a change or `n` iterations have elapsed). The default
pipeline ships:

| Stage                            | Rules                                                                                             | What it buys                                                                                       |
| -------------------------------- | ------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------- |
| Analyzer — pure-literal fold     | `ConstantFoldSemantic`                                                                            | Downstream rules can assume pure-literal subtrees have collapsed to a single `Lit`                 |
| Once — heuristic fold            | `ConstantFoldHeuristic`                                                                           | Boolean identity simplification (`true AND X → X`, `false OR X → X`)                               |
| FixedPoint — predicate pushdown  | `FilterFusion`, `FilterProjectTranspose`, `FilterAggregateTranspose`, `FilterRangeWindowTranspose`| Filters move below projections / aggregates / range windows so CH skip-indexes can fire on a `Scan`|
| FixedPoint — projection pushdown | `ProjectionPushdown`                                                                              | Late materialisation: wide columns are only resolved after `LIMIT` cuts the row set                |
| FixedPoint — MV substitution     | `MVSubstitution`                                                                                  | Swaps `RangeWindow(Scan(otel_metrics_*))` to a pre-aggregated rollup view when the rewrite is safe |

The optimiser is gated by termination, decision-pin, rule-interaction,
property, and gremlins (mutation) tests — see
[`docs/optimizer-research.md`](docs/optimizer-research.md) for the
backlog and the DataFusion `AnalyzerRule` / `OptimizerRule` rationale.

### Typed SQL — `internal/chsql`

Every emitted byte goes through a typed builder. Query shapes compose
through `QueryBuilder` slots (`.Select` / `.From` / `.Where` /
`.GroupBy` / `.OrderBy` / `.Limit` / `.Prewhere` / `.Join` /
`.WithRecursive`); expressions compose through typed `Frag`
constructors (`Eq`, `And`, `Or`, `Paren`, `Cast`, `In`, `Like`, `Add`,
`Call`, `Array`, `Subscript`, `If`, `Lambda1`, `Subquery`,
`BareIdent`, `InlineLit`, …). `Builder.writeSQL` is unexported and the
`chsql.Raw` / `chsql.Concat` public escape hatches are gone. **External
packages cannot produce raw SQL by construction** — the typed Frag
surface is closed, and adding a new shape means adding a new typed
constructor.

The emitter is also CH-native rather than ANSI-ish:

- **`PREWHERE` promotion** fuses `Filter(Scan)` into a single
  `SELECT … FROM <table> [PREWHERE …] WHERE …`, partitions conjuncts
  into a sort-prefix bucket / skip-index bucket / rest, and promotes
  cheap predicates that touch no wide column into `PREWHERE` when the
  projection reads any wide column.
- **`WITH RECURSIVE`** for label-set / trace-graph traversal.
- **Streaming `clickhouse-go/v2` cursor** — bounded RSS, no row buffer
  on the hot path; the engine's `QueryCursor` opens a streaming
  cursor when the underlying client implements `CursorQuerier`.

### Schema — drop-in OTel

Defaults to the
[OpenTelemetry ClickHouse Exporter](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/exporter/clickhouseexporter)
layout (`otel_metrics_*`, `otel_logs`, `otel_traces`). The schema
source of truth is the upstream OTel-CH exporter via the
[`tsouza/opentelemetry-collector-contrib:cerberus-ddl`](https://github.com/tsouza/opentelemetry-collector-contrib/tree/cerberus-ddl)
fork — cerberus consumes the same DDL templates the production
exporter emits, so a deployment where the exporter writes and cerberus
reads sees one schema across both sides. A thin YAML override config
supports SigNoz schemas and custom column layouts.

## Compatibility harnesses

Correctness for each query language is gated by a **differential
harness**: cerberus and a reference engine answer the same corpus of
queries against the same seeded data, and the responses are diffed
case-for-case. Unit-level golden tests pin SQL bytes; the harnesses
pin **observed semantics on real ClickHouse** against an upstream
oracle, which is what catches the gap between "the SQL we emit looks
right" and "the rows ClickHouse returns are right". Each harness lives
under `compatibility/<head>/`.

### PromQL — `prometheus/compliance`

- **Reference**: bundled Prometheus engine, queried side-by-side with cerberus.
- **Corpus**: vendored
  [`prometheus/compliance/promql/promql-test-queries.yml`](https://github.com/prometheus/compliance).
- **Driver**: upstream `promql-compliance-tester`.
- **Today**: **536/536** cases pass, `expected-failures.json` allow-list empty.

### LogQL — `grafana/loki:pkg/logql/bench`

- **Reference**: Loki single-binary container, queried side-by-side with cerberus.
- **Corpus**: vendored
  [`grafana/loki:pkg/logql/bench/queries/{fast,regression,exhaustive}`](https://github.com/grafana/loki/tree/main/pkg/logql/bench/queries).
- **Driver**: cerberus-owned `loki-compliance-tester`, shape-compatible
  JSON report with the Prom driver so both feed a single downstream
  analyser.
- **Today**: stack + seeder + driver landed (PR 5 of rollout); corpus
  expansion pending (PR 6 widens the seeder + regenerates
  `dataset_metadata.json` so the `${SELECTOR}` / `${LABEL_*}` templates
  resolve).

### TraceQL — cerberus-owned driver

- **Reference**: Tempo monolithic container.
- **Corpus**: cerberus-owned TXTAR corpus, patterned on `cmd/tempo-vulture`.
- **Driver**: cerberus-owned binary with `seed` + `diff` subcommands
  (OTLP push to Tempo + direct CH `INSERT` to cerberus, both from one
  in-memory fixture so per-span fields stay 1:1 across both read paths).
- **Today**: `/api/search`, `/api/traces/<id>`, and the four tag /
  tag-values endpoints (V1 + V2) land in PR 6; metrics endpoints
  (`/api/metrics/query_range` + `/api/metrics/query`) ship in PR 5.

Each harness ships as a Docker Compose stack — reference engine,
cerberus, ClickHouse, and a one-shot seeder. Local execution:

```sh
just compatibility            # PromQL — prometheus/compliance
just loki-compatibility       # LogQL — grafana/loki pkg/logql/bench
just tempo-compatibility      # TraceQL — tempo-vulture-patterned driver
just compatibility-all        # run all three
```

The unified workflow at
[`.github/workflows/compatibility.yml`](.github/workflows/compatibility.yml)
runs all three on push-to-main + nightly + manual dispatch. Per-head
JSON reports are uploaded as artefacts; on `push: main` only the
workflow commits a fresh `compat-score.json` to the orphan
`compat-scores` branch under `badges/<head>.json` — the source the
shields.io endpoint badges at the top of this README read from.

**Today's gating posture is informational on all three.** The PromQL
harness flips to a required PR check at the `v1.0.0` cut. The LogQL
and TraceQL harnesses become required checks at the end of their
respective rollouts (PR 7 each — see
[`docs/loki-compliance-plan.md`](docs/loki-compliance-plan.md) and
[`docs/tempo-compliance-plan.md`](docs/tempo-compliance-plan.md)).

**Allow-list discipline.** Each harness directory carries an
`expected-failures.json` listing queries where cerberus knowingly
diverges. Every entry requires a written reason — upstream parser
quirk that ClickHouse-side execution can't sensibly reproduce, a
documented OTel-CH schema difference, or a deferred-to-a-future-RC
gap with a doc link. Reviewers gate every addition. The PromQL
allow-list is currently empty.

See [`docs/compatibility.md`](docs/compatibility.md) for the full
playbook (local reproduction, report shape, adding test cases,
known limitations).

## Testing

Cerberus is tested in 12 layers — AST shape pinning, plan-IR
invariants, optimizer properties, emitted-SQL goldens, chDB-backed
roundtrips, HTTP wire conformance, system lifecycle, differential
harnesses, Playwright UX flows, chaos / goleak, perf benchmarks with
alloc regressions, and an oracle-based property framework. See
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
| **Compat**       | Differential parity vs reference Prom / Loki / Tempo                              | `just compatibility-all` (per-head recipes below)   |
| **Mutation**     | Gremlins matrix — see `docs/test-strategy.md` § "Gremlins phased rollout" for bar | `just mutate` (slow, nightly in CI)                 |

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

Pull the container image. Cerberus is alpha, so pin an RC tag — `:latest`
is **only** advanced by stable releases (none yet) and won't move until
`v1.0.0` cuts:

```sh
docker pull ghcr.io/tsouza/cerberus:v1.0.0-RC3
docker run --rm -p 8080:8080 \
  -e CERBERUS_CH_ADDR=clickhouse:9000 \
  ghcr.io/tsouza/cerberus:v1.0.0-RC3
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

## Project structure

```text
cmd/cerberus/            # main entrypoint
internal/
  api/{prom,loki,tempo}/ # HTTP handlers per upstream API
  engine/                # shared parse → optimize → emit → execute pipeline
  promql/                # PromQL head: parse + lower
  logql/                 # LogQL head: parse + lower
  traceql/               # TraceQL head: parse + lower
  chplan/                # shared plan IR
  chsql/                 # plan → CH SQL emitter (typed Builder + Frags)
  optimizer/             # rule + driver + rule implementations
  chclient/              # CH driver wrapper (clickhouse-go/v2)
  schema/                # OTel schema defaults + overrides
  config/                # runtime config (env-driven)
compatibility/
  prometheus/            # PromQL differential harness (prometheus/compliance)
  loki/                  # LogQL differential harness (grafana/loki pkg/logql/bench)
  tempo/                 # TraceQL differential harness (tempo-vulture-patterned)
test/
  spec/                  # TXTAR fixture-driven tests + chDB roundtrips
  property/              # oracle-based rapid property tests
  e2e/                   # k3d + Playwright
  e2e/{k3s,grafana}/     # k3d manifests + provisioned datasources/dashboards
  regression/            # CI-failure pins (goleak detectors, justfile shape)
```

## Contributing

Open an issue or a discussion before opening a large PR — the seed is
opinionated and the architecture lockdown is recent. Smaller PRs (a new
optimizer rule, a new TXTAR fixture, a parser-dep bump) are welcome any
time. See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[MIT](LICENSE) © Thiago Souza.
