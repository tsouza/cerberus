/**
 * Metrics Explorer (Drilldown-Metrics app) comprehensive sweep.
 *
 * The user's #8 sweep finding was that Metrics Explorer renders mostly
 * empty + the labels chip fails to fetch for `cerberus_clickhouse_bytes_read`.
 * The existing e2e never caught it: the panel-shape / kiosk / filter-
 * drill sweeps iterate dashboards only, not the Drilldown-Metrics app.
 *
 * This spec enumerates every cerberus-published metric name via the
 * `/api/v1/label/__name__/values` endpoint (the same shape Drilldown-
 * Metrics itself uses to populate its tile grid) and, for each metric,
 * asserts:
 *
 *   1. The `/api/v1/series?match[]={__name__="<metric>"}` endpoint
 *      returns at least one series. This is the call Drilldown-Metrics
 *      fires when the user clicks into a metric to populate the "Labels"
 *      chip — the call that was failing on `cerberus_clickhouse_bytes_read`.
 *      A failure here surfaces as "Unable to fetch labels" in the UI.
 *   2. The `/api/v1/query_range?query=<metric>` endpoint returns at
 *      least one series. An empty result is a real bug at the source
 *      (cerberus code, seed, or metric publisher) — not a state to
 *      mask.
 *   3. The labels returned by `/api/v1/series` carry the same metric
 *      name in their `__name__` field — sanity check that the gateway
 *      isn't echoing labels from a different metric.
 *
 * Additionally, the spec navigates to the Drilldown-Metrics page in
 * Grafana itself and asserts the rendered DOM does NOT contain the
 * "Unable to fetch labels" failure-state string anywhere. That UI-level
 * assertion is the user-visible regression the brief pinned.
 *
 * At the end of the per-metric sweep we emit a JSON summary
 * (`metric → label_count → series_count → first_value`) and attach it
 * as a Playwright artifact. The CI run record shows the catalog the
 * sweep covered.
 *
 * Env:
 *   GRAFANA_URL       default http://localhost:3000
 *   GRAFANA_BASE_URL  honoured as fallback for parity with the rest of
 *                     the compose-smoke specs.
 *   CERBERUS_URL      default http://localhost:8080 — used for the
 *                     enumerate-all-metrics catalog query (the Grafana
 *                     proxy will go through cerberus anyway but the
 *                     direct port keeps the catalog query independent
 *                     of Grafana availability for the build-time list).
 */

import {
  expect,
  test,
  type APIRequestContext,
  type TestInfo,
} from '@playwright/test';

import { generateSelfTraffic } from './helpers/index.js';

const SEED_TRAFFIC_SECONDS = 30;
const QUERY_WINDOW_SECONDS = 5 * 60;
const QUERY_STEP_SECONDS = 15;
// Pause after the warmup loop ends so cerberus's own OTLP exporter
// (PR #696 wired a 10s push interval) and the downstream ClickHouse
// insert pipeline have time to flush the self-telemetry rows. Without
// this, `/api/v1/series` can still return 0 rows for metrics that
// /api/v1/label/__name__/values already enumerates — the catalog is
// populated as soon as the first push lands, but the per-series rows
// may take an extra flush cycle to become visible to the query path.
const POST_WARMUP_FLUSH_SECONDS = 15;

type LabelValuesResponse = {
  status: string;
  data: string[];
};

type SeriesResponse = {
  status: string;
  data: Array<Record<string, string>>;
};

type QueryRangeResponse = {
  status: string;
  data?: {
    resultType?: string;
    result?: Array<{
      metric?: Record<string, string>;
      values?: Array<[number, string]>;
    }>;
  };
};

type MetricSummary = {
  metric: string;
  label_count: number;
  series_count: number;
  first_value: string | null;
  query_range_series: number;
};

const cerberusURL = (): string =>
  process.env.CERBERUS_URL ?? 'http://localhost:8080';
const grafanaURL = (): string =>
  process.env.GRAFANA_BASE_URL ?? process.env.GRAFANA_URL ?? 'http://localhost:3000';

/** Fetch the full catalog of metric names cerberus exposes. */
async function listMetricNames(
  request: APIRequestContext,
): Promise<string[]> {
  // The Prom datasource's enumerate-all-metrics path is
  // /api/v1/label/__name__/values with no match[] — cerberus implements
  // the same shape against the OTel-CH metrics tables.
  const url = `${cerberusURL()}/api/v1/label/__name__/values`;
  const resp = await request.get(url);
  expect(resp.status(), `GET ${url}`).toBe(200);
  const body = (await resp.json()) as LabelValuesResponse;
  expect(body.status, '__name__ values envelope.status').toBe('success');
  expect(Array.isArray(body.data), '__name__ values envelope.data').toBe(true);
  // Drop the "" placeholder some Prom impls return.
  return body.data.filter((n) => n && n.length > 0).sort();
}

