# Cerberus — agent context

Drop-in **Prometheus / Loki / Tempo** HTTP gateway for **ClickHouse**. Parses each upstream query language with its reference parser, lowers into a shared plan IR (`internal/chplan`), applies a small rule-based optimizer, and emits parameterised ClickHouse SQL. The HTTP layer speaks the upstream Prom / Loki / Tempo wire format so Grafana sees cerberus as three drop-in datasources.

## Hard rules (non-negotiable)

- **PR-per-change.** No direct pushes to `main` — branch protection rejects them. Required CI checks: `check`, `lint`, `forbid-skip`, `compatibility/{prometheus,loki,tempo}`, `probe`, `roundtrip (promql)`, `roundtrip (logql)`, `roundtrip (traceql)`, `compose-smoke`. The `dashboard` full-stack smoke (k3d + cerberus + Grafana + Playwright; lives as the `dashboard` job inside `.github/workflows/e2e.yml`) runs on push-to-main + nightly + manual dispatch only — it is informational on merges, not a PR gate. Force-push and deletion are off on `main`; the GitHub "Update branch" button (merge-commits) works for stale PRs. **Never use `gh pr merge --admin`** — every PR must merge cleanly with all required checks green. If a required check is failing, fix the code or fix the workflow; don't bypass. Branch protection has `enforce_admins: true` and the personal token doesn't grant override.
- **Agent-driven work goes through PRs, not Issues.** When *you* (an AI assistant) are doing the work, capture intent in the PR description — don't open an issue to track follow-up. Human contributors (or the maintainer) **are welcome to open issues** for bug reports, design discussions, feature proposals — the issues feature is on. The rule is about agent workflow hygiene, not project policy.
- **Conventional Commits**, enforced by `commitlint` (see `.commitlintrc.json`). The `subject-case` rule is relaxed so Dependabot's `Bump X from Y to Z` subjects pass.
- **Justfile is the canonical task runner.** `just` lists every recipe. Don't reach for `go test ./...` directly when `just test` exists — the recipe sets the race flag, the cover profile, and the right toolchain.
- **No manual pre-flight; lefthook + CI own it.** Don't run `just test`, `just lint`, `go test`, `golangci-lint run`, `go build`, or `markdownlint-cli2` manually before pushing. The repo's `lefthook.yml` is layered:
  - `pre-commit` — sub-second formatters on staged files (`gofumpt` / `goimports` / `markdownlint-cli2 --fix`).
  - `commit-msg` — Conventional-Commits via `commitlint`.
  - `pre-push` — once-per-push gate that mirrors the CI `check` + `lint` + `forbid-skip` jobs: `golangci-lint run ./...`, `markdownlint-cli2` verify, and the discipline greps (`t.Skip*`, "not implemented" / "skipped" / "deferred" wording, soft-assertions / silent recovers). Bypass with `LEFTHOOK=0 git push` for WIP branches.
  - CI runs the full test suite + the compat / e2e / mutation lanes the local hook intentionally doesn't.
  - New contributors run `just hooks-install` once after cloning; agents trust the hooks + CI and don't pre-flight manually.
- **Compatibility is the source of truth for PromQL.** `compatibility.yml` runs on main pushes + nightly + manual dispatch and acts as the informational baseline; a future cut can re-enable the `pull_request:` trigger and add `compatibility/prometheus` to required checks. An entry in `compatibility/prometheus/expected-failures.json` requires a comment explaining the upstream rationale.
- **No raw SQL strings — typed chsql API only.** Use `internal/chsql.Builder` / `chsql.QueryBuilder` — the custom CH-flavored builder API. Compose clauses via typed `QueryBuilder` slots (`.Select` / `.From` / `.Where` / `.GroupBy` / `.OrderBy` / `.Limit` / `.Prewhere` / `.Join` / `.WithRecursive`) and expressions via typed Frags (`Eq` / `And` / `Or` / `Paren` / `Cast` / `In` / `Like` / `Add` / `Call` / `Array` / `Subscript` / `If` / `Lambda1` / `Subquery` / `BareIdent` / `InlineLit` / etc.). The typed-Frag surface is closed by construction: external packages cannot raw-write SQL. Add new typed constructors when a shape isn't covered; never compose SQL via string concatenation. Reviewer discipline + the typed API are the enforcement.

## Architecture map

