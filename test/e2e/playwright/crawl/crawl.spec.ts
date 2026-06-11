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
 * Depth doctrine (see helpers/depth.ts — depth changes STATES, never
 * RULES): at 'lean' (the per-PR gate) the crawl visits the root page,
 * the nav links harvested from it, and one representative per
 * drilldown app. At 'full' (nightly) the BFS is exhaustive up to a
 * HARD page cap that fails the run when exceeded — surface growth
 * must force a deliberate cap bump, never a silent partial crawl.
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
  type BrowserContext,
  type Page,
  type Response,
} from '@playwright/test';

import {
  captureConsoleErrors,
  describeSweepDepth,
  generateSelfTraffic,
  iterateDashboards,
  readPanelExpectation,
  sweepDepth,
  tolerateRepaintFlicker,
} from '../helpers/index.js';
import {
  ALERT_ERROR_PATTERNS,
  PAGE_CRASH_PATTERNS,
  assertInventoryBootstrapped,
  canonicalTarget,
  canonicalizeURL,
  collectVisibleAlertBanners,
  diffInventory,
  expandSiblingTabs,
  harvestLinks,
  inventoryPath,
  loadExclusions,
  loadInventory,
  marshalInventory,
  truncate,
  type ScopeRules,
  type SurfaceInventory,
} from './lib.js';
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

// Recycle the browser context every N visited pages. A single
// renderer reused across the whole full-depth crawl accumulates state
// until Chromium refuses requests with net::ERR_INSUFFICIENT_RESOURCES
// (same failure mode iterate-panel-kiosk documents at ~190
// navigations); 40 keeps a wide margin while preserving cheap
// navigation within a batch.
const CONTEXT_RECYCLE_PAGES = 40;

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

// ---------------------------------------------------------------------------
// The crawl
// ---------------------------------------------------------------------------

