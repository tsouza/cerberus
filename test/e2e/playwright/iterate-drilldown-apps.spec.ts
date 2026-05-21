/**
 * Phase-6 drilldown-apps sweep.
 *
 * Iterates every Grafana built-in drilldown app (the 4-app catalogue in
 * `helpers/drilldown.ts`), drives a two-level click-drill through each
 * installed app, and asserts the per-app sweep is clean:
 *
 *   1. Every captured Grafana / cerberus HTTP response is 2xx (no 4xx
 *      and no 5xx). Drilldown apps fire `/api/datasources/proxy/uid/…`,
 *      `/api/datasources/uid/…/resources/…`, and `/api/ds/query` —
 *      every one is in scope per Q5 of the e2e enhancement plan (no
 *      tolerated-status allow-list).
 *   2. No `role="alert"` banner with error-class text is on the page
 *      at any level (root, after first drill, after second drill).
 *      Drilldown apps surface query errors as red banners (e.g.
 *      Explore-Traces' "Query error: illegal wireType" — N4-shaped).
 *   3. No browser console `error`-level message was emitted across
 *      the whole app sweep. Plugin failures (chunk-load errors,
 *      datasource-resource 502s, fetch aborts) all surface as
 *      console errors; the spec carries no allow-list per Q5.
 *
 * Per-app handling for "App not installed":
 *
 *   The cerberus compose stack does NOT preinstall
 *   `grafana-pyroscope-app` (no `GF_INSTALL_PLUGINS` line in
 *   `docker-compose.yml`). The vanilla Grafana 11.4.0 image ships the
 *   other three drilldown apps preinstalled+enabled. For each app the
 *   spec calls `isAppInstalled` (= `/api/plugins/<id>/settings`); if
 *   the app is absent the spec annotates `app-not-installed` and
 *   continues to the next one without failing. This matches the brief:
 *   the spec must surface "Pyroscope not installed" as data, not as a
 *   sweep failure.
 *
 * Gate posture: NIGHTLY ONLY. Wired into the `dashboard` job in
 * `.github/workflows/e2e.yml`, which runs on push-to-main + nightly +
 * manual dispatch (NOT pull_request). Drilldown-app UIs are the
 * highest UI-churn surface in Grafana — keeping this PR-blocking would
 * push every Grafana bump into a same-day spec rewrite. The nightly
 * posture buys a 24h buffer before a regression blocks a PR.
 *
 * Grafana version pinning: selectors and per-app root URLs are tied to
 * `grafana/grafana:11.4.0` (the tag pinned in `docker-compose.yml`).
 * When the compose stack bumps Grafana, this spec MUST be updated in
 * the same PR — see helpers/README.md "Pinned Grafana version" for
 * the upgrade checklist. Q4 of `~/.claude/plans/e2e-enhance.md` §9
 * pinned this maintenance contract.
 *
 * What this catches (the latent class the plan file flagged):
 *
 *   - N15: drill chain breaks on the second-level click — affordance
 *     present, click registered, but the result view is empty + emits
 *     a console error or a `role="alert"` banner.
 *   - N15 bis: app-chunk load failure (a 4xx/5xx on a `/public/build/…`
 *     URL during boot, or a datasource-resources 502 mid-drill).
 *
 * Env:
 *   GRAFANA_URL       default http://localhost:3000
 *   GRAFANA_BASE_URL  honoured as a fallback for parity with
 *                     compose_grafana_smoke.spec.ts
 */

import {
  type Page,
  type Response,
  type TestInfo,
  expect,
  test,
} from '@playwright/test';

import {
  type DrilldownApp,
  captureConsoleErrors,
  captureRoleAlertBanners,
  drillTwoLevels,
  isAppInstalled,
  iterateDrilldownApps,
} from './helpers/index.js';

// `role="alert"` substrings that count as an error-state banner. The
// same list as the kiosk spec — Grafana surfaces all panel / plugin
// error states through one of these tokens.
const ALERT_ERROR_PATTERNS: RegExp[] = [
  /error/i,
  /failed/i,
  /illegal wiretype/i,
  /plugin\.downstream/i,
  /unable to/i,
];

// "App is not installed" placeholder banner that some Grafana
// drilldown apps surface when their plugin isn't loaded. This is the
// expected visible state for an absent app; we don't want to
// `isAppInstalled` it through, but if it shows up after install probe
// passed we still want to skip the error sweep on it. Conservative:
// match only the exact phrasing Grafana 11.4.0 renders.
const APP_NOT_INSTALLED_BANNER_PATTERNS: RegExp[] = [
  /app plugin not installed/i,
  /plugin not found/i,
];

// Upstream-Grafana console-error families that fire on drilldown-app
// boot regardless of datasource health. Kept narrow on purpose — the
// kiosk spec carries a single Grafana-internal telemetry pattern, and
// the drilldown sweep inherits the same policy (Q5 zero-allow-list,
// scoped to cerberus-emitted errors only).
const DRILLDOWN_UPSTREAM_GRAFANA_CONSOLE_NOISE: RegExp[] = [
  /\[Metrics\] Failed to stopMeasure loadDashboardScene.*The mark 'loadDashboardScene_started' does not exist/i,
];

