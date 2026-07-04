import {
  test,
  expect,
  type Response,
  type Page,
  type APIRequestContext,
} from '@playwright/test';
import {
  generateSelfTraffic,
  awaitSelfTelemetryRangeSignal,
  awaitSeedFixtureSignal,
} from './helpers/index.js';

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

// --- Shared warmup: gate BOTH tests on a warm, seeded stack -----------------
//
// Both tests below assert against a freshly-booted compose stack with ZERO
// tolerance for datasource errors. On a cold start the cerberus->CH circuit
// breaker can still be OPEN (or CH still settling) when the sweep fires, so a
// panel query intermittently caught `503 circuit breaker open`, and the traceql
// probe found no self-telemetry trace within its budget — a recovered-on-retry
// flake that CI's failOnFlakyTests turns into a red shard. The sweep runs before
// this spec's own seed (driveCerberusQLPartition), so nothing guaranteed the
// stack was warm first. Seeding self-traffic and WAITING until it (and the seed
// fixture) are queryable proves cerberus is warm — breaker closed, CH reachable,
// schema present — before any assertion. Mirrors iterate-all-dashboards.spec.ts.
const SEED_TRAFFIC_SECONDS = 30;
const WARMUP_SIGNAL_DEADLINE_SECONDS = 120;
const WARMUP_BOUNDED_WAITS = 3; // 2 self-telemetry exprs + 1 seed-fixture wait
const WARMUP_BEFOREALL_TIMEOUT_MS =
  (SEED_TRAFFIC_SECONDS + WARMUP_BOUNDED_WAITS * WARMUP_SIGNAL_DEADLINE_SECONDS) *
  1000;

test.beforeAll(async ({ request }) => {
  test.setTimeout(WARMUP_BEFOREALL_TIMEOUT_MS);
  await generateSelfTraffic(request, SEED_TRAFFIC_SECONDS);
  await awaitSelfTelemetryRangeSignal(request);
  await awaitSeedFixtureSignal(request);
});

test('compose: home, drilldown app, and every provisioned dashboard load without datasource errors', async ({
  page,
  request,
}, testInfo) => {
  // The drilldown app + multi-surface sweep is heavier than the old
  // dashboard-only loop; bump the overall budget to 8 minutes. The
  // extra 2 minutes (vs the prior 6m budget) absorbs the ~95s self-
  // traffic seed `driveCerberusQLPartition` now runs before its
  // [5m]-rate panel assertion (see seedCerberusSelfTraffic) — without
  // that seed, fresh compose stacks flaked when the lower-volume
  // `traceql` head landed only a single OTel export inside the rate
  // window (#664/#681/#682 on 2026-05-21).
  testInfo.setTimeout(480_000);

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
  //                                 cerberus dashboard targets;
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

    // 3a. HTTP status sweep over every captured response — zero
    //     tolerance for non-2xx. Every failure is a real bug to fix
    //     at the source (implement the endpoint, fix the proxy, or
    //     drop the surface from the iteration).
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
  //    We locate the "Slow cerberus traces" panel on the cerberus dashboard,
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
  //    The cerberus dashboard's "Query rate by language" panel
  //    fires `sum by (cerberus_ql) (rate(cerberus_queries_total[5m]))`.
  //    OTel writes the `cerberus.ql` attribute under the dotted form
  //    in storage; the matcher-side lookup must cross the
  //    dot↔underscore boundary or the partition collapses to a
  //    single anonymous "Value" series. The sweep asserts the panel's
  //    legend / table renders ≥ 2 distinct grouped series — the three
  //    cerberus heads (`promql`, `logql`, `traceql`) are the seed
  //    contract, and any ≥ 2 catches the regression cleanly.
  const partitionFailures = await driveCerberusQLPartition(page, baseURL, request);
  failures.push(...partitionFailures);

  // (The historical by-SeverityText partition sweep against the
  //  otel-fixture-explorer dashboard was retired alongside the fake-data
  //  seeder so cerberus's own self-telemetry is the only data source in
  //  the quickstart compose stack. Task #218's regression — top-level
  //  OTel column lookup collapsing to an empty-string bucket — is still
  //  covered by the LogQL conformance / golden tests under test/spec/logql/.)

  if (failures.length > 0) {
    const header = `compose-grafana-smoke caught ${failures.length} failure(s) across ${surfaces.length} surface(s):`;
    const surfaceList = surfaces
      .map((s) => `  - [${s.kind}] ${s.label}`)
      .join('\n');
    const detail = failures.map((f) => `* ${f}`).join('\n');
    throw new Error(`${header}\nprobed surfaces:\n${surfaceList}\nfailures:\n${detail}`);
  }
});

/**
 * Drive the cerberus dashboard → "Slow cerberus traces" panel → click a
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

  // 1. Navigate to the cerberus dashboard and wait for panels to settle.
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
 * Drive the cerberus dashboard → "Query rate by language" panel and assert
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
 * variant without the cerberus dashboard) the function returns
 * cleanly — the dashboard sweep above already covers the "panel
 * exists" case.
 */
