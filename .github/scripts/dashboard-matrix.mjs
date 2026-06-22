// dashboard-matrix.mjs — single source of truth for how the `dashboard`
// (k3d) e2e lane fans its Playwright spec set out across a balanced matrix of
// isolated-k3d-cluster shards (e2e.yml).
//
// Why this exists
// ---------------
// The `dashboard` job boots ONE k3d cluster (cerberus + ClickHouse + the OTel
// pipeline + Grafana), seeds rolling OTel data, runs the Go e2e tests, then
// drives Grafana through the Playwright suite. Historically that was ONE
// runner: an UNFILTERED `npx playwright test` (auto-discovering every non-crawl
// `*.spec.ts`) run SERIALLY (playwright.config.ts pins `workers: 1` in CI)
// against the single k3d Grafana, followed — on schedule/dispatch only — by the
// k3d crawl trio (crawl/crawl.spec.ts + dsquery + lints) at SWEEP_DEPTH=full.
//
// The dominant cost is crawl/crawl.spec.ts: a SINGLE async BFS `test()` that
// walks every reachable Grafana surface — the ~50min long pole at
// SWEEP_DEPTH=full. It is indivisible by Playwright's native `--shard` (which
// splits at `test()` granularity, so a one-test spec stays whole) AND by
// spec-file sharding (it is one file, one test). The only parallelism this PR
// buys is LOGICAL and COARSE: split the non-crawl smoke specs across N k3d
// clusters running concurrently, and put the crawl trio on its OWN dedicated
// k3d cluster so the ~50min crawl runs CONCURRENTLY with the smoke shards
// instead of serially after them. Wall-clock drops from
// (smoke serial + crawl) toward max(crawl ~50min, slowest smoke shard).
//
// What this manifest does NOT do (the follow-up): it does not split the crawl
// BFS frontier itself. Beating the ~50min floor needs the crawl engine to
// PARTITION its discovered frontier across shards (a CRAWL_SHARD_INDEX /
// CRAWL_SHARD_COUNT contract that deterministically assigns each discovered
// surface to a shard, with each shard emitting its visited slice and a final
// job merging + asserting the union against the pinned inventory). That is a
// deep change to the 1383-line BFS + the inventory ratchet (lib.ts diffInventory
// asserts the WHOLE visited set) and is blocked today by the k3d inventory being
// unbootstrapped (grafana-surface-inventory.k3d.json carries surfaces: []), so
// the union couldn't be validated. It is tracked as the explicit next step in
// the PR body. This manifest is the safe, coarse win that lands first.
//
// k3d cost/flake trade-off: a k3d cluster is heavy (~3-5min bring-up) and flaky
// (telemetrygen / otel-collector-gateway readiness BackOff). A matrix of N
// clusters multiplies BOTH cost and flake surface, so the shard count is kept
// deliberately MODEST (3): two smoke shards + one crawl shard.
//
// Two modes (env MODE, or argv[2]; default `verify`):
//   - verify : assert the SHARDS partition is a total, disjoint cover of the
//              dashboard-lane spec cohort (discovered specs minus the explicit
//              EXCLUDED list). `::error::` + exit 1 on ANY drift — an
//              unassigned spec (the forbidden silent-coverage-gap), a
//              double-assigned spec, a phantom/stale entry, or a bad shard
//              name. This is the tripwire that makes "add a spec, forget to
//              shard it → red CI" hold. Runs ~50ms, boots no cluster.
//   - emit   : run the same assertions, then write the GitHub `strategy.matrix`
//              JSON (`{include:[{name,specs,crawlStack,runGoE2E}, …]}`) to
//              $GITHUB_OUTPUT so the `dashboard-shard` matrix job can
//              interpolate each shard's space-joined spec list, its CRAWL_STACK
//              value, and whether it runs the Go e2e suite. emit re-runs the
//              assertions internally, so it can never ship a matrix that
//              silently drops a spec even if the verify step is removed.
//
// Discovery (not a hardcoded canonical set) is deliberate: a newly-added
// `*.spec.ts` is IN the cohort by construction, forcing the author to either
// assign it to a shard or name it in EXCLUDED (visibly, in a reviewed diff).
// There is no third "I forgot" outcome that silently drops it.
//
// Env:
//   MODE            `emit` | `verify` (also argv[2]); default `verify`.
//   PLAYWRIGHT_DIR  glob root; default `test/e2e/playwright`.
//   GITHUB_OUTPUT   (emit) runner file the matrix JSON is appended to.
//
// Exit: 0 clean / matrix emitted; 1 on any coverage violation or bad MODE.
//
// node: builtins only (via lib/gh.mjs) — no npm deps, no setup-node needed.

