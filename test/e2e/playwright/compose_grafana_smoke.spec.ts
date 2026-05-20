import { test, expect, type Response, type Page } from '@playwright/test';

/**
 * Compose-stack Grafana catch-net.
 *
 * Drives the docker-compose quickstart stack end-to-end across multiple
 * surfaces:
 *   1. The Grafana home page (`/`) — many panels there pull from the
 *      provisioned cerberus datasources; a regression in the home
 *      dashboard or in the welcome-panel queries shows up here first.
 *   2. The Loki drilldown / Explore-Logs app
 *      (`/a/grafana-lokiexplore-app/explore?var-ds=cerberus-loki`) — this
 *      app drives `/api/datasources/uid/.../resources/detected_labels`
 *      and friends, which the dashboard loop never touches.
 *   3. Every provisioned dashboard enumerated via Grafana's
 *      `/api/search?type=dash-db`.
 *
 * For each surface we:
 *   - Capture every Grafana → datasource request:
 *       * `/api/ds/query`              — the classic query proxy
 *       * `/api/dashboards/`           — dashboard fetch
 *       * `/api/datasources/proxy/uid/` — datasource proxy used by some
 *                                          plugins, alerting evals, and
 *                                          Tempo /buildinfo probes
 *       * `/api/datasources/uid/.../resources/...` — used by Loki for
 *                                          `/detected_labels`, by Tempo
 *                                          for tag enumeration, etc.
 *   - Assert:
 *       * HTTP status is 2xx for every captured response.
 *       * No `/api/ds/query` response body carries a non-empty error in
 *         `.results.<refId>.error` (Grafana 200s the request and tunnels
 *         per-target failures inside the body).
 *       * No panel is still in the "loading" state after we wait for
 *         network idle — a stuck spinner means the query never resolved.
 *       * No panel renders Grafana's red error-state banner.
 *
 * Failures are aggregated and reported with a uniform label
 *   `[<kind>:<label>] <issue>: <detail>`
 * so a maintainer scanning the CI log learns which surface broke from
 * the first line of each failure.
 *
 * Env:
 *   GRAFANA_BASE_URL  default http://localhost:3000
 */

type DashboardEntry = { uid: string; title: string; type: string };

type Surface = {
  kind: string; // 'home' | 'app:<name>' | 'dash:<uid>'
  label: string; // human-readable surface label, used in failure messages
  url: string;
};

type DSQueryError = {
  url: string;
  refId: string;
  status: number;
  error: string;
};

