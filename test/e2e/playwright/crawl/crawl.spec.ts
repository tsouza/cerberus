/**
 * Grafana surface crawler — BFS from the root page with universal
 * per-page oracles.
 *
 * Where the iterate-* specs enumerate KNOWN surfaces (dashboards,
 * panels, drilldown catalogue entries), the crawler DISCOVERS
 * surfaces: it BFS-walks every same-origin link reachable from the
 * Grafana root, canonicalizes URLs so the visited-set converges (see
 * crawl/lib.ts), and applies the same four oracles on every page it
 * lands on — no per-page code:
 *
 *   1. zero browser console errors. No cerberus-origin noise filter,
 *      ever (Q5 policy); there is currently no upstream-Grafana
 *      filter either — if a Grafana bump introduces an unfixable
 *      upstream console error, follow the precedent set by
 *      KIOSK_UPSTREAM_GRAFANA_CONSOLE_NOISE in
 *      iterate-panel-kiosk.spec.ts (single narrowly-scoped regex +
 *      upstream issue reference), never a broad filter.
 *   2. zero non-2xx responses on the datasource API surface families
 *      (`/api/ds/query`, `/api/dashboards/`,
 *      `/api/datasources/proxy/uid/`, `/api/datasources/uid/…/resources/`
 *      — the same capture set every existing sweep watches), and zero
 *      tunneled `.results.<refId>.error` in 2xx ds/query bodies. The
 *      ONLY sanctioned failures are those attributable to a panel
 *      with a declared `cerberus.expect: "error:<substring>"`
 *      contract on the dashboard being rendered.
 *   3. panel tri-state: every rendered panel must end in
 *      has-data | declared-empty | declared-error. A "No data" panel
 *      without a cerberus.expect declaration fails with the panel
 *      title + page URL in the message.
 *   4. no page-level crash banner ("an unexpected error happened",
 *      "application error", …) and no `role="alert"` banner with
 *      error-class text anywhere on the page.
 *
 * INTERACTION SWEEP (interactions.ts): visiting a surface at its
 * default state is not enough — the 2026-06-10 maintainer find
 * (Traces Drilldown breakdown groupBy=kind → nil-comparison 422) was
 * a state no harvested link encodes, reached only by clicking a
 * control. After each surface's base audit the crawler discovers its
 * view-affecting controls (tab strips, radio groups, comboboxes,
 * attribute pickers, metric tiles, adhoc-filter builders; mutating
 * affordances and time pickers excluded) and drives each planned
 * deviation against a FRESH navigation. A deviation that encodes to
 * the URL becomes a first-class surface (the canonicalizer retains
 * structural params — see StructuralParamRule in lib.ts); one that
 * doesn't is audited in place with the same oracle set and pins into
 * the inventory as `<canonical>#<control>=<value>`. Bounding is the
 * locked pairwise design (see interactions.ts): structural controls
 * enumerate fully, high-cardinality controls take one
 * representative, cross-control combos form pairwise via surface
 * chaining, and every plan is hard-capped — overflow fails loudly.
 *
 * Depth doctrine (see helpers/depth.ts — depth changes STATES, never
 * RULES): at 'lean' (the per-PR gate) the crawl visits the root page,
 * the nav links harvested from it, and one representative per
 * drilldown app, and sweeps interactions on the configured
 * representative roots with one state per control. At 'full'
 * (nightly) the BFS is exhaustive up to a HARD page cap that fails
 * the run when exceeded — surface growth must force a deliberate cap
 * bump, never a silent partial crawl — and the interaction sweep
 * covers every eligible surface exhaustively.
 *
 * STACK FRAMEWORK: this spec is the stack-agnostic engine driver.
 * CRAWL_STACK=<name> selects a config from crawl/stacks.ts (base
 * URL default, scope rules, inventory file, lean seeds, page caps);
 * nothing here branches on a stack name. The visited-set is pinned
 * by the active stack's crawl/grafana-surface-inventory.<stack>.json
 * (the ratchet): a new surface (e.g. a Grafana bump adds an app page)
 * fails the run until the inventory is regenerated deliberately via
 *
 *   CERBERUS_UPDATE_INVENTORY=1 SWEEP_DEPTH=full CRAWL_STACK=<stack> \
 *     npx playwright test crawl/crawl.spec.ts
 *
 * against a healthy instance of that stack — mirroring the
 * test/inventory/ convention. Coverage shrink (a pinned surface no
 * longer visited) fails symmetrically and has no regen escape: fix
 * the regression. A newly registered stack starts from an EMPTY
 * committed inventory and FAILS LOUDLY on every run until the
 * bootstrap regen lands (see assertInventoryBootstrapped) — the
 * bootstrap state cannot silently become permanent.
 *
 * Motivation: an off-CI AI screenshot sweep (2026-06-09) found 34
 * unique error signatures across 55 BFS-visited pages, several on
 * surfaces no enumerated spec visits (drilldown-app tabs,
 * logs-drilldown service pages, traces-drilldown comparison). The AI
 * sweep's irreplaceable role is DISCOVERING which invariants to
 * check, off-CI; this crawler carries the accumulated deterministic
 * versions in CI forever.
 *
 * Env:
 *   CRAWL_STACK                      stack config name (see stacks.ts);
 *                                    unset → playwright.config.ts
 *                                    ignores crawl/** (0 tests);
 *                                    unknown → loud config error
 *   GRAFANA_URL / GRAFANA_BASE_URL   default: the stack config's URL
 *   CERBERUS_URL                     default http://localhost:8080
 *   SWEEP_DEPTH                      'lean' (default) | 'full'
 *   CERBERUS_UPDATE_INVENTORY        regen the surface inventory
 */

import { readFileSync, writeFileSync } from 'node:fs';
import {
  expect,
  test,
  type Browser,
  type BrowserContext,
  type Page,
  type Response,
} from '@playwright/test';

import {
  awaitSeedFixtureSignal,
  awaitSelfTelemetryRangeSignal,
  captureConsoleErrors,
  describeSweepDepth,
  generateSelfTraffic,
  iterateDashboards,
  readPanelExpectation,
  resolveInitRaceConsoleTwins,
  sweepDepth,
  tolerateRepaintFlicker,
  UNREADABLE_BODY_SENTINEL,
} from '../helpers/index.js';
import {
  ALERT_ERROR_PATTERNS,
  PAGE_CRASH_PATTERNS,
  STRUCTURAL_PARAM_RULES,
  assertInventoryBootstrapped,
  byCodepoint,
  canonicalTarget,
  canonicalizeURL,
  collectVisibleAlertBanners,
  diffInventory,
  expandSiblingTabs,
  harvestLinks,
  inventoryPath,
  isSupersededDsQueryFailure,
  isTransientInitRaceFailure,
  loadExclusions,
  loadInventory,
  marshalInventory,
  pinnedStructuralParamCount,
  refIdToExpr,
  succeededDsQuerySignatures,
  truncate,
  type ScopeRules,
  type SurfaceInventory,
} from './lib.js';
import {
  discoverControls,
  driveInteraction,
  interactionStateKey,
  planInteractions,
  representativeOption,
  settleAdhocFilterBar,
  type PlannedInteraction,
} from './interactions.js';
import {
  activeStack,
  knownStackNames,
  stackByName,
} from './stacks.js';

// Self-traffic warmup — same rationale + value as the iterate-* specs:
// without populated counters/streams/traces, a "No data" panel on a
// fresh stack is indistinguishable from a real regression.
const SEED_TRAFFIC_SECONDS = 30;

// Hard page caps live in the stack config (stack.pageCapLean /
// stack.pageCapFull). The FULL cap fails the run when the frontier is
// still non-empty at the cap — surface growth (a Grafana bump adding
// pages) must force a deliberate, reviewed cap bump in stacks.ts,
// never a silently-partial crawl. The lean cap exists for the same
// reason at fast-lane scale.

// Recycle the browser context every N NAVIGATIONS. A single renderer
// reused across the whole full-depth crawl accumulates state until
// Chromium refuses requests with net::ERR_INSUFFICIENT_RESOURCES or
// crashes the renderer outright (iterate-panel-kiosk documents the
// cliff at ~190 navigations; the first full interaction-sweep run
// crashed far earlier because every planned gesture adds a fresh
// navigation of a heavy scenes app). Counting NAVIGATIONS — base
// visits AND per-interaction fresh navigations — keeps the margin
// wide; 25 trades a little context-boot overhead for renderer
// stability.
const CONTEXT_RECYCLE_NAVIGATIONS = 25;

/**
 * Page provider with navigation-budgeted context recycling and crash
 * recovery. Every navigation in the crawl — BFS base visits and the
 * interaction sweep's per-gesture fresh navigations — goes through
 * `acquire()` + `noteNavigation()`, so the recycle budget counts
 * what actually wears the renderer out. A renderer crash flags the
 * lease and the next acquire() starts a clean context instead of
 * cascading "Page crashed" through every remaining state (the
 * failure shape of the first full-depth run).
 */
type PageLease = {
  acquire: () => Promise<Page>;
  noteNavigation: () => void;
  close: () => Promise<void>;
};

function makePageLease(browser: Browser): PageLease {
  let context: BrowserContext | null = null;
  let page: Page | null = null;
  let navsInContext = 0;
  let crashed = false;
  return {
    acquire: async () => {
      if (
        page === null ||
        page.isClosed() ||
        crashed ||
        navsInContext >= CONTEXT_RECYCLE_NAVIGATIONS
      ) {
        if (context !== null) await context.close().catch(() => {});
        context = await browser.newContext();
        page = await context.newPage();
        page.on('crash', () => {
          crashed = true;
        });
        navsInContext = 0;
        crashed = false;
      }
      return page;
    },
    noteNavigation: () => {
      navsInContext++;
    },
    close: async () => {
      if (context !== null) await context.close().catch(() => {});
      context = null;
      page = null;
    },
  };
}

