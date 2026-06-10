/**
 * Grafana surface-crawler library.
 *
 * The crawler (crawl.spec.ts) BFS-walks the live Grafana UI from the
 * root page, harvesting same-origin links per page, and applies the
 * same universal oracle set on EVERY visited page. This module holds
 * the deterministic plumbing — URL canonicalization, scope
 * exclusions, the surface-inventory file format — so the spec stays a
 * thin driver and the rules are unit-pinnable.
 *
 * Why a crawler on top of the enumerated iterate-* specs: an off-CI
 * AI screenshot sweep (2026-06-09) found 34 unique error signatures
 * across 55 BFS-visited pages — several on surfaces NO enumerated
 * spec visits (drilldown-app tabs, logs-drilldown service pages,
 * traces-drilldown comparison view). Every find decomposed into a
 * deterministic signal once named; the crawler carries those signals
 * in CI forever, and the committed surface inventory
 * (grafana-surface-inventory.json) makes coverage growth explicit
 * and shrink impossible.
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
 * The canonical key is PATH-ONLY: every query param is stripped.
 * Grafana routes surfaces by path; query strings carry volatile or
 * session state — time windows (`from`/`to`/`refresh`), org context,
 * kiosk / view-panel modes, Explore state blobs (`panes`/`left`/
 * `right`), variable selections (`var-*`), and the drilldown apps'
 * serialized UI state (`patterns`, `displayedFields`, `urlColumns`,
 * `layout`, …). The first full crawl demonstrated why an
 * param-retention list can't work: the logs-drilldown service page
 * alone produced four param-permutations of one surface. A state of
 * a surface is not a new surface.
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
  for (const re of EXCLUDED_PATH_PATTERNS) {
    if (re.test(path)) return null;
  }

  // Explore collapses wholesale — session state is not a surface.
  if (path === '/explore' || path.startsWith('/explore/')) {
    return { canonical: '/explore', concrete: '/explore' };
  }

  // Bare app redirector → entry route (canonical AND concrete).
  const alias = APP_BARE_PATH_ALIASES.get(path);
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
  for (const re of EXCLUDED_PATH_PATTERNS) {
    if (re.test(path)) return null;
  }

  // Concrete: the raw path + raw query (parameterized canonicals
  // aren't navigable, and drilldown-app hrefs carry var-* state the
  // page needs to boot into the linked view — navigating the
  // logs-drilldown service page without its var-filters leaves the
  // tab bar unmounted). Only the CANONICAL key strips the query.
  return { canonical: path, concrete: `${url.pathname}${url.search}` };
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
): string | null {
  return canonicalTarget(href, baseURL)?.canonical ?? null;
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
  /** Stack the inventory pins (only the compose stack is crawled). */
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

export const INVENTORY_FILENAME = 'grafana-surface-inventory.json';
export const EXCLUSIONS_FILENAME = 'grafana-surface-exclusions.json';

export function inventoryPath(): string {
  return join(__dirname, INVENTORY_FILENAME);
}

export function exclusionsPath(): string {
  return join(__dirname, EXCLUSIONS_FILENAME);
}

export function loadInventory(): SurfaceInventory {
  const raw = readFileSync(inventoryPath(), 'utf8');
  const parsed = JSON.parse(raw) as SurfaceInventory;
  if (!Array.isArray(parsed.surfaces)) {
    throw new Error(
      `loadInventory: ${INVENTORY_FILENAME} has no surfaces[] array`,
    );
  }
  return parsed;
}

export function loadExclusions(): SurfaceExclusions {
  const raw = readFileSync(exclusionsPath(), 'utf8');
  const parsed = JSON.parse(raw) as SurfaceExclusions;
  if (!Array.isArray(parsed.exclusions)) {
    throw new Error(
      `loadExclusions: ${EXCLUSIONS_FILENAME} has no exclusions[] array`,
    );
  }
  return parsed;
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
        `NEW surface ${JSON.stringify(url)} visited but not pinned in ${INVENTORY_FILENAME} — coverage grew; regenerate deliberately with CERBERUS_UPDATE_INVENTORY=1 SWEEP_DEPTH=full`,
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
// Misc
// ---------------------------------------------------------------------------

export function truncate(s: string, max: number): string {
  if (s.length <= max) return s;
  return `${s.slice(0, max)}…<truncated, ${s.length - max} more char(s)>`;
}
