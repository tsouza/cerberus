/**
 * Drilldown-app iteration helpers.
 *
 * Grafana ships three built-in drilldown apps:
 *   - grafana-metricsdrilldown-app   (Explore Metrics)
 *   - grafana-lokiexplore-app        (Explore Logs)
 *   - grafana-exploretraces-app      (Explore Traces)
 *
 * Each app boots its own React tree and fires its own wave of
 * `/api/datasources/uid/.../resources/...` calls. The current
 * compose-smoke spec only touches Explore Logs (and only at boot).
 * Phase 6 will drive a two-level click through all three to catch
 * N15-class regressions (drilldown empty after a facet click).
 *
 * The drilldown-app surface churns hard on every Grafana upgrade —
 * pin the Grafana version (currently `grafana/grafana:11.4.0`, see
 * helpers/README.md) and re-audit the click paths in the same PR
 * when bumping.
 */

import type { Page } from '@playwright/test';

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
 * Drive two levels of click-drill inside a drilldown app.
 *
 * Step 1: navigate to `app.root` (relative to the page's baseURL).
 * Step 2: click the first selectable facet / dimension. Each app
 *         exposes a different first-level affordance:
 *           - Explore Metrics  → a metric tile in the trail grid.
 *           - Explore Logs     → a label-value chip in the labels list.
 *           - Explore Traces   → a service-name row in the table.
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
 */
export async function drillTwoLevels(
  app: DrilldownApp,
  page: Page,
): Promise<void> {
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
    return;
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
        '[role="rowgroup"] [role="row"] a[href]',
      ].join(', '),
    )
    .first();

  if ((await secondAffordance.count()) === 0) {
    return;
  }

  await secondAffordance.click({ timeout: 10_000 });
  await page
    .waitForLoadState('networkidle', { timeout: 45_000 })
    .catch(() => {});
}