/**
 * Budget for one crawl navigation to reach `domcontentloaded`. The
 * render settle that follows is bounded separately.
 */
const NAVIGATION_TIMEOUT_MS = 90_000;

/**
 * Navigate with the SAME cold app state every time.
 *
 * Grafana's frontend and the first-party drilldown apps persist UI
 * state in localStorage / sessionStorage — the docked-nav state, the
 * Metrics Drilldown recent-metric list, the Traces Drilldown favourite
 * attributes, and the scenes apps' restored variable and filter sets
 * (the same persistence lib.ts documents on the exploretraces re-entry
 * boot). One browser context reused across navigations therefore hands
 * each surface a boot state that depends on which surfaces preceded it
 * — and, because the context is recycled on a rolling navigation
 * budget rather than a structural boundary, on how many interactions
 * those surfaces happened to plan.
 *
 * That is a run-to-run coupling between unrelated surfaces: one
 * control discovered a beat late early in the walk shifts the boot
 * state of everything after it, so the crawl stops being a function of
 * the application and starts being a function of its own history.
 * Clearing both stores immediately before every navigation gives every
 * surface — and every interaction state, whose provenance claim is
 * "surface default plus exactly one control deviation" — the identical
 * cold baseline that claim already assumed.
 */
async function gotoCold(page: Page, url: string): Promise<void> {
  // A context that has not navigated yet sits on about:blank, where
  // storage access throws SecurityError. Nothing is persisted there,
  // so there is nothing to clear.
  if (page.url().startsWith('http')) {
    await page.evaluate(() => {
      localStorage.clear();
      sessionStorage.clear();
    });
  }
  await page.goto(url, {
    waitUntil: 'domcontentloaded',
    timeout: NAVIGATION_TIMEOUT_MS,
  });
}

type CrawlFailure = {
  url: string; // canonical surface
  rule: string;
  detail: string;
};

type QueueEntry = {
  canonical: string;
  /** Concrete URL (path + query) actually navigated for this surface. */
  concrete: string;
  /** Canonical URL of the page that first discovered this surface. */
  via: string;
};

// ---------------------------------------------------------------------------
// Canonicalization pins — pure-function regression pins for the rules
// the inventory's stability depends on. A rule drift that re-keys
// surfaces would otherwise surface as a confusing inventory diff.
// ---------------------------------------------------------------------------

