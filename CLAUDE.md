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
- **Compatibility is the source of truth for all three heads.** The unified `compatibility.yml` workflow runs on pull_request + push-to-main + nightly + manual dispatch. All three jobs — `compatibility/prometheus`, `compatibility/loki`, `compatibility/tempo` — are required status checks on `main` (gate flip completed 2026-05-19; verified on 2026-05-21 with 9/9 consecutive green runs after the standalone `tempo-compatibility.yml` workflow was deleted and consolidated into this one). There are **no allow-lists**: the old `expected-failures.json` mechanism is deleted, and every diff against a reference backend is a real bug to fix at the source. The only pinned exclusion set is `compatibility/loki/upstream-skip-baseline.txt`, which records the corpus entries *upstream itself* marks `skip: true` (no reference baseline exists for them); the harness fails on any drift in that set (see `docs/compatibility.md`).
- **Non-trivial CI step logic lives in `.github/scripts/*.mjs`, not inline YAML.** Multi-line `bash` / `jq` / `awk` / `perl` embedded in a workflow `run:` block that encodes real logic (discipline greps, threshold gates, baseline/ref resolution, score-summary formatting) belongs in a dependency-light Node ESM module under `.github/scripts/` — env-driven inputs (documented at the top of the file), `::error::` / `::notice::` workflow commands, and `process.exit(1)` on failure / `0` on success, preserving the exact behaviour of the bash it replaces. Import only `node:` builtins (no npm deps, no `@actions/*`); `ubuntu-latest` ships node, so `run: node .github/scripts/<name>.mjs` needs no `setup-node`. Shared `::error::` / `git` / `lsFiles` helpers live in `.github/scripts/lib/gh.mjs`. This keeps step logic testable (run the `.mjs` locally), lintable, and reusable across jobs (e.g. the three compatibility heads share one summary script). **Trivial one-liners and official Actions usage stay inline.** See `.github/scripts/README.md` for the module + env-contract inventory.
- **No raw SQL strings — typed chsql API only.** Use `internal/chsql.Builder` / `chsql.QueryBuilder` — the custom CH-flavored builder API. Compose clauses via typed `QueryBuilder` slots (`.Select` / `.From` / `.Where` / `.GroupBy` / `.OrderBy` / `.Limit` / `.Prewhere` / `.Join` / `.WithRecursive`) and expressions via typed Frags (`Eq` / `And` / `Or` / `Paren` / `Cast` / `In` / `Like` / `Add` / `Call` / `Array` / `Subscript` / `If` / `Lambda1` / `Subquery` / `BareIdent` / `InlineLit` / etc.). The typed-Frag surface is closed by construction: external packages cannot raw-write SQL. Add new typed constructors when a shape isn't covered; never compose SQL via string concatenation. Reviewer discipline + the typed API are the enforcement.
  - **CI can't catch this — it's an agent-level discipline gate.** Writing SQL tokens into a `strings.Builder` (`b.sb.Write*`), `b.writeSQL(...)`, `fmt.Sprintf` of SQL, or `+`-concatenating SQL is **forbidden EVERYWHERE EXCEPT the Frag-primitive constructors in `internal/chsql/builder.go`** (`Call` / `binOp` / `Cast` / `Paren` / `InlineLit` / the QueryBuilder clause renderer — these *implement* the typed surface, so they legitimately write tokens). The domain emitters (`range_window.go`, `absent_over_time.go`, `range_lwr.go`, `emit_node.go`, `histogram_over_time.go`, `range_bucket_fanout.go`, `metrics_compare.go`, `histogram_quantile*.go`, `vector_join.go`, `structural_join.go`, …) build query/expression SHAPES and MUST compose Frags — any CH function is `Call("fn", args…)`, arithmetic is `Mul/Add/Sub/Div/Mod/Neg`, comparisons are `Lt/Gt/Eq/…`, lambdas are `Lambda1/Lambda2` with `BareIdent("i")` params. `verbatim(...)` is reserved for emitter-chosen synthetic tokens (alias names, pre-quoted literals, pre-rendered subquery SQL) — never for whole expression shapes.
  - **Self-check before any chsql change:** `grep -rn 'sb.Write\|writeSQL\|strings.Builder' internal/chsql/ | grep -v builder.go` should surface nothing but the sanctioned `rawAs` synthetic-alias helper and the pre-rendered-subquery splice. A non-empty list anywhere else is a regression — recompose it as Frags.
  - **Before/after** (the `epochAlignedEndFrag` / `durationToStart` shape): raw `b.sb.WriteString("fromUnixTimestamp64Nano(intDiv(toUnixTimestamp64Nano("); end(b); b.sb.WriteString(") / step) * step)")` → `Call("fromUnixTimestamp64Nano", Mul(Call("intDiv", Call("toUnixTimestamp64Nano", end), step), step))`. `Call`/binOp add no parens and `InlineLit(int64)` emits the bare integer, so the typed form is byte-identical to the hand-rolled string — regenerate goldens and confirm zero churn.
