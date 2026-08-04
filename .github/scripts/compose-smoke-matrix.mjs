// compose-smoke-matrix.mjs — single source of truth for how the
// `compose-smoke` required PR gate fans its Playwright spec set out across
// a balanced matrix of isolated-compose-stack shards (e2e.yml).
//
// Why this exists
// ---------------
// `compose-smoke` boots the full quickstart docker-compose stack (ClickHouse
// + collector + Grafana + cerberus) and drives Grafana through 10 Playwright
// spec files. Historically that was ONE runner running ONE `npx playwright
// test` over all 10 specs SERIALLY (playwright.config.ts pins `workers: 1`
// in CI). The three heaviest specs are each a SINGLE async `test()` that
// loops internally over every dashboard/panel/surface — indivisible by
// Playwright's native `--shard` (which splits at `test()` granularity, so a
// one-test spec stays whole). The only real parallelism is LOGICAL: split
// the spec FILES across N jobs, each booting its OWN isolated compose stack,
// balanced by wall-clock weight. That partition lives here.
//
// Two modes (env MODE, or argv[2]; default `verify`):
//   - verify : assert the SHARDS partition is a total, disjoint cover of the
//              compose-smoke spec cohort (discovered specs minus the explicit
//              EXCLUDED list). `::error::` + exit 1 on ANY drift — an
//              unassigned spec (the forbidden silent-coverage-gap), a
//              double-assigned spec, a phantom/stale entry, or a bad shard
//              name. This is the tripwire that makes "add a spec, forget to
//              shard it → red CI" hold. Runs ~50ms, boots no stack.
//   - emit   : run the same assertions, then write the GitHub `strategy.matrix`
//              JSON (`{include:[{name,specs}, …]}`) to $GITHUB_OUTPUT so the
//              `compose-smoke-shard` matrix job can interpolate each shard's
//              space-joined spec list straight into `npx playwright test`.
//              emit re-runs the assertions internally, so it can never ship a
//              matrix that silently drops a spec even if the verify step is
//              removed.
//
// Discovery (not a hardcoded canonical set) is deliberate: a newly-added
// `*.spec.ts` is IN the cohort by construction, forcing the author to either
// assign it to a shard or name it in EXCLUDED (visibly, in a reviewed diff).
// There is no third "I forgot" outcome that silently drops it.
//
// Env:
//   MODE            `emit` | `verify` (also argv[2]); default `verify`.
//   PLAYWRIGHT_DIR  glob root; default `test/e2e/playwright`.
//   IS_SCHEDULE     (emit) "true" on the nightly schedule — selects the FULL
//                   per-shard timeout for non-crawl shards (120 vs the 45 lean
//                   PR/push ceiling). The crawl shard's 30-min cap is constant.
//   GITHUB_OUTPUT   (emit) runner file the matrix JSON is appended to.
//
// Exit: 0 clean / matrix emitted; 1 on any coverage violation or bad MODE.
//
// node: builtins only (via lib/gh.mjs) — no npm deps, no setup-node needed.

import process from 'node:process';
import { error, notice, log, setOutput, appendStepSummary } from './lib/gh.mjs';
import { SHARD_NAME_RE, collectShardCoverageViolations, discoverSpecs } from './lib/shard-coverage.mjs';

// ---------------------------------------------------------------------------
// Per-shard wall-clock ceilings (timeout-minutes on the compose-smoke-shard
// job, interpolated as `matrix.timeoutMinutes`).
//
// The CRAWL shard gets a HARD 30-min cap regardless of event. crawl/crawl.spec.ts
// is a slow BFS COVERAGE lane (NOT a correctness gate — it is de-gated from the
// required `compose-smoke` aggregate, see GATE_EXCLUDED_SHARDS below) whose single
// indivisible BFS test() intermittently flakes (the app-init-race 400, #115/#934)
// and, on a hang, runs out to its long job timeout holding the
// `cancel-in-progress: false` concurrency slot for ~2h. A 30-min cap makes it FAIL
// FAST and release the slot instead. The PR/push lean crawl's internal budget is
// 14min (testInfo.setTimeout in crawl.spec.ts), so 30min is comfortable headroom
// for a healthy run; a hung/flaking run is cut at 30 instead of riding to 120.
//
// NON-CRAWL shards keep their prior effective ceilings: the nightly schedule runs
// SWEEP_DEPTH=full (the heavier sweep) at 120, PR/push run SWEEP_DEPTH=lean at 45.
// Those shards are fast (≤~35s lean per spec) so they comfortably fit; the 45/120
// split is preserved verbatim from the old per-job `timeout-minutes` expression.
const CRAWL_SHARD_TIMEOUT_MIN = 30;
const NONCRAWL_SHARD_TIMEOUT_FULL_MIN = 120;
const NONCRAWL_SHARD_TIMEOUT_LEAN_MIN = 45;