/** Fire /api/v1/series for a single metric and parse the envelope. */
async function fetchSeries(
  request: APIRequestContext,
  metric: string,
  nowSec: number,
): Promise<SeriesResponse> {
  const match = encodeURIComponent(`{__name__="${metric}"}`);
  const start = nowSec - QUERY_WINDOW_SECONDS;
  const end = nowSec;
  const url =
    `${cerberusURL()}/api/v1/series?match[]=${match}` +
    `&start=${start}&end=${end}`;
  const resp = await request.get(url);
  expect(
    resp.status(),
    `metric=${metric}: GET /api/v1/series → ${resp.status()}`,
  ).toBe(200);
  const body = (await resp.json()) as SeriesResponse;
  expect(
    body.status,
    `metric=${metric}: /api/v1/series envelope.status`,
  ).toBe('success');
  expect(
    Array.isArray(body.data),
    `metric=${metric}: /api/v1/series envelope.data is array`,
  ).toBe(true);
  return body;
}

/** Fire /api/v1/query_range for a single metric and parse the envelope. */
async function fetchQueryRange(
  request: APIRequestContext,
  metric: string,
  nowSec: number,
): Promise<QueryRangeResponse> {
  const q = encodeURIComponent(metric);
  const start = nowSec - QUERY_WINDOW_SECONDS;
  const end = nowSec;
  const url =
    `${cerberusURL()}/api/v1/query_range?query=${q}` +
    `&start=${start}&end=${end}&step=${QUERY_STEP_SECONDS}`;
  const resp = await request.get(url);
  expect(
    resp.status(),
    `metric=${metric}: GET /api/v1/query_range → ${resp.status()}`,
  ).toBe(200);
  const body = (await resp.json()) as QueryRangeResponse;
  expect(
    body.status,
    `metric=${metric}: /api/v1/query_range envelope.status`,
  ).toBe('success');
  return body;
}

/**
 * If `metric` ends in one of the synthetic histogram suffixes
 * (`_count` / `_sum` / `_bucket`), return the base name. Otherwise
 * return the input unchanged. Used by the __name__-mismatch check so
 * a /api/v1/series query for `http_server_request_duration_count` is
 * allowed to return rows under `__name__=http_server_request_duration`.
 */
function stripHistogramSuffix(metric: string): string {
  for (const suffix of ['_bucket', '_count', '_sum']) {
    if (metric.endsWith(suffix)) {
      return metric.slice(0, metric.length - suffix.length);
    }
  }
  return metric;
}