test.describe('crawl: canonicalization pins', () => {
  const base = 'http://localhost:3000';
  // Scope rules come from the ACTIVE stack — the rules are per-stack
  // data, and the pins assert them under whichever stack the lane
  // selected. Today every registered stack shares the Grafana-12
  // scope (see stacks.ts), so the expectations below hold under any
  // CRAWL_STACK; a stack that diverges gets its own pin rows.
  const scope: ScopeRules = activeStack().scope;

  test('CRAWL_STACK selection: unknown stack names fail loudly, registered configs are sound', () => {
    // A typo'd stack name must never silently skip the suite — the
    // same check runs at config-load time in playwright.config.ts;
    // this pin keeps the error shape itself from regressing.
    expect(() => stackByName('no-such-stack')).toThrow(
      /names no registered stack config/,
    );
    expect(knownStackNames().length).toBeGreaterThan(0);
    for (const name of knownStackNames()) {
      const cfg = stackByName(name);
      expect(cfg.name, `stack ${name}: registry key matches config name`).toBe(
        name,
      );
      expect(
        cfg.pageCapLean,
        `stack ${name}: lean page cap is positive`,
      ).toBeGreaterThan(0);
      expect(
        cfg.pageCapFull,
        `stack ${name}: full cap is at least the lean cap (lean ⊆ full)`,
      ).toBeGreaterThanOrEqual(cfg.pageCapLean);
      expect(
        cfg.expectedDatasources.length,
        `stack ${name}: at least one expected datasource`,
      ).toBeGreaterThan(0);
      expect(
        new Set(cfg.expectedDatasources.map((d) => d.uid)).size,
        `stack ${name}: expected datasource uids are unique`,
      ).toBe(cfg.expectedDatasources.length);
      for (const root of cfg.leanSeedRoots) {
        expect(
          canonicalTarget(root, cfg.defaultGrafanaURL, cfg.scope),
          `stack ${name}: lean seed root ${root} canonicalizes in-scope`,
        ).not.toBeNull();
      }
      expect(
        cfg.leanInteractionRoots.length,
        `stack ${name}: at least one lean interaction root (the gap class the sweep exists for)`,
      ).toBeGreaterThan(0);
      for (const root of cfg.leanInteractionRoots) {
        // Interaction roots are CANONICAL surface keys: they must be
        // their own canonical form (already path-rewritten, no
        // session params, no in-place state suffix).
        expect(
          canonicalizeURL(root, cfg.defaultGrafanaURL, cfg.scope),
          `stack ${name}: lean interaction root ${root} is a canonical surface key`,
        ).toBe(root);
      }
      // EVERY stack's committed files must load (existence + shape +
      // the inventory's stack field matching the config name) and the
      // inventory must round-trip byte-for-byte through the canonical
      // marshaller — asserted here for all stacks so each lane guards
      // the files of stacks it never activates (a hand-edited k3d
      // file can't drift while only the compose lane runs per-PR).
      const inv = loadInventory(cfg);
      loadExclusions(cfg);
      expect(
        readFileSync(inventoryPath(cfg), 'utf8'),
        `stack ${name}: committed inventory is in canonical marshalled form`,
      ).toBe(marshalInventory(inv));
      // Regenerating must produce a surfaces-only diff: the committed
      // doc header has to match what crawl.spec.ts would write from
      // the config on the next CERBERUS_UPDATE_INVENTORY=1 run.
      expect(
        inv.doc,
        `stack ${name}: committed inventory doc matches the config's inventoryDoc`,
      ).toBe(cfg.inventoryDoc);
    }
  });

  test('canonical keys are path-only — volatile and session-state params are stripped', () => {
    expect(
      canonicalizeURL(
        '/d/abc/some-slug?orgId=1&from=now-1h&to=now&refresh=10s&viewPanel=4&kiosk',
        base,
        scope,
      ),
    ).toBe('/d/abc');
    // Drilldown-app session state (patterns/displayedFields/layout/…)
    // is a state of the surface, not a new surface — the first full
    // crawl produced four param-permutations of this one page.
    expect(
      canonicalizeURL(
        '/a/grafana-lokiexplore-app/explore/service/cerberus/logs?patterns=%5B%5D&displayedFields=%5B%5D&visualizationType=%22logs%22',
        base,
        scope,
      ),
    ).toBe('/a/grafana-lokiexplore-app/explore/service/{service}/logs');
    expect(canonicalizeURL('/dashboards?tag=b&tag=a&orgId=1', base, scope)).toBe(
      '/dashboards',
    );
    expect(canonicalizeURL('/?orgId=1', base, scope)).toBe('/');
  });

  test('bare app redirectors alias to their entry route', () => {
    expect(canonicalTarget('/a/grafana-exploretraces-app', base, scope)).toEqual({
      canonical: '/a/grafana-exploretraces-app/explore',
      concrete: '/a/grafana-exploretraces-app/explore',
    });
    expect(canonicalizeURL('/a/grafana-metricsdrilldown-app', base, scope)).toBe(
      '/a/grafana-metricsdrilldown-app/drilldown',
    );
  });

  test('provisioning-minted folder uids parameterize and slugs drop', () => {
    expect(canonicalizeURL('/dashboards/f/efor9e5025vcwb', base, scope)).toBe(
      '/dashboards/f/{folder}',
    );
    expect(
      canonicalizeURL('/dashboards/f/efor9e5025vcwb/cerberus', base, scope),
    ).toBe('/dashboards/f/{folder}');
    expect(
      canonicalizeURL('/dashboards/f/efor9e5025vcwb/cerberus/alerting', base, scope),
    ).toBe('/dashboards/f/{folder}/alerting');
  });

  test('data-derived label segments parameterize', () => {
    expect(
      canonicalizeURL(
        '/a/grafana-lokiexplore-app/explore/service/shop/label/detected_level',
        base,
        scope,
      ),
    ).toBe('/a/grafana-lokiexplore-app/explore/service/{service}/label/{label}');
  });

  test('structural params join the canonical key; defaults and session params drop', () => {
    // The maintainer-found gap: var-groupBy selects WHICH query the
    // breakdown fires — a consumption mode, hence a surface. The
    // ATTRIBUTE NAME is data-derived, so the value parameterizes.
    // Boot defaults (actionView=breakdown, var-metric=rate,
    // var-groupBy=resource.service.name) drop so the app rewriting
    // its defaults into the URL can't re-key the bare surface.
    expect(
      canonicalizeURL(
        '/a/grafana-exploretraces-app/explore?from=now-30m&to=now&var-ds=cerberus-tempo&var-filters=&var-metric=rate&var-groupBy=kind&actionView=breakdown',
        base,
        scope,
      ),
    ).toBe('/a/grafana-exploretraces-app/explore?var-groupBy={var-groupBy}');
    // Two pinned params sort by name — pairwise-terminal state.
    expect(
      canonicalizeURL(
        '/a/grafana-exploretraces-app/explore?var-groupBy=kind&actionView=comparison',
        base,
        scope,
      ),
    ).toBe(
      '/a/grafana-exploretraces-app/explore?actionView=comparison&var-groupBy={var-groupBy}',
    );
    // All-defaults URL keys the bare surface.
    expect(
      canonicalizeURL(
        '/a/grafana-exploretraces-app/explore?actionView=breakdown&var-metric=rate&var-groupBy=resource.service.name&var-primarySignal=nestedSetParent%3C0',
        base,
        scope,
      ),
    ).toBe('/a/grafana-exploretraces-app/explore');
    // High-cardinality structural params parameterize ({metric}), and
    // boot defaults written alongside still drop.
    expect(
      canonicalizeURL(
        '/a/grafana-metricsdrilldown-app/drilldown?metric=cerberus_admit_rejected_total&actionView=breakdown&var-groupby=%24__all&layout=grid',
        base,
        scope,
      ),
    ).toBe('/a/grafana-metricsdrilldown-app/drilldown?metric={metric}');
    // Logs service pages: visualizationType deviations key surfaces
    // (values are JSON-string-quoted by the app); the boot default drops.
    expect(
      canonicalizeURL(
        '/a/grafana-lokiexplore-app/explore/service/shop/logs?visualizationType=%22table%22&sortOrder=%22Descending%22',
        base,
        scope,
      ),
    ).toBe(
      '/a/grafana-lokiexplore-app/explore/service/{service}/logs?visualizationType="table"',
    );
    expect(
      canonicalizeURL(
        '/a/grafana-lokiexplore-app/explore/service/shop/logs?visualizationType=%22logs%22',
        base,
        scope,
      ),
    ).toBe('/a/grafana-lokiexplore-app/explore/service/{service}/logs');
  });

  test('parameterizing var-groupBy absorbs data churn but still catches real coverage change', () => {
    // #1825: one added resource attribute reshuffled the breakdown's
    // option list, so the sweep's representative moved from
    // resource.service.version to resource.service.namespace and the
    // ratchet fired BOTH halves at once for a pure data change. Under
    // the parameterized rule the two states key one surface.
    const asVersion = canonicalizeURL(
      '/a/grafana-exploretraces-app/explore?var-groupBy=resource.service.version',
      base,
      scope,
    );
    const asNamespace = canonicalizeURL(
      '/a/grafana-exploretraces-app/explore?var-groupBy=resource.service.namespace',
      base,
      scope,
    );
    expect(asVersion).toBe(asNamespace);

    const stack = activeStack();
    const groupBySurface = asNamespace!;
    const inventory = {
      doc: '',
      stack: stack.name,
      surfaces: [
        { url: '/a/grafana-exploretraces-app/explore', lean: true },
        { url: groupBySurface, lean: true },
      ],
    };
    const noExclusions = { doc: '', exclusions: [] };

    // Data churn alone: the ratchet is silent.
    expect(
      diffInventory(
        new Set(['/a/grafana-exploretraces-app/explore', groupBySurface]),
        inventory,
        noExclusions,
        'lean',
        stack,
      ),
    ).toEqual([]);

    // The gate is NOT dead — a genuinely unvisited surface still fails.
    // If the sweep stopped driving the groupBy control entirely, no
    // value of it can key groupBySurface, so this is exactly the
    // regression the collapse must still catch.
    const shrank = diffInventory(
      new Set(['/a/grafana-exploretraces-app/explore']),
      inventory,
      noExclusions,
      'lean',
      stack,
    );
    expect(shrank).toHaveLength(1);
    expect(shrank[0]).toContain('coverage shrank');
    expect(shrank[0]).toContain(groupBySurface);

    // …and a genuinely new surface still fails.
    const grew = diffInventory(
      new Set([
        '/a/grafana-exploretraces-app/explore',
        groupBySurface,
        '/a/grafana-exploretraces-app/explore?actionView=comparison',
      ]),
      inventory,
      noExclusions,
      'lean',
      stack,
    );
    expect(grew).toHaveLength(1);
    expect(grew[0]).toContain('coverage grew');
  });

  test('parameterizing metrics-drilldown var-groupby absorbs label churn but still catches real coverage change', () => {
    // Same derivation as the traces var-groupBy, reached by asking the
    // one question rather than by waiting for a red: the Group by
    // control is a QueryVariable over `label_names(<metric>)`, so its
    // options are the label names of whichever series the stack
    // ingested. No closed set exists, so it cannot enumerate — and
    // enumerating it meant every label added to the seeded metrics
    // would have re-keyed a surface and reported churn as coverage.
    const asJob = canonicalizeURL(
      '/a/grafana-metricsdrilldown-app/drilldown?metric=cerberus_http_requests_total&var-groupby=job',
      base,
      scope,
    );
    const asInstance = canonicalizeURL(
      '/a/grafana-metricsdrilldown-app/drilldown?metric=cerberus_http_requests_total&var-groupby=instance',
      base,
      scope,
    );
    expect(asJob).toBe(asInstance);
    expect(asJob).toBe(
      '/a/grafana-metricsdrilldown-app/drilldown?metric={metric}&var-groupby={var-groupby}',
    );
    // The includeAll sentinel is the cold-boot value and still drops.
    expect(
      canonicalizeURL(
        '/a/grafana-metricsdrilldown-app/drilldown?metric=cerberus_http_requests_total&var-groupby=%24__all',
        base,
        scope,
      ),
    ).toBe('/a/grafana-metricsdrilldown-app/drilldown?metric={metric}');

    // The collapse is not a hole: losing the grouped surface entirely
    // — the sweep no longer driving the Group by control at all — is
    // still a coverage loss the ratchet reports.
    const stack = activeStack();
    const bare = '/a/grafana-metricsdrilldown-app/drilldown?metric={metric}';
    const grouped = asJob!;
    const inventory = {
      doc: '',
      stack: stack.name,
      surfaces: [
        { url: bare, lean: true },
        { url: grouped, lean: true },
      ],
    };
    const noExclusions = { doc: '', exclusions: [] };

    expect(
      diffInventory(
        new Set([bare, grouped]),
        inventory,
        noExclusions,
        'lean',
        stack,
      ),
    ).toEqual([]);

    const shrank = diffInventory(
      new Set([bare]),
      inventory,
      noExclusions,
      'lean',
      stack,
    );
    expect(shrank).toHaveLength(1);
    expect(shrank[0]).toContain('coverage shrank');
    expect(shrank[0]).toContain(grouped);
  });

  test('every enumerated structural param declares a closed option set holding its default', () => {
    // The mode is chosen by ONE question — "can the complete option
    // set be written down from the pinned app version?" — and this is
    // where the answer is held to account. An enumerate rule with no
    // set, or whose cold-boot value is not one of its own options, is
    // a rule written from observation rather than from the bundle:
    // exactly the defect that recorded var-primarySignal as two
    // options when its bundle array carries five.
    const enumerated = STRUCTURAL_PARAM_RULES.filter(
      (r) => r.mode === 'enumerate',
    );
    expect(enumerated.length).toBeGreaterThan(0);
    for (const rule of enumerated) {
      const where = `${rule.param} on ${rule.pathPattern.source}`;
      expect(rule.values.length, `${where}: empty option set`).toBeGreaterThan(
        0,
      );
      expect(
        new Set(rule.values).size,
        `${where}: duplicate options`,
      ).toBe(rule.values.length);
      // A canonical key is `param=value` joined by '&'; a value
      // carrying '&' would silently split into two pinned params.
      for (const v of rule.values) {
        expect(v, `${where}: option ${JSON.stringify(v)} carries '&'`).not.toContain(
          '&',
        );
      }
      if (rule.defaultValue !== undefined) {
        expect(
          rule.values,
          `${where}: cold-boot default ${JSON.stringify(rule.defaultValue)} is not one of its own options`,
        ).toContain(rule.defaultValue);
      }
    }
  });

  test('an enumerated value outside the declared set fails loudly instead of minting a surface', () => {
    // Without this the same story replays every time: the app offers
    // an option the rule never catalogued, the crawl reaches it, and
    // the only symptom is an anonymous "coverage grew" row 40 minutes
    // into the compose lane naming neither the param nor the cause.
    // Here it fails in microseconds, naming both, and states the two
    // possible fixes.
    let thrown: Error | undefined;
    try {
      canonicalizeURL(
        '/a/grafana-exploretraces-app/explore?var-primarySignal=kind=producer',
        base,
        scope,
      );
    } catch (e) {
      thrown = e as Error;
    }
    expect(thrown, 'undeclared option was silently accepted').toBeDefined();
    expect(thrown!.message).toContain('var-primarySignal');
    expect(thrown!.message).toContain('kind=producer');
    expect(thrown!.message).toContain('parameterize');

    // The check is not a blanket rejection of unfamiliar text: a value
    // the bundle really does offer canonicalizes normally.
    expect(
      canonicalizeURL(
        '/a/grafana-exploretraces-app/explore?var-primarySignal=kind=consumer',
        base,
        scope,
      ),
    ).toBe('/a/grafana-exploretraces-app/explore?var-primarySignal=kind=consumer');
  });

  test('enumerating var-primarySignal keeps every signal a distinct surface', () => {
    // The mirror image of the var-groupBy case, and the reason the
    // collapse doctrine is NOT "every var-* parameterizes". The five
    // primary signals are a literal array in the plugin bundle, each
    // splicing a DIFFERENT TraceQL fragment into every query the page
    // fires. Collapsing them would key five query families as one
    // surface and pin whichever the crawl reached first — which is
    // precisely how #1889 (Service structure 500s on every primary
    // signal but the default) would go unseen.
    const rule = STRUCTURAL_PARAM_RULES.find(
      (r) =>
        r.param === 'var-primarySignal' &&
        r.pathPattern.test('/a/grafana-exploretraces-app/explore'),
    );
    if (rule === undefined || rule.mode !== 'enumerate') {
      throw new Error(
        'var-primarySignal must remain an enumerate rule: each signal is a ' +
          'distinct TraceQL filter, hence a distinct query family',
      );
    }

    const keys = rule.values.map(
      (v) =>
        canonicalizeURL(
          `/a/grafana-exploretraces-app/explore?var-primarySignal=${encodeURIComponent(v)}`,
          base,
          scope,
        )!,
    );
    // Every non-default signal keys its own surface; the cold-boot
    // signal keys the bare one. No two collapse together.
    expect(new Set(keys).size).toBe(rule.values.length);
    expect(keys).toContain('/a/grafana-exploretraces-app/explore');

    // …and dropping one is a coverage LOSS the ratchet still reports.
    // Today's red was `kind=server` arriving as new coverage; this
    // pins the other half — that losing it again is caught.
    const stack = activeStack();
    const serverSignal =
      '/a/grafana-exploretraces-app/explore?var-primarySignal=kind=server';
    const allSpans = '/a/grafana-exploretraces-app/explore?var-primarySignal=true';
    const inventory = {
      doc: '',
      stack: stack.name,
      surfaces: [
        { url: '/a/grafana-exploretraces-app/explore', lean: true },
        { url: allSpans, lean: true },
        { url: serverSignal, lean: true },
      ],
    };
    const noExclusions = { doc: '', exclusions: [] };

    expect(
      diffInventory(
        new Set([
          '/a/grafana-exploretraces-app/explore',
          allSpans,
          serverSignal,
        ]),
        inventory,
        noExclusions,
        'lean',
        stack,
      ),
    ).toEqual([]);

    const shrank = diffInventory(
      new Set(['/a/grafana-exploretraces-app/explore', allSpans]),
      inventory,
      noExclusions,
      'lean',
      stack,
    );
    expect(shrank).toHaveLength(1);
    expect(shrank[0]).toContain('coverage shrank');
    expect(shrank[0]).toContain(serverSignal);
  });

  test('pinned structural-param counting (the pairwise depth bound)', () => {
    expect(
      pinnedStructuralParamCount('/a/grafana-exploretraces-app/explore'),
    ).toBe(0);
    expect(
      pinnedStructuralParamCount(
        '/a/grafana-exploretraces-app/explore?var-groupBy={var-groupBy}',
      ),
    ).toBe(1);
    expect(
      pinnedStructuralParamCount(
        '/a/grafana-exploretraces-app/explore?actionView=comparison&var-groupBy={var-groupBy}',
      ),
    ).toBe(2);
    expect(
      pinnedStructuralParamCount(
        '/a/grafana-metricsdrilldown-app/drilldown?metric={metric}',
      ),
    ).toBe(1);
  });

  test('interaction planning honours the locked pairwise bounds', () => {
    const control = (key: string, n: number, forced = false) => ({
      kind: 'radio' as const,
      key,
      options: Array.from({ length: n }, (_, i) => `opt${i}`),
      selectedIndex: 0,
      forcedHighCardinality: forced,
      optionHints: Array.from({ length: n }, (_, i) => `opt${i}`),
      controlHint: '',
      clickHops: 0,
    });
    // Structural controls enumerate fully (minus the selected option).
    const single = planInteractions([control('a', 4)], 0);
    expect(single.map((p) => p.option)).toEqual(['opt1', 'opt2', 'opt3']);
    expect(single.map((p) => p.leanRepresentative)).toEqual([
      true,
      false,
      false,
    ]);
    // High-cardinality (by size or by construction) → one
    // representative with a parameterized state value.
    const high = planInteractions(
      [control('big', 20), control('tiles', 3, true)],
      0,
    );
    expect(high.map((p) => `${p.control.key}=${p.stateValue}`)).toEqual([
      'big={rep}',
      'tiles={rep}',
    ]);
    // One pinned param → representative plan (pairwise combos).
    expect(
      planInteractions([control('a', 4), control('b', 4)], 1).map(
        (p) => `${p.control.key}=${p.option}`,
      ),
    ).toEqual(['a=opt1', 'b=opt1']);
    // ≥2 pinned params → terminal.
    expect(planInteractions([control('a', 4)], 2)).toEqual([]);
    // Cap overflow fails loudly, listing the plan.
    const many = Array.from({ length: 30 }, (_, i) => control(`c${i}`, 2));
    expect(() => planInteractions(many, 0)).toThrow(/exceeding the single-sweep cap/);
    expect(() =>
      planInteractions(
        Array.from({ length: 20 }, (_, i) => control(`c${i}`, 2)),
        1,
      ),
    ).toThrow(/exceeding the pairwise cap/);
  });

  test('the high-cardinality representative is a function of the option SET, not its order', () => {
    // The pick must not move when the app renders the same options in a
    // different order — a data-backed list (label names, tag values,
    // metric names) reorders as the stack ingests, and a first-in-DOM
    // pick would drive a different option, mint a different surface,
    // and make two runs of the same command disagree.
    const options = ['zulu', 'alpha', 'mike'];
    expect(representativeOption(options)).toBe('alpha');
    expect(representativeOption([...options].reverse())).toBe('alpha');
    expect(representativeOption(['mike', 'zulu', 'alpha'])).toBe('alpha');
    // Codepoint order, not host-collation order: a locale-aware
    // comparison would make the crawl a function of the runner's ICU
    // data, which differs between a developer box and CI.
    expect(representativeOption(['a', 'B'])).toBe('B');
    // A one-option control still has a representative.
    expect(representativeOption(['only'])).toBe('only');
    // The same set drives the same plan whichever order it arrives in.
    const forced = (options: string[]) => ({
      kind: 'combobox' as const,
      key: 'k',
      options,
      selectedIndex: -1,
      forcedHighCardinality: true,
      optionHints: options,
      controlHint: '',
      clickHops: 0,
    });
    expect(planInteractions([forced(['b', 'a', 'c'])], 0)[0]?.option).toBe(
      planInteractions([forced(['c', 'a', 'b'])], 0)[0]?.option,
    );
  });

  test('the inventory order is a function of the URLs, not of the host locale', () => {
    // Surface keys are mostly punctuation — `#`, `?`, `=`, `{`, `"`,
    // `-` — and a collating comparison ranks punctuation at a lower
    // strength than letters, so it disagrees with codepoint order on
    // exactly the strings this file is made of. Committing an artefact
    // in collation order would make it a function of the regenerating
    // machine's `LANG` and ICU build.
    const urls = ['/a-b', '/ab', '/a/b', '/a#b', '/A/b'];
    const surfaces = urls.map((url) => ({ url, lean: false }));
    const marshalled = marshalInventory({ doc: 'd', stack: 'compose', surfaces });
    expect(
      (JSON.parse(marshalled) as SurfaceInventory).surfaces.map((s) => s.url),
    ).toEqual([...urls].sort(byCodepoint));
    // Pinned literally, so the expectation cannot drift with the
    // comparator it exists to pin.
    expect(
      (JSON.parse(marshalled) as SurfaceInventory).surfaces.map((s) => s.url),
    ).toEqual(['/A/b', '/a#b', '/a-b', '/a/b', '/ab']);
    // Order in, same order out.
    expect(marshalInventory({ doc: 'd', stack: 'compose', surfaces })).toBe(
      marshalInventory({
        doc: 'd',
        stack: 'compose',
        surfaces: [...surfaces].reverse(),
      }),
    );
  });

  test('explore collapses to a single surface', () => {
    expect(
      canonicalizeURL('/explore?panes=%7B%22x%22%3A1%7D&schemaVersion=1', base, scope),
    ).toBe('/explore');
    expect(canonicalizeURL('/explore/metrics', base, scope)).toBe('/explore');
  });

  test('dynamic path segments parameterize', () => {
    expect(
      canonicalizeURL(
        '/a/grafana-lokiexplore-app/explore/service/cerberus/logs?var-ds=x',
        base,
        scope,
      ),
    ).toBe('/a/grafana-lokiexplore-app/explore/service/{service}/logs');
    expect(
      canonicalizeURL(
        '/a/grafana-exploretraces-app/trace/0123456789abcdef0123456789abcdef',
        base,
        scope,
      ),
    ).toBe('/a/grafana-exploretraces-app/trace/{hex}');
    expect(
      canonicalizeURL(
        '/a/grafana-lokiexplore-app/explore/service/cerberus/label/level',
        base,
        scope,
      ),
    ).toBe('/a/grafana-lokiexplore-app/explore/service/{service}/label/{label}');
    expect(
      canonicalizeURL(
        '/a/grafana-lokiexplore-app/explore/service/cerberus/field/order_id?visualizationType=%22table%22',
        base,
        scope,
      ),
    ).toBe(
      '/a/grafana-lokiexplore-app/explore/service/{service}/field/{field}?visualizationType="table"',
    );
  });

  test('parameterizing the field segment absorbs log-field churn but still catches real coverage change', () => {
    // #1862: the segment after 'field' is a log FIELD name harvested
    // from whatever the seeded logs happened to carry, so pinning it
    // pinned the dataset — the same class of defect #1825 was on the
    // query-parameter axis. Renaming or reordering a log field must
    // not move the surface key.
    const asOrderID = canonicalizeURL(
      '/a/grafana-lokiexplore-app/explore/service/cerberus/field/order_id',
      base,
      scope,
    );
    const asThread = canonicalizeURL(
      '/a/grafana-lokiexplore-app/explore/service/cerberus/field/thread',
      base,
      scope,
    );
    expect(asOrderID).toBe(asThread);

    const stack = activeStack();
    const fieldSurface = asThread!;
    const servicePage = '/a/grafana-lokiexplore-app/explore/service/{service}/logs';
    const inventory = {
      doc: '',
      stack: stack.name,
      surfaces: [
        { url: servicePage, lean: true },
        { url: fieldSurface, lean: true },
      ],
    };
    const noExclusions = { doc: '', exclusions: [] };

    // Field churn alone: the ratchet is silent.
    expect(
      diffInventory(
        new Set([servicePage, fieldSurface]),
        inventory,
        noExclusions,
        'lean',
        stack,
      ),
    ).toEqual([]);

    // The gate is NOT dead — if the sweep stopped following the
    // fields-tab drill entirely, no field name can key fieldSurface,
    // so the collapse must still report the loss.
    const shrank = diffInventory(
      new Set([servicePage]),
      inventory,
      noExclusions,
      'lean',
      stack,
    );
    expect(shrank).toHaveLength(1);
    expect(shrank[0]).toContain('coverage shrank');
    expect(shrank[0]).toContain(fieldSurface);

    // …and a genuinely new surface under the same route family still fails.
    const grew = diffInventory(
      new Set([
        servicePage,
        fieldSurface,
        `${fieldSurface}?visualizationType="table"`,
      ]),
      inventory,
      noExclusions,
      'lean',
      stack,
    );
    expect(grew).toHaveLength(1);
    expect(grew[0]).toContain('coverage grew');
  });

  test('committed inventory + exclusions files are internally consistent', () => {
    // Live-stack-free meta-checks (the live diff runs at the end of
    // the crawl): the active stack's inventory round-trips
    // byte-for-byte through the canonical marshaller (so regeneration
    // is reproducible — the test/inventory/ convention), is
    // bootstrapped (non-empty — an empty inventory fails LOUDLY with
    // the bootstrap instructions unless this run IS the bootstrap,
    // i.e. CERBERUS_UPDATE_INVENTORY is set), carries a non-empty
    // lean subset, and the exclusions file is sound (rationales
    // present, no URL in both files).
    const stack = activeStack();
    const inv = loadInventory(stack);
    const exc = loadExclusions(stack);
    assertInventoryBootstrapped(inv, stack);
    if (inv.surfaces.length > 0) {
      // Bypassed only on the sanctioned bootstrap run itself (empty
      // inventory + CERBERUS_UPDATE_INVENTORY set, enforced above) —
      // there is no lean subset to assert before the first regen.
      expect(
        inv.surfaces.filter((s) => s.lean).length,
        'lean subset is non-empty',
      ).toBeGreaterThan(0);
    }
    expect(readFileSync(inventoryPath(stack), 'utf8')).toBe(
      marshalInventory(inv),
    );
    const inventoryUrls = new Set(inv.surfaces.map((s) => s.url));
    for (const e of exc.exclusions) {
      expect(e.rationale.trim(), `exclusion ${e.url} rationale`).not.toBe('');
      expect(
        inventoryUrls.has(e.url),
        `exclusion ${e.url} must not also be a pinned inventory surface`,
      ).toBe(false);
    }
  });

  test('out-of-scope routes return null', () => {
    expect(canonicalizeURL('/alerting/list', base, scope)).toBeNull();
    expect(canonicalizeURL('/admin/settings', base, scope)).toBeNull();
    expect(canonicalizeURL('/connections/datasources', base, scope)).toBeNull();
    expect(canonicalizeURL('/dashboard/new', base, scope)).toBeNull();
    expect(canonicalizeURL('/d/abc/edit', base, scope)).toBeNull();
    expect(canonicalizeURL('/d-solo/abc?panelId=2', base, scope)).toBeNull();
    expect(canonicalizeURL('/login', base, scope)).toBeNull();
    expect(canonicalizeURL('/api/search', base, scope)).toBeNull();
    expect(canonicalizeURL('https://grafana.com/docs', base, scope)).toBeNull();
    expect(canonicalizeURL('mailto:x@example.com', base, scope)).toBeNull();
  });
});

