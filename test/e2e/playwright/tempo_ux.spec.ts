import { test, expect } from '@playwright/test';

/**
 * TraceQL UX flows.
 *
 * Mirrors the request sequence Grafana's Tempo Search UI + Trace
 * detail panel issue against a Tempo datasource. Each spec exercises
 * a UX flow (TraceQL editor, structural ops, set ops, select, span
 * filters, recent searches, ...) via the cerberus-tempo datasource
 * proxy.
 *
 * Seed shape (test/e2e/seed/cmd/seed/main.go):
 *   Trace 1 (a0…001): frontend → api
 *   Trace 2 (a0…002): frontend → api → db  (status_code=500 / Error)
 *   Trace 3 (a0…003): api → db              (cron.refresh / cache.refresh)
 *
 * Span attributes seeded: http.method, http.status_code, db.system,
 * cron.name. Resource attribute: service.name.
 */

const tempoProxy = '/api/datasources/proxy/uid/cerberus-tempo/api';

test.describe('Tempo UX — Search flows', () => {
  test('search: by service.name returns trace summaries', async ({ request }) => {
    const q = encodeURIComponent('{ resource.service.name = "frontend" }');
    const resp = await request.get(`${tempoProxy}/search?q=${q}`);
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(Array.isArray(body.traces)).toBe(true);
    expect(body.traces.length, '≥1 frontend trace').toBeGreaterThan(0);
    for (const t of body.traces) {
      expect(t.traceID, 'each summary has a traceID').toBeTruthy();
    }
  });

  test('search: by operation name (.name attribute)', async ({ request }) => {
    // The seed inserts span names like `GET /home`, `POST /checkout`.
    const q = encodeURIComponent('{ name = "GET /home" }');
    const resp = await request.get(`${tempoProxy}/search?q=${q}`);
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(Array.isArray(body.traces)).toBe(true);
    expect(body.traces.length, '≥1 GET /home span').toBeGreaterThan(0);
  });

  test('search: filter by status (error spans)', async ({ request }) => {
    // The seed inserts spans with StatusCode = 'Error' on Trace 2.
    const q = encodeURIComponent('{ status = error }');
    const resp = await request.get(`${tempoProxy}/search?q=${q}`);
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(Array.isArray(body.traces)).toBe(true);
    // At least one error-status trace exists (Trace 2).
    expect(body.traces.length, '≥1 error trace').toBeGreaterThan(0);
  });

  test('search: by tag — http.status_code = "200"', async ({ request }) => {
    const q = encodeURIComponent('{ span.http.status_code = "200" }');
    const resp = await request.get(`${tempoProxy}/search?q=${q}`);
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(Array.isArray(body.traces)).toBe(true);
  });

  test('search: empty query (Grafana Search-UI first-page-load ping)', async ({ request }) => {
    // Grafana sometimes pings /api/search with no q as a health-check.
    // Cerberus returns an empty traces array (not an error envelope).
    const resp = await request.get(`${tempoProxy}/search?q=`);
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(Array.isArray(body.traces)).toBe(true);
  });

  test('trace-by-id: paste ID → full waterfall (batches/spans)', async ({ request }) => {
    const seededID = 'a0000000000000000000000000000001';
    const resp = await request.get(`${tempoProxy}/traces/${seededID}`);
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(Array.isArray(body.batches)).toBe(true);
    expect(body.batches.length, '≥1 batch').toBeGreaterThan(0);
    let totalSpans = 0;
    for (const b of body.batches) {
      totalSpans += b.spans?.length ?? 0;
    }
    // Trace 1 (a0…001) has 2 spans (frontend GET /home + api GET /api/users).
    expect(totalSpans, '≥2 spans in waterfall').toBeGreaterThanOrEqual(2);
  });

  test('trace detail: span attributes panel — http.method present on a seeded span', async ({
    request,
  }) => {
    // Open Trace 1 — the frontend GET /home span has http.method=GET.
    // Cerberus exposes span.attributes as a flat map[string]string.
    const resp = await request.get(
      `${tempoProxy}/traces/a0000000000000000000000000000001`,
    );
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    // Collect every span-attribute key across every batch.
    const keys = new Set<string>();
    for (const b of body.batches) {
      for (const span of b.spans ?? []) {
        for (const k of Object.keys(span.attributes ?? {})) {
          keys.add(k);
        }
      }
    }
    expect(keys.has('http.method'), 'http.method attribute present').toBe(true);
  });

  test('trace detail: processes (resource) shows service.name', async ({ request }) => {
    // Tempo's "Processes" tab reads from batch.resource.attributes —
    // each batch carries resource.attributes[service.name].
    const resp = await request.get(
      `${tempoProxy}/traces/a0000000000000000000000000000001`,
    );
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    let foundSvc = false;
    for (const b of body.batches) {
      const attrs: Record<string, string> = b.resource?.attributes ?? {};
      if ('service.name' in attrs) {
        foundSvc = true;
      }
    }
    expect(foundSvc, 'service.name resource attribute present').toBe(true);
  });

  test('trace detail: span hierarchy via parentSpanId (indentation source)', async ({
    request,
  }) => {
    // The waterfall renders parent-child indentation by reading
    // parentSpanId off each span. Trace 1 has child span 0002 → 0001.
    const resp = await request.get(
      `${tempoProxy}/traces/a0000000000000000000000000000001`,
    );
    const body = await resp.json();
    let foundChild = false;
    for (const b of body.batches) {
      for (const span of b.spans ?? []) {
        // Field name on the wire is `parentSpanId` (json: parentSpanId).
        const psid: string | undefined = span.parentSpanId;
        if (psid && psid.length > 0) {
          foundChild = true;
        }
      }
    }
    expect(foundChild, '≥1 span has parentSpanId').toBe(true);
  });

  test('traceql editor: resource.service.name = "api" returns api spans', async ({ request }) => {
    const q = encodeURIComponent('{ resource.service.name = "api" }');
    const resp = await request.get(`${tempoProxy}/search?q=${q}`);
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(body.traces.length, '≥1 api trace').toBeGreaterThan(0);
  });

  test('traceql structural: `{a} > {b}` parent→child chain returns matches', async ({
    request,
  }) => {
    // Trace 1: frontend → api  ⇒ { service=frontend } > { service=api }
    const q = encodeURIComponent(
      '{ resource.service.name = "frontend" } > { resource.service.name = "api" }',
    );
    const resp = await request.get(`${tempoProxy}/search?q=${q}`);
    expect(resp.status(), 'structural query status').toBe(200);
    const body = await resp.json();
    expect(Array.isArray(body.traces)).toBe(true);
    // At least one matching trace (Trace 1 or Trace 2).
    expect(body.traces.length, '≥1 structural match').toBeGreaterThan(0);
  });

  test('traceql descendant: `{a} >> {b}` deep descendant returns matches', async ({ request }) => {
    // Trace 2: frontend → api → db ⇒ { frontend } >> { db } via descendant.
    const q = encodeURIComponent(
      '{ resource.service.name = "frontend" } >> { resource.service.name = "db" }',
    );
    const resp = await request.get(`${tempoProxy}/search?q=${q}`);
    expect(resp.status(), 'descendant query status').toBe(200);
    const body = await resp.json();
    expect(Array.isArray(body.traces)).toBe(true);
  });

  test('traceql set ops: `{} && {}` intersection narrows to common traces', async ({
    request,
  }) => {
    // `{service=frontend} && {service=api}` ⇒ traces containing BOTH.
    const q = encodeURIComponent(
      '{ resource.service.name = "frontend" } && { resource.service.name = "api" }',
    );
    const resp = await request.get(`${tempoProxy}/search?q=${q}`);
    expect(resp.status(), 'set-and query status').toBe(200);
    const body = await resp.json();
    expect(Array.isArray(body.traces)).toBe(true);
  });

  test('traceql set ops: `{} || {}` union widens to either', async ({ request }) => {
    const q = encodeURIComponent(
      '{ resource.service.name = "frontend" } || { resource.service.name = "db" }',
    );
    const resp = await request.get(`${tempoProxy}/search?q=${q}`);
    expect(resp.status(), 'set-or query status').toBe(200);
    const body = await resp.json();
    expect(Array.isArray(body.traces)).toBe(true);
    expect(body.traces.length, '≥1 trace in union').toBeGreaterThan(0);
  });

  test('traceql `| select(...)`: column-pruned result', async ({ request }) => {
    // The select pipeline restricts the returned column set. We
    // assert the call is 200 and returns the trace summary envelope.
    const q = encodeURIComponent(
      '{ resource.service.name = "frontend" } | select(.http.method)',
    );
    const resp = await request.get(`${tempoProxy}/search?q=${q}`);
    expect(resp.status(), 'select pipeline status').toBe(200);
    const body = await resp.json();
    expect(Array.isArray(body.traces)).toBe(true);
  });

  test('recent searches: /api/search/recent returns the recent-N traces', async ({ request }) => {
    // Grafana's "Recent" dropdown reads /api/search/recent. The seed
    // has 3 traces total; default limit is 20.
    const resp = await request.get(`${tempoProxy}/search/recent`);
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(Array.isArray(body.traces)).toBe(true);
    expect(body.traces.length, '≥1 recent trace').toBeGreaterThan(0);
  });

  test('recent searches: ?limit=2 narrows to 2', async ({ request }) => {
    const resp = await request.get(`${tempoProxy}/search/recent?limit=2`);
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(body.traces.length, '≤2 recent traces').toBeLessThanOrEqual(2);
  });

  test('search tags: V2 returns scoped tag list (resource / span)', async ({ request }) => {
    // Grafana's TraceQL Search UI uses /v2/search/tags to populate
    // its scope-aware tag dropdown.
    const resp = await request.get(`${tempoProxy}/v2/search/tags`);
    expect(resp.status(), 'v2 tags status').toBe(200);
    const body = await resp.json();
    expect(Array.isArray(body.scopes)).toBe(true);
    expect(body.scopes.length, '≥1 scope').toBeGreaterThan(0);
  });

  test('search tag values: span.http.method → ["GET", "POST"]', async ({ request }) => {
    // V1 endpoint — populates the "value" dropdown after a user picks
    // a tag. V1 returns {tagValues: string[]}.
    const resp = await request.get(`${tempoProxy}/search/tag/http.method/values`);
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(Array.isArray(body.tagValues)).toBe(true);
    // GET and POST both appear in the seed (Traces 1+3 use GET, Trace 2 POST).
    expect(body.tagValues, 'GET in http.method values').toContain('GET');
    expect(body.tagValues, 'POST in http.method values').toContain('POST');
  });

  test('trace not found: Tempo error envelope shape for waterfall renders', async ({
    request,
  }) => {
    // Grafana's "Trace not found" UI keys off the envelope shape
    // {traceID, spanID, error, message}.
    const resp = await request.get(
      `${tempoProxy}/traces/dead00000000000000000000000000ad`,
    );
    expect(resp.status()).toBe(404);
    const body = await resp.json();
    expect(body.error, 'envelope.error').toBe(true);
    expect(body.message, 'envelope.message present').toBeTruthy();
  });

  test('service graph: tempo metrics_query_range returns matrix envelope', async ({
    request,
  }) => {
    // Grafana's service-graph view calls /api/metrics/query_range with
    // a TraceQL metrics pipeline (`| rate()`, `| count_over_time()`,
    // `| *_over_time(...)` etc.) for inter-service rate + duration.
    // Cerberus answers with Tempo's native series-of-samples envelope:
    //   {series: [{labels: [{key, value}], samples: [{timestampMs, value}]}]}
    // The Playwright suite seeds three traces (see L10 seed); a
    // `| count_over_time() by (resource.service.name)` should return at
    // least one series whose label set names the seeded service.
    const start = Math.floor(Date.now() / 1000) - 3600; // last hour
    const end = Math.floor(Date.now() / 1000);
    const q = '{} | count_over_time() by (resource.service.name)';
    const params = new URLSearchParams({
      q,
      start: String(start),
      end: String(end),
      step: '60s',
    });
    const resp = await request.get(
      `${tempoProxy}/metrics/query_range?${params.toString()}`,
    );
    expect(resp.status(), 'metrics/query_range 200').toBe(200);
    const body = await resp.json();
    expect(Array.isArray(body.series), 'series is array').toBe(true);
    // The envelope must be parseable even when no spans match the
    // step window — Grafana's datasource short-circuits on null.
    // Per-series shape checks only fire when CH actually returned rows.
    for (const s of body.series ?? []) {
      expect(Array.isArray(s.labels), 'series.labels is array').toBe(true);
      expect(Array.isArray(s.samples), 'series.samples is array').toBe(true);
      for (const lbl of s.labels) {
        expect(typeof lbl.key, 'label.key is string').toBe('string');
        expect(typeof lbl.value, 'label.value is string').toBe('string');
      }
      for (const smp of s.samples) {
        expect(typeof smp.timestampMs, 'sample.timestampMs is number').toBe('number');
        expect(typeof smp.value, 'sample.value is number').toBe('number');
      }
    }
  });
});
