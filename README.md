# cerberus

**Drop-in Prometheus / Loki / Tempo HTTP gateway for ClickHouse.**
Keep Grafana, alerting, and your CLI tooling. Swap the backend.

> [!WARNING]
> **RELEASE CANDIDATE — NOT YET GA.** Cerberus is at `v1.0.0-RC1`;
> the surface is feature-complete for 1.0 and the three differential
> harnesses gate every merge, but correctness, performance, and
> operational hardening are still being burned down toward GA. Evaluate
> it against your own corpus before standing it in for a running
> Prom / Loki / Tempo deployment, and expect breaking changes to be
> possible between release candidates.

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

## Quick start

```sh
git clone https://github.com/tsouza/cerberus.git && cd cerberus
docker compose up --wait
open http://localhost:3000   # Grafana (auto-login as admin); cerberus on :8080
```

That builds cerberus, boots single-node ClickHouse, loads a deterministic
OTel fixture (logs / traces / metrics), and brings up Grafana
pre-provisioned with cerberus as three datasources (Prometheus + Loki +
Tempo). A fresh dashboard populates in ~30s; `docker compose down -v`
wipes the volume.

Cerberus is one stateless binary configured via environment variables,
with ClickHouse and the OTel collector as attached resources — the same
image runs unchanged under Compose, Kubernetes, or bare metal. See
[`docs/operations.md`](docs/operations.md) for the runtime + env-var
contract (and the `CERBERUS_OTLP_EXPORT_INTERVAL` time-to-first-panel knob).

### From a published release

Pull the container image. Cerberus is still pre-GA, so pin an explicit
tag — `:latest` only moves with stable releases:

```sh
docker pull ghcr.io/tsouza/cerberus:<tag>
docker run --rm -p 8080:8080 \
  -e CERBERUS_CH_ADDR=clickhouse:9000 \
  ghcr.io/tsouza/cerberus:<tag>
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

## Why cerberus?

Metrics, logs, and traces almost never live in one store. The usual
answer is Prometheus + Loki + Tempo — three retention policies, three
index strategies, three storage bills, three on-call playbooks. The
duplication doesn't buy anything: the OTLP payload going into each store
is largely the same data, just sliced for a different query language.
ClickHouse is a great single store for all three signals; the missing
piece is the **query side**. Point Grafana at cerberus as three
datasources (Prometheus, Loki, Tempo) and the queries you already have
keep working — translated into optimised ClickHouse SQL underneath.

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

**Release candidate — `v1.0.0-RC1`.** PromQL, LogQL, and TraceQL parse +
lowering are in place; the pattern-based optimiser, the typed `chsql`
SQL emitter, and the shared `internal/engine` pipeline drive every
query end to end. Self-observability is wired across all three OTel
pillars (logs + metrics + traces, all OTLP-exported); operational
scaffolding (`/readyz`, admission control, one-command local dev) is in
place. Upstream parser shims route through the
[`tsouza/tempo:cerberus-accessors`](https://github.com/tsouza/tempo/tree/cerberus-accessors)
fork; the schema source of truth is
[`tsouza/opentelemetry-collector-contrib:cerberus-ddl`](https://github.com/tsouza/opentelemetry-collector-contrib/tree/cerberus-ddl).
See [`CHANGELOG.md`](CHANGELOG.md) for the per-release breakdown.

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
 │  Analyzer (semantic) → Once (heuristic) →        │   semantic + heuristic +
 │  FixedPoint (rules that unlock each other)       │   fixpoint rewrites; no cost
 │                                                  │   model (see docs/performance.md)
 └──────────────────────────────────────────────────┘
                       │
                       ▼
 ┌──────────────────────────────────────────────────┐
 │           internal/chsql — typed emitter         │   • parameterised, escape-free
 │  QueryBuilder slots + typed Frag constructors;   │   • PREWHERE promotion on Filter(Scan)
 │  the typed surface is closed — external packages │   • sort-key-aware predicate ordering
 │  cannot compose raw SQL by construction          │   • streaming clickhouse-go/v2 cursor
 └──────────────────────────────────────────────────┘
                       │
                       ▼
                  ClickHouse
```

The three heads share one engine — [`internal/engine/`](internal/engine).
Each head implements `Lang` (`Parse` + `ProjectSamples`); the engine
holds the optimiser, the ClickHouse `Querier`, and the OTel spans around
each pipeline stage. The shared `internal/chplan` algebra is the meeting
point of all three heads, so **new optimisations cost one
implementation, not three** — the Catalyst-style rule-based optimiser,
the closed typed-Frag `chsql` emitter (CH-native `PREWHERE` /
`WITH RECURSIVE` / streaming cursor), and the drop-in OTel schema all
work over that one IR.

