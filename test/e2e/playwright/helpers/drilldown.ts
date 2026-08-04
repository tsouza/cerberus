/**
 * Drilldown-app iteration helpers.
 *
 * The cerberus stacks run three Grafana first-party drilldown apps
 * (preinstalled by Grafana 12.x out of the box):
 *   - grafana-metricsdrilldown-app   (Explore Metrics)
 *   - grafana-lokiexplore-app        (Explore Logs)
 *   - grafana-exploretraces-app      (Explore Traces)
 *
 * Each app boots its own React tree and fires its own wave of
 * `/api/datasources/uid/.../resources/...` calls. The compose-smoke
 * spec only touches Explore Logs (and only at boot). The nightly
 * `iterate-drilldown-apps.spec.ts` drives a two-level click through
 * each app to catch N15-class regressions (drilldown empty after a
 * facet click).
 *
 * Every catalog entry MUST be installed in the compose stack — the
 * spec hard-asserts install status (after a bounded readiness wait,
 * see `waitForAppInstalled`). If a future Grafana upgrade drops one
 * of the three apps from the vanilla image, either provision it via
 * `GF_INSTALL_PLUGINS` or remove the entry from the catalog.
 * `grafana-pyroscope-app` is deliberately not in the catalog because
 * cerberus does not ship profiling.
 *
 * The drilldown-app surface churns hard on every Grafana upgrade —
 * pin the Grafana version (currently `grafana/grafana:12.2.9`, see
 * helpers/README.md) and re-audit the click paths in the same PR
 * when bumping.
 *
 * IMPORTANT: the three apps are also NOT pinned to a Grafana version
 * on their own. They're grafana.com-hosted plugins the core image
 * downloads asynchronously at boot (see `waitForAppInstalled` above),
 * and every fresh container pulls whatever the CURRENT published
 * version is — independent of the pinned core Grafana tag. Selectors
 * here can therefore drift even with zero changes to
 * `docker-compose.yml` / the k3d Grafana manifest. This bit cerberus
 * for real: `EXPECTED_DRILL_DEPTH` (#1502 / #1661) went from
 * annotation-only to a hard gate and immediately failed on the very
 * next push-to-main run, purely from app auto-updates
 * (grafana-exploretraces-app 2.1.0, grafana-metricsdrilldown-app
 * 2.3.1, grafana-lokiexplore-app 2.4.0 as of 2026-08-04) — no Grafana
 * core bump involved. `drillTwoLevels` was re-verified live against
 * those versions and is documented per-selector below; re-verify the
 * same way (`docker compose up -d`, drive each app through
 * `npx playwright test iterate-drilldown-apps.spec.ts`) whenever this
 * spec starts failing depth-below-floor with no other change in
 * sight — it's more likely app drift than a cerberus regression, but
 * only live verification (not assumption) tells you which.
 */

import type { APIRequestContext, Page, Response } from '@playwright/test';

export type DrilldownApp = {
  /** Grafana plugin id. */
  id: string;
  /** Path under the Grafana base URL to land on the app root. */
  root: string;
  /** Human-readable label for failure messages. */
  label: string;
};

// The catalogue lists exactly the drilldown apps the pinned Grafana
// image (grafana/grafana:12.2.9) preinstalls first-party — Grafana
// 12.x hardcodes grafana-lokiexplore-app, grafana-metricsdrilldown-app,
// and grafana-exploretraces-app in its preinstall list
// (pkg/setting/setting_plugins.go), so none of them needs
// GF_INSTALL_PLUGINS. grafana-pyroscope-app stays OUT of the
// catalogue: cerberus ships no profiling backend, so the app has
// nothing to drill into.
export const DRILLDOWN_APPS: ReadonlyArray<DrilldownApp> = [
  {
    id: 'grafana-metricsdrilldown-app',
    root: '/a/grafana-metricsdrilldown-app/trail',
    label: 'Explore Metrics',
  },
  {
    id: 'grafana-lokiexplore-app',
    root: '/a/grafana-lokiexplore-app/explore?var-ds=cerberus-loki',
    label: 'Explore Logs',
  },
  {
    id: 'grafana-exploretraces-app',
    root: '/a/grafana-exploretraces-app/explore',
    label: 'Explore Traces',
  },
];

