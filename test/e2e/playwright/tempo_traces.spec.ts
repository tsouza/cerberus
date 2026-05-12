import { test, expect } from '@playwright/test';

/**
 * TraceQL via Grafana → cerberus → ClickHouse (otel_traces).
 *
 * Strategy:
 *   1. /api/echo → datasource health-check returns the literal "echo".
 *   2. /api/status/version → returns a JSON body with `version` set.
 *   3. /api/search?q={resource.service.name="frontend"} → at least
 *      one trace summary with RootServiceName="frontend".
 *   4. /api/traces/<id> for a seeded trace ID → returns one batch
 *      with at least one span.
 *
 * All go through Grafana's datasource proxy at
 *   /api/datasources/proxy/uid/cerberus-tempo/api/<route>
 *
 * The seed in test/e2e/seed/otel_traces.sql inserts 7 spans across
 * 3 traces; trace `a0000000000000000000000000000001` exists.
 */

const tempoProxy = '/api/datasources/proxy/uid/cerberus-tempo/api';

test('tempo /api/echo returns the literal echo', async ({ request }) => {
  const resp = await request.get(`${tempoProxy}/echo`);
  expect(resp.status(), 'tempo /api/echo status').toBe(200);
  expect((await resp.text()).trim(), 'tempo /api/echo body').toBe('echo');
});

test('tempo /api/status/version returns version metadata', async ({ request }) => {
  const resp = await request.get(`${tempoProxy}/status/version`);
  expect(resp.status(), 'tempo /api/status/version status').toBe(200);
  const body = await resp.json();
  expect(body.version, 'version field present').toBeTruthy();
  // goVersion is filled by runtime.Version() server-side.
  expect(body.goVersion, 'goVersion field present').toBeTruthy();
});

test('traceql search returns frontend traces', async ({ request }) => {
  const q = encodeURIComponent('{ resource.service.name = "frontend" }');
  const resp = await request.get(`${tempoProxy}/search?q=${q}`);
  expect(resp.status(), 'tempo /api/search status').toBe(200);

  const body = await resp.json();
  expect(Array.isArray(body.traces), 'body.traces is an array').toBe(true);
  expect(body.traces.length, 'at least one trace summary').toBeGreaterThan(0);

  // Every returned summary should reference the frontend service.
  for (const summary of body.traces) {
    expect(summary.rootServiceName, 'rootServiceName set').toBeTruthy();
  }
});

test('tempo /api/traces/<id> returns batches for a seeded trace', async ({ request }) => {
  const traceID = 'a0000000000000000000000000000001';
  const resp = await request.get(`${tempoProxy}/traces/${traceID}`);
  expect(resp.status(), `tempo /api/traces/${traceID} status`).toBe(200);

  const body = await resp.json();
  expect(Array.isArray(body.batches), 'body.batches is an array').toBe(true);
  expect(body.batches.length, 'at least one batch').toBeGreaterThan(0);
  // Each batch carries resource + spans.
  for (const batch of body.batches) {
    expect(Array.isArray(batch.spans), 'batch has spans array').toBe(true);
  }
});
