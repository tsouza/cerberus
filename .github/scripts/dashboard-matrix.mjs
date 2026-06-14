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
// What this manifest DOES with the crawl (the frontier split): it fans the
// crawl trio across CRAWL_SUB_SHARDS dedicated k3d clusters, each a matrix entry
// carrying a CRAWL_SHARD_INDEX / CRAWL_SHARD_COUNT pair. The crawl engine
// (test/e2e/playwright/crawl/sharding.ts) has every shard run the WHOLE cheap
// BFS discovery walk but audit + pin only the ~1/N of surfaces it OWNS
// (fnv1a(path) % COUNT == INDEX) — the heavy per-surface interaction sweep is
// the dominant cost, so splitting it drops the ~50min long pole toward ~50/N.
// On the bootstrap/regen dispatch each sub-shard writes its owned slice as an
// artifact and a final merge job (crawl-inventory-merge.mjs) unions them into
// the full inventory, asserting an exact disjoint cover (no surface missed, none
// doubled). The per-shard inventory ratchet (lib.ts diffInventory, scoped to the
// shard's owned rows) plus that union-merge together preserve the WHOLE-set
// coverage guarantee the unsharded crawl had.
//
// k3d cost/flake trade-off: a k3d cluster is heavy (~3-5min bring-up) and flaky
// (telemetrygen / otel-collector-gateway readiness BackOff). A matrix of N
// clusters multiplies BOTH cost and flake surface, so the total cluster count is
// kept deliberately MODEST: two smoke shards + CRAWL_SUB_SHARDS (2) crawl
// shards = 4 clusters.
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

// How many dedicated k3d clusters the crawl frontier fans across. The crawl
// engine has every shard run the WHOLE cheap BFS discovery but audit only the
// ~1/N of surfaces it owns (the heavy interaction sweep), so the ~50min long
// pole drops toward ~50/N. Kept MODEST (2): k3d is heavy + flaky, so total
// cluster count (2 smoke + this) stays at 4. Bumping it is a one-line reviewed
// change — and re-bootstrapping the k3d inventory via a dispatch is NOT required
// (the merge unions whatever COUNT the shards ran with), but the merge job's
// CRAWL_SHARD_COUNT must match.
const CRAWL_SUB_SHARDS = 2;

// The crawl trio every crawl sub-shard runs. Each sub-shard runs the SAME specs
// but with a distinct CRAWL_SHARD_INDEX, so the BFS partition (sharding.ts)
// gives each a disjoint ~1/N slice of the heavy audit work.
const CRAWL_TRIO = [
  'crawl/crawl.spec.ts', //                  full BFS — frontier sharded across CRAWL_SUB_SHARDS
  'crawl/dsquery.spec.ts',
  'crawl/lints.spec.ts',
];

// Smoke shards carry no crawl sharding (CRAWL_STACK empty → crawl/** ignored);
// their crawlShardIndex/Count render to empty env so the spec sees the
// single-shard default. A crawl sub-shard sets all three.
const NO_CRAWL_SHARD = { crawlShardIndex: '', crawlShardCount: '' };

// ---------------------------------------------------------------------------
// The partition of the dashboard-lane spec set across isolated k3d clusters.
//
// Shards, each its own k3d cluster:
//   - shard-smoke-a / shard-smoke-b — the non-crawl specs, split by
//     wall-clock weight (the internally-looping heavies — iterate-panel-kiosk,
//     compose_grafana_smoke, the iterate-* sweeps — are spread across the two
//     so neither shard carries all the long poles). CRAWL_STACK is empty →
//     crawl/** is ignored, exactly as the old unfiltered smoke step ran. Go e2e
//     runs ONCE, on shard-smoke-a, so the Go suite isn't redundantly re-run on
//     every cluster.
//   - shard-crawl-<i> (CRAWL_SUB_SHARDS of them) — the crawl trio, CRAWL_STACK=k3d,
//     SWEEP_DEPTH=full, each with its own CRAWL_SHARD_INDEX. Every sub-shard runs
//     the whole cheap BFS discovery; the heavy per-surface interaction sweep is
//     partitioned across them, so the BFS long pole drops toward ~50/N. The
//     sub-shards run concurrently with the smoke shards.
//
// Spec paths are relative to PLAYWRIGHT_DIR — exactly how they're passed to
// `npx playwright test <files>` — so the matrix entry's space-joined string
// drops verbatim into the run step.
// ---------------------------------------------------------------------------
const SMOKE_SHARDS = [
  {
    name: 'shard-smoke-a',
    crawlStack: CRAWL_STACK_NONE,
    ...NO_CRAWL_SHARD,
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
    ...NO_CRAWL_SHARD,
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
    ],
  },
];