test('crawl: BFS over every reachable Grafana surface with universal oracles + inventory ratchet', async ({
  browser,
  request,
}, testInfo) => {
  const stack = activeStack();
  const depth = sweepDepth();
  // Budget: lean ≈ 10 pages × ~6s + 30s seed; full ≈ cap pages.
  testInfo.setTimeout(depth === 'full' ? 30 * 60_000 : 8 * 60_000);
  // eslint-disable-next-line no-console
  console.log(`crawl stack: ${stack.name} — ${describeSweepDepth(depth)}`);

  const baseURL =
    process.env.GRAFANA_URL ??
    process.env.GRAFANA_BASE_URL ??
    stack.defaultGrafanaURL;

  await generateSelfTraffic(request, SEED_TRAFFIC_SECONDS);

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
    for (const d of [...dashboards].sort((a, b) => a.uid.localeCompare(b.uid))) {
      queue.push({
        canonical: `/d/${d.uid}`,
        concrete: `/d/${d.uid}`,
        via: '<seed:dashboard>',
      });
    }
  }

  const pageCap = depth === 'full' ? stack.pageCapFull : stack.pageCapLean;
  const visited = new Map<string, string>(); // canonical → concrete navigated
  const failures: CrawlFailure[] = [];

  let context: BrowserContext | null = null;
  let page: Page | null = null;
  let pagesInContext = 0;

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

      if (page === null || pagesInContext >= CONTEXT_RECYCLE_PAGES) {
        if (context !== null) await context.close();
        context = await browser.newContext();
        page = await context.newPage();
        pagesInContext = 0;
      }
      pagesInContext++;
      visited.set(entry.canonical, entry.concrete);

      const { harvested, pageFailures } = await visitAndAudit(
        page,
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
          ([a], [b]) => a.localeCompare(b),
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
    }
  } finally {
    if (context !== null) await context.close();
  }

  // eslint-disable-next-line no-console
  console.log(
    `crawl: visited ${visited.size} surface(s) at depth=${depth} stack=${stack.name}:\n${[...visited.keys()]
      .sort()
      .map((u) => `  - ${u}`)
      .join('\n')}`,
  );

  if (failures.length > 0) {
    const detail = failures
      .map((f) => `[crawl:${f.url}] ${f.rule}: ${f.detail}`)
      .join('\n\n');
    throw new Error(
      `crawl oracles violated on ${failures.length} surface state(s):\n\n${detail}`,
    );
  }

  // -------------------------------------------------------------------------
  // Surface-inventory ratchet.
  // -------------------------------------------------------------------------
  if (process.env.CERBERUS_UPDATE_INVENTORY) {
    expect(
      depth,
      'inventory regeneration requires the exhaustive crawl: rerun with SWEEP_DEPTH=full',
    ).toBe('full');
    const inv: SurfaceInventory = {
      doc: stack.inventoryDoc,
      stack: stack.name,
      surfaces: [...visited.keys()].map((url) => ({
        url,
        lean: leanSet.has(url),
      })),
    };
    writeFileSync(inventoryPath(stack), marshalInventory(inv));
    // eslint-disable-next-line no-console
    console.log(
      `crawl: regenerated ${inventoryPath(stack)} with ${inv.surfaces.length} surface(s)`,
    );
    return;
  }

  // Bootstrap guard before the diff: an EMPTY committed inventory
  // means the stack was registered but never crawled exhaustively —
  // fail with the bootstrap instructions instead of one NEW-surface
  // row per visited page.
  const committed = loadInventory(stack);
  assertInventoryBootstrapped(committed, stack);
  const violations = diffInventory(
    new Set(visited.keys()),
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

async function visitAndAudit(
  page: Page,
  baseURL: string,
  entry: QueueEntry,
  declaredNoData: ReadonlyMap<string, Set<string>>,
  declaredErrorExprs: ReadonlyMap<string, Set<string>>,
): Promise<{ harvested: string[]; pageFailures: CrawlFailure[] }> {
  const pageFailures: CrawlFailure[] = [];
  const fail = (rule: string, detail: string) =>
    pageFailures.push({ url: entry.canonical, rule, detail });

  // Which dashboard (if any) this surface renders — keys the declared
  // cerberus.expect contracts.
  const dashUid = /^\/d\/([^/?]+)/.exec(entry.canonical)?.[1];
  const noDataDeclared = (dashUid && declaredNoData.get(dashUid)) || new Set();
  const errExprsDeclared =
    (dashUid && declaredErrorExprs.get(dashUid)) || new Set<string>();

  const { messages: consoleErrors, stop: stopConsole } =
    await captureConsoleErrors(page);

  // Datasource API capture — same surface families every existing
  // sweep watches. Deliberately NOT all of /api/: e.g. Grafana fires
  // /api/datasources/uid/cerberus-tempo/health on page loads and its
  // Tempo plugin has no backend CheckHealth, so that endpoint 404s
  // with plugin.notImplemented by Grafana's own design (see the
  // datasource-health probe comment in compose_grafana_smoke.spec.ts).
  type CapturedDsResponse = {
    url: string;
    method: string;
    status: number;
    body: string;
    requestBody: string;
  };
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
            body = await resp.text();
          } catch {
            body = '<unreadable>';
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

  let harvested: string[] = [];
  try {
    await page.goto(`${baseURL}${entry.concrete}`, {
      waitUntil: 'domcontentloaded',
      timeout: 90_000,
    });
    await tolerateRepaintFlicker(page, { settleMs: 600, timeoutMs: 45_000 });

    harvested = await harvestLinks(page);

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
      if (noDataDeclared.has(title)) continue;
      fail(
        'panel-no-data-undeclared',
        `panel ${JSON.stringify(title)} rendered "No data" with no cerberus.expect declaration ` +
          `(dashboard=${dashUid ?? '<not a dashboard>'}, url=${entry.concrete}) — `
          + `fix the bug at the source (cerberus code, seed, dashboard, or panel), or declare the contract on a showcase panel`,
      );
    }
  } catch (err) {
    fail(
      'navigation-threw',
      `goto(${entry.concrete}) threw: ${(err as Error).message}`,
    );
  } finally {
    page.off('response', onResponse);
    stopConsole();
  }
  await Promise.all(captureReads);

  // Oracle 2a — non-2xx on the datasource API families. Sanctioned
  // only when every query in the failing ds/query request is a
  // declared-error panel target on this dashboard.
  for (const resp of captured) {
    if (resp.status >= 200 && resp.status <= 299) continue;
    if (
      resp.url.includes('/api/ds/query') &&
      requestFullyDeclaredError(resp.requestBody, errExprsDeclared)
    ) {
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
      if (expr !== '' && errExprsDeclared.has(expr)) continue;
      fail(
        'ds-query-tunneled-error',
        `refId=${refId} url=${resp.url}\n  error: ${truncate(target.error, 600)}`,
      );
    }
  }

  // Oracle 1 — console errors. Zero, with no noise filter (see the
  // file header for the escalation path if a Grafana bump ever makes
  // one unavoidable).
  if (consoleErrors.length > 0) {
    fail(
      'console-error',
      `${consoleErrors.length} console error(s):\n${consoleErrors
        .map((m) => `  - ${truncate(m, 400)}`)
        .join('\n')}`,
    );
  }

  return { harvested, pageFailures };
}

/**
 * Map refId → expr/query from a ds/query request body. Returns an
 * empty map for non-JSON bodies.
 */
function refIdToExpr(requestBody: string): Map<string, string> {
  const out = new Map<string, string>();
  try {
    const parsed = JSON.parse(requestBody) as {
      queries?: Array<{ refId?: string; expr?: string; query?: string }>;
    };
    for (const q of parsed.queries ?? []) {
      const expr = (q.expr ?? q.query ?? '').trim();
      if (q.refId) out.set(q.refId, expr);
    }
  } catch {
    // fallthrough — empty map; caller treats every refId as undeclared
  }
  return out;
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
