# Upstream tracking — parser & schema deps

Cerberus parses each of its three query languages, then renders ClickHouse SQL. The parsers and the schema templates come from a mix of in-house code and upstream libraries:

- **PromQL** — `github.com/prometheus/prometheus/promql/parser` (+ `model/labels`, `model/histogram`). Upstream, **Apache-2.0**.
- **LogQL** — cerberus's own **in-house, clean-room Apache reimplementation** of the LogQL grammar: `internal/logql/lsyntax` (parser), `internal/logql/logpattern` (the `pattern` parser), and `internal/drain` (the Drain log-pattern miner). No upstream parser library.
- **TraceQL** — cerberus's own **in-house, clean-room Apache reimplementation** of the TraceQL grammar: `internal/traceql/ast`. No upstream parser library.
- **Schema DDL** — `github.com/open-telemetry/opentelemetry-collector-contrib/exporter/clickhouseexporter/sqltemplates` (+ three sibling submodules). Upstream, **Apache-2.0**.

The shipped `cmd/cerberus` binary additionally links the **Apache-2.0** `github.com/grafana/tempo/pkg/tempopb` protobuf wire types (Tempo's HTTP API surface), and rides the wider Grafana ecosystem (`dskit`, the forked `memberlist`) on upstream tags.

> **Licence rule.** The upstream Grafana **LogQL** (`grafana/loki/v3/pkg/logql`) and **TraceQL** (`grafana/tempo/pkg/traceql`) parsers are **AGPLv3**, and the Apache-2.0 binary links neither. They appear only in **test-only oracles**: the `agpl_oracle`-tagged tests and the `compatibility/{loki,tempo}` differential harnesses. What keeps them out of the binary is **reachability, not module membership**. The root `go.mod` requires `grafana/loki/v3`, and `compatibility/` has no `go.mod` of its own, so the harnesses are root-module packages that `./cmd/cerberus` never imports; neither does it import anything behind the `agpl_oracle` build tag. The `test/oracle` nested module is a second, stronger boundary where a hard module break is wanted. The `agpl-clean` CI gate (`.github/scripts/agpl-clean.mjs`) runs `go list -deps ./cmd/cerberus` and fails the build if any AGPL package is reachable from the binary. See [`compatibility.md`](compatibility.md) and the `NOTICE` file.

## Forks

Three upstream deps route through `github.com/tsouza/*` forks. Each is wired by a `go.mod` `replace` directive pinning a **semver tag** on the fork's long-lived `cerberus-*` branch, which is also the fork's default branch.

| Fork                                                                                                  | Branch               | Patches                                                                                             | Kept current by                  | Tag                                             |
| ----------------------------------------------------------------------------------------------------- | -------------------- | --------------------------------------------------------------------------------------------------- | -------------------------------- | ----------------------------------------------- |
| [`tsouza/prometheus`](https://github.com/tsouza/prometheus)                                           | `cerberus-parser`    | none (one commit adds the fork's `cerberus-branch-check` workflow)                                  | forks-monitor, `track: release`  | `v0.(300+MINOR).PATCH-cerberus-parser`          |
| [`tsouza/opentelemetry-collector-contrib`](https://github.com/tsouza/opentelemetry-collector-contrib) | `cerberus-ddl`       | the `sqltemplates` hoist, the MetricName-first metrics sort key and the metrics timestamp precision | forks-monitor, `track: main`     | `v0.0.N-cerberus-ddl`, plus per-submodule tags  |
| [`tsouza/tempo`](https://github.com/tsouza/tempo)                                                     | `cerberus-accessors` | exported accessors for test-only oracles                                                            | by hand                          | `v0.0.N-cerberus-accessors`                     |

- **`tsouza/prometheus`** sits on an upstream **release** commit, never on upstream `main`: the `v3.15.0` release, commit `5241a27fe3c6983549fccc32f6e65917408c63cd`, the revision the `prom/prometheus:v3.15.0` reference image reports. Its tag is that release's Go-library version, `v0.315.0-cerberus-parser` for `v3.15.0`. Cerberus's parser, the spec lane's in-process oracle (`test/spec/parityoracle/promql`, which runs the same module's `promql.Engine`) and the compat reference server are one Prometheus release.
- **`tsouza/opentelemetry-collector-contrib`** exposes the OTel-CH exporter's DDL templates (`sqltemplates`) as a Go API. `internal/schema/ddl/` renders `CREATE TABLE` from upstream's own templates; there is no hand-maintained DDL. The rendered DDL matches the stock exporter's except on the five metrics tables (`metrics_{gauge,sum,histogram,exp_histogram,summary}_table.sql`). The fork parameterises their timestamp columns' type and the sort key's time element: `sqltemplates.MetricsCreateTables(sqltemplates.MetricsTimestampNanos)` renders `DateTime64(9)`, `MetricsTimestampSeconds` renders `DateTime`, and the exported `Metrics*CreateTable` variables are the seconds rendering. Both renderings lead the sort key with `MetricName`:

  ```text
  ORDER BY (MetricName, Attributes, ServiceName, toUnixTimestamp64Nano(TimeUnix))  -- nanos
  ORDER BY (MetricName, Attributes, ServiceName, toUnixTimestamp(TimeUnix))        -- seconds
  ```

  `CERBERUS_SCHEMA_METRICS_TIMESTAMP_PRECISION` picks the rendering. The traces and logs tables are stock. The metrics sort key, and the timestamp type under the default `nanos`, are where cerberus's auto-created schema differs from a stock OTel-CH deployment; [`operations.md`](operations.md#schema-divergence-metrics-sort-key-and-timestamp-type) has the operator-facing view. The fork also carries per-submodule tags (`exporter/clickhouseexporter/v…`, `internal/coreinternal/v…`, `pkg/core/xidutils/v…`, `pkg/translator/jaeger/v…`) at the same SHA, one for each submodule `go.mod` replaces.
- **`tsouza/tempo`** carries accessor-only patches, consumed by test-only code: `pkg/traceql` accessors, and `vparquet4.NewSpan`, a data-only constructor for the concrete span type that `DescendantOf` / `ChildOf` / `SiblingOf` operate on, which `test/spec/parityoracle/traceql` uses to run upstream's structural operators over spans read from chDB.

`grafana/loki/v3` is a **plain upstream require** (no fork), consumed by the AGPL test-only oracles. `grafana/tempo` is required at an upstream pseudo-version and `prometheus/prometheus` at the release's library version (`v0.315.0`); each is `replace`d by its fork tag. The `require` line records the upstream base the fork sits on, which is what the version-skew gate below reads, and the fork tag is what compiles.

Routing tempo through a fork does not move it into the binary: `cmd/cerberus` imports only the Apache-2.0 `pkg/tempopb` tree, and `pkg/traceql` / `tempodb/**` are reachable only from `agpl_oracle`-tagged test code.

## In-house LogQL / TraceQL parsers

LogQL and TraceQL are parsed by clean-room Apache reimplementations of the published language grammars, written from the grammar specification rather than derived from Grafana's AGPL source:

- **LogQL** — `internal/logql/lsyntax` (selector + pipeline + metric-query parser), `internal/logql/logpattern` (the `pattern` / `unpack` line parser), and `internal/drain` (the Drain pattern miner behind `/detected_fields`).
- **TraceQL** — `internal/traceql/ast` (spanset filters, pipelines, structural operators, metrics first/second stages).

Both expose the AST surface cerberus's lowering needs as exported types and accessors, so no `unsafe.Pointer` / `reflect.FieldByName` reach into parser internals is required. Those two shapes are banned across every `internal/**` package by a `forbidigo` rule in `.golangci.yml`.

Fidelity is held by two independent layers, neither of which links the AGPL parser into the binary:

- **Differential `agpl_oracle` tests** (build-tagged; never compiled into `cmd/cerberus`) parse the same corpus with the upstream AGPL parser and assert structural / behavioural agreement — e.g. `internal/logql/lsyntax/oracle_agpl_test.go`, `internal/logql/jsonpath_agpl_test.go`, `internal/logql/logpattern/pattern_agpl_test.go`.
- **The `compatibility/{loki,tempo}` harnesses** diff cerberus end-to-end against a reference Loki / Tempo backend.

## The forks-monitor

[`tsouza/cerberus-forks-monitor`](https://github.com/tsouza/cerberus-forks-monitor) runs daily at 10:17 UTC (`daily.yml`, configured by `monitor.yml`). For each fork it watches, it clones the fork and the upstream and picks a base by the entry's `track`:

- **`track: release`** (prometheus) — the newest stable upstream release tag (`vX.Y.Z`). If the fork's last tag already carries that release, the fork is skipped; otherwise the base is that release.
- **`track: main`** (collector-contrib) — `upstream/main`, but only when `git log <last-tag>..upstream/main -- <relevant_paths>` is non-empty; otherwise the fork is skipped.

With a base chosen, the monitor rebases `cerberus-<branch>` onto it, runs the configured subtree tests, force-pushes the branch and mints the new tag: the release's library version on `track: release`, the next patch on `track: main` (plus the per-submodule tags for collector-contrib). On a rebase conflict or red tests it opens, or comments on, one issue per fork and failure kind in the monitor repo, and leaves the fork untouched.

## Fork-tag drift

Dependabot does not rewrite `replace` targets. `.github/workflows/fork-tag-drift.yml` does, daily at 11:23 UTC (an hour after the forks-monitor) and on dispatch, through `.github/scripts/fork-tag-drift.mjs`:

1. It reads every `replace … => github.com/tsouza/<fork>[/<subdir>] vX.Y.Z-cerberus-<flavor>` line in every tracked `go.mod`.
2. It lists each fork's tags and takes the newest `-cerberus-<flavor>` version tagged for every replaced submodule path, so the collector-contrib submodules always move together.
3. When that version is newer than a pinned one:
   - a **replace-only fork** (collector-contrib, tempo) gets one pull request from `deps/fork-tag/<fork>`, labelled `dependencies`, that rewrites the replace lines in every `go.mod` and re-tidies each module. A branch that already pins the version is left untouched, so an open bump pull request is not re-pushed and a closed one is not reopened;
   - a **release-tracked fork**, recognised by a tag version core equal to the module's `require` (prometheus), gets one issue instead. Moving it is the [release move](#moving-prometheus-to-a-new-release).

The bump pull request is not queued automatically. Before adding `queue`, read what the tag changes (`git log <old>..<new>` on the fork, limited to the paths cerberus imports) and, when the change reaches the DDL templates or a parser, dispatch `compatibility.yml` on the branch. `TestMetricsSortingKeyPrefix_MatchesUpstreamDDL` and `TestTimestampColumnTypes_InPinnedExporterDDL` (`internal/schema`) fail a collector-contrib tag that changes the metrics sort key, or the timestamp column type of any table, at either metrics precision for the metrics tables.

Run it now, or preview it without side effects:

```bash
gh workflow run fork-tag-drift.yml
DRY_RUN=1 node .github/scripts/fork-tag-drift.mjs
```

## Dependabot and the queue

`.github/dependabot.yml` runs daily for the Go root module. Group `upstream-parsers` covers the upstream `require` lines of the collector-contrib, loki, tempo, `dskit` and `memberlist` modules. Group `prometheus-release` holds `github.com/prometheus/prometheus` on its own.

`.github/workflows/auto-merge-deps.yml` adds the `queue` label to a Dependabot pull request that is a patch-only bump of an allow-listed module, or a patch or minor bump of a GitHub Action. The label places it in the Mergify merge queue (`.github/mergify.yml`), which merges it only once every check the main ruleset requires passes, as [`agent-workflow.md`](agent-workflow.md) describes.

`github.com/prometheus/prometheus` is not on the allow-list. A Dependabot bump of its `require` moves the require alone, which fails the version-skew gate below. Turn that pull request into a release move by pushing the companion changes listed under [Moving Prometheus to a new release](#moving-prometheus-to-a-new-release), then queue it.

## Version-skew gate (parser version ↔ compat reference container)

`test/regression/fork_version_skew_test.go` (`TestForkVersionSkew`, in the required `check` lane — no build tag, no network) pins the upstream version each head's parser tracks to the reference backend container the matching compatibility harness diffs cerberus against. Both sides are read from committed files.

| Head       | go.mod `require`                            | Compat reference container                       | Compared at         |
| ---------- | ------------------------------------------- | ------------------------------------------------ | ------------------- |
| prometheus | `v0.(300+MINOR).PATCH` — e.g. `v0.315.0`    | `prom/prometheus:vMAJOR.MINOR.PATCH` (`v3.15.0`) | exact release       |
| loki       | `grafana/loki/v3 vMAJOR.MINOR.PATCH`        | `grafana/loki:MAJOR.MINOR.PATCH` (`3.7.0`)       | MAJOR.MINOR         |
| tempo      | pseudo-version `…-<commit12>`               | `grafana/tempo:main-<commit7>`                   | commit prefix       |

The Prometheus library reports `v0.(300+MINOR).PATCH` for release `v3.MINOR.PATCH`; the gate lowers the library version into release coordinates before comparing.

Further assertions:

- **prometheus** — the `replace` points at `github.com/tsouza/prometheus` on a `-cerberus-parser` tag whose version core equals the `require`. Every other compose file that runs the reference Prometheus (the migration tier-1 and tier-2 stacks, the histogram bench) names the compatibility image byte-for-byte, and `test/oracle/go.mod` requires and replaces the parser exactly as the root module does.
- **tempo** — `compatibility/tempo/upstream/VERSION`'s `upstream_commit` matches the go.mod pseudo-version commit, and the `replace` points at `github.com/tsouza/tempo` on a `-cerberus-accessors` tag.

If the gate fails, **bump whichever side is wrong** (the reference image to match the require, or the `require` to match the image). Do not relax the assertion.

### Moving Prometheus to a new release

One pull request:

1. Make sure `tsouza/prometheus` carries the release. The monitor mints `v0.(300+MINOR).PATCH-cerberus-parser` on the release commit on its first run after the release is published, and [fork-tag drift](#fork-tag-drift) then opens an issue for the move; otherwise [rebase the fork by hand](#manually-rebase-a-fork).
2. Bump the `require` and the `replace` in the root `go.mod` and in `test/oracle/go.mod`.
3. Bump every `prom/prometheus` image pin: `compatibility/prometheus/docker-compose.yml`, the restatements the gate lists, and the GHCR mirror inventory in `.github/scripts/lib/mirror.mjs` (pinned by `test/regression/mirror_inventory_test.go`).

## Manual operations

### Force a fork re-check

```bash
gh -R tsouza/cerberus-forks-monitor workflow run daily.yml
```

### Manually rebase a fork

Use this after a conflict the monitor could not resolve, or to move `tsouza/tempo`, which the monitor does not watch.

```bash
git clone git@github.com:tsouza/<fork>.git
cd <fork>
git remote add upstream https://github.com/<upstream>.git
git fetch upstream main --tags
git checkout cerberus-<branch>
# prometheus (track: release): rebase onto the release tag
git rebase vX.Y.Z
# collector-contrib (track: main) and tempo: rebase onto upstream main
git rebase upstream/main
# resolve conflicts, then test the subtree
go test ./<subtree>/...
git push --force-with-lease origin cerberus-<branch>
# prometheus: tag with the release's library version
git tag v0.<300+MINOR>.<PATCH>-cerberus-parser
# collector-contrib and tempo: bump the patch of the last cerberus-* tag
LAST=$(git describe --tags --abbrev=0 --match 'v*-cerberus-*')
NEXT=$(awk -F. '{patch=$3; gsub(/-.*/,"",patch); printf "%s.%s.%d-cerberus-...", $1, $2, patch+1}' <<<"$LAST")
git tag "$NEXT"
git push origin --tags
```

Collector-contrib also needs the per-submodule tags at the same SHA. [Fork-tag drift](#fork-tag-drift) then opens the cerberus pull request that bumps the `replace` directives on its next run.

### Add a new upstream

1. Fork the repo to `tsouza/<name>`.
2. Create a `cerberus-<flavor>` branch off the upstream commit you want as baseline. Push it. Make it the default branch: `gh repo edit tsouza/<name> --default-branch cerberus-<flavor>`.
3. Tag the branch head as `v0.0.1-cerberus-<flavor>` (or `v<MAJOR>.0.0-cerberus-<flavor>` if upstream uses a `/vMAJOR` module path).
4. Add `.github/workflows/cerberus-branch-check.yml` to the fork running `go test` on the relevant subtree.
5. Add an entry to `monitor.yml` in `tsouza/cerberus-forks-monitor`, with its `track`, `relevant_paths` and `test_subtrees`.
6. Extend the `upstream-parsers` group in cerberus's `.github/dependabot.yml`, and the allow-list in `.github/workflows/auto-merge-deps.yml` when a patch bump of the `require` can merge on its own.
7. Add a `replace` directive in cerberus's `go.mod`.

## Transitive replaces and ignored trees

Two kinds of `go.mod` directive sit outside the `tsouza/*` fork boundary and are not watched by the forks-monitor:

- **`replace github.com/hashicorp/memberlist => github.com/grafana/memberlist`.** Loki, Tempo and `dskit` each swap `hashicorp/memberlist` for Grafana's fork through their own `replace` directives. A `replace` never propagates to a consumer module, so the root module repeats it. The pinned pseudo-version is whatever the Grafana modules in `go.mod` themselves require; bump it alongside them. Dependabot groups `hashicorp/memberlist*` with the `upstream-parsers` group.
- **`ignore ./compatibility/tempo/upstream` and `ignore ./compatibility/loki/upstream`.** Those directories hold vendored snapshots of upstream Go source (Tempo's `pkg/httpclient`, Loki's `pkg/logql/bench`) that the compatibility harnesses read as corpus and reference code. The `ignore` directive keeps them out of every `./...` walk while leaving them on disk.

## References

- [`tsouza/cerberus-forks-monitor`](https://github.com/tsouza/cerberus-forks-monitor) — the daily cron repo. `README.md` there has the operational detail.
- `.github/dependabot.yml` — the daily `upstream-parsers` and `prometheus-release` groups.
- `.github/workflows/auto-merge-deps.yml` — queues trusted patch-only bumps into the merge queue.
- `.github/workflows/fork-tag-drift.yml` / `.github/scripts/fork-tag-drift.mjs` — moves `replace` targets onto new fork tags.
- `.github/scripts/agpl-clean.mjs` — the licence gate (fails if any AGPL package reaches `cmd/cerberus`).
- `.golangci.yml` — `forbidigo` rule blocking `unsafe.Pointer` / `reflect.Value.FieldByName` anywhere under `internal/**`.
- `internal/logql/lsyntax`, `internal/logql/logpattern`, `internal/drain`, `internal/traceql/ast` — the in-house Apache parsers.
- `internal/schema/ddl/` — consumes the `sqltemplates` API exposed by the collector-contrib fork.
- `NOTICE` — third-party attribution + the in-house-parser clean-room statement.
- `go.mod` — the `replace` / `ignore` directives above; `CLAUDE.md` links here for them.

The reasoning behind these choices is in [`upstream-forks.background.md`](upstream-forks.background.md).