test('compose: home, drilldown app, and every provisioned dashboard load without datasource errors', async ({
  page,
  request,
}, testInfo) => {
  // The drilldown app + multi-surface sweep is heavier than the old
  // dashboard-only loop; bump the overall budget to 6 minutes.
  testInfo.setTimeout(360_000);

  const baseURL = process.env.GRAFANA_BASE_URL ?? 'http://localhost:3000';

  // 1. Enumerate provisioned dashboards via /api/search. The dashboard
  //    list is dynamic at run time, so we read it here and stitch it
  //    into the fixed-surfaces list below.
  const searchResp = await request.get(`${baseURL}/api/search?type=dash-db`);
  expect(searchResp.status(), 'grafana /api/search status').toBe(200);
  const dashboards = (await searchResp.json()) as DashboardEntry[];
  expect(dashboards.length, 'at least one provisioned dashboard').toBeGreaterThan(0);

  // 2. Fixed surfaces the maintainer keeps hitting that the dynamic
  //    dashboard loop misses entirely.
  //
  //    Explore-mode entries each exercise one of the regressions a 2026-05-20
  //    corner-sweep surfaced and four PRs (#630–#633) fixed:
  //      - prom-up:                 Grafana sends ms-timestamps over
  //                                 /api/datasources/uid/.../resources/...
  //                                 — overflow before #630 made every
  //                                 Prom Explore page 502.
  //      - prom-cerberus-metric:    the self-telemetry stream the
  //                                 cerberus-self dashboard targets;
  //                                 catches dotted-vs-underscored regressions.
  //      - loki-allstreams:         exercises `/loki/api/v1/query_range`
  //                                 with a permissive selector; also
  //                                 triggers the Loki CheckHealth probe
  //                                 (`vector(1)+vector(1)`) on the
  //                                 datasource — caught the ResourceAttributes
  //                                 scope error fixed by #631.
  //      - tempo-search:            `{}` against `/api/search`; also fires
  //                                 the buildinfo probe (#633) + the V2
  //                                 tag-values probe.
  const exploreLeft = (ds: string, kind: 'prom' | 'loki' | 'tempo', expr: string) => {
    const queryType = kind === 'loki' ? 'range' : undefined;
    const state: any = {
      datasource: ds,
      queries: [{ refId: 'A', expr, datasource: { type: kind, uid: ds }, queryType }],
      range: { from: 'now-1h', to: 'now' },
    };
    return `${baseURL}/explore?orgId=1&left=${encodeURIComponent(JSON.stringify(state))}`;
  };

  const fixedSurfaces: Surface[] = [
    { kind: 'home', label: '/', url: `${baseURL}/` },
    {
      kind: 'app:lokiexplore',
      label: '/a/grafana-lokiexplore-app',
      url: `${baseURL}/a/grafana-lokiexplore-app/explore?var-ds=cerberus-loki`,
    },
    { kind: 'explore:prom', label: 'prom up', url: exploreLeft('cerberus-prometheus', 'prom', 'up') },
    {
      kind: 'explore:prom',
      label: 'prom cerberus_queries_total',
      url: exploreLeft('cerberus-prometheus', 'prom', 'cerberus_queries_total'),
    },
    {
      kind: 'explore:loki',
      label: 'loki {service_name=~".+"}',
      url: exploreLeft('cerberus-loki', 'loki', '{service_name=~".+"}'),
    },
    {
      kind: 'explore:tempo',
      label: 'tempo {}',
      url: exploreLeft('cerberus-tempo', 'tempo', '{}'),
    },
  ];

  const dashboardSurfaces: Surface[] = dashboards.map((d) => ({
    kind: `dash:${d.uid}`,
    label: d.title,
    url: `${baseURL}/d/${d.uid}`,
  }));

  const surfaces: Surface[] = [...fixedSurfaces, ...dashboardSurfaces];

  const failures: string[] = [];

  for (const surface of surfaces) {
    const captured: Response[] = [];

    // Subscribe BEFORE navigation so we don't miss the in-flight
    // requests fired during initial render.
    const onResponse = (resp: Response) => {
      const url = resp.url();
      if (
        url.includes('/api/ds/query') ||
        url.includes('/api/dashboards/') ||
        // Datasource-proxy used by drilldown plugins + Tempo
        // /buildinfo + alerting evals.
        url.includes('/api/datasources/proxy/uid/') ||
        // Datasource resources endpoint: Loki `/detected_labels`,
        // Tempo tag enumeration, etc.
        (url.includes('/api/datasources/uid/') && url.includes('/resources/'))
      ) {
        captured.push(resp);
      }
    };
    page.on('response', onResponse);

    try {
      await page.goto(surface.url, {
        waitUntil: 'domcontentloaded',
        timeout: 90_000,
      });

      // Grafana fires panel queries asynchronously after the page DOM
      // is up. networkidle waits for the queries to settle; cap it so
      // a slow CH cold-start doesn't burn the whole budget. The
      // drilldown app boots heavier than a plain dashboard (async
      // plugin chunk loads), so 45s, not 30s.
      await page
        .waitForLoadState('networkidle', { timeout: 45_000 })
        .catch(() => {
          // networkidle timing out isn't fatal — we just stop waiting
          // and inspect what we captured so far. The stuck-loading
          // sweep below will surface a panel that never resolved.
        });
    } finally {
      page.off('response', onResponse);
    }

    // 3a. HTTP status sweep over every captured response.
    for (const resp of captured) {
      const status = resp.status();
      if (status < 200 || status > 299) {
        const method = resp.request().method();
        const path = stripBase(resp.url(), baseURL);
        if (isKnownTolerated404(status, path)) {
          // Documented surface that cerberus does not yet implement and
          // whose 404 has no UI / dashboard consequence. The list is
          // narrow on purpose — see isKnownTolerated404 for the
          // per-path rationale.
          continue;
        }
        let body = '';
        try {
          body = await resp.text();
        } catch {
          body = '<unreadable>';
        }
        failures.push(
          `[${surface.kind}:${surface.label}] http: ${method} ${path} → ${status}\n  body: ${truncate(body, 800)}`,
        );
      }
    }

    // 3b. /api/ds/query tunneled-error sweep. Grafana returns 200 for
    //     a ds/query request even when individual targets failed, and
    //     pushes the error string into body.results.<refId>.error.
    for (const resp of captured) {
      if (!resp.url().includes('/api/ds/query')) continue;
      if (resp.status() < 200 || resp.status() > 299) continue;
      let parsed: { results?: Record<string, { error?: string }> };
      try {
        parsed = (await resp.json()) as typeof parsed;
      } catch {
        continue; // some ds/query responses may legitimately be empty
      }
      const results = parsed.results ?? {};
      for (const [refId, target] of Object.entries(results)) {
        if (target && typeof target.error === 'string' && target.error.length > 0) {
          const dsErr: DSQueryError = {
            url: stripBase(resp.url(), baseURL),
            refId,
            status: resp.status(),
            error: target.error,
          };
          failures.push(
            `[${surface.kind}:${surface.label}] ds-query: refId=${dsErr.refId} url=${dsErr.url}\n  error: ${truncate(dsErr.error, 800)}`,
          );
        }
      }
    }

    // 3c. DOM-level stuck-loading sweep. The Grafana 11.x panel
    //     wrapper carries `data-testid="data-testid Panel header <title>"`
    //     (yes, the literal "data-testid " prefix is part of the value
    //     — that's Grafana's @grafana/e2e-selectors convention). A
    //     panel that is still rendering its loading state exposes a
    //     spinner via `[data-testid="data-testid Panel header loading"]`
    //     or class `panel-loading`. We accept either selector to be
    //     resilient to small Grafana version skew. See
    //     https://github.com/grafana/grafana/blob/main/packages/grafana-e2e-selectors/src/selectors/components.ts
    const stuckLoading = await collectStuckLoadingPanels(page);
    for (const title of stuckLoading) {
      failures.push(
        `[${surface.kind}:${surface.label}] stuck-loading: panel "${title}"`,
      );
    }

    // 3d. DOM-level panel-error sweep. A panel that resolved its query
    //     but errored renders Grafana's red error-state banner. The
    //     stable selector across Grafana 11.x is the "Panel status"
    //     testid; the visible error message lives in the tooltip /
    //     status icon's aria-label.
    const panelErrors = await collectPanelErrors(page);
    for (const { title, message } of panelErrors) {
      failures.push(
        `[${surface.kind}:${surface.label}] panel-error: panel "${title}"\n  message: ${truncate(message, 400)}`,
      );
    }
  }

  // 4. Datasource-health probes. Grafana calls
  //    `/api/datasources/uid/<uid>/health` per datasource on every page
  //    load + when the user clicks "Save & test". A non-200 here
  //    surfaces in the Grafana UI as a red "Unable to connect"
  //    banner — exactly the failure mode #631 + #633 fixed.
  //
  //    Probing these explicitly catches regressions even when no
  //    dashboard / Explore page happens to trigger the probe under
  //    the load-state we waited for above.
  // cerberus-tempo is intentionally excluded: Grafana's Tempo
  // datasource plugin does not implement a backend CheckHealth method,
  // so `/api/datasources/uid/cerberus-tempo/health` always returns 404
  // with `{"messageId":"plugin.notImplemented",...}` — that's a Grafana
  // plugin shape, not a cerberus 404. The Tempo per-page-load buildinfo
  // probe + the dashboard-driven /api/ds/query sweep above still cover
  // the cerberus tempo surface for regressions (see #633).
  const probedDatasources = ['cerberus-prometheus', 'cerberus-loki'];
  for (const ds of probedDatasources) {
    const resp = await request.get(`${baseURL}/api/datasources/uid/${ds}/health`);
    const body = await resp.text();
    if (resp.status() < 200 || resp.status() > 299) {
      failures.push(
        `[health:${ds}] datasource health probe → ${resp.status()}\n  body: ${truncate(body, 600)}`,
      );
      continue;
    }
    // Grafana's contract for /health is `{status, message}`; "ERROR"
    // is the documented failure value (mirrors the red UI banner).
    try {
      const parsed = JSON.parse(body) as { status?: string; message?: string };
      if (parsed.status && parsed.status !== 'OK' && parsed.status !== 'success') {
        failures.push(
          `[health:${ds}] datasource health status=${parsed.status} message=${truncate(parsed.message ?? '', 240)}`,
        );
      }
    } catch {
      // non-JSON body is suspicious for /health but not fatal on its
      // own; the 2xx check above is the load-bearing assertion.
    }
  }

  if (failures.length > 0) {
    const header = `compose-grafana-smoke caught ${failures.length} failure(s) across ${surfaces.length} surface(s):`;
    const surfaceList = surfaces
      .map((s) => `  - [${s.kind}] ${s.label}`)
      .join('\n');
    const detail = failures.map((f) => `* ${f}`).join('\n');
    expect
      .soft(failures, `${header}\nprobed surfaces:\n${surfaceList}\nfailures:\n${detail}`)
      .toEqual([]);
    throw new Error(`${header}\n${detail}`);
  }
});