/**
 * Enumerate the drilldown apps the spec phases should sweep.
 *
 * Returns a fresh array each call so callers can mutate (filter /
 * slice) without poisoning the module-level constant.
 */
export function iterateDrilldownApps(): DrilldownApp[] {
  return [...DRILLDOWN_APPS];
}

/**
 * Probe whether a Grafana drilldown app is installed by reading
 * `/api/plugins/<id>/settings`. Grafana returns 200 + a JSON
 * `{ enabled: true, ... }` body for an installed+enabled app, 404
 * for an app that's not installed, and 200 + `{ enabled: false }` for
 * an app that's installed but disabled.
 *
 * Throws a descriptive error if the plugin endpoint returns non-2xx,
 * an unparseable body, or `enabled !== true`. The caller's
 * `isAppInstalled` predicate must surface a `false` result as a hard
 * failure — every catalog entry MUST be installed.
 */
export async function isAppInstalled(
  request: APIRequestContext,
  baseURL: string,
  appId: string,
): Promise<boolean> {
  const resp = await request.get(`${baseURL}/api/plugins/${appId}/settings`);
  const status = resp.status();
  if (status < 200 || status > 299) return false;
  let parsed: { enabled?: boolean };
  try {
    parsed = (await resp.json()) as typeof parsed;
  } catch {
    return false;
  }
  return parsed.enabled === true;
}

/**
 * Bounded readiness wait for the async first-party preinstall flow.
 *
 * Grafana 12.x downloads its preinstalled drilldown apps from
 * grafana.com asynchronously at boot, AFTER `/api/health` already
 * reports green (grafana/grafana#106871). A spec that probes
 * `/api/plugins/<id>/settings` right after the stack comes up can
 * therefore see a transient 404 / `enabled: false` for an app that
 * will be installed seconds later.
 *
 * This helper polls `isAppInstalled` until it reports true or the
 * deadline expires. It is startup *synchronization* — the same role
 * `docker compose up --wait` plays for containers — NOT failure
 * tolerance: callers must still hard-assert the final result, and an
 * app that never installs within the budget fails exactly as a
 * never-installed app does.
 */
export async function waitForAppInstalled(
  request: APIRequestContext,
  baseURL: string,
  appId: string,
  timeoutMs = 120_000,
  pollIntervalMs = 3_000,
): Promise<boolean> {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    if (await isAppInstalled(request, baseURL, appId)) return true;
    if (Date.now() + pollIntervalMs > deadline) return false;
    await new Promise((resolve) => setTimeout(resolve, pollIntervalMs));
  }
}

/**
 * Outcome shape returned by `drillTwoLevels`. The spec uses this to
 * annotate per-app "got to level N" without inferring depth from a
 * void return.
 */
export type DrillTwoLevelsResult = {
  /** Number of drill clicks the helper successfully executed (0, 1, or 2). */
  levelsClicked: number;
  /** True if the app root navigation rendered no clickable affordance. */
  rootHadNoAffordance: boolean;
};

// Third-party drilldown-app "select this facet" buttons that carry no
// data-testid of their own (Traces Drilldown's per-attribute
// Include/Exclude pair, Metrics Drilldown's by-label breakdown-panel
// "Select" button — both verified live against the grafana.com-hosted
// app versions running under grafana/grafana:12.2.9 on 2026-08-04:
// grafana-exploretraces-app@2.1.0, grafana-metricsdrilldown-app@2.3.1).
// Scoped to a `data-testid^="data-testid Panel header "` ancestor (a
// stable, already-load-bearing Grafana convention used throughout every
// drilldown app) and `:not([data-testid])` so the pattern can't shadow
// a sibling app's OWN testid'd button with the same visible text —
// Logs Drilldown's "Include" filter chip carries
// `data-testid="data-testid button-filter-include"` and must keep
// matching through its own selector below, not this generic one.
const PANEL_BUTTON_INCLUDE = '[data-testid^="data-testid Panel header "] button:has-text("Include"):not([data-testid])';
const PANEL_BUTTON_SELECT = '[data-testid^="data-testid Panel header "] button:has-text("Select"):not([data-testid])';

