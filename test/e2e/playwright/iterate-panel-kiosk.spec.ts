/**
 * Phase-4 single-panel-kiosk sweep.
 *
 * Iterates every provisioned dashboard, every panel, and — for each
 * panel — opens Grafana's single-panel kiosk view
 * (`/d/<uid>?viewPanel=<id>&kiosk`). Once the kiosk view has settled
 * past its repaint flicker (see `tolerateRepaintFlicker` in
 * `helpers/dom.ts`), the spec asserts:
 *
 *   1. The panel rendered something visible — a chart canvas, a table
 *      DOM, or another known Grafana panel-body container. A panel
 *      with zero visible body nodes in kiosk view is a regression
 *      (N13-shaped: the kiosk wrapper re-mounts the panel, and a
 *      layout-mismatch bug surfaces as an empty body even though the
 *      panel rendered fine in grid mode).
 *   2. No `role="alert"` banner with error-class text is on the page.
 *      Grafana's red error-state banner and the trace-view "Query
 *      error" banner both surface as `role="alert"`; the kiosk pass
 *      catches plugin-specific banners that only fire in viewPanel
 *      mode (some Grafana plugins ship a different render path for
 *      single-panel kiosk vs the grid container).
 *   3. No browser console `error`-level messages were emitted during
 *      the kiosk-mode navigation. Reuses `captureConsoleErrors`.
 *      Q5 in `~/.claude/plans/e2e-enhance.md` §9 forbids an allow-list
 *      for cerberus-emitted errors. One narrow exception applies here:
 *      Grafana's `[Metrics] loadDashboardScene` perf-measure error
 *      fires on every kiosk-view navigation regardless of datasource
 *      (kiosk bypasses the scene-load path that primes the matching
 *      `performance.mark`). See `KIOSK_UPSTREAM_GRAFANA_CONSOLE_NOISE`
 *      below — every other console error remains a failure.
 *
 * After the per-panel assertions, the spec presses ESC to leave kiosk
 * view and re-checks the dashboard for stuck-loading state + orphaned
 * modals — kiosk → ESC → grid round-trips also surface their own
 * regressions (a kiosk wrapper that doesn't unmount cleanly leaves a
 * stranded overlay that traps clicks).
 *
 * The spec wires into the existing compose-smoke job (PR-blocking),
 * not nightly. Performance budget: +90-120s incremental over the
 * compose-smoke + phase-1/2/3 baseline (one extra browser navigation
 * per panel — Phase 4's headline cost per the plan file).
 *
 * What this catches (resolved on main; this is a pin, not a hunt):
 *   - N13: hover-click panel title → "View" kiosk → panel renders
 *     with a different layout that errors. The latent class that the
 *     plan file flagged but no shipped PR closed — once the spec
 *     lands, the regression cannot return silently.
 *
 * Env:
 *   GRAFANA_URL       default http://localhost:3000
 *   GRAFANA_BASE_URL  honoured as a fallback for parity with
 *                     compose_grafana_smoke.spec.ts
 */

import { type Page, expect, test } from '@playwright/test';

import {
  type Dashboard,
  type Panel,
  captureConsoleErrors,
  captureFailedResponses,
  captureRoleAlertBanners,
  describeSweepDepth,
  generateSelfTraffic,
  iterateDashboards,
  iteratePanels,
  readPanelExpectation,
  sweepDepth,
  tolerateRepaintFlicker,
} from './helpers/index.js';

// Self-traffic warmup. Mirrors the phase-1/2 specs so cerberus
// dashboards have populated panels by the time kiosk view opens —
// otherwise a panel legitimately empty due to "no traffic yet" would
// false-positive the visible-body assertion. 30s is the low end of
// "long enough to populate the cerberus panels".
const SEED_TRAFFIC_SECONDS = 30;

// Substrings on a `role="alert"` banner that count as an error-state
// surface. The banner text Grafana renders for a panel-error state
// (`"Query error"`, `"Failed to fetch"`, `"plugin.downstreamError"`,
// `"illegal wireType"`, etc.) all contain one of these tokens. We
// match case-insensitively. Pure-informational banners (`"Auto-refresh"
// paused`, dashboard-save toasts) don't carry these tokens and are
// intentionally not surfaced.
const ALERT_ERROR_PATTERNS: RegExp[] = [
  /error/i,
  /failed/i,
  /illegal wiretype/i,
  /plugin\.downstream/i,
  /unable to/i,
];

