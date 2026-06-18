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

## Modules

- **`forbid-skip.mjs`** — `ci.yml`, the five `forbid-skip` discipline scans.
  - Env: `CHECK` is one of `t-skip`, `not-implemented`,
    `soft-assert`, `should-skip`, `escape-hatch`.
  - Exit: `0` clean, `1` on any banned pattern or bad `CHECK`.
- **`clickhouse-version-sync.mjs`** — `ci.yml`, the `forbid-skip` job's
  ClickHouse version-consistency gate. Reads `versions.yaml` (the single
  source of truth) and asserts the docker-compose quickstart + compatibility
  image tags, the preflight floor, and the chDB substrate all match it, and
  that the quickstart is new enough for every optimization it enables (floors
  derived from `internal/chopt/registry.go`, not duplicated). See
  `docs/optimization-rules.md` (Rule 1, step 4).
  - Args: `--self-test` pins the parse / compare / drift-detection logic
    (run as a CI step before the gate); no args runs the gate over the tree.
  - Exit: `0` consistent (or self-test green), `1` on any drift.
- **`gremlins-threshold.mjs`** — `mutation.yml`, the
  `enforce efficacy threshold` step.
  - Env: `REPORT` (default `gremlins.json`), `THRESHOLD` (a number).
  - Exit: `0` when efficacy is `>=` threshold, `1` when below.
