# Cerberus — agent context

<!-- AGENTS.md is a symlink to this file: both names resolve to these exact bytes, so this
     context is shared with every agent tool rather than duplicated. There is deliberately no
     `@AGENTS.md` import line — it would import this file into itself. -->

Drop-in **Prometheus / Loki / Tempo** HTTP gateway for **ClickHouse**. Each query language is parsed
(PromQL with the upstream Apache prometheus parser; LogQL and TraceQL with cerberus's own in-house
Apache reimplementations), lowered into a shared plan IR (`internal/chplan`), rewritten by a
rule-based optimizer, and emitted as parameterised ClickHouse SQL. The HTTP layer speaks the upstream
Prom / Loki / Tempo wire format, so Grafana sees cerberus as three drop-in datasources.

## Build and test commands

`just` is the canonical task runner and lists every recipe. Never reach for a bare `go test ./...`
when a recipe exists — the recipe sets the race flag, the cover profile, the build tags and the
toolchain.

- `just` — list every recipe.
- `just ci` — the CI entry point: `lint` + `test` + `build`.
- `just test` — `test-unit` (race-detected unit + spec suite) plus `vet-tagged`.
- `just test-unit` — `go test -race ./...`.
- `just vet-tagged` — type-check the build-tagged migration lanes no other recipe compiles.
- `just build` — build cerberus into `./bin`.
- `just lint` — `golangci-lint run` over both the untagged and the tagged build configurations.
- `just lint-actions` — `actionlint` over the workflow files.
- `just lint-md` / `just fmt-md` — markdownlint-cli2 verify / auto-fix.
- `just fmt` — `gofumpt` + `goimports -local` over the tree.
- `just update-golden <shard>...` — regenerate the named generated artefacts (see invariant 9).
- `just coverage` — coverage profile + floor check.
- `just property` — rapid-based property tests (needs chDB).
- `just mutate` / `just mutate-pkg PATH` — gremlins mutation run.
- `just chdb-install` — install `libchdb.so`, required by every chdb-tagged lane.
- `just hooks-install` — one-shot after clone: install lefthook and activate the git hooks.
- `just e2e` — full k3d + Grafana + Playwright smoke.
- `just e2e-up` / `e2e-seed` / `e2e-run` / `e2e-down` — the e2e lane, one step at a time.
- `just compat-promql` / `compat-logql` / `compat-traceql` / `compat-all` — differential harnesses
  against the reference backends.
- `just migration-tier1` / `migration-tier2` — the migration lane tiers.
- `just deps-tidy` — `go mod tidy`.

## Architecture decision tree

Where a change belongs, by the question it answers:

- **"The query language parses or rejects the wrong thing"** → `internal/promql/`,
  `internal/logql/`, `internal/traceql/`. PromQL uses the upstream parser; LogQL parses in
  `internal/logql/lsyntax` (with `internal/logql/logpattern` and `internal/drain`); TraceQL parses
  in `internal/traceql/ast`.
- **"The query lowers to the wrong plan"** → the same three packages' `lower.go`.
  `internal/promql/lower.go` is the canonical pattern; mirror it for the other two heads.
- **"The plan IR cannot express this"** → `internal/chplan/` (`Scan`, `Filter`, `Project`,
  `Aggregate`, `RangeWindow`, `Limit`, and the `Expr` tree). Shared by all three heads.
- **"The plan is correct but slow"** → `internal/optimizer/`, a rule-based fixpoint driver with a
  Pattern API and an analyzer/optimizer rule split (transposes, PREWHERE promotion, late
  materialisation).
- **"The SQL is wrong"** → `internal/chsql/`, the plan → ClickHouse SQL emitter. Typed Frags only
  (invariant 10).
- **"The HTTP response shape is wrong"** → `internal/api/prom/`, `internal/api/loki/`,
  `internal/api/tempo/`.
- **"It talks to ClickHouse wrong"** → `internal/chclient/` (clickhouse-go/v2 wrapper).
- **"The table or column layout is wrong"** → `internal/schema/` (OTel-CH default + override config).
- **"A configuration knob is missing"** → `internal/config/` (`cerberus.yaml` + `CERBERUS_*` env,
  env wins).
