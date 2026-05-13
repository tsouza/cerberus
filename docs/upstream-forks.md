# Upstream tracking — parser bumps + cerberus forks

Cerberus depends on three upstream parsers as Go libraries:

- `github.com/prometheus/prometheus/promql/parser`
- `github.com/grafana/loki/v3/pkg/logql/syntax`
- `github.com/grafana/tempo/pkg/traceql`

Plus the wider Grafana ecosystem (`dskit`, the forked `memberlist`) and the OTel Collector. Two forks under `github.com/tsouza/` carry the cerberus-specific patches; everything else rides upstream tags.

## Active forks

| Fork                                                                                                                     | Branch                | Purpose                                                                                                                                                                                                                                                                                                                                                                                                |
| ------------------------------------------------------------------------------------------------------------------------ | --------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| [`tsouza/tempo`](https://github.com/tsouza/tempo)                                                                        | `cerberus-accessors`  | Exposes the unexported `traceql` AST state cerberus needs for `internal/traceql/aggregate.go`, `internal/traceql/select.go`, and the MetricsPipeline lowering in `internal/traceql/metrics_pipeline.go`. Replaces the `unsafe.Pointer` + `reflect.FieldByName` shims cerberus used through P0 #7 (RC2).                                                                                                  |
| [`tsouza/opentelemetry-collector-contrib`](https://github.com/tsouza/opentelemetry-collector-contrib)                    | `cerberus-ddl`        | Surfaces the OTel-CH exporter's DDL templates (`sqltemplates`) as a consumable Go API so cerberus's `internal/schema/ddl/` package generates the same `CREATE TABLE` statements the exporter writes against. Single source of truth for the OTel-CH schema — no hand-maintained DDL.                                                                                                                   |

Each fork is wired via a `go.mod` `replace` directive pinning a pseudo-version at the head of the branch. Both forks track upstream `main`; `cerberus-accessors` and `cerberus-ddl` are long-lived branches rebased onto each upstream release cerberus wants to absorb.

## Why fork (not reflect / unsafe)

Both forks replace fragile reflective access to unexported upstream state. The `tsouza/tempo` fork retired:

- `internal/traceql/aggregate.go`'s `*(*traceql.FieldExpression)(unsafe.Pointer(field.UnsafeAddr()))` shim on `Aggregate.e` (the inner expression of `sum/avg/min/max(…)`).
- `internal/traceql/aggregate.go`'s `reflect.Value.FieldByName("op")` read on `Aggregate.op`.
- `internal/traceql/select.go`'s `reflect.Value.FieldByName("attrs")` walk on `SelectOperation.attrs`.
- The blocker for TraceQL MetricsPipeline lowering — `RootExpr.MetricsPipeline` is typed against the unexported `firstStageElement` / `secondStageElement` interfaces; cerberus could read the field but not type-switch on it without naming the interface.

The accessors are pure additions on the fork — one commit per accessor or per logically-coherent group. The total patch size on the fork is ~80–120 LoC of additions plus the two interface renames (`firstStageElement` → `FirstStageElement`, `secondStageElement` → `SecondStageElement`).

Both shapes (`unsafe.Pointer` + `reflect.Value.FieldByName`) are now banned in `internal/traceql/` and `internal/api/tempo/` via a `forbidigo` rule in `.golangci.yml`. New shims regress the lint gate.

## Rebase workflow

1. On the fork: `git fetch upstream && git checkout cerberus-accessors` (or `cerberus-ddl`).
2. `git rebase upstream/main` (or onto a specific upstream tag).
3. Resolve conflicts. The accessors are pure additions in mostly-stable files (`ast.go`, `ast_metrics.go`, `enum_aggregates.go` on the Tempo fork; `sqltemplates.go` on the collector-contrib fork). Conflicts surface as adjacent-line edits when upstream changes the surrounding code.
4. `go test ./pkg/traceql/...` (Tempo) or `go test ./exporter/clickhouseexporter/...` (collector-contrib) must stay green.
5. `git push --force-with-lease origin <branch>`.
6. In cerberus: `go get github.com/tsouza/tempo@<new-sha>` (or `…/opentelemetry-collector-contrib@<new-sha>`) → `go mod tidy` → push. CI must stay green; if it doesn't, the fork rebase exposed a real semantic drift in upstream and the cerberus migration code needs an update — exactly the early-warning value we wanted.

Dependabot in cerberus picks up the new pseudo-version like any other Go dep bump (daily, grouped under `upstream-parsers` per `.github/dependabot.yml`).

## Auto-bump: Dependabot grouping + auto-merge

`.github/dependabot.yml` runs **daily** for Go modules with two groups:

- `upstream-parsers` — Prom + Loki + Tempo + dskit + memberlist bumped together. They share state via the forked `memberlist` `replace` (see CLAUDE.md "Transitive-dep gotcha"); bumping one without the others tends to fail the build.
- `go-deps` — everything else, also daily, grouped.

Patch + minor bumps land in those grouped PRs. **Major bumps stay one-per-PR** — they usually need code changes and benefit from individual review.

GitHub Actions get a weekly bump (slower-moving) under `github-actions`. Playwright / npm deps under `test/e2e/playwright` get weekly under `playwright-deps`.

### Auto-merge on green CI

`.github/workflows/auto-merge-deps.yml` watches Dependabot PRs. When the PR is **patch-only** for an explicitly-trusted set of deps, the workflow enables auto-merge after CI passes. Trusted today:

- `github.com/prometheus/prometheus` (patch-only)
- `github.com/grafana/loki/v3` (patch-only)
- `github.com/grafana/tempo` (patch-only)
- `github.com/grafana/dskit` (patch-only)
- All `actions/*` GitHub Actions (patch + minor)
- Playwright (`@playwright/test`) (patch-only)

Minor bumps for parsers stay manual — even patch-named upstream releases sometimes ship semver-violating parser changes.

**Branch protection rules still apply.** The auto-merge marks the PR; merge happens only after `check + lint` go green. `enforce_admins: true` prevents bypass.

## When to add a new accessor

The fork's patch series stays minimal. Add an accessor when:

- Cerberus needs to read an unexported field today, and the alternative is a new `unsafe.Pointer` shim or a `reflect.FieldByName` read (both forbidden by `.golangci.yml`).
- An upstream interface is unexported and cerberus needs to type-switch on a value of that interface (e.g. `firstStageElement`).

Open the PR on the fork first (one commit per accessor), rebase `cerberus-accessors` / `cerberus-ddl` onto the new commit, then bump the `replace` directive in cerberus's `go.mod`.

## References

- `.github/dependabot.yml` — daily-grouped config.
- `.github/workflows/auto-merge-deps.yml` — auto-merge on green CI for trusted patch-only bumps.
- `.golangci.yml` — `forbidigo` rule blocking `unsafe.Pointer` / `reflect.Value.FieldByName` from being reintroduced in `internal/traceql/` and `internal/api/tempo/`.
- `internal/schema/ddl/` — consumes the `sqltemplates` API exposed by the collector-contrib fork.
- `internal/traceql/aggregate.go`, `internal/traceql/select.go`, `internal/traceql/metrics_pipeline.go` — call the Tempo fork's accessors.