```text
internal/
  api/{prom,loki,tempo}/    HTTP handlers per upstream API
  promql/, logql/, traceql/ three heads: parse + lower
  chplan/                    shared plan IR (Scan, Filter, Project, Aggregate, RangeWindow, Limit + Expr tree)
  optimizer/                 rule-based, fixpoint driver. Pattern API + analyzer/optimizer rule split; transposes + PREWHERE promotion + MV substitution + late materialisation.
  chsql/                     plan → ClickHouse SQL emitter
  chclient/                  CH driver wrapper (clickhouse-go/v2)
  schema/                    OTel-CH default + override config
  config/                    runtime config (env-driven)
cmd/cerberus/                main entrypoint
test/spec/                   TXTAR golden tests (input QL → SQL/plan + `-- chplan --` IR snapshots + optional `-- seed --` / `-- expected_rows --` chDB roundtrip). `test/spec/chplan_print.go` is the deterministic IR pretty-printer used by Layer 2a snapshots.
test/property/               oracle-based property tests (`pgregory.net/rapid` shrinking + chDB execution); `gen/` random data + query generators; `oracle/` from-scratch evaluator.
test/regression/             meta-tests that pin past CI failures so they can't silently recur — goleak detectors across every handler entrypoint (added by #253), justfile-shape pins, seed-program invariants.
test/e2e/                    k3d cluster + Grafana playwright smoke
test/e2e/{k3s,grafana}/      k3d manifests + Grafana provisioning (datasources, dashboards) consumed by the smoke
compatibility/prometheus/    prometheus/compliance Docker Compose harness — PromQL differential testing
compatibility/loki/          LogQL differential harness vs reference Loki + vendored `grafana/loki:pkg/logql/bench` corpus
compatibility/tempo/         TraceQL differential harness vs reference Tempo + vendored `cmd/tempo-vulture` / `pkg/httpclient` snapshot
docs/                        engine.md, compatibility.md, test-strategy.md, observability.md, operations.md, upstream-forks.md, health.md, …
```

See [`docs/test-strategy.md`](docs/test-strategy.md) for the canonical 12-layer test map, the CI-gate inventory, the gremlins phased rollout, and the property-test phase plan.

Top-level reading order for any new contributor (human or agent):

1. `README.md` — what the project is, quick start.
2. `docs/engine.md` — shared query pipeline (`internal/engine/`), the `Lang` contract, and the extension points each new head plugs into.
3. `docs/operations.md` — runtime contract: configuration, lifecycle, scaling.
4. `docs/test-strategy.md` — 12-layer test map + CI gate inventory.
5. `internal/promql/lower.go` — the canonical lowering pattern; mirror it when adding LogQL / TraceQL slices.

## Common workflows

- **Add a TXTAR fixture** — use the `/cerberus:add-fixture` skill (under `.claude/skills/`). It creates `test/spec/<ql>/<name>.txtar` with the right section headers (`-- input --`, `-- sql --`, `-- chplan --`, optional `-- seed --` / `-- expected_rows --`); run `just update-golden` after the implementation lands to fill in expected sections.
- **Add an optimizer rule** — use the `/cerberus:add-optimizer-rule` skill. Scaffolds `internal/optimizer/<name>.go` + test + TXTAR fixtures.
- **Add a property test** — add a row to the generator + oracle under `test/property/{gen,oracle}/` and a case to `test/property/promql_test.go`. The framework wires `rapid.Check` → dataset gen → chDB exec → oracle → comparator; you only swap the data shape + oracle. Build-tagged `chdb`; runs in the `chdb` workflow only.
- **Bump parser deps** — use the `/cerberus:bump-parser-deps` skill. Runs `go get -u` on the three upstream parsers, runs `go mod tidy`, captures the diff for the PR description.
- **Run E2E locally** — `just e2e-up && just e2e-seed && just e2e-run && just e2e-down`.
- **Run the compatibility suite** — `just compat-promql`. Diffs cerberus against reference Prometheus on a deterministic OTel fixture.
- **Find which test layer covers a class of bug** — see [`docs/test-strategy.md`](docs/test-strategy.md) for the layer map + per-layer "catches X / misses Y" guidance.

## Toolchain notes

