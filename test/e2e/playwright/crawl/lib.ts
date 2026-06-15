/**
 * Grafana surface-crawler library — the stack-agnostic ENGINE.
 *
 * The crawler (crawl.spec.ts) BFS-walks the live Grafana UI from the
 * root page, harvesting same-origin links per page, and applies the
 * same universal oracle set on EVERY visited page. This module holds
 * the deterministic plumbing — URL canonicalization, scope
 * exclusions, the surface-inventory file format — so the spec stays a
 * thin driver and the rules are unit-pinnable.
 *
 * One engine, N stacks: nothing in this module (or the specs) may
 * branch on a stack name. Per-stack variation — base URL, scope
 * rules, inventory file, datasource UIDs, lint floors, lean seeds,
 * page caps — is declared as data in stacks.ts and threaded through
 * as parameters. The constants below (EXCLUDED_PATH_PATTERNS,
 * APP_BARE_PATH_ALIASES) are the shared Grafana-12.x defaults the
 * registered stack configs reference; the engine consumes them only
 * via the ScopeRules a config hands it.
 *
 * Why a crawler on top of the enumerated iterate-* specs: an off-CI
 * AI screenshot sweep (2026-06-09) found 34 unique error signatures
 * across 55 BFS-visited pages — several on surfaces NO enumerated
 * spec visits (drilldown-app tabs, logs-drilldown service pages,
 * traces-drilldown comparison view). Every find decomposed into a
 * deterministic signal once named; the crawler carries those signals
 * in CI forever, and the committed per-stack surface inventory
 * (grafana-surface-inventory.<stack>.json) makes coverage growth
 * explicit and shrink impossible.
 *
 * Determinism: the visited-set converges because canonicalization
 * strips volatile params (time ranges, session-state blobs) and
 * parameterizes dynamic path segments (service names, trace ids).
 * Two crawls of the same provisioned stack + pinned Grafana version
 * yield the same canonical set.
 */

import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import type { Page } from '@playwright/test';
// Type-only (erased at runtime) — stacks.ts value-imports the scope
// constants below, so a value import here would be a cycle.
import type { CrawlStackConfig } from './stacks.js';

/**
 * The scope slice of a stack config the canonicalizer consumes:
 * which route families are out of crawl scope, which bare app paths
 * alias to an entry route, and which query params are STRUCTURAL —
 * i.e. encode a consumption mode rather than session state.
 */
export type ScopeRules = {
  excludedPathPatterns: ReadonlyArray<RegExp>;
  appBarePathAliases: ReadonlyMap<string, string>;
  structuralParamRules: ReadonlyArray<StructuralParamRule>;
};

/**
 * A query param that is part of a surface's IDENTITY rather than its
 * session state. The default canonicalization doctrine strips every
 * query param ("a state of a surface is not a new surface") — but the
 * 2026-06-10 maintainer find proved the doctrine over-broad for a
 * narrow param class: the Traces Drilldown breakdown `var-groupBy`
 * param selects WHICH query family the page fires (`| rate()
 * by(kind)` vs `by(resource.service.name)`), and the groupBy=kind
 * state 422'd while the crawler — which only ever saw the default —
 * stayed green. Params that change the queries a page issues are
 * distinct consumption modes, hence distinct surfaces.
 *
 * Two retention modes mirror the path rules:
 *   - 'enumerate' — low-cardinality structural params (actionView
 *     tabs, groupBy attribute, metric type): every value keys its own
 *     surface. `defaultValue` names the value the app writes on a
 *     cold boot; it is DROPPED from the canonical so the default
 *     state stays keyed by the bare surface (the app rewriting its
 *     defaults into the URL must not re-key the surface).
 *   - 'parameterize' — high-cardinality structural params (the
 *     metrics-drilldown `metric` name): the value collapses to
 *     `{param}`, one representative surface family — the same
 *     doctrine as the `{service}` path segment.
 */
export type StructuralParamRule = {
  /** Canonical-path family the rule applies to (post path-rewrite). */
  pathPattern: RegExp;
  /** Exact query param name. */
  param: string;
  mode: 'enumerate' | 'parameterize';
  /** 'enumerate' only: the app's cold-boot value, dropped from keys. */
  defaultValue?: string;
};

