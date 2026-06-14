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

- **`forbid-skip.mjs`** — `ci.yml`, the six `forbid-skip` discipline scans.
  - Env: `CHECK` is one of `t-skip`, `wording-tests`, `not-implemented`,
    `soft-assert`, `should-skip`, `escape-hatch`.
  - Exit: `0` clean, `1` on any banned pattern or bad `CHECK`.
- **`gremlins-threshold.mjs`** — `mutation.yml`, the
  `enforce efficacy threshold` step.
  - Env: `REPORT` (default `gremlins.json`), `THRESHOLD` (a number).
  - Exit: `0` when efficacy is `>=` threshold, `1` when below.
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
  detectors fire.
  - Env: `MODE` (`verify` | `emit`; also `argv[2]`; default `verify`),
    `PLAYWRIGHT_DIR` (default `test/e2e/playwright`), `GITHUB_OUTPUT` (emit:
    the runner file the `{include:[{name,specs}]}` matrix JSON is written to).
  - Exit: `0` clean / matrix emitted, `1` on any coverage violation or bad
    `MODE`.

- **`dashboard-matrix.mjs`** — `e2e.yml`, the `dashboard-setup` job. The k3d
  twin of `compose-smoke-matrix.mjs`: single source of truth for how the
  `dashboard` (k3d) lane fans its Playwright spec set across a MODEST matrix
  of isolated-k3d-cluster shards. Two smoke shards (non-crawl specs,
  `CRAWL_STACK` unset → `crawl/**` ignored) run CONCURRENTLY with
  `CRAWL_SUB_SHARDS` (2) dedicated crawl shards (`CRAWL_STACK=k3d`,
  `SWEEP_DEPTH=full`), each carrying a distinct `CRAWL_SHARD_INDEX` /
  `CRAWL_SHARD_COUNT`. The crawl engine (`crawl/sharding.ts`) has every
  sub-shard run the WHOLE cheap BFS discovery but audit + pin only the ~1/N of
  surfaces it OWNS (the heavy interaction sweep), so the ~50min BFS long pole
  drops toward ~50/N. The crawl trio is REPLICATED across the crawl sub-shards
  (same specs, distinct index) — the coverage check treats that as one logical
  assignment, and a crawl spec leaking onto a smoke shard is a hard failure.
  The `SHARDS` partition (each carrying `specs` + `crawlStack` + `runGoE2E` +
  `crawlShardIndex`/`crawlShardCount`) + `EXCLUDED` list live in this module;
  specs are DISCOVERED (`git ls-files`) so a new `*.spec.ts` is a hard failure
  unless assigned or excluded — no silent no-run. Coverage assertion is
  collect-all-violations (unassigned, double-assigned, phantom/stale,
  bad-shard-name, the crawl-replication leak, and the "exactly one shard runs
  Go e2e" invariant), then `exit 1`. `dashboard-matrix.test.mjs` is the
  `node --test` guard (run on the cheap `gate` lane) pinning the invariant +
  proving the detectors fire. k3d is heavy + flaky, so the cluster count is
  kept deliberately small.
  - Env: `MODE` (`verify` | `emit`; also `argv[2]`; default `verify`),
    `PLAYWRIGHT_DIR` (default `test/e2e/playwright`), `GITHUB_OUTPUT` (emit:
    the runner file the
    `{include:[{name,specs,crawlStack,runGoE2E,crawlShardIndex,crawlShardCount}]}`
    matrix JSON + the `crawl_shard_count` output are written to).
  - Exit: `0` clean / matrix emitted, `1` on any coverage violation or bad
    `MODE`.

- **`crawl-inventory-merge.mjs`** — `e2e.yml`, the `dashboard-merge` job (only
  on the `update_crawl_inventory=true` dispatch). Unions the per-shard crawl
  surface SLICES the sharded BFS crawl emits (`grafana-surface-slice.<stack>.
  shard-<i>.json`) into ONE full inventory, asserting an EXACT, DISJOINT, TOTAL
  cover — a surface missing from every slice (discovery gap), a surface owned
  by two shards (non-disjoint), a missing shard index, or a slice smuggling in
  a surface the deterministic assignment doesn't map to it all `::error::` +
  `exit 1`. This is the coverage guarantee that keeps frontier-sharding from
  weakening the inventory ratchet. Mirrors
  `crawl/sharding.ts:mergeShardSlices` (same FNV-1a hash, path-only ownership
  key, disjoint-union rules) — the two are kept in lockstep, pinned by
  `crawl-inventory-merge.test.mjs` (run on the cheap `gate` lane).
  - Env: `MODE` (`merge` | `verify`; also `argv[2]`; default `merge`),
    `SLICES_DIR` (dir of downloaded slice JSONs), `STACK` (e.g. `k3d`),
    `INVENTORY_DOC` (the `doc` header for the merged inventory), `INVENTORY_OUT`
    (merge: the inventory path to write).
  - Exit: `0` clean / inventory written, `1` on any partition violation or bad
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
