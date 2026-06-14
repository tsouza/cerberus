# Upstream tracking — parser deps routed through tsouza/* forks

Cerberus depends on four upstream parser / schema sources as Go libraries:

- `github.com/prometheus/prometheus/promql/parser` (+ `model/labels`, `model/histogram`)
- `github.com/grafana/loki/v3/pkg/logql/syntax` (+ `pkg/logql/log`, `pkg/logqlmodel`)
- `github.com/grafana/tempo/pkg/traceql`
- `github.com/open-telemetry/opentelemetry-collector-contrib/exporter/clickhouseexporter/sqltemplates` (+ three sibling submodules)

Plus the wider Grafana ecosystem (`dskit`, the forked `memberlist`) which still rides upstream tags.

**All four upstreams are routed through `github.com/tsouza/*` forks.** The forks exist primarily as a *Dependabot watch boundary*: cerberus's `go.mod` `replace` directives point at semver tags on the forks, not pseudo-versions on upstream. A dedicated cron repo, [`tsouza/cerberus-forks-monitor`](https://github.com/tsouza/cerberus-forks-monitor), decides daily whether anything cerberus cares about landed upstream and only then mints a new fork tag. Dependabot in cerberus sees a clean stream of patch bumps it can grouped-PR on.

## Active forks

| Fork                                                                                                  | Branch               | Patches             | Purpose                                                                                                                                                                                                                                                                                                                                                    |
| ----------------------------------------------------------------------------------------------------- | -------------------- | ------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| [`tsouza/prometheus`](https://github.com/tsouza/prometheus)                                           | `cerberus-parser`    | zero                | Pure Dependabot watch boundary. Cerberus consumes a narrow subtree (`promql/parser`, `model/labels`, `model/histogram`, a couple of adjacent files); the fork lets us mint tags only when those paths change.                                                                                                                                              |
| [`tsouza/loki`](https://github.com/tsouza/loki)                                                       | `cerberus-parser`    | zero                | Same watch-boundary role for `pkg/logql/syntax`, `pkg/logql/log/pattern`, `pkg/logqlmodel`, and a few `logql/log/*.go` files. Tag stream uses the `/v3` major version to match upstream's module path.                                                                                                                                                     |
| [`tsouza/tempo`](https://github.com/tsouza/tempo)                                                     | `cerberus-accessors` | ~6 accessors        | Exposes the unexported `traceql` AST state cerberus needs for `internal/traceql/aggregate.go`, `internal/traceql/select.go`, and the MetricsPipeline lowering in `internal/traceql/metrics_pipeline.go`. The accessors avoid the `unsafe.Pointer` + `reflect.FieldByName` pattern that would otherwise be required to reach those upstream-private fields. |
| [`tsouza/opentelemetry-collector-contrib`](https://github.com/tsouza/opentelemetry-collector-contrib) | `cerberus-ddl`       | sqltemplates hoist  | Surfaces the OTel-CH exporter's DDL templates (`sqltemplates`) as a Go API: `internal/schema/ddl/` renders `CREATE TABLE` from upstream's own templates — no hand-maintained DDL. Matches stock exporter DDL except the five metrics tables carry a **MetricName-first sort key**; the one deliberate, correctness-neutral divergence (see below).         |

Each fork is wired via a `go.mod` `replace` directive pinning a **semver tag** at the head of the long-lived `cerberus-*` branch. The forks' default branch IS the `cerberus-*` branch (so Dependabot resolves tags against it). The collector-contrib fork also carries per-submodule tags (`exporter/clickhouseexporter/v…`, `internal/coreinternal/v…`, `pkg/core/xidutils/v…`, `pkg/translator/jaeger/v…`) at the same SHA, because Go's module proxy resolves submodule versions independently in a monorepo.

## Why fork all four — even the unpatched ones?

A fork that we never patch is still valuable as a **Dependabot watch boundary**. The motivation:

- Cerberus consumes a **tiny slice** of each upstream parser (typically the parser tree + a couple of `model` packages).
- Upstream releases happen ~weekly. Without a fork, Dependabot would open a PR per release regardless of whether the change touched anything cerberus actually imports.
- A fork lets *us* decide "is this commit relevant?" before it ever becomes a tag. That decision is automated by `cerberus-forks-monitor`.

The cost is one extra layer (the fork, the monitor) and an additional invariant ("don't merge anything to upstream branches on the fork — only force-push the rebased `cerberus-*` branch"). The forks-monitor `README` covers operational details.

## Why the patched forks exist (tempo / otelc)

Both patched forks replace fragile reflective access to unexported upstream state.

The `tsouza/tempo` fork retired:

- `internal/traceql/aggregate.go`'s `*(*traceql.FieldExpression)(unsafe.Pointer(field.UnsafeAddr()))` shim on `Aggregate.e` (the inner expression of `sum/avg/min/max(…)`).
- `internal/traceql/aggregate.go`'s `reflect.Value.FieldByName("op")` read on `Aggregate.op`.
- `internal/traceql/select.go`'s `reflect.Value.FieldByName("attrs")` walk on `SelectOperation.attrs`.
- The blocker for TraceQL MetricsPipeline lowering — `RootExpr.MetricsPipeline` is typed against the unexported `firstStageElement` / `secondStageElement` interfaces; cerberus could read the field but not type-switch on it without naming the interface.

The accessors are pure additions on the fork — one commit per accessor or per logically-coherent group. The total patch size on the fork is ~80–120 LoC of additions plus the two interface renames (`firstStageElement` → `FirstStageElement`, `secondStageElement` → `SecondStageElement`).

The `tsouza/opentelemetry-collector-contrib` fork hoists the `sqltemplates` package out of `internal/` so cerberus's `internal/schema/ddl/` package can consume it directly. Without the hoist, cerberus would have to fork the templates wholesale, losing the upstream tracking that's the whole point.

The fork carries one deliberate content change on top of the hoist: the five metrics-table templates (`metrics_{gauge,sum,histogram,exp_histogram,summary}_table.sql`) lead their `ORDER BY` with `MetricName` —

```text
ORDER BY (MetricName, Attributes, ServiceName, toUnixTimestamp64Nano(TimeUnix))
```

— where stock OTel-CH leads with `ServiceName`. ClickHouse's `ORDER BY` (the table sort key) governs only data-skipping and on-disk layout, never query results, so this divergence is **correctness-neutral**: cerberus answers identically whether the metrics tables carry the stock key or the MetricName-first key. The only difference is performance — the metric-name-first key lets the common metric query (no `service.name` matcher) binary-search the primary key instead of falling to a generic-exclusion granule scan, measured at ~17× fewer granules read (see [`benchmarks.md`](benchmarks.md#metricname-first-order-by)). The traces and logs tables are rendered stock, unchanged. This is the single point at which cerberus's auto-created schema differs from a stock OTel-CH deployment; the operator-facing framing lives in [`operations.md`](operations.md#schema-divergence-metricname-first-metrics-sort-key).

Both shapes (`unsafe.Pointer` + `reflect.Value.FieldByName`) are banned across every `internal/**` package via a `forbidigo` rule in `.golangci.yml`. New shims regress the lint gate.

## How a new upstream change reaches cerberus

```text
┌──────────────────────┐                             ┌──────────────────────────┐
│ upstream repo        │ ────────────────────────▶   │ tsouza/<fork>            │
│ (prometheus, loki,   │   ─ relevant paths only ─   │   cerberus-<branch>      │
│  tempo, otel-c)      │   ─ as a new tag ─          │     ├── v0.0.1 (baseline)│
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

## Version-skew gate (fork base ↔ compat reference container)

`test/regression/fork_version_skew_test.go` (`TestForkVersionSkew`, runs in the
required `check` lane — no build tag, no network) pins the correspondence
between the upstream version each parser fork is based on and the reference
backend container the matching compatibility harness diffs cerberus against.

The compat lanes are the
parity oracle for all three heads; the sharded-pushdown solver trusts "route A"
precisely because it matches the reference engine those lanes run. But route A
parses with a `tsouza/*` fork of that engine's parser. If a fork drifts to a
different upstream version than the reference container, the lane silently
compares cerberus-with-parser-vX against reference-engine-vY, and any parity it
certifies is unsound for whatever grammar/semantics moved between vX and vY. The
gate turns that drift into a `check`-lane failure instead.

What it asserts, per head — both sides are statically derivable from committed
files, so the check needs no network call:

| Head       | Fork base (go.mod `require`)                | Compat reference container                       | Compared at         |
| ---------- | ------------------------------------------- | ------------------------------------------------ | ------------------- |
| prometheus | `v0.(300+MINOR).PATCH` — e.g. `v0.311.3`    | `prom/prometheus:vMAJOR.MINOR.PATCH` (`v3.11.3`) | release MAJOR.MINOR |
| loki       | `grafana/loki/v3 vMAJOR.MINOR.PATCH`        | `grafana/loki:MAJOR.MINOR.PATCH` (`3.7.0`)       | MAJOR.MINOR         |
| tempo      | pseudo-version `…-<commit12>`               | `grafana/tempo:main-<commit7>`                   | commit prefix       |

For tempo the gate additionally cross-checks the committed
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
match the fork base, or the fork `require` to match the image) — do not relax
the assertion to make the skew disappear. When the forks-monitor cron bumps a
fork onto a new upstream minor, this gate is what forces the matching
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

### Add an accessor to the tempo fork

The fork's patch series stays minimal. Add an accessor when:

- Cerberus needs to read an unexported field today, and the alternative is a new `unsafe.Pointer` shim or a `reflect.FieldByName` read (both forbidden by `.golangci.yml`).
- An upstream interface is unexported and cerberus needs to type-switch on a value of that interface (e.g. `firstStageElement`).

Open the PR on the fork first (one commit per accessor), let the `cerberus-branch-check.yml` workflow on the fork pass, then either wait for the monitor to mint a new tag or mint one by hand. Then bump the `replace` directive in cerberus's `go.mod`.

### Add a new upstream

If a new RC introduces a fifth parser dep:

1. Fork the repo to `tsouza/<name>`.
2. Locally create a `cerberus-<flavor>` branch off the upstream commit you want as baseline. Push it. Set as default via `gh repo edit tsouza/<name> --default-branch cerberus-<flavor>`.
3. Tag the branch head as `v0.0.1-cerberus-<flavor>` (or `v<MAJOR>.0.0-cerberus-<flavor>` if upstream uses a `/vMAJOR` module path).
4. Add `.github/workflows/cerberus-branch-check.yml` to the fork running `go test` on the relevant subtree.
5. Add an entry to `monitor.yml` in `tsouza/cerberus-forks-monitor`.
6. Extend the `upstream-parsers` group + the auto-merge allowlist in cerberus's `.github/dependabot.yml` and `.github/workflows/auto-merge-deps.yml`.
7. Add a `replace` directive in cerberus's `go.mod`.

## References

- [`tsouza/cerberus-forks-monitor`](https://github.com/tsouza/cerberus-forks-monitor) — the daily cron repo. `README.md` there has the operational detail.
- `.github/dependabot.yml` — daily-grouped config. Group `upstream-parsers` covers all four forks.
- `.github/workflows/auto-merge-deps.yml` — auto-merge on green CI for trusted patch-only bumps.
- `.golangci.yml` — `forbidigo` rule blocking `unsafe.Pointer` / `reflect.Value.FieldByName` from being reintroduced anywhere under `internal/**`.
- `internal/schema/ddl/` — consumes the `sqltemplates` API exposed by the collector-contrib fork.
- `internal/traceql/aggregate.go`, `internal/traceql/select.go`, `internal/traceql/metrics_pipeline.go` — call the Tempo fork's accessors.
- `CLAUDE.md` § "Transitive-dep gotcha" — the unrelated memberlist replace.