/**
 * Grafana-12.x structural params, verified live against
 * grafana/grafana:12.2.9 (2026-06-11) by driving each control and
 * reading the URL the app wrote:
 *
 *   - Traces Drilldown (`grafana-exploretraces-app`): `actionView`
 *     (Breakdown | Service structure | Comparison | Traces tabs,
 *     boot value `breakdown`), `var-metric` (rate | errors |
 *     duration, boot `rate`), `var-groupBy` (the breakdown attribute,
 *     boot `resource.service.name` — `kind` is the maintainer-found
 *     422), `var-primarySignal` (Root spans | All spans, boot
 *     `nestedSetParent<0`).
 *   - Metrics Drilldown (`grafana-metricsdrilldown-app`): `metric`
 *     (the selected metric NAME — one per series family in the
 *     stack, high-cardinality → parameterize), `actionView`
 *     (breakdown | related, written as `breakdown` on metric select),
 *     `var-groupby` (boot `$__all`).
 *   - Logs Drilldown service pages (`grafana-lokiexplore-app`):
 *     `visualizationType` (logs | table | json — JSON-string-quoted
 *     in the URL, boot `"logs"`), `sortOrder` (boot `"Descending"`).
 *
 * Time params (`from`/`to`), filters (`var-filters`), display state
 * (`displayedFields`, `urlColumns`, `patterns`, …) stay stripped —
 * they are session state, owned by the iterate-* sweeps.
 */
export const STRUCTURAL_PARAM_RULES: ReadonlyArray<StructuralParamRule> = [
  {
    pathPattern: /^\/a\/grafana-exploretraces-app\/explore$/,
    param: 'actionView',
    mode: 'enumerate',
    defaultValue: 'breakdown',
  },
  {
    pathPattern: /^\/a\/grafana-exploretraces-app\/explore$/,
    param: 'var-metric',
    mode: 'enumerate',
    defaultValue: 'rate',
  },
  {
    pathPattern: /^\/a\/grafana-exploretraces-app\/explore$/,
    param: 'var-groupBy',
    mode: 'enumerate',
    defaultValue: 'resource.service.name',
  },
  {
    pathPattern: /^\/a\/grafana-exploretraces-app\/explore$/,
    param: 'var-primarySignal',
    mode: 'enumerate',
    defaultValue: 'nestedSetParent<0',
  },
  {
    pathPattern: /^\/a\/grafana-metricsdrilldown-app\/(drilldown|trail)$/,
    param: 'metric',
    mode: 'parameterize',
  },
  {
    pathPattern: /^\/a\/grafana-metricsdrilldown-app\/(drilldown|trail)$/,
    param: 'actionView',
    mode: 'enumerate',
    defaultValue: 'breakdown',
  },
  {
    pathPattern: /^\/a\/grafana-metricsdrilldown-app\/(drilldown|trail)$/,
    param: 'var-groupby',
    mode: 'enumerate',
    defaultValue: '$__all',
  },
  {
    pathPattern: /^\/a\/grafana-lokiexplore-app\/explore\/service\/\{service\}\//,
    param: 'visualizationType',
    mode: 'enumerate',
    defaultValue: '"logs"',
  },
  {
    pathPattern: /^\/a\/grafana-lokiexplore-app\/explore\/service\/\{service\}\//,
    param: 'sortOrder',
    mode: 'enumerate',
    defaultValue: '"Descending"',
  },
];

// ---------------------------------------------------------------------------
// Scope exclusions
// ---------------------------------------------------------------------------

/**
 * Canonical paths the crawler never navigates. The crawl is strictly
 * read-only — no mutations, no auth flows, no admin surfaces. Each
 * exclusion family is structural (the route class can never be a
 * cerberus consumption surface), NOT a tolerance for a failing page:
 *
 *   - auth flows (`/login`, `/logout`, `/signup`, `/verify`) — anonymous
 *     auth is on in the stacks; driving auth would mutate session state.
 *   - admin / org / user / connections / plugin-config — Grafana
 *     management UI, no datasource traffic, write affordances.
 *   - alerting — no cerberus alerting integration is provisioned; the
 *     surface is Grafana-internal.
 *   - create / edit / save routes — write affordances (`/dashboard/new`,
 *     folder creation, panel edit). Read-only crawl.
 *   - `/d-solo/` — the single-panel render path; per-panel render
 *     coverage is owned by iterate-panel-kiosk.spec.ts (`?viewPanel`),
 *     and one /d-solo link per panel would blow the page budget with
 *     duplicate states.
 *   - `/goto/` — short-link redirector (volatile token in the path).
 *   - `/api/`, `/apis/`, `/public/`, `/swagger`, `/metrics` — not UI
 *     pages; API behaviour is asserted by the response capture on the
 *     pages that call them.
 */