See [`docs/engine.md`](docs/engine.md) for the `Lang` contract, the
request lifecycle, and the in-depth per-stage breakdown (IR algebra,
the optimiser rule table, the typed-SQL emitter, and the OTel schema).
For **how cerberus keeps queries fast** — the compute-fan-out strategy,
the per-layer optimisations, and the four-layer regression-proofing that
holds them in place — see [`docs/performance.md`](docs/performance.md).

## Compatibility harnesses

Correctness for each query language is gated by a **differential
harness**: cerberus and a reference engine answer the same corpus of
queries against the same seeded data, and the responses are diffed
case-for-case. Golden tests pin SQL bytes; the harnesses pin **observed
semantics on real ClickHouse** against an upstream oracle — the gap
between "the SQL we emit looks right" and "the rows ClickHouse returns
are right". Each harness lives under `compatibility/<head>/`.

| Head    | Reference + corpus                                          | Required gate              |
| ------- | ----------------------------------------------------------- | -------------------------- |
| PromQL  | bundled Prometheus engine vs `prometheus/compliance` corpus | `compatibility/prometheus` |
| LogQL   | reference Loki vs `grafana/loki:pkg/logql/bench` corpus     | `compatibility/loki`       |
| TraceQL | reference Tempo vs cerberus-owned TXTAR corpus              | `compatibility/tempo`      |

Each harness ships as a Docker Compose stack — reference engine,
cerberus, ClickHouse, and a one-shot seeder. Local execution:

```sh
just compat-promql      # PromQL  — prometheus/compliance
just compat-logql       # LogQL   — grafana/loki pkg/logql/bench
just compat-traceql     # TraceQL — tempo-vulture-patterned driver
just compat-all         # run all three
```

The unified workflow at
[`.github/workflows/compatibility.yml`](.github/workflows/compatibility.yml)
runs all three on PRs + push-to-main + nightly + manual dispatch. All
three jobs are required PR status checks on `main`; per-head shields.io
badges read off the `compat-scores` orphan branch.

**No allow-lists.** No harness carries an expected-failures or skip
overlay: every diff against the reference backend is a real bug to fix
at the source. The only pinned exclusion set is
[`compatibility/loki/upstream-skip-baseline.txt`](compatibility/loki/upstream-skip-baseline.txt),
which records corpus entries *upstream itself* marks `skip: true`; the
harness fails on any drift in that set.

See [`docs/compatibility.md`](docs/compatibility.md) for the full
playbook — per-head driver / endpoint detail, local reproduction, report
shape, rejection parity, adding test cases, and the
`upstream-skip-baseline.txt` drift contract.

## Testing

Cerberus is tested in 11 layers spanning AST-shape pinning, plan-IR
invariants, optimizer properties, emitted-SQL goldens, chDB-backed
roundtrips, HTTP wire conformance, system lifecycle, differential
harnesses, Playwright UX flows, chaos / goleak, perf benchmarks, and an
oracle-based property framework. See
[`docs/test-strategy.md`](docs/test-strategy.md) for the canonical
layer map, CI-gate inventory, gremlins phased rollout, and per-layer
recipes. Quick reference:

| Layer family     | What it covers                                                                    | How to run                                          |
| ---------------- | --------------------------------------------------------------------------------- | --------------------------------------------------- |
| **Unit**         | Per-package logic, `Equal` contracts, optimizer rule kernels, Frag goldens        | `just test`                                         |
| **Spec (TXTAR)** | `<QL> → expected SQL` + chplan IR snapshots + optional chDB roundtrip             | `just test`; `just spec-chdb` for roundtrip lane    |
| **Property**     | Oracle-based property tests with `rapid` shrinking and chDB execution             | `just property`                                     |
| **Integration**  | `chclient` against a real ClickHouse via testcontainers                           | `just chclient-integration`                         |
| **E2E**          | k3d cluster with CH + Grafana + cerberus; Grafana Playwright smoke                | `just e2e`                                          |
| **Compat**       | Differential parity vs reference Prom / Loki / Tempo                              | `just compat-all` (per-head recipes above)          |
| **Mutation**     | Gremlins matrix — see `docs/test-strategy.md` § "Gremlins phased rollout" for bar | `just mutate` (slow, nightly in CI)                 |

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
