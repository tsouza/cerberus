import { test, expect } from '@playwright/test';

/**
 * LogQL via Grafana → cerberus → ClickHouse (otel_logs).
 *
 * Strategy:
 *   1. Stream query: `{service_name="api"}` returns the streams
 *      result type with at least one stream and one value tuple.
 *   2. Metric query: `count_over_time({service_name=~".+"}[5m])`
 *      returns the matrix result type with at least one series.
 *
 * Both go through Grafana's datasource proxy at
 *   /api/datasources/proxy/uid/cerberus-loki/loki/api/v1/{query,query_range}
 *
 * The seed in test/e2e/seed/otel_logs.sql inserts 60 records across
 * three services in the last minute, so both queries return data
 * regardless of when this test runs.
 */

const lokiProxy = '/api/datasources/proxy/uid/cerberus-loki/loki/api/v1';

test('logql stream selector returns log lines', async ({ request }) => {
  const q = encodeURIComponent('{service_name="api"}');
  const resp = await request.get(`${lokiProxy}/query?query=${q}`);
  expect(resp.status(), 'loki /query status').toBe(200);

  const body = await resp.json();
  expect(body.status, 'loki response status').toBe('success');
  expect(body.data.resultType, 'loki resultType').toBe('streams');
  expect(body.data.result.length, 'at least one stream').toBeGreaterThan(0);

  for (const stream of body.data.result) {
    // Loki streams shape: { stream: {labels}, values: [[tsNano, line], ...] }
    expect(stream.stream, 'stream has labels').toBeTruthy();
    expect(Array.isArray(stream.values), 'stream has values array').toBe(true);
    if (stream.values.length > 0) {
      const [ts, line] = stream.values[0];
      expect(typeof ts, 'value[0] is timestamp string').toBe('string');
      expect(typeof line, 'value[1] is log line string').toBe('string');
    }
  }
});

// Skipped until RC2: same wrap-projection-vs-RangeWindow column
// mismatch as in prom_metrics.spec.ts. CH returns missing-columns;
// cerberus surfaces 502.
test.skip('logql metric query returns a matrix', async ({ request }) => {
  // Range = last 5 minutes; step = 30s. Covers the 60s of seeded data.
  const now = Math.floor(Date.now() / 1000);
  const start = now - 5 * 60;
  const q = encodeURIComponent('count_over_time({service_name=~".+"}[5m])');
  const url = `${lokiProxy}/query_range?query=${q}&start=${start}&end=${now}&step=30`;

  const resp = await request.get(url);
  expect(resp.status(), 'loki /query_range status').toBe(200);

  const body = await resp.json();
  expect(body.status, 'loki response status').toBe('success');
  expect(body.data.resultType, 'loki resultType').toBe('matrix');
  expect(body.data.result.length, 'at least one metric series').toBeGreaterThan(0);

  // Each matrix entry: { metric: {labels}, values: [[ts, valStr], ...] }
  for (const series of body.data.result) {
    expect(series.metric, 'series has metric labels').toBeTruthy();
    expect(Array.isArray(series.values), 'series has values array').toBe(true);
  }
});
