# `.github/scripts/` — reusable CI step logic (Node ESM)

Non-trivial CI step logic (multi-line `bash` / `jq` / `awk` / `perl`
embedded in workflow `run:` blocks) lives here as dependency-light Node
ESM (`.mjs`) modules, not inline YAML. See the **CI / workflow scripts**
rule in the repo `CLAUDE.md` for the why.

Each module:

- imports only `node:` builtins (no npm deps, no `@actions/*`), so a bare
  `run: node .github/scripts/<name>.mjs` works on `ubuntu-latest` with no
  `setup-node` step;
- reads its inputs from env vars (documented at the top of the file and in
  the table below);
- prints `::error::` / `::notice::` GitHub workflow commands on the
  relevant outcome;
- `process.exit(1)` on failure, `0` on success — preserving the exact
  exit semantics of the bash it replaced.

`lib/gh.mjs` is the shared helper library: workflow-command emitters
(`error` / `notice` / `warning` / `group`), `capture` / `exec` / `git`
wrappers around `node:child_process`, a `lsFiles` `git ls-files -z`
wrapper, plus `appendStepSummary` / `setOutput` for the runner files.

`lib/shard-coverage.mjs` holds the Playwright spec-partition rules the two
e2e shard-matrix modules share — `discoverSpecs()` (the tracked spec
universe, from `PLAYWRIGHT_DIR`) and `collectShardCoverageViolations()`
(unassigned / double-assigned / phantom / stale-exclude / bad-shard-name).
One implementation means a new rule guards BOTH lanes at once.