/**
 * The wire capture reads response bodies to feed oracles 2a/2b. Grafana's
 * Drilldown apps leave streaming responses open across a navigation, so a
 * body read can never complete — and the capture's `stop()` awaits every
 * read before the crawl advances. Without a bound the crawl parks on that
 * one response until the test timeout and emits no inventory at all.
 */
test.describe('crawl: wire-capture body reads are bounded', () => {
  /** Short enough to keep the unit lane fast; the production bound is
   * WIRE_BODY_READ_TIMEOUT_MS. */
  const PROBE_BOUND_MS = 50;

  test('a body that never settles rejects at the bound instead of hanging', async () => {
    const neverSettles = {
      text: () => new Promise<string>(() => {}),
    } as unknown as Response;

    await expect(
      readBodyWithin(neverSettles, PROBE_BOUND_MS),
    ).rejects.toThrow(/did not settle/);
  });

  test('a body that settles is returned unchanged', async () => {
    const settles = {
      text: async () => '{"results":{}}',
    } as unknown as Response;

    expect(await readBodyWithin(settles, PROBE_BOUND_MS)).toBe(
      '{"results":{}}',
    );
  });

  test('a body read that throws propagates so the caller records the sentinel', async () => {
    const throws = {
      text: async () => {
        throw new Error('target page closed');
      },
    } as unknown as Response;

    await expect(readBodyWithin(throws, PROBE_BOUND_MS)).rejects.toThrow(
      /target page closed/,
    );
  });
});

