# Upstream tracking — parser & schema deps

Cerberus parses each of its three query languages, then renders ClickHouse SQL. The parsers and the schema templates come from a mix of in-house code and upstream libraries:

- **PromQL** — `github.com/prometheus/prometheus/promql/parser` (+ `model/labels`, `model/histogram`). Upstream, **Apache-2.0**.
- **LogQL** — cerberus's own **in-house, clean-room Apache reimplementation** of the LogQL grammar: `internal/logql/lsyntax` (parser), `internal/logql/logpattern` (the `pattern` parser), and `internal/drain` (the Drain log-pattern miner). No upstream parser library.
- **TraceQL** — cerberus's own **in-house, clean-room Apache reimplementation** of the TraceQL grammar: `internal/traceql/ast`. No upstream parser library.
- **Schema DDL** — `github.com/open-telemetry/opentelemetry-collector-contrib/exporter/clickhouseexporter/sqltemplates` (+ three sibling submodules). Upstream, **Apache-2.0**.

The shipped `cmd/cerberus` binary additionally links the **Apache-2.0** `github.com/grafana/tempo/pkg/tempopb` protobuf wire types (Tempo's HTTP API surface), and rides the wider Grafana ecosystem (`dskit`, the forked `memberlist`) on upstream tags.

> **Licence note.** The upstream Grafana **LogQL** (`grafana/loki/v3/pkg/logql`) and **TraceQL** (`grafana/tempo/pkg/traceql`) parsers are **AGPLv3**. Cerberus ships under Apache-2.0 and **does not link either** — that is the whole reason the LogQL/TraceQL parsers were rewritten in-house. The AGPL parsers survive only as **test-only oracles** (the `agpl_oracle`-tagged tests and the `compatibility/{loki,tempo}` differential harnesses), quarantined into the `test/oracle` nested module so they can never reach the binary. The `agpl-clean` CI gate (`.github/scripts/agpl-clean.mjs`) enforces this: it fails the build if any AGPL package is reachable from `./cmd/cerberus`. See [`compatibility.md`](compatibility.md) and the `NOTICE` file.

## Forks

Two upstream deps route through `github.com/tsouza/*` forks. The forks exist primarily as a *Dependabot watch boundary*: cerberus's `go.mod` `replace` directives point at semver tags on the forks, not pseudo-versions on upstream. A dedicated cron repo, [`tsouza/cerberus-forks-monitor`](https://github.com/tsouza/cerberus-forks-monitor), decides daily whether anything cerberus cares about landed upstream and only then mints a new fork tag. Dependabot in cerberus sees a clean stream of patch bumps it can grouped-PR on.

- **[`tsouza/prometheus`](https://github.com/tsouza/prometheus)** — branch `cerberus-parser`, **zero patches**. Pure Dependabot watch boundary. Cerberus consumes a narrow subtree (`promql/parser`, `model/labels`, `model/histogram`, a couple of adjacent files); the fork lets us mint tags only when those paths change.
- **[`tsouza/opentelemetry-collector-contrib`](https://github.com/tsouza/opentelemetry-collector-contrib)** — branch `cerberus-ddl`, **one patch (sqltemplates hoist)**. Surfaces the OTel-CH exporter's DDL templates (`sqltemplates`) as a Go API: `internal/schema/ddl/` renders `CREATE TABLE` from upstream's own templates — no hand-maintained DDL. Matches stock exporter DDL except the five metrics tables carry a **MetricName-first sort key**; the one deliberate divergence (see below).

Each fork is wired via a `go.mod` `replace` directive pinning a **semver tag** at the head of the long-lived `cerberus-*` branch. The forks' default branch IS the `cerberus-*` branch (so Dependabot resolves tags against it). The collector-contrib fork also carries per-submodule tags (`exporter/clickhouseexporter/v…`, `internal/coreinternal/v…`, `pkg/core/xidutils/v…`, `pkg/translator/jaeger/v…`) at the same SHA, because Go's module proxy resolves submodule versions independently in a monorepo.

The `grafana/loki/v3` and `grafana/tempo` libraries are consumed on **plain upstream requires** (no fork): `tempo` for the Apache `pkg/tempopb` wire types in the binary, and both for the AGPL test-only oracles described above.

## Why fork prometheus even though it is unpatched?

A fork that we never patch is still valuable as a **Dependabot watch boundary**. The motivation:

- Cerberus consumes a **tiny slice** of the prometheus library (the PromQL parser tree + a couple of `model` packages).
- Upstream releases happen ~weekly. Without a fork, Dependabot would open a PR per release regardless of whether the change touched anything cerberus actually imports.
- A fork lets *us* decide "is this commit relevant?" before it ever becomes a tag. That decision is automated by `cerberus-forks-monitor`.

The cost is one extra layer (the fork, the monitor) and an additional invariant ("don't merge anything to upstream branches on the fork — only force-push the rebased `cerberus-*` branch"). The forks-monitor `README` covers operational details.

## Why the otelc fork exists

The `tsouza/opentelemetry-collector-contrib` fork hoists the `sqltemplates` package out of `internal/` so cerberus's `internal/schema/ddl/` package can consume it directly. Without the hoist, cerberus would have to fork the templates wholesale, losing the upstream tracking that's the whole point.

The fork carries one deliberate content change on top of the hoist: the five metrics-table templates (`metrics_{gauge,sum,histogram,exp_histogram,summary}_table.sql`) lead their `ORDER BY` with `MetricName` —

```text
ORDER BY (MetricName, Attributes, ServiceName, toUnixTimestamp64Nano(TimeUnix))
```

— where stock OTel-CH leads with `ServiceName`. ClickHouse's `ORDER BY` (the table sort key) governs only data-skipping and on-disk layout, never query results, so this divergence is **correctness-neutral**: cerberus answers identically whether the metrics tables carry the stock key or the MetricName-first key. The only difference is performance — the metric-name-first key lets the common metric query (no `service.name` matcher) binary-search the primary key instead of falling to a generic-exclusion granule scan, measured at ~17× fewer granules read (see [`benchmarks.md`](benchmarks.md#metricname-first-order-by)). The traces and logs tables are rendered stock, unchanged. This is the single point at which cerberus's auto-created schema differs from a stock OTel-CH deployment; the operator-facing framing lives in [`operations.md`](operations.md#schema-divergence-metricname-first-metrics-sort-key).

## In-house LogQL / TraceQL parsers

LogQL and TraceQL are parsed by clean-room Apache reimplementations of the published language grammars, written from the grammar specification rather than derived from Grafana's AGPL source:

- **LogQL** — `internal/logql/lsyntax` (selector + pipeline + metric-query parser), `internal/logql/logpattern` (the `pattern` / `unpack` line parser), and `internal/drain` (the Drain pattern miner behind `/detected_fields`).
- **TraceQL** — `internal/traceql/ast` (spanset filters, pipelines, structural operators, metrics first/second stages).

Both expose the AST surface cerberus's lowering needs as exported types and accessors, so no `unsafe.Pointer` / `reflect.FieldByName` reach into parser internals is required. Those two shapes remain banned across every `internal/**` package via a `forbidigo` rule in `.golangci.yml`.

Fidelity is held by two independent layers, neither of which links the AGPL parser into the binary:

- **Differential `agpl_oracle` tests** (build-tagged; never compiled into `cmd/cerberus`) parse the same corpus with the upstream AGPL parser and assert structural / behavioural agreement — e.g. `internal/logql/lsyntax/oracle_agpl_test.go`, `internal/logql/jsonpath_agpl_test.go`, `internal/logql/logpattern/pattern_agpl_test.go`, `internal/api/loki/detected_extract_agpl_test.go`.
- **The `compatibility/{loki,tempo}` harnesses** diff cerberus end-to-end against a reference Loki / Tempo backend.

## How a new upstream change reaches cerberus

```text
┌──────────────────────┐                             ┌──────────────────────────┐
│ upstream repo        │ ────────────────────────▶   │ tsouza/<fork>            │
│ (prometheus,         │   ─ relevant paths only ─   │   cerberus-<branch>      │
│  otel-collector-c)   │   ─ as a new tag ─          │     ├── v0.0.1 (baseline)│
│                      │                             │     ├── v0.0.2           │
│                      │                             │     └── …                │
└──────────────────────┘                             └────────────┬─────────────┘
                                                                  │
                                  cerberus-forks-monitor (cron) ──┘
                                                                  │
                                       Dependabot watches tags    │
                                                                  ▼
                                                     ┌──────────────────────────┐
                                                     │ tsouza/cerberus go.mod   │
                                                     │   replace directives use │
                                                     │   tsouza/<fork>@vX.Y.Z   │
                                                     └──────────────────────────┘
```

Concretely, the daily cycle is:

1. `cerberus-forks-monitor` cron triggers at 10:17 UTC. The job lives at [`tsouza/cerberus-forks-monitor`](https://github.com/tsouza/cerberus-forks-monitor); the configuration is `monitor.yml`.
2. For each fork, the monitor clones the fork + the upstream and runs `git log <last-tag>..upstream/main -- <relevant_paths>`.
3. If empty: skip silently.
4. If non-empty: rebase `cerberus-<branch>` onto `upstream/main`, run the configured subtree tests (`go test ./promql/parser/...`, etc.), push, and mint a new patch-bumped tag (plus per-submodule tags for collector-contrib).
5. On rebase conflict or red tests: open an issue in the monitor repo for human resolution. The fork is NOT force-pushed in that case.
6. Dependabot in cerberus picks up the new tag on its next daily run (`.github/dependabot.yml`, group `upstream-parsers`) and opens a single grouped PR.
7. The patch-only auto-merge workflow (`.github/workflows/auto-merge-deps.yml`) enables auto-merge once `check + lint` go green. Branch protection still gates the actual merge.

## Version-skew gate (parser version ↔ compat reference container)

`test/regression/fork_version_skew_test.go` (`TestForkVersionSkew`, runs in the
required `check` lane — no build tag, no network) pins the correspondence
between the upstream version each head's parser tracks and the reference
backend container the matching compatibility harness diffs cerberus against.

The compat lanes are the parity oracle for all three heads; the sharded-pushdown
solver trusts "route A" precisely because it matches the reference engine those
lanes run. If the parser cerberus uses drifts to a different upstream version
than the reference container, the lane silently compares cerberus-with-grammar-vX
against reference-engine-vY, and any parity it certifies is unsound for whatever
grammar/semantics moved between vX and vY. The gate turns that drift into a
`check`-lane failure instead.

What it asserts, per head — both sides are statically derivable from committed
files, so the check needs no network call:

| Head       | go.mod `require`                            | Compat reference container                       | Compared at         |
| ---------- | ------------------------------------------- | ------------------------------------------------ | ------------------- |
| prometheus | `v0.(300+MINOR).PATCH` — e.g. `v0.311.3`    | `prom/prometheus:vMAJOR.MINOR.PATCH` (`v3.11.3`) | release MAJOR.MINOR |
| loki       | `grafana/loki/v3 vMAJOR.MINOR.PATCH`        | `grafana/loki:MAJOR.MINOR.PATCH` (`3.7.0`)       | MAJOR.MINOR         |
| tempo      | pseudo-version `…-<commit12>`               | `grafana/tempo:main-<commit7>`                   | commit prefix       |

For loki and tempo the pinned version is the one the `agpl_oracle` differential
tests + the compat harness parse against (the binary itself links neither
parser). For tempo the gate additionally cross-checks the committed
`compatibility/tempo/upstream/VERSION` `upstream_commit` field against the same
go.mod pseudo-version commit, so the vendored snapshot can't drift either.

The Prometheus library reports `v0.(300+MINOR).PATCH` for release
`v3.MINOR.PATCH` since the v3.0.0 release; the gate lowers the library version
into release coordinates before comparing. The two semver heads are checked at
MAJOR.MINOR — that is the grain at which the query-language grammar and
evaluation semantics that parity rests on actually change; patch releases are
bug fixes that don't move the language, and the reference image routinely lags
the Go library by a patch (loki today: go.mod `v3.7.1`, image `3.7.0` — same
`3.7` grammar, gate green). Tempo is commit-pinned on every side, so it is
checked at the exact commit prefix.

If the gate fails, **bump whichever side is wrong** (the reference image to
match the require, or the `require` to match the image) — do not relax the
assertion to make the skew disappear. When the forks-monitor cron bumps the
prometheus fork onto a new upstream minor, this gate is what forces the matching
compat-image bump to land in the same PR.

## Manual operations

### Force a fork re-check

```bash
gh -R tsouza/cerberus-forks-monitor workflow run daily.yml
```

### Manually rebase a fork (e.g. after a conflict the bot couldn't handle)

```bash
git clone git@github.com-tsouza:tsouza/<fork>.git
cd <fork>
git remote add upstream https://github.com/<upstream>.git
git fetch upstream main
git checkout cerberus-<branch>
git rebase upstream/main
# resolve conflicts; if patches are pure additions, conflicts are rare
go test ./<subtree>/...
git push --force-with-lease origin cerberus-<branch>
# mint a new tag (bump patch from the last cerberus-* tag)
LAST=$(git describe --tags --abbrev=0 --match 'v*-cerberus-*')
NEXT=$(awk -F. '{patch=$3; gsub(/-.*/,"",patch); printf "%s.%s.%d-cerberus-...", $1, $2, patch+1}' <<<"$LAST")
git tag "$NEXT"
git push origin "$NEXT"
```

### Add a new upstream

If a new head introduces an upstream parser/schema dep that warrants a watch boundary:

1. Fork the repo to `tsouza/<name>`.
2. Locally create a `cerberus-<flavor>` branch off the upstream commit you want as baseline. Push it. Set as default via `gh repo edit tsouza/<name> --default-branch cerberus-<flavor>`.
3. Tag the branch head as `v0.0.1-cerberus-<flavor>` (or `v<MAJOR>.0.0-cerberus-<flavor>` if upstream uses a `/vMAJOR` module path).
4. Add `.github/workflows/cerberus-branch-check.yml` to the fork running `go test` on the relevant subtree.
5. Add an entry to `monitor.yml` in `tsouza/cerberus-forks-monitor`.
6. Extend the `upstream-parsers` group + the auto-merge allowlist in cerberus's `.github/dependabot.yml` and `.github/workflows/auto-merge-deps.yml`.
7. Add a `replace` directive in cerberus's `go.mod`.

## References

- [`tsouza/cerberus-forks-monitor`](https://github.com/tsouza/cerberus-forks-monitor) — the daily cron repo. `README.md` there has the operational detail.
- `.github/dependabot.yml` — daily-grouped config. Group `upstream-parsers` covers the prometheus + collector-contrib forks.
- `.github/workflows/auto-merge-deps.yml` — auto-merge on green CI for trusted patch-only bumps.
- `.github/scripts/agpl-clean.mjs` — the provably-clean-build licence gate (fails if any AGPL package reaches `cmd/cerberus`).
- `.golangci.yml` — `forbidigo` rule blocking `unsafe.Pointer` / `reflect.Value.FieldByName` from being reintroduced anywhere under `internal/**`.
- `internal/logql/lsyntax`, `internal/logql/logpattern`, `internal/drain`, `internal/traceql/ast` — the in-house Apache parsers.
- `internal/schema/ddl/` — consumes the `sqltemplates` API exposed by the collector-contrib fork.
- `NOTICE` — third-party attribution + the in-house-parser clean-room statement.
- `CLAUDE.md` § "Transitive-dep gotcha" — the unrelated memberlist replace.