// Upstream-Grafana console-error families that fire on kiosk-view
// navigation regardless of datasource and cannot be addressed from
// cerberus's side. The base `captureConsoleErrors` helper carries no
// allow-list per §9 Q5 of the e2e enhancement plan; that policy
// targets cerberus-emitted errors. Kiosk view (`?viewPanel=N&kiosk`)
// bypasses Grafana's normal scene-load bootstrap, so its `PerformanceBackend`
// fires `stopMeasure('loadDashboardScene')` without the matching
// `loadDashboardScene_started` `performance.mark` having been set
// during the scene boot. The same console error reproduces against
// vanilla Grafana 11.x + any datasource. Every other category of
// console error (React render failures, network errors, plugin
// failures, our own datasource errors) still surfaces as a failure;
// only this specific Grafana-internal telemetry pattern is filtered.
const KIOSK_UPSTREAM_GRAFANA_CONSOLE_NOISE: RegExp[] = [
  /\[Metrics\] Failed to stopMeasure loadDashboardScene.*The mark 'loadDashboardScene_started' does not exist/i,
];

/**
 * The set of selectors that count as "visible panel body content".
 * A Grafana 11.x panel exposes one of these once it has rendered. We
 * accept any of them — the per-panel-type selector is too brittle to
 * maintain across the timeseries / stat / table / logs panel matrix
 * the cerberus dashboards exercise.
 *
 * `data-testid Panel data` is the canonical post-render testid (it
 * wraps the actual visualisation node — canvas / table / log list).
 * The legacy class fallbacks cover panels that don't yet ship the
 * testid (some plugin-provided panels lag the convention).
 */
const PANEL_BODY_SELECTORS: string[] = [
  '[data-testid="data-testid panel content"]',
  '[data-testid^="data-testid Panel data"]',
  '.panel-content canvas',
  '.panel-content table',
  '.panel-content [role="table"]',
  '.panel-content .logs-rows',
  '.panel-container canvas',
  '.panel-container table',
];

type KioskFailure = {
  dashboardTitle: string;
  panelTitle: string;
  panelId: number;
  rule: string;
  detail: string;
};

test('panel-kiosk: every panel renders cleanly in single-panel kiosk view + back-nav is clean', async ({
  page,
  request,
}, testInfo) => {
  // Per-panel navigation is the runtime tax here. Budget mirrors the
  // plan file's +90-120s estimate (§8.1) on top of the compose-smoke
  // ~3 min baseline; an 8 min ceiling covers the seed + the full
  // dashboard × panel iteration on a slow CI runner.
  testInfo.setTimeout(8 * 60_000);

  const baseURL =
    process.env.GRAFANA_URL ??
    process.env.GRAFANA_BASE_URL ??
    'http://localhost:3000';

  // Seed traffic so cerberus panels have something to render
  // when kiosk mode re-mounts them. Without this, a panel that's
  // legitimately empty (no traffic yet) would trip the visible-body
  // assertion below.
  await generateSelfTraffic(request, SEED_TRAFFIC_SECONDS);

  const allDashboards = await iterateDashboards(request, baseURL);
  expect(allDashboards.length, 'at least one provisioned dashboard').toBeGreaterThan(
    0,
  );

  // Depth-gated STATE count (rules unchanged): at 'lean' (the per-PR
  // gate) the kiosk render sweeps ops-family dashboards only —
  // showcase-prefixed boards are covered per-PR by the API-layer
  // probes in iterate-all-dashboards and get their per-panel kiosk
  // render on the nightly 'full' lane. Mirrors the browser-render
  // gating in iterate-all-dashboards.spec.ts.
  const depth = sweepDepth();
  // eslint-disable-next-line no-console
  console.log(describeSweepDepth(depth));
  const dashboards = allDashboards.filter(
    (d) => depth === 'full' || !d.uid.startsWith('showcase-'),
  );

  const failures: KioskFailure[] = [];
  const perDashboardCounts: Array<{ title: string; panels: number }> = [];

  for (const dashboard of dashboards) {
    const panels = iteratePanels(dashboard);
    perDashboardCounts.push({ title: dashboard.title, panels: panels.length });

    for (const panel of panels) {
      // Some Grafana row-style placeholders survive the row-flatten
      // pass with id=0; exclude them — they have no kiosk URL.
      if (panel.id === 0) continue;

      const sweepFailures = await sweepPanelKiosk(
        page,
        baseURL,
        dashboard,
        panel,
      );
      failures.push(...sweepFailures);
    }
  }

  testInfo.annotations.push({
    type: 'panel-kiosk',
    description: perDashboardCounts
      .map((d) => `${d.title}: ${d.panels} panel(s)`)
      .join('; '),
  });

  if (failures.length > 0) {
    const detail = failures
      .map(
        (f) =>
          `[${f.dashboardTitle} :: ${f.panelTitle} (#${f.panelId}) :: ${f.rule}] ${f.detail}`,
      )
      .join('\n\n');
    throw new Error(
      `panel-kiosk rule violated for ${failures.length} panel(s):\n\n${detail}`,
    );
  }
});