// ---------------------------------------------------------------------------
// The crawl
// ---------------------------------------------------------------------------

test('crawl: BFS over every reachable Grafana surface with universal oracles + inventory ratchet', async ({
  browser,
  request,
}, testInfo) => {
  const stack = activeStack();
  const depth = sweepDepth();
  // Budget: lean ≈ 10 pages × ~6s + 30s seed + the representative
  // interaction sweep over the 3 drilldown roots (~3 min); full ≈
  // cap pages + the exhaustive interaction sweep (a fresh navigation
  // per planned gesture).
  testInfo.setTimeout(depth === 'full' ? 75 * 60_000 : 14 * 60_000);
  // eslint-disable-next-line no-console
  console.log(`crawl stack: ${stack.name} — ${describeSweepDepth(depth)}`);

  const baseURL =
    process.env.GRAFANA_URL ??
    process.env.GRAFANA_BASE_URL ??
    stack.defaultGrafanaURL;

  await generateSelfTraffic(request, SEED_TRAFFIC_SECONDS);
  // Flake #89: url=/ is the first surface this BFS audits, and the
  // cerberus-self home dashboard's "Error rate by language" panel
  // divides rate(cerberus_queries_total{result="error"}[5m]) by the
  // aggregate rate — both need ≥2 exported samples in the [5m] window
  // before they emit a point. generateSelfTraffic guarantees REQUESTS,
  // not that their exported samples have landed in ClickHouse, so on a
  // cold stack the panel could render "No data" and trip the
  // panel-no-data-undeclared oracle. This bounded, data-driven wait
  // gates the whole crawl until the panel's data is provably
  // rate()-able — parity with dsquery.spec.ts + lints.spec.ts. Loud
  // deadline failure, never a skip.
  await awaitSelfTelemetryRangeSignal(request);
  // The wait above only covers cerberus SELF-TELEMETRY. The showcase-*
  // surfaces this crawl audits render the one-shot FIXTURE seed, whose
  // compose service has no healthcheck — so the boot can hand off to the
  // crawl before the seed's first INSERT lands and the showcase-promql
  // set-operator panel (`up unless up{job="db"}`) renders an empty
  // anti-join. Gate the crawl on the seed being queryable too.
  await awaitSeedFixtureSignal(request);

  // The engine drives no login flow — every stack config declares
  // anonymousAuth and the crawl proves the assumption live before
  // walking (the `request` fixture carries no credentials).
  const authProbe = await request.get(`${baseURL}/api/search?type=dash-db`);
  expect(
    authProbe.status(),
    `stack ${stack.name} declares anonymous Grafana auth but an unauthenticated ` +
      `/api/search returned ${authProbe.status()} — fix the stack's Grafana provisioning ` +
      `(the crawler has no login step by design)`,
  ).toBe(200);

  // Declared cerberus.expect contracts, keyed by dashboard uid. The
  // crawler consumes them two ways:
  //   - declaredNoData: panel titles whose 'empty' / 'error:*'
  //     declaration legitimizes a "No data" render (tri-state oracle).
  //   - declaredErrorExprs: target expressions of declared-error
  //     panels — the only sanctioned source of non-2xx / tunneled
  //     ds/query failures on that dashboard's page.
  const dashboards = await iterateDashboards(request, baseURL);
  const declaredNoData = new Map<string, Set<string>>();
  const declaredErrorExprs = new Map<string, Set<string>>();
  for (const d of dashboards) {
    const noData = new Set<string>();
    const errExprs = new Set<string>();
    for (const p of d.panels) {
      const e = readPanelExpectation(p);
      if (!e.declared || e.expect === 'nonempty') continue;
      noData.add(p.title);
      if (e.expect.startsWith('error:')) {
        for (const t of p.targets) {
          const expr = (t.expr ?? t.query ?? '').trim();
          if (expr !== '') errExprs.add(expr);
        }
      }
    }
    declaredNoData.set(d.uid, noData);
    declaredErrorExprs.set(d.uid, errExprs);
  }

  // Seed frontier. Order is load-bearing for determinism: root first
  // (its harvest defines the lean nav set), then the stack's lean
  // representative seeds (the drilldown app entry routes), then — at
  // full depth — every provisioned dashboard (also reachable via
  // /dashboards, but seeding them makes the crawl independent of the
  // browse-page's pagination/virtualised list rendering).
  const queue: QueueEntry[] = [{ canonical: '/', concrete: '/', via: '<seed>' }];
  for (const root of stack.leanSeedRoots) {
    const target = canonicalTarget(root, baseURL, stack.scope);
    if (target === null) {
      throw new Error(
        `crawl: lean seed root ${root} canonicalizes out of scope — fix the stack config or the exclusion rules`,
      );
    }
    // Navigate the config's concrete root (it may pin a var-ds the
    // entry route needs on a cold context), keyed by the canonical.
    queue.push({
      canonical: target.canonical,
      concrete: new URL(root, baseURL).pathname + new URL(root, baseURL).search,
      via: '<seed:lean>',
    });
  }
  // The lean surface set: root + the configured representatives (the
  // nav links harvested from the root page join it during the
  // root visit below). Snapshot BEFORE the full-depth dashboard
  // seeds — dashboards are full-lane states (their fast-lane coverage
  // is the API-layer iterate-all-dashboards probes).
  const leanSet = new Set<string>(queue.map((q) => q.canonical));
  if (depth === 'full') {
    for (const d of [...dashboards].sort((a, b) => byCodepoint(a.uid, b.uid))) {
      queue.push({
        canonical: `/d/${d.uid}`,
        concrete: `/d/${d.uid}`,
        via: '<seed:dashboard>',
      });
    }
  }

  const pageCap = depth === 'full' ? stack.pageCapFull : stack.pageCapLean;
  const visited = new Map<string, string>(); // canonical → concrete navigated
  // In-place interaction states (`<canonical>#<control>=<value>`) →
  // concrete URL the gesture ran against. Kept separate from
  // `visited` because they are gestures on an already-counted page,
  // not navigations — the page cap governs navigations.
  const inPlaceVisited = new Map<string, string>();
  const failures: CrawlFailure[] = [];

  const lease = makePageLease(browser);

  try {
    while (queue.length > 0) {
      const entry = queue.shift()!;
      if (visited.has(entry.canonical)) continue;

      if (visited.size >= pageCap) {
        const remaining = [
          entry,
          ...queue.filter((q) => !visited.has(q.canonical)),
        ]
          .map((q) => `${q.canonical} (via ${q.via})`)
          .filter((v, i, a) => a.indexOf(v) === i);
        throw new Error(
          `crawl: page cap ${pageCap} (${depth}, stack=${stack.name}) exceeded with ${remaining.length} surface(s) still queued — ` +
            `surface growth must be absorbed by a deliberate cap bump in stacks.ts, not a partial crawl:\n  - ${remaining.join('\n  - ')}`,
        );
      }

      visited.set(entry.canonical, entry.concrete);

      const { harvested, pageFailures } = await visitAndAudit(
        lease,
        baseURL,
        entry,
        declaredNoData,
        declaredErrorExprs,
      );
      failures.push(...pageFailures);

      // Lean visits the seed set + the nav links harvested from the
      // root page only; full expands from every page. Same harvest
      // RULE, fewer expansion states (depth doctrine).
      if (depth === 'full' || entry.canonical === '/') {
        const canonicals = new Map<string, string>();
        for (const href of harvested) {
          const target = canonicalTarget(href, baseURL, stack.scope);
          if (target === null || visited.has(target.canonical)) continue;
          if (!canonicals.has(target.canonical)) {
            canonicals.set(target.canonical, target.concrete);
          }
        }
        for (const [canonical, concrete] of [...canonicals.entries()].sort(
          ([a], [b]) => byCodepoint(a, b),
        )) {
          queue.push({ canonical, concrete, via: entry.canonical });
          if (entry.canonical === '/') leanSet.add(canonical);
          // Known sibling-route families expand deterministically —
          // see expandSiblingTabs.
          for (const sib of expandSiblingTabs(canonical, concrete)) {
            if (!visited.has(sib.canonical) && !canonicals.has(sib.canonical)) {
              queue.push({ ...sib, via: `${entry.canonical} (sibling)` });
            }
          }
        }
      }

      // Interaction sweep — every clickable control that changes the
      // surface's consumption mode (see interactions.ts). Depth
      // doctrine: full sweeps every eligible surface exhaustively;
      // lean sweeps the configured representative roots with the
      // representative plan (one state per control). Eligibility is
      // the pairwise bound: surfaces pinning ≥2 structural params are
      // terminal (planInteractions returns an empty plan for them).
      const isLeanRoot = stack.leanInteractionRoots.includes(entry.canonical);
      if (depth === 'full' || isLeanRoot) {
        const sweep = await sweepInteractions(
          lease,
          baseURL,
          entry,
          stack.scope,
          depth !== 'full',
          declaredNoData,
          declaredErrorExprs,
        );
        failures.push(...sweep.failures);
        for (const [stateKey, state] of sweep.inPlaceStates) {
          inPlaceVisited.set(stateKey, state.concrete);
          if (isLeanRoot && state.leanRepresentative) leanSet.add(stateKey);
        }
        for (const d of sweep.discovered) {
          if (isLeanRoot && d.leanRepresentative) leanSet.add(d.canonical);
          if (!visited.has(d.canonical)) {
            queue.push({
              canonical: d.canonical,
              concrete: d.concrete,
              via: d.via,
            });
          }
        }
      }
    }
  } finally {
    await lease.close();
  }

  // The full audited state set: navigated surfaces plus the in-place
  // interaction states (`<canonical>#<control>=<value>` notation) —
  // both pin into the same inventory ratchet.
  const auditedStates = new Map<string, string>([
    ...visited,
    ...inPlaceVisited,
  ]);

  // eslint-disable-next-line no-console
  console.log(
    `crawl: audited ${auditedStates.size} state(s) (${visited.size} navigated surface(s), ` +
      `${inPlaceVisited.size} in-place interaction state(s)) at depth=${depth} stack=${stack.name}:\n${[...auditedStates.keys()]
      .sort()
      .map((u) => `  - ${u}`)
      .join('\n')}`,
  );

  // -------------------------------------------------------------------------
  // Surface-inventory ratchet. The regen WRITE happens before the
  // oracle-failure throw: the inventory pins COVERAGE (which states
  // the crawl audits) while the failures report HEALTH — a deliberate
  // regen against a stack carrying known-red states (e.g. a found bug
  // whose fix is in flight) must still capture the coverage, and the
  // run still fails loudly on the failures right below.
  // -------------------------------------------------------------------------
  if (process.env.CERBERUS_UPDATE_INVENTORY) {
    expect(
      depth,
      'inventory regeneration requires the exhaustive crawl: rerun with SWEEP_DEPTH=full',
    ).toBe('full');
    const inv: SurfaceInventory = {
      doc: stack.inventoryDoc,
      stack: stack.name,
      surfaces: [...auditedStates.keys()].map((url) => ({
        url,
        lean: leanSet.has(url),
      })),
    };
    writeFileSync(inventoryPath(stack), marshalInventory(inv));
    // eslint-disable-next-line no-console
    console.log(
      `crawl: regenerated ${inventoryPath(stack)} with ${inv.surfaces.length} surface(s)`,
    );
  }

  if (failures.length > 0) {
    const detail = failures
      .map((f) => `[crawl:${f.url}] ${f.rule}: ${f.detail}`)
      .join('\n\n');
    throw new Error(
      `crawl oracles violated on ${failures.length} surface state(s):\n\n${detail}`,
    );
  }

  if (process.env.CERBERUS_UPDATE_INVENTORY) return;

  // Bootstrap guard before the diff: an EMPTY committed inventory
  // means the stack was registered but never crawled exhaustively —
  // fail with the bootstrap instructions instead of one NEW-surface
  // row per visited page.
  const committed = loadInventory(stack);
  assertInventoryBootstrapped(committed, stack);
  const violations = diffInventory(
    new Set(auditedStates.keys()),
    committed,
    loadExclusions(stack),
    depth,
    stack,
  );
  expect(
    violations,
    `surface-inventory ratchet violated:\n  - ${violations.join('\n  - ')}`,
  ).toEqual([]);
});

