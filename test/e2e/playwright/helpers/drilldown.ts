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

/**
 * Drive two levels of click-drill inside a drilldown app.
 *
 * Step 1: navigate to `app.root` (relative to the page's baseURL).
 * Step 2: click the first selectable facet / dimension. Each app
 *         exposes a different first-level affordance:
 *           - Explore Metrics  → a metric tile in the trail grid.
 *           - Explore Logs     → a label-value chip in the labels list.
 *           - Explore Traces   → a service-name row in the table.
 *           - Explore Profiles → a profile-type / service row.
 *         The helper uses a single tolerant selector union so an
 *         affordance schema drift on Grafana upgrade fails loudly
 *         in *this* helper (not silently in a phase spec).
 * Step 3: click the first selectable affordance in the resulting view
 *         (= "drill one more level").
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
  // type; the selector union below is permissive on purpose.
  const firstAffordance = page
    .locator(
      [
        // Metrics Drilldown (12.x) — per-metric "Select" action on the
        // tile grid (testid select-action-<metric_name>, verified live
        // against grafana/grafana:12.2.9).
        '[data-testid^="select-action-"]',
        // Explore Metrics — metric tile / trail card (11.x families,
        // kept so a selector rename fails loudly here, not silently).
        '[data-testid^="data-testid metric-select"]',
        '[data-testid^="data-testid trail-"]',
        // Logs Drilldown (12.x) — per-service "Show logs" select button
        // (verified live against grafana/grafana:12.2.9).
        '[data-testid="data-testid button-select-service"]',
        // Explore Logs — label chip / detected-label entry.
        '[data-testid^="data-testid detected-label"]',
        '[data-testid^="data-testid label-name"]',
        // Explore Traces — service-name row / facet button.
        '[data-testid^="data-testid service-"]',
        '[data-testid^="data-testid facet-"]',
        // Explore Profiles — profile-type / flamegraph entry.
        '[data-testid^="data-testid profile-"]',
        '[data-testid^="data-testid flamegraph"]',
        // Generic Grafana table-row affordance (last-resort).
        '[role="rowgroup"] [role="row"] a[href]',
      ].join(', '),
    )
    .first();

  if ((await firstAffordance.count()) === 0) {
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

  // Step 3 — second drill. The selector union is the same; after a
  // first click most apps render a secondary breakdown view whose
  // affordances reuse the same testid families.
  const secondAffordance = page
    .locator(
      [
        // 12.x families first (see the first-drill union above): a
        // second select-action drills into a breakdown label; the
        // include-filter button drills a logs facet.
        '[data-testid^="select-action-"]',
        '[data-testid="data-testid button-filter-include"]',
        '[data-testid^="data-testid metric-select"]',
        '[data-testid^="data-testid trail-"]',
        '[data-testid^="data-testid detected-label-value"]',
        '[data-testid^="data-testid label-value"]',
        '[data-testid^="data-testid service-"]',
        '[data-testid^="data-testid facet-"]',
        '[data-testid^="data-testid profile-"]',
        '[data-testid^="data-testid flamegraph"]',
        '[role="rowgroup"] [role="row"] a[href]',
      ].join(', '),
    )
    .first();

  if ((await secondAffordance.count()) === 0) {
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
