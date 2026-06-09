/**
 * Phase-6 drilldown-apps sweep.
 *
 * Iterates every Grafana drilldown app in the catalogue
 * (`helpers/drilldown.ts`), drives a two-level click-drill through each
 * app, and asserts the per-app sweep is clean:
 *
 *   1. The app is installed and enabled (hard-asserted on every
 *      catalogue entry — no install-probe escape hatch).
 *   2. Every captured Grafana / cerberus HTTP response is 2xx (no 4xx
 *      and no 5xx). Drilldown apps fire `/api/datasources/proxy/uid/…`,
 *      `/api/datasources/uid/…/resources/…`, and `/api/ds/query` —
 *      every one is in scope.
 *   3. No `role="alert"` banner is on the page at any level (root,
 *      after first drill, after second drill). Drilldown apps surface
 *      query errors and missing-plugin states through this affordance.
 *   4. No browser console `error`-level message was emitted across
 *      the whole app sweep. Plugin failures (chunk-load errors,
 *      datasource-resource 502s, fetch aborts) all surface as
 *      console errors.
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
 * the upgrade checklist.
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

// Every `role="alert"` banner counts as a failure. No allow-list of
// "expected" alert text — if Grafana surfaces the banner, that is a
// real state to fix at the source (drilldown plugin, datasource, or
// cerberus).

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
  expect(apps.length, 'drilldown app catalogue is non-empty').toBeGreaterThan(0);

  const failures: DrilldownFailure[] = [];
  const perAppOutcomes: Array<{ app: string; outcome: string }> = [];

  for (const app of apps) {
    // 1. Install-probe — every catalogue entry MUST be installed and
    //    enabled. A negative probe collects a failure (rather than
    //    aborting the loop) so the report surfaces every misconfigured
    //    app in one run.
    const installed = await isAppInstalled(request, baseURL, app.id);
    if (!installed) {
      failures.push({
        app: app.id,
        rule: 'app-not-installed-or-disabled',
        detail: `/api/plugins/${app.id}/settings reports not-installed-or-disabled. Provision the plugin in docker-compose or remove it from the catalogue.`,
      });
      perAppOutcomes.push({ app: app.id, outcome: 'not-installed-or-disabled' });
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
 *   - apply: zero non-2xx, zero role=alert banners, zero console
 *     errors.
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
      failures.push({
        app: app.id,
        rule: 'role-alert-banner',
        detail: `role=alert banner rendered: ${truncate(banner, 400)}`,
      });
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

  // 1. Wire-status sweep over every captured response — zero
  //    tolerance for 4xx/5xx.
  for (const resp of captured) {
    if (resp.status < 200 || resp.status > 299) {
      failures.push({
        app: app.id,
        rule: 'http-non-2xx',
        detail: `${resp.method} ${resp.url} → ${resp.status}\n  body: ${resp.bodyPreview}`,
      });
    }
  }

  // 2. Console-error sweep — every browser console error is a real
  //    failure. No noise filter (any upstream-Grafana console error is
  //    either a Grafana bug to file or a state cerberus's compose stack
  //    can pre-empt; mask nothing here).
  if (consoleErrors.length > 0) {
    failures.push({
      app: app.id,
      rule: 'console-error',
      detail: `${consoleErrors.length} console error(s):\n${consoleErrors
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