- **Entrypoint** → `cmd/cerberus/`.

Test layers, by what they pin:

- `test/spec/` — TXTAR goldens: input QL → SQL, `-- chplan --` IR snapshots, optional `-- seed --` /
  `-- expected_rows --` chDB roundtrip. The promql spec lane runs **pre-optimizer**, so an optimizer
  rule is verified through the optimizer's own layer, not here.
- `test/property/` — oracle-based property tests (`pgregory.net/rapid` + chDB execution).
- `test/regression/` — meta-tests pinning past CI failures so they cannot silently recur.
- `test/e2e/` — k3d cluster + Grafana Playwright smoke.
- `compatibility/{prometheus,loki,tempo}/` — differential harnesses against the reference backends.

`docs/test-strategy.md` holds the canonical 14-layer test map, the CI-gate inventory, and the
per-layer "catches X / misses Y" guidance.

## Hard invariants (non-negotiable)

1. **PR-per-change.** Branch protection rejects direct pushes to `main` and requires a large set of
   status checks. The source of truth for that set is
   `gh api repos/tsouza/cerberus/branches/main/protection --jq '.required_status_checks.contexts[]'`;
   `docs/test-strategy.md` is the per-gate reference. **Never `gh pr merge --admin`** — if a required
   check is red, fix the code or fix the workflow. Ship with
   `gh pr merge --squash --delete-branch`.
2. **A pushed branch gets a PR in the same breath.** The push and the `gh pr create` are one step. A
   branch with no PR is invisible: no gate reports on it, no reviewer sees it, and its commits get
   stranded. "Not ready to review" is a reason to open the PR as a draft, never a reason to skip it.
   Branch off the current `origin/main`, never off another in-flight branch, and never push to the
   head branch of a PR that has already merged.
3. **Out-of-scope work becomes a GitHub Issue, filed before the PR that found it merges.** A sentence
   in a PR body is not a resting place — it is invisible the moment the PR merges. Verify the finding
   first; a phantom issue is as bad as a dropped one. The required `forbid-deferral` check
   (`.github/scripts/forbid-deferral.mjs`) scans a change's own additions — its description, the
   commit messages in its range, and the `+` lines of its diff — and fails unless each deferral
   marker cites an issue in this repository that is open and is an issue rather than a pull request.
   There is no tolerance file and no exemption list.
4. **Conventional Commits**, enforced by `commitlint` (`.commitlintrc.json`). Subject ≤ 100
   characters.
5. **No speculative pre-flight, but a red check is reproduced locally before the next push.** These
   are two halves of one rule, and the second half wins wherever they meet.
   - *Before* a push, do not run `just test`, `just lint`, `go test`, `golangci-lint run`,
     `go build`, or `markdownlint-cli2` as a ritual over the whole tree. `lefthook.yml` is layered
     and every layer is cheap: `pre-commit` runs sub-second formatters on staged files, `commit-msg`
     runs commitlint on the message being written, and `pre-push` runs the `forbid-skip` and
     `repo-hygiene` scans plus `actionlint` in about a second. Compilation, whole-tree walks and
     containers belong to CI, which runs them once on the commit that merges. `LEFTHOOK=0 git push`
     bypasses for WIP branches.
   - *After* CI reports a red check, **reproduce that exact failure locally, narrowed to the thing
     that failed**, and confirm the fix locally before pushing again. Pushing a speculative fix and
     waiting on CI to find out is forbidden: a full run is ~19 checks and ~40 minutes, so a
     guess-and-wait loop burns CI credits and wall-clock for feedback a targeted local run gives in
     seconds. Narrow to the failing unit — `go test -run '^TestName$/^subtest$' ./internal/<pkg>/`,
     or the single `node .github/scripts/<gate>.mjs`. Never re-run a whole recipe to check one test,
     and never invoke `go test` directly where a recipe exists; the recipe carries the build tags,
     the race flag and the timeouts, and bypassing it produces failures the recipe would not have.

   Three lanes genuinely cannot be settled locally, and only these three justify a CI round-trip:
   `lint` (golangci-lint in an agent worktree reports "No issues found" without analysing — a local
   green is not evidence), `strict-scan` and the compat lanes (need a real ClickHouse, not chDB), and
   `e2e` / `compose-smoke` / crawl (need k3d or Docker Compose). For those, read the CI log for the
   exact rule, `file:line`, or assertion and reason about it directly. Concurrency hazard: this repo
   has no per-worktree Docker Compose project-name isolation, so a compose run from one worktree
   corrupts another's containers — never start compose while another agent is live.