// Logs Drilldown (2.4.0) moved its per-field breakdown behind a "Fields"
// tab: the per-service view a first-level click lands on (.../service/
// <name>/logs) has no second-level facet inline, so the second drill has
// to switch to that tab before a `data-testid breakdown-select-value`
// button becomes available. See the fallback in `drillTwoLevels` below
// for why this is folded into the second click rather than surfaced as
// its own counted level.
const LOGS_FIELDS_TAB = '[data-testid="data-testid tab-fields"]';
const LOGS_BREAKDOWN_SELECT_VALUE = '[data-testid="data-testid breakdown-select-value"]';

// Shared first/second-level selector union. Both drills reuse the same
// tolerant set of facet-affordance patterns — after a first click most
// apps render a secondary breakdown view whose affordances reuse the
// same testid families (or, for Traces Drilldown, literally the same
// selector: selecting one attribute value re-renders the panel grid one
// groupBy level deeper with a fresh Include/Exclude pair).
const AFFORDANCE_SELECTOR = [
  // Metrics Drilldown (12.x) — per-metric "Select" action on the
  // tile grid (testid select-action-<metric_name>, verified live
  // against grafana/grafana:12.2.9), and the untestid'd "Select"
  // button on a by-label breakdown panel (second drill level).
  '[data-testid^="select-action-"]',
  PANEL_BUTTON_SELECT,
  // Explore Metrics — metric tile / trail card (11.x families,
  // kept so a selector rename fails loudly here, not silently).
  '[data-testid^="data-testid metric-select"]',
  '[data-testid^="data-testid trail-"]',
  // Logs Drilldown (12.x) — per-service "Show logs" select button,
  // and the per-field "Include" filter chip (both carry stable
  // testids; verified live against grafana/grafana:12.2.9).
  '[data-testid="data-testid button-select-service"]',
  '[data-testid="data-testid button-filter-include"]',
  '[data-testid="data-testid breakdown-select-value"]',
  // Explore Logs — label chip / detected-label entry (older families).
  '[data-testid^="data-testid detected-label"]',
  '[data-testid^="data-testid label-name"]',
  '[data-testid^="data-testid detected-label-value"]',
  '[data-testid^="data-testid label-value"]',
  // Explore Traces (2.x) — the per-attribute-value Include button
  // (untestid'd; see PANEL_BUTTON_INCLUDE doc above). Also try the
  // older service-/facet- families in case a future version restores
  // dedicated testids.
  PANEL_BUTTON_INCLUDE,
  '[data-testid^="data-testid service-"]',
  '[data-testid^="data-testid facet-"]',
  // Explore Profiles — profile-type / flamegraph entry.
  '[data-testid^="data-testid profile-"]',
  '[data-testid^="data-testid flamegraph"]',
  // Generic Grafana table-row affordance (last-resort).
  '[role="rowgroup"] [role="row"] a[href]',
].join(', ');

// Upper bound on how long a breakdown panel gets to finish its async
// render after `networkidle` fires. `networkidle` only means the
// network went quiet for 500ms — it does NOT mean React has finished
// mounting the panel grid from the data that already arrived, and the
// by-label / by-attribute breakdown panels (Metrics Drilldown's
// PANEL_BUTTON_SELECT, Traces Drilldown's PANEL_BUTTON_INCLUDE) render
// progressively after that point. A locator `.count()` taken right
// after `networkidle` can therefore read 0 on an affordance that
// exists a second later — this is the exact gap that made the
// drill-depth floor flake locally until the check switched from a
// point-in-time count to a bounded wait.
const AFFORDANCE_SETTLE_TIMEOUT_MS = 15_000;

/**
 * Wait up to `AFFORDANCE_SETTLE_TIMEOUT_MS` for `locator`'s first match
 * to attach, without throwing. Returns whether it appeared in time.
 */
async function affordanceAppears(locator: ReturnType<Page['locator']>): Promise<boolean> {
  try {
    await locator.first().waitFor({ state: 'attached', timeout: AFFORDANCE_SETTLE_TIMEOUT_MS });
    return true;
  } catch {
    return false;
  }
}