export const EXCLUDED_PATH_PATTERNS: ReadonlyArray<RegExp> = [
  /^\/login(\/|$)/,
  /^\/logout(\/|$)/,
  /^\/signup(\/|$)/,
  /^\/verify(\/|$)/,
  /^\/user(\/|$)/,
  /^\/profile(\/|$)/,
  /^\/admin(\/|$)/,
  /^\/org(\/|$)/,
  /^\/connections(\/|$)/,
  /^\/plugins(\/|$)/,
  /^\/alerting(\/|$)/,
  /^\/a\/grafana-easystart-app(\/|$)/, // "connect data" onboarding app — write affordances
  /^\/dashboard\/new(\/|$)/,
  /^\/dashboard\/import(\/|$)/,
  /^\/dashboards\/folder\/new(\/|$)/,
  /^\/d-solo\//,
  /^\/goto\//,
  /^\/api(s)?\//,
  /^\/public\//,
  /^\/swagger(\/|$)/,
  /^\/metrics(\/|$)/,
  /^\/playlists(\/|$)/, // playlist play mutates view state on a timer
  /\/edit(\/|$)/,
  /\/new$/,
];

/**
 * Bare `/a/<app-id>` paths are client-side REDIRECTORS into each
 * app's entry route — Grafana's app loader boots the plugin and
 * immediately replaces the URL (verified live: both paths land on
 * the identical entry URL). They alias to the entry route so the
 * crawl visits each app exactly once.
 *
 * Why this matters beyond dedupe: the grafana-exploretraces-app
 * RE-ENTRY boot (second mount in one browser context) restores its
 * persisted filter state with an empty primary-signal and fires the
 * malformed TraceQL `{ && true}` — which reference Tempo 2.7 rejects
 * with the byte-identical "syntax error: unexpected &&" 400 cerberus
 * returns (verified against grafana/tempo:2.7.1; the upstream
 * tempo parser rejects it at parse stage). An upstream app-state
 * bug, not a cerberus surface — collapsing the redirector onto the
 * entry route keeps the crawl to one deterministic boot per app, the
 * same posture iterate-drilldown-apps.spec.ts takes (one fresh-page
 * visit per catalogue entry).
 */
export const APP_BARE_PATH_ALIASES: ReadonlyMap<string, string> = new Map([
  ['/a/grafana-lokiexplore-app', '/a/grafana-lokiexplore-app/explore'],
  ['/a/grafana-exploretraces-app', '/a/grafana-exploretraces-app/explore'],
  ['/a/grafana-metricsdrilldown-app', '/a/grafana-metricsdrilldown-app/drilldown'],
  ['/a/grafana-pyroscope-app', '/a/grafana-pyroscope-app/explore'],
]);

// ---------------------------------------------------------------------------
// Canonicalization
// ---------------------------------------------------------------------------

/** 16+ hex chars — trace ids (32), span ids (16), short-url tokens. */
const HEX_SEGMENT = /^[0-9a-f]{16,64}$/;
/** UUID path segments (Grafana folder/library-panel uids are NOT UUIDs). */
const UUID_SEGMENT =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

/**
 * Canonicalize a harvested href against the Grafana base URL,
 * returning both the canonical surface key and the concrete URL the
 * crawler should navigate for it. Returns null when the URL is out
 * of crawl scope (different origin, excluded route family, non-HTTP
 * scheme).
 *
 * The canonical key is the rewritten path plus the surface's
 * STRUCTURAL params only (see StructuralParamRule). Grafana routes
 * surfaces by path; query strings mostly carry volatile or session
 * state — time windows (`from`/`to`/`refresh`), org context,
 * kiosk / view-panel modes, Explore state blobs (`panes`/`left`/
 * `right`), filter selections, and the drilldown apps' serialized UI
 * state (`patterns`, `displayedFields`, `urlColumns`, `layout`, …).
 * The first full crawl demonstrated why a broad param-retention list
 * can't work: the logs-drilldown service page alone produced four
 * param-permutations of one surface. A state of a surface is not a
 * new surface — UNLESS the param changes which queries the page
 * fires: those params (the structural rules) join the canonical key,
 * either verbatim (`actionView=comparison`, low-cardinality) or
 * parameterized (`metric={metric}`, high-cardinality), with the
 * app's cold-boot default dropped so the default state keys the bare
 * surface.
 *
 * Path rules, in order:
 *   1. Same-origin only; hash dropped.
 *   2. Excluded route families → null (checked on the RAW path,
 *      before any rewriting, so `/d/<uid>/edit` can't slip past via
 *      the slug-drop; and re-checked after, so no rewrite can map
 *      into an excluded family).
 *   3. `/explore...` collapses to `/explore` — every Explore
 *      permutation is one surface.
 *   4. Bare `/a/<app-id>` redirector paths alias to the app's entry
 *      route (see APP_BARE_PATH_ALIASES) — the concrete URL becomes
 *      the entry route too.
 *   5. `/d/<uid>/<slug>` drops the cosmetic slug → `/d/<uid>`.
 *   6. Dynamic path segments parameterize (the canonical is a
 *      surface FAMILY; the concrete first-seen instance is what the
 *      crawler navigates):
 *        - `/dashboards/f/<folder-uid>` → `{folder}` (provisioned
 *          folder uids are minted at stack-boot time — pinning a
 *          concrete one would break the inventory on every rebuild);
 *        - the segment after `service` (logs-drilldown service
 *          pages) → `{service}`;
 *        - 16+-hex and UUID segments → `{hex}` (trace / span ids).
 */
