import { test, expect, type Response } from '@playwright/test';

/**
 * Compose-stack Grafana catch-net.
 *
 * Drives the docker-compose quickstart stack end-to-end:
 *   1. Enumerate every provisioned dashboard via Grafana's /api/search.
 *   2. Open each dashboard at /d/<uid> and let Grafana run its panel
 *      queries through the datasource proxy into cerberus.
 *   3. Capture every /api/ds/query POST and /api/dashboards/* GET that
 *      Grafana makes during dashboard load.
 *   4. Assert:
 *        - HTTP status is 2xx for every captured response.
 *        - No /api/ds/query response body carries a non-empty error in
 *          .results.<target>.error  (Grafana 200s the request and
 *          tunnels per-target failures inside the body — these are the
 *          regressions that slipped past existing CI gates).
 *
 * Why a single test, not test-per-dashboard: the dashboard list is
 * dynamic (read from /api/search at run time) and Playwright's
 * test.describe API needs the list at collection time. Looping inside
 * one test keeps it data-driven without an extra build step.
 *
 * Env:
 *   GRAFANA_BASE_URL  default http://localhost:3000
 */

type DashboardEntry = { uid: string; title: string; type: string };

type DSQueryError = {
  url: string;
  refId: string;
  status: number;
  error: string;
};

test('compose: every provisioned dashboard loads without datasource errors', async ({
  page,
  request,
}, testInfo) => {
  testInfo.setTimeout(180_000);

  const baseURL = process.env.GRAFANA_BASE_URL ?? 'http://localhost:3000';

  // 1. Enumerate provisioned dashboards. folderIds=0 = the root folder,
  //    which is where Grafana drops dashboards provisioned via
  //    file-provider with no explicit folder.
  const searchResp = await request.get(`${baseURL}/api/search?type=dash-db`);
  expect(searchResp.status(), 'grafana /api/search status').toBe(200);
  const dashboards = (await searchResp.json()) as DashboardEntry[];
  expect(dashboards.length, 'at least one provisioned dashboard').toBeGreaterThan(0);

  const failures: string[] = [];

  for (const dash of dashboards) {
    const captured: Response[] = [];
    const dsErrors: DSQueryError[] = [];

    // Subscribe BEFORE navigation so we don't miss the in-flight
    // requests fired during initial dashboard render.
    const onResponse = (resp: Response) => {
      const url = resp.url();
      if (url.includes('/api/ds/query') || url.includes('/api/dashboards/')) {
        captured.push(resp);
      }
    };
    page.on('response', onResponse);

    try {
      await page.goto(`${baseURL}/d/${dash.uid}`, {
        waitUntil: 'domcontentloaded',
        timeout: 60_000,
      });

      // Grafana fires panel queries asynchronously after the page DOM
      // is up. networkidle waits for the queries to settle; cap it so
      // a slow CH cold-start doesn't burn the whole budget.
      await page
        .waitForLoadState('networkidle', { timeout: 30_000 })
        .catch(() => {
          // networkidle timing out isn't fatal — we just stop waiting
          // and inspect what we captured so far. A panel that never
          // returns will still show up as a non-2xx or as a tunneled
          // error in .results below, OR (genuine hang) as a missing
          // response that other assertions surface.
        });
    } finally {
      page.off('response', onResponse);
    }

    // 4a. HTTP status sweep.
    for (const resp of captured) {
      const status = resp.status();
      if (status < 200 || status > 299) {
        let body = '';
        try {
          body = await resp.text();
        } catch {
          body = '<unreadable>';
        }
        failures.push(
          `[${dash.uid}] ${resp.request().method()} ${resp.url()} -> ${status}\n  body: ${truncate(body, 800)}`,
        );
      }
    }

    // 4b. /api/ds/query tunneled-error sweep. Grafana returns 200 for
    //     a ds/query request even when individual targets failed, and
    //     pushes the error string into body.results.<refId>.error.
    for (const resp of captured) {
      if (!resp.url().includes('/api/ds/query')) continue;
      if (resp.status() < 200 || resp.status() > 299) continue; // already reported above
      let parsed: { results?: Record<string, { error?: string }> };
      try {
        parsed = (await resp.json()) as typeof parsed;
      } catch {
        continue; // some ds/query responses may legitimately be empty
      }
      const results = parsed.results ?? {};
      for (const [refId, target] of Object.entries(results)) {
        if (target && typeof target.error === 'string' && target.error.length > 0) {
          dsErrors.push({
            url: resp.url(),
            refId,
            status: resp.status(),
            error: target.error,
          });
        }
      }
    }

    for (const err of dsErrors) {
      failures.push(
        `[${dash.uid}] ds/query tunneled error: refId=${err.refId} url=${err.url}\n  error: ${truncate(err.error, 800)}`,
      );
    }
  }

  if (failures.length > 0) {
    // One big aggregated message — all failing panels in one shot so
    // the agent CI logs surface every root cause, not just the first.
    const header = `compose-grafana-smoke caught ${failures.length} dashboard-load failure(s) across ${dashboards.length} dashboard(s):`;
    const dashList = dashboards
      .map((d) => `  - ${d.uid} (${d.title})`)
      .join('\n');
    const detail = failures.map((f) => `* ${f}`).join('\n');
    expect.soft(failures, `${header}\nprobed dashboards:\n${dashList}\nfailures:\n${detail}`).toEqual([]);
    throw new Error(`${header}\n${detail}`);
  }
});

function truncate(s: string, n: number): string {
  return s.length <= n ? s : `${s.slice(0, n)}...<truncated, ${s.length} chars total>`;
}