- **`release-preflight.mjs`** — `release.yml`, the `preflight` job. The
  GATE that refuses to publish a release unless the substantive lanes of
  `main` are green on the exact commit being tagged — every stable
  push-triggered lane (ci/check, lint, compatibility ×3, chdb, coverage,
  mutation/gremlins, perf-profile, property, probe, roundtrip ×3,
  compose-smoke, CodeQL, …), not just the PR-required subset. Reads the
  check-runs + commit statuses on `GITHUB_SHA` and fails on any
  non-`success`/`skipped`/`neutral` or still-pending lane. Re-runs are
  deduped by name (latest wins); the release run's own jobs are excluded;
  scheduled (nightly) re-runs are excluded (the merge-time push result is
  the truth). **Flaky UI COVERAGE lanes are also excluded** (`FLAKY_UI_LANE_RE`):
  the BFS `crawl` shards (`compose-smoke-shard-info (shard-crawl)`, the k3d
  `dashboard-shard (shard-crawl)`) and the whole informational `dashboard`
  k3d lane (`dashboard` / `dashboard-setup` / `dashboard-shard (…)`). These
  are coverage, not correctness gates (exploretraces "Failed to fetch",
  the app-init-race 400 = #115/#934), so a coverage flake must not block a
  release; the regex is anchored/specific (fail SAFE — only these lanes are
  dropped, the required `compose-smoke` still gates).
  - Env: `GITHUB_TOKEN`, `GITHUB_REPOSITORY`, `GITHUB_SHA`; optional
    `GITHUB_API_URL` (default `https://api.github.com`) and
    `RELEASE_SELF_JOBS` (default `preflight,goreleaser`).
  - Exit: `0` when every non-self, non-scheduled, non-flaky-UI check on the
    commit is green, `1` otherwise (with one `::error::` per red/pending lane).
- **`chart-publish.mjs`** — `release.yml`, the `chart-release` job. Three
  subcommands (argv[2]): `version-gate` compares the local Chart.yaml
  `version:` against the latest chart tag in the OCI registry and sets the
  `publish=true|false` + `version` step outputs (an app-only `v*` tag must NOT
  republish an unchanged chart); `push` runs `helm push` and parses the pushed
  `sha256:` digest out of helm's output into the `digest` + `ref` step outputs
  for the downstream cosign-sign / attest steps; `ah-metadata` idempotently
  pushes `artifacthub-repo.yml` as the special Artifact Hub OCI artifact via
  `oras`.
  - Env: `CHART_DIR` (default `deploy/helm/cerberus`), `OCI_REPO` (default
    `oci://ghcr.io/tsouza/cerberus/charts`), `CHART_NAME` (default `cerberus`),
    `CHART_TGZ` (push only), `GITHUB_OUTPUT` (runner-provided).
  - Exit: `0` on success (gate sets `publish` either way); `1` on a parse
    failure / `helm push` / `oras push` error, or when the version-gate cannot
    definitively determine existence (fails closed, with one `::error::`).
- **`chart-kubeconform.mjs`** — `chart-ci.yml`, the `Render + kubeconform`
  step. Renders the chart for the default values and every `ci/*-values.yaml`
  fixture, schema-validates each manifest set with `kubeconform -strict`, and
  probes the rendered container image tag against the registry (fails only on a
  DEFINITIVE not-found — the guard for an `appVersion` pointing at an
  unpublished tag).
  - Env: `CHART_DIR` (default `deploy/helm/cerberus`), `KUBE_VERSION` (default
    `1.28.0`), `SKIP_IMAGE_CHECK` (set `1` to skip the registry probe).
  - Exit: `0` when all fixtures validate + images present; `1` on any
    kubeconform failure or a missing image.
- **`compat-step-summary.mjs`** — `compatibility.yml`, the three
  `Append score to step summary` steps.
  - Env: `HEAD` (`prometheus`, `tempo`, or `loki`), `SCORE` (path to that
    head's `compat-score.json`).
  - Exit: always `0` (housekeeping; never gates).
- **`compat-ratchet.mjs`** — `compatibility.yml`, the three
  `Parity-regression ratchet` steps. The GATE that makes the required
  `compatibility/{prometheus,loki,tempo}` checks fail on a numeric parity
  regression (not just on infra breakage). Compares the run's
  `compat-score.json` against the committed floor in
  `compatibility/parity-baseline.json` and fails when `passed` or `total`
  drops below baseline. Integer comparison only, so it can't flake. Not
  an allow-list — pins the aggregate floor, never individual cases.
  - Env: `HEAD` (`prometheus`, `tempo`, or `loki`), `SCORE` (path to that
    head's `compat-score.json`), `BASELINE` (optional; default
    `compatibility/parity-baseline.json`).
  - Exit: `0` at or above baseline, `1` on a below-baseline regression or
    a missing/malformed score or baseline.
- **`resolve-bench-refs.mjs`** — `perf-benchmark.yml`, the
  `resolve baseline + ref SHAs` step.
  - Env: `INPUT_BASELINE_REF` (optional); writes `ref_sha`,
    `baseline_sha`, and `baseline_ref` to `$GITHUB_OUTPUT`.
  - Exit: `0` resolved, `1` on baseline `==` ref or a git error.
- **`chaos-run.mjs`** — `e2e.yml`, the `chaos` job (live-stack
  chaos-engineering lane, Layer 12). Fault-injects against the running
  k3d e2e stack (kubectl pod-kill / NetworkPolicy partition / slow-query
  timeout / concurrency burst) and asserts the gateway's resilience
  contracts (circuit breaker, per-query wall-clock timeout, admission
  control, replica resilience) hold under real faults. Phase-1 scenarios
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
  guard (run on the cheap `gate` lane) that pins the invariant + proves the
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
  `node --test` guard (run on the cheap `gate` lane) pinning the invariant +
  proving the detectors fire. k3d is heavy + flaky, so the shard count is kept
  deliberately small. Each emitted entry also carries a per-shard
  `timeoutMinutes`: the crawl shard gets a hard 30-min cap
  (`CRAWL_SHARD_TIMEOUT_MIN`; fail fast, release the k3d concurrency slot), the
  smoke shards keep their 75-min cluster-lifetime bound (`SMOKE_SHARD_TIMEOUT_MIN`).
  The whole `dashboard` lane is informational (never a PR gate), and the crawl
  shard is also excluded from the release preflight.
  - Env: `MODE` (`verify` | `emit`; also `argv[2]`; default `verify`),
    `PLAYWRIGHT_DIR` (default `test/e2e/playwright`), `INCLUDE_CRAWL` (emit:
    `"true"` adds the crawl shard — schedule/dispatch only), `GITHUB_OUTPUT`
    (emit: the runner file the
    `{include:[{name,specs,crawlStack,runGoE2E,timeoutMinutes}]}` matrix JSON is
    written to).
  - Exit: `0` clean / matrix emitted, `1` on any coverage violation or bad
    `MODE`.

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