export function canonicalTarget(
  href: string,
  baseURL: string,
  scope: ScopeRules,
): { canonical: string; concrete: string } | null {
  let url: URL;
  let base: URL;
  try {
    base = new URL(baseURL);
    url = new URL(href, baseURL);
  } catch {
    return null;
  }
  if (url.origin !== base.origin) return null;
  if (url.protocol !== 'http:' && url.protocol !== 'https:') return null;

  let path = url.pathname.replace(/\/+$/, '');
  if (path === '') path = '/';

  // Exclusions on the raw path, before any rewriting.
  for (const re of scope.excludedPathPatterns) {
    if (re.test(path)) return null;
  }

  // Explore collapses wholesale — session state is not a surface.
  if (path === '/explore' || path.startsWith('/explore/')) {
    return { canonical: '/explore', concrete: '/explore' };
  }

  // Bare app redirector → entry route (canonical AND concrete).
  const alias = scope.appBarePathAliases.get(path);
  if (alias !== undefined) {
    return { canonical: alias, concrete: alias };
  }

  // /d/<uid>/<slug> → /d/<uid>.
  const dashMatch = /^\/d\/([^/]+)(?:\/.*)?$/.exec(path);
  if (dashMatch) {
    path = `/d/${dashMatch[1]}`;
  }

  // /dashboards/f/<uid>[/<slug>][/<tab>…] → /dashboards/f/{folder}[/<tab>…].
  // The folder uid is minted at provisioning time and the slug is the
  // cosmetic title; harvested hrefs carry the slugged and unslugged
  // forms interchangeably depending on which page rendered the link,
  // so both must key one surface family or the inventory flickers.
  const folderMatch = /^\/dashboards\/f\/([^/]+)(?:\/[^/]+)?(\/.*)?$/.exec(
    path,
  );
  if (folderMatch) {
    path = `/dashboards/f/{folder}${folderMatch[2] ?? ''}`;
  }

  // Parameterize dynamic segments.
  const segments = path.split('/');
  for (let i = 0; i < segments.length; i++) {
    const seg = segments[i] ?? '';
    if (HEX_SEGMENT.test(seg) || UUID_SEGMENT.test(seg)) {
      segments[i] = '{hex}';
      continue;
    }
    // Logs-drilldown service pages: the segment AFTER 'service' is the
    // service name harvested from live data; the segment after
    // 'label' is a label NAME from live data (the labels-tab drill).
    // Both parameterize to one representative per route family.
    if (i >= 1 && segments[i - 1] === 'service') {
      segments[i] = '{service}';
      continue;
    }
    if (i >= 1 && segments[i - 1] === 'label') {
      segments[i] = '{label}';
    }
  }
  path = segments.join('/');
  if (path === '') path = '/';

  // Re-check after rewriting — no rewrite may land in-scope a URL
  // whose canonical form is excluded.
  for (const re of scope.excludedPathPatterns) {
    if (re.test(path)) return null;
  }

  // Structural params join the canonical key (sorted by param name
  // so the key is order-independent of the app's URL writing). Every
  // other param is stripped from the canonical only.
  const retained: string[] = [];
  for (const rule of scope.structuralParamRules) {
    if (!rule.pathPattern.test(path)) continue;
    const value = url.searchParams.get(rule.param);
    if (value === null || value === '') continue;
    if (rule.mode === 'enumerate') {
      if (value === rule.defaultValue) continue;
      retained.push(`${rule.param}=${value}`);
    } else {
      retained.push(`${rule.param}={${rule.param}}`);
    }
  }
  retained.sort();
  const canonical =
    retained.length > 0 ? `${path}?${retained.join('&')}` : path;

  // Concrete: the raw path + raw query (parameterized canonicals
  // aren't navigable, and drilldown-app hrefs carry var-* state the
  // page needs to boot into the linked view — navigating the
  // logs-drilldown service page without its var-filters leaves the
  // tab bar unmounted). Only the CANONICAL key reduces the query.
  return { canonical, concrete: `${url.pathname}${url.search}` };
}

/**
 * Number of structural params pinned in a canonical surface key —
 * the interaction sweep's pairwise depth bound (see
 * interactions.ts): 0 pinned → full single-control enumeration;
 * 1 pinned → representative combos only (each interaction there
 * forms a structural-param PAIR); ≥2 pinned → terminal, visited
 * with the universal oracles but not expanded further.
 */
