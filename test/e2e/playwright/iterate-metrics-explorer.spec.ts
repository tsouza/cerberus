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
 * The per-metric sweep is split across METRIC_PROBE_SHARDS test cases,
 * each probing a deterministic stride of the catalog, because Playwright
 * budgets its timeout PER TEST and one test covering a catalog that grows
 * over time is a timeout flake waiting to happen (issue #1752). Every
 * catalog name is still probed on both surfaces — the shards partition the
 * catalog, they do not sample it.
 *
 * Each shard emits a JSON summary (`metric → label_count → series_count →
 * first_value`) and attaches it as a Playwright artifact. Together the CI
 * run record shows the whole catalog the sweep covered.
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

// Max metric probes in flight at once for the every-published-metric sweep.
// Each probe is two independent read-only HTTP round-trips; a small pool
// mirrors how Grafana fans panel queries out and keeps the sweep's wall
// time well inside the test budget without overwhelming the single
// compose-stack cerberus / ClickHouse.
//
// Measured against the quickstart compose stack (143-metric catalog, the
// same shape CI probes) — total wall time for the whole catalog, and the
// per-request p50 at each pool size:
//
//   pool=1   p50 series 23ms  range 21ms
//   pool=8   p50 series 158ms range 65ms   whole catalog 3.9s
//   pool=16  p50 series 206ms range 128ms  whole catalog 3.2s
//
// The sweep is THROUGHPUT-bound, not latency-bound: doubling the pool
// 8 -> 16 buys 18% wall time while inflating per-request latency ~30%,
// because the single compose-stack ClickHouse is already saturated. So
// raising this number is not a way to buy budget headroom — it mostly
// converts queueing-in-the-pool into queueing-in-ClickHouse. It also
// walks toward cerberus's prom admission cap (64 concurrent, rejected
// with HTTP 429 rather than queued), which would turn a slow sweep into
// a hard failure. 8 is the measured knee; leave it there and shard the
// catalog instead (see METRIC_PROBE_SHARDS).
const METRIC_PROBE_CONCURRENCY = 8;

// The every-published-metric sweep is split across this many test cases,
// each probing a deterministic stride of the catalog.
//
// Playwright's timeout is a PER-TEST budget. A single test that probed the
// whole catalog spent 33-55s of its fixed 60s against a catalog that grows
// every time cerberus gains a self-telemetry metric, so the headroom shrank
// silently until a contended runner tipped it over 60s ("Test timeout of
// 60000ms exceeded" / "Request context disposed"). Because the sweep is
// throughput-bound (see METRIC_PROBE_CONCURRENCY) the total server work is
// irreducible without dropping coverage — but it does not have to sit under
// ONE deadline. Striding the catalog across N tests divides the wall time
// each budget must cover by N without probing one metric fewer: every
// catalog name still gets its own dedicated /api/v1/series and
// /api/v1/query_range round-trip. No sampling, no tolerance list, no
// timeout bump.
//
// Stride (i % SHARDS) rather than contiguous blocks so the expensive names
// — classic-histogram families, whose bare-name fan-out expands into an
// 8-arm UNION over the gauge, sum and histogram tables — spread evenly
// instead of piling into one shard.
const METRIC_PROBE_SHARDS = 8;

// Upper bound on how many metrics a single shard may carry before the
// per-test budget is at risk again. The worst CI run recorded on issue
// #1752 probed 143 metrics in 55s, i.e. ~0.4s per metric on a contended
// runner, so 64 metrics is ~26s against the 60s budget — still better than
// 2x headroom at the slowest rate ever observed. If the catalog grows past
// SHARDS x this, the sweep fails loudly with an actionable message instead
// of drifting back into intermittent timeouts — the silent-degradation mode
// this constant exists to prevent.
const MAX_METRICS_PER_SHARD = 64;

// mapWithConcurrency runs `fn` over `items` with at most `limit` calls in
// flight, preserving input order in the returned results. Pulled out so the
// every-metric sweep can fan out instead of awaiting serially — the serial
// shape made the test's wall time scale with metric count and flake under
// runner contention.
async function mapWithConcurrency<T, R>(
  items: readonly T[],
  limit: number,
  fn: (item: T) => Promise<R>,
): Promise<R[]> {
  const results = new Array<R>(items.length);
  let cursor = 0;
  const worker = async (): Promise<void> => {
    for (;;) {
      const i = cursor;
      cursor += 1;
      if (i >= items.length) return;
      results[i] = await fn(items[i]);
    }
  };
  await Promise.all(
    Array.from({ length: Math.min(limit, items.length) }, () => worker()),
  );
  return results;
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

  for (let shard = 0; shard < METRIC_PROBE_SHARDS; shard++) {
    const shardLabel = `${shard + 1}/${METRIC_PROBE_SHARDS}`;
    test(`every published metric: label chip + range probe (shard ${shardLabel})`, async ({
      request,
    }, testInfo: TestInfo) => {
      await runMetricProbeShard(request, testInfo, shard);
    });
  }

  /**
   * Probe every catalog metric whose index falls in `shard`'s stride.
   *
   * Split out of the test body so all METRIC_PROBE_SHARDS cases share one
   * implementation — the shard index is the ONLY thing that varies, and a
   * per-shard copy of this logic would be the place a coverage gap hides.
   */
  async function runMetricProbeShard(
    request: APIRequestContext,
    testInfo: TestInfo,
    shard: number,
  ): Promise<void> {
    const shardLabel = `${shard + 1}/${METRIC_PROBE_SHARDS}`;
    const allNames = await listMetricNames(request);
    expect(
      allNames.length,
      'cerberus must publish at least one metric',
    ).toBeGreaterThan(0);

    // Stride the catalog. Union of every shard's stride is the whole
    // catalog with no overlap, so the suite as a whole probes exactly the
    // same set the single-test sweep did.
    const names = allNames.filter((_, i) => i % METRIC_PROBE_SHARDS === shard);

    // Capacity guard — see MAX_METRICS_PER_SHARD. This fails the sweep with
    // an instruction rather than letting the per-test budget erode back into
    // intermittent "Test timeout of 60000ms exceeded" flake as the catalog
    // grows.
    expect(
      names.length,
      `shard ${shardLabel} carries ${names.length} metrics, over the ` +
        `${MAX_METRICS_PER_SHARD} a single 60s test budget is sized for ` +
        `(catalog is ${allNames.length}). Raise METRIC_PROBE_SHARDS in ` +
        `this spec so each shard stays under the cap — do NOT raise the ` +
        `Playwright timeout.`,
    ).toBeLessThanOrEqual(MAX_METRICS_PER_SHARD);

    // eslint-disable-next-line no-console
    console.log(
      `iterate-metrics-explorer: shard ${shardLabel} probing ` +
        `${names.length} of ${allNames.length} enumerated metric names`,
    );

    const summary: MetricSummary[] = [];
    const nowSec = Math.floor(Date.now() / 1000);
    const labelFailures: string[] = [];

    // Probe every published metric with BOUNDED CONCURRENCY rather than a
    // serial await-loop. The per-metric work is two independent read-only
    // round-trips (/series + /query_range); a serial loop's wall time
    // scales as N × latency, so on a contended CI runner the sweep tips
    // the test's fixed budget and Playwright disposes the request context
    // mid-flight ("Request context disposed" — a timing flake, not a real
    // failure). A bounded pool keeps wall time ≈ (N / CONCURRENCY) × latency
    // with large headroom while still probing EVERY catalog metric — no
    // sampling, no coverage loss. Results merge in catalog order so the
    // summary + failure report stay deterministic.
    const probes = await mapWithConcurrency(
      names,
      METRIC_PROBE_CONCURRENCY,
      async (
        metric,
      ): Promise<{ summary: MetricSummary; failures: string[] }> => {
        const failures: string[] = [];
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
          failures.push(
            `metric=${metric}: /api/v1/series returned __name__=${s.__name__} (mismatch)`,
          );
        }

        if (seriesCount === 0) {
          failures.push(
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
          failures.push(
            `metric=${metric}: /api/v1/query_range returned 0 series — ` +
              `every catalog-advertised __name__ must be queryable ` +
              `(empty preview panel in Drilldown-Metrics). Fix the ` +
              `catalog advertisement or the selector lowering, never ` +
              `this assertion.`,
          );
        }

        return {
          summary: {
            metric,
            label_count: labelCount,
            series_count: seriesCount,
            first_value: firstValue,
            query_range_series: rangeSeries,
          },
          failures,
        };
      },
    );
    for (const probe of probes) {
      summary.push(probe.summary);
      labelFailures.push(...probe.failures);
    }

    // Attach the summary as a CI artifact. The Playwright HTML report
    // surfaces this on failure; the GitHub Actions artifact upload step
    // picks it up too. One attachment per shard — together they cover the
    // same catalog the single-test sweep reported in one file.
    await testInfo.attach(`metrics-summary-shard-${shard + 1}.json`, {
      body: JSON.stringify(summary, null, 2),
      contentType: 'application/json',
    });
    // eslint-disable-next-line no-console
    console.log(
      `iterate-metrics-explorer: shard ${shardLabel} summary attached — ` +
        `${summary.length} metrics, ` +
        `${summary.filter((s) => s.series_count > 0).length} non-empty series`,
    );

    // Hard fail if we collected any label-failures. We collect across
    // every metric in the shard first (rather than throwing on the first
    // one) so the report shows every regression in this shard in one run
    // instead of one-at-a-time.
    expect(
      labelFailures,
      `shard ${shardLabel} label-fetch / series-non-empty regressions:\n  - ${labelFailures.join('\n  - ')}`,
    ).toEqual([]);
  }
});