// Shards EXCLUDED from the required `compose-smoke` aggregate roll-up. The crawl
// shard still RUNS and reports its own `compose-smoke-shard (shard-crawl)` check
// (visible, never masked with continue-on-error), but its pass/fail does NOT fail
// the required `compose-smoke` status. Rationale: the crawl is slow BFS COVERAGE,
// not a correctness gate; it is slow/flaky by nature (~6min compose / ~50min k3d,
// app-init-race 400 = #115/#934), and a coverage flake must not block every PR.
// The required gate keeps the real correctness shards (smoke, kiosk). The
// `compose-smoke` aggregator reads this list (emitted as `gate_excluded`) to
// decide which shard outcomes gate it. Emitted from a single source of truth so
// the de-gate can't drift from the partition.
const GATE_EXCLUDED_SHARDS = ['shard-crawl'];

// ---------------------------------------------------------------------------
// The wall-clock-balanced partition of the compose-smoke spec set.
//
// Balanced from REAL lean per-spec timings (main run 27495030583,
// SWEEP_DEPTH=lean, --reporter=list). Each shard is anchored by one of the
// three independent ~2min heavies (kiosk / smoke / crawl-BFS), with the
// light specs distributed to flatten the tail so the three playwright phases
// land within ~24s of each other (≈185 / 206 / 181s).
//
// shard-crawl deliberately carries the FEWEST companion weight, because
// crawl/crawl.spec.ts is the ~50min long pole at SWEEP_DEPTH=full (its single
// BFS test() cannot be split) and must run near-alone so the full nightly
// lane's wall-clock is ≈ max(crawl shard) rather than sum-behind-crawl.
//
// Spec paths are relative to PLAYWRIGHT_DIR — exactly how they're passed to
// `npx playwright test <files>` — so the matrix entry's space-joined string
// drops verbatim into the run step.
// ---------------------------------------------------------------------------
const SHARDS = [
  {
    name: 'shard-kiosk',
    specs: [
      'iterate-panel-kiosk.spec.ts', //        144.0s lean — heavy anchor
      'iterate-all-dashboards.spec.ts', //      20.6s
      'crawl/dsquery.spec.ts', //               20.5s
      'loki_explore_columns.spec.ts', //         1.6s — lightest companion, lands on the lean shard
      'loki_tail.spec.ts', //                   ~5s — direct-WS tail no-loss/no-dup (#1011 oracle)
    ],
  },
  {
    name: 'shard-smoke',
    specs: [
      'compose_grafana_smoke.spec.ts', //      114.1s lean — heavy anchor
      'iterate-filter-drill.spec.ts', //        33.2s
      'iterate-histogram-completeness.spec.ts', // 30.7s
      'iterate-metrics-explorer.spec.ts', //    27.7s
      'metrics_histogram.spec.ts', //           ~5s happy path — direct cerberus API, polls the telemetrygen-fed exp + classic histogram quantiles
    ],
  },
  {
    name: 'shard-crawl',
    specs: [
      'crawl/crawl.spec.ts', //                120.1s lean / ~50min full — long pole, runs near-alone
      'iterate-panel-shape.spec.ts', //         30.9s
      'crawl/lints.spec.ts', //                 30.5s
    ],
  },
];