export function pinnedStructuralParamCount(canonical: string): number {
  const q = canonical.split('?', 2)[1];
  if (q === undefined || q === '') return 0;
  return q.split('&').length;
}

/**
 * Known sibling-route families: when the crawl discovers one member,
 * it deterministically enqueues the rest — tab links inside scenes
 * apps mount only after the page's first query wave resolves, so
 * harvest-only discovery of them is timing-sensitive (run-to-run
 * inventory flicker), while the route family itself is fixed by the
 * pinned app version.
 *
 * Today: the logs-drilldown service page tabs (verified live against
 * grafana-lokiexplore-app on grafana/grafana:12.2.9).
 */
export function expandSiblingTabs(
  canonical: string,
  concrete: string,
): Array<{ canonical: string; concrete: string }> {
  const m = /^(\/a\/grafana-lokiexplore-app\/explore\/service\/\{service\})\/([a-z]+)$/.exec(
    canonical,
  );
  if (!m) return [];
  const tabs = ['logs', 'labels', 'fields', 'patterns'];
  const current = m[2] ?? '';
  const [concretePath, concreteQuery = ''] = concrete.split('?', 2);
  const concreteBase = (concretePath ?? '').replace(/\/[a-z]+$/, '');
  return tabs
    .filter((t) => t !== current)
    .map((t) => ({
      canonical: `${m[1]}/${t}`,
      concrete: `${concreteBase}/${t}${concreteQuery ? `?${concreteQuery}` : ''}`,
    }));
}

/** Canonical-only convenience wrapper (the pin tests use this). */
export function canonicalizeURL(
  href: string,
  baseURL: string,
  scope: ScopeRules,
): string | null {
  return canonicalTarget(href, baseURL, scope)?.canonical ?? null;
}

// ---------------------------------------------------------------------------
// Link harvesting
// ---------------------------------------------------------------------------

/**
 * Harvest every same-document `<a href>` currently in the DOM,
 * returning raw href strings (canonicalization is the caller's job).
 *
 * Before harvesting, the helper opens Grafana's mega menu if the
 * toggle is present — the nav tree only mounts its links while the
 * menu is open (Grafana 12.x renders the collapsed menu without
 * anchors). Opening the menu is a read-only UI gesture.
 */
export async function harvestLinks(page: Page): Promise<string[]> {
  // Open the mega menu so nav links mount. The toggle carries
  // aria-label "Open menu" when closed; if it's absent or already
  // open ("Close menu"), proceed with whatever the DOM has.
  const toggle = page.locator('[aria-label="Open menu"]').first();
  if ((await toggle.count()) > 0) {
    await toggle.click({ timeout: 5_000 }).catch(() => {
      // A nav overlay can obscure the toggle on narrow layouts; links
      // already in the DOM are still harvested below.
    });
    await page.waitForTimeout(300);
  }
  const hrefs = await page
    .locator('a[href]')
    .evaluateAll((nodes) =>
      nodes
        .map((n) => n.getAttribute('href') ?? '')
        .filter((h) => h.length > 0),
    );
  return hrefs;
}

// ---------------------------------------------------------------------------
// Page-crash banner signatures
// ---------------------------------------------------------------------------

/**
 * Substrings on a `role="alert"` banner that classify it as an
 * error-state surface. Mirrors ALERT_ERROR_PATTERNS in
 * iterate-panel-kiosk.spec.ts — kept in lockstep so a banner that
 * fails the kiosk sweep also fails the crawl.
 */
export const ALERT_ERROR_PATTERNS: ReadonlyArray<RegExp> = [
  /error/i,
  /failed/i,
  /illegal wiretype/i,
  /plugin\.downstream/i,
  /unable to/i,
];

/**
 * Read every VISIBLE `role="alert"` banner's text. The crawler can't
 * reuse helpers/dom.ts's captureRoleAlertBanners verbatim: Grafana
 * pre-mounts a hidden alert skeleton on some pages (`display:none;
 * opacity:0` — e.g. an "Unknown error" toast template on /explore,
 * found by the first crawl run) that never renders to the user.
 * Visibility-filtering is a correctness fix for the oracle (assert
 * what a user can see), not a tolerance: a banner that actually
 * displays still fails the crawl.
 */
export async function collectVisibleAlertBanners(
  page: Page,
): Promise<string[]> {
  return await page
    .locator('[role="alert"]')
    .evaluateAll((nodes) =>
      nodes
        .filter((n) =>
          (n as HTMLElement).checkVisibility
            ? (n as HTMLElement).checkVisibility({
                checkOpacity: true,
                checkVisibilityCSS: true,
              })
            : true,
        )
        .map((n) => (n.textContent ?? '').trim())
        .filter((s) => s.length > 0),
    );
}

