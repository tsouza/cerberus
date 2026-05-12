import { test, expect } from '@playwright/test';

/**
 * Datasource health-check scenarios.
 *
 * Grafana issues a specific request shape to "Test datasource" — each
 * datasource type has its own probe. If the probe path or response
 * shape regresses, the green checkmark in the Datasources settings
 * page silently goes away.
 *
 * These three specs validate the probe path end-to-end through
 * Grafana → cerberus → ClickHouse.
 */

// Grafana 11's Prom datasource health probe issues `?query=1+1`.
// Cerberus folds scalar-only PromQL in Go and returns the canonical
// scalar envelope without a CH round-trip.
test('prometheus datasource health probe succeeds', async ({ request }) => {
  const resp = await request.get(
    '/api/datasources/proxy/uid/cerberus-prometheus/api/v1/query?query=1%2B1',
  );
  expect(resp.status(), 'cerberus-prometheus probe status').toBe(200);
  const body = await resp.json();
  expect(body.status, 'body.status').toBe('success');
  expect(body.data.resultType, 'scalar resultType').toBe('scalar');
  // result shape: [<ts_float>, "<value_string>"]
  expect(body.data.result[1], 'folded value').toBe('2');
});

// `up` instant-vector probe — the smallest supported shape that
// actually hits ClickHouse.
test('prometheus datasource probe — query=up works', async ({ request }) => {
  const resp = await request.get(
    '/api/datasources/proxy/uid/cerberus-prometheus/api/v1/query?query=up',
  );
  expect(resp.status(), 'cerberus-prometheus probe status').toBe(200);
  const body = await resp.json();
  expect(body.status, 'body.status').toBe('success');
});

test('tempo datasource health probe — /api/echo', async ({ request }) => {
  // Grafana's Tempo datasource probe hits /api/echo.
  const resp = await request.get(
    '/api/datasources/proxy/uid/cerberus-tempo/api/echo',
  );
  expect(resp.status(), 'cerberus-tempo /api/echo status').toBe(200);
  expect((await resp.text()).trim(), 'echo body').toBe('echo');
});

test('tempo datasource version probe', async ({ request }) => {
  // Grafana hits /api/status/version to display the Tempo version
  // string in the datasource settings page.
  const resp = await request.get(
    '/api/datasources/proxy/uid/cerberus-tempo/api/status/version',
  );
  expect(resp.status(), 'cerberus-tempo /api/status/version status').toBe(200);
  const body = await resp.json();
  expect(body.version, 'version field').toBeTruthy();
});
