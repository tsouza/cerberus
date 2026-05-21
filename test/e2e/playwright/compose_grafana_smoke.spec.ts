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
    //
    // Zero 404 toleration (Q5, /home/thiago/.claude/plans/e2e-enhance.md
    // §9.5): every non-2xx captured during the sweep is a failure. The
    // prior `isKnownTolerated404` allow-list was retired in PR
    // test/e2e-phase-7-retire-404-allow-list (task #230) — its last
    // remaining entries (`/api/v1/rules` + `/api/v1/alerts`) became 200
    // when PR #632 merged. The fix for any new 404 is to implement the
    // endpoint or to remove that surface from the iteration, not to
    // extend an allow-list. The new iterate-* specs already enforce the
    // same policy via helpers/assertions.ts::assertNon200ResponseClass.
    for (const resp of captured) {
      const status = resp.status();
      if (status < 200 || status > 299) {
        const method = resp.request().method();
        const path = stripBase(resp.url(), baseURL);
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

  // 5. Trace-click drill-through.
  //
  //    Grafana 11.x's Tempo datasource plugin sends
  //    `Accept: application/protobuf` to `/api/traces/{id}` and
  //    `proto.Unmarshal`-s the body into a `tempopb.Trace`. cerberus
  //    used to always return JSON, which surfaced on the Grafana side
  //    as `proto: illegal wireType …` → an `/api/ds/query` 500 + a red
  //    "Query error" banner in the trace view. The fix is the proto
  //    Accept branch on the cerberus handler; this drill-through
  //    asserts the round trip is clean.
  //
  //    We locate the "Slow cerberus traces" panel on cerberus-self,
  //    click the first row's trace-ID link, wait for Grafana's
  //    `/explore` navigation, and re-run the same `/api/ds/query` +
  //    DOM error sweeps over the new view. If no traces exist on the
  //    dev stack (the seeder hasn't run, or the dashboard panel
  //    renders "No data"), the click is a no-op — we skip the
  //    assertion rather than fail on missing fixture data.
  const traceClickFailures = await driveTraceClick(page, baseURL);
  failures.push(...traceClickFailures);

  // 6. Underscored-OTel-label partition sweep.
  //
  //    The cerberus-self dashboard's "Query rate by language" panel
  //    fires `sum by (cerberus_ql) (rate(cerberus_queries_total[5m]))`.
  //    OTel writes the `cerberus.ql` attribute under the dotted form
  //    in storage; the matcher-side lookup must cross the
  //    dot↔underscore boundary or the partition collapses to a
  //    single anonymous "Value" series. The sweep asserts the panel's
  //    legend / table renders ≥ 2 distinct grouped series — the three
  //    cerberus heads (`promql`, `logql`, `traceql`) are the seed
  //    contract, and any ≥ 2 catches the regression cleanly.
  const partitionFailures = await driveCerberusQLPartition(page, baseURL);
  failures.push(...partitionFailures);

  // 7. LogQL by-severity partition sweep.
  //
  //    The otel-fixture-explorer dashboard's "Log volume by severity"
  //    panel fires `sum by (SeverityText) (rate({service_name=~".+"}
  //    [5m]))`. SeverityText is a top-level otel_logs column, not a
  //    key inside ResourceAttributes — pre-fix, the lowering looked it
  //    up as `ResourceAttributes['SeverityText']` and the panel
  //    collapsed every row into a single anonymous series. Task #218
  //    plumbed the outer by-clause down so the inner range identity
  //    surfaces SeverityText into the augmented map. The sweep asserts
  //    the panel's response renders ≥ 2 distinct severity legend
  //    entries (compose seeds INFO / WARN / ERROR rows, so a healthy
  //    stack yields 3).
  const severityFailures = await driveSeverityPartition(page, baseURL);
  failures.push(...severityFailures);

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
 * Drive the cerberus-self → "Slow cerberus traces" panel → click a
 * trace row → land on /explore drill-through and assert no
 * ds/query 500, no DOM error banner, no "illegal wireType" text.
 *
 * Returns a list of failure strings (empty if the flow is clean OR
 * if no clickable trace row exists on the panel — both are
 * acceptable states on the compose-stack catch-net, which only
 * asserts when there's something to click).
 */
async function driveTraceClick(page: Page, baseURL: string): Promise<string[]> {
  const failures: string[] = [];

  // 1. Navigate to the cerberus-self dashboard and wait for panels to settle.
  await page.goto(`${baseURL}/d/cerberus-self`, {
    waitUntil: 'domcontentloaded',
    timeout: 90_000,
  });
  await page
    .waitForLoadState('networkidle', { timeout: 45_000 })
    .catch(() => {
      // Stuck loading is reported by the sweep above; here we just
      // proceed with whatever rendered.
    });

  // 2. Find a clickable trace-ID link inside the panel header
  //    "Slow cerberus traces". The Grafana table panel renders the
  //    `traceID` column as anchor `<a>` elements whose href starts
  //    with `/explore?...` — that's the affordance we want.
  const panelTitle = 'Slow cerberus traces (>100ms)';
  const panelLocator = page.locator(
    `[data-testid="data-testid Panel header ${panelTitle}"]`,
  );
  if ((await panelLocator.count()) === 0) {
    // Dashboard not provisioned in this stack — nothing to click; the
    // dashboard sweep above already covers the "panel exists" case.
    return failures;
  }

  // The link selector tolerates Grafana 11.x's two anchor renderings
  // (data-link cell vs button-style cell). We restrict by panel
  // ancestry so we don't pick a link from a sibling panel.
  const panelContainer = panelLocator
    .locator(
      'xpath=ancestor::*[@data-testid and starts-with(@data-testid, "data-testid Panel container")][1]',
    )
    .first();
  const traceLink = panelContainer
    .locator('a[href*="/explore"]')
    .first();

  // Wait briefly for the panel data to settle; if the panel renders
  // "No data" the count stays 0 and we skip the click.
  const linkCount = await traceLink.count();
  if (linkCount === 0) {
    return failures;
  }

  // 3. Subscribe to responses BEFORE the click so we catch the
  //    /api/ds/query the drill-through fires AND any direct
  //    trace-by-id call Grafana proxies through.
  //
  //    Grafana 11.x's Tempo datasource defaults to `tempoApiVersion >=
  //    v2`, which means trace drill-downs route through
  //    `/api/v2/traces/<id>` (via the datasource proxy / resources
  //    endpoint), not the legacy `/api/traces/<id>`. We capture every
  //    trace-by-id URL the page fires so the assertion below can pin
  //    "the request landed on v2" — without that gate, a regression
  //    that drops the v2 alias from cerberus's Mount() would silently
  //    404 the trace view (task #208).
  const captured: Response[] = [];
  const traceByIDRequests: { url: string; status: number }[] = [];
  const onResponse = (resp: Response) => {
    const url = resp.url();
    if (url.includes('/api/ds/query')) {
      captured.push(resp);
    }
    // Match every outbound trace-by-id Grafana issues against the
    // cerberus-tempo datasource — whichever proxy path the plugin
    // uses internally (proxy/uid/<uid>/api/... vs
    // datasources/uid/<uid>/resources/api/...).
    if (url.includes('/cerberus-tempo/') && /\/(v2\/)?traces\/[0-9a-f]+/i.test(url)) {
      traceByIDRequests.push({ url, status: resp.status() });
    }
  };
  page.on('response', onResponse);

  try {
    await Promise.all([
      page.waitForURL(/\/explore/, { timeout: 60_000 }).catch(() => {}),
      traceLink.click({ timeout: 10_000 }),
    ]);
    await page
      .waitForLoadState('networkidle', { timeout: 45_000 })
      .catch(() => {});
  } finally {
    page.off('response', onResponse);
  }

  // 3b. Trace-by-id URL gate. If Grafana fired any trace-by-id
  //     request during the drill-through, at least one must hit the
  //     v2 URL (the Grafana 11.x default for newly-provisioned Tempo
  //     datasources, which is what compose's
  //     test/e2e/grafana/compose/datasources/cerberus.yaml ships).
  //     Every captured v2 hit must also resolve 2xx — that's the
  //     load-bearing gate for task #208: cerberus aliased
  //     `/api/v2/traces/{id}` to the same handler so the modern UI
  //     stops 404-ing every drill-down.
  //
  //     When `traceByIDRequests` is empty the Tempo plugin didn't
  //     hit cerberus directly during this click (Grafana sometimes
  //     resolves the trace view client-side from the panel's existing
  //     query result), and the tunneled-error / DOM-alert sweeps
  //     below still cover the rendering side.
  if (traceByIDRequests.length > 0) {
    const v2Hits = traceByIDRequests.filter((r) => r.url.includes('/v2/traces/'));
    if (v2Hits.length === 0) {
      failures.push(
        `[trace-click] no /api/v2/traces hit observed — Grafana 11.x defaults to tempoApiVersion>=v2, so every trace drill-down must route through v2; captured:\n${traceByIDRequests
          .map((r) => `  ${r.status} ${r.url}`)
          .join('\n')}`,
      );
    }
    for (const hit of v2Hits) {
      if (hit.status < 200 || hit.status > 299) {
        failures.push(
          `[trace-click] /api/v2/traces hit ${hit.url} → ${hit.status}, want 2xx (task #208: cerberus must alias the v2 URL to handleTraceByID)`,
        );
      }
    }
  }

  // 4a. /api/ds/query status sweep over what the click fired.
  for (const resp of captured) {
    const status = resp.status();
    if (status >= 500) {
      let body = '';
      try {
        body = await resp.text();
      } catch {
        body = '<unreadable>';
      }
      failures.push(
        `[trace-click] /api/ds/query → ${status}\n  body: ${truncate(body, 800)}`,
      );
    }
  }

  // 4b. /api/ds/query tunneled-error sweep — Grafana 200s the
  //     request and pushes the error string into the body. The proto
  //     decode failure surfaces here verbatim:
  //     `failed to convert tempo response to Otlp: proto: illegal wireType N`.
  for (const resp of captured) {
    if (resp.status() < 200 || resp.status() > 299) continue;
    let parsed: { results?: Record<string, { error?: string }> };
    try {
      parsed = (await resp.json()) as typeof parsed;
    } catch {
      continue;
    }
    for (const [refId, target] of Object.entries(parsed.results ?? {})) {
      if (target && typeof target.error === 'string' && target.error.length > 0) {
        failures.push(
          `[trace-click] ds-query: refId=${refId}\n  error: ${truncate(target.error, 800)}`,
        );
      }
    }
  }

  // 4c. DOM-level "Query error" / "illegal wireType" / "plugin.downstreamError"
  //     sweep. Grafana's red error banner aria-labels itself with the
  //     plugin error string, and the body sometimes shows the same
  //     text in a `role="alert"` container.
  const alerts = await page
    .locator('[role="alert"]')
    .evaluateAll((nodes) => nodes.map((n) => n.textContent ?? ''));
  for (const text of alerts) {
    const lc = text.toLowerCase();
    if (
      lc.includes('illegal wiretype') ||
      lc.includes('plugin.downstreamerror') ||
      lc.includes('query error') ||
      lc.includes('failed to convert tempo response')
    ) {
      failures.push(`[trace-click] DOM alert: ${truncate(text, 400)}`);
    }
  }

  return failures;
}

/**
 * Drive the cerberus-self → "Query rate by language" panel and assert
 * the underscored-matcher → dotted-OTel-attribute fallback emits at
 * least 2 distinct grouped series.
 *
 * Pre-fix bug shape (task #214): `sum by (cerberus_ql) (...)` would
 * emit `Attributes['cerberus_ql']` which misses every CH row whose
 * attribute key is the OTel-canonical dotted form `cerberus.ql` —
 * collapsing the panel to a single anonymous "Value" series. The
 * /promql/lower.go `attributeLookup` helper now emits an
 * `if(mapContains(...), ...)` chain over the dot↔underscore
 * candidates so the lookup hits either form.
 *
 * The assertion targets the /api/ds/query response the panel fires:
 * the JSON envelope's `data.result` array must carry at least 2
 * entries. Three cerberus heads (`promql`, `logql`, `traceql`) seed
 * the underlying counter so a healthy stack yields 3; we assert ≥ 2
 * to tolerate a stack where a single head momentarily has no traffic.
 *
 * If the panel isn't provisioned in the current stack (compose
 * variant without the cerberus-self dashboard) the function returns
 * cleanly — the dashboard sweep above already covers the "panel
 * exists" case.
 */
async function driveCerberusQLPartition(
  page: Page,
  baseURL: string,
): Promise<string[]> {
  const failures: string[] = [];

  // Capture ds/query responses BEFORE navigation so the panel's
  // initial fetch is in our buffer when the load settles.
  const captured: { url: string; body: string; status: number }[] = [];
  const onResponse = async (resp: Response) => {
    const url = resp.url();
    if (!url.includes('/api/ds/query')) return;
    let body = '';
    try {
      body = await resp.text();
    } catch {
      body = '';
    }
    captured.push({ url, body, status: resp.status() });
  };
  page.on('response', onResponse);

  try {
    await page.goto(`${baseURL}/d/cerberus-self`, {
      waitUntil: 'domcontentloaded',
      timeout: 90_000,
    });
    await page
      .waitForLoadState('networkidle', { timeout: 45_000 })
      .catch(() => {});
  } finally {
    page.off('response', onResponse);
  }

  // The panel may not be provisioned in every stack variant. The
  // dashboard sweep already failed loudly if the dashboard 404s, so
  // here we just no-op when the panel header isn't present.
  const panelTitle = 'Query rate by language';
  const panelLocator = page.locator(
    `[data-testid="data-testid Panel header ${panelTitle}"]`,
  );
  if ((await panelLocator.count()) === 0) {
    return failures;
  }

  // Find a ds/query response whose body references `cerberus_ql`
  // (the panel's group-by key). Grafana 11.x stringifies the parsed
  // PromQL into the response envelope alongside the result, so
  // `body.includes('cerberus_ql')` narrows to the panel's request
  // without parsing the JSON.
  const panelResponses = captured.filter((c) => c.body.includes('cerberus_ql'));
  if (panelResponses.length === 0) {
    failures.push(
      `[partition:${panelTitle}] no /api/ds/query response referenced cerberus_ql — Grafana may have served the panel from cache; rerun with cleared session if seen`,
    );
    return failures;
  }

  let maxSeries = 0;
  // Collect every `cerberus_ql=...` value Grafana decoded out of the
  // frame schemas. Pre-task-#215 this set was empty (the panel
  // collapsed to a single anonymous bucket) even when frames.length
  // looked plausible — so checking the legend CONTENT, not just the
  // frame count, is the load-bearing signal.
  const seenLanguages = new Set<string>();
  for (const resp of panelResponses) {
    if (resp.status < 200 || resp.status > 299) {
      failures.push(
        `[partition:${panelTitle}] /api/ds/query → ${resp.status}\n  url: ${resp.url}\n  body: ${truncate(resp.body, 600)}`,
      );
      continue;
    }
    try {
      const parsed = JSON.parse(resp.body) as {
        results?: Record<
          string,
          {
            frames?: Array<{
              schema?: {
                fields?: Array<{
                  name?: string;
                  labels?: Record<string, string>;
                }>;
              };
            }>;
          }
        >;
      };
      const results = parsed.results ?? {};
      for (const refID of Object.keys(results)) {
        const frames = results[refID]?.frames ?? [];
        // Each grouped series renders as a separate frame; the
        // "Value" field of each frame's schema carries the
        // `cerberus_ql=...` label. Frames count = series count.
        if (frames.length > maxSeries) {
          maxSeries = frames.length;
        }
        for (const frame of frames) {
          const fields = frame.schema?.fields ?? [];
          for (const f of fields) {
            const ql = f.labels?.cerberus_ql;
            if (typeof ql === 'string' && ql !== '') {
              seenLanguages.add(ql);
            }
          }
        }
      }
    } catch (err) {
      failures.push(
        `[partition:${panelTitle}] response body is not valid JSON: ${truncate(
          (err as Error).message,
          200,
        )}\n  body: ${truncate(resp.body, 600)}`,
      );
    }
  }

  if (maxSeries < 2) {
    failures.push(
      `[partition:${panelTitle}] expected ≥ 2 grouped series (cerberus_ql=promql/logql/traceql) but saw ${maxSeries}\n  ` +
        `regression of task #214 — dotted-OTel-key lookup fell back to a single anonymous bucket`,
    );
  }

  // Legend-content pin: every healthy stack should expose at least two
  // of the three cerberus heads on the `cerberus_ql` label. Without
  // this assertion a regression that emits two frames both labelled
  // `cerberus_ql=""` would slip past the maxSeries gate (frames > 1
  // can come from any group-by column the panel surfaces).
  const expectedLanguages = ['promql', 'logql', 'traceql'];
  const seenExpected = expectedLanguages.filter((l) => seenLanguages.has(l));
  if (seenExpected.length < 2) {
    failures.push(
      `[partition:${panelTitle}] expected ≥ 2 of {promql,logql,traceql} on the cerberus_ql legend, saw ${JSON.stringify(
        [...seenLanguages].sort(),
      )}\n  ` +
        `task #215 N2 regression — group-by chain didn't surface the OTel-dotted attribute values on the wire`,
    );
  }

  return failures;
}

/**
 * Drive the otel-fixture-explorer → "Log volume by severity" panel and
 * assert the LogQL `sum by (SeverityText) (rate(...))` lowering returns
 * at least 2 distinct severity series.
 *
 * Pre-fix bug shape (task #218): the LogQL lowering resolved every
 * outer by-clause label as `ResourceAttributes[<label>]`. SeverityText
 * is a top-level otel_logs column (not a key inside ResourceAttributes),
 * so the lookup returned the empty string for every row and the panel
 * collapsed every severity into a single `{SeverityText:""}` series.
 * The fix plumbs the outer by-clause down through lowerCtx so the
 * inner range identity wrap surfaces SeverityText into the augmented
 * map, and the outer Aggregate reads it back via MapAccess.
 *
 * The assertion targets the /api/ds/query response the panel fires:
 * the JSON envelope's `data.result` array must carry at least 2
 * entries. The compose seeder writes INFO / WARN / ERROR rows
 * (test/e2e/seed/cmd/seed/main.go) so a healthy stack yields 3; we
 * assert ≥ 2 to tolerate a stack where one severity momentarily has
 * no samples.
 *
 * If the panel isn't provisioned in the current stack (compose
 * variant without the otel-fixture-explorer dashboard) the function
 * returns cleanly — the dashboard sweep above already covers the
 * "panel exists" case.
 */
async function driveSeverityPartition(
  page: Page,
  baseURL: string,
): Promise<string[]> {
  const failures: string[] = [];

  // Capture ds/query responses BEFORE navigation so the panel's
  // initial fetch is in our buffer when the load settles.
  const captured: { url: string; body: string; status: number }[] = [];
  const onResponse = async (resp: Response) => {
    const url = resp.url();
    if (!url.includes('/api/ds/query')) return;
    let body = '';
    try {
      body = await resp.text();
    } catch {
      body = '';
    }
    captured.push({ url, body, status: resp.status() });
  };
  page.on('response', onResponse);

  try {
    await page.goto(`${baseURL}/d/otel-fixture-explorer`, {
      waitUntil: 'domcontentloaded',
      timeout: 90_000,
    });
    await page
      .waitForLoadState('networkidle', { timeout: 45_000 })
      .catch(() => {});
  } finally {
    page.off('response', onResponse);
  }

  const panelTitle = 'Log volume by severity';
  const panelLocator = page.locator(
    `[data-testid="data-testid Panel header ${panelTitle}"]`,
  );
  if ((await panelLocator.count()) === 0) {
    return failures;
  }

  // Find a ds/query response whose body references `SeverityText`
  // (the panel's group-by key). Grafana 11.x stringifies the parsed
  // LogQL into the response envelope alongside the result, so
  // `body.includes('SeverityText')` narrows to the panel's request
  // without parsing the JSON.
  const panelResponses = captured.filter((c) => c.body.includes('SeverityText'));
  if (panelResponses.length === 0) {
    failures.push(
      `[partition:${panelTitle}] no /api/ds/query response referenced SeverityText — Grafana may have served the panel from cache; rerun with cleared session if seen`,
    );
    return failures;
  }

  let maxSeries = 0;
  for (const resp of panelResponses) {
    if (resp.status < 200 || resp.status > 299) {
      failures.push(
        `[partition:${panelTitle}] /api/ds/query → ${resp.status}\n  url: ${resp.url}\n  body: ${truncate(resp.body, 600)}`,
      );
      continue;
    }
    try {
      const parsed = JSON.parse(resp.body) as {
        results?: Record<string, { frames?: Array<{ schema?: { fields?: unknown[] } }> }>;
      };
      const results = parsed.results ?? {};
      for (const refID of Object.keys(results)) {
        const frames = results[refID]?.frames ?? [];
        if (frames.length > maxSeries) {
          maxSeries = frames.length;
        }
      }
    } catch (err) {
      failures.push(
        `[partition:${panelTitle}] response body is not valid JSON: ${truncate(
          (err as Error).message,
          200,
        )}\n  body: ${truncate(resp.body, 600)}`,
      );
    }
  }

  if (maxSeries < 2) {
    failures.push(
      `[partition:${panelTitle}] expected ≥ 2 grouped severity series (INFO/WARN/ERROR seed) but saw ${maxSeries}\n  ` +
        `regression of task #218 — top-level OTel column lookup collapsed to a single empty-string bucket`,
    );
  }

  return failures;
}

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

/*
 * `isKnownTolerated404` retired in task #230 (PR
 * test/e2e-phase-7-retire-404-allow-list).
 *
 * The function previously allow-listed two Grafana-polled Prom paths
 * (`/api/v1/rules`, `/api/v1/alerts`) while their backing stubs were
 * still in flight. Both endpoints now return 200 (empty envelopes)
 * after PR #632 merged on 2026-05-20:
 *
 *   $ curl -s http://localhost:8080/api/v1/rules
 *   {"status":"success","data":{"groups":[]}}
 *   $ curl -s http://localhost:8080/api/v1/alerts
 *   {"status":"success","data":{"alerts":[]}}
 *
 * Per resolved decision Q5 in /home/thiago/.claude/plans/e2e-enhance.md
 * §9.5 — "NO toleration. Every 404 surfaced during the sweep is a bug,
 * not a tolerated state" — the helper and its allow-list are removed
 * outright. New non-2xx responses fail the sweep at step 3a; the fix
 * is to implement the endpoint or to drop the surface from the
 * iteration, not to re-introduce an allow-list. The phase-1..4
 * iterate-* specs enforce the same policy via
 * helpers/assertions.ts::assertNon200ResponseClass, which by design
 * carries no allow-list parameter.
 */
