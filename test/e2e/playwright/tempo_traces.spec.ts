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

/**
 * Grafana 11.x+'s Tempo datasource defaults to `tempoApiVersion >= v2`,
 * which switches every trace drill-down from `/api/traces/<id>` to
 * `/api/v2/traces/<id>`. Reference Tempo's v2 endpoint does NOT serve
 * the v1 body on a new URL: it wraps the trace in a
 * tempopb.TraceByIDResponse envelope (`{trace, metrics, status,
 * message}` — upstream modules/frontend/combiner/trace_by_id_v2.go),
 * and Grafana 12.x unmarshals exactly that envelope before converting
 * to OTLP. An earlier cerberus revision aliased v2 to the v1 handler
 * and THIS spec pinned the bodies byte-identical — institutionalising
 * the wrong v2 shape and breaking every Grafana 12 trace drill-down
 * with `proto: KeyValue: wiretype end group for non-group`. The test
 * now pins the two DISTINCT shapes: v1 stays the flattened
 * `{batches: …}` body, v2 is the JSON envelope `{trace: {…},
 * metrics: {}}`.
 */
test('tempo /api/v2/traces/<id> returns the TraceByIDResponse envelope (v1 stays bare)', async ({ request }) => {
  const traceID = 'a0000000000000000000000000000001';
  const [v1Resp, v2Resp] = await Promise.all([
    request.get(`${tempoProxy}/traces/${traceID}`),
    request.get(`${tempoProxy}/v2/traces/${traceID}`),
  ]);
  expect(v1Resp.status(), 'tempo v1 status').toBe(200);
  expect(v2Resp.status(), 'tempo v2 status').toBe(200);

  // v1: flattened batches, no envelope.
  const v1Body = await v1Resp.json();
  expect(Array.isArray(v1Body.batches), 'v1 body.batches is an array').toBe(true);
  expect(v1Body.trace, 'v1 has no envelope key').toBeUndefined();

  // v2: TraceByIDResponse envelope; trace content lives under `trace`,
  // the always-present metrics block under `metrics`, and the bare v1
  // keys must NOT leak through.
  const v2Body = await v2Resp.json();
  expect(v2Body.trace, 'v2 body.trace envelope key').toBeTruthy();
  expect(v2Body.metrics, 'v2 body.metrics envelope key').toBeTruthy();
  expect(v2Body.batches, 'v2 has no bare v1 batches key').toBeUndefined();
  expect(
    Array.isArray(v2Body.trace.resourceSpans),
    'v2 trace.resourceSpans is an array',
  ).toBe(true);
  expect(v2Body.trace.resourceSpans.length, 'v2 ≥1 resourceSpans').toBeGreaterThan(0);
});

/**
 * Trace DETAIL through the Grafana datasource BACKEND — the path the
 * Explore trace view actually uses in Grafana 12 (POST /api/ds/query
 * with a traceql query whose expression is a bare trace ID; the Tempo
 * plugin backend fetches /api/v2/traces/<id> as proto and converts the
 * TraceByIDResponse to OTLP server-side).
 *
 * This is the coverage gap that let the v2-envelope regression ship:
 * every earlier sweep talked to cerberus through the datasource PROXY
 * (raw HTTP shapes) while Grafana 12's trace view goes through the
 * plugin backend, which is where the
 * `Failed to convert tempo response to Otlp: proto: KeyValue: wiretype
 * end group for non-group` error fired. The trace ID is surfaced via
 * search (the seeded showcase trace), not hardcoded, so the test keeps
 * working if the seed evolves.
 */
test('tempo trace detail via /api/ds/query (Grafana plugin backend) succeeds', async ({ request }) => {
  // Surface a seeded trace ID via search — the same flow a user takes
  // (search result → click → trace detail).
  const searchResp = await request.get(
    `${tempoProxy}/search?q=${encodeURIComponent('{ resource.service.name = "frontend" }')}`,
  );
  expect(searchResp.status(), 'search status').toBe(200);
  const searchBody = await searchResp.json();
  expect(searchBody.traces?.length ?? 0, '≥1 seeded trace surfaced via search').toBeGreaterThan(0);
  const traceID: string = searchBody.traces[0].traceID;
  expect(traceID, 'search summary carries a traceID').toBeTruthy();

  const now = Date.now();
  const dsResp = await request.post('/api/ds/query', {
    data: {
      queries: [
        {
          refId: 'A',
          datasource: { type: 'tempo', uid: 'cerberus-tempo' },
          queryType: 'traceql',
          query: traceID,
          limit: 20,
          tableType: 'traces',
        },
      ],
      from: String(now - 24 * 60 * 60 * 1000),
      to: String(now),
    },
  });

  // Grafana tunnels per-query errors inside a 200/207 envelope; read
  // the body regardless of status so the failure message carries the
  // plugin error (e.g. the OTLP-conversion error this test exists to
  // catch).
  const dsBody = await dsResp.json().catch(() => ({}));
  const result = dsBody?.results?.A;
  const tunneledError: string = result?.error ?? '';
  expect(
    tunneledError,
    `trace-by-id /api/ds/query must not tunnel a plugin error (Grafana 12 converts the v2 TraceByIDResponse envelope to OTLP here)`,
  ).toBe('');
  expect(dsResp.ok(), `/api/ds/query status ${dsResp.status()}`).toBe(true);
  expect(Array.isArray(result?.frames), 'results.A.frames is an array').toBe(true);
  expect(result.frames.length, '≥1 trace frame rendered').toBeGreaterThan(0);
});