/**
 * Find panels still in the loading state. Returns the panel titles.
 *
 * Grafana 11.x panels expose two stable signals for "still loading":
 *   1. A spinner element with testid `data-testid Panel header loading`
 *      (the literal "data-testid " prefix is part of the value —
 *      that's how @grafana/e2e-selectors flattens its strings).
 *   2. Legacy class `.panel-loading` still present on some panel
 *      wrappers in 11.x.
 * We OR these two together to be resilient to small version skew.
 */
async function collectStuckLoadingPanels(page: Page): Promise<string[]> {
  const titles = await page
    .locator(
      [
        '[data-testid="data-testid Panel header loading"]',
        '.panel-loading',
        '[aria-label="Loading"]',
      ].join(', '),
    )
    .evaluateAll((nodes) =>
      nodes.map((node) => {
        // Walk up to the panel container and read its title. The
        // container is identified by a testid that starts with
        // "data-testid Panel header ". The title text node is the
        // header h2 / h6 inside the panel chrome.
        let cur: Element | null = node;
        for (let i = 0; i < 8 && cur; i++) {
          const titleEl =
            cur.querySelector?.('[data-testid="data-testid Panel header title"]') ??
            cur.querySelector?.('header h6, header h2, .panel-title');
          if (titleEl && titleEl.textContent) {
            return titleEl.textContent.trim();
          }
          cur = cur.parentElement;
        }
        return '<untitled panel>';
      }),
    );
  // Deduplicate so the same panel doesn't show up twice when both the
  // spinner and the legacy class match.
  return Array.from(new Set(titles));
}