// ---------------------------------------------------------------------------
// Specs that live under PLAYWRIGHT_DIR but are NOT part of compose-smoke.
// Every one must be named here (with the reason) or `verify` fails on it as
// an UNASSIGNED spec. The k3d `dashboard` job runs `npx playwright test`
// UNFILTERED (= all specs) + its own crawl trio; these specs belong to that
// lane (or are helper self-tests), not the compose-smoke cohort.
// ---------------------------------------------------------------------------
const EXCLUDED = [
  'crawl/reconcile.spec.ts', //          crawl-suite reconcile pin; not in the compose-smoke crawl trio
  'cross_datasource.spec.ts', //         dashboard(k3d)-lane / unfiltered-discovery only
  'datasource_health.spec.ts', //        dashboard-lane only
  'expectation-contracts.spec.ts', //    contract unit-spec; dashboard-lane / unfiltered only
  'helpers.spec.ts', //                  helper self-test; not compose-smoke
  'helpers-validity.spec.ts', //         helper self-test; not compose-smoke
  'helpers-variables.spec.ts', //        helper self-test; not compose-smoke
  'iterate-drilldown-apps.spec.ts', //   dashboard-lane sweep; not in compose-smoke's iterate set
  'iterate-time-ranges.spec.ts', //      phase-5 matrix sweep; explicitly excluded from compose-smoke
  'loki_logs.spec.ts', //                head-specific flow; dashboard-lane only
  'loki_ux.spec.ts', //                  *_ux lane; dashboard-lane only
  'prom_explore_flow.spec.ts', //        head-specific flow; dashboard-lane only
  'prom_metrics.spec.ts', //             head-specific flow; dashboard-lane only
  'prom_ux.spec.ts', //                  *_ux lane; dashboard-lane only
  'service_graph.spec.ts', //            head-specific flow; dashboard-lane only
  'smoke.spec.ts', //                    legacy single-smoke; dashboard-lane only
  'split_isolation.spec.ts', //          split-mode head-isolation; needs k3d per-head deployments, not the single-container compose stack; dashboard-lane only
  'tempo_grpc_streaming.spec.ts', //     gRPC/h2c StreamingQuerier trigger (#1454); real Explore + Live round-trip, dashboard-lane only
  'tempo_search_flow.spec.ts', //        head-specific flow; dashboard-lane only
  'tempo_traces.spec.ts', //             head-specific flow; dashboard-lane only
  'tempo_traces_drilldown.spec.ts', //   head-specific flow; dashboard-lane only
  'tempo_two_phase_compare.spec.ts', //  opt-in structural two-phase A/B; needs `docker compose --profile twophase up` (cerberus-nosplit + telemetrygen-traces) + CERBERUS_NOSPLIT_URL, skipped otherwise — not a compose-smoke shard
  'tempo_ux.spec.ts', //                 *_ux lane; dashboard-lane only
];

// discover() — the tracked compose-smoke spec universe (lib/shard-coverage.mjs).
export const discover = discoverSpecs;

// collectViolations() — returns a string[] of human-readable violations
// (empty == clean). The partition rules are shared with the dashboard lane so
// a new rule guards both; this lane adds none of its own.
export function collectViolations(discovered) {
  const { violations } = collectShardCoverageViolations({
    discovered,
    shards: SHARDS,
    excluded: EXCLUDED,
    emptySubstrate: 'a compose stack',
  });
  return violations;
}

function assertCoverageOrExit(discovered) {
  const v = collectViolations(discovered);
  if (v.length === 0) return;
  for (const m of v) {
    error(`compose-smoke-matrix: ${m}`, { title: 'compose-smoke shard coverage violation' });
  }
  error(
    `compose-smoke-matrix: ${v.length} coverage violation(s); fix SHARDS / EXCLUDED in .github/scripts/compose-smoke-matrix.mjs`,
  );
  process.exit(1);
}

function verify() {
  const discovered = discover();
  assertCoverageOrExit(discovered);
  const assignedCount = SHARDS.reduce((n, s) => n + s.specs.length, 0);
  notice(
    `compose-smoke-matrix OK: ${SHARDS.length} shards, ${assignedCount} specs assigned, ` +
      `${EXCLUDED.length} excluded, ${discovered.length} discovered.`,
  );
  process.exit(0);
}

