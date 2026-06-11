/**
 * Stack registry for the Grafana surface-crawler framework.
 *
 * ONE ENGINE, N STACK CONFIGS. The engine — BFS walk, URL
 * canonicalization, the universal per-page oracles, the ds/query
 * replays, the data-quality lints, and the inventory-ratchet
 * mechanics — lives in lib.ts + the three crawl specs and is
 * single-source: it never branches on a stack name. Everything that
 * legitimately differs between Grafana deployments is declared here,
 * per stack, as data:
 *
 *   - the default Grafana base URL (env still wins),
 *   - the anonymous-auth assumption (the crawler drives no login
 *     flow — a stack that can't be crawled anonymously cannot
 *     register until the engine grows an auth step),
 *   - the crawl scope rules (route-family exclusions + app
 *     redirector aliases),
 *   - the inventory + exclusions file the ratchet pins,
 *   - the exact datasource UID set the dsquery replays expect,
 *   - the lint input floors (how much quantile surface the stack's
 *     provisioned dashboards are KNOWN to carry),
 *   - the lean-lane representative seeds and the page caps.
 *
 * CRAWL_STACK=<name> selects the config. Unset → playwright.config.ts
 * ignores crawl/** entirely (0 crawl tests — the suite never runs
 * against an unidentified stack). Set to an unknown name → loud
 * config error at config-load time (stackByName throws), never a
 * silent skip.
 *
 * Adding a stack = adding a config object here + committing its
 * (initially empty) inventory/exclusions files + wiring CRAWL_STACK
 * into the stack's CI lane. See "Grafana surface crawler" in
 * docs/test-strategy.md for the bootstrap convention.
 */

import {
  APP_BARE_PATH_ALIASES,
  EXCLUDED_PATH_PATTERNS,
  type ScopeRules,
} from './lib.js';
// Imported from the concrete module (not the helpers barrel):
// playwright.config.ts loads this registry at config time to validate
// CRAWL_STACK, and the barrel would drag every helper module — and
// their @playwright/test value imports — into config load.
import { DRILLDOWN_APPS } from '../helpers/drilldown.js';

export type DatasourceType = 'prometheus' | 'loki' | 'tempo';

export type ExpectedDatasource = {
  uid: string;
  type: DatasourceType;
};

export type CrawlStackConfig = {
  /**
   * Stack name. Must equal the CRAWL_STACK value that selects it AND
   * the `stack` field inside the committed inventory file
   * (loadInventory cross-checks).
   */
  name: string;
  /**
   * Grafana base URL used when GRAFANA_URL / GRAFANA_BASE_URL are
   * unset. CI lanes set the env explicitly; this is the local-dev
   * default.
   */
  defaultGrafanaURL: string;
  /**
   * The crawler drives NO login flow — every gesture and API probe
   * runs unauthenticated. The type is literally `true`: a stack whose
   * Grafana requires auth cannot register here until the engine grows
   * an explicit auth step (loud type error, not a silently-broken
   * crawl). crawl.spec.ts asserts the assumption live before walking.
   */
  anonymousAuth: true;
  /** Crawl scope: route-family exclusions + app redirector aliases. */
  scope: ScopeRules;
  /** Inventory file the ratchet pins, relative to crawl/. */
  inventoryFilename: string;
  /** Exclusions file, relative to crawl/. */
  exclusionsFilename: string;
  /**
   * The `doc` header written into the inventory file on regeneration
   * (CERBERUS_UPDATE_INVENTORY=1). Per stack because the lane shape —
   * which CI lane runs lean, which runs full — differs.
   */
  inventoryDoc: string;
  /**
   * EXACT set of prometheus/loki/tempo datasources the stack
   * provisions. dsquery.spec.ts pins the live /api/datasources
   * answer against this set — a provisioning drift (datasource
   * added, removed, or re-uid'd) fails loudly instead of silently
   * shrinking the replay coverage.
   */
  expectedDatasources: ReadonlyArray<ExpectedDatasource>;
  /**
   * Lint input floors — declarations of how much quantile surface
   * the stack's PROVISIONED dashboards carry, so the lints'
   * vacuous-pass guards stay honest per stack. These are floors on
   * the lint's INPUT (shrink below the floor fails — a dashboard
   * regression), never tolerances on its verdicts: every family /
   * panel that exists is always judged on every stack.
   */
  lints: {
    /**
     * Minimum number of classic histogram families consumed by
     * histogram_quantile panels (lint 1's input).
     */
    minQuantileConsumedFamilies: number;
    /**
     * Minimum number of panels carrying ≥2 distinct
     * histogram_quantile targets (lint 2's input). 0 is legitimate
     * for a stack whose provisioned dashboards have no
     * multi-quantile panel — the lint still judges any that appear.
     */
    minMultiQuantilePanels: number;
  };
  /**
   * Lean-lane representative seeds: concrete paths (relative to the
   * Grafana base URL, query allowed) enqueued after the root page.
   * Together with the root and the nav links harvested from it,
   * these define the lean (fast-lane) surface set.
   */
  leanSeedRoots: ReadonlyArray<string>;
  /** Hard page caps — exceeding one FAILS the crawl (never partial). */
  pageCapLean: number;
  pageCapFull: number;
};

