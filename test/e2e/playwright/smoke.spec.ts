import { test, expect } from '@playwright/test';

/**
 * Smoke test for the Grafana <-> cerberus integration.
 *
 * Strategy:
 *   1. Hit Grafana's /api/health to confirm Grafana itself is up.
 *   2. Hit cerberus's /healthz to confirm cerberus is up.
 *   3. Query through Grafana's datasource proxy at
 *      /api/datasources/proxy/uid/cerberus-prometheus/api/v1/query?query=up
 *      and assert we get a `success` Prom-shaped response with at least
 *      one series carrying the `job` label. This proves the entire
 *      chain: Grafana sees cerberus as a Prom datasource, cerberus
 *      receives the request, lowers it, executes against ClickHouse,
 *      and returns Prom-shaped JSON that Grafana parses.
 *
 * Grafana anonymous viewer access is enabled in deploy/k3s/grafana.yaml,
 * so the request goes through without auth.
 *
 * UI-level (Explore page) checks land once M2 lands real label/series
 * endpoints; today the metadata picker in Explore would error.
 */
test('grafana sees cerberus as a prometheus datasource', async ({ request }) => {
  // Grafana health.
  const grafanaHealth = await request.get('/api/health');
  expect(grafanaHealth.status(), 'grafana /api/health').toBe(200);

  // cerberus health (direct, not via Grafana).
  const cerberusURL = process.env.CERBERUS_URL ?? 'http://localhost:8080';
  const cerberusHealth = await request.get(`${cerberusURL}/healthz`);
  expect(cerberusHealth.status(), 'cerberus /healthz').toBe(200);

  // Datasource proxy query via Grafana.
  const queryResp = await request.get(
    '/api/datasources/proxy/uid/cerberus-prometheus/api/v1/query?query=up',
  );
  expect(queryResp.status(), 'grafana ds-proxy query status').toBe(200);

  const body = await queryResp.json();
  expect(body.status, 'prom response status').toBe('success');
  expect(body.data.resultType, 'prom resultType').toBe('vector');
  expect(body.data.result.length, 'at least one series in vector').toBeGreaterThan(0);

  // Every series should carry __name__=up and a job label (from the seed).
  for (const sample of body.data.result) {
    expect(sample.metric.__name__, 'series __name__').toBe('up');
    expect(sample.metric.job, 'series job label').toBeTruthy();
  }
});