async function driveCerberusQLPartition(
  page: Page,
  baseURL: string,
  request: APIRequestContext,
): Promise<string[]> {
  const failures: string[] = [];

  // Seed cerberus self-traffic across the rate-window boundary BEFORE
  // navigating to the dashboard.
  //
  // Why this exists: the panel fires
  //   `sum by (cerberus_ql) (rate(cerberus_queries_total[5m]))`
  // and `rate()` needs ≥ 2 distinct samples per series inside the
  // window. `cerberus_queries_total` is fed by cerberus's OTel SDK
  // `PeriodicReader` (internal/telemetry/telemetry.go), which exports
  // on its 60s default interval. On a freshly-started compose stack,
  // the surfaces sweep above hits each head only a handful of times —
  // enough for the dashboard-load assertions, but borderline for the
  // ≥ 2-samples-per-cerberus_ql contract. PRs #664/#681/#682 flaked
  // here exactly when the lower-volume head (`traceql`) landed only a
  // single export batch inside the rate window.
  //
  // The fix fires two self-traffic bursts straddling an OTel export
  // boundary, guaranteeing the next two exports land monotonically-
  // increasing counter values for promql / logql / traceql:
  //   t=0    burst 1: 6 hits each to /api/v1/query (prom),
  //          /loki/api/v1/query (loki), /api/search (tempo)
  //   t=75s  burst 2: same shape — counter grows, next export
  //          publishes a sample distinct from burst 1's
  //   t=95s  proceed: ≥2 samples per cerberus_ql now inside [5m]
  //
  // 95s seed > 60s OTel interval × 1, so even a worst-case "burst 1
  // landed just before an export tick" still leaves ≥ 1 export between
  // bursts. The 6-hit count per burst tolerates a single sporadic
  // request failure without dropping a head off the legend.
  await seedCerberusSelfTraffic(request, baseURL);

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

/**
 * Fire two self-traffic bursts at cerberus, spaced across an OTel
 * export boundary, so the next two PeriodicReader exports publish
 * monotonically-increasing samples of `cerberus_queries_total` for
 * each `cerberus_ql` label value (promql / logql / traceql).
 *
 * Burst-1 → wait 75s → burst-2 → wait 20s. Total ≈ 95s. With cerberus's
 * SDK default 60s metric export interval, this guarantees at least one
 * export tick lands between the two bursts (so the second sample's
 * counter value is strictly greater than the first), and a final tick
 * has time to flush burst-2 to ClickHouse before the panel query fires.
 *
 * Errors per individual request are swallowed: the partition assertion
 * downstream needs ≥ 2 of the three heads on the legend, so a single
 * sporadic failure shouldn't tip a healthy stack into a false-flake.
 * Burst sizes are sized at 6 so even half-failing is fine.
 */
async function seedCerberusSelfTraffic(
  request: APIRequestContext,
  grafanaBaseURL: string,
): Promise<void> {
  // Cerberus URL: env override (the compose-smoke job sets CERBERUS_URL),
  // else fall back to the Grafana datasource-proxy path — works in both
  // direct-host and proxied configurations.
  const cerberusURL = process.env.CERBERUS_URL;
  const headTargets: Array<{
    ql: 'promql' | 'logql' | 'traceql';
    direct: string;
    proxied: string;
  }> = [
    {
      ql: 'promql',
      direct: '/api/v1/query?query=up',
      proxied: '/api/datasources/proxy/uid/cerberus-prometheus/api/v1/query?query=up',
    },
    {
      ql: 'logql',
      direct: `/loki/api/v1/query?query=${encodeURIComponent('{service_name=~".+"}')}`,
      proxied: `/api/datasources/proxy/uid/cerberus-loki/loki/api/v1/query?query=${encodeURIComponent(
        '{service_name=~".+"}',
      )}`,
    },
    {
      ql: 'traceql',
      direct: `/api/search?q=${encodeURIComponent('{}')}`,
      proxied: `/api/datasources/proxy/uid/cerberus-tempo/api/search?q=${encodeURIComponent('{}')}`,
    },
  ];

  const fireBurst = async () => {
    const HITS_PER_HEAD = 6;
    for (const t of headTargets) {
      for (let i = 0; i < HITS_PER_HEAD; i++) {
        const url = cerberusURL
          ? `${cerberusURL}${t.direct}`
          : `${grafanaBaseURL}${t.proxied}`;
        // Warmup-traffic call — the helper's job is to nudge the
        // counters; the downstream legend / panel assertion is the
        // load-bearing check, so a per-iteration HTTP error here is
        // discarded rather than cascaded into the assertion phase.
        try {
          await request.get(url, { timeout: 5_000 });
        } catch {
          // Discarded: warmup-only call; the downstream assertion is
          // load-bearing.
        }
      }
    }
  };

  await fireBurst();
  // Wait long enough that a 60s OTel PeriodicReader export tick lands
  // between bursts. 75s > 60s × 1, so even a worst-case "burst 1 landed
  // just before an export" still publishes a sample with the burst-1
  // counter value before burst 2 grows it.
  await new Promise<void>((resolve) => setTimeout(resolve, 75_000));
  await fireBurst();
  // Give the post-burst-2 export tick + collector flush + CH insert time
  // to settle so the panel's [5m] window sees both samples.
  await new Promise<void>((resolve) => setTimeout(resolve, 20_000));
}

function truncate(s: string, n: number): string {
  return s.length <= n ? s : `${s.slice(0, n)}...<truncated, ${s.length} chars total>`;
}

function stripBase(url: string, base: string): string {
  return url.startsWith(base) ? url.slice(base.length) : url;
}


/**
 * Trace DETAIL through the Grafana datasource BACKEND — POST
 * /api/ds/query with a traceql query whose expression is a bare trace
 * ID. In Grafana 12 the Tempo plugin backend resolves that by fetching
 * `/api/v2/traces/<id>` as protobuf and converting the
 * tempopb.TraceByIDResponse envelope to OTLP server-side.
 *
 * This is the exact path the v2-envelope regression broke (`Failed to
 * convert tempo response to Otlp: proto: KeyValue: wiretype end group
 * for non-group` → "An error occurred within the plugin" in Explore)
 * while every existing sweep stayed green: the UI trace-click sweep
 * above only gates the proxy-level /api/v2/traces status, and the
 * dashboard sweeps never open a trace detail through the backend.
 *
 * The trace ID is surfaced via TraceQL search over cerberus's own
 * self-telemetry spans (the compose stack exports them through the
 * OTel collector on a 60s tick), polled with a generous deadline so a
 * fresh stack has time to land its first export — no hardcoded IDs,
 * no expected-empty escape hatch: if search never surfaces a trace,
 * that's a real failure of the traces pipeline and the test reports
 * it loudly.
 */
test('compose: tempo trace detail via /api/ds/query (Grafana plugin backend) succeeds', async ({
  request,
}, testInfo) => {
  // Search polling below tolerates a fresh stack's first OTel export
  // tick (60s) plus collector flush + CH insert; budget accordingly.
  testInfo.setTimeout(300_000);

  const baseURL = process.env.GRAFANA_BASE_URL ?? 'http://localhost:3000';
  const tempoProxy = `${baseURL}/api/datasources/proxy/uid/cerberus-tempo/api`;

  // 1. Surface a trace ID via search. Each poll iteration also fires a
  //    couple of cerberus queries so a fresh stack generates spans to
  //    find (cerberus traces itself; the queries below land in
  //    otel_traces once the 60s export tick fires).
  let traceID = '';
  const deadline = Date.now() + 240_000;
  while (Date.now() < deadline) {
    // Span-generating nudge — errors are discarded; the search poll is
    // the load-bearing check.
    try {
      await request.get(
        `${baseURL}/api/datasources/proxy/uid/cerberus-prometheus/api/v1/query?query=up`,
        { timeout: 5_000 },
      );
    } catch {
      // Warmup-only call; failure here is not the signal under test.
    }
    const searchResp = await request.get(
      `${tempoProxy}/search?q=${encodeURIComponent('{}')}`,
      { timeout: 10_000 },
    );
    if (searchResp.ok()) {
      const body = await searchResp.json().catch(() => ({}));
      const id = body?.traces?.[0]?.traceID;
      if (id) {
        traceID = id;
        break;
      }
    }
    await new Promise<void>((resolve) => setTimeout(resolve, 5_000));
  }
  expect(
    traceID,
    'TraceQL search must surface at least one self-telemetry trace within the polling budget',
  ).toBeTruthy();

  // 2. Trace detail through the plugin backend.
  const now = Date.now();
  const dsResp = await request.post(`${baseURL}/api/ds/query`, {
    data: {
      queries: [
        {
          refId: 'A',
          datasource: { type: 'tempo', uid: 'cerberus-tempo' },
          // 'traceId', not 'traceql': the Explore pane URL carries
          // queryType=traceql, but Grafana's tempo datasource frontend
          // reclassifies a bare-hex query before POSTing — the backend
          // serves trace-by-id only under queryType traceId and rejects
          // traceql with "backend TraceQL search queries are not
          // supported" (verified against grafana 12.2.9).
          queryType: 'traceId',
          query: traceID,
          limit: 20,
          tableType: 'traces',
        },
      ],
      from: String(now - 24 * 60 * 60 * 1000),
      to: String(now),
    },
  });

  const dsBody = await dsResp.json().catch(() => ({}));
  const result = dsBody?.results?.A;
  const tunneledError: string = result?.error ?? '';
  expect(
    tunneledError,
    'trace-by-id /api/ds/query must not tunnel a plugin error (Grafana 12 unmarshals the v2 TraceByIDResponse envelope and converts it to OTLP here)',
  ).toBe('');
  expect(dsResp.ok(), `/api/ds/query status ${dsResp.status()}`).toBe(true);
  expect(Array.isArray(result?.frames), 'results.A.frames is an array').toBe(true);
  expect(result.frames.length, '≥1 trace frame rendered').toBeGreaterThan(0);
});