// ---------------------------------------------------------------------------
// Shared building blocks
// ---------------------------------------------------------------------------

/**
 * Both registered stacks run the same pinned Grafana image
 * (grafana/grafana:12.2.9) with mirrored provisioning, so they share
 * the Grafana-level scope rules and the datasource UID set. The
 * sharing is deliberate convergence, not a framework assumption — a
 * stack with different provisioning declares its own values.
 */
const GRAFANA_12_SCOPE: ScopeRules = {
  excludedPathPatterns: EXCLUDED_PATH_PATTERNS,
  appBarePathAliases: APP_BARE_PATH_ALIASES,
};

/**
 * Mirrored across test/e2e/grafana/compose/datasources/cerberus.yaml
 * and test/e2e/k3s/grafana.yaml: the three Cerberus-* datasources plus
 * the three grafanacloud-* aliases Grafana's first-party drilldown
 * apps boot against.
 */
const CERBERUS_DATASOURCES: ReadonlyArray<ExpectedDatasource> = [
  { uid: 'cerberus-prometheus', type: 'prometheus' },
  { uid: 'cerberus-loki', type: 'loki' },
  { uid: 'cerberus-tempo', type: 'tempo' },
  { uid: 'grafanacloud-metrics', type: 'prometheus' },
  { uid: 'grafanacloud-logs', type: 'loki' },
  { uid: 'grafanacloud-traces', type: 'tempo' },
];

/**
 * The lean representative set both stacks use today: one entry route
 * per first-party drilldown app (the catalogue in
 * helpers/drilldown.ts). Concrete paths — they may pin var-* state
 * the app needs on a cold context.
 */
const DRILLDOWN_APP_SEEDS: ReadonlyArray<string> = DRILLDOWN_APPS.map(
  (app) => app.root,
);

// ---------------------------------------------------------------------------
// Stack configs
// ---------------------------------------------------------------------------

/** Repo-root quickstart docker-compose stack (the `compose-smoke` CI job). */
export const COMPOSE_STACK: CrawlStackConfig = {
  name: 'compose',
  defaultGrafanaURL: 'http://localhost:3000',
  anonymousAuth: true,
  scope: GRAFANA_12_SCOPE,
  inventoryFilename: 'grafana-surface-inventory.compose.json',
  exclusionsFilename: 'grafana-surface-exclusions.compose.json',
  inventoryDoc:
    'Pinned canonical Grafana surface set for the compose stack, emitted by ' +
    'test/e2e/playwright/crawl/crawl.spec.ts. lean=true rows are the per-PR crawl ' +
    '(root + nav + one representative per drilldown app); every row is crawled nightly. ' +
    'Regenerate deliberately against a healthy compose stack with: ' +
    'CERBERUS_UPDATE_INVENTORY=1 SWEEP_DEPTH=full CRAWL_STACK=compose npx playwright test crawl/crawl.spec.ts',
  expectedDatasources: CERBERUS_DATASOURCES,
  lints: {
    // cerberus.json + showcase-promql carry quantile panels; otelcol.json
    // carries the one multi-quantile (p50/p95/p99) panel.
    minQuantileConsumedFamilies: 1,
    minMultiQuantilePanels: 1,
  },
  leanSeedRoots: DRILLDOWN_APP_SEEDS,
  pageCapLean: 30,
  pageCapFull: 80,
};