- **No `unsafe.Pointer` / `reflect.FieldByName` against upstream parser internals — fork + accessors instead.** Cerberus's `internal/**` packages must never reach into unexported fields of `prometheus`, `loki`, or `tempo` parser ASTs via `unsafe.Pointer` or `reflect.Value.FieldByName`. When a parser doesn't expose what cerberus needs, add the accessor to the relevant `tsouza/*:cerberus-*` fork (see `docs/upstream-forks.md`), bump the `replace` in `go.mod`, and consume the typed accessor. The `forbidigo` linter enforces both patterns project-wide across `internal/**` (see `.golangci.yml`); the gate started life scoped to `internal/traceql/` + `internal/api/tempo/` (the original shim sites) and was widened to all of `internal/` as forward-looking hygiene — zero current violations exist, the gate exists to keep it that way.
- **No magic constants.** A meaning-bearing numeric literal sitting inline — `x + 5_000_000_000`, `n > 200`, `now64(9)` — must be lifted to a named `const` with a short name that *is* the explanation. The name carries the meaning; ideally no comment block is needed at all (one terse line max if the value's rationale isn't obvious from the name). If you can't find a short, honest name for the number, that's the signal it's probably wrong or unnecessary — stop and rethink, don't paper over it with a comment. Out of scope: self-evident `+1`/`-1` (next index / 1-based / grid-anchor count), trivial `0`/`1`/`2` loop bounds, slice-capacity hints (`make([]T, 0, 64)`), and literals already named. Before/after: `if n > 200 { n = 200 }` → `const maxSearchRecentLimit = 200` then `if n > maxSearchRecentLimit { … }`.
- **Subagent worktree isolation — stay in your assigned path.** When the harness dispatches you with an isolated worktree under `.claude/worktrees/agent-<id>/`, every git / filesystem operation you make MUST happen inside that path. **Never `cd /home/thiago/workspace/cerberus`** (or any other cerberus checkout) — the main checkout shares the same `.git` object store but is on a different branch, so a `git commit` or `git checkout` run from there will land your work on whichever branch another concurrent agent has checked out (see "Why this is a hard rule" below). Use the worktree path the runtime gave you verbatim: pass absolute paths to `Bash`, `Read`, `Write`, and `Edit` calls (or `cd` once at the top of a compound command). If a tool call resets cwd between invocations, re-anchor with an absolute path every time — don't trust the inherited cwd. See [Subagent worktree isolation](#subagent-worktree-isolation) for the recovery procedure if you suspect contamination has already happened.

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