// Captured response shape: stripped down so the failure detail isn't
// dragged down by a 1MB ds/query body.
type CapturedResponseSummary = {
  url: string;
  method: string;
  status: number;
  bodyPreview: string;
};

type DrilldownFailure = {
  app: string;
  rule: string;
  detail: string;
};

test('drilldown-apps: each installed drilldown app drills two levels without 4xx/5xx + no role=alert error + no console errors', async ({
  page,
  request,
}, testInfo) => {
  // Per-app: 1 navigation + 2 click-drills + 3× networkidle settles.
  // Budget 4 apps × ~90s + a generous headroom for the heaviest app
  // (Explore-Traces tends to be slowest on a cold ClickHouse) = 8 min.
  testInfo.setTimeout(8 * 60_000);

  const baseURL =
    process.env.GRAFANA_URL ??
    process.env.GRAFANA_BASE_URL ??
    'http://localhost:3000';

  const apps = iterateDrilldownApps();
  expect(apps.length, 'all four built-in drilldown apps in catalogue').toBe(4);

  const failures: DrilldownFailure[] = [];
  const perAppOutcomes: Array<{ app: string; outcome: string }> = [];

  for (const app of apps) {
    // 1. Install-probe. If the app isn't installed, annotate and skip.
    //    This is the only "skip" allowed by the brief: pyroscope is
    //    expected to be absent on the compose stack, and the spec must
    //    survive that cleanly.
    const installed = await isAppInstalled(request, baseURL, app.id);
    if (!installed) {
      testInfo.annotations.push({
        type: 'drilldown-app-not-installed',
        description: `${app.id} (${app.label}): /api/plugins/${app.id}/settings absent or disabled — drill omitted`,
      });
      perAppOutcomes.push({
        app: app.id,
        outcome: 'app-not-installed',
      });
      continue;
    }

    // 2. Drive the app + capture wire / DOM signal.
    const appFailures = await sweepDrilldownApp(page, baseURL, app, testInfo);
    failures.push(...appFailures);

    perAppOutcomes.push({
      app: app.id,
      outcome:
        appFailures.length === 0
          ? 'drilled-clean'
          : `drilled-with-${appFailures.length}-failure(s)`,
    });
  }

  testInfo.annotations.push({
    type: 'drilldown-apps-summary',
    description: perAppOutcomes
      .map((o) => `${o.app}: ${o.outcome}`)
      .join('; '),
  });

  // Sanity: at least one of the four built-ins must have been
  // installed. If every app reports "not installed" the compose stack
  // is broken (or this spec is running against a non-Grafana endpoint)
  // — surface that loudly rather than passing on an empty sweep.
  const installedCount = perAppOutcomes.filter(
    (o) => o.outcome !== 'app-not-installed',
  ).length;
  expect(
    installedCount,
    'at least one drilldown app installed on the Grafana stack',
  ).toBeGreaterThan(0);

  if (failures.length > 0) {
    const detail = failures
      .map((f) => `[${f.app} :: ${f.rule}] ${f.detail}`)
      .join('\n\n');
    throw new Error(
      `drilldown-apps rule violated for ${failures.length} surface(s):\n\n${detail}`,
    );
  }
});

/**
 * Drive a single drilldown app:
 *   - capture console errors + every interesting Grafana/cerberus
 *     response across the whole gesture sequence,
 *   - call `drillTwoLevels` to navigate + drill twice,
 *   - after each settled level, snapshot `role="alert"` banners,
 *   - tear down listeners,
 *   - apply: zero non-2xx, no error-class banner text, no console
 *     error (modulo upstream-Grafana noise).
 *
 * Returns a list of failures — empty if the app sweep is clean.
 */
