import { test, expect } from '@playwright/test';

/**
 * Mirrors Grafana's Tempo trace-search-then-open flow:
 *
 *   1. /api/search?q=<TraceQL> — returns trace summaries with
 *      traceID + rootServiceName + durationMs (these populate the
 *      Tempo search-result tile view).
 *   2. /api/traces/{id} — returns the trace's batches+spans (this
 *      renders the waterfall view).
 *
 * For step 1 to be useful in step 2, the summary's traceID field
 * needs to round-trip — that's the most common breakage class
 * (toTraceSummaries currently uses a synthetic key, so the test
 * verifies a known-seeded traceID works on its own).
 */

const tempoProxy = '/api/datasources/proxy/uid/cerberus-tempo/api';

test('search-then-open: frontend traces yield batches', async ({ request }) => {
  // Step 1: search.
  const searchResp = await request.get(
    `${tempoProxy}/search?q=${encodeURIComponent('{ resource.service.name = "frontend" }')}`,
  );
  expect(searchResp.status(), 'search status').toBe(200);
  const searchBody = await searchResp.json();
  expect(Array.isArray(searchBody.traces), 'traces is array').toBe(true);
  expect(searchBody.traces.length, '≥1 frontend trace summary').toBeGreaterThan(0);

  // Step 2: open one of the seeded traces by its known ID.
  const seededID = 'a0000000000000000000000000000001';
  const traceResp = await request.get(`${tempoProxy}/traces/${seededID}`);
  expect(traceResp.status(), `traces/${seededID} status`).toBe(200);
  const traceBody = await traceResp.json();
  expect(Array.isArray(traceBody.batches), 'batches is array').toBe(true);
  expect(traceBody.batches.length, '≥1 batch in trace').toBeGreaterThan(0);

  let totalSpans = 0;
  for (const b of traceBody.batches) {
    totalSpans += b.spans?.length ?? 0;
  }
  expect(totalSpans, '≥1 span across all batches').toBeGreaterThan(0);
});

test('search returns rootServiceName for tile-view rendering', async ({ request }) => {
  // Grafana's tile view (and the new TraceQL Search UI in Grafana 11)
  // displays rootServiceName as the column heading. Empty / missing
  // values break the UI.
  const resp = await request.get(
    `${tempoProxy}/search?q=${encodeURIComponent('{ resource.service.name = "frontend" }')}`,
  );
  expect(resp.status()).toBe(200);
  const body = await resp.json();
  for (const summary of body.traces) {
    expect(summary.rootServiceName, 'rootServiceName').toBeTruthy();
  }
});

test('not-found trace returns the Tempo error envelope', async ({ request }) => {
  // Grafana renders a specific "Trace not found" UI when the response
  // body matches Tempo's distinct envelope: {traceID,spanID,error,message}.
  const resp = await request.get(`${tempoProxy}/traces/deadbeef00000000000000000000dead`);
  expect(resp.status()).toBe(404);
  const body = await resp.json();
  expect(body.error, 'envelope.error').toBe(true);
  expect(body.message, 'envelope.message present').toBeTruthy();
});