import process from 'node:process';
import { error, notice, log, lsFiles, setOutput, appendStepSummary } from './lib/gh.mjs';

const PW_DIR = process.env.PLAYWRIGHT_DIR || 'test/e2e/playwright';

// CRAWL_STACK value that selects the k3d crawl-suite config (crawl/stacks.ts).
// A smoke shard leaves it EMPTY so playwright.config.ts ignores crawl/**
// (matching the dashboard job's old unfiltered "Run Playwright smoke" step);
// the crawl shard sets it to `k3d` so the crawl trio runs against the k3d
// inventory at SWEEP_DEPTH=full (the old "Run Playwright crawl" step).
const CRAWL_STACK_K3D = 'k3d';
const CRAWL_STACK_NONE = '';

// Chart deployment topologies the smoke lane exercises (E2E_MODE in `just
// e2e-up`). The SAME Grafana/Playwright smoke runs against both: `monolith`
// (one process serving all three heads) and `split` (three isolated per-head
// Deployments + bare-named Services). Each smoke shard fans out into one
// matrix entry per mode; the crawl shard runs monolith-only (it is the ~50min
// long pole and is mode-agnostic COVERAGE, not a topology assertion).
const MODE_MONOLITH = 'monolith';
const MODE_SPLIT = 'split';
const SMOKE_MODES = [MODE_MONOLITH, MODE_SPLIT];

// Specs that are MEANINGFUL ONLY in split mode and must be filtered OUT of the
// monolith matrix entries. They stay ASSIGNED to a shard in SHARDS (so the
// coverage gate counts them), but the emit-time cross-product drops them from
// every monolith entry. split_isolation.spec.ts scales one head to zero and
// asserts the other two keep serving — a monolith (one process) cannot pass
// it, and the spec hard-fails if run with CERBERUS_MODE != split.
const SPLIT_ONLY_SPECS = new Set(['split_isolation.spec.ts']);

// Per-shard wall-clock ceilings (timeout-minutes on the dashboard-shard job,
// interpolated as `matrix.timeoutMinutes`).
//
// The CRAWL shard gets a HARD 30-min cap. The k3d crawl is a slow BFS COVERAGE
// lane (not a correctness gate — the whole `dashboard` lane is informational,
// never a PR gate); on a hang it rode the job to its long timeout holding the
// `cancel-in-progress: false` k3d concurrency slot. A 30-min cap makes it FAIL
// FAST and release the slot. The smoke shards keep their prior effective 75-min
// job ceiling (k3d bring-up ~3-5min + the non-crawl smoke specs fit comfortably);
// the value is preserved verbatim from the old single per-job `timeout-minutes`.
const CRAWL_SHARD_TIMEOUT_MIN = 30;
const SMOKE_SHARD_TIMEOUT_MIN = 75;

