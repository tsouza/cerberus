/**
 * HTTP probe helpers used by the phase specs to fetch + assert on
 * Grafana / cerberus endpoints out-of-band of the page-driven sweep.
 *
 * Two surfaces:
 *
 *   1. `fetchAndAssert200` — GET a URL via Playwright's
 *      `APIRequestContext`, assert 2xx, return the parsed JSON. The
 *      assertion enforces the zero-404-toleration policy (Q5,
 *      /home/thiago/.claude/plans/e2e-enhance.md §9): every non-2xx
 *      is a failure; no allow-list.
 *
 *   2. `extractDataSourceProxyURL` — given a Dashboard + Panel +
 *      PanelTarget, compute the
 *      `/api/datasources/proxy/uid/<ds-uid>/api/...` URL Grafana
 *      itself would fire when the panel renders. The histogram
 *      `_bucket`-presence probe (assertHistogramComplete) calls this
 *      to fire a `/api/v1/series?match[]=...` probe against the same
 *      datasource the panel uses.
 */

import type { APIRequestContext, Page } from '@playwright/test';
import type { Dashboard, Panel, PanelTarget } from './dashboard.js';

export type JsonResponse = unknown;

/**
 * GET `url` and parse the body as JSON. Throws on non-2xx (the Q5
 * zero-toleration gate) or on non-JSON body.
 *
 * The `page` argument is accepted but currently unused — passed for
 * forward-compatibility with a future variant that derives the
 * request context from the page's BrowserContext.
 */
export async function fetchAndAssert200(
  url: string,
  request: APIRequestContext,
  _page?: Page,
): Promise<JsonResponse> {
  const resp = await request.get(url);
  const status = resp.status();
  if (status < 200 || status > 299) {
    let body = '';
    try {
      body = await resp.text();
    } catch {
      body = '<unreadable>';
    }
    throw new Error(
      `fetchAndAssert200: GET ${url} → ${status}\n  body: ${truncate(body, 600)}`,
    );
  }
  try {
    return (await resp.json()) as JsonResponse;
  } catch (err) {
    const body = await resp.text().catch(() => '<unreadable>');
    throw new Error(
      `fetchAndAssert200: GET ${url} → 2xx but body is not valid JSON: ${
        (err as Error).message
      }\n  body: ${truncate(body, 600)}`,
    );
  }
}

/**
 * Build the `/api/datasources/proxy/uid/<ds-uid>/...` URL the phase
 * specs use to probe a datasource directly.
 *
 * Resolution order for the datasource uid:
 *   1. `target.datasource?.uid` — the panel target's own override.
 *   2. `panel.datasource?.uid` — the panel's default.
 *   3. throw — every Grafana 11.x panel target carries one of these;
 *      if neither is set, the dashboard JSON is malformed and the
 *      phase spec should fail loudly rather than guess.
 *
 * `dashboard` is currently unused but accepted to match the brief's
 * signature; future variants may need it to resolve a datasource
 * variable (`${DS_PROMETHEUS}`) via `dashboard.templating.list`.
 *
 * The returned URL is *relative* to the Grafana base — concatenate
 * with the baseURL before firing:
 *
 *   const path = extractDataSourceProxyURL(d, p, t);
 *   await fetchAndAssert200(`${baseURL}${path}/api/v1/series?...`, req);
 */
export function extractDataSourceProxyURL(
  dashboard: Dashboard,
  panel: Panel,
  target: PanelTarget,
): string {
  const uid = target.datasource?.uid ?? panel.datasource?.uid;
  if (!uid || uid === '') {
    throw new Error(
      `extractDataSourceProxyURL: dashboard "${dashboard.title}" panel "${panel.title}" target ${target.refId} has no datasource uid`,
    );
  }
  return `/api/datasources/proxy/uid/${uid}`;
}

function truncate(s: string, n: number): string {
  return s.length <= n ? s : `${s.slice(0, n)}...<truncated, ${s.length} chars total>`;
}
