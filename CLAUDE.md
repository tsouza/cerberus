# Cerberus — agent context

Drop-in **Prometheus / Loki / Tempo** HTTP gateway for **ClickHouse**. Parses each upstream query language with its reference parser, lowers into a shared plan IR (`internal/chplan`), applies a small rule-based optimizer, and emits parameterised ClickHouse SQL. The HTTP layer speaks the upstream Prom / Loki / Tempo wire format so Grafana sees cerberus as three drop-in datasources.

## Hard rules (non-negotiable)

- **PR-per-change.** No direct pushes to `main` — branch protection rejects them. Required CI checks: `check` + `lint` + `dashboard` (full-stack smoke: k3d + cerberus + Grafana + Playwright; lives as the `dashboard` job inside `.github/workflows/e2e.yml`). Linear history; force-push and deletion are off. **Never use `gh pr merge --admin`** — every PR must merge cleanly with all required checks green. If a required check is failing, fix the code or fix the workflow; don't bypass. Branch protection has `enforce_admins: true` and the personal token doesn't grant override.
- **Agent-driven work goes through PRs, not Issues.** When *you* (an AI assistant) are doing the work, capture intent in the PR description — don't open an issue to track follow-up. Backlog narratives live in `docs/*.md` files ([`docs/roadmap.md`](docs/roadmap.md), [`docs/optimizer-research.md`](docs/optimizer-research.md)); milestone status lives in the [`Cerberus v1.0.0 Roadmap` GitHub Project](https://github.com/users/tsouza/projects/1) (RC / Workstream / Area / Status fields). Human contributors (or the maintainer) **are welcome to open issues** for bug reports, design discussions, feature proposals — the issues feature is on. The rule is about agent workflow hygiene, not project policy.
- **Conventional Commits**, enforced by `commitlint` (see `.commitlintrc.json`). The `subject-case` rule is relaxed so Dependabot's `Bump X from Y to Z` subjects pass.
- **Justfile is the canonical task runner.** `just` lists every recipe. Don't reach for `go test ./...` directly when `just test` exists — the recipe sets the race flag, the cover profile, and the right toolchain.
- **Compatibility is the source of truth for PromQL — at M6.** Until the M5 → M6 cut, `compatibility.yml` runs only on main pushes + nightly + manual dispatch (not on PRs) and acts as an informational baseline. At M6 we re-enable the `pull_request:` trigger and add `prometheus/compliance` to required checks; an entry in `harness/compatibility/expected-failures.json` then requires a comment explaining the upstream rationale.
- **No raw SQL strings — refactor lands at RC6.** `fmt.Sprintf` (or any string concatenation) used to build a ClickHouse query is forbidden going forward; new emitter code must go through `internal/chsql.Builder` — a custom CH-flavored builder API tailored to chplan IR (see [`docs/sql-builder-evaluation.md`](docs/sql-builder-evaluation.md) for the build-vs-buy decision and [`docs/roadmap.md` § RC6](docs/roadmap.md#rc6--type-safe-sql-via-custom-internalchsqlbuilder) for the milestone sequence). The existing `internal/chsql/emit_*.go` and `internal/api/prom/metadata.go` Sprintf-built SQL is grandfathered until the RC6 port (R6.1–R6.10), but no PR may add a new instance of the pattern. RC6 R6.9 wires a lint rule to enforce automatically.

## Architecture map

```text
internal/
  api/{prom,loki,tempo}/    HTTP handlers per upstream API
  promql/, logql/, traceql/ three heads: parse + lower
  chplan/                    shared plan IR (Scan, Filter, Project, Aggregate, RangeWindow, Limit + Expr tree)
  optimizer/                 rule-based, fixpoint driver. RC1 ships 3 rules (filter-fusion, constant-fold, projection-pushdown); RC3 expands.
  chsql/                     plan → ClickHouse SQL emitter
  chclient/                  CH driver wrapper (clickhouse-go/v2)
  schema/                    OTel-CH default + override config
  config/                    runtime config (env-driven)
cmd/cerberus/                main entrypoint
test/spec/                   TXTAR golden tests (input QL → SQL/plan)
test/e2e/                    k3d cluster + Grafana playwright smoke
harness/compatibility/          prometheus/compliance Docker Compose harness (lands M0.6)
deploy/{k3s,grafana}/        Kubernetes manifests + Grafana Helm values
docs/                        roadmap.md, optimizer-research.md, compatibility.md (M5.4)
```

Top-level reading order for any new contributor (human or agent):

1. `README.md` — what the project is, quick start.
2. `docs/roadmap.md` — RC1 / RC2 / RC3 plan, milestone tables, exit criteria.
3. `docs/optimizer-research.md` — durable optimizer backlog for RC3.
4. `internal/promql/lower.go` — the canonical lowering pattern; mirror it when adding LogQL / TraceQL slices.

## Common workflows

- **Add a TXTAR fixture** — use the `/cerberus:add-fixture` skill (under `.claude/skills/`). It creates `test/spec/<ql>/<name>.txtar` with the right section headers; run `just update-golden` after the implementation lands to fill in expected sections.
- **Add an optimizer rule** — use the `/cerberus:add-optimizer-rule` skill. Scaffolds `internal/optimizer/<name>.go` + test + TXTAR fixtures.
- **Bump parser deps** — use the `/cerberus:bump-parser-deps` skill. Runs `go get -u` on the three upstream parsers, runs `go mod tidy`, captures the diff for the PR description.
- **Run E2E locally** — `just e2e-up && just e2e-seed && just e2e-run && just e2e-down` (lands in M0.1).
- **Run the compatibility suite** — `just compatibility` (lands in M0.6). Diffs cerberus against reference Prometheus on a deterministic OTel fixture.

## Toolchain notes

- **Go version** — `go.mod` may pin a newer Go than what's installed system-wide. `GOTOOLCHAIN=auto` (the default) silently downloads the right version into `~/go/pkg/mod/golang.org/toolchain@...`. The `.envrc` (loaded by `direnv allow`) puts both the system Go and the downloaded toolchains on PATH.
- **CGO** — left at the platform default so `go test -race` works. Goreleaser pins `CGO_ENABLED=0` for release builds independently.
- **`golangci-lint` v2** — the config in `.golangci.yml` uses the v2 schema. `gofumpt` + `goimports` are configured under `formatters`, not `linters`. The v2 install path is `github.com/golangci/golangci-lint/v2/cmd/golangci-lint` (note the `/v2/`).

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
- "Where do I add this feature?" → check `docs/roadmap.md` first; the milestone tells you which package + which fixture dir.
- "Is this RC1 / RC2 / RC3?" → roadmap tables make this explicit. When in doubt, add a comment to the PR asking.
- "Can I update the Project from a PR?" → yes, the repo is linked. Move the matching draft item to `In Progress` when you start, `Done` when the PR merges (or wire a workflow that does it).