// ---------------------------------------------------------------------------
// Per-page visit + oracles
// ---------------------------------------------------------------------------

/**
 * The declared-contract slice the oracles consume for one surface:
 * which dashboard (if any) it renders, and that dashboard's declared
 * no-data titles / error expressions.
 */
type OracleContracts = {
  dashUid: string | undefined;
  noDataDeclared: ReadonlySet<string>;
  errExprsDeclared: ReadonlySet<string>;
};

function contractsFor(
  canonical: string,
  declaredNoData: ReadonlyMap<string, Set<string>>,
  declaredErrorExprs: ReadonlyMap<string, Set<string>>,
): OracleContracts {
  const dashUid = /^\/d\/([^/?#]+)/.exec(canonical)?.[1];
  return {
    dashUid,
    noDataDeclared: (dashUid && declaredNoData.get(dashUid)) || new Set(),
    errExprsDeclared:
      (dashUid && declaredErrorExprs.get(dashUid)) || new Set<string>(),
  };
}

type CapturedDsResponse = {
  url: string;
  method: string;
  status: number;
  body: string;
  requestBody: string;
};

/**
 * Ceiling on one captured response-body read. Grafana's Drilldown apps
 * leave streaming responses open across a navigation, and `resp.text()`
 * on one of those never settles: an unbounded `Promise.all` over the
 * reads then parks the whole crawl until the test timeout, which yields
 * NO inventory and NO verdict rather than a bad one. A read that blows
 * the ceiling records the same UNREADABLE_BODY_SENTINEL a read that
 * throws already records, so the reconciler's semantics are unchanged.
 */
const WIRE_BODY_READ_TIMEOUT_MS = 15_000;

/**
 * `resp.text()` bounded by `ms` — rejects rather than hanging when the
 * response body never completes.
 */
async function readBodyWithin(resp: Response, ms: number): Promise<string> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([
      resp.text(),
      new Promise<never>((_, reject) => {
        timer = setTimeout(
          () => reject(new Error(`response body did not settle within ${ms}ms`)),
          ms,
        );
      }),
    ]);
  } finally {
    clearTimeout(timer);
  }
}

