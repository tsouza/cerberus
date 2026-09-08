// compose-smoke-matrix.mjs — single source of truth for how the
// `compose-smoke` release gate fans its Playwright spec set out across
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
//   IS_SCHEDULE     (emit) "true" on the nightly schedule — the lane then runs
//                   SWEEP_DEPTH=full, which selects the FULL per-shard timeout
//                   for the non-crawl shards (120 vs the 45 lean PR/push
//                   ceiling) AND the full-depth crawl cap (see lib/crawl-budget.mjs).
//   GITHUB_OUTPUT   (emit) runner file the matrix JSON is appended to.
//
// Exit: 0 clean / matrix emitted; 1 on any coverage violation or bad MODE.
//
// node: builtins only (via lib/gh.mjs) — no npm deps, no setup-node needed.

import { readFileSync } from 'node:fs';
import process from 'node:process';
import { error, notice, log, setOutput, appendStepSummary } from './lib/gh.mjs';
import { SHARD_NAME_RE, collectShardCoverageViolations, discoverSpecs } from './lib/shard-coverage.mjs';
import { crawlShardTimeoutMinutes } from './lib/crawl-budget.mjs';

// ---------------------------------------------------------------------------
// Per-shard wall-clock ceilings (timeout-minutes on the compose-smoke-shard
// job, interpolated as `matrix.timeoutMinutes`).
//
// The CRAWL shard's cap tracks the DEPTH it runs at, because the cap only does
// its job while it sits above the spec's own budget. crawl/crawl.spec.ts is a
// slow BFS COVERAGE lane (NOT a correctness gate — it is de-gated from the
// required `compose-smoke` aggregate, see GATE_EXCLUDED_SHARDS below) whose
// single indivisible BFS test() intermittently flakes (the app-init-race 400,
// #115/#934) and, on a hang, would otherwise ride to a 2h ceiling holding the
// `cancel-in-progress: false` concurrency slot. The cap makes that fail fast
// and release the slot — but a cap BELOW the spec budget stops being a
// fail-fast and becomes a guaranteed runner kill, which GitHub records as
// `cancelled`: no verdict, no evidence. A constant 30-min cap did exactly that
// to every nightly SWEEP_DEPTH=full crawl (#1861). lib/crawl-budget.mjs derives
// both caps from the spec budgets so the ordering holds by construction.
//
// NON-CRAWL shards keep their prior effective ceilings: the nightly schedule runs
// SWEEP_DEPTH=full (the heavier sweep) at 120, PR/push run SWEEP_DEPTH=lean at 45.
// Those shards are fast (≤~35s lean per spec) so they comfortably fit; the 45/120
// split is preserved verbatim from the old per-job `timeout-minutes` expression.
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
export const CRAWL_FRONTIER_SHARD_COUNT = 3;

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
  {
    name: 'shard-twophase',
    // The ONE shard that boots a docker-compose PROFILE. The Tempo structural
    // two-phase A/B needs a SECOND cerberus head with the split OFF (:8081)
    // plus a dense-descendant trace source, and both live behind the
    // `twophase` profile — so the shard that runs the A/B is the shard that
    // asks for them.
    //
    // Its own shard rather than a companion spec on an existing one: the
    // profile's extra head and its telemetrygen would otherwise ride along for
    // the WHOLE of a sibling shard's run, and shards are concurrent, so the
    // lane's wall-clock is unchanged by paying for one more runner instead.
    //
    // Until this shard existed the spec ran in NO lane at all (#3181): it was
    // named in BOTH planners' EXCLUDED lists, so the correctness argument for
    // a default-ON split — that two-phase is result-identical to the single
    // wide query — had never once been executed by CI.
    composeProfiles: 'twophase',
    specs: [
      'tempo_two_phase_compare.spec.ts', //     ~1-3min — polls telemetrygen, then one frozen-window A≡B
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
  'tempo_ux.spec.ts', //                 *_ux lane; dashboard-lane only
];

// discover() — the tracked compose-smoke spec universe (lib/shard-coverage.mjs).
export const discover = discoverSpecs;

// ---------------------------------------------------------------------------
// Compose-profile coverage — the SPEC partition's rule, applied to the STACK.
//
// A spec that no shard lists is caught above as an UNASSIGNED silent coverage
// gap. A compose PROFILE that no shard boots is the same defect one layer
// down, and nothing caught it: `tempo_two_phase_compare.spec.ts` sat in both
// planners' EXCLUDED lists for the whole life of the `twophase` profile, so
// the profile's two services — the split-OFF cerberus head the A/B compares
// against, and the trace source that feeds it — were declared, built, and
// never once started by CI (#3181).
//
// So the profile set is a cover too: every profile docker-compose.yml defines
// must be booted by exactly one shard, and every profile a shard names must
// exist in docker-compose.yml. A new profile with no lane is now a red check
// rather than a service nobody notices is dead.
// ---------------------------------------------------------------------------
export const COMPOSE_FILE = process.env.COMPOSE_FILE_PATH || 'docker-compose.yml';

// composeProfilesDefined() — the profile names docker-compose.yml declares.
//
// Deliberately a text scan rather than `docker compose config`: this rule runs
// on the cheap `node --test` lint lane, which has no Docker daemon, and the
// whole value of the rule is that it costs milliseconds on every PR instead of
// waiting for a stack boot. Both YAML sequence spellings are read — inline
// flow (`profiles: [a, b]`) and a block sequence of `- name` items — and a
// `profiles:` key whose value matches NEITHER shape throws, because a profile
// this cannot read is a profile it would silently report as covered.
export function composeProfilesDefined(composeFile = COMPOSE_FILE) {
  const lines = readFileSync(composeFile, 'utf8').split('\n');
  const found = new Set();
  for (let i = 0; i < lines.length; i += 1) {
    const m = /^(\s*)profiles:(.*)$/.exec(lines[i]);
    if (!m) continue;
    const [, indent, rest] = m;
    const inline = rest.trim();
    if (inline.startsWith('[')) {
      const close = inline.indexOf(']');
      if (close < 0) {
        throw new Error(`${composeFile}:${i + 1}: unterminated inline profiles list: ${inline}`);
      }
      for (const raw of inline.slice(1, close).split(',')) {
        const name = raw.trim().replace(/^["']|["']$/g, '');
        if (name) found.add(name);
      }
      continue;
    }
    if (inline === '') {
      // Block sequence: consume the more-indented `- name` items that follow.
      let consumed = 0;
      for (let j = i + 1; j < lines.length; j += 1) {
        const item = /^(\s*)-\s*(.+?)\s*$/.exec(lines[j]);
        if (!item || item[1].length <= indent.length) break;
        found.add(item[2].replace(/^["']|["']$/g, ''));
        consumed += 1;
      }
      if (consumed === 0) {
        throw new Error(`${composeFile}:${i + 1}: profiles: key with no readable list`);
      }
      i += consumed;
      continue;
    }
    throw new Error(`${composeFile}:${i + 1}: unreadable profiles value: ${inline}`);
  }
  return found;
}

// collectProfileViolations() — the profile cover, both directions.
export function collectProfileViolations(defined, shards = SHARDS) {
  const v = [];
  const bootedBy = new Map();
  for (const s of shards) {
    if (!s.composeProfiles) continue;
    for (const p of s.composeProfiles.split(',').map((x) => x.trim()).filter(Boolean)) {
      bootedBy.set(p, [...(bootedBy.get(p) || []), s.name]);
    }
  }
  for (const [profile, who] of bootedBy) {
    if (!defined.has(profile)) {
      v.push(
        `phantom compose profile (booted but not defined in ${COMPOSE_FILE}): ${profile} [shard ${who.join(', ')}]`,
      );
    }
    if (who.length > 1) {
      v.push(`double-booted compose profile ${profile} -> shards [${who.join(', ')}]`);
    }
  }
  for (const profile of defined) {
    if (!bootedBy.has(profile)) {
      v.push(
        `UNBOOTED compose profile (silent coverage gap): ${profile} — its services are defined in ` +
          `${COMPOSE_FILE} but no shard starts them, so nothing they exist for runs; give a shard ` +
          `\`composeProfiles: '${profile}'\` in SHARDS`,
      );
    }
  }
  return v;
}

// collectViolations() — returns a string[] of human-readable violations
// (empty == clean). The partition rules are shared with the dashboard lane so
// a new rule guards both; this lane adds the compose-profile cover, which has
// no k3d counterpart.
export function collectViolations(discovered, opts = {}) {
  const { violations } = collectShardCoverageViolations({
    discovered,
    shards: SHARDS,
    excluded: EXCLUDED,
    emptySubstrate: 'a compose stack',
  });
  return [...violations, ...collectProfileViolations(opts.profilesDefined ?? composeProfilesDefined())];
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

// shardSweepDepth() — the depth ONE shard sweeps on this event, and the single
// source of truth for it: e2e.yml reads it back as `matrix.sweepDepth` for the
// spec's own SWEEP_DEPTH, and shardTimeoutMinutes() below derives the job
// ceiling from the same answer.
//
// The single source is a fix, not a tidy-up. The depth and the ceiling used to
// be computed independently — the shard's SWEEP_DEPTH from a workflow
// expression that knew about the inventory-regen dispatch, the ceiling from
// `isSchedule` alone, which did not. A dispatch asking for a compose inventory
// regen therefore ran the crawl at FULL depth (crawl.spec.ts refuses to write
// the inventory at lean) under the LEAN 29-minute ceiling, and GitHub cancelled
// the job at 29m27s, before the spec's own 75-minute budget could report. The
// regen path was unusable, and it failed as a cancellation rather than an
// error, which reads like infrastructure noise instead of a bug.
export function shardSweepDepth(
  shardName,
  { isSchedule, regeneratesComposeInventory } = {},
) {
  if (isSchedule) return 'full';
  // Only the CRAWL shard goes full for a regen: it is the shard that writes
  // the inventory, and making the required shards pay a full sweep for it
  // would buy nothing.
  if (regeneratesComposeInventory && GATE_EXCLUDED_SHARDS.includes(shardName)) {
    return 'full';
  }
  return 'lean';
}

// shardTimeoutMinutes() — the per-shard `timeout-minutes` ceiling, derived from
// that shard's own sweep depth. The crawl cap comes from the spec's budget at
// that depth so the spec always times out first and reports a verdict
// (lib/crawl-budget.mjs); a job that outlives its spec turns a real crawl
// failure into an uninformative cancellation.
export function shardTimeoutMinutes(shardName, opts = {}) {
  const depth = shardSweepDepth(shardName, opts);
  if (GATE_EXCLUDED_SHARDS.includes(shardName)) return crawlShardTimeoutMinutes(depth);
  return depth === 'full'
    ? NONCRAWL_SHARD_TIMEOUT_FULL_MIN
    : NONCRAWL_SHARD_TIMEOUT_LEAN_MIN;
}

// shardEntry() — the strategy.matrix `include` row for a shard.
export const shardEntry = (s, opts) => ({
  name: s.name,
  specs: s.specs.join(' '),
  sweepDepth: shardSweepDepth(s.name, opts),
  timeoutMinutes: shardTimeoutMinutes(s.name, opts),
  // COMPOSE_PROFILES for this shard's stack — empty for every shard that wants
  // only the default services. e2e.yml sets it at JOB level so `pull`, `up` and
  // `down` all agree on which services the stack has; a profile named on `up`
  // alone would leave `down -v` orphaning the ones it started.
  composeProfiles: s.composeProfiles ?? '',
});

export const crawlShardEntries = (s, opts) =>
  Array.from({ length: opts.regeneratesComposeInventory ? 1 : CRAWL_FRONTIER_SHARD_COUNT }, (_, crawlShardIndex) => ({
    ...shardEntry(s, opts),
    name: `${s.name}-${crawlShardIndex}`,
    crawlShardIndex,
    crawlShardCount: opts.regeneratesComposeInventory ? 1 : CRAWL_FRONTIER_SHARD_COUNT,
  }));

const isGateExcluded = (name) => GATE_EXCLUDED_SHARDS.includes(name);

function emit() {
  const discovered = discover();
  assertCoverageOrExit(discovered);
  const isSchedule = process.env.IS_SCHEDULE === 'true';
  // Which inventory a workflow_dispatch asked to regenerate, forwarded raw
  // from the dispatch input (empty on every other event). The decision it
  // feeds lives here rather than in a workflow expression so that the depth
  // and the timeout cannot disagree again.
  const update = process.env.UPDATE_CRAWL_INVENTORY ?? '';
  const depthOpts = {
    isSchedule,
    regeneratesComposeInventory: update === 'compose' || update === 'both',
  };

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

  setOutput('matrix', JSON.stringify({ include: required.map((s) => shardEntry(s, depthOpts)) }));
  setOutput('matrix_informational', JSON.stringify({ include: informational.flatMap((s) => crawlShardEntries(s, depthOpts)) }));
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
export { SHARDS, EXCLUDED, SHARD_NAME_RE, GATE_EXCLUDED_SHARDS };