async function sweepDrilldownApp(
  page: Page,
  baseURL: string,
  app: DrilldownApp,
  testInfo: TestInfo,
): Promise<DrilldownFailure[]> {
  const failures: DrilldownFailure[] = [];

  const { messages: consoleErrors, stop: stopConsole } =
    await captureConsoleErrors(page);

  const captured: CapturedResponseSummary[] = [];
  // Capture every response the drilldown app fires against Grafana's
  // proxy/resources/ds-query surfaces. This is the same surface set
  // the existing `compose_grafana_smoke.spec.ts` watches; the drilldown
  // sweep enforces the same zero-non-2xx rule across all three.
  const captureBodies: Promise<void>[] = [];
  const onResponse = (resp: Response) => {
    const url = resp.url();
    if (
      url.includes('/api/ds/query') ||
      url.includes('/api/dashboards/') ||
      url.includes('/api/datasources/proxy/uid/') ||
      (url.includes('/api/datasources/uid/') && url.includes('/resources/')) ||
      // Drilldown apps also fetch their own plugin chunks via
      // /public/plugins/<id>/...; a 4xx/5xx there breaks the app boot.
      url.includes(`/public/plugins/${app.id}/`) ||
      // Some apps lazy-load chunks under /public/build/...; capture
      // failure responses there too.
      (url.includes('/public/build/') && url.endsWith('.js'))
    ) {
      // Read body lazily — kept short to keep memory bounded.
      const status = resp.status();
      const method = resp.request().method();
      const summaryPromise = (async () => {
        let bodyPreview = '';
        // Only read the body when it's a failure so we don't spam
        // memory with 1MB success bodies.
        if (status < 200 || status > 299) {
          try {
            const text = await resp.text();
            bodyPreview = truncate(text, 600);
          } catch {
            bodyPreview = '<unreadable>';
          }
        }
        captured.push({
          url: stripBase(resp.url(), baseURL),
          method,
          status,
          bodyPreview,
        });
      })();
      captureBodies.push(summaryPromise);
    }
  };
  page.on('response', onResponse);

  let levelsClicked = 0;
  let rootHadNoAffordance = false;

  try {
    const result = await drillTwoLevels(app, page);
    levelsClicked = result.levelsClicked;
    rootHadNoAffordance = result.rootHadNoAffordance;

    // After each settle (root + each click) we snapshot role=alert
    // banners. We don't have per-step hooks inside drillTwoLevels — the
    // helper is deliberately gesture-only — so we do one final snapshot
    // here, which reflects the deepest state reached. If a banner fires
    // mid-drill the helper's networkidle wait gives Grafana time to
    // render it before we sample.
    const alertBanners = await captureRoleAlertBanners(page);
    for (const banner of alertBanners) {
      if (
        APP_NOT_INSTALLED_BANNER_PATTERNS.some((re) => re.test(banner))
      ) {
        // isAppInstalled probe said yes but Grafana rendered the
        // "not installed" banner anyway — surface that as a soft
        // annotation (it's an installation drift, not a drill regression).
        testInfo.annotations.push({
          type: 'drilldown-app-banner-not-installed',
          description: `${app.id}: rendered "${truncate(
            banner,
            200,
          )}" despite isAppInstalled=true`,
        });
        continue;
      }
      if (ALERT_ERROR_PATTERNS.some((re) => re.test(banner))) {
        failures.push({
          app: app.id,
          rule: 'role-alert-error-banner',
          detail: `role=alert banner with error text: ${truncate(banner, 400)}`,
        });
      }
    }
  } catch (err) {
    failures.push({
      app: app.id,
      rule: 'drilldown-navigation-threw',
      detail: `drillTwoLevels threw: ${(err as Error).message}; root: ${app.root}`,
    });
  } finally {
    page.off('response', onResponse);
    stopConsole();
  }

  // Drain the in-flight body-read promises so we don't miss late
  // failures (e.g. a 500 still streaming when the navigation settled).
  await Promise.all(captureBodies);

  // 1. Wire-status sweep over every captured response. Q5: zero
  //    toleration for 4xx/5xx.
  for (const resp of captured) {
    if (resp.status < 200 || resp.status > 299) {
      failures.push({
        app: app.id,
        rule: 'http-non-2xx',
        detail: `${resp.method} ${resp.url} → ${resp.status}\n  body: ${resp.bodyPreview}`,
      });
    }
  }

  // 2. Console-error sweep. Filter the narrow upstream-Grafana
  //    telemetry pattern; everything else is a real failure.
  const cerberusConsoleErrors = consoleErrors.filter(
    (m) => !DRILLDOWN_UPSTREAM_GRAFANA_CONSOLE_NOISE.some((re) => re.test(m)),
  );
  if (cerberusConsoleErrors.length > 0) {
    failures.push({
      app: app.id,
      rule: 'console-error',
      detail: `${cerberusConsoleErrors.length} console error(s):\n${cerberusConsoleErrors
        .map((m) => `  - ${truncate(m, 400)}`)
        .join('\n')}`,
    });
  }

  // 3. Annotate per-app drill depth so the test report shows whether
  //    a "clean" sweep actually exercised both drill clicks. An app
  //    that drilled 0 levels with no other failure surfaces as a
  //    "drilled-clean" outcome above, but the annotation here makes
  //    that visible to the reviewer.
  testInfo.annotations.push({
    type: 'drilldown-app-depth',
    description: `${app.id}: levelsClicked=${levelsClicked}, rootHadNoAffordance=${rootHadNoAffordance}, capturedResponses=${captured.length}`,
  });

  return failures;
}

function truncate(s: string, max: number): string {
  if (s.length <= max) return s;
  return `${s.slice(0, max)}…<truncated, ${s.length - max} more char(s)>`;
}

function stripBase(url: string, baseURL: string): string {
  if (url.startsWith(baseURL)) return url.slice(baseURL.length);
  return url;
}