/**
 * Page-level crash signatures: React error boundaries and Grafana's
 * top-level failure pages render one of these phrases in the body.
 * Any visited page whose text content carries one fails the crawl
 * loudly — these are whole-page failures, not per-panel states.
 */
export const PAGE_CRASH_PATTERNS: ReadonlyArray<RegExp> = [
  /an unexpected error happened/i,
  /application error/i,
  /something went wrong/i,
  /if you keep getting this error/i,
];

// ---------------------------------------------------------------------------
// Surface inventory (the ratchet)
// ---------------------------------------------------------------------------

/**
 * One pinned crawl surface. `url` is the canonical form produced by
 * canonicalizeURL. `lean: true` marks the surfaces the lean (per-PR)
 * crawl visits: the root page, the nav links harvested from it, and
 * one representative per drilldown app. Every surface is visited at
 * 'full' depth.
 */
export type InventorySurface = {
  url: string;
  lean: boolean;
};

export type SurfaceInventory = {
  /** Free-text header documenting the regen convention. */
  doc: string;
  /** Stack the inventory pins — must equal the stack config's name. */
  stack: string;
  surfaces: InventorySurface[];
};

export type SurfaceExclusion = {
  url: string;
  rationale: string;
};

export type SurfaceExclusions = {
  doc: string;
  exclusions: SurfaceExclusion[];
};

/** The slice of a stack config the inventory plumbing consumes. */
type StackFiles = Pick<
  CrawlStackConfig,
  'name' | 'inventoryFilename' | 'exclusionsFilename'
>;

export function inventoryPath(stack: StackFiles): string {
  return join(__dirname, stack.inventoryFilename);
}

export function exclusionsPath(stack: StackFiles): string {
  return join(__dirname, stack.exclusionsFilename);
}

export function loadInventory(stack: StackFiles): SurfaceInventory {
  const raw = readFileSync(inventoryPath(stack), 'utf8');
  const parsed = JSON.parse(raw) as SurfaceInventory;
  if (!Array.isArray(parsed.surfaces)) {
    throw new Error(
      `loadInventory: ${stack.inventoryFilename} has no surfaces[] array`,
    );
  }
  if (parsed.stack !== stack.name) {
    throw new Error(
      `loadInventory: ${stack.inventoryFilename} pins stack ${JSON.stringify(parsed.stack)} ` +
        `but the active config is ${JSON.stringify(stack.name)} — the config points at the wrong file`,
    );
  }
  return parsed;
}

export function loadExclusions(stack: StackFiles): SurfaceExclusions {
  const raw = readFileSync(exclusionsPath(stack), 'utf8');
  const parsed = JSON.parse(raw) as SurfaceExclusions;
  if (!Array.isArray(parsed.exclusions)) {
    throw new Error(
      `loadExclusions: ${stack.exclusionsFilename} has no exclusions[] array`,
    );
  }
  return parsed;
}

/**
 * Bootstrap guard. A stack registers with an EMPTY committed
 * inventory (the bootstrap convention: no inventory can exist before
 * the stack's first exhaustive crawl). Empty is ONLY legal on the
 * bootstrap run itself (CERBERUS_UPDATE_INVENTORY=1) — every other
 * run fails loudly here, so the bootstrap state can't silently
 * become permanent: the stack's CI lane stays red until the
 * regenerated inventory is committed.
 */
export function assertInventoryBootstrapped(
  inventory: SurfaceInventory,
  stack: StackFiles,
): void {
  if (inventory.surfaces.length > 0) return;
  if (process.env.CERBERUS_UPDATE_INVENTORY) return;
  throw new Error(
    `stack ${JSON.stringify(stack.name)} has an EMPTY surface inventory (${stack.inventoryFilename}) — ` +
      `it is registered but not yet bootstrapped. Bootstrap against a healthy ${stack.name} stack with:\n` +
      `  CERBERUS_UPDATE_INVENTORY=1 SWEEP_DEPTH=full CRAWL_STACK=${stack.name} npx playwright test crawl/crawl.spec.ts\n` +
      `then commit the regenerated ${stack.inventoryFilename}. This error fires on EVERY run until ` +
      `the inventory lands — the empty state is a bootstrap convention, never a steady state.`,
  );
}

/**
 * Render the canonical serialized form of an inventory — sorted by
 * URL, two-space indent, trailing newline — so regeneration is
 * byte-for-byte reproducible (mirrors test/inventory/'s
 * MarshalInventory convention).
 */