6. **Tests assert or are removed.** Never `t.Skip`, soft-assert, silent-recover, or `should_skip` a
   test. A feature that cannot run on the CI substrate (for example a CH function above the chDB
   floor) is gated at runtime and validated elsewhere, never skipped. `forbid-skip` enforces this
   behaviourally.
7. **No allow-lists, no tolerance files, no expected-failure sets.** Compatibility is the source of
   truth for all three heads, and every diff against a reference backend is a real bug to fix at the
   source. The single pinned exclusion set is `compatibility/loki/upstream-skip-baseline.txt` — the
   corpus entries upstream itself marks `skip: true`, for which no reference baseline exists — and
   the harness fails on any drift in it.
8. **"Pre-existing" is not an escape hatch.** Diagnosing a bug as pre-existing routes *which*
   branch or PR fixes it; it never decides *whether* it gets fixed. The same applies to "adjacent"
   and "out of scope". Never label a real failure a flake without evidence and a fix.
9. **Never hand-edit a generated artefact — regenerate it.** `just update-golden <shard>...` rewrites
   the TXTAR goldens, the migration goldens, the solver decision baseline, the parity ledgers, and
   the cardinality baseline. The shard argument is **required** — `promql`, `logql`, `traceql`,
   `chsql`, `optimizer`, `codegen`, `migration`, `parity`, `solver`, `cardinality`, or `all` — and a
   bare invocation errors and prints the vocabulary. Naming too few is safe rather than a trap: the
   recipe derives the shards the branch's own diff implies are stale and refuses to start until the
   set covers them, printing the exact command. It needs `libchdb.so` (`just chdb-install`), without
   which the chdb-tagged `-- expected_rows --` cells go stale. Every generated path is marked
   `-merge` in `.gitattributes`
   precisely because line-merging one produces a file that still parses and is silently wrong.
   Review `git diff test/spec/ test/e2e/migration/archetypes/ test/perf/ test/surface-parity/
   test/rejection-parity/` before committing. A few ledgers sit outside `update-golden` because they
   need Docker or the `agpl_oracle` tag; the recipe's own comments name each one and its regeneration
   command.
10. **No raw SQL strings — typed chsql API only.** Compose clauses via `chsql.QueryBuilder` slots and
    expressions via typed Frags (`Eq` / `And` / `Call` / `Cast` / `Lambda1` / `Subquery` /
    `InlineLit` and friends). Any CH function is `Call("fn", args…)`; arithmetic is
    `Mul/Add/Sub/Div/Mod/Neg`. Writing SQL tokens into a `strings.Builder`, through `writeSQL(...)`,
    through `fmt.Sprintf`, or by `+`-concatenation is forbidden everywhere except the Frag-primitive
    constructors in `internal/chsql/builder.go`. `verbatim(...)` is for emitter-chosen synthetic
    tokens (alias names, pre-quoted literals, pre-rendered subquery SQL), never for whole expression
    shapes. Self-check before any chsql change: `node .github/scripts/forbid-sql-raw.mjs` from the
    repo root. CI catches the token-writing primitives; reviewer discipline catches the semantic
    shape.
11. **No `unsafe.Pointer` / `reflect.FieldByName` against upstream parser internals.** When a parser
    does not expose what cerberus needs, add the accessor to the relevant `tsouza/*:cerberus-*` fork
    (`docs/upstream-forks.md`), bump the `replace` in `go.mod`, and consume the typed accessor. The
    `forbidigo` linter enforces both patterns across all of `internal/**`.
12. **No caching of query results.** Cerberus never caches the answer to a query. Internal
    performance caches are acceptable only where staleness is proven output-safe.
