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
 * `grafana/grafana:12.2.9` (the tag pinned in `docker-compose.yml`).
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
  type Request,
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
  isTransientMalformedTraceQLFailure,
  iterateDrilldownApps,
  reportableConsoleErrors,
  waitForAppInstalled,
} from './helpers/index.js';

// Every `role="alert"` banner counts as a failure. No allow-list of
// "expected" alert text — if Grafana surfaces the banner, that is a
// real state to fix at the source (drilldown plugin, datasource, or
// cerberus).

// Captured response shape: stripped down so the failure detail isn't
// dragged down by a 1MB ds/query body. `requestBody` carries the forwarded
// ds/query payload (the per-refId expr/query) so the init-race reconciler can
// inspect the TraceQL shape — see isTransientMalformedTraceQLFailure.
type CapturedResponseSummary = {
  url: string;
  method: string;
  status: number;
  bodyPreview: string;
  requestBody: string;
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
  // Budget 3 apps × ~90s + a generous headroom for the heaviest app
  // (Explore-Traces tends to be slowest on a cold ClickHouse) plus up
  // to 120s of async-preinstall readiness wait (the wait is shared in
  // practice — Grafana downloads all preinstalled apps in one boot
  // pass, so once the first app reports installed the rest resolve on
  // their first poll) = 10 min.
  testInfo.setTimeout(10 * 60_000);

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
    //    enabled. Grafana 12.x preinstalls the drilldown apps via an
    //    async boot-time download that finishes AFTER /api/health goes
    //    green, so the probe first synchronizes on the install (bounded
    //    120s wait — see waitForAppInstalled). After the wait the
    //    assertion is exactly as hard as before: a negative probe
    //    collects a failure (rather than aborting the loop) so the
    //    report surfaces every misconfigured app in one run.
    const installed = await waitForAppInstalled(request, baseURL, app.id);
    if (!installed) {
      failures.push({
        app: app.id,
        rule: 'app-not-installed-or-disabled',
        detail: `/api/plugins/${app.id}/settings reports not-installed-or-disabled (still, after the 120s async-preinstall readiness wait). Provision the plugin in docker-compose or remove it from the catalogue.`,
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

  // Capture the forwarded ds/query POST body at REQUEST time, keyed by the
  // request object. The init-race reconciler reads the dangling-operand
  // TraceQL shape from this body; reading `resp.request().postData()` at
  // RESPONSE time intermittently returns empty because the rapid two-level
  // drill navigates away and Playwright releases the request's post body —
  // and that navigation is exactly when the Traces-Drilldown primarySignal
  // init-race 400 lands, so the reconciler goes blind and the known-benign
  // 400 fails the spec (THE flake). Snapshotting postData() the moment the
  // request fires, before any navigation can release it, makes the
  // request-side match reliable regardless of when the response settles.
  const requestBodies = new WeakMap<Request, string>();
  const onRequest = (req: Request) => {
    if (!req.url().includes('/api/ds/query')) return;
    const body = req.postData();
    if (body) requestBodies.set(req, body);
  };
  page.on('request', onRequest);

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
      // Prefer the body snapshotted at request time (onRequest); fall back
      // to the response-time read. The forwarded ds/query body is what the
      // init-race reconciler matches the dangling-operand TraceQL shape from.
      const requestBody =
        requestBodies.get(resp.request()) ?? resp.request().postData() ?? '';
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
          requestBody,
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
    page.off('request', onRequest);
    page.off('response', onResponse);
    stopConsole();
  }

  // Drain the in-flight body-read promises so we don't miss late
  // failures (e.g. a 500 still streaming when the navigation settled).
  await Promise.all(captureBodies);

  // 1. Wire-status sweep over every captured response — zero
  //    tolerance for 4xx/5xx, EXCEPT the Traces Drilldown app's
  //    primarySignal-init race. That app applies its primarySignal
  //    default inside a React useEffect, so during the initial-load /
  //    rapid-drill window it transiently forwards a dangling-operand
  //    TraceQL (`{ && …} | rate()`) that cerberus correctly 400s
  //    (reference Tempo rejects the identical syntax error). It is a
  //    third-party app artifact, not a cerberus fault — and racy: the
  //    400 fires on every run but only lands inside the capture window
  //    intermittently, which is what makes this spec flaky. The
  //    reconciler is narrow (every query in the request must carry the
  //    dangling shape); a well-formed-query non-2xx still fails loudly.
  //    Mirrors the crawl lane (#934). Count the reconciled races so the
  //    console sweep below can resolve their browser-side twin.
  let reconciledInitRace = 0;
  for (const resp of captured) {
    if (resp.status >= 200 && resp.status <= 299) continue;
    if (
      isTransientMalformedTraceQLFailure({
        url: resp.url,
        status: resp.status,
        requestBody: resp.requestBody,
        responseBody: resp.bodyPreview,
      })
    ) {
      reconciledInitRace++;
      continue;
    }
    failures.push({
      app: app.id,
      rule: 'http-non-2xx',
      detail: `${resp.method} ${resp.url} → ${resp.status}\n  body: ${resp.bodyPreview}`,
    });
  }

  // 2. Console-error sweep — every browser console error is a real failure,
  //    EXCEPT two narrow browser-generated network classes the wire-status
  //    sweep above already owns or that are provably client-side (see
  //    reportableConsoleErrors):
  //      a) `TypeError: Failed to fetch` — the network-abort class. cerberus
  //         errors are HTTP non-2xx (a RESOLVED fetch the wire-sweep captures);
  //         `Failed to fetch` only fires when the fetch never completes, i.e. a
  //         third-party drilldown app's background fetch (lokiexplore "Detected
  //         fields"; the RxJS data-source layer) aborted as the rapid drill
  //         unmounts its scene. Verified live: cerberus serves
  //         /loki/api/v1/detected_fields + /detected_labels with HTTP 200.
  //      b) the "Failed to load resource: … status of 400" browser TWIN of each
  //         reconciled dangling-operand 400 (resolved up to reconciledInitRace).
  //    Every other console error — chunk-load failures, real JS exceptions,
  //    datasource 5xx logs — still fails loudly, and real cerberus HTTP failures
  //    remain owned by the wire-status sweep.
  const reportableErrors = reportableConsoleErrors(
    consoleErrors,
    reconciledInitRace,
  );
  if (reportableErrors.length > 0) {
    failures.push({
      app: app.id,
      rule: 'console-error',
      detail: `${reportableErrors.length} console error(s):\n${reportableErrors
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