// The crawl sub-shards, generated so adding a cluster is a single const bump.
// Each runs the WHOLE crawl trio (the dsquery/lints pins are cheap + run on
// every sub-shard; the BFS is the sharded long pole).
const CRAWL_SHARDS = Array.from({ length: CRAWL_SUB_SHARDS }, (_, i) => ({
  name: `shard-crawl-${i}`,
  crawlStack: CRAWL_STACK_K3D,
  runGoE2E: false,
  crawlShardIndex: String(i),
  crawlShardCount: String(CRAWL_SUB_SHARDS),
  // Each sub-shard lists the same crawl specs; double-assignment across crawl
  // sub-shards is EXPECTED (they differ by CRAWL_SHARD_INDEX, not spec set), so
  // the coverage check below treats crawl specs as sub-shard-replicated rather
  // than a double-assignment violation.
  specs: [...CRAWL_TRIO],
}));

const SHARDS = [...SMOKE_SHARDS, ...CRAWL_SHARDS];

// The crawl trio is intentionally replicated across every crawl sub-shard
// (same specs, different CRAWL_SHARD_INDEX). The coverage check must treat
// those as ONE logical assignment, not N double-assignments.
const CRAWL_REPLICATED_SPECS = new Set(CRAWL_TRIO);

// ---------------------------------------------------------------------------
// Specs that live under PLAYWRIGHT_DIR but are NOT part of the dashboard
// (k3d) lane. Every one must be named here (with the reason) or `verify`
// fails on it as an UNASSIGNED spec. The old dashboard job ran the 27
// non-crawl specs (unfiltered) + the crawl TRIO (crawl/dsquery + lints +
// the BFS) — crawl/reconcile.spec.ts was never invoked by this lane.
// ---------------------------------------------------------------------------
const EXCLUDED = [
  'crawl/reconcile.spec.ts', //          crawl-suite reconcile pin; the k3d lane runs only the crawl trio, never reconcile
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

  // double-assignment (wasted work + a spec running on two clusters) — EXCEPT
  // the crawl trio, which is deliberately replicated across the crawl
  // sub-shards (same specs, distinct CRAWL_SHARD_INDEX). A replicated crawl
  // spec must appear ONLY on crawl sub-shards (a `shard-crawl-*` name), never
  // leak onto a smoke shard.
  for (const [spec, who] of owners) {
    if (CRAWL_REPLICATED_SPECS.has(spec)) {
      const nonCrawl = who.filter((n) => !n.startsWith('shard-crawl-'));
      if (nonCrawl.length > 0) {
        v.push(`crawl-replicated spec ${spec} assigned to non-crawl shard(s) [${nonCrawl.join(', ')}]`);
      }
      continue;
    }
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
  const uniqueAssigned = new Set(SHARDS.flatMap((s) => s.specs)).size;
  notice(
    `dashboard-matrix OK: ${SHARDS.length} shards (${CRAWL_SUB_SHARDS} crawl sub-shards), ` +
      `${uniqueAssigned} unique specs assigned, ${EXCLUDED.length} excluded, ${discovered.length} discovered.`,
  );
  process.exit(0);
}

function emit() {
  const discovered = discover();
  assertCoverageOrExit(discovered);
  const include = SHARDS.map((s) => ({
    name: s.name,
    specs: s.specs.join(' '),
    crawlStack: s.crawlStack,
    runGoE2E: s.runGoE2E,
    // Empty string on smoke shards → the run step renders empty env, so the
    // crawl spec sees CRAWL_SHARD_INDEX/COUNT unset (single-shard default).
    crawlShardIndex: s.crawlShardIndex ?? '',
    crawlShardCount: s.crawlShardCount ?? '',
  }));
  setOutput('matrix', JSON.stringify({ include }));
  setOutput('shard_names', JSON.stringify(SHARDS.map((s) => s.name)));
  // The merge job needs the crawl shard count to assert the slice union is
  // complete (every shard index in [0, count) uploaded a slice).
  setOutput('crawl_shard_count', String(CRAWL_SUB_SHARDS));
  appendStepSummary(
    [
      '### dashboard (k3d) shard matrix',
      '',
      '| shard | specs | CRAWL_STACK | crawl shard | Go e2e |',
      '| --- | --- | --- | --- | --- |',
      ...SHARDS.map(
        (s) =>
          `| \`${s.name}\` | ${s.specs.length} | ${s.crawlStack || '(none)'} | ` +
          `${s.crawlShardCount ? `${Number(s.crawlShardIndex) + 1}/${s.crawlShardCount}` : '(n/a)'} | ` +
          `${s.runGoE2E ? 'yes' : 'no'} |`,
      ),
    ].join('\n'),
  );
  log(`dashboard-matrix: emitted ${include.length}-shard matrix.`);
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
export { SHARDS, EXCLUDED, SHARD_NAME_RE, CRAWL_SUB_SHARDS, CRAWL_TRIO };