/**
 * Drive two levels of click-drill inside a drilldown app.
 *
 * Step 1: navigate to `app.root` (relative to the page's baseURL).
 * Step 2: click the first selectable facet / dimension. Each app
 *         exposes a different first-level affordance:
 *           - Explore Metrics  → a metric tile in the trail grid.
 *           - Explore Logs     → a per-service "Show logs" button.
 *           - Explore Traces   → a per-attribute-value Include button.
 *           - Explore Profiles → a profile-type / service row.
 *         The helper uses a single tolerant selector union so an
 *         affordance schema drift on Grafana upgrade fails loudly
 *         in *this* helper (not silently in a phase spec).
 * Step 3: click the first selectable affordance in the resulting view
 *         (= "drill one more level"). Logs Drilldown's second-level
 *         facet lives behind a "Fields" tab rather than inline — if
 *         the shared union finds nothing, click that tab (when
 *         present) and re-probe once before giving up.
 *
 * Each navigation waits for `networkidle` with a 45s cap (matches the
 * existing compose-smoke timing budget). Assertions over the
 * captured network traffic + DOM are the spec's job, not this
 * helper's — `drillTwoLevels` only drives the gestures.
 *
 * Returns a `DrillTwoLevelsResult` so the spec can annotate
 * "completed N drill click(s)" without inferring depth from the page.
 */
export async function drillTwoLevels(
  app: DrilldownApp,
  page: Page,
): Promise<DrillTwoLevelsResult> {
  // Step 1 — land on the app root.
  await page.goto(app.root, { waitUntil: 'domcontentloaded', timeout: 90_000 });
  await page
    .waitForLoadState('networkidle', { timeout: 45_000 })
    .catch(() => {
      // networkidle timing out isn't fatal; the spec's wire sweep
      // will surface a request that never resolved.
    });

  // Step 2 — first drill. Each app exposes a different affordance
  // type; the selector union is permissive on purpose.
  const firstAffordance = page.locator(AFFORDANCE_SELECTOR).first();

  if (!(await affordanceAppears(firstAffordance))) {
    // App root rendered no clickable affordance — that's a spec-level
    // failure (covered by the wire / DOM sweeps), not a precondition
    // bug in this helper. Bail out cleanly so the spec sees the
    // empty-state and reports it with full context.
    return { levelsClicked: 0, rootHadNoAffordance: true };
  }

  await firstAffordance.click({ timeout: 10_000 });
  await page
    .waitForLoadState('networkidle', { timeout: 45_000 })
    .catch(() => {});

  // Step 3 — second drill. Same tolerant union first.
  let secondAffordance = page.locator(AFFORDANCE_SELECTOR).first();
  let secondAffordanceFound = await affordanceAppears(secondAffordance);

  if (!secondAffordanceFound) {
    // Logs Drilldown-shaped fallback: the per-service view the first
    // click landed on has no inline second-level facet — it's parked
    // behind a "Fields" tab. Switch to it (if present) and re-probe
    // once; this is still one logical "drill one level deeper"
    // gesture from the caller's point of view; the tab switch is
    // plumbing, not a counted click of its own.
    const fieldsTab = page.locator(LOGS_FIELDS_TAB);
    if (await affordanceAppears(fieldsTab)) {
      await fieldsTab.first().click({ timeout: 10_000 }).catch(() => {});
      await page
        .waitForLoadState('networkidle', { timeout: 45_000 })
        .catch(() => {});
      secondAffordance = page.locator(LOGS_BREAKDOWN_SELECT_VALUE).first();
      secondAffordanceFound = await affordanceAppears(secondAffordance);
    }
  }

  if (!secondAffordanceFound) {
    return { levelsClicked: 1, rootHadNoAffordance: false };
  }

  await secondAffordance.click({ timeout: 10_000 });
  await page
    .waitForLoadState('networkidle', { timeout: 45_000 })
    .catch(() => {});

  return { levelsClicked: 2, rootHadNoAffordance: false };
}

/**
 * `Response` is re-exported for spec convenience so the
 * iterate-drilldown-apps spec doesn't need a second `@playwright/test`
 * import just for the response capture type.
 */
export type { Response };