/**
 * Wire the datasource-API response capture — the same surface
 * families every existing sweep watches. Deliberately NOT all of
 * /api/: e.g. Grafana fires /api/datasources/uid/cerberus-tempo/health
 * on page loads and its Tempo plugin has no backend CheckHealth, so
 * that endpoint 404s with plugin.notImplemented by Grafana's own
 * design (see the datasource-health probe comment in
 * compose_grafana_smoke.spec.ts).
 *
 * Returns the live capture array and an async stop that detaches the
 * listener and settles every in-flight body read — each one bounded by
 * WIRE_BODY_READ_TIMEOUT_MS, so a response that never finishes cannot
 * wedge the crawl.
 */
function startWireCapture(
  page: Page,
  baseURL: string,
): { captured: CapturedDsResponse[]; stop: () => Promise<void> } {
  const captured: CapturedDsResponse[] = [];
  const captureReads: Promise<void>[] = [];
  const onResponse = (resp: Response) => {
    const u = resp.url();
    const isDsQuery = u.includes('/api/ds/query');
    if (
      !isDsQuery &&
      !u.includes('/api/dashboards/') &&
      !u.includes('/api/datasources/proxy/uid/') &&
      !(u.includes('/api/datasources/uid/') && u.includes('/resources/'))
    ) {
      return;
    }
    const status = resp.status();
    const method = resp.request().method();
    const requestBody = resp.request().postData() ?? '';
    captureReads.push(
      (async () => {
        let body = '';
        // Read bodies for failures always, and for ds/query 2xx too
        // (the tunneled-error oracle needs them).
        if (status < 200 || status > 299 || isDsQuery) {
          try {
            body = await readBodyWithin(resp, WIRE_BODY_READ_TIMEOUT_MS);
          } catch {
            body = UNREADABLE_BODY_SENTINEL;
          }
        }
        captured.push({
          url: u.startsWith(baseURL) ? u.slice(baseURL.length) : u,
          method,
          status,
          body,
          requestBody,
        });
      })(),
    );
  };
  page.on('response', onResponse);
  return {
    captured,
    stop: async () => {
      page.off('response', onResponse);
      await Promise.all(captureReads);
    },
  };
}

type FailFn = (rule: string, detail: string) => void;

/**
 * Oracles 2a + 2b over a settled wire capture: non-2xx on the
 * datasource API families, and tunneled per-target errors in 2xx
 * ds/query bodies. Sanctioned only via declared-error contracts.
 *
 * Returns how many non-2xx were reconciled as Traces-Drilldown init-race
 * artifacts. Each of those ALSO reaches the browser as an anonymous "Failed to
 * load resource: … status of 400" console line, which oracle 1 would otherwise
 * report as a fresh failure — the wire and console oracles look at the same
 * response through two windows. The count is the budget oracle 1 spends
 * resolving those twins; see resolveInitRaceConsoleTwins.
 */
function evaluateWireOracles(
  captured: ReadonlyArray<CapturedDsResponse>,
  contracts: OracleContracts,
  fail: FailFn,
): number {
  let reconciledInitRace = 0;
  // Signatures of every ds/query request that ultimately succeeded
  // (2xx) in this capture window. A non-2xx whose signature lands here
  // was a Grafana-aborted, superseded in-flight request — last-write-
  // wins reconciliation, mirroring what the DOM oracle already asserts
  // (it only inspects the final rendered panel state). This is NOT an
  // escape hatch: a genuinely-broken query never produces a 2xx sibling,
  // so it still fails loudly. See succeededDsQuerySignatures in lib.ts.
  const succeededSigs = succeededDsQuerySignatures(captured);

  // Oracle 2a — non-2xx on the datasource API families. Sanctioned
  // only when every query in the failing ds/query request is a
  // declared-error panel target on this dashboard.
  for (const resp of captured) {
    if (resp.status >= 200 && resp.status <= 299) continue;
    if (
      resp.url.includes('/api/ds/query') &&
      requestFullyDeclaredError(resp.requestBody, contracts.errExprsDeclared)
    ) {
      continue;
    }
    if (isSupersededDsQueryFailure(resp, succeededSigs)) {
      continue;
    }
    // The Traces Drilldown app's primarySignal-init race transiently
    // forwards a dangling-operand TraceQL (`{ && …} | rate()`) that
    // cerberus correctly 400s (reference Tempo rejects the identical
    // syntax error). Distinct expr → no 2xx sibling, so the
    // supersession reconciler above can't catch it; this one keys on
    // the malformed shape itself. Covers both transports the app uses:
    // the ds/query POST and the datasource-proxy /api/search GET.
    // See isTransientInitRaceFailure.
    if (isTransientInitRaceFailure({ ...resp, responseBody: resp.body })) {
      reconciledInitRace++;
      continue;
    }
    fail(
      'http-non-2xx',
      `${resp.method} ${resp.url} → ${resp.status}\n  body: ${truncate(resp.body, 600)}`,
    );
  }

  // Oracle 2b — tunneled per-target errors in 2xx ds/query bodies.
  for (const resp of captured) {
    if (!resp.url.includes('/api/ds/query')) continue;
    if (resp.status < 200 || resp.status > 299) continue;
    let parsed: { results?: Record<string, { error?: string }> };
    try {
      parsed = JSON.parse(resp.body) as typeof parsed;
    } catch {
      continue; // streamed/chunked ds/query bodies have no JSON envelope
    }
    const refToExpr = refIdToExpr(resp.requestBody);
    for (const [refId, target] of Object.entries(parsed.results ?? {})) {
      if (!target || typeof target.error !== 'string' || target.error === '') {
        continue;
      }
      const expr = refToExpr.get(refId) ?? '';
      if (expr !== '' && contracts.errExprsDeclared.has(expr)) continue;
      fail(
        'ds-query-tunneled-error',
        `refId=${refId} url=${resp.url}\n  error: ${truncate(target.error, 600)}`,
      );
    }
  }

  return reconciledInitRace;
}

/**
 * Oracles 3 + 4 over the page's current DOM: visible role=alert
 * banners, page-level crash signatures, and the panel tri-state.
 */
async function evaluateDomOracles(
  page: Page,
  contracts: OracleContracts,
  concrete: string,
  fail: FailFn,
): Promise<void> {
  // Oracle 4a — VISIBLE role=alert banners with error-class text
  // (Grafana pre-mounts hidden alert skeletons on some pages; see
  // collectVisibleAlertBanners).
  const banners = await collectVisibleAlertBanners(page);
  for (const banner of banners) {
    if (ALERT_ERROR_PATTERNS.some((re) => re.test(banner))) {
      fail(
        'role-alert-banner',
        `role=alert banner with error text: ${truncate(banner, 400)}`,
      );
    }
  }

  // Oracle 4b — page-level crash signatures.
  const bodyText = await page
    .locator('body')
    .innerText({ timeout: 10_000 })
    .catch(() => '');
  for (const re of PAGE_CRASH_PATTERNS) {
    const m = re.exec(bodyText);
    if (m) {
      fail(
        'page-crash-banner',
        `page body carries crash signature ${re}: …${truncate(
          bodyText.slice(Math.max(0, m.index - 80), m.index + 160),
          300,
        )}…`,
      );
    }
  }

  // Oracle 3 — panel tri-state. Every "No data" render must be
  // covered by a declared 'empty' / 'error:*' contract.
  const noDataPanels = await collectNoDataPanels(page);
  for (const title of noDataPanels) {
    if (contracts.noDataDeclared.has(title)) continue;
    fail(
      'panel-no-data-undeclared',
      `panel ${JSON.stringify(title)} rendered "No data" with no cerberus.expect declaration ` +
        `(dashboard=${contracts.dashUid ?? '<not a dashboard>'}, url=${concrete}) — `
        + `fix the bug at the source (cerberus code, seed, dashboard, or panel), or declare the contract on a showcase panel`,
    );
  }
}

async function visitAndAudit(
  lease: PageLease,
  baseURL: string,
  entry: QueueEntry,
  declaredNoData: ReadonlyMap<string, Set<string>>,
  declaredErrorExprs: ReadonlyMap<string, Set<string>>,
): Promise<{ harvested: string[]; pageFailures: CrawlFailure[] }> {
  const pageFailures: CrawlFailure[] = [];
  const fail: FailFn = (rule, detail) =>
    pageFailures.push({ url: entry.canonical, rule, detail });

  // Declared cerberus.expect contracts this surface renders under.
  const contracts = contractsFor(
    entry.canonical,
    declaredNoData,
    declaredErrorExprs,
  );

  const page = await lease.acquire();
  const { messages: consoleErrors, stop: stopConsole } =
    await captureConsoleErrors(page);
  const wire = startWireCapture(page, baseURL);

  let harvested: string[] = [];
  try {
    lease.noteNavigation();
    await gotoCold(page, `${baseURL}${entry.concrete}`);
    await tolerateRepaintFlicker(page, { settleMs: 600, timeoutMs: 45_000 });

    harvested = await harvestLinks(page);

    await evaluateDomOracles(page, contracts, entry.concrete, fail);
  } catch (err) {
    fail(
      'navigation-threw',
      `goto(${entry.concrete}) threw: ${(err as Error).message}`,
    );
  } finally {
    stopConsole();
  }
  await wire.stop();
  const reconciledInitRace = evaluateWireOracles(wire.captured, contracts, fail);

  // Oracle 1 — console errors. Zero, with no noise filter beyond the
  // browser twins oracle 2a already accounted for (see the file header
  // for the escalation path if a Grafana bump ever makes a real filter
  // unavoidable).
  const reportableErrors = resolveInitRaceConsoleTwins(
    consoleErrors,
    reconciledInitRace,
  );
  if (reportableErrors.length > 0) {
    fail(
      'console-error',
      `${reportableErrors.length} console error(s):\n${reportableErrors
        .map((m) => `  - ${truncate(m, 400)}`)
        .join('\n')}`,
    );
  }

  return { harvested, pageFailures };
}