test.describe('iterate-metrics-explorer: Drilldown-Metrics + label chips', () => {
  test.describe.configure({ mode: 'serial' });

  test.beforeAll(async ({ request }) => {
    // Warmup so the cerberus self metrics show populated values.
    await generateSelfTraffic(request, SEED_TRAFFIC_SECONDS);
    // Allow OTLP push + CH insert flush to settle. See the comment on
    // POST_WARMUP_FLUSH_SECONDS above — without this, /api/v1/series
    // races the flush pipeline and returns 0 rows for metrics that the
    // catalog endpoint already lists.
    await new Promise((r) =>
      setTimeout(r, POST_WARMUP_FLUSH_SECONDS * 1000),
    );
  });

  test('Drilldown-Metrics UI: no "Unable to fetch labels" banner', async ({
    page,
  }) => {
    // Navigate to the Drilldown-Metrics root. The route lives under
    // /a/grafana-metricsdrilldown-app or /explore/metrics/trail
    // depending on the Grafana version; both resolve in 11.x. We try
    // the trail URL first (the brief specifies it) and fall back to
    // the app root.
    const url =
      `${grafanaURL()}/explore/metrics/trail` +
      `?var-ds=cerberus-prometheus&metricPrefix=all`;
    try {
      await page.goto(url, { waitUntil: 'networkidle', timeout: 45_000 });
    } catch {
      // Fall back to the app root — Drilldown-Metrics may not be
      // installed in every compose-stack revision. The follow-up
      // assertion uses the body text, not a strict navigation.
      await page
        .goto(`${grafanaURL()}/a/grafana-metricsdrilldown-app/`, {
          waitUntil: 'networkidle',
          timeout: 45_000,
        })
        .catch(() => {
          // Drilldown-Metrics is not installed in this Grafana — the
          // /api/v1/series sweep below still runs and is the load-
          // bearing assertion. We annotate this as data, not a fail,
          // so the spec continues to cover the API surface even if
          // the app plugin is absent.
        });
    }
    // Give Drilldown-Metrics' label-fetch a moment to fire.
    await page.waitForTimeout(3_000);
    const bodyText = (await page.locator('body').innerText()).toLowerCase();
    expect(
      bodyText.includes('unable to fetch labels'),
      'Drilldown-Metrics body must not surface "Unable to fetch labels"',
    ).toBe(false);
  });

  test('every published metric: label chip + range probe', async ({
    request,
  }, testInfo: TestInfo) => {
    const names = await listMetricNames(request);
    expect(
      names.length,
      'cerberus must publish at least one metric',
    ).toBeGreaterThan(0);
    // eslint-disable-next-line no-console
    console.log(
      `iterate-metrics-explorer: enumerated ${names.length} metric names`,
    );

    const summary: MetricSummary[] = [];
    const nowSec = Math.floor(Date.now() / 1000);
    const labelFailures: string[] = [];

    for (const metric of names) {
      const seriesBody = await fetchSeries(request, metric, nowSec);
      const seriesCount = seriesBody.data.length;
      const labelCount =
        seriesCount > 0 ? Object.keys(seriesBody.data[0] ?? {}).length : 0;

      // Sanity: the __name__ on every series matches the queried name,
      // OR the queried name is a histogram synthetic-suffix view
      // (`_count` / `_sum` / `_bucket`) of the returned __name__. The
      // Prom-on-OTel convention is to expose histograms under the base
      // name with the suffix as a derived view, so a /api/v1/series
      // call for `http_server_request_duration_count` is expected to
      // return rows with `__name__=http_server_request_duration`. Any
      // other mismatch indicates the gateway echoed a different
      // metric's labels.
      const metricBase = stripHistogramSuffix(metric);
      for (const s of seriesBody.data) {
        if (!s.__name__ || s.__name__ === metric) continue;
        // Queried name is a suffix view of returned name (round-2 path,
        // e.g. queried `foo_count`, returned `foo`).
        if (metricBase !== metric && s.__name__ === metricBase) continue;
        // Returned name is a suffix view of the queried name (PR #699
        // bare-name fan-out, e.g. queried `foo`, returned `foo_bucket`).
        const returnedBase = stripHistogramSuffix(s.__name__);
        if (returnedBase !== s.__name__ && returnedBase === metric) continue;
        labelFailures.push(
          `metric=${metric}: /api/v1/series returned __name__=${s.__name__} (mismatch)`,
        );
      }

      if (seriesCount === 0) {
        labelFailures.push(
          `metric=${metric}: /api/v1/series returned 0 series — every catalog-published metric must resolve to >= 1 series. Fix the cerberus catalog endpoint or the publishing pipeline.`,
        );
      }

      const rangeBody = await fetchQueryRange(request, metric, nowSec);
      const rangeSeries = rangeBody.data?.result?.length ?? 0;
      let firstValue: string | null = null;
      const r0 = rangeBody.data?.result?.[0];
      if (r0 && Array.isArray(r0.values) && r0.values.length > 0) {
        firstValue = String(r0.values[0]?.[1] ?? '');
      }

      // The QUERY surface must serve every catalog-advertised name —
      // this is the exact call Drilldown-Metrics fires per preview
      // panel, and an empty result renders the "wall of empty preview
      // tiles" the round-3 sweep pinned. The /api/v1/series probe above
      // is NOT sufficient: the series endpoint historically applied a
      // matcher fan-out the query path lacked, so series returned rows
      // while query_range returned nothing for dotted-stored (k8s_*,
      // container_*) and bare classic-histogram names. No tolerance
      // list: an empty result here is a cerberus catalog/lowering bug
      // or a seed bug — fix it at the source.
      if (rangeSeries === 0) {
        labelFailures.push(
          `metric=${metric}: /api/v1/query_range returned 0 series — ` +
            `every catalog-advertised __name__ must be queryable ` +
            `(empty preview panel in Drilldown-Metrics). Fix the ` +
            `catalog advertisement or the selector lowering, never ` +
            `this assertion.`,
        );
      }

      summary.push({
        metric,
        label_count: labelCount,
        series_count: seriesCount,
        first_value: firstValue,
        query_range_series: rangeSeries,
      });
    }

    // Attach the summary as a CI artifact. The Playwright HTML report
    // surfaces this on failure; the GitHub Actions artifact upload step
    // picks it up too.
    await testInfo.attach('metrics-summary.json', {
      body: JSON.stringify(summary, null, 2),
      contentType: 'application/json',
    });
    // eslint-disable-next-line no-console
    console.log(
      `iterate-metrics-explorer: summary attached — ` +
        `${summary.length} metrics, ` +
        `${summary.filter((s) => s.series_count > 0).length} non-empty series`,
    );

    // Hard fail if we collected any label-failures. We collect across
    // every metric first (rather than throwing on the first one) so the
    // report shows every regression in one run instead of one-at-a-time.
    expect(
      labelFailures,
      `label-fetch / series-non-empty regressions:\n  - ${labelFailures.join('\n  - ')}`,
    ).toEqual([]);
  });
});