`lib/registry.mjs` owns the one retry policy every container-registry fetch
follows — the attempt budget (`registryAttempts`), the linear backoff
(`readBackoffStepSeconds` / `sleepSeconds`), and `isTransientRegistryFailure()`,
which decides whether command output names a registry / network fault (Docker
Hub 429, manifest HEAD failure, TLS / reset / DNS / module-proxy transport) or a
genuine build error. `pull-buildkit-image.mjs` (host-side bootstrap pull) and
`build-with-registry-retry.mjs` (the build's own `FROM` resolution) hit the same
rate-limit bucket and fail the same way, so they share the policy instead of
re-deriving it.

`lib/scope-gate.mjs` answers "does THIS change touch the scope a heavy lane
guards?" — `runsFullLane()` (which events must never take a scoped subset),
`changedPaths()` (the change's own diff — a pull request against its merge base,
a merge-queue entry across `base_sha..head_sha` — or `null` when it cannot be
computed, which callers must read as "run everything" rather than as "nothing
changed"), and segment-wise `underPrefix` / `matchesAny`.
It exists so the two dangerous parts — the always-full event set and the
uncomputable-diff fallback — cannot drift between the lanes that use it.

## Modules

- **`agpl-clean.mjs`** — `ci.yml`, the `agpl-clean` job. The provably-clean-build
  licence gate: runs `go list -deps ./cmd/cerberus` and reports any AGPLv3
  package the Apache-2.0 binary links (`github.com/grafana/loki/*`, or
  `github.com/grafana/tempo/*` except the Apache-licensed `pkg/tempopb`). The
  test-only AGPL importers are quarantined into the `test/oracle` nested module
  (a hard module boundary `go list -deps` does not cross), so only production
  imports can surface here. The binary is licence-clean: LogQL / TraceQL parse
  with in-house Apache reimplementations and PromQL with the upstream Apache
  prometheus parser.
  - Env: `AGPL_CLEAN_PACKAGE` (optional; default `./cmd/cerberus`).
  - Exit: `0` clean; `1` on a violation. ENFORCING (a violation fails CI) and a
    required status check on `main`.
- **`forbid-skip.mjs`** — `ci.yml`, the `forbid-skip` discipline scans.
  - Env: `CHECK` is one of `t-skip`, `not-implemented`,
    `soft-assert`, `should-skip`, `escape-hatch`, `feature-discipline`.
  - Exit: `0` clean, `1` on any banned pattern or bad `CHECK`.
- **`forbid-deferral.mjs`** — `ci.yml`, the `forbid-deferral` job. The prose
  sibling of `forbid-skip`: where that one rejects a test that declines to
  assert, this rejects a change that names work it is not doing and walks away.
  Scans exactly three surfaces, all of them the change's OWN additions — the PR
  description, the commit messages in `BASE_SHA...HEAD_SHA`, and the `+` lines
  of that diff — against the exported `DEFERRAL_MARKERS` table, and requires
  every hit to cite an issue in this repository that is **open** and is an issue
  rather than a pull request (GitHub's issues endpoint returns both; the
  `pull_request` key discriminates). Citation scope is the paragraph for prose
  and `CITATION_WINDOW_LINES` either side for a diff hunk, so a comment block
  that already names its issue satisfies the gate when it grows.
  The commit-message surface is measured, not assumed: of 217 commits on `main`
  carrying deferral text, 178 carry it ONLY in intra-branch commit messages, so
  a description-only gate would miss ~82% of them.
  The tree at large is deliberately NOT scanned — the same phrases are ordinary
  architecture prose in `internal/**`, and a gate that fired on those would be
  routed around. That is scoping, not an allow-list: there is no tolerance file
  and no way to park a violation. Anti-vacuity is explicit — a missing
  description surface, an unresolvable commit range, an empty commit list, an
  empty file set or an empty marker table each fail LOUDLY rather than passing
  green. `forbid-deferral.test.mjs` is the `node --test` guard (run as the step
  BEFORE the gate): it proves every table row fires on a real example and that
  the three measured false-positive shapes — Go's `defer` statement, the phrase
  that records COMPLETED work, and a change that only DELETES a marker line —
  stay clean.
  - Env: `GITHUB_REPOSITORY`, `GITHUB_TOKEN` (needs `issues: read` and
    `pull-requests: read`), `GITHUB_EVENT_NAME`, `PR_BODY` (required on a
    `pull_request` run; may be empty, may not be unset), `BASE_SHA`, `HEAD_SHA`,
    `GITHUB_API_URL` (optional; runner-provided).
  - Exit: `0` when every marker is tracked by an open issue (or none were
    found); `1` on an untracked deferral or a malformed input. ENFORCING and a
    required status check on `main`.
- **`repo-hygiene.mjs`** — `ci.yml`, the `forbid-skip` job's committed-artefact
  gate. Every other gate asks whether the tree COMPILES and PASSES; none asks
  what it CONTAINS, so a build artefact that is `git add`-ed by accident
  survives indefinitely. Two scans close that class. `binary` rejects any
  tracked blob that is compiled output, detected by CONTENT — an executable
  magic (ELF / Mach-O / PE / WebAssembly / ar archive) at offset 0, or a NUL
  byte inside the same leading window git itself sniffs when it classifies a
  blob as binary — and never by extension, since a Go binary built from
  `./cmd/<name>` carries none. Submodule gitlinks and symlinks are skipped
  (neither stores content in this repository). `root-allowlist` holds the
  repository root to `ROOT_ALLOWLIST`, an exhaustive list rather than a
  pattern: dotfiles are enumerated too, so a stray `.perf-profile` is caught
  exactly like a stray `perf-profile`. The comparison runs in BOTH directions
  — an allow-list entry that is no longer tracked is also an error, so the
  list cannot rot into a pre-approval for a future file of the same name. The
  gate fails CLOSED: a tracked blob that cannot be read from the working tree
  is retried out of the object store and, failing that, exits non-zero rather
  than being assumed to be text. Since `git ls-files` is the input, the
  companion fix for anything this catches is a `git rm` PLUS a `.gitignore`
  rule — the ignore rule is what stops it coming back.
  `repo-hygiene.test.mjs` is the `node --test` guard (cheap discipline lane,
  run as the step BEFORE the gate): it builds a throwaway git repo, plants a
  synthetic ELF blob and a stray root file, and asserts a non-zero exit
  naming each, plus a clean exit on a conforming fixture — a gate never shown
  to fail is indistinguishable from one that does nothing.
  - Env: `CHECK` is one of `binary`, `root-allowlist`; `REPO_ROOT` (optional)
    points the scan at another checkout (the self-test's fixture repo).
  - Exit: `0` clean, `1` on any tracked binary / unsanctioned or rotted root
    entry / unreadable blob / bad `CHECK`.
- **`clickhouse-version-sync.mjs`** — `ci.yml`, the `forbid-skip` job's
  ClickHouse version-consistency gate. Reads `versions.yaml` (the single
  source of truth) and asserts the docker-compose quickstart + compatibility
  image tags, the preflight floor, and the chDB substrate all match it, that
  the migration lane's Tier-1 stack tracks the quickstart (deployment-surface)
  tag rather than the chDB substrate, and that the quickstart is new enough for
  every optimization it enables (floors derived from
  `internal/chopt/registry.go`, not duplicated). See
  `docs/optimization-rules.md` (Rule 1, step 4).
  - Args: `--self-test` pins the parse / compare / drift-detection logic
    (run as a CI step before the gate); no args runs the gate over the tree.
  - Exit: `0` consistent (or self-test green), `1` on any drift.
  - The CHECK-arm count here is the source of truth for the "N checks"
    claim in `docs/forbid-skip.md`, asserted live by `doc-counts.mjs`.
- **`doc-refs.mjs`** — `ci.yml`, the `doc-to-code reference check` step in
  the `lint` job. The GATE that keeps prose docs honest about the code they
  cite: greps `docs/**/*.md` for inline `(internal|cmd|test|deploy)/<path>.go`
  references (with an optional leading module prefix, so
  `compatibility/prometheus/cmd/seed/prom_remote.go` is captured WHOLE) and
  HARD-FAILS when the path no longer exists (`git ls-files`). A `:line` /
  `:start-end` pin is BOUNDS-checked only — fail iff the (high) line exceeds
  the file's length; docs pin approximate / tilde line numbers that drift by
  a line as code moves, so the cited line is NOT required to contain anything
  specific, only to be in range. A trailing-slash / no-`.go` token is a
  directory-existence check. `./`/`../`-prefixed tokens are accepted under
  EITHER the repo-root or doc-relative interpretation (a `go test ./test/...`
  snippet vs a `[..](../test/..)` markdown link), so only a path dead under
  every interpretation is a violation. Vendored snapshots
  (`compatibility/*/upstream/**`) are excluded, mirroring the markdownlint /
  forbid-skip exclude set. Structure mirrors `forbid-skip.mjs`: pure exported
  helpers + a `--self-test` flag; `doc-refs.test.mjs` is the `node --test`
  guard (cheap lint lane) that pins the extraction regex + verdict logic and
  proves each detector fires. The companion lychee gates are the OFFLINE
  internal `link-check` job (ci.yml) and the schedule-only
  `link-check-external.yml` — link existence/anchors vs doc-to-code path
  existence are complementary, non-overlapping concerns.
  - Env: `DOCS_GLOBS` (optional; default `:(glob)docs/**/*.md`); argv
    `--self-test` runs the in-process assertion suite.
  - Exit: `0` when every cited path exists + pins are in range (or self-test
    passes), `1` on any dead reference / out-of-range pin (or a failed
    self-test).
- **`doc-counts.mjs`** — `ci.yml`, the `forbid-skip` job step "Assert
  doc-stated counts match source". The assert-from-source gate that stops
  doc-stated integer counts from drifting away from the source structures
  they describe. It derives each count LIVE — NOT from a hardcoded literal
  (which would just relocate the staleness) — and asserts every matching
  prose claim equals it:
  - **forbid-skip CHECK count** — parses the `case '<name>':` arms of the
    `CHECK` switch in `forbid-skip.mjs` (today: 5 — `t-skip`,
    `not-implemented`, `soft-assert`, `should-skip`, `escape-hatch`) and
    asserts the "N checks / scans / CHECK categories" claims in
    `docs/forbid-skip.md` match. The doc distinguishes the 7 regex pattern
    ROWS from the 5 dispatched scans; the gate keys on the scan/check
    vocabulary, never the ambiguous bare "patterns".
  - **test-layer count** — counts the DISTINCT integer layer numbers across
    the `### Layer N[sub]` headings in `docs/test-strategy.md` (1..13,
    collapsing 2a/2b/6d/7b to their integer = 13) and asserts the
    "N-layer test map" / "tested in N layers" claims in `CLAUDE.md`,
    `docs/test-strategy.md`, and `README.md` match.
  - Counts are parsed from the actual structures (switch arms / markdown
    headings), never from a string match on the prose they validate, so a
    doc can only go green by matching reality.
  - **`--self-test`** is a meta-test that feeds the derivers / extractors
    deliberately-drifted inputs and proves each assertion FAILS on a
    mismatch (and ACCEPTS the corrected wording). The CI step runs
    `--self-test` first, then the real assertion.
  - Env: none (paths are repo-relative to the script).
  - Exit: `0` when every doc count matches source (or every self-test
    meta-assertion passes), `1` on any drift / undetected mutation.
- **`pr-body-check.mjs`** — `pr-hygiene.yml`, the `pr-body` job. Rejects a
  pull request with an empty or stub description (the `gh pr create … --body
  'cat'` incident, where a malformed heredoc shipped a PR whose entire body was
  the word `cat`). Strips boilerplate that carries no description (the AI footer,
  `Co-authored-by:` trailers, HTML/template comments, image-only lines), then
  requires the remainder to be substantive: at least `MIN_CHARS` (20) of
  meaningful text and not a lone placeholder token (`cat`/`wip`/`todo`/…). Runs
  on `pull_request` (incl. `edited`, so fixing the body re-runs the gate). The
  body is read from the `PR_BODY` env var — never interpolated into a shell line.
  - Env: `PR_BODY` (the pull request body); argv `--self-test`.
  - Exit: `0` when the description is substantive (or self-test passes), `1` on
    an empty / stub body (with one `::error::` explaining what to write).
- **`pr-type-label.mjs`** — `pr-label.yml`, BOTH jobs (`label` + `backfill`).
  The single source of truth for the PR-title -> Conventional-Commit type-label
  mapping. Pure exported `labelsForTitle(title)` returns the label array a PR
  with that title should carry (`feat`->enhancement, `fix`->bug, `docs`->
  documentation, `ci`/`test`/`refactor`/`chore`/`build`/`revert`-> same name,
  `perf`->performance, `style`->none); scope overrides `*(deps)`->dependencies
  and `chore(release)`->release+chore take precedence over the bare type. The
  two workflow jobs `require()` this module so the event-driven and the
  self-healing backfill paths can't drift. Unlike the other scripts this one is
  imported into a `github-script` step (node24 `require(ESM)`), not run as a
  standalone `node ...` process, so it has no env contract — the title comes
  from the caller. The `backfill` job walks every OPEN PR and applies any
  MISSING expected label (idempotent; skips already-correct PRs), self-healing
  the case where the event-driven run was queued / failed / never fired (the
  #1049 / #1050 incident). Bot-authored PRs (`login` ending `[bot]`) are skipped
  by both paths — Dependabot self-labels via `.github/dependabot.yml`, and a
  label edit from here would block its auto-rebase.
  - Env: none (the title is passed by the caller); argv `--self-test` pins the
    full mapping incl. the deps/release scope overrides and the no-match cases.
  - Exit: `0` on a green self-test, `1` on any failed assertion.
- **`gremlins-threshold.mjs`** — `mutation.yml`, the
  `enforce efficacy threshold` step.
  - Env: `REPORT` (default `gremlins.json`), `THRESHOLD` (a number).
  - Exit: `0` when efficacy is `>=` threshold, `1` when below.
- **`mutation-phases.mjs`** — data, imported by `mutation-matrix.mjs`. The single
  source of truth for the `mutation` lane's phase partition: each entry is one
  gremlins run (`scope` package, optional scope-relative `exclude_files` RE2
  alternation, `workers` cap, `--threshold-efficacy` bar), rendered straight into
  `mutation.yml`'s `strategy.matrix`. It carries the per-leg rationale — the
  logql/traceql OOM splits, the equivalent-mutant analysis behind each bar — that
  used to live in the workflow. Also exports `HARNESS_PATHS`: the paths that
  change the lane itself and therefore force the FULL matrix.
  - Env: none (pure data module).
- **`mutation-matrix.mjs`** — `mutation.yml`, the `select` job. Decides WHICH
  phases run and emits them as the `mutate` `strategy.matrix`. On push /
  schedule / dispatch and on a `release/*` PR it selects every phase; on an
  ordinary PR — and on a merge-queue entry, off its own `base_sha..head_sha` —
  it selects only the legs whose scope the diff touches, applying each leg's
  `exclude_files` to the SCOPE-RELATIVE path exactly as gremlins does.
  A changed path that lies inside a phase scope while claiming no leg is a
  coverage gap and fails the job rather than being dropped. `verify` mode asserts
  the table alone (every scope an existing directory, every pattern legal under
  Go's RE2 — no lookaround, no backreferences); `emit` re-runs verify first, so
  it cannot ship a matrix built from a table that would have failed. `dump`
  validates and then writes the table to stdout as JSON and nothing else —
  that is the stream `test/regression/mutation_leg_partition_test.go` parses to
  assert every mutable file under a mutated package is claimed by exactly one
  leg, an invariant no single leg's regex can express.
  - Env: `MODE` (`emit` | `verify` | `dump`, also argv\[2]; default `verify`),
    `EVENT_NAME`, `HEAD_REF`, `BASE_SHA`, `HEAD_SHA`, `GITHUB_OUTPUT`.
  - Outputs: `matrix` (`{include:[…]}`), `has_phases` (`true` | `false` — the
    aggregator reads it to tell "nothing in scope changed" apart from "the matrix
    did not run").
  - Exit: `0` clean / matrix emitted; `1` on a table violation, a coverage gap,
    or a bad `MODE`.
  - Tests: `node --test .github/scripts/mutation-matrix.test.mjs` (run by the
    `forbid-skip` job).
- **`release-version-gate.mjs`** — `release.yml`, the `gate` job (app side).
  The publish-on-merge pipeline ships when a validated `release/*` PR is MERGED
  to main (not on a raw pushed tag — that trigger is retired). On the resulting
  push to main this gate decides whether the APP needs a release: it parses
  Chart.yaml's `appVersion:` and sets `publish=true` ONLY when no `v<appVersion>`
  git tag exists yet (a genuinely new app version), else `publish=false` (the
  no-op case an ordinary code/docs merge hits). The app's release identity IS
  the `vX.Y.Z` git tag — goreleaser derives `{{ .Version }}` from it and the
  immutable GitHub release is keyed on it — so "already released" == "the tag
  exists" (`git tag -l v*`). This is the app-side twin of `chart-publish.mjs`'s
  `version-gate` (which gates the chart on its own `version:` line vs the OCI
  registry); together they make a merge that bumped neither line a complete
  no-op. Pure exported `parseAppVersion(chartYaml)` + `decide(appVersion,
  existingTags)` (no I/O, no `process.exit`) + a `--self-test` the workflow runs
  before the gate. A prerelease appVersion (e.g. `1.5.0-rc.1`) is gated on the
  exact `v1.5.0-rc.1` tag, never masked by a stable `v1.5.0`. Because the gate is
  tag-ABSENT and not newest-wins, a MAINTENANCE-line hotfix (`v1.4.1` cut off
  `release/1.4.x` while `v1.5.0` is already the latest tag) still publishes — its
  `v1.4.1` tag simply doesn't exist yet.
  - Env: `CHART_DIR` (default `deploy/helm/cerberus`), `GITHUB_OUTPUT`
    (runner-provided; sets `publish` / `version` / `tag`); argv `app-version-gate`
    (or no argument) runs the gate, `--self-test` runs the assertion suite.
  - Exit: `0` after writing the outputs (or a green self-test), `1` on a
    missing/malformed `appVersion`, a `git tag` failure, or a bad subcommand.
- **`resolve-release-trigger.mjs`** — `prepare-release.yml`, the `resolve
    trigger` step. Normalises the workflow's two entrypoints — a manual
    `workflow_dispatch` and an `issues: labeled` event — into the
    `version`/`bump`/`chart_bump` outputs the `stage release files` step
    (`prepare-release.mjs`) consumes. On dispatch it passes the three inputs
    through verbatim; on a label it parses `github.event.label.name`:
    `release:patch|minor|major` -> `bump=<that>`, `chart_bump=patch`;
    `release:<semver>` (e.g. `release:1.4.2`) -> `version=<semver>` (v-prefix
    stripped), `chart_bump=patch`; anything else under `release:` is an error.
    `prepare-release.mjs` still owns the value semantics (explicit VERSION
    overrides BUMP, BUMP=none is its no-op placeholder); this resolver only
    decides which event shape supplied them. Pure exported `resolve(env)` + a
    `--self-test` the workflow runs before it stages anything.
  - Env: `EVENT_NAME` (`workflow_dispatch` | `issues`); dispatch path reads
    `VERSION` / `BUMP` / `CHART_BUMP`; issues path reads `LABEL_NAME`;
    `GITHUB_OUTPUT` (runner-provided; sets `version` / `bump` / `chart_bump`).
  - Exit: `0` after writing the outputs (or a green self-test), `1` on an
    unrecognised `release:` label or an unsupported event.
- **`prepare-release.mjs`** — `prepare-release.yml`, the release-staging
    workflow (`workflow_dispatch` or an `issues: labeled` `release:*` trigger;
    see `resolve-release-trigger.mjs`). Bumps the chart `version:` + `appVersion:`, the image tag, and the
    Artifact Hub `changes` annotation, and rewrites the CHANGELOG `[Unreleased]`
    section into a dated `## [vX.Y.Z]` one — deriving the change summary and the
    PR body from the conventional commits since the last `v*` tag. The commit
    history is the single source of truth: the generated section is always what
    lands and any stale `[Unreleased]` content is discarded (maintainers enrich
    the prose by editing the opened PR, not by hand-staging `[Unreleased]`). Pure
    exported helpers (`bumpSemver`, `parseCommits`, `renderChangelogSection`,
    `renderAhChanges`, `editChart`, `editChangelog`) + a `--self-test` flag the
    workflow runs before it edits anything.
  - Env: `VERSION` (explicit target appVersion; overrides `BUMP`), `BUMP`
    (`patch`|`minor`|`major`, or the workflow's `none` placeholder),
    `CHART_BUMP` (default `patch`), `PR_BODY_FILE` (default `release-pr-body.md`),
    `GITHUB_OUTPUT` (runner-provided; sets `new_version` / `chart_version`).
  - Exit: `0` after staging the files (or a green self-test), `1` on a bad /
    missing version input or a malformed Chart.yaml / CHANGELOG.
- **`chart-publish.mjs`** — `release.yml`, the `chart-release` job. Three
  subcommands (argv[2]): `version-gate` compares the local Chart.yaml
  `version:` against the latest chart tag in the OCI registry and sets the
  `publish=true|false` + `version` step outputs (an app-only `v*` tag must NOT
  republish an unchanged chart); `push` runs `helm push` and parses the pushed
  `sha256:` digest out of helm's output into the `digest` + `ref` step outputs
  for the downstream cosign-sign / attest steps; `ah-metadata` idempotently
  pushes `artifacthub-repo.yml` as the special Artifact Hub OCI artifact via
  `oras`. The `version-gate` is ABSENCE-keyed, not newest-wins: it publishes iff
  this exact chart version is not already in the registry, so a MAINTENANCE-line
  chart hotfix whose version is OLDER than the latest published chart still
  publishes. Pure exported `notFoundError(stderr)` + `decideFromProbe({status,
  stderr})` (classify probe → `{publish}` | `{error}`, no I/O) + a `--self-test`
  the `gate` job runs before deciding.
  - Env: `CHART_DIR` (default `deploy/helm/cerberus`), `OCI_REPO` (default
    `oci://ghcr.io/tsouza/cerberus/charts`), `CHART_NAME` (default `cerberus`),
    `CHART_TGZ` (push only), `GITHUB_OUTPUT` (runner-provided); argv `--self-test`
    runs the assertion suite.
  - Exit: `0` on success (gate sets `publish` either way, or a green self-test);
    `1` on a parse failure / `helm push` / `oras push` error, or when the
    version-gate cannot definitively determine existence (fails closed, with one
    `::error::`).
- **`release-gate-drift.mjs`** — `release-gate-drift.yml`, the weekly `drift`
  job. Rot detector for the EXPECTED set `release-preflight.mjs` gates on. Reads
  `RELEASE_REQUIRED_CHECKS` + `RELEASE_INFORMATIONAL_CHECKS` out of release.yml
  itself (one copy of the data, one parser; an empty parse throws rather than
  comparing against nothing) and checks two directions the preflight structurally
  cannot see from the inside. PROTECTION DRIFT: a live branch-protection required
  context in neither list is a lane every PR must pass and the release does not
  wait for — the dangerous direction, and invisible to an allow-list of names to
  wait for. LANE DRIFT: a required name that posted no check-run anywhere in the
  scanned commit window no longer matches a lane, so the next release waits out
  its full window and aborts mid-publish. The window spans many commits because a
  single commit's check-runs are a subset of the lane inventory (a docs-only push
  skips `check`); absent from every commit means dead, not skipped. Pure exported
  `parseBlockScalar` / `parseCheckLists` / `protectionDrift` / `laneDrift` plus a
  `--self-test` the job runs first.
  - Env: `GITHUB_TOKEN` (must be repo-ADMIN — branch protection is unreadable
    with the default workflow token at any `permissions:` level, so the workflow
    passes `RELEASE_PAT`), `GITHUB_REPOSITORY`, `GITHUB_API_URL` (default
    `https://api.github.com`), `DRIFT_BRANCH` (default `main`), `DRIFT_HISTORY`
    (default 20 commits), `RELEASE_WORKFLOW` (default
    `.github/workflows/release.yml`).
  - Exit: `0` when both directions are clean; `1` on any drift, an API read
    failure, an unparseable release.yml, or protection reporting zero contexts
    (fails closed — unreadable protection is not a clean bill of health).
- **`release-preflight.mjs`** — `release.yml`, the `preflight` job. The
  green-check guard for BOTH release paths, gated on the publish decision rather
  than on the branch: it runs whenever `app_publish` or `chart_publish` is true,
  in `mainline` mode on `main` and in `maintenance` mode on a
  `release/<major>.<minor>.x` push. Before the publish jobs run it reads the
  pushed commit's check-runs + legacy statuses via the GitHub API and FAILS the
  release unless every check it gates is settled green (success / skipped /
  neutral). Branch protection is not sufficient on either path: it can only
  cover lanes eligible to BE a branch-protection check, and the heavyweight
  push/schedule-only lanes (`migration-e2e`) are not.
  **ABSENCE IS A FAILURE.** The gate is not derived from what the API returned —
  `RELEASE_REQUIRED_CHECKS` is an EXPECTED SET, and a required lane that posted
  no check-run on the commit is a blocking problem, not a silence. Three
  degradations are themselves blocking problems: an EMPTY required set (the gate
  would collapse back into an observational deny-list), a required name also
  listed in `RELEASE_SELF_JOBS`, and a required name swallowed by a
  `RELEASE_INFORMATIONAL_CHECKS` prefix. Because `release.yml` fires on the SAME
  push as the CI workflows, the driver first WAITS — polling the commit's
  check-SUITES (`allSuitesSettled`) until every suite except this release run's
  own (resolved via `GITHUB_RUN_ID` -> `check_suite_id`) is `completed`, AND
  polling `requiredChecksPending` until every required lane is present and
  completed, so a lane that never starts times out loudly instead of reading as
  settled. The wait is bounded (`releaseWaitMinutes` / `pollIntervalMs` ~30 s);
  on timeout it ABORTS naming the missing lanes and the still-running suites
  (fail-safe — never publish on an incomplete state). The release run's own jobs
  (`gate` / `preflight` / `goreleaser` / `release-artifact-migration` /
  `publish` / `brew-smoke` / `chart-release`) are excluded via
  `RELEASE_SELF_JOBS`, along with their reusable-workflow children (`<self-job>
  / <child>`, split on `REUSABLE_JOB_SEPARATOR`) — gating on in-flight self-jobs
  would deadlock. Latest-per-name dedup means a green re-run supersedes an
  earlier red. The branch-tip rule is `maintenance`-only: `main` legitimately
  moves while a release waits on CI. The publish jobs guard
  `needs.preflight.result == 'success'` with no `|| skipped` disjunction, since
  the preflight now fires on every publishing run. It ALSO enforces
  the **release support-window / EOL policy** (`SUPPORTED_MINOR_LINES = 3`): the
  pushed line must be within the latest 3 minor lines (current + the two prior);
  a push to a line 3+ minors behind the current minor (derived from the stable
  `v*` tag set, listed via the API so no fetch-depth is needed) is REFUSED
  before any artifact publishes, independent of how green the commit is. See
  `docs/operations.md` "Release support window / EOL policy". The SAME module
  also drives the **active** half of the EOL policy via the `eol-retire-line`
  command (the `eol-retire` job in `release.yml`): post-publish, it computes the
  line that just fell out of the window with `retireLineForPublish` (the same
  `SUPPORTED_MINOR_LINES` math — publishing `1.6.0` retires `release/1.3.x`) and
  DELETES that `release/X.W.x` branch via the Git refs API iff it exists.
  Conservative + fail-open: it retires at most one line, only on a minor open
  (`X.Y.0`, `Y>0`) — patches / major bumps / backports / prereleases retire
  nothing — deletes only a provably out-of-window branch that exists (idempotent
  on a 404) with a `supportWindowProblem` cross-check, and ANY deletion failure
  logs `::error::` and still exits 0 (the release already published). Pure
  exported `MAINTENANCE_BRANCH_RE` + `currentMinor(tags)` +
  `supportWindowProblem({branch, tags, windowSize})` +
  `retireLineForPublish({version, tags, windowSize})` + `evaluate({branchHead,
  pushedSha, checkRuns, statuses, selfJobs, branchLabel, tags, informational,
  required, mode})` + `requiredChecksPending(checkRuns, required)` (no network,
  no `process.exit`) + a `--self-test`. `release-preflight.test.mjs` is the
  `node --test` sibling wired into the required `lint` lane — `release.yml` has
  no `pull_request:` trigger, so without it every edit to the gate would be
  unverified until a release is cut. Its headline case is an absent required
  lane blocking the publish, paired with the negative control that the same
  world is green once the name leaves `required`.
  - Env (preflight, default command): `GITHUB_TOKEN` (checks:read +
    statuses:read + contents:read), `GITHUB_REPOSITORY`, `GITHUB_SHA` (pushed
    commit), `GITHUB_REF_NAME` (`main` -> `mainline` mode,
    `release/<major>.<minor>.x` -> `maintenance` mode), `GITHUB_API_URL`
    (default `https://api.github.com`), `RELEASE_SELF_JOBS` (newline-separated
    self-job names to exclude), `RELEASE_REQUIRED_CHECKS` (newline-separated
    EXPECTED set — every name must have posted a green check-run; empty is a
    hard failure), `RELEASE_INFORMATIONAL_CHECKS` (newline-separated name
    PREFIXES to observe but not gate on). All three are split on the NEWLINE
    and only the newline: check-run names are job display names that may
    contain commas — `property (PromQL + LogQL + TraceQL, rapid N=500)` is a
    branch-protection required context — so a comma-separated value yields
    names no lane can ever post, and the preflight waits out its whole window
    and aborts the release. `release.yml` declares all three as YAML block
    scalars, one name per line.
  - Env (`eol-retire-line` command): `GITHUB_TOKEN` (contents:write — the
    workflow passes `RELEASE_PAT || github.token`), `GITHUB_REPOSITORY`,
    `RELEASE_APP_VERSION` (the just-published `X.Y.Z`), `GITHUB_API_URL`.
  - Args: argv `--self-test` runs the assertion suite; `eol-retire-line` runs
    the active-EOL retirement; no command runs the preflight gate.
  - Exit (preflight): `0` when every required lane ran green and all other gated
    checks are green (plus, in `maintenance` mode, the line is in-window and the
    commit is the branch tip) — or a green self-test; `1` on a required lane
    that never ran, an empty / self-colliding / informational-swallowed required
    set, any red/running gated check, an end-of-life line, a non-tip /
    unresolved commit, or a missing required env var.
  - Exit (`eol-retire-line`): `0` always (fail-open — a retirement failure must
    never fail an already-published release); `1` only on a gross wiring error
    (missing repo/token) before any publish-affecting work.
- **`brew-smoke.mjs`** — `release.yml`, the `brew-smoke` job (post-`publish`),
  and `brew-verify.yml`, which re-runs the identical assertions on demand or
  weekly against an ALREADY-published version (the release-run job cannot be
  replayed with a fix, since `rerun-failed-jobs` restores the workflow as it
  was at the release commit). Both jobs run a `macos-latest` + `ubuntu-latest`
  matrix, so `brew install` is exercised under Linuxbrew as well as on a Mac —
  cerberus is a Linux-first server binary, and a cask broken for Linux is
  indistinguishable from a healthy one when only macOS installs it. The Ubuntu
  image ships Homebrew under `/home/linuxbrew` but leaves it off `PATH`; one
  Linux-only step adds it.
  Proves the Homebrew tap actually serves the version that just shipped. Reads
  `Casks/cerberus.rb` from `tsouza/homebrew-tap` through the API FIRST: that
  is the anti-vacuous check, because a deleted `homebrew_casks:` block or an expired
  `HOMEBREW_TAP_GITHUB_TOKEN` leaves a STALE cask that a warm `brew install`
  would install happily. Then it branches EXPLICITLY on the release kind rather
  than skipping: a stable `X.Y.Z` on the newest release line requires the
  cask to declare exactly that version, then installs it and asserts the
  binary lands under `brew --prefix`,
  that `cerberus --version` EQUALS the bare release version (string equality, so
  an off-by-`v` or a stale ldflag cannot pass a substring test), and that two
  OFFLINE payload verbs work — `migrate schema` (emits `CREATE` DDL) and
  `config-docs -check` against `REPO_ROOT`'s `docs/configuration.md`. The other
  two release shapes take negative branches, because `.goreleaser.yml`'s
  `skip_upload` template keeps both out of the tap: an `rc.*` must NOT have
  written a cask, and a release that is not the highest stable tag (a
  maintenance backport) must have left a STRICTLY NEWER cask in place — the
  regression that let v1.12.1, cut after v1.13.0, downgrade every
  `brew install`. `migrate gate` / `migrate verify` are deliberately unused —
  they exit non-zero on a legitimate no-go, so they cannot distinguish a broken
  binary from a correct verdict. On EVERY branch — including the two that
  install nothing — the cask source is also checked for cross-platform shape:
  all four `darwin_amd64` / `darwin_arm64` / `linux_amd64` / `linux_arm64`
  artefacts present, and no macOS-only artifact stanza (`app`, `pkg`,
  `installer`, …), which is the sole condition Homebrew's Linux cask installer
  refuses on. Whatever cask the tap holds is what every `brew install` gets, so
  a Linux-broken cask is broken today regardless of which release was allowed
  to write it. Pure exports `isStableRelease(version)`,
  `caskVersion(rbSource)` (declaration, then archive-name fallback, then
  THROWS — an unparseable cask never degrades into a pass),
  `caskPortabilityProblems(rbSource)`,
  `formulaShadowProblems(tapPaths)` / `tapShadowingFormulaPaths(tapPaths)` — the
  tap must not still serve the `Formula/cerberus.rb` it held before
  `.goreleaser.yml` moved off `brews:`, because Homebrew resolves a bare
  `tsouza/tap/cerberus` to the FORMULA when a tap holds both, and goreleaser
  deletes nothing it did not write; the whole tap listing is matched against
  every root Homebrew loads formulae from, so a sharded or relocated formula
  cannot slip past — `installedArtifactProblems(caskList, formulaList)` — which
  of the two the bare install actually resolved to, read off
  `brew list --{cask,formula} --versions`, via the shared
  `brewListNames(out)` — `tapMigrationProblems(source)` — the tap's
  `tap_migrations.json` must map `cerberus` to the bare tap name `tsouza/tap`,
  which is what tells Homebrew the formula BECAME the cask; a missing map is
  invisible to every fresh install and strands every existing formula install on
  its old binary with no error printed, so the file is asserted on every release
  rather than once at migration time —
  `compareVersions(a, b)` and `verdict({version, caskSource, isLatest})`,
  covered by `brew-smoke.test.mjs` on the required `lint` lane and as the job's
  own first step.
  - Env: `RELEASE_VERSION` (the BARE `X.Y.Z[-rc.N]`, i.e.
    `needs.gate.outputs.app_version`, never the `v`-prefixed tag),
    `RELEASE_IS_LATEST` (`"true"`/`"false"` — whether this is the highest stable
    tag, i.e. the line that owns the tap's single cask; required, and
    rejected unless it is exactly one of those two words, since it selects the
    assertion branch), `GITHUB_TOKEN` (contents:read on the tap),
    `GITHUB_API_URL` (default `https://api.github.com`), `REPO_ROOT`
    (checkout root, for the `config-docs -check` payload).
  - Exit: `0` when the cask state matches the release kind and (newest stable
    line only) the installed binary passes every assertion; `1` on a stale /
    missing / unparseable cask, a prerelease cask that should not exist, a
    tap cask that a backport overwrote, a missing or unrecognised
    `RELEASE_IS_LATEST`, a failed install, a version mismatch, or a failing
    payload verb.
- **`brew-upgrade-path.mjs`** — `brew-verify.yml`, the `brew-migration` job
  (`macos-latest` + `ubuntu-latest`, weekly + on demand). Proves an EXISTING
  formula install is carried across to the cask. Every other install path CI
  performs starts from an empty runner, and a machine with no cerberus on it
  cannot observe the upgrade at all — which is how the formula → cask move
  shipped with no `tap_migrations.json`: fresh installs were correct throughout
  while every machine that already had the formula silently kept its old binary
  (`brew upgrade` files a deleted formula under *Deleted Installed Formulae* and
  moves on; installing the cask afterwards will not link over the formula's keg;
  nothing in that sequence prints an error). The harness reconstructs a real
  pre-migration machine: it rewinds the tap clone to the parent of the commit
  that deleted the formula — derived from history, since a pinned SHA would drift
  out of the tap and leave the job installing the CASK in step one and then
  asserting vacuously that a cask is installed — installs the formula from it via
  the bare ref, and lets `brew update` catch the clone up. That is the real
  trigger: Homebrew drives the migration off the update's report of what was
  DELETED, so it fires only for a clone that observes the deletion. It then
  asserts three independent things, because they fail for different reasons: the
  migration was announced, the cask ended up installed (Homebrew rescues a failed
  migration install by design, so the announcement alone proves nothing), and the
  binary on PATH reports the cask's version (the symptom the user sees, and the
  one that fails on its own when the cask installed but could not link). Pure
  export `upgradeOutcomeProblems({updateOutput, caskList, reportedVersion,
  expectedVersion})`, covered by `brew-upgrade-path.test.mjs` on the required
  `lint` job and as the job's own first step.
  - Env: `TAP_MIGRATION_LEGACY_REV` (optional; overrides the derived rewind
    point, for reproducing a specific report).
  - Exit: `0` when the migration carried the machine across; `1` on a silent
    update, a cask that never installed, a version that never moved, or a tap
    whose history holds no formula to rewind to. There is no "could not tell"
    exit.
- **`goreleaser-deprecations.mjs`** — `ci.yml`, the `lint` job. Fails the build
  when `.goreleaser.yml` uses any goreleaser feature upstream has deprecated.
  `goreleaser check` prints a deprecation as an advisory line and still exits
  `0`, and release.yml (which has no `pull_request:` trigger) is the only other
  place goreleaser runs — so a deprecation notice first becomes visible in the
  scroll-back of a release that already published, which is how `brews:` stayed
  in the config for several releases after `homebrew_casks:` replaced it. The
  gate reads the real tool's output rather than scanning the YAML for a
  hardcoded list of dead keys, so a deprecation announced upstream tomorrow is
  caught without this module knowing about it in advance. The CI step installs
  the same `distribution` / `version` release.yml pins, so the gate reports
  exactly what the next release run will warn about. Pure export
  `deprecationsIn(output)` returns the DEDUPLICATED notices in a transcript
  (goreleaser repeats one line per offending block, and reporting the same
  migration N times obscures how many distinct ones are outstanding), covered by
  `goreleaser-deprecations.test.mjs` on the same lane.
  - Env: `GORELEASER_CONFIG` (default `.goreleaser.yml`), `GORELEASER_BIN`
    (default `goreleaser`).
  - Exit: `0` when the config validates with no deprecation notices; `1` on any
    notice, or when `goreleaser check` itself fails or cannot run.
- **`chart-kubeconform.mjs`** — `chart-ci.yml`, the `Render + kubeconform`
  step. Renders the chart for the default values and every `ci/*-values.yaml`
  fixture, schema-validates each manifest set with `kubeconform -strict`, and
  probes the rendered container image tag against the registry — the guard for
  an `appVersion` pointing at an unpublished tag. An image passes only when the
  probe positively confirms it: a definitive not-found fails, and so does any
  probe that reaches no verdict (auth refusal, rate limit, DNS/TLS), because a
  guard that could not run has verified nothing. Because the chart renders a
  Docker Hub ref and Docker Hub rate-limits CI bursts, a probe with no verdict
  is retried — five attempts with linear backoff, mirroring the Justfile's
  `_pull-retry` — so a transient refusal does not become a permanent
  non-verdict; a verdict (`present` or a definitive not-found) is never
  retried. Exhausted retries still fail. The one exemption is the appVersion
  the change itself stages, and it covers a definitive not-found only. The
  `--self-test` drives the real probe against a controlled failing command, so
  the spawn options are pinned at the call site and the retry loop runs for
  real with its sleep injected.
  - Env: `CHART_DIR` (default `deploy/helm/cerberus`), `KUBE_VERSION` (default
    `1.28.0`), `SKIP_IMAGE_CHECK` (set `1` to skip the registry probe entirely
    — the only waiver, for air-gapped runs).
  - Args: argv `--self-test` runs the assertion suite; no args runs the gate.
  - Exit: `0` when all fixtures validate and every image is confirmed present
    (or exempt); `1` on any kubeconform failure, a missing image, or an image
    the probe could not verify.
- **`chart-render-assert.mjs`** — `chart-ci.yml`, the `Render assertions`
  step. Behavioural render checks kubeconform's schema validation cannot make:
  split mode renders one PodDisruptionBudget per enabled head (each with its
  `app.kubernetes.io/component` selector), the monolith PDB render is unchanged,
  and each container gets a `GOMEMLIMIT` env sized to ~80% of its own
  `resources.limits.memory` (per-head in split, per-pod in monolith) — with an
  explicit `extraEnv` `GOMEMLIMIT` always winning and an unset limit emitting
  nothing.
  - Env: `CHART_DIR` (default `deploy/helm/cerberus`).
  - Exit: `0` when every assertion holds; `1` on the first failure.
- **`compat-step-summary.mjs`** — `compatibility.yml`, the three
  `Append score to step summary` steps.
  - Env: `HEAD` (`prometheus`, `tempo`, or `loki`), `SCORE` (path to that
    head's `compat-score.json`).
  - Exit: always `0` (housekeeping; never gates).
- **`compat-ratchet.mjs`** — `compatibility.yml`, the three
  `Parity-regression ratchet` steps. The GATE that makes the required
  `compatibility/{prometheus,loki,tempo}` checks fail on a parity
  regression (not just on infra breakage). Compares the run's
  `compat-cases.json` roster against the committed roster in
  `compatibility/parity-baseline.json` and fails on any case that moved:
  REGRESSED (recorded case now diverges), VANISHED (recorded case did not
  run), ARRIVED-FAILING (new case diverges on arrival), UNRECORDED (new
  case passes but nothing gates on it yet). Gating on case identity rather
  than a count is the point — a count cannot tell a swap from a steady
  state. Case IDs come from static corpus identity, so it can't flake. Not
  an allow-list: the roster names the cases that must pass, and the
  baseline records full parity (`passed == total == cases.length`), so
  there is no shape in which a divergence is recordable as acceptable.
  - Env: `HEAD` (`prometheus`, `tempo`, or `loki`), `SCORE` (path to that
    head's `compat-score.json`), `CASES` (path to that head's
    `compat-cases.json`), `BASELINE` (optional; default
    `compatibility/parity-baseline.json`).
  - Exit: `0` when the run matches the baseline roster exactly, `1` on any
    moved case or a missing/malformed/internally-inconsistent score,
    cases, or baseline file.
  - Tests: `compat-ratchet.test.mjs` (run in `ci.yml`), which drives the
    script over fixture artefacts and includes the negative control — a
    case regresses while an unrelated one starts passing, so every
    aggregate count is satisfied and the gate must still fail.
- **`compat-baseline-sync.mjs`** — not wired into a workflow; run by hand
  when a corpus change legitimately moves a head's roster. Rewrites
  `heads.<head>` in `compatibility/parity-baseline.json` from a run's
  `compat-cases.json` artefact, sorted and with counts derived from the
  roster, so the committed list cannot drift from what the harness ran.
  Refuses to write a roster that omits a failing case — that would make it
  an allow-list generator.
  - Args: path to a `compat-cases.json` (its `head` field selects the
    entry to rewrite).
  - Env: `BASELINE` (optional; default
    `compatibility/parity-baseline.json`).
  - Exit: `0` on a rewritten (or already-current) entry, `1` on bad
    arguments, unreadable/malformed input, or a run containing a failing
    case.
- **`compat-publish-score.mjs`** — `compatibility.yml`, the three
  `Publish score to compat-scores branch` steps. Publishes the head's
  shields.io endpoint-badge JSON to `badges/<head>.json` on the
  `compat-scores` orphan branch, which the README badges read over the raw
  URL. Projects the score file down to the four endpoint-schema keys
  (shields.io renders an `invalid properties` title if it sees cerberus's
  `passed` / `total` / `percent`), bootstraps the orphan branch on first
  run, and retries a rejected push so the three head jobs can race the same
  branch. The workflow's `if:` gate keeps it to push-to-`main`. All git work
  happens in a throwaway `git worktree` under `$RUNNER_TEMP` that is torn
  down on every exit path, so the job's checkout is never switched: later
  steps and the POST phase of a local composite action such as
  `./.github/actions/setup-buildx` resolve their files out of it.
  - Env: `HEAD` (`prometheus`, `tempo`, or `loki`), `SRC` (path to that
    head's `compat-score.json`); `RUNNER_TEMP` (optional; the OS temp dir
    off-runner) sites the scratch worktree.
  - Exit: `0` published / already current / no score file to publish; `1`
    on a wiring slip, an unreadable score, or every push attempt rejected.
  - Tests: `compat-publish-score.test.mjs` (run in `ci.yml`), which
    publishes against a real bare-repo remote in a temp dir and asserts on
    every scenario that the checkout came out on its original branch with
    its files and scratch-worktree count unchanged.
- **`resolve-bench-refs.mjs`** — `perf-benchmark.yml`, the
  `resolve baseline + ref SHAs` step.
  - Env: `INPUT_BASELINE_REF` (optional); writes `ref_sha`,
    `baseline_sha`, and `baseline_ref` to `$GITHUB_OUTPUT`.
  - Exit: `0` resolved, `1` on baseline `==` ref or a git error.
- **`chaos-run.mjs`** — `e2e.yml`, the `chaos` job (live-stack
  chaos-engineering lane, Layer 12). Fault-injects against the running
  k3d e2e stack (kubectl pod-kill / NetworkPolicy partition / slow-query
  timeout / concurrency burst / a memory-cap-crossing query_range) and
  asserts the gateway's resilience contracts (circuit breaker, per-query
  wall-clock timeout, admission control, replica resilience,
  failure-driven route-memo A->B activation) hold under real faults.
  Phase-1 scenarios
  run sequentially with heal-between-each; metric corroboration is read
  back through cerberus's own Prom head (settle poll). After a
  CH-destructive scenario (which recreates CH empty on ephemeral storage),
  the heal gate shells out to `just e2e-reseed` to repopulate ClickHouse
  before the next scenario asserts. INFORMATIONAL — never a PR gate.
  - Env: `CERBERUS_URL` (default `http://localhost:8080`), `CHAOS_NS`
    (default `cerberus`), `CHAOS_PHASE` (`phase-1` | `all`, default
    `phase-1`), `CHAOS_SCENARIOS` (comma list to run a subset),
    `CHAOS_MANIFESTS` (default `test/e2e/chaos/manifests`).
  - Exit: `0` all selected scenarios passed (or recorded not-applicable
    with a `::notice::`), `1` on any contract-assertion failure.
- **`e2e-bwc-verify-placement.mjs`** — `e2e.yml`, the `bwc-minio` job
  (bundled-ClickHouse-on-object-storage lane), invoked via
  `just e2e-bwc-verify`. Asserts the bundled CH data tier physically lives on
  object storage: `storage_policy='bwc_object_store'` stamped on every
  cerberus-managed OTel MergeTree table, all active `system.parts` on the
  object/cache disk (never the local `default` disk), and the MinIO bucket
  non-empty after the seed (polled; placement object writes lag the insert).
  INFORMATIONAL — never a PR gate.
  - Env: `NAMESPACE` (default `cerberus`), `DATABASE` (default `otel`),
    `CH_USER`/`CH_PASSWORD` (default `cerberus`/`cerberus`), `BUCKET` (default
    `cerberus-bwc`), `STORAGE_POLICY` (default `bwc_object_store`), `MC_IMAGE`
    (default the pinned `minio/mc` RELEASE), `POLL_SECONDS` (default `30`).
  - Exit: `0` all assertions passed, `1` on any placement-assertion failure.
- **`e2e-cerberus-restart-gate.mjs`** — `e2e.yml`, the `Assert zero
  cerberus restarts` step on the k3d dashboard/crawl shards. Sums
  `restartCount` across the cerberus pods; on any restart dumps the
  OOM-specific evidence the inline bash lacked — `lastState.terminated`
  Reason (OOMKilled surfaced loudly; a `--previous` log tail is empty for an
  OOM kill), `resources.limits` + `GOMEMLIMIT`, `kubectl top` per-container
  usage (best-effort, skips gracefully when metrics-server is absent in
  k3d), and a live `/debug/pprof/heap` pulled from each running container
  (when `CERBERUS_DEBUG_PPROF` is on) into `PPROF_OUT_DIR` for artifact
  upload. A kubectl read failure is treated as "couldn't determine" (exit 0,
  matching the prior leniency) rather than a false fail.
  - Env: `NAMESPACE` (default `cerberus`), `PPROF_OUT_DIR` (default `/tmp`).
  - Exit: `0` when restarts == 0 (or unreadable), `1` when restarts > 0
    (after dumping evidence).
- **`promql-surface-gate.mjs`** — `compatibility.yml`, the
  `compatibility/promql-surface` job (reference-backed full-surface PromQL
  rejection-completeness gate, #106). Stands up a flag-enabled reference
  Prometheus (`--enable-feature=promql-experimental-functions`), probes
  every PromQL parser symbol over HTTP `/api/v1/query`, and fails on any
  symbol the reference ACCEPTS that cerberus REJECTS but isn't a recorded
  wrong-reject (a silent coverage gap), on artifact drift, or on a
  showcase declared-rejection panel the reference accepts.
  - Env: `PROM_IMAGE` (default `prom/prometheus:v3.11.3`), `REF_PORT`
    (default `39090`), `INVENTORY`, `ARTIFACT`, `SHOWCASE` (defaults under
    `test/surface-parity/` + the compose showcase dashboard), `REGENERATE`
    (`1` rewrites the verdict artifact from the live reference + exits),
    `KEEP_REF` (`1` leaves the reference container up for local debugging).
  - Exit: `0` all checks pass / regenerate done, `1` on any gap / drift /
    misfile / infra error. Self-managing: starts + `docker rm -f`s its own
    reference container.

- **`compose-smoke-scope.mjs`** — `e2e.yml`, the `compose-smoke-scope` job.
  Decides whether a pull request has to boot the compose stack at all. The lane
  is a release gate, so an ordinary PR short-circuits it — but it is also the
  only layer that runs cerberus against a REAL ClickHouse server, and chDB (what
  every other execution layer uses) coerces column types the server rejects. So
  the module keeps the short-circuit for changes the stack cannot see and boots
  it for the ones it can: `HARNESS_PATHS` (the stack's own definition and
  driver) and `SCOPE_PATHS` (`internal/chsql` emitted types, `internal/api` HTTP
  surface, `internal/chclient` driver conversation, `cmd/cerberus` startup) —
  deliberately NOT all of `internal/**`, which would re-gate every PR for
  coverage the chdb-backed layers already give. `verify` asserts every declared
  path still exists (a renamed entry matches nothing and silently retires the
  gate); `emit` writes `in_scope` to `$GITHUB_OUTPUT`. Every ambiguity resolves
  to `true`: push / schedule / `release/*` PRs always run the full lane, and an
  uncomputable diff boots the stack rather than skipping it. A merge-queue entry
  is scoped like a pull request, off its own `base_sha..head_sha`. `workflow_dispatch`
  is the one named exception (`NON_BOOTING_EVENTS`) — e2e.yml's only dispatch
  input regenerates the k3d crawl inventory, which no compose shard can see.
  `compose-smoke-scope.test.mjs` pins the in/out decisions exactly (run on the
  `forbid-skip` job), and the `compose-smoke` aggregator treats a skipped setup
  as green only when this job SUCCEEDED and reported `in_scope=false`.
  - Env: `MODE` (`verify` | `emit`; also `argv[2]`; default `verify`),
    `EVENT_NAME`, `HEAD_REF`, `BASE_SHA`, `HEAD_SHA`, `GITHUB_OUTPUT`.

- **`compose-smoke-matrix.mjs`** — `e2e.yml`, the `compose-smoke-setup` job.
  Single source of truth for how the `compose-smoke` required PR gate fans its
  10 Playwright spec files out across a balanced matrix of isolated-compose-
  stack shards. The three heaviest specs are each one indivisible async
  `test()` (Playwright's native `--shard` can't split them), so the
  parallelism is LOGICAL — split the spec FILES across jobs, each booting its
  own stack. The `SHARDS` partition + `EXCLUDED` list live in this module;
  specs are DISCOVERED (`git ls-files`) so a new `*.spec.ts` is a hard CI
  failure unless assigned to a shard or named in `EXCLUDED` — no silent
  no-run. Coverage assertion is collect-all-violations: unassigned (the
  forbidden gap), double-assigned, phantom/stale, and bad-shard-name are each
  reported, then `exit 1`. `compose-smoke-matrix.test.mjs` is the `node --test`
  guard (run on the cheap `forbid-skip` job) that pins the invariant + proves the
  detectors fire. Two extra responsibilities: (1) it carries the per-shard
  `timeoutMinutes` ceiling on each emitted entry — the crawl shard gets a hard
  30-min cap (`CRAWL_SHARD_TIMEOUT_MIN`; fail fast, release the concurrency
  slot), non-crawl shards keep 120 (nightly full, `IS_SCHEDULE=true`) / 45
  (PR/push lean); (2) it splits the partition into a REQUIRED `matrix` and an
  informational `matrix_informational` (the `GATE_EXCLUDED_SHARDS` coverage
  shards — today `shard-crawl`). The required `compose-smoke` aggregator
  `needs:` only the required matrix, so a crawl flake/hang reports its own
  visible `compose-smoke-shard-info (shard-crawl)` check but does NOT fail the
  required gate. Both matrices derive from the same `SHARDS` +
  `GATE_EXCLUDED_SHARDS`, so they can't drift.
  - Env: `MODE` (`verify` | `emit`; also `argv[2]`; default `verify`),
    `PLAYWRIGHT_DIR` (default `test/e2e/playwright`), `IS_SCHEDULE` (emit:
    `"true"` selects the full non-crawl timeout), `GITHUB_OUTPUT` (emit: the
    runner file the `matrix` / `matrix_informational` / `has_informational` /
    `gate_excluded` outputs are written to).
  - Exit: `0` clean / matrix emitted, `1` on any coverage violation or bad
    `MODE`.

- **`migration-e2e.mjs`** — `ci.yml`'s `lint` job (`MODE=verify`) and
  `migration-e2e.yml` (all four modes). The Layer-14 migration lane's
  coverage ratchet, tier-matrix emitter, scenario runner and lane roll-up.
  `TIER_JOBS` is the single table of which tiers `migration-e2e.yml` has a job
  for, each tier's ceiling, and whether that job is matrix-driven — tier0 is
  one generic runner fanned out by `strategy.matrix`; tier1 is a fixed stanza
  that raises a compose stack around the suite, so it is runnable but stays
  out of the emitted matrix (a tier1 matrix entry would run the tier-1 suite
  on a bare runner with no stack behind it). `MODE=rollup` folds the tier
  results: every tier `emit` selected must report `success` — `cancelled` and
  `skipped` included, which is exactly what a `contains(needs.*.result,
  'failure')` fold would wave through — and a tier that ran without being
  selected is reported too, since the job gate and the roll-up read the same
  `tiers` output and must not disagree. It consumes the
  JSON that `test/e2e/migration/cmd/scenarios` projects out of the Gherkin
  feature files — it never parses Gherkin itself, so exactly one Gherkin
  parser exists in the tree — and holds that scenario set to anchors derived
  LIVE from `docs/migration-testing.md`: the 26-story table in section 4, the
  Tier(s) column in section 6, and the archetype table in section 7 (which is
  cross-checked against the directories under `test/e2e/migration/archetypes`).
  Fifteen collect-all violation classes cover a stale/duplicate/missing story
  tag, a tier tag contradicting the doc, an unrecognised tag (the route by
  which `@wip` / `@skip` reach the ratchet), an unknown archetype, a Scenario
  with no `Then` (it asserts nothing), and a number or an operator in step
  text (an inline epsilon is a per-case allow-list; the arithmetic lives in
  Go). `test/e2e/migration/coverage-baseline.json` pins the aggregate
  scenario floor — raise-only, and growing coverage without raising it fails
  too, so the ratchet tightens instead of ossifying. It is NOT an allow-list:
  it names no story, and every structural class applies unconditionally to
  every scenario that exists. `migration-e2e.test.mjs` is the `node --test`
  guard (run on the required `lint` lane) that pins the doc parsers against
  the live doc and proves each detector fires.
  `MODE=attest` is the execution gate: `verify` walks feature FILES, so a
  scenario that never ran counts exactly the same as one that passed — the
  shape that let a branch report "30/30 across 26/26 stories; 0 violations"
  while its Tier-2 scenarios had been skipped by a `needs:` cascade.
  `MODE=run` therefore owns the path each tier's godog suite writes a
  cucumber-JSON run report to (via `test/e2e/migration/lib.SuiteFormat`, so a
  new tier is attestable by construction), each tier job uploads it, and the
  `migration-aggregate` job downloads them all and holds every counted
  scenario to *appeared in a report, every step passed*. It attests only the
  tiers `emit` SELECTED — the same `tiers` output the roll-up reads — so a
  narrowed dispatch is not failed by the tiers it deliberately skipped.
  `verify`'s notice reports enumerated and attested coverage as two different
  numbers. `test/e2e/migration/pass-assertions.pin.json` hashes every
  section-6 PASS cell: it does not forbid narrowing a PASS assertion, it
  forbids narrowing one *silently*, by making the re-pin a reviewed line in
  the same diff (there is deliberately no regeneration mode).
  - Env: `MODE` (`verify` | `emit` | `run` | `attest` | `rollup`; also
    `argv[2]`; default `verify`), `SCENARIOS_JSON` (default
    `build/migration-scenarios.json`), `STORIES_DOC` (default
    `docs/migration-testing.md`), `ARCHETYPE_ROOT` (default
    `test/e2e/migration/archetypes`), `FEATURE_ROOT` (default
    `test/e2e/migration/features`), `COVERAGE_BASELINE` (default
    `test/e2e/migration/coverage-baseline.json`), `PASS_ASSERTION_PIN`
    (default `test/e2e/migration/pass-assertions.pin.json`), `REPORT_DIR`
    (run/attest/verify's notice; default `build/migration-reports`), `TIER`
    (emit/run: `all` | `tier0` | `tier1` | `tier2`; default `all`), `STORY`
    (run/attest: a single MIG id), `CERBERUS_BIN` (run: a prebuilt binary,
    passed through to the suite), `GITHUB_OUTPUT` (emit: the runner file the
    `matrix` output is written to), `EXPECTED_TIERS` (rollup/attest: `emit`'s
    `tiers` output, verbatim), `SETUP_RESULT` / `RESULT_TIER0..2` (rollup).
    `MIGRATION_REPORT` is an OUTPUT, not an input: `MODE=run` exports it to
    the `go test` child as the absolute report path.
  - Exit: `0` clean, `1` on any coverage violation, an unattested scenario, a
    PASS-cell pin mismatch, an unknown `MODE`, a missing/malformed input, a
    requested tier the workflow has no job for, or a failed suite run.

- **`migration-artifact.mjs`** — `migration-e2e.yml`, the first step of every
  tier job (`migration-tier0` / `migration-tier1` / `migration-tier2`). The
  single decision point for *which cerberus the lane proves*, and the reason the
  lane can be pointed at a released artifact at all. Two inputs travel together
  and are the whole contract: supply both `cerberus_image` + `expect_version`
  (`release.yml` calls the lane this way, naming the image goreleaser just
  built) and the module pulls that image, `docker cp`s
  `/usr/local/bin/cerberus` out of it, and exports it as `CERBERUS_BIN`;
  supply neither (every PR / dispatch / push run) and it `go build`s the CLI
  from the tree instead. Supplying exactly one is an error, not a default — a
  half pair would run a source build under an artifact-shaped job name, which
  is the exact hollow green the module exists to prevent. The CLI is extracted
  FROM the image rather than from a release tarball so the binary the scenarios
  exec and the server the stack runs are the same bytes. Whichever binary it
  produced is then held to its expected version: the module runs `--version`,
  requires exit 0 and exactly one non-empty line, and string-compares it — a
  build that is not the build this run claims to drive fails here, before a
  single scenario runs, instead of passing thirty of them against the wrong
  artifact. The two paths carry different stamps by construction (`dev` for a
  bare `go build`, `e2e` for `Dockerfile.local`, `<version>` for a goreleaser
  build), which is what makes the probe a real discriminator; the same
  constants live in `test/e2e/migration/lib/provenance.go` for the Go side, and
  `test/regression/migration_tier1_test.go` holds every copy equal. The
  exported `CERBERUS_IMAGE` is what the tier-1/tier-2 compose stacks
  interpolate
  (`image: ${CERBERUS_IMAGE:-cerberus:migration-tier1${COMPOSE_PROJECT_SUFFIX:-}}`,
  `pull_policy: never`), and `CERBERUS_EXPECT_VERSION` is exported only on the
  image path — on the source path the CLI and the compose image are two
  different builds, so each is held to its own stamp rather than to a shared
  one. `migration-artifact.test.mjs` is a `node --test` guard on the required
  `lint` lane covering the plan resolver, including the mutation control that a
  half pair never yields a plan in either direction.
  - Env: `CERBERUS_IMAGE_INPUT` / `CERBERUS_EXPECT_VERSION_INPUT` (the two
    workflow inputs; both empty = build from source, both set = test that
    image, exactly one set = error),
    `GITHUB_ENV` (the runner file `CERBERUS_BIN` / `CERBERUS_IMAGE` /
    `CERBERUS_EXPECT_VERSION` are appended to), `COMPOSE_PROJECT_SUFFIX` (the
    per-checkout suffix the local image tag carries, so the tag this module
    names is the one the stack runs; empty in a CI checkout, which is what
    every CI checkout is — see `scripts/compose-project-suffix.sh`).
  - Exit: `0` once a binary exists and its `--version` matches, `1` on a half
    pair, a failed build / pull / extract, a `--version` that errors, prints
    nothing, prints more than one line, or prints the wrong stamp.

- **`dashboard-matrix.mjs`** — `e2e.yml`, the `dashboard-setup` job. The k3d
  twin of `compose-smoke-matrix.mjs`: single source of truth for how the
  `dashboard` (k3d) lane fans its Playwright spec set across a MODEST matrix
  (3) of isolated-k3d-cluster shards. The dominant cost is the crawl BFS — one
  indivisible async `test()`, the ~50min long pole — so the parallelism is
  COARSE: two smoke shards (non-crawl specs, `CRAWL_STACK` unset → `crawl/**`
  ignored) run CONCURRENTLY with a DEDICATED crawl shard (`CRAWL_STACK=k3d`,
  `SWEEP_DEPTH=full`). Splitting the crawl frontier itself is the follow-up.
  The `SHARDS` partition (each carrying `specs` + `crawlStack` + `runGoE2E`) +
  `EXCLUDED` list live in this module; specs are DISCOVERED (`git ls-files`) so
  a new `*.spec.ts` is a hard failure unless assigned or excluded — no silent
  no-run. Coverage assertion is collect-all-violations (unassigned,
  double-assigned, phantom/stale, bad-shard-name, and the "exactly one shard
  runs Go e2e" invariant), then `exit 1`. `dashboard-matrix.test.mjs` is the
  `node --test` guard (run on the cheap `forbid-skip` job) pinning the invariant +
  proving the detectors fire. k3d is heavy + flaky, so the shard count is kept
  deliberately small. Each emitted entry also carries a per-shard
  `timeoutMinutes`: the crawl shard gets a hard 30-min cap
  (`CRAWL_SHARD_TIMEOUT_MIN`; fail fast, release the k3d concurrency slot), the
  smoke shards keep their 75-min cluster-lifetime bound (`SMOKE_SHARD_TIMEOUT_MIN`).
  The `dashboard` aggregator is a branch-protection required check; the crawl
  shard runs only on schedule, dispatch, and release/* PRs, so it never gates
  an ordinary PR's merge-when-green.
  - Env: `MODE` (`verify` | `emit`; also `argv[2]`; default `verify`),
    `PLAYWRIGHT_DIR` (default `test/e2e/playwright`), `INCLUDE_CRAWL` (emit:
    `"true"` adds the crawl shard — schedule, dispatch, and release/* PRs),
    `INCLUDE_SPLIT` (emit: `"true"` fans the smoke shards over monolith AND
    split mode — same event set as `INCLUDE_CRAWL`), `GITHUB_OUTPUT`
    (emit: the runner file the
    `{include:[{name,specs,crawlStack,runGoE2E,timeoutMinutes}]}` matrix JSON is
    written to).
  - Exit: `0` clean / matrix emitted, `1` on any coverage violation or bad
    `MODE`.
- **`dependabot-tidy-nested-modules.mjs`** — `dependabot-tidy-nested-modules.yml`,
  the `tidy` job. `test/oracle` is a nested Go module carrying
  `replace github.com/tsouza/cerberus => ../..`, so its module graph is
  entangled with the root's; a root-only Dependabot bump routinely leaves
  `test/oracle/go.mod`/`go.sum` stale and fails `ci.yml`'s "test oracle
  module" step. Runs `go mod tidy` in each nested module dir and, only if
  that produced a diff, commits and pushes a fixup straight to the
  Dependabot PR branch — closing the loop without a human re-running it by
  hand (see PR #1211).
  - Env: `NESTED_MODULE_DIRS` (space-separated, default `test/oracle`),
    `BRANCH` (required — the branch to push the fixup to), `GIT_USER_NAME` /
    `GIT_USER_EMAIL` (default `github-actions[bot]` identity).
  - Exit: `0` nothing to do or fixup pushed; `1` on a `go mod tidy` or git
    failure.
- **`pull-buildkit-image.mjs`** — `.github/actions/setup-buildx` (the composite
  every image-building job goes through). Acquires the BuildKit bootstrap image
  into the local docker daemon, with retry, before
  `docker/setup-buildx-action` boots a builder from it. The
  `docker-container` driver pulls `moby/buildkit:<tag>` single-attempt inside
  its own bootstrap — a step no `run:`-level retry can reach — so one reset
  connection to Docker Hub fails the job before a single image is built. buildx
  falls back to a locally present copy when its pull fails, which is what makes
  pre-acquiring it sufficient; the module therefore asserts presence in the
  daemon rather than a successful pull. The composite passes the same image ref
  to `driver-opts: image=…`, so the warmed ref and the booted ref cannot drift.
  - Env: `BUILDKIT_IMAGE` (required — image ref to acquire),
    `BUILDKIT_PULL_BACKOFF_SECONDS` (optional; default `3` — linear backoff
    step, attempt N sleeps N × this).
  - Exit: `0` when the image is in the local daemon, `1` when it is not.
- **`build-with-registry-retry.mjs`** — the Justfile (`e2e-up`, `e2e-bwc-up`,
  `migration-cerberus-image`, `migration-tier{1,2}-up`), `e2e.yml`'s
  compose-smoke shards, the three `compatibility/*/scripts/run-*.sh` harnesses,
  and `bench/histogram/run.sh`. Runs an image-building command (`docker build` /
  `docker compose up|build`), retrying it when — and only when — its output
  names a registry or network fault. It exists because a build's BASE images are
  resolved by BuildKit *during* the build, so no host-side pre-pull protects
  them: a Docker Hub 429 on `golang:1.26` (the `FROM` of `Dockerfile.local`)
  failed `e2e` and `migration-e2e` on main with `unexpected status from HEAD
  request … 429 Too Many Requests`. The retry wraps the command rather than
  pre-pulling the ref because the built-in `docker` driver resolves `FROM` from
  the daemon's image store while the `docker-container` driver
  (`.github/actions/setup-buildx`) never does — a pre-pull would protect some
  lanes and silently protect nothing in the rest. Unlike the bootstrap pull
  there is no local-image fallback: a tag an earlier run left in the daemon
  attests nothing about this tree, and a build failure that is not a registry
  failure fails on the FIRST attempt so a real break is never retried into
  invisibility. Takes the command as its trailing argv.
  - Env: `IMAGE_BUILD_RETRY_BACKOFF_SECONDS` (optional; default `10` — linear
    backoff step, attempt N sleeps N × this).
  - Exit: `0` on success; the command's own status when it failed for a
    non-registry reason; `1` when the retry budget is spent.

## Notes

- **`forbid-skip.mjs` regexes are a contract.** They are kept
  byte-identical to `scripts/test-forbid-skip.sh` (the self-test step
  that pins the patterns against canonical match / no-match examples) and
  to `docs/forbid-skip.md`. When widening or normalising a pattern,
  update all three in the same change.
- **Local check / behaviour test.** Each script is plain Node — run it
  directly with representative env (e.g.
  `THRESHOLD=95 REPORT=/tmp/g.json node .github/scripts/gremlins-threshold.mjs`)
  and `node --check .github/scripts/<name>.mjs` for a syntax check.
- **Trivial one-liners and official Actions stay inline** in the
  workflow YAML — only extract steps that encode real logic.