/**
 * Drive a single panel through:
 *   - navigate to `/d/<uid>?viewPanel=<id>&kiosk`
 *   - settle past the kiosk repaint flicker
 *   - assert visible body, no role=alert error, no console errors
 *   - press ESC to return to the grid view
 *   - assert no stuck-loading panel + no orphaned modal
 *
 * Returns a list of failures — empty if the round-trip is clean.
 *
 * The function captures + tears down console listeners per panel so
 * one panel's noise doesn't bleed into another's assertion.
 */
async function sweepPanelKiosk(
  page: Page,
  baseURL: string,
  dashboard: Dashboard,
  panel: Panel,
): Promise<KioskFailure[]> {
  const failures: KioskFailure[] = [];

  // The panel's declared cerberus.expect contract (showcase-family
  // privilege; the default is the undeclared nonempty contract).
  // The API-layer sweep (iterate-all-dashboards) owns the
  // bidirectional data/error enforcement; here the declaration only
  // adjusts which RENDER outcomes are legitimate:
  //
  //   - error:<s> panels showcase a rejected query. In kiosk view the
  //     legitimate render is EITHER Grafana's error banner (the query
  //     4xxes verbatim) OR a normal body (Grafana clamps request
  //     params — e.g. the resolution-cap panel's 1ms min interval is
  //     clamped by maxDataPoints, so the browser-side query
  //     succeeds). The alert-banner / console-error / failed-response
  //     rules therefore don't apply; navigation integrity (back-nav,
  //     something rendered) still does.
  //   - empty panels render Grafana's "No data" placeholder, which
  //     carries no canvas; the visible-body rule accepts the
  //     placeholder instead.
  const expectation = readPanelExpectation(panel);
  const isErrorContract =
    expectation.declared && expectation.expect.startsWith('error:');
  const isEmptyContract =
    expectation.declared && expectation.expect === 'empty';

  const { messages: consoleErrors, stop: stopConsole } =
    await captureConsoleErrors(page);
  // Companion network capture: the browser logs a non-2xx resource
  // load as the anonymous "Failed to load resource: … status of 400"
  // console line, which names neither the request nor the responder.
  // Recording status/method/URL/body alongside makes a kiosk failure
  // actionable from the CI log alone.
  const { failures: failedResponses, stop: stopResponses } =
    await captureFailedResponses(page);

  const kioskURL = `${baseURL}/d/${dashboard.uid}?viewPanel=${panel.id}&kiosk`;

  try {
    await page.goto(kioskURL, {
      waitUntil: 'domcontentloaded',
      timeout: 90_000,
    });

    // Kiosk re-mounts the panel into a full-screen wrapper. The
    // flicker handler waits for `networkidle` → settle → `networkidle`
    // so the repaint commits before we probe the DOM.
    await tolerateRepaintFlicker(page, { settleMs: 750, timeoutMs: 45_000 });

    // 1. Visible-body assertion. Accept any of the canonical panel
    //    body selectors — the per-panel-type body selector matrix
    //    is too brittle to maintain (timeseries → canvas, table →
    //    [role="table"], logs → .logs-rows, etc.).
    const bodyCount = await page
      .locator(PANEL_BODY_SELECTORS.join(', '))
      .count();
    const alertBanners = await captureRoleAlertBanners(page);
    const errorBanners = alertBanners.filter((text) =>
      ALERT_ERROR_PATTERNS.some((re) => re.test(text)),
    );
    const noDataCount = await page
      .locator('[data-testid="data-testid Panel data error message"]')
      .or(page.getByText('No data', { exact: true }))
      .count();
    const rendered =
      bodyCount > 0 ||
      (isEmptyContract && noDataCount > 0) ||
      (isErrorContract && (noDataCount > 0 || errorBanners.length > 0));
    if (!rendered) {
      failures.push({
        dashboardTitle: dashboard.title,
        panelTitle: panel.title,
        panelId: panel.id,
        rule: 'kiosk-empty-body',
        detail: `no visible panel-body element found in kiosk view; url: ${kioskURL}`,
      });
    }

    // 2. role=alert error-banner sweep. Read every banner currently
    //    rendered; flag any whose text contains an error-class token.
    //    Declared error-contract panels are exempt — the banner is
    //    the showcased outcome (the API-layer sweep enforces the
    //    declared error substring bidirectionally).
    if (!isErrorContract) {
      for (const banner of errorBanners) {
        failures.push({
          dashboardTitle: dashboard.title,
          panelTitle: panel.title,
          panelId: panel.id,
          rule: 'kiosk-alert-banner',
          detail: `role=alert banner with error text: ${truncate(banner, 400)}`,
        });
      }
    }

    // 3. ESC back to the grid view + back-nav cleanliness.
    await page.keyboard.press('Escape');
    await tolerateRepaintFlicker(page, { settleMs: 500, timeoutMs: 30_000 });

    // 3a. Stuck-loading sweep on the grid view. ESC should unmount
    //     the kiosk wrapper and rehydrate the grid; a panel still
    //     stuck on the spinner is a back-nav regression.
    const stuckCount = await page
      .locator(
        [
          '[data-testid="data-testid Panel header loading"]',
          '.panel-loading',
          '[aria-label="Loading"]',
        ].join(', '),
      )
      .count();
    if (stuckCount > 0) {
      failures.push({
        dashboardTitle: dashboard.title,
        panelTitle: panel.title,
        panelId: panel.id,
        rule: 'kiosk-back-nav-stuck-loading',
        detail: `${stuckCount} panel(s) still spinning after ESC from kiosk view`,
      });
    }

    // 3b. Orphaned-modal sweep. A clean ESC leaves no `role="dialog"`
    //     or Grafana modal wrapper behind. The selector matches both
    //     the ARIA role + Grafana's own modal class for resilience.
    const orphanModalCount = await page
      .locator('[role="dialog"], .modal-content, [data-testid="data-testid Modal"]')
      .count();
    if (orphanModalCount > 0) {
      failures.push({
        dashboardTitle: dashboard.title,
        panelTitle: panel.title,
        panelId: panel.id,
        rule: 'kiosk-back-nav-orphan-modal',
        detail: `${orphanModalCount} dialog/modal node(s) still mounted after ESC from kiosk view`,
      });
    }
  } catch (err) {
    failures.push({
      dashboardTitle: dashboard.title,
      panelTitle: panel.title,
      panelId: panel.id,
      rule: 'kiosk-navigation-threw',
      detail: `kiosk navigation threw: ${(err as Error).message}; url: ${kioskURL}`,
    });
  } finally {
    stopConsole();
    stopResponses();
  }

  // 4. Console-error sweep. Read what got captured during the kiosk
  //    navigation + back-nav. Filter only the narrow set of
  //    Grafana-internal kiosk-bootstrap telemetry errors that cannot
  //    be fixed from cerberus's side
  //    (KIOSK_UPSTREAM_GRAFANA_CONSOLE_NOISE — see comment at the top
  //    of this file). Every other console error remains a failure.
  //    Done after the listener is torn down so a late-fire doesn't
  //    race the read.
  //    Declared error-contract panels are exempt: their 4xx ds/query
  //    response fires the browser's "Failed to load resource" console
  //    error by design — that error IS the showcased behaviour.
  const cerberusConsoleErrors = isErrorContract
    ? []
    : consoleErrors.filter(
        (m) => !KIOSK_UPSTREAM_GRAFANA_CONSOLE_NOISE.some((re) => re.test(m)),
      );
  if (cerberusConsoleErrors.length > 0) {
    const responseDetail =
      failedResponses.length > 0
        ? `\nnon-2xx responses captured during the same window:\n${failedResponses
            .map((r) => `  - ${truncate(r, 700)}`)
            .join('\n')}`
        : '\n(no non-2xx HTTP response captured — the console errors came from elsewhere)';
    failures.push({
      dashboardTitle: dashboard.title,
      panelTitle: panel.title,
      panelId: panel.id,
      rule: 'kiosk-console-error',
      detail: `${cerberusConsoleErrors.length} console error(s):\n${cerberusConsoleErrors
        .map((m) => `  - ${truncate(m, 400)}`)
        .join('\n')}${responseDetail}`,
    });
  }

  return failures;
}

function truncate(s: string, max: number): string {
  if (s.length <= max) return s;
  return `${s.slice(0, max)}…<truncated, ${s.length - max} more char(s)>`;
}