// ---------------------------------------------------------------------------
// The partition of the dashboard-lane spec set across isolated k3d clusters.
//
// Three shards, each its own k3d cluster:
//   - shard-smoke-a / shard-smoke-b — the 27 non-crawl specs, split by
//     wall-clock weight (the three internally-looping heavies —
//     iterate-panel-kiosk, compose_grafana_smoke, the iterate-* sweeps — are
//     spread across the two so neither shard carries all the long poles).
//     CRAWL_STACK is empty → crawl/** is ignored, exactly as the old
//     unfiltered smoke step ran. Go e2e runs ONCE, on shard-smoke-a, so the
//     Go suite isn't redundantly re-run on every cluster.
//   - shard-crawl — the crawl trio ONLY, CRAWL_STACK=k3d, SWEEP_DEPTH=full.
//     The ~50min BFS long pole runs ALONE on its own cluster, concurrently
//     with the smoke shards. It carries no companion specs (its single BFS
//     test() already saturates the shard's wall-clock budget).
//
// Spec paths are relative to PLAYWRIGHT_DIR — exactly how they're passed to
// `npx playwright test <files>` — so the matrix entry's space-joined string
// drops verbatim into the run step.
// ---------------------------------------------------------------------------
const SHARDS = [
  {
    name: 'shard-smoke-a',
    crawlStack: CRAWL_STACK_NONE,
    // Go e2e tests (`just e2e-run`) run once across the matrix, here — they're
    // stack-health Go tests, not Playwright, and don't need re-running per
    // cluster. The crawl + the other smoke shard skip them.
    runGoE2E: true,
    specs: [
      'iterate-panel-kiosk.spec.ts', //          heavy internally-looping anchor
      'compose_grafana_smoke.spec.ts', //        heavy catch-net anchor
      'iterate-all-dashboards.spec.ts',
      'iterate-metrics-explorer.spec.ts',
      'iterate-histogram-completeness.spec.ts',
      'cross_datasource.spec.ts',
      'datasource_health.spec.ts',
      'expectation-contracts.spec.ts',
      'loki_logs.spec.ts',
      'loki_ux.spec.ts',
      'loki_explore_columns.spec.ts',
      'prom_explore_flow.spec.ts',
      'prom_metrics.spec.ts',
      'prom_ux.spec.ts',
    ],
  },
  {
    name: 'shard-smoke-b',
    crawlStack: CRAWL_STACK_NONE,
    runGoE2E: false,
    specs: [
      'iterate-time-ranges.spec.ts', //          phase-5 matrix sweep — heavy anchor
      'iterate-filter-drill.spec.ts',
      'iterate-panel-shape.spec.ts',
      'iterate-drilldown-apps.spec.ts',
      'service_graph.spec.ts',
      'smoke.spec.ts',
      'tempo_search_flow.spec.ts',
      'tempo_traces.spec.ts',
      'tempo_traces_drilldown.spec.ts',
      'tempo_ux.spec.ts',
      'helpers.spec.ts',
      'helpers-validity.spec.ts',
      'helpers-variables.spec.ts',
      // split-only: runs ONLY on the split leg (filtered out of monolith
      // entries at emit time — see SPLIT_ONLY_SPECS / buildMatrix). It is
      // assigned HERE so the coverage gate counts it as covered; the spec
      // itself hard-fails if run outside split mode (CERBERUS_MODE != split).
      'split_isolation.spec.ts',
    ],
  },
  {
    name: 'shard-crawl',
    crawlStack: CRAWL_STACK_K3D,
    runGoE2E: false,
    specs: [
      'crawl/crawl.spec.ts', //                  ~50min full BFS long pole — runs ALONE
      'crawl/dsquery.spec.ts',
      'crawl/lints.spec.ts',
    ],
  },
];

// ---------------------------------------------------------------------------
// Specs that live under PLAYWRIGHT_DIR but are NOT part of the dashboard
// (k3d) lane. Every one must be named here (with the reason) or `verify`
// fails on it as an UNASSIGNED spec. The old dashboard job ran the 27
// non-crawl specs (unfiltered) + the crawl TRIO (crawl/dsquery + lints +
// the BFS) — crawl/reconcile.spec.ts was never invoked by this lane.
// ---------------------------------------------------------------------------
const EXCLUDED = [
  'crawl/reconcile.spec.ts', //          crawl-suite reconcile pin; the k3d lane runs only the crawl trio, never reconcile
  'loki_tail.spec.ts', //                direct-WebSocket Loki live-tail oracle (#1011); runs in the compose-smoke lane (shard-kiosk), not the k3d dashboard lane — it drives the tail WS endpoint on the compose stack, never Grafana on k3d
];

// Shard names render straight into the matrix `name` -> child check context
// `dashboard-shard (shard-x)` + the per-shard artifact name. Keep them
// filename-safe.
const SHARD_NAME_RE = /^[a-z0-9-]+$/;

const stripDir = (p) => p.replace(new RegExp(`^${PW_DIR}/`), '');

// discover() — the tracked dashboard-lane spec universe. Two explicit globs
// (the dir root + the crawl/ subdir) rather than `**` so a future deeper
// subdir is itself a reviewable event, not silently vacuumed into scope.
export function discover() {
  const paths = lsFiles([`${PW_DIR}/*.spec.ts`, `${PW_DIR}/crawl/*.spec.ts`]);
  return paths.map(stripDir);
}