- **Add a TXTAR fixture** — use the `/cerberus:add-fixture` skill (under `.claude/skills/`). It creates `test/spec/<ql>/<name>.txtar` with the right section headers (`-- input --`, `-- sql --`, `-- chplan --`, optional `-- seed --` / `-- expected_rows --`); run `just update-golden` after the implementation lands to fill in expected sections — it covers both the default-tag text goldens and the chdb-tagged `-- expected_rows --` cells (requires libchdb.so via `just chdb-install`).
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
replace github.com/open-telemetry/opentelemetry-collector-contrib/exporter/clickhouseexporter            => github.com/tsouza/opentelemetry-collector-contrib/exporter/clickhouseexporter                v0.0.2-cerberus-ddl
# (plus three sibling submodule replaces under the same fork)
```

The fork repos exist primarily as a **Dependabot watch boundary**: cerberus consumes only a narrow subtree of each upstream, so we don't want a Dependabot PR every time upstream cuts a release. Instead, [`tsouza/cerberus-forks-monitor`](https://github.com/tsouza/cerberus-forks-monitor) runs a daily cron that rebases each `cerberus-*` branch onto `upstream/main`, runs subtree tests, and **only mints a new patch tag if commits touched the watched paths**. Dependabot in cerberus then sees a clean stream of "this is a change cerberus actually cares about" tags. See [`docs/upstream-forks.md`](docs/upstream-forks.md) for the full flow.

Two of the four forks carry actual patches:

- **`tsouza/tempo:cerberus-accessors`** — ~6 accessor methods on top of `pkg/traceql` to replace the `unsafe.Pointer` + `reflect.FieldByName` shims cerberus needed for `internal/traceql/`.
- **`tsouza/opentelemetry-collector-contrib:cerberus-ddl`** — hoists the `sqltemplates` package out of `internal/` so cerberus's `internal/schema/ddl/` can consume the OTel-CH exporter's DDL templates directly.

The other two (`tsouza/prometheus:cerberus-parser`, `tsouza/loki:cerberus-parser`) are unpatched — they exist solely as the Dependabot boundary.

## Tooling fork — gremlins (mutation testing)

`mutation.yml` consumes the **`tsouza/gremlins`** fork (installed via `go install github.com/tsouza/gremlins/cmd/gremlins@v0.6.0-cerberus-sigterm-consume`) instead of upstream `go-gremlins/gremlins@v0.6.0`. The fork carries a single fix on top of `v0.6.0`:

- Upstream's signal handler closes the channel that `os/signal` still writes to, so a second signal (typical CI runner sequence: SIGTERM → SIGKILL) panics with `send on closed channel` from `signal.process`. Worse, mutants whose `go test` subprocess is still running at cancellation time fall through `runTests` to the default `return mutator.Lived` branch (the per-test ctx is rooted in `context.Background()`, so only `DeadlineExceeded` is checked). The result on cerberus PR #664 / push-to-main run 26213450154: four untested mutants recorded as LIVED, deflating `test_efficacy`.
- The fork fixes both: the signal handler no longer self-closes, and the executor now threads the engine's run ctx into the per-test ctx so cancelled-in-flight mutants are reported with the status from the new `--on-shutdown-status` flag. Cerberus passes `--on-shutdown-status=not-run` so cancelled-in-flight mutants land in `NOT_COVERED`, outside the `KILLED / (KILLED + LIVED)` efficacy formula entirely. Upstream PR: <https://github.com/go-gremlins/gremlins/pull/283>.

The fork ships two parallel branches/tags by design:

- **`cerberus-sigterm-fix` @ `v0.6.0-cerberus-sigterm`** — the branch the upstream PR is built from. Keeps the upstream module path `github.com/go-gremlins/gremlins` so the diff stays clean and reviewable.
- **`cerberus-sigterm-fix-consume` @ `v0.6.0-cerberus-sigterm-consume`** — the branch cerberus's `mutation.yml` installs. Adds a single extra commit that renames the `go.mod` module path to `github.com/tsouza/gremlins` (and rewrites all internal imports), because `go install github.com/tsouza/gremlins/cmd/gremlins@...` rejects the module otherwise with `module declares its path as: github.com/go-gremlins/gremlins`. The fix itself is identical to the upstream-PR branch.

Unlike the four parser forks, this one is not on the Dependabot-watch flow — it's a build-time tool, not a Go module dep, and both branches will be retired once the upstream PR lands and a release tag is cut.

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

## Subagent worktree isolation

The dispatcher gives every subagent its own worktree under `.claude/worktrees/agent-<id>/`, on its own per-agent branch (typically `worktree-agent-<id>` or a task-specific branch you create off `origin/main`). The main checkout at `/home/thiago/workspace/cerberus` is itself a worktree of the same repo — it shares the `.git` object DB but is on whatever branch the maintainer last had checked out (often the most recent task branch — `git worktree list` showed e.g. `fix/tempo-search-traceid-zero-pad-emit` while three different subagents were running concurrently on unrelated tasks).

### Why this is a hard rule

Three separate post-mortems (issues #207, #209, #210) reported the same shape: an agent doing work in its isolation worktree somehow saw commits land on the main checkout's branch, or saw an unrelated branch's commits show up in their tree. The investigation under task #213 traced the root cause to subagents using `/home/thiago/workspace/cerberus` (the main checkout path) for git / file operations — the path their briefing pasted in as "the cerberus repo" — instead of their assigned `.claude/worktrees/agent-<id>/` path. When two agents both ran `git commit` from the main checkout, their commits stacked onto whichever branch was currently checked out there, contaminating an unrelated PR. The same root cause hits `Edit` and `Write` tool calls — if you pass `/home/thiago/workspace/cerberus/<file>` as `file_path`, the edit lands in the main checkout's working tree, not your worktree's.

Git's worktree machinery itself does protect against the obvious failure modes — the same branch can't be checked out in two worktrees, and each worktree has its own `.git/worktrees/<id>/HEAD` ref — but **none of that helps if you run git from the wrong path.** The protection is path-based, not agent-based.

### Recovery procedure (if contamination is suspected)

1. `cd` into your assigned worktree path (`/home/thiago/workspace/cerberus/.claude/worktrees/agent-<your-id>/`).
2. `git worktree list` — verify your worktree shows up with the expected branch name. If it doesn't, you have been operating in the wrong tree the whole time.
3. From your assigned worktree, run `git log --oneline origin/main..HEAD` and compare against the work you actually did. Any commit you don't recognize is contamination.
4. From the main checkout (`/home/thiago/workspace/cerberus`), run `git status` and `git log --oneline` on whichever branch is checked out there. Uncommitted edits or commits you authored that don't belong to that branch's PR are the bleed-through.
5. If the bleed-through is uncommitted: from the main checkout, `git checkout -- <files>` to revert (only when you are certain nothing else is in flight there — when in doubt, ask the maintainer rather than `git checkout --` a shared tree). Then re-apply the change in the correct worktree using the correct absolute path.
6. If the bleed-through is committed: cherry-pick contaminated commits onto the correct branch (`git cherry-pick <sha>` from inside the right worktree), then revert them from the wrong one (`git revert <sha>` + push). Don't `git reset --hard` a shared branch — that rewrites history other agents may have pushed.
7. Open a follow-up note on task #213 with the contamination SHAs / file paths + which worktree paths were involved, so the pattern doesn't repeat silently.

### Defence-in-depth (future work)

Documentation is the cheapest fix and the one this section implements. A more durable option is for the dispatcher to use `git worktree add --detach .claude/worktrees/agent-<id>` then have the subagent create its own branch — that way no branch is shared between the main checkout and any worktree, and a stray `git commit` from the wrong path lands on a detached HEAD that's trivially recoverable. That structural change is tracked separately; until it lands, the rule above is the gate.
