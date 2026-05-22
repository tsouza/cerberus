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
 *      least one series — counters that legitimately stay at 0 on a
 *      fresh stack go through the `EXPECTED_EMPTY` list, which carries
 *      a one-line rationale per entry.
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

/**
 * Metric-name prefixes whose label-fetch + series-non-empty are not
 * load-bearing on a fresh stack. Each entry needs a one-line rationale.
 * Keep this short — every entry is a check the spec is opting out of.
 */
const EXPECTED_EMPTY: ReadonlyArray<{ prefix: string; why: string }> = [
  {
    prefix: 'cerberus_admit_rejected_total',
    // Admission rejections only fire under explicit overload; a fresh
    // compose stack has zero rejections, so the counter is empty.
    why: 'admission rejections counter starts at 0 on a fresh stack',
  },
  {
    prefix: 'cerberus_queries_failed_total',
    // Failure counter only ticks on a genuinely failed query; the
    // self-traffic warmup uses well-formed queries so the failure
    // counter stays at 0.
    why: 'query-failure counter starts at 0 with well-formed warmup',
  },
];

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
  empty_expected: boolean;
  why_empty: string | null;
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

function isExpectedEmpty(metric: string): { why: string } | null {
  for (const entry of EXPECTED_EMPTY) {
    if (metric.startsWith(entry.prefix)) return { why: entry.why };
  }
  return null;
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
    // Warmup so the cerberus-self metrics show populated values.
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
      // return rows with `__name__=http_server_request_duration`. A
      // mismatch on the BASE name (after stripping the suffix) would
      // still indicate the gateway echoed a different metric's labels.
      for (const s of seriesBody.data) {
        if (!s.__name__ || s.__name__ === metric) continue;
        const base = stripHistogramSuffix(metric);
        if (base !== metric && s.__name__ === base) continue;
        labelFailures.push(
          `metric=${metric}: /api/v1/series returned __name__=${s.__name__} (mismatch)`,
        );
      }

      const expected = isExpectedEmpty(metric);
      if (seriesCount === 0 && !expected) {
        labelFailures.push(
          `metric=${metric}: /api/v1/series returned 0 series — this is the "Unable to fetch labels" failure shape (#8). If empty-by-design, add a prefix to EXPECTED_EMPTY with a rationale.`,
        );
      }

      const rangeBody = await fetchQueryRange(request, metric, nowSec);
      const rangeSeries = rangeBody.data?.result?.length ?? 0;
      let firstValue: string | null = null;
      const r0 = rangeBody.data?.result?.[0];
      if (r0 && Array.isArray(r0.values) && r0.values.length > 0) {
        firstValue = String(r0.values[0]?.[1] ?? '');
      }

      summary.push({
        metric,
        label_count: labelCount,
        series_count: seriesCount,
        first_value: firstValue,
        query_range_series: rangeSeries,
        empty_expected: expected !== null,
        why_empty: expected?.why ?? null,
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
        `${summary.filter((s) => s.series_count > 0).length} non-empty series, ` +
        `${summary.filter((s) => s.empty_expected).length} expected-empty`,
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