// collectViolations() — returns a string[] of human-readable violations
// (empty == clean). Collect-then-fail (not fail-fast) so a maintainer
// reworking the partition sees every problem in one run.
export function collectViolations(discovered) {
  const v = [];
  const dset = new Set(discovered);
  if (dset.size !== discovered.length) {
    v.push('discovery returned duplicate paths');
  }

  // Shard hygiene + build the spec -> [owning shard…] map.
  const owners = new Map();
  const names = new Set();
  let goE2ECount = 0;
  for (const s of SHARDS) {
    if (!SHARD_NAME_RE.test(s.name)) {
      v.push(`bad shard name "${s.name}" (must match ${SHARD_NAME_RE})`);
    }
    if (names.has(s.name)) {
      v.push(`duplicate shard name: ${s.name}`);
    }
    names.add(s.name);
    if (s.runGoE2E) goE2ECount += 1;
    if (!s.specs || s.specs.length === 0) {
      v.push(`empty shard (would boot a k3d cluster to run nothing): ${s.name}`);
      continue;
    }
    for (const spec of s.specs) {
      owners.set(spec, [...(owners.get(spec) || []), s.name]);
    }
  }

  // Exactly one shard runs the Go e2e suite — zero would drop Go e2e coverage
  // from the lane silently; two would redundantly re-run it on two clusters.
  if (goE2ECount !== 1) {
    v.push(`exactly one shard must set runGoE2E:true (the Go e2e suite runs once across the matrix); found ${goE2ECount}`);
  }

  const assigned = new Set(owners.keys());
  const excluded = new Set(EXCLUDED);

  if (excluded.size !== EXCLUDED.length) {
    v.push('EXCLUDED contains duplicate entries');
  }

  // double-assignment (wasted work + a spec running on two clusters).
  for (const [spec, who] of owners) {
    if (who.length > 1) {
      v.push(`double-assigned spec ${spec} -> shards [${who.join(', ')}]`);
    }
  }
  // assigned AND excluded — a contradiction.
  for (const spec of assigned) {
    if (excluded.has(spec)) {
      v.push(`spec is both assigned and excluded: ${spec}`);
    }
  }
  // stale exclude: names a spec that no longer exists (rename/delete) — it
  // could be masking a coverage hole, so it's a hard error not a warning.
  for (const spec of excluded) {
    if (!dset.has(spec)) {
      v.push(`excluded spec not found on disk (stale exclude / rename?): ${spec}`);
    }
  }
  // phantom assignment: a shard lists a spec that isn't on disk.
  for (const spec of assigned) {
    if (!dset.has(spec)) {
      v.push(`phantom spec (assigned but not on disk): ${spec} [shard ${owners.get(spec).join(', ')}]`);
    }
  }
  // THE coverage gap: discovered, neither assigned nor excluded.
  for (const spec of discovered) {
    if (!assigned.has(spec) && !excluded.has(spec)) {
      v.push(
        `UNASSIGNED spec (silent coverage gap): ${spec} — assign it to a shard in SHARDS or add it to EXCLUDED with a reason`,
      );
    }
  }
  return v;
}

// shardTimeoutMinutes() — the per-shard `timeout-minutes` ceiling. The crawl
// shard (CRAWL_STACK=k3d) is a constant 30-min hard cap (fail fast, release the
// k3d concurrency slot); the smoke shards keep the prior effective 75-min job
// ceiling.
export function shardTimeoutMinutes(shard) {
  return shard.crawlStack === CRAWL_STACK_K3D ? CRAWL_SHARD_TIMEOUT_MIN : SMOKE_SHARD_TIMEOUT_MIN;
}

function assertCoverageOrExit(discovered) {
  const v = collectViolations(discovered);
  if (v.length === 0) return;
  for (const m of v) {
    error(`dashboard-matrix: ${m}`, { title: 'dashboard shard coverage violation' });
  }
  error(
    `dashboard-matrix: ${v.length} coverage violation(s); fix SHARDS / EXCLUDED in .github/scripts/dashboard-matrix.mjs`,
  );
  process.exit(1);
}

function verify() {
  const discovered = discover();
  assertCoverageOrExit(discovered);
  const assignedCount = SHARDS.reduce((n, s) => n + s.specs.length, 0);
  notice(
    `dashboard-matrix OK: ${SHARDS.length} shards, ${assignedCount} specs assigned, ` +
      `${EXCLUDED.length} excluded, ${discovered.length} discovered.`,
  );
  process.exit(0);
}

