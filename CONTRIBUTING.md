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

The required checks on `main` are `check` (golangci-lint + race tests + build), `lint` (commitlint + markdownlint), `forbid-skip`, `compatibility/{prometheus,loki,tempo}`, `probe`, `roundtrip (promql|logql|traceql)`, and `compose-smoke` — see the Testing layers below for what each covers. Branch protection is strict — your branch must be up-to-date with `main` before merging. Squash is the only merge style.

## House rules

1. **PR-per-change.** No direct pushes to `main` — branch protection rejects them. Even tiny fixes go through a PR.
2. **Issues are open for bug reports + design discussion.** File one if you want to flag a bug, propose a feature, or ask a design question before implementing. The internal maintainer workflow (and AI assistants helping with the project) tracks active work in [the Cerberus v1.0.0 Roadmap Project](https://github.com/users/tsouza/projects/1) plus PR descriptions; as an external contributor you don't need to use the Project — a clear issue + a follow-up PR is fine.
3. **Conventional Commits.** Subjects look like `feat(promql): support offset modifier` or `chore(deps): bump grafana/loki/v3 to v3.8.0`. `subject-case` is relaxed (Dependabot's `Bump X from Y to Z` passes); type + scope are still enforced. `.commitlintrc.json` is the source of truth.
4. **Justfile is the canonical task runner.** `just` lists every recipe. Don't run `go test ./...` directly — `just test` sets the race flag, cover profile, and correct toolchain. If you want a new workflow, add a recipe.
5. **Fixture-first PRs.** A new PromQL/LogQL/TraceQL feature lands with its TXTAR spec first (failing, with the right `query.<ql>` section), then implementation that turns it green. Reviewers can sanity-check intent by reading fixtures before code.
6. **Compatibility suite is the source of truth.** If a PromQL feature lands but doesn't move the `prometheus/compliance` pass rate, the PR is incomplete. There is no allow-list for any head — every diff against the reference backend is a real bug to fix at the source. The only pinned exclusion set is `compatibility/loki/upstream-skip-baseline.txt`, which records corpus entries *upstream itself* marks `skip: true`; see `docs/compatibility.md`.

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

End-to-end against a real ClickHouse + Grafana in k3d:

```sh
just e2e-up            # boot k3d cluster, deploy CH / Grafana / cerberus
just e2e-seed          # ingest sample OTel data
just e2e-run           # Go E2E tests + Grafana Playwright smoke
just e2e-down          # tear down
```

`go.mod` may pin a newer Go than your system Go. `GOTOOLCHAIN=auto` (the default) silently fetches the right version into `~/go/pkg/mod/golang.org/toolchain@...`; no manual install needed.

## Pull request flow

1. **Open an issue first if the work is non-trivial.** Sketches, design questions, and bug reports go through GitHub Issues so the direction can be agreed before code lands.
2. **Branch from current `main`.** Branch names match the work: `feat/promql-binary-expr`, `chore/bump-grafana-deps`, `fix/range-window-counter-reset`.
3. **Write the failing fixture first.** For QL changes, that's a TXTAR under `test/spec/<ql>/`. Use the `/cerberus:add-fixture` skill if you're driving Claude Code.
4. **Implement + iterate.** `just test` for the inner loop; `just lint` before pushing.
5. **Commit with Conventional Commits.** Type + scope enforced by `commitlint`.
6. **Push + open PR.** Title matches the commit subject; body explains *why* + a Test plan checklist.
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

If you're touching the compatibility harness, include the before/after pass rate (`compat-score.json`) in the body — and if a Loki corpus re-snapshot moved `compatibility/loki/upstream-skip-baseline.txt`, include that diff too.

## Testing layers

[`docs/test-strategy.md`](docs/test-strategy.md) is the canonical layer
map. Headline:

- **Unit + spec (TXTAR)** — run on every PR; merge gate.
- **Compatibility** — PromQL / LogQL / TraceQL differential harnesses
  against reference Prom / Loki / Tempo; all three
  (`compatibility/{prometheus,loki,tempo}`) are required PR checks, and
  per-head scores are published to the `compat-scores` branch.
- **Compose smoke** — `compose-smoke` (the repo-root `docker compose up`
  quickstart stack) is a required PR check.
- **E2E (k3d + Grafana Playwright)** — the `dashboard` job is
  informational: it runs on push-to-main + nightly + manual dispatch,
  not as a PR gate.
- **Mutation** — Gremlins nightly; per-phase 95% efficacy threshold.

## Project memory and AI assistants

If you use Claude Code (or any agent that reads `CLAUDE.md` / `AGENTS.md`), the context is pre-loaded. The three skills under `.claude/skills/` cover the most common workflows (add fixture, add optimizer rule, bump parser deps).

## Releasing

The binary and the Helm chart (`deploy/helm/cerberus/`) follow **independent
SemVer lines**. `Chart.yaml` carries both: `version` is the *chart's* own
SemVer (its `values.yaml` contract — breaking values change = major, additive
toggle = minor, fix = patch), and `appVersion` tracks the cerberus binary it
deploys by default. They move independently so each kind of change is
expressible:

| Change                                                   | What to do                                                                                                    | What publishes                                                                                |
| -------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------- |
| **App + chart** (normal release)                         | bump `appVersion` **and** chart `version`, then push `vX.Y.Z`                                                 | goreleaser (binary/image) **and** chart-release (the version-gate sees a new chart version)   |
| **App only** (no chart change)                           | bump `appVersion` + a chart `version` **patch** (the default image tag moved = a chart change), push `vX.Y.Z` | goreleaser; chart-release publishes the patched chart                                         |
| **App only, chart truly unchanged**                      | push `vX.Y.Z`; leave Chart.yaml as-is                                                                         | goreleaser only — chart-release's version-gate skips the already-published chart version      |
| **Chart only** (template fix, new toggle, values change) | bump chart `version` per the chart's SemVer; leave `appVersion`, push **`chart-vX.Y.Z`**                      | chart-release only — goreleaser is guarded off `chart-v*` tags, so no spurious binary release |

This is why lockstep (`version == appVersion`) is **not** used: a chart-only fix
couldn't be shipped without re-tagging the app, and a breaking values change
couldn't be a chart-major while the app is a patch. See `.github/workflows/release.yml`
(the `chart-v*` trigger + the goreleaser `if:` guard) and
`.github/scripts/chart-publish.mjs` (the version-gate).

The chart is published as an OCI artifact to `ghcr.io/tsouza/cerberus/charts/cerberus`,
cosign-signed (keyless) with SLSA provenance — the same supply-chain posture as
the binary. `deploy/helm/cerberus/README.md` is generated by helm-docs from
`values.yaml`; run helm-docs and commit it when you change values (the
`chart-ci` workflow fails on drift).

## Reporting security issues

See [`SECURITY.md`](SECURITY.md) — don't open public PRs for security-sensitive reports.

## Code of conduct

See [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md). Be kind; assume good faith.