export function marshalInventory(inv: SurfaceInventory): string {
  const sorted = [...inv.surfaces].sort((a, b) => a.url.localeCompare(b.url));
  return `${JSON.stringify({ doc: inv.doc, stack: inv.stack, surfaces: sorted }, null, 2)}\n`;
}

/**
 * Compare a crawl's visited set against the committed inventory for
 * the active depth. Returns human-readable violations (empty = the
 * ratchet holds):
 *
 *   - a visited surface missing from the inventory → coverage GREW
 *     (e.g. a Grafana bump added an app page); regenerate the
 *     inventory deliberately via CERBERUS_UPDATE_INVENTORY=1.
 *   - an inventory surface (for this depth) the crawl didn't visit →
 *     coverage SHRANK; that is a regression to fix, never a row to
 *     delete silently.
 *   - a surface listed in both the inventory and the exclusions file
 *     → stale exclusion.
 *   - an exclusion without a rationale → unsound exclusion.
 */
export function diffInventory(
  visited: ReadonlySet<string>,
  inventory: SurfaceInventory,
  exclusions: SurfaceExclusions,
  depth: 'lean' | 'full',
  stack: StackFiles,
): string[] {
  const out: string[] = [];

  const excluded = new Set<string>();
  for (const e of exclusions.exclusions) {
    if (!e.rationale || e.rationale.trim() === '') {
      out.push(
        `exclusion ${JSON.stringify(e.url)} has no rationale — every exclusion must document why the surface is genuinely uncrawlable`,
      );
    }
    if (excluded.has(e.url)) {
      out.push(`exclusion ${JSON.stringify(e.url)} is listed twice`);
    }
    excluded.add(e.url);
  }

  const expected = new Set<string>();
  for (const s of inventory.surfaces) {
    if (excluded.has(s.url)) {
      out.push(
        `surface ${JSON.stringify(s.url)} is in BOTH the inventory and the exclusions file — a crawled surface cannot be excluded (stale exclusion)`,
      );
    }
    if (depth === 'full' || s.lean) expected.add(s.url);
  }

  for (const url of [...visited].sort()) {
    if (!expected.has(url)) {
      out.push(
        `NEW surface ${JSON.stringify(url)} visited but not pinned in ${stack.inventoryFilename} — coverage grew; regenerate deliberately with CERBERUS_UPDATE_INVENTORY=1 SWEEP_DEPTH=full CRAWL_STACK=${stack.name}`,
      );
    }
  }
  for (const url of [...expected].sort()) {
    if (!visited.has(url)) {
      out.push(
        `pinned surface ${JSON.stringify(url)} (${depth} set) was NOT visited — coverage shrank; fix the crawl/stack regression, never delete the row to make this pass`,
      );
    }
  }
  return out;
}

// ---------------------------------------------------------------------------
// ds/query supersession reconciliation
// ---------------------------------------------------------------------------

/**
 * Minimal structural view of a captured datasource-API response the
 * supersession reconciler needs. Mirrors the wire-capture shape in
 * crawl.spec.ts without coupling to its full type.
 */
export type DsResponseView = {
  url: string;
  status: number;
  requestBody: string;
};

/**
 * Map refId → expr/query from a ds/query request body. Returns an
 * empty map for non-JSON bodies. Both the Prom/Loki `expr` shape and
 * the Tempo `query` (TraceQL) shape are handled.
 */
