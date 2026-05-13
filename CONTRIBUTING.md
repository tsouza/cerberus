# Contributing to cerberus

Thanks for your interest. cerberus is small + opinionated — here's how to land a change.

## The 30-second version

```sh
git checkout -b feat/<short-name>
# ...edit...
just lint && just test
git commit -m "feat(<scope>): <subject>"   # Conventional Commits
git push -u origin <branch>
gh pr create
```

Two CI checks gate `main`: **`ci / check`** (golangci-lint + race tests + build) and **`ci / lint`** (commitlint + markdownlint). Branch protection is strict — your branch must be up-to-date with `main` before merging. Squash is the only merge style.

## House rules

1. **PR-per-change.** No direct pushes to `main` — branch protection rejects them. Even tiny fixes go through a PR.
2. **Issues are open for bug reports + design discussion.** File one if you want to flag a bug, propose a feature, or ask a design question before implementing. The internal maintainer workflow (and AI assistants helping with the project) tracks active work in [the Cerberus v1.0.0 Roadmap Project](https://github.com/users/tsouza/projects/1) plus PR descriptions; as an external contributor you don't need to use the Project — a clear issue + a follow-up PR is fine.
3. **Conventional Commits.** Subjects look like `feat(promql): support offset modifier` or `chore(deps): bump grafana/loki/v3 to v3.8.0`. `subject-case` is relaxed (Dependabot's `Bump X from Y to Z` passes); type + scope are still enforced. `commitlint.config.json` is the source of truth.
4. **Justfile is the canonical task runner.** `just` lists every recipe. Don't run `go test ./...` directly — `just test` sets the race flag, cover profile, and correct toolchain. If you want a new workflow, add a recipe.
5. **Fixture-first PRs.** A new PromQL/LogQL/TraceQL feature lands with its TXTAR spec first (failing, with the right `query.<ql>` section), then implementation that turns it green. Reviewers can sanity-check intent by reading fixtures before code.
6. **Compatibility suite is the source of truth.** If a PromQL feature lands but doesn't move the `prometheus/compliance` pass rate, the PR is incomplete. Adding an entry to `harness/compatibility/expected-failures.json` requires a comment with the upstream rationale; never empty-string.

## Setup

```sh
git clone git@github.com-tsouza:tsouza/cerberus.git   # or HTTPS; SSH alias if you have multiple GH identities
cd cerberus
direnv allow                                          # loads .envrc; puts Go + GOTOOLCHAIN=auto on PATH
just install-tools                                    # one-time: golangci-lint, gofumpt, goimports, gremlins
just hooks-install                                    # one-time: lefthook (pre-commit formatters + commit-msg lint)
just ci                                               # lint + test + build
```

Hooks are lightweight: `pre-commit` runs `gofumpt -w` / `goimports -w` /
`markdownlint-cli2 --fix` on staged files (auto-fixes; restages), and
`commit-msg` runs `commitlint` so a malformed subject is caught locally
instead of in CI. Heavy validation (`go test`, `golangci-lint run`,
`go build`) deliberately is **not** in the hook — CI owns that. Don't
pre-flight manually.

If `direnv allow` complains, `eval "$(direnv export bash)"` once per shell.

`go.mod` may pin a newer Go than your system Go. `GOTOOLCHAIN=auto` (the default) silently fetches the right version into `~/go/pkg/mod/golang.org/toolchain@...`; no manual install needed.

## Pull request flow

1. **Pick a milestone.** [`docs/roadmap.md`](docs/roadmap.md) lists every RC1 milestone (M0.1 → M5.4) with one-line outcomes. Lower-numbered milestones generally come first.
2. **Branch from current `main`.** Branch names match the work: `seed/m1.2-binary-expr`, `chore/bump-grafana-deps`, `fix/range-window-counter-reset`.
3. **Write the failing fixture first.** For QL changes, that's a TXTAR under `test/spec/<ql>/`. Use the `/cerberus:add-fixture` skill if you're driving Claude Code.
4. **Implement + iterate.** `just test` for the inner loop; `just lint` before pushing.
5. **Commit with Conventional Commits.** Examples in [`docs/roadmap.md`](docs/roadmap.md) milestone tables.
6. **Push + open PR.** Title matches the commit subject; body explains *why* + a Test plan checklist. Link the milestone draft in the [Project](https://github.com/users/tsouza/projects/1).
7. **CI green → squash-merge.** Branch deleted on merge.

### A good PR description

```markdown
## Summary
- One bullet per substantive change, focused on *why*.

## Test plan
- [ ] just lint clean
- [ ] just test passes (race + spec)
- [ ] new TXTAR fixture(s) reviewed: <paths>
- [ ] compatibility pass rate moved: <from> → <to>
- [ ] CI green
```

If you're touching the compatibility harness, include the JSON diff (`harness/compatibility/expected-failures.json` before/after) in the body.

## Testing layers

`docs/roadmap.md` has the full table. Headline:

- **Unit + spec (TXTAR)** — run on every PR; merge gate.
- **PromQL compatibility** — `prometheus/compliance` Docker harness; merge gate for PRs touching `internal/promql/**`, `internal/chsql/**`, `internal/optimizer/**`.
- **E2E (k3d + Grafana playwright)** — path-gated on `internal/api/**` + `deploy/**`; merge gate when touched.
- **Mutation** — Gremlins nightly; informational, investigated weekly.

## Project memory and AI assistants

If you use Claude Code (or any agent that reads `CLAUDE.md` / `AGENTS.md`), the context is pre-loaded. The three skills under `.claude/skills/` cover the most common workflows (add fixture, add optimizer rule, bump parser deps).

## Reporting security issues

See [`SECURITY.md`](SECURITY.md) — don't open public PRs for security-sensitive reports.

## Code of conduct

See [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md). Be kind; assume good faith.