// buildMatrix() — the emit-time cross-product of shards × deployment modes.
// Pure (no I/O, no process.exit) so the unit guard can call it directly.
//
// - SMOKE shards (CRAWL_STACK unset) fan out into one entry per SMOKE_MODES
//   value (monolith + split): the SAME smoke runs against both topologies.
//   SPLIT_ONLY_SPECS are stripped from the monolith entries (they stay assigned
//   in SHARDS for the coverage gate; here they only actually RUN on split).
//   A monolith entry that ends up with zero specs after the filter is dropped
//   (it would boot a cluster to run nothing) — today no smoke shard is all
//   split-only, so every smoke shard yields both legs.
// - The CRAWL shard runs ONCE, monolith only: it is the ~50min long pole and is
//   mode-agnostic coverage, not a topology assertion. It is included only when
//   includeCrawl is true (schedule + manual dispatch), matching the prior
//   smoke-only-on-PR/push behaviour.
// - runGoE2E is carried only on the MONOLITH leg of the shard that declares it
//   in SHARDS, so exactly one emitted entry runs the Go e2e suite.
//
// Each entry's `name` encodes the mode (e.g. shard-smoke-a-monolith) so the
// matrix keys, concurrency group, and artifact names stay unique.
export function buildMatrix(includeCrawl) {
  const include = [];
  for (const s of SHARDS) {
    if (s.crawlStack === CRAWL_STACK_K3D) {
      // Crawl: monolith-only, dispatched only when crawl is included.
      if (!includeCrawl) continue;
      include.push({
        name: `${s.name}-${MODE_MONOLITH}`,
        mode: MODE_MONOLITH,
        specs: s.specs.join(' '),
        crawlStack: s.crawlStack,
        runGoE2E: s.runGoE2E,
        timeoutMinutes: shardTimeoutMinutes(s),
      });
      continue;
    }
    // Smoke shard: one entry per mode.
    for (const mode of SMOKE_MODES) {
      const specs =
        mode === MODE_SPLIT ? s.specs : s.specs.filter((spec) => !SPLIT_ONLY_SPECS.has(spec));
      if (specs.length === 0) continue; // nothing to run in this mode → no cluster
      include.push({
        name: `${s.name}-${mode}`,
        mode,
        specs: specs.join(' '),
        crawlStack: s.crawlStack,
        // Go e2e runs once: only on the monolith leg of the runGoE2E shard.
        runGoE2E: s.runGoE2E && mode === MODE_MONOLITH,
        timeoutMinutes: shardTimeoutMinutes(s),
      });
    }
  }
  return include;
}

function emit() {
  const discovered = discover();
  assertCoverageOrExit(discovered);
  // The crawl shard (CRAWL_STACK=k3d) is the ~50min full-depth long pole, so it
  // is dispatched only on schedule + manual dispatch (INCLUDE_CRAWL=true). On
  // pull_request + push the matrix is smoke-only: the parallel smoke shards run
  // (fast, testable per-change) with no all-skipped crawl leg. Coverage is
  // unaffected — every spec is still ASSIGNED to a shard (asserted above); the
  // crawl shard is simply not dispatched on those events.
  const includeCrawl = process.env.INCLUDE_CRAWL === 'true';
  const include = buildMatrix(includeCrawl);
  setOutput('matrix', JSON.stringify({ include }));
  setOutput('shard_names', JSON.stringify(include.map((e) => e.name)));
  appendStepSummary(
    [
      '### dashboard (k3d) shard matrix',
      '',
      '| shard | mode | specs | CRAWL_STACK | Go e2e | timeout (min) |',
      '| --- | --- | --- | --- | --- | --- |',
      ...include.map(
        (e) =>
          `| \`${e.name}\` | ${e.mode} | ${e.specs.split(' ').filter(Boolean).length} | ${e.crawlStack || '(none)'} | ${e.runGoE2E ? 'yes' : 'no'} | ${e.timeoutMinutes} |`,
      ),
    ].join('\n'),
  );
  log(
    `dashboard-matrix: emitted ${include.length}-entry matrix` +
      (includeCrawl ? '.' : ' (smoke-only; crawl shard runs on schedule/dispatch).'),
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
    error(`dashboard-matrix: unknown MODE "${mode}" (want emit|verify)`);
    process.exit(1);
  }
}

// Exported for the unit guard (.github/scripts/dashboard-matrix.test.mjs).
export {
  SHARDS,
  EXCLUDED,
  SHARD_NAME_RE,
  CRAWL_SHARD_TIMEOUT_MIN,
  SMOKE_SHARD_TIMEOUT_MIN,
  CRAWL_STACK_K3D,
  MODE_MONOLITH,
  MODE_SPLIT,
  SMOKE_MODES,
  SPLIT_ONLY_SPECS,
};
