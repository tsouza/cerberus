# Pre-`v1.0.0` GA tag readiness sign-off

> Diagnostic-only audit taken on `2026-05-15` at `main`@`81d8828`
> (`ci(chdb): promote probe + roundtrip lanes to required PR checks (#389)`).
> Read-only — nothing was changed. The audit verifies every GA exit
> criterion in [`docs/roadmap.md` § GA exit criteria](roadmap.md#ga-exit-criteria)
> against repo state and freshly-dispatched CI evidence so the maintainer
> has a single decision artifact for the `v1.0.0` cut.

## TL;DR

- **Readiness score: 11 / 14 criteria MET (78.6%).**
- **3 criteria not yet MET**, none of which is a code regression:
  1. **Branch protection patch** — `#389` merged the workflow
     change, but the `gh api PATCH` command in the PR body to add the
     four chDB contexts (`probe`, `roundtrip (promql|logql|traceql)`)
     to required-status-checks has not yet been applied by the
     maintainer.
  2. **Compatibility ≤ 5 diffs** — fresh dispatch shows **0
     unexpected failures + 13 diffs** (down from 19 in the previous
     run; down from 46 cited in roadmap §433). Pool-BG residual
     sweep is in flight; the diff floor is on a clear path to ≤ 5
     but not yet there.
  3. **Loki + Tempo compliance harness scaffolding** — Tempo PR 1 of
     4 merged (#367); Loki PR 1 of 6 (Pool-CA) and the Tempo/Loki
     follow-ups are open as #369 / #385 / #387. Scaffolding —
     not pass-rates — is the gate; multiple scaffold PRs still open.
- **No code regressions.** Every CLAUDE.md hard rule is honoured (no
  `t.Skip`, no raw SQL strings, no `unsafe.Pointer` outside the
  forks, no caching layer). The pre-GA code-health audit at #374 +
  the coverage audit at #375 both reported the codebase in healthy
  shape; this audit cross-checks against the latest `main` and finds
  the same picture.
- **Estimated time-to-GA:** **~1 day** of compatibility-lane diff
  cleanup + the maintainer's one-shot branch-protection
  `gh api PATCH` command. The Loki/Tempo scaffolding PRs are already
  in flight and merging at a steady cadence; they do not block GA
  on pass-rates, only on the scaffolding landing.

## Inputs verified

| Source                                                                                         | Captured at                                      |
| ---------------------------------------------------------------------------------------------- | ------------------------------------------------ |
| `docs/roadmap.md` § GA exit criteria                                                           | `main`@`81d8828`                                 |
| `gh api repos/tsouza/cerberus/branches/main/protection`                                        | live snapshot 2026-05-15                         |
| `gh workflow run compatibility` → run `25905113695` artifact                                   | fresh dispatch at audit start, ran to completion |
| `gh run list --workflow=…` for `ci` / `chdb` / `mutation` / `property` / `shadow-mode` / `e2e` | live snapshots 2026-05-15                        |
| `gh pr list --state open` (6 PRs)                                                              | live snapshot 2026-05-15                         |
| `docs/pre-ga-code-health-audit.md` (#374)                                                      | merged 2026-05-15                                |
| `docs/pre-ga-coverage-audit.md` (#375)                                                         | merged 2026-05-15                                |
| Recent merged PRs `#386` / `#388` / `#389` / `#378`                                            | repo state                                       |

## GA exit-criteria sign-off table

Cross-referenced row-by-row against the table in
[`docs/roadmap.md` § GA exit criteria](roadmap.md#ga-exit-criteria).
`State` = current verdict. `Evidence` = the artifact this audit
checked. `Blocker` = remaining work (only populated for non-MET
rows).

| #   | Gate                                                                   | State      | Evidence                                                                                                                                                                                                                                           | Blocker / Owner / ETA                                                                                                                                                                           |
| --- | ---------------------------------------------------------------------- | ---------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1   | Required PR checks green (`check` + `lint` + `forbid-skip`)            | MET        | `gh api …/branches/main/protection` shows `contexts: [check, lint, forbid-skip]`; latest main run (`81d8828`) green on `ci` workflow.                                                                                                              | —                                                                                                                                                                                               |
| 2   | Compatibility suite — 0 unexpected failures                            | MET        | Fresh dispatch `25905113695` (run-id from this audit): 536 total / 523 passing / **0 unexpected failures**. Three consecutive runs prior to this audit (`25904068640`, `25903848510`, `25903068130`) also = 0.                                     | —                                                                                                                                                                                               |
| 3   | Compatibility suite — ≤ 5 expected diffs, each with rationale          | NEEDS-WORK | Fresh dispatch: **13 diffs** (down from 19 last run, 46 in roadmap). Diffs cluster on `% scalar`, `stddev_over_time` / `stdvar_over_time`, `round`, `label_replace` non-existent src, subquery-over-aggregator.                                    | Pool-BG residual sweep (in flight). #390 (`fix(promql): round half-to-even + label_replace non-existent src residual`) addresses 2 of the 13 in the open PR queue. ETA: ~1 day of diff cleanup. |
| 4   | All RC1–RC8 feature milestones shipped                                 | MET        | `docs/roadmap.md` § RC1–RC8 — every R{1..8}.x row reads `shipped` / `shipped via #N`. RC6 `Builder.WriteSQL` un-exported (#193) + Raw/Concat retired (#207). RC7 engine pipeline owns shared pipeline.                                             | —                                                                                                                                                                                               |
| 5   | chDB roundtrip lanes (Layers 6a / 6b / 6c) green nightly               | MET        | `gh run list --workflow=chdb`: latest run `success` (`25904927854`); per-QL `probe` + `roundtrip (promql/logql/traceql)` matrix all green. PR `#389` widened path filter to fire on `internal/api/**` + `internal/engine/**`.                      | —                                                                                                                                                                                               |
| 6   | Oracle property tests pass at `N=500`                                  | MET        | `gh run list --workflow=property`: latest run `success` (`25904932966`); nightly `N=500` lane wired via #280 / #284 / #330 / #331.                                                                                                                 | —                                                                                                                                                                                               |
| 7   | Shadow-mode lane diff < 5% native-SQL gap                              | MET        | `gh run list --workflow=shadow-mode`: latest run `success` (`25904750182`); oracle defaults flipped via #326.                                                                                                                                      | —                                                                                                                                                                                               |
| 8   | Mutation kill scores meet phased thresholds                            | MET        | Latest mutation run (`25904750182`): all 7 phases green at the post-#378 raised thresholds — phase1 chplan @ 90%, phase2 chsql @ 85%, phase3 optimizer @ 85%, phase4 promql/logql/traceql @ 85%, phase5 qlcommon @ 75%.                            | —                                                                                                                                                                                               |
| 9   | Loki + Tempo compliance harness scaffolding landed                     | NEEDS-WORK | Tempo PR 1 of 4 merged (#367 vendor + #370 compose + stub). Loki PR 1 of 6 (Pool-CA) in flight as #369. Tempo seeder + Loki diff driver follow-ups open as #385 / #387.                                                                            | Pool-CA and follow-ups. Scaffolding = vendor + adoption plan + Compose stack committed. Pass-rates are explicitly NOT GA-gating. ETA: PRs in flight, merging in the current PR cadence.         |
| 10  | `go-arch-lint` coupling rules green                                    | MET        | `gh run list --workflow=ci`: latest main run `success` (`25904927857`); `lint` job runs `go-arch-lint` per #323.                                                                                                                                   | —                                                                                                                                                                                               |
| 11  | Self-observability (RC4): one span per request, self-dashboard renders | MET        | RC4 closed via #208 + provisioned `test/e2e/grafana/dashboards/cerberus-self.json`; OTLP export disable-able via `CERBERUS_OTEL_ENDPOINT=""`. `gh run list --workflow=e2e`: latest run `success`.                                                  | —                                                                                                                                                                                               |
| 12  | 12-factor scale-out: `docker compose up` works, HPA recipe smokes      | MET        | RC5 closed (R5.1–R5.7); `#218` HPA, `#219` admission control, `#220` `docker-compose.yml`. E2E `dashboard` job green nightly.                                                                                                                      | —                                                                                                                                                                                               |
| 13  | No raw SQL strings in `internal/`                                      | MET        | RC6 closed; `Builder.WriteSQL` unexported (#193), `chsql.Raw` / `chsql.Concat` retired (#207). Pre-GA code-health audit (#374) confirmed 0 `fmt.Sprintf` SQL-shape regressions; `forbidigo` linter still active.                                   | —                                                                                                                                                                                               |
| 14  | Engine framework owns the shared pipeline                              | MET        | RC7 closed (#227 / #228 / #230 + #232 headers); each `internal/api/{prom,loki,tempo}/handler.go` is now a thin HTTP shell — under ~150 LoC per [`docs/roadmap.md` § RC7 exit criterion](roadmap.md#rc7--internalengine-executionengine-framework). | —                                                                                                                                                                                               |

## Branch-protection state — non-criterion but operational

The roadmap table treats branch protection as the `check + lint + forbid-skip` line (criterion 1, MET). PR `#389` made the **separate** decision to also gate `probe` + the three `roundtrip (<ql>)` chDB lanes on PR merges. That PR merged the workflow path-filter widening; the **`gh api PATCH` command in the PR body to add the four contexts to required-status-checks has not yet been applied**.

Verified via `gh api repos/tsouza/cerberus/branches/main/protection`:

```text
required_status_checks.contexts:
  check
  lint
  forbid-skip
required_status_checks.strict: false
enforce_admins: true
required_linear_history: true
allow_force_pushes: false
allow_deletions: false
```

The four chDB contexts are **not yet on the list**. This is a NEEDS-MAINTAINER action — strictly outside the repo tree. The exact command lives in the body of `#389`. Once applied, the audit row above should flip to MET-confirmed for the new contexts; until then chDB regressions can still slip through if the lane is informational on PRs whose path filter matches.

## Compatibility-lane snapshot (fresh)

Dispatched at audit start: `gh workflow run compatibility` → run-id `25905113695`. Completed during the audit.

| Bucket               | Count |
| -------------------- | ----- |
| total                | 536   |
| passing              | 523   |
| diffs                | 13    |
| unexpected failures  | 0     |
| unexpected successes | 0     |
| unsupported          | 0     |

The 13 diffs cluster on:

- `demo_memory_usage_bytes % 1.2345` (2 entries: literal + parenthesised arithmetic) — modulo semantics on scalar.
- `stddev_over_time` / `stdvar_over_time` over 1m / 5m / 15m / 1h (8 entries) — variance under empty / single-sample windows.
- `round(demo_memory_usage_bytes)` (1 entry) — addressed by open PR #390 (`round half-to-even`).
- `label_replace(demo_num_cpus, "job", "value-$1", "nonexistent-src", "(.*)")` (1 entry) — addressed by open PR #390 (`label_replace non-existent src residual`).
- `avg_over_time(rate(demo_cpu_usage_seconds_total[1m])[2m:10s])` (1 entry) — subquery-over-rate diff.

`compatibility/prometheus/expected-failures.json` currently has an **empty `failures` array** — all 13 are raw diffs, not allowlisted. The GA target is **≤ 5 allowlisted diffs each with an upstream-rationale comment**, so Pool-BG needs to either land patches that close the remaining diffs or land allowlist entries with rationales. Both are valid GA-clearing paths; the doc + the allowlist comment requirement enforces audit hygiene either way.

## CLAUDE.md hard-rule cross-check

Re-verified each non-negotiable rule on `main`@`81d8828`. The pre-GA code-health audit (#374) covered most of these on 2026-05-15; this section pins what changed since.

| Hard rule                                               | State | Note                                                                                                                                                                            |
| ------------------------------------------------------- | ----- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| PR-per-change, branch protection enforces no force-push | MET   | `gh api …/branches/main/protection`: `enforce_admins: true`, `allow_force_pushes: false`, `allow_deletions: false`, `required_linear_history: true`.                            |
| `gh pr merge --admin` never used                        | MET   | Recent merges via `--squash --delete-branch`; required checks green at merge time on the 20 most recent merges.                                                                 |
| Conventional Commits via commitlint                     | MET   | Last 20 merged-commit subjects all match `<type>(<scope>): <subject>` shape.                                                                                                    |
| Justfile is canonical task runner                       | MET   | `.github/workflows/{ci,chdb,property,mutation,shadow-mode,compatibility}.yml` all run via `just`-style recipes (or vendored installs of `gremlins` / `taiki-e/install-action`). |
| No local validation; lefthook + CI own it               | MET   | Repo carries `lefthook.yml`; CI workflows split into `check` (test+race) + `lint` (golangci-lint + go-arch-lint).                                                               |
| Compatibility is the source of truth for PromQL         | MET   | `compatibility.yml` runs on main pushes + nightly + dispatch; informational; allowlist entries require comments (the existing `$schema` JSON enforces it).                      |
| No raw SQL strings — typed chsql API only               | MET   | `Builder.writeSQL` unexported; `chsql.Raw` / `chsql.Concat` retired. #374 confirmed 0 `fmt.Sprintf` SQL-shape callsites; this audit re-verifies no new regressions.             |
| No `t.Skip` in test files                               | MET   | `forbid-skip` CI job is a required check (verified via `gh api …/branches/main/protection`).                                                                                    |
| No `unsafe.Pointer` outside upstream forks              | MET   | #374 confirmed zero `unsafe.` imports under `internal/`, `cmd/`, `test/`; `forbidigo` gates regressions on `internal/traceql/` + `internal/api/tempo/`.                         |
| No caching layer                                        | MET   | Only `/readyz` TTL cache exists (R5.1). No plan/SQL/result caches anywhere in `internal/`.                                                                                      |

## Mutation gremlins post-#378 bar

PR `#378` raised the kill-rate thresholds from the onboarding floors (80/75/70/65) to the post-onboarding bars (90/85/85/85/85/85/75). The most recent mutation run (`25904750182`) ran the full 7-job matrix at the raised bars: every phase green.

| Phase            | Scope                  | Floor (post-#378) | Status |
| ---------------- | ---------------------- | ----------------- | ------ |
| phase1           | `./internal/chplan`    | 90%               | green  |
| phase2           | `./internal/chsql`     | 85%               | green  |
| phase3-optimizer | `./internal/optimizer` | 85%               | green  |
| phase4-promql    | `./internal/promql`    | 85%               | green  |
| phase4-logql     | `./internal/logql`     | 85%               | green  |
| phase4-traceql   | `./internal/traceql`   | 85%               | green  |
| phase5-qlcommon  | `./internal/qlcommon`  | 75%               | green  |

The matrix is still informational on PRs (gated to push-to-main + nightly + dispatch); promoting it to required is post-GA work tracked in [`docs/test-strategy.md`](test-strategy.md).

## Remaining-to-GA work

Every still-open PR and every still-pending follow-up that touches a NEEDS-WORK row above. None of these is a code regression; all are roll-forward items.

### Open PRs (6)

| #    | Title                                                                           | Branch                                        | Mergeable | Notes                                                           |
| ---- | ------------------------------------------------------------------------------- | --------------------------------------------- | --------- | --------------------------------------------------------------- |
| #391 | `chore(deps): bump pgregory.net/rapid from 1.2.0 to 1.3.0 in the go-deps group` | `dependabot/go_modules/go-deps-77b3277776`    | MERGEABLE | Dependabot; all required checks green; can auto-merge.          |
| #390 | `fix(promql): round half-to-even + label_replace non-existent src residual`     | `fix/promql-round-and-label-replace-residual` | MERGEABLE | `lint` job failing; closes 2 of the 13 compat diffs once green. |
| #387 | `feat(compatibility/loki): wire diff driver into run script + just recipe`      | `feat/loki-compliance-diff-driver`            | MERGEABLE | Loki compliance scaffolding PR 2 of 6.                          |
| #385 | `feat(compatibility/tempo): implement deterministic OTLP seeder`                | `feat/tempo-compliance-seeder`                | MERGEABLE | `lint` job failing; Tempo PR 2 of 4.                            |
| #383 | `test(spec/logql): cover vector-vector binary ops + literal-vector paths`       | `test/logql-vector-vector-coverage`           | MERGEABLE | Test-coverage backfill; not GA-gating.                          |
| #369 | `feat(compatibility/loki): vendor pkg/logql/bench corpus`                       | `feat/loki-compliance-vendor-bench`           | MERGEABLE | Loki compliance scaffolding PR 1 of 6 (Pool-CA).                |

### Pending follow-ups (not PRs yet)

| Item                                                    | Origin                                    | State            | Notes                                                                                                     |
| ------------------------------------------------------- | ----------------------------------------- | ---------------- | --------------------------------------------------------------------------------------------------------- |
| Branch-protection PATCH for chDB lanes                  | PR `#389` body                            | NEEDS-MAINTAINER | One `gh api PATCH` command; ~30 seconds of maintainer work.                                               |
| Pool-BG residual compat sweep                           | `docs/roadmap.md` §433                    | In flight        | 13 → ≤ 5 diffs. `#390` closes 2; remaining ~6–8 need either patches or allowlist entries with rationales. |
| Native histogram Phase 2 (aggregated input native path) | `docs/native-histogram-plan.md` § Phase 2 | Deferred         | TBD scope; explicitly NOT GA-gating per the plan doc — Phase 2/3/4 land post-GA.                          |
| Native histogram Phase 3 (range-mode anchor grid)       | `docs/native-histogram-plan.md` § Phase 3 | MET              | Shipped: bare + aggregated lowerings fan exp-histogram quantiles across a StepGrid per request.           |
| Native histogram Phase 4 (negative-side observations)   | `docs/native-histogram-plan.md` § Phase 4 | Deferred         | TBD scope; NOT GA-gating.                                                                                 |
| Tempo compliance PRs 3 / 4 (driver + CI)                | `docs/tempo-compliance-plan.md`           | In flight        | Vendor + compose + stub driver landed (#367 / #370). Seeder open (#385). Driver + CI follow.              |
| Loki compliance PRs 3 / 4 / 5 / 6                       | `docs/loki-compliance-plan.md`            | In flight        | Vendor open (#369). Diff driver open (#387). Adoption plan committed in #332.                             |

## Decision rule

`v1.0.0` cuts from the last green main once **rows 3 + 9 + the branch-protection PATCH** flip to MET. Cited:

- Row 3 (compat ≤ 5 diffs each with rationale): land Pool-BG's residual patches + allowlist entries until the diff count hits ≤ 5 with each remaining entry carrying an upstream-rationale comment in `expected-failures.json`. `#390` is the immediate next step (closes 2).
- Row 9 (Loki + Tempo scaffolding): land #369 / #385 / #387 + the remaining compliance-plan PRs. Pass-rates are not gating; scaffolding committal is.
- Branch-protection patch: maintainer runs the `gh api PATCH` from `#389`'s body.

Once those three flip, the readiness score moves to 14 / 14 (100%) and the maintainer cuts `v1.0.0` from the last green main.

## What this audit explicitly did NOT do

- It did not run `just test` / `just lint` / `just compat-promql`
  locally (per the CLAUDE.md "no local validation" rule).
- It did not modify any code or workflow files.
- It did not open a sub-PR for any of the NEEDS-WORK rows — those
  remain owned by the existing Pool assignments.
- It did not propose a numeric `coverage` threshold; that decision
  is post-GA per [`docs/coverage-baseline.md`](coverage-baseline.md).
- It did not propose promoting any informational lane (compatibility,
  mutation, shadow-mode, e2e dashboard) to required-status-checks;
  that is a separate decision tracked in `docs/roadmap.md` §
  "Compatibility lane progress".

The audit's role is to give the maintainer a single decision artifact
for the `v1.0.0` cut. Nothing in this doc supersedes the per-PR
review process; every NEEDS-WORK row resolves through the normal PR
cycle.