export function refIdToExpr(requestBody: string): Map<string, string> {
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
 * Stable signature of a ds/query request by its LOGICAL query payload
 * — the datasource-typed set of `refId=expr` pairs — with the
 * per-request `requestId`/`SQR…` nonce deliberately excluded. Two
 * ds/query requests carrying the same query set over the same
 * datasource type share a signature even though their unique
 * `requestId` differs.
 */
export function dsQuerySignature(resp: DsResponseView): string {
  const dsType =
    new URL(resp.url, 'http://x').searchParams.get('ds_type') ?? '';
  const pairs = [...refIdToExpr(resp.requestBody).entries()]
    .map(([refId, expr]) => `${refId}=${expr}`)
    .sort();
  return `${dsType} ${pairs.join(' ')}`;
}

/**
 * Set of ds/query signatures that ultimately SUCCEEDED (2xx) anywhere
 * in a capture window.
 *
 * Reconciles Grafana's last-write-wins query supersession: a Scenes
 * app (Explore Traces, Metrics Drilldown, …) re-fires a panel's query
 * whenever a variable resolves, and ABORTS the older in-flight request
 * the instant the newer one is issued. The aborted request surfaces to
 * the browser as a transient `plugin.requestFailureError` 500 (the
 * backend call ate a `context canceled`), but it is invisible to the
 * user — the panel renders the newer request's result. A non-2xx whose
 * signature appears here is one of those superseded ghosts, not a
 * server fault. A genuinely-broken query never has a 2xx sibling, so it
 * is NOT suppressed — this is reconciliation, not an escape hatch.
 */
export function succeededDsQuerySignatures(
  captured: ReadonlyArray<DsResponseView>,
): Set<string> {
  const out = new Set<string>();
  for (const resp of captured) {
    if (!resp.url.includes('/api/ds/query')) continue;
    if (resp.status >= 200 && resp.status <= 299) {
      out.add(dsQuerySignature(resp));
    }
  }
  return out;
}

/**
 * True iff `resp` is a non-2xx ds/query whose exact query signature
 * later (or earlier) succeeded in the same window — i.e. a superseded,
 * Grafana-aborted in-flight request that the user never saw fail.
 */
export function isSupersededDsQueryFailure(
  resp: DsResponseView,
  succeededSigs: ReadonlySet<string>,
): boolean {
  if (!resp.url.includes('/api/ds/query')) return false;
  if (resp.status >= 200 && resp.status <= 299) return false;
  return succeededSigs.has(dsQuerySignature(resp));
}

// ---------------------------------------------------------------------------
// app-init-race reconciliation: dangling-operand TraceQL
// ---------------------------------------------------------------------------

/**
 * Matches a TraceQL spanset whose boolean filter has a DANGLING /
 * EMPTY operand around `&&` / `||` — i.e. a `{` followed immediately
 * by a binary operator (empty left operand: `{ && true}`), or a
 * binary operator immediately followed by the closing `}` (empty
 * right operand: `{true && }`), or two adjacent operators (`{a && &&
 * b}`). This is a SYNTAX ERROR, not a value-level filter: TraceQL's
 * grammar requires a non-empty operand on each side of `&&` / `||`,
 * so cerberus's parser (and reference Tempo's identical grammar)
 * rejects it with `syntax error: unexpected &&` (HTTP 400).
 *
 * The shape is produced by the Grafana Traces Drilldown app
 * (grafana-exploretraces-app), NOT by cerberus. The app's
 * `PrimarySignalVariable` (a Scenes CustomVariable) applies its
 * default value inside the component's `useEffect`, not in its
 * constructor — so during the initial-load / rapid-navigation window,
 * before that effect fires, the `${primarySignal}` interpolation is
 * EMPTY and the app builds `{ && ${filters}} | rate()` (and the
 * errors / duration variants). The instant the effect resolves the
 * default, the app re-fires the well-formed query; the user only ever
 * sees the settled result. See
 * src/pages/Explore/PrimarySignalVariable.tsx in
 * grafana/traces-drilldown.
 */
const DANGLING_TRACEQL_OPERAND =
  /\{\s*(?:&&|\|\|)|(?:&&|\|\|)\s*\}|(?:&&|\|\|)\s*(?:&&|\|\|)/;

/**
 * True iff `resp` is a non-2xx Tempo ds/query whose EVERY forwarded
 * TraceQL query carries a dangling-operand spanset (see
 * DANGLING_TRACEQL_OPERAND) — the Traces Drilldown app's
 * primarySignal-init-race shape. cerberus correctly 400s these
 * (reference Tempo rejects the identical syntax-error), so they are a
 * transient app-side artifact, not a cerberus fault.
 *
 * This is NOT the supersession reconciler's twin: the malformed query
 * has a DIFFERENT expr than the settled one, so it never shares a
 * signature with a 2xx sibling and `isSupersededDsQueryFailure` can't
 * resolve it. It is also NOT a blanket 400 suppressor — a non-2xx
 * carrying any well-formed query still fails loudly; only the specific
 * dangling-`&&`/`||` syntax shape the app's known init race emits is
 * reconciled, and ALL queries in the request must exhibit it (a mixed
 * request with one well-formed query is a genuine failure).
 */
export function isTransientMalformedTraceQLFailure(
  resp: DsResponseView,
): boolean {
  if (!resp.url.includes('/api/ds/query')) return false;
  if (resp.status >= 200 && resp.status <= 299) return false;
  const dsType = new URL(resp.url, 'http://x').searchParams.get('ds_type');
  if (dsType !== 'tempo') return false;
  const exprs = [...refIdToExpr(resp.requestBody).values()];
  if (exprs.length === 0) return false;
  return exprs.every((expr) => DANGLING_TRACEQL_OPERAND.test(expr));
}

// ---------------------------------------------------------------------------
// Misc
// ---------------------------------------------------------------------------

export function truncate(s: string, max: number): string {
  if (s.length <= max) return s;
  return `${s.slice(0, max)}…<truncated, ${s.length - max} more char(s)>`;
}