- **Go version** — `go.mod` may pin a newer Go than what's installed system-wide. `GOTOOLCHAIN=auto` (the default) silently downloads the right version into `~/go/pkg/mod/golang.org/toolchain@...`. The `.envrc` (loaded by `direnv allow`) puts both the system Go and the downloaded toolchains on PATH.
- **CGO** — left at the platform default so `go test -race` works. Goreleaser pins `CGO_ENABLED=0` for release builds independently.
- **`golangci-lint` v2** — the config in `.golangci.yml` uses the v2 schema. `gofumpt` + `goimports` are configured under `formatters`, not `linters`. The v2 install path is `github.com/golangci/golangci-lint/v2/cmd/golangci-lint` (note the `/v2/`).

## Upstream parser deps — all four flow through tsouza/* forks

All four upstream parser / schema deps in `go.mod` are routed through `github.com/tsouza/*` forks pinned to **semver tags** (not pseudo-versions):

```text
replace github.com/prometheus/prometheus                                                                => github.com/tsouza/prometheus                                                                v0.0.1-cerberus-parser
replace github.com/grafana/loki/v3                                                                      => github.com/tsouza/loki/v3                                                                    v3.0.0-cerberus-parser
replace github.com/grafana/tempo                                                                        => github.com/tsouza/tempo                                                                      v0.0.1-cerberus-accessors
replace github.com/open-telemetry/opentelemetry-collector-contrib/exporter/clickhouseexporter            => github.com/tsouza/opentelemetry-collector-contrib/exporter/clickhouseexporter                v0.0.1-cerberus-ddl
# (plus three sibling submodule replaces under the same fork)
```

The fork repos exist primarily as a **Dependabot watch boundary**: cerberus consumes only a narrow subtree of each upstream, so we don't want a Dependabot PR every time upstream cuts a release. Instead, [`tsouza/cerberus-forks-monitor`](https://github.com/tsouza/cerberus-forks-monitor) runs a daily cron that rebases each `cerberus-*` branch onto `upstream/main`, runs subtree tests, and **only mints a new patch tag if commits touched the watched paths**. Dependabot in cerberus then sees a clean stream of "this is a change cerberus actually cares about" tags. See [`docs/upstream-forks.md`](docs/upstream-forks.md) for the full flow.

Two of the four forks carry actual patches:

- **`tsouza/tempo:cerberus-accessors`** — ~6 accessor methods on top of `pkg/traceql` to replace the `unsafe.Pointer` + `reflect.FieldByName` shims cerberus needed for `internal/traceql/`.
- **`tsouza/opentelemetry-collector-contrib:cerberus-ddl`** — hoists the `sqltemplates` package out of `internal/` so cerberus's `internal/schema/ddl/` can consume the OTel-CH exporter's DDL templates directly.

The other two (`tsouza/prometheus:cerberus-parser`, `tsouza/loki:cerberus-parser`) are unpatched — they exist solely as the Dependabot boundary.

## Transitive-dep gotcha (the one that bit us)

`go.mod` has this entry:

```text
replace github.com/hashicorp/memberlist => github.com/grafana/memberlist v0.3.1-0.20260410131411-8c2f3bdae9db
```

Grafana's Loki, Tempo, and `dskit` all use a forked memberlist internally (via their own `replace` directives). Those replaces **do not propagate** to consumers. Without our own replace, the build fails with `undefined: memberlist.NodeState`, `mlCfg.NodeSelection`, `mlCfg.PushPullNodes` from `dskit/kv/memberlist`. If you bump Loki / Tempo and the build breaks here, check whether they've updated their pinned memberlist version and bump ours in lock-step.

## GitHub identity

Local SSH config has two GitHub identities:

- `github.com` (default) → `tsouza-squid` (work). **Don't push cerberus work through this.**
- `github.com-tsouza` → `tsouza` (personal). All cerberus commits + pushes go through this alias. Remote URL is `git@github.com-tsouza:tsouza/cerberus.git`.

`gh` CLI may need `gh auth switch -u tsouza` if it lands on a different account. Project ops require the `project` scope on the `tsouza` token.

## Pointers if you're lost

- "How does this PR ship?" → branch + push + `gh pr create` → CI must pass → squash-merge with `gh pr merge --squash --delete-branch`.
- "Where do I add this feature?" → match the layer to the head: `internal/{promql,logql,traceql}/` for parse + lowering, `internal/chplan/` for the shared IR, `internal/optimizer/` for rewrites, `internal/chsql/` for SQL emission, `internal/api/{prom,loki,tempo}/` for HTTP handlers. Fixtures live in `test/spec/<head>/`.
- "Can I update the Project from a PR?" → yes, the repo is linked. Move the matching draft item to `In Progress` when you start, `Done` when the PR merges (or wire a workflow that does it).