13. **No magic constants.** A meaning-bearing numeric literal must be a named `const` whose name *is*
    the explanation: `if n > 200` becomes `const maxSearchRecentLimit = 200`. If no short honest name
    fits, the number is probably wrong. Out of scope: self-evident `+1` / `-1`, trivial `0` / `1` /
    `2` loop bounds, and slice-capacity hints.
14. **No AGPL in the binary.** The upstream LogQL and TraceQL parsers are AGPLv3; the in-house
    reimplementations exist so the Apache-2.0 binary never links them. They survive only as
    test-only oracles behind the `agpl_oracle` build tag, quarantined in the `test/oracle` nested
    module. The `agpl-clean` gate fails the build if any AGPL package reaches `cmd/cerberus`.
15. **Non-trivial CI step logic lives in `.github/scripts/*.mjs`, not inline YAML.**
    Dependency-light Node ESM, `node:` builtins only, env-driven inputs documented at the top of the
    file, `::error::` / `::notice::` workflow commands, and `process.exit(1)` on failure. Trivial
    one-liners and official Actions usage stay inline. See `.github/scripts/README.md`.
16. **A new `internal/**` package must be declared in `.go-arch-lint.yml`.** The gate is CI-only, so
    an undeclared package passes locally and fails on the PR.
17. **Subagent worktree isolation.** Every git and filesystem operation happens inside the assigned
    worktree path, passed absolutely on every call. Never operate on another checkout of this
    repository — the object store is shared but the branch is not, so a stray `git commit` lands on
    somebody else's branch. `docs/agent-workflow.md` has the recovery procedure.

## Workflow for a non-trivial change

Plan first, code last. Plan mode → requirements interview → a spec at `docs/specs/<feature>.md` → a
numbered task list for owner review → only then implementation. Trivial changes (a typo, a one-line
fix with an obvious test) skip straight to the PR. `docs/agent-workflow.md` describes each stage and
what a spec must contain.

## Common workflows

- **Add a TXTAR fixture** — `.claude/skills/cerberus-add-fixture.md`. Creates
  `test/spec/<ql>/<name>.txtar` with the right section headers; run `just update-golden <shard>...`
  once the implementation lands to fill in the expected sections.
- **Add an optimizer rule** — `.claude/skills/cerberus-add-optimizer-rule.md`. Scaffolds
  `internal/optimizer/<name>.go` plus its test and fixtures.
- **Add a property test** — add a row to `test/property/{gen,oracle}/` and a case to
  `test/property/promql_test.go`. Build-tagged `chdb`.
- **Bump parser deps** — `.claude/skills/cerberus-bump-parser-deps.md`.
- **Run E2E locally** — `just e2e-up && just e2e-seed && just e2e-run && just e2e-down`.
- **Run a compatibility suite** — `just compat-promql` (or `compat-logql` / `compat-traceql`).

## Reference docs

Read these when the task touches them. They are deliberately linked as paths rather than imported,
so they cost nothing until something needs them.

- `docs/engine.md` — the shared query pipeline, the `Lang` contract, and the extension points a new
  head plugs into.
- `docs/test-strategy.md` — the 14-layer test map, the CI-gate inventory, the gremlins rollout.
- `docs/operations.md` — runtime contract, configuration, lifecycle, scaling, the release ritual, and
  the support window.
- `docs/compatibility.md` — the three differential harnesses and their ratchets.
- `docs/agent-workflow.md` — the PR ritual in detail, the deferral-to-issue rule, worktree isolation
  and its recovery procedure, the plan-mode / spec workflow, and the checked-in Claude Code harness
  (hooks and project subagents).
- `docs/toolchain.md` — Go toolchain, CGO, golangci-lint v2, the gremlins mutation-testing fork, and
  the dependency gotchas.
- `docs/upstream-forks.md` — the `tsouza/*` fork boundary, the parser dependency map, and the
  transitive-dependency `replace` entries.
- `docs/observability.md`, `docs/health.md`, `docs/performance.md`, `docs/solver.md`,
  `docs/coverage.md`, `docs/forbid-skip.md`, `docs/migration.md` — subsystem references.