/**
 * Find panels currently rendering Grafana's red error-state banner.
 * Returns the panel title plus the visible error message (aria-label of
 * the status icon, which Grafana populates with the actual error text).
 */
async function collectPanelErrors(
  page: Page,
): Promise<Array<{ title: string; message: string }>> {
  return await page
    .locator(
      [
        '[data-testid="data-testid Panel status error"]',
        '[data-testid="data-testid Panel header error"]',
      ].join(', '),
    )
    .evaluateAll((nodes) =>
      nodes.map((node) => {
        const message =
          node.getAttribute('aria-label') ??
          node.getAttribute('title') ??
          node.textContent?.trim() ??
          '<no error message>';
        let cur: Element | null = node;
        let title = '<untitled panel>';
        for (let i = 0; i < 8 && cur; i++) {
          const titleEl =
            cur.querySelector?.('[data-testid="data-testid Panel header title"]') ??
            cur.querySelector?.('header h6, header h2, .panel-title');
          if (titleEl && titleEl.textContent) {
            title = titleEl.textContent.trim();
            break;
          }
          cur = cur.parentElement;
        }
        return { title, message };
      }),
    );
}

function truncate(s: string, n: number): string {
  return s.length <= n ? s : `${s.slice(0, n)}...<truncated, ${s.length} chars total>`;
}

function stripBase(url: string, base: string): string {
  return url.startsWith(base) ? url.slice(base.length) : url;
}

/**
 * Surfaces whose 404 has no user-visible consequence and that we
 * therefore tolerate while the corresponding implementation PR lands.
 *
 * Keep this list intentionally narrow — each entry is a known gap
 * tracked by an open PR. When the PR merges, drop the entry so the
 * catch-net snaps back to "every captured request is 2xx".
 *
 * Current entries:
 *
 *   * `/api/datasources/uid/cerberus-prometheus/resources/api/v1/rules`
 *   * `/api/datasources/uid/cerberus-prometheus/resources/api/v1/alerts`
 *     Grafana's Prom datasource polls /api/v1/rules + /api/v1/alerts on
 *     every Explore / page load to gate the "Alert Rules" / "Alerts"
 *     UI affordances. cerberus is a query gateway with no rule engine,
 *     so PR #632 ships an empty-envelope stub. Until #632 merges,
 *     tolerate the 404 — the affordance simply renders empty, no panel
 *     or dashboard surface degrades.
 */
function isKnownTolerated404(status: number, path: string): boolean {
  if (status !== 404) return false;
  return (
    path.includes('/api/datasources/uid/cerberus-prometheus/resources/api/v1/rules') ||
    path.includes('/api/datasources/uid/cerberus-prometheus/resources/api/v1/alerts')
  );
}
