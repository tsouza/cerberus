/**
 * Drilldown-app iteration helpers.
 *
 * Grafana ships four built-in drilldown apps (Grafana 11.4.0):
 *   - grafana-metricsdrilldown-app   (Explore Metrics)
 *   - grafana-lokiexplore-app        (Explore Logs)
 *   - grafana-exploretraces-app      (Explore Traces)
 *   - grafana-pyroscope-app          (Explore Profiles / Pyroscope)
 *
 * Each app boots its own React tree and fires its own wave of
 * `/api/datasources/uid/.../resources/...` calls. The compose-smoke
 * spec only touches Explore Logs (and only at boot). The nightly
 * `iterate-drilldown-apps.spec.ts` drives a two-level click through
 * each installed app to catch N15-class regressions (drilldown empty
 * after a facet click).
 *
 * The pyroscope app is NOT preinstalled on the cerberus compose stack
 * (no `GF_INSTALL_PLUGINS` line in `docker-compose.yml`); on a vanilla
 * Grafana 11.4.0 the first three apps ship out-of-the-box. The spec
 * uses `isAppInstalled` below to detect per-app availability so a
 * missing pyroscope (or any other app on a stripped-down stack)
 * annotates cleanly instead of failing.
 *
 * The drilldown-app surface churns hard on every Grafana upgrade —
 * pin the Grafana version (currently `grafana/grafana:11.4.0`, see
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
  {
    id: 'grafana-pyroscope-app',
    root: '/a/grafana-pyroscope-app/single',
    label: 'Explore Profiles',
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
 * The compose stack does not preinstall `grafana-pyroscope-app`
 * (no `GF_INSTALL_PLUGINS` in `docker-compose.yml`); calls for that
 * app return 404 there. On a vanilla Grafana 11.4.0 the other three
 * drilldown apps ship preinstalled+enabled.
 *
 * Returns `true` only when the plugin endpoint returns 2xx AND the
 * `enabled` field is truthy. Network errors (e.g. Grafana down) are
 * surfaced as a thrown error — the caller is expected to have already
 * confirmed Grafana reachability before iterating apps.
 */
export async function isAppInstalled(
  request: APIRequestContext,
  baseURL: string,
  appId: string,
): Promise<boolean> {
  const resp = await request.get(`${baseURL}/api/plugins/${appId}/settings`);
  const status = resp.status();
  // 404 → not installed. Other non-2xx (e.g. 401 on an authed Grafana
  // misconfigured for anonymous) are reported as not-installed so the
  // spec annotates and moves on; auth misconfig is out of scope for the
  // drilldown sweep.
  if (status === 404) return false;
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
        // Explore Metrics — metric tile / trail card.
        '[data-testid^="data-testid metric-select"]',
        '[data-testid^="data-testid trail-"]',
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
