import { test, expect } from '@playwright/test';

/**
 * PromQL via Grafana → cerberus → ClickHouse (otel_metrics_*).
 *
 * Strategy (beyond what smoke.spec.ts covers):
 *   1. rate() over a range — proves the windowed-array SQL path
 *      reaches CH and decodes back as a matrix.
 *   2. /api/v1/labels — proves it returns a non-empty list.
 *   3. /api/v1/label/__name__/values — proves it returns the metric
 *      names we seeded.
 *
 * All go through Grafana's datasource proxy.
 */

const promProxy = '/api/datasources/proxy/uid/cerberus-prometheus/api/v1';

test('rate(http_server_request_duration_count[5m]) returns a matrix', async ({ request }) => {
  const now = Math.floor(Date.now() / 1000);
  const start = now - 5 * 60;
  const q = encodeURIComponent('rate(http_server_request_duration_count[5m])');
  const url = `${promProxy}/query_range?query=${q}&start=${start}&end=${now}&step=30`;

  const resp = await request.get(url);
  expect(resp.status(), 'prom /query_range status').toBe(200);

  const body = await resp.json();
  expect(body.status, 'prom response status').toBe('success');
  expect(body.data.resultType, 'prom resultType').toBe('matrix');
  expect(body.data.result.length, 'at least one series').toBeGreaterThan(0);
});

test('prom /api/v1/labels returns label names', async ({ request }) => {
  const resp = await request.get(`${promProxy}/labels`);
  expect(resp.status(), 'prom /labels status').toBe(200);

  const body = await resp.json();
  expect(body.status, 'prom labels status').toBe('success');
  expect(Array.isArray(body.data), 'data is an array').toBe(true);
  expect(body.data.length, 'at least one label').toBeGreaterThan(0);
  // The seed inserts `job` as an Attributes key; should surface.
  expect(body.data, 'job in labels').toContain('job');
});

test('prom /api/v1/label/__name__/values returns seeded metric names', async ({ request }) => {
  const resp = await request.get(`${promProxy}/label/__name__/values`);
  expect(resp.status(), 'prom label values status').toBe(200);

  const body = await resp.json();
  expect(body.status, 'prom label values status').toBe('success');
  expect(Array.isArray(body.data), 'data is an array').toBe(true);
  // Seeded metrics: `up` (gauge) and `http_server_request_duration_count` (sum).
  expect(body.data, 'up metric present').toContain('up');
  expect(body.data, 'counter metric present').toContain('http_server_request_duration_count');
});

test('subquery max_over_time(rate(...)[5m:1m]) returns the canonical Grafana shape', async ({ request }) => {
  const q = encodeURIComponent('max_over_time(rate(http_server_request_duration_count[5m])[5m:1m])');
  const url = `${promProxy}/query?query=${q}`;

  const resp = await request.get(url);
  expect(resp.status(), 'prom /query status').toBe(200);

  const body = await resp.json();
  expect(body.status, 'prom response status').toBe('success');
  expect(body.data.resultType, 'prom resultType').toBe('vector');
  expect(body.data.result.length, 'at least one series').toBeGreaterThan(0);
});

test('subquery up[1m:30s] (bare vector) returns a vector', async ({ request }) => {
  const q = encodeURIComponent('up[1m:30s]');
  const url = `${promProxy}/query?query=${q}`;

  const resp = await request.get(url);
  expect(resp.status(), 'prom /query status').toBe(200);

  const body = await resp.json();
  expect(body.status, 'prom response status').toBe('success');
  expect(body.data.result.length, 'at least one series').toBeGreaterThan(0);
});

/**
 * Grafana's Metrics Explorer (Explore → Metrics) surfaces the BARE
 * histogram base name from cerberus's `__name__` listing — e.g.
 * `http_server_request_duration` — and queries `match[]=<base>` for
 * the labels chip. Before the fan-out fix the bare-name matcher
 * lowered to a gauge-table scan, matched zero rows, and the chip
 * rendered "Unable to fetch labels".
 *
 * This test pins the contract end-to-end: a bare histogram name
 * resolves to the labels of its `_bucket` companion, including the
 * synthetic `le` key.
 */
test('prom /api/v1/labels?match[]=<histogram_base> includes le from bucket companion', async ({ request }) => {
  const matcher = encodeURIComponent('http_server_request_duration');
  const resp = await request.get(`${promProxy}/labels?match%5B%5D=${matcher}`);
  expect(resp.status(), 'prom /labels status').toBe(200);

  const body = await resp.json();
  expect(body.status, 'prom labels status').toBe('success');
  expect(Array.isArray(body.data), 'data is an array').toBe(true);
  // The fan-out reaches the histogram-table `_bucket` companion, so
  // the synthetic `le` label must surface — that's the chip Grafana
  // Metrics Explorer renders.
  expect(body.data, 'le label from bucket companion').toContain('le');
});

/**
 * Companion to the labels test: /api/v1/series with a bare-name
 * matcher returns the three companion series (`_bucket`, `_count`,
 * `_sum`). Grafana uses this surface for the same labels chip when
 * the explorer asks for one row per series.
 */
test('prom /api/v1/series?match[]=<histogram_base> returns companion series', async ({ request }) => {
  const matcher = encodeURIComponent('http_server_request_duration');
  const resp = await request.get(`${promProxy}/series?match%5B%5D=${matcher}`);
  expect(resp.status(), 'prom /series status').toBe(200);

  const body = await resp.json();
  expect(body.status, 'prom series status').toBe('success');
  expect(Array.isArray(body.data), 'data is an array').toBe(true);
  expect(body.data.length, 'at least one series from companion fan-out').toBeGreaterThan(0);
  const names = new Set(body.data.map((s: Record<string, string>) => s.__name__));
  // At least one of the companion names must appear; the bucket
  // companion is the load-bearing one for the labels chip.
  const seenBucket = names.has('http_server_request_duration_bucket');
  expect(seenBucket, `expected http_server_request_duration_bucket in series; got ${[...names].join(',')}`).toBe(true);
});