// ---------------------------------------------------------------------------
// Interaction sweep — see interactions.ts for the discovery/planning
// engine and docs/test-strategy.md for the bounding rules.
// ---------------------------------------------------------------------------

type InteractionSweepResult = {
  /** URL-encoding deviations → first-class surfaces to enqueue. */
  discovered: Array<QueueEntry & { leanRepresentative: boolean }>;
  /**
   * Non-URL deviations audited in place, keyed by the
   * `<canonical>#<control>=<value>` state notation → concrete URL
   * the gesture ran against, plus the lean-representative flag for
   * the inventory's lean marking.
   */
  inPlaceStates: Map<string, { concrete: string; leanRepresentative: boolean }>;
  failures: CrawlFailure[];
};

/**
 * The Traces Drilldown explore surface is the one place the adhoc
 * "+ add filter" bar mounts only after a primarySignal-init race (see
 * settleAdhocFilterBar). Gate the extra settle to it so no other
 * surface pays the wait; the Metrics / Logs Drilldown adhoc bars have
 * no such race and are already discovered within the normal settle.
 */
const TRACES_DRILLDOWN_EXPLORE_CANONICAL =
  '/a/grafana-exploretraces-app/explore';

function needsAdhocBarSettle(canonical: string): boolean {
  return canonical === TRACES_DRILLDOWN_EXPLORE_CANONICAL;
}

/**
 * Sweep one visited surface's interactive controls.
 *
 * Every planned interaction runs against a FRESH navigation of the
 * surface's concrete URL (deterministic provenance: state = surface
 * default + exactly one control deviation; no gesture-order
 * coupling). The capture window opens AFTER the page settles, so a
 * boot-time failure stays attributed to the base surface (the base
 * visit already audited it) and the interaction state only owns what
 * the gesture caused.
 *
 * Post-gesture, the page URL decides the state's identity:
 *   - canonicalizes to a DIFFERENT in-scope surface → the deviation
 *     is URL-encoded; it is returned as a first-class surface and the
 *     BFS visits it fresh with the full oracle set (captures from the
 *     gesture itself are discarded — the fresh visit owns them).
 *   - same canonical (or out of scope) → the deviation is in-page
 *     state; the full oracle set evaluates right here and the state
 *     pins into the inventory as `<canonical>#<control>=<value>`.
 */
async function sweepInteractions(
  lease: PageLease,
  baseURL: string,
  entry: QueueEntry,
  scope: ScopeRules,
  representativeOnly: boolean,
  declaredNoData: ReadonlyMap<string, Set<string>>,
  declaredErrorExprs: ReadonlyMap<string, Set<string>>,
): Promise<InteractionSweepResult> {
  const failures: CrawlFailure[] = [];
  const discovered: InteractionSweepResult['discovered'] = [];
  const inPlaceStates: InteractionSweepResult['inPlaceStates'] = new Map();
  const contracts = contractsFor(
    entry.canonical,
    declaredNoData,
    declaredErrorExprs,
  );

  // Discovery pass on a fresh navigation (the base visit's link
  // harvest left the mega menu open, which would occlude controls).
  let plan: PlannedInteraction[];
  try {
    const page = await lease.acquire();
    lease.noteNavigation();
    await gotoCold(page, `${baseURL}${entry.concrete}`);
    await tolerateRepaintFlicker(page, { settleMs: 500, timeoutMs: 30_000 });
    if (needsAdhocBarSettle(entry.canonical)) await settleAdhocFilterBar(page);
    const controls = await discoverControls(page);
    const fullPlan = planInteractions(
      controls,
      pinnedStructuralParamCount(entry.canonical),
    );
    plan = representativeOnly
      ? fullPlan.filter((p) => p.leanRepresentative)
      : fullPlan;
  } catch (err) {
    failures.push({
      url: entry.canonical,
      rule: 'interaction-discovery-failed',
      detail: (err as Error).message,
    });
    return { discovered, inPlaceStates, failures };
  }

  for (const planned of plan) {
    const stateName = `${planned.control.key}=${planned.stateValue}`;
    const stateKey = interactionStateKey(
      entry.canonical,
      planned.control,
      planned.stateValue,
    );
    const fail: FailFn = (rule, detail) =>
      failures.push({ url: stateKey, rule, detail });

    const page = await lease.acquire();
    try {
      lease.noteNavigation();
      await gotoCold(page, `${baseURL}${entry.concrete}`);
      await tolerateRepaintFlicker(page, { settleMs: 500, timeoutMs: 30_000 });
      if (needsAdhocBarSettle(entry.canonical)) await settleAdhocFilterBar(page);
    } catch (err) {
      fail('navigation-threw', `goto(${entry.concrete}) threw: ${(err as Error).message}`);
      continue;
    }

    const { messages: consoleErrors, stop: stopConsole } =
      await captureConsoleErrors(page);
    const wire = startWireCapture(page, baseURL);
    let drove = true;
    try {
      await driveInteraction(page, planned);
    } catch (err) {
      drove = false;
      fail(
        'interaction-drive-failed',
        `driving ${planned.control.kind}:${stateName} threw: ${(err as Error).message}`,
      );
    }
    if (drove) {
      await tolerateRepaintFlicker(page, { settleMs: 500, timeoutMs: 20_000 });
      // Close any select menu the gesture left open so the DOM
      // oracles see the page, not the dropdown overlay.
      await page.keyboard.press('Escape').catch(() => {});
    }
    stopConsole();
    await wire.stop();
    if (!drove) continue;

    const post = canonicalTarget(page.url(), baseURL, scope);
    if (post !== null && post.canonical !== entry.canonical) {
      // URL-encoded deviation → first-class surface; the fresh BFS
      // visit owns its oracles.
      const postURL = new URL(page.url());
      discovered.push({
        canonical: post.canonical,
        concrete: `${postURL.pathname}${postURL.search}`,
        via: `${entry.canonical} (interaction ${stateName})`,
        leanRepresentative: planned.leanRepresentative,
      });
      continue;
    }

    // In-place deviation → full oracle set, keyed by the state
    // notation, pinned into the inventory.
    const reconciledInitRace = evaluateWireOracles(
      wire.captured,
      contracts,
      fail,
    );
    await evaluateDomOracles(page, contracts, entry.concrete, fail);
    const reportableErrors = resolveInitRaceConsoleTwins(
      consoleErrors,
      reconciledInitRace,
    );
    if (reportableErrors.length > 0) {
      fail(
        'console-error',
        `${reportableErrors.length} console error(s):\n${reportableErrors
          .map((m) => `  - ${truncate(m, 400)}`)
          .join('\n')}`,
      );
    }
    inPlaceStates.set(stateKey, {
      concrete: entry.concrete,
      leanRepresentative: planned.leanRepresentative,
    });
  }

  return { discovered, inPlaceStates, failures };
}

/**
 * True iff the ds/query request body contains ≥1 query and EVERY
 * query expression is a declared-error panel target. Only then is a
 * non-2xx response the declared, showcased outcome.
 */
function requestFullyDeclaredError(
  requestBody: string,
  declared: ReadonlySet<string>,
): boolean {
  if (declared.size === 0) return false;
  const exprs = [...refIdToExpr(requestBody).values()];
  return exprs.length > 0 && exprs.every((e) => e !== '' && declared.has(e));
}

/**
 * Collect the titles of panels currently rendering Grafana's
 * "No data" placeholder. Title resolution walks up from the "No
 * data" node to the panel container and reads the panel-header
 * testid (`data-testid Panel header <title>` — Grafana's
 * @grafana/e2e-selectors convention, same one
 * compose_grafana_smoke.spec.ts keys on).
 */
async function collectNoDataPanels(page: Page): Promise<string[]> {
  return await page.evaluate(() => {
    const out: string[] = [];
    const isNoData = (el: Element) =>
      (el.textContent ?? '').trim() === 'No data';
    const candidates = [
      ...document.querySelectorAll(
        '[data-testid="data-testid Panel data error message"]',
      ),
      ...[...document.querySelectorAll('div, span, p')].filter(isNoData),
    ];
    const seen = new Set<Element>();
    for (const el of candidates) {
      if (!isNoData(el)) continue;
      const panel =
        el.closest('[data-viz-panel-key]') ??
        el.closest('section[data-testid^="data-testid Panel"]') ??
        el.closest('.panel-container');
      if (!panel || seen.has(panel)) continue;
      seen.add(panel);
      const header = panel.querySelector('[data-testid^="data-testid Panel header"]');
      const headerTestId = header?.getAttribute('data-testid') ?? '';
      const title =
        headerTestId.replace(/^data-testid Panel header ?/, '') ||
        panel.querySelector('h2')?.textContent?.trim() ||
        '<untitled panel>';
      out.push(title);
    }
    return [...new Set(out)];
  });
}