/**
 * k3d full-stack cluster (the `dashboard` job in
 * .github/workflows/e2e.yml; `just e2e-up` locally). Same Grafana
 * image + datasource provisioning as compose, but only the cerberus
 * self-observability dashboard is provisioned
 * (test/e2e/grafana/dashboards/) — the compose-only receiver
 * dashboards (clickhouse / host / otelcol / showcase-*) don't exist
 * here, so the surface set and the lint floors differ.
 *
 * Lane shape: the dashboard job's crawl step runs on the nightly
 * schedule + manual dispatch at SWEEP_DEPTH=full (the per-PR / per-
 * merge fast lane is the compose stack's job). The committed
 * inventory starts EMPTY — the bootstrap convention: the first
 * dispatch with update_crawl_inventory=true regenerates it against
 * the live cluster and uploads it as an artifact to commit; until
 * that lands, every k3d crawl run fails loudly via
 * assertInventoryBootstrapped, so the bootstrap state cannot
 * silently become permanent.
 */
export const K3D_STACK: CrawlStackConfig = {
  name: 'k3d',
  defaultGrafanaURL: 'http://localhost:3000',
  anonymousAuth: true,
  scope: GRAFANA_12_SCOPE,
  inventoryFilename: 'grafana-surface-inventory.k3d.json',
  exclusionsFilename: 'grafana-surface-exclusions.k3d.json',
  inventoryDoc:
    'Pinned canonical Grafana surface set for the k3d stack, emitted by ' +
    'test/e2e/playwright/crawl/crawl.spec.ts. The k3d crawl lane (dashboard job, ' +
    '.github/workflows/e2e.yml) runs nightly + on manual dispatch at SWEEP_DEPTH=full, so every ' +
    'row is crawled each run; lean=true rows mark the local fast-lane subset only. ' +
    'Regenerate deliberately against a healthy k3d stack (just e2e-up && just e2e-seed-rolling) with: ' +
    'CERBERUS_UPDATE_INVENTORY=1 SWEEP_DEPTH=full CRAWL_STACK=k3d npx playwright test crawl/crawl.spec.ts ' +
    '— or dispatch the e2e workflow with update_crawl_inventory=true and commit the uploaded artifact.',
  expectedDatasources: CERBERUS_DATASOURCES,
  lints: {
    // test/e2e/grafana/dashboards/cerberus.json carries one
    // histogram_quantile panel (p95 over
    // cerberus_queries_duration_seconds) and NO multi-quantile panel.
    // Floor 0 declares that fact — lint 2 still judges any
    // multi-quantile panel a future dashboard adds.
    minQuantileConsumedFamilies: 1,
    minMultiQuantilePanels: 0,
  },
  leanSeedRoots: DRILLDOWN_APP_SEEDS,
  pageCapLean: 30,
  pageCapFull: 80,
};

// ---------------------------------------------------------------------------
// Selection
// ---------------------------------------------------------------------------

const STACKS: ReadonlyMap<string, CrawlStackConfig> = new Map(
  [COMPOSE_STACK, K3D_STACK].map((s) => [s.name, s]),
);

export function knownStackNames(): string[] {
  return [...STACKS.keys()];
}

/**
 * Resolve a stack config by name. Throws on an unknown name — a
 * typo'd CRAWL_STACK must fail the run loudly (playwright.config.ts
 * calls this at config-load time), never silently skip the suite.
 */
export function stackByName(name: string): CrawlStackConfig {
  const cfg = STACKS.get(name);
  if (cfg === undefined) {
    throw new Error(
      `CRAWL_STACK=${JSON.stringify(name)} names no registered stack config — ` +
        `known stacks: ${knownStackNames().join(', ')}. Register the stack in ` +
        `test/e2e/playwright/crawl/stacks.ts (config + inventory bootstrap) before crawling it.`,
    );
  }
  return cfg;
}

/**
 * The stack selected by CRAWL_STACK. Unset is a programming error
 * here: playwright.config.ts ignores crawl/** when CRAWL_STACK is
 * unset, so no crawl spec should ever execute without a selection.
 */
export function activeStack(): CrawlStackConfig {
  const raw = process.env.CRAWL_STACK ?? '';
  if (raw === '') {
    throw new Error(
      'activeStack: CRAWL_STACK is unset — playwright.config.ts must ignore crawl/** ' +
        'when no stack is selected; reaching this error means that gate broke',
    );
  }
  return stackByName(raw);
}