// shardTimeoutMinutes() — the per-shard `timeout-minutes` ceiling. The crawl
// shard is a constant 30-min hard cap (fail fast, release the concurrency slot);
// non-crawl shards take the full (nightly) or lean (PR/push) ceiling.
export function shardTimeoutMinutes(shardName, { isSchedule } = {}) {
  if (GATE_EXCLUDED_SHARDS.includes(shardName)) return CRAWL_SHARD_TIMEOUT_MIN;
  return isSchedule ? NONCRAWL_SHARD_TIMEOUT_FULL_MIN : NONCRAWL_SHARD_TIMEOUT_LEAN_MIN;
}

// shardEntry() — the strategy.matrix `include` row for a shard.
const shardEntry = (s, isSchedule) => ({
  name: s.name,
  specs: s.specs.join(' '),
  timeoutMinutes: shardTimeoutMinutes(s.name, { isSchedule }),
});

const isGateExcluded = (name) => GATE_EXCLUDED_SHARDS.includes(name);

function emit() {
  const discovered = discover();
  assertCoverageOrExit(discovered);
  const isSchedule = process.env.IS_SCHEDULE === 'true';

  // The partition is split into TWO matrices so the de-gate is structural, not
  // a fragile after-the-fact result filter. A GitHub matrix exposes only ONE
  // rolled-up `.result` to dependents (success iff EVERY child succeeded), so a
  // single matrix can't let one shard fail without failing the whole roll-up.
  // Splitting at the source — both matrices derived from the SAME SHARDS +
  // GATE_EXCLUDED_SHARDS list, so they can't drift — lets the required
  // `compose-smoke` aggregator `needs` only the REQUIRED matrix while the crawl
  // shard runs in its own informational matrix and reports its own child check
  // (`compose-smoke-shard-info (shard-crawl)`), visible and unmasked.
  const required = SHARDS.filter((s) => !isGateExcluded(s.name));
  const informational = SHARDS.filter((s) => isGateExcluded(s.name));

  setOutput('matrix', JSON.stringify({ include: required.map((s) => shardEntry(s, isSchedule)) }));
  setOutput('matrix_informational', JSON.stringify({ include: informational.map((s) => shardEntry(s, isSchedule)) }));
  setOutput('has_informational', informational.length > 0 ? 'true' : 'false');
  setOutput('shard_names', JSON.stringify(SHARDS.map((s) => s.name)));
  setOutput('gate_excluded', JSON.stringify(GATE_EXCLUDED_SHARDS));
  appendStepSummary(
    [
      '### compose-smoke shard matrix',
      '',
      '| shard | specs | timeout (min) | gates required check |',
      '| --- | --- | --- | --- |',
      ...SHARDS.map(
        (s) =>
          `| \`${s.name}\` | ${s.specs.length} | ${shardTimeoutMinutes(s.name, { isSchedule })} | ${isGateExcluded(s.name) ? 'no (coverage)' : 'yes'} |`,
      ),
    ].join('\n'),
  );
  log(
    `compose-smoke-matrix: emitted ${required.length} required + ${informational.length} informational shard(s).`,
  );
  process.exit(0);
}

// Only dispatch when run as a script — importing for the unit test must not
// exit the test runner.
const invokedDirectly = process.argv[1] && import.meta.url === new URL(`file://${process.argv[1]}`).href;
if (invokedDirectly) {
  const mode = (process.env.MODE || process.argv[2] || 'verify').toLowerCase();
  if (mode === 'emit') emit();
  else if (mode === 'verify') verify();
  else {
    error(`compose-smoke-matrix: unknown MODE "${mode}" (want emit|verify)`);
    process.exit(1);
  }
}

// Exported for the unit guard (.github/scripts/compose-smoke-matrix.test.mjs).
export { SHARDS, EXCLUDED, SHARD_NAME_RE, GATE_EXCLUDED_SHARDS, CRAWL_SHARD_TIMEOUT_MIN };
