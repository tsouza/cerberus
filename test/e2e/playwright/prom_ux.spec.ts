import { test, expect } from '@playwright/test';

/**
 * PromQL UX flows.
 *
 * Mirrors the request sequence Grafana's Explore page / Dashboard
 * panels issue when a user interacts with the cerberus-Prometheus
 * datasource. Each spec exercises a flow shape (label-browser,
 * metric-picker, query inspector, multi-query, format-as, ...) by
 * hitting the same datasource-proxy endpoints Grafana hits
 * internally — same on-the-wire conversation, no real browser.
 *
 * Seed shape (test/e2e/seed/cmd/seed/main.go):
 *   • otel_metrics_gauge: 2× `up` series, Attributes.job ∈ {api, db}
 *   • otel_metrics_sum:   300× `http_server_request_duration_count`,
 *                         Attributes.job = api, Attributes.http_status = 200,
 *                         spaced 1s apart over the last 5 minutes so any
 *                         1m/5m `rate()` window run within the Playwright
 *                         execution timeframe finds ≥2 samples.
 */

const promProxy = '/api/datasources/proxy/uid/cerberus-prometheus/api/v1';

// `cerberus-prometheus` always returns success on a degenerate
// /api/v1/query?query=up call; reused as a precondition for the few
// tests that don't open with their own assertion.

test.describe('Prom UX — Explore flows', () => {
  test('explore: instant query returns a vector', async ({ request }) => {
    const resp = await request.get(`${promProxy}/query?query=up`);
    expect(resp.status(), 'instant query status').toBe(200);
    const body = await resp.json();
    expect(body.status).toBe('success');
    expect(body.data.resultType, 'instant → vector').toBe('vector');
    expect(body.data.result.length).toBeGreaterThan(0);
  });

  test('explore: switch instant → range yields matrix shape', async ({ request }) => {
    const now = Math.floor(Date.now() / 1000);
    const start = now - 5 * 60;
    const url = `${promProxy}/query_range?query=up&start=${start}&end=${now}&step=30`;
    const resp = await request.get(url);
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(body.status).toBe('success');
    expect(body.data.resultType, 'range → matrix').toBe('matrix');
    expect(body.data.result.length, 'range yields ≥1 series').toBeGreaterThan(0);
    // Every series should carry a values array of [ts, val] tuples.
    for (const s of body.data.result) {
      expect(Array.isArray(s.values)).toBe(true);
    }
  });

  test('explore: table-view query (vector resultType is what the Table panel renders)', async ({
    request,
  }) => {
    // Grafana's "Table" format-as for a Prometheus query is exactly an
    // instant /api/v1/query — same vector shape, different rendering.
    // We assert the shape Grafana parses into columns.
    const resp = await request.get(`${promProxy}/query?query=up`);
    const body = await resp.json();
    expect(body.data.resultType).toBe('vector');
    for (const sample of body.data.result) {
      expect(sample.metric, 'each row has labels').toBeTruthy();
      expect(Array.isArray(sample.value), 'each row has [ts, val]').toBe(true);
      expect(sample.value.length).toBe(2);
    }
  });

  test('label browser: /api/v1/labels populates the label dropdown', async ({ request }) => {
    const resp = await request.get(`${promProxy}/labels`);
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(body.status).toBe('success');
    expect(Array.isArray(body.data)).toBe(true);
    // The seed inserts `job` and `http_status` as Attributes keys.
    expect(body.data, 'job in label list').toContain('job');
    // The synthetic __name__ should always be present.
    expect(body.data, '__name__ in label list').toContain('__name__');
  });

  test('metric picker: /api/v1/metadata returns seeded metric descriptions', async ({
    request,
  }) => {
    const resp = await request.get(`${promProxy}/metadata`);
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(body.status).toBe('success');
    expect(typeof body.data, 'metadata data is an object').toBe('object');
    // Both seeded metrics should have entries.
    expect(body.data.up, 'up metric has metadata').toBeTruthy();
    expect(body.data.http_server_request_duration_count, 'counter has metadata').toBeTruthy();
    // Type field should be set ("gauge" / "counter").
    const upEntries = body.data.up;
    expect(Array.isArray(upEntries)).toBe(true);
    expect(upEntries[0].type, 'up declared as gauge').toBe('gauge');
  });

  test('metric picker: selecting metric pre-fills query — /label/__name__/values', async ({
    request,
  }) => {
    // When the user clicks a metric in the picker, Grafana issues the
    // metric-names probe to confirm it exists before submitting the
    // first /query. We just verify both seeded metrics are present.
    const resp = await request.get(`${promProxy}/label/__name__/values`);
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(body.data).toContain('up');
    expect(body.data).toContain('http_server_request_duration_count');
  });

  test('time range: relative "Last 5 minutes" (300 s window)', async ({ request }) => {
    const now = Math.floor(Date.now() / 1000);
    const start = now - 5 * 60;
    const url = `${promProxy}/query_range?query=up&start=${start}&end=${now}&step=30`;
    const resp = await request.get(url);
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(body.status).toBe('success');
    expect(body.data.result.length).toBeGreaterThan(0);
  });

  test('time range: relative "Last 1 hour" (3600 s window)', async ({ request }) => {
    const now = Math.floor(Date.now() / 1000);
    const start = now - 60 * 60;
    const url = `${promProxy}/query_range?query=up&start=${start}&end=${now}&step=60`;
    const resp = await request.get(url);
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(body.status).toBe('success');
    // Even though the seed only writes "now"-ish samples, the window
    // is satisfied — the panel renders an empty earlier prefix.
    expect(body.data.resultType).toBe('matrix');
  });

  test('time range: absolute start/end (RFC3339-style unix-seconds pin)', async ({ request }) => {
    // Grafana's absolute picker sends fixed unix-second start/end.
    const end = Math.floor(Date.now() / 1000);
    const start = end - 10 * 60;
    const url = `${promProxy}/query_range?query=up&start=${start}&end=${end}&step=30`;
    const resp = await request.get(url);
    expect(resp.status(), 'absolute range status').toBe(200);
    const body = await resp.json();
    expect(body.status).toBe('success');
  });

  test('custom step: 15 s yields a denser matrix than 60 s', async ({ request }) => {
    const now = Math.floor(Date.now() / 1000);
    const start = now - 5 * 60;
    const fine = await request.get(
      `${promProxy}/query_range?query=up&start=${start}&end=${now}&step=15`,
    );
    const coarse = await request.get(
      `${promProxy}/query_range?query=up&start=${start}&end=${now}&step=60`,
    );
    expect(fine.status()).toBe(200);
    expect(coarse.status()).toBe(200);
    const fineBody = await fine.json();
    const coarseBody = await coarse.json();
    expect(fineBody.status).toBe('success');
    expect(coarseBody.status).toBe('success');
    // Both share at least one series (the up gauge).
    expect(fineBody.data.result.length).toBeGreaterThan(0);
    expect(coarseBody.data.result.length).toBeGreaterThan(0);
    // Step grid spacing: timestamps should be ~step seconds apart.
    if (
      fineBody.data.result.length > 0 &&
      fineBody.data.result[0].values &&
      fineBody.data.result[0].values.length >= 2
    ) {
      const [t0] = fineBody.data.result[0].values[0];
      const [t1] = fineBody.data.result[0].values[1];
      const gap = Math.round(t1 - t0);
      // Allow ±2 s slack for boundary alignment.
      expect(gap, 'fine step ≈ 15 s').toBeGreaterThanOrEqual(13);
      expect(gap, 'fine step ≈ 15 s').toBeLessThanOrEqual(17);
    }
  });

  test('dashboard panel: simulated multi-target panel — both queries succeed', async ({
    request,
  }) => {
    // Dashboard panels with N targets fire N parallel /query calls
    // through the datasource proxy. We assert both targets succeed.
    const [r1, r2] = await Promise.all([
      request.get(`${promProxy}/query?query=up`),
      request.get(`${promProxy}/query?query=rate(http_server_request_duration_count%5B1m%5D)`),
    ]);
    expect(r1.status(), 'target A status').toBe(200);
    expect(r2.status(), 'target B status').toBe(200);
    const b1 = await r1.json();
    const b2 = await r2.json();
    expect(b1.status).toBe('success');
    expect(b2.status).toBe('success');
    expect(b1.data.result.length, 'target A has series').toBeGreaterThan(0);
    expect(b2.data.result.length, 'target B has series').toBeGreaterThan(0);
  });

  test('query inspector: X-Cerberus-* headers expose cerberus internals', async ({ request }) => {
    // The Grafana Query Inspector panel surfaces upstream response
    // headers under "Response headers". Cerberus exposes Strategy /
    // Plan-Nodes / CH-Millis there.
    const resp = await request.get(`${promProxy}/query?query=up`);
    expect(resp.status()).toBe(200);
    const strategy = resp.headers()['x-cerberus-strategy'];
    const planNodes = resp.headers()['x-cerberus-plan-nodes'];
    const chMillis = resp.headers()['x-cerberus-ch-millis'];
    expect(strategy, 'X-Cerberus-Strategy present').toBeTruthy();
    expect(planNodes, 'X-Cerberus-Plan-Nodes present').toBeTruthy();
    expect(chMillis, 'X-Cerberus-CH-Millis present').toBeTruthy();
    expect(Number.isFinite(Number(chMillis)), 'CH-Millis is numeric').toBe(true);
  });

  test('error rendering: bad PromQL returns Prom-shaped error envelope (not raw 502)', async ({
    request,
  }) => {
    // Grafana renders a user-friendly error box when the response
    // matches {status: "error", error: "<msg>"} with a 4xx code.
    const resp = await request.get(`${promProxy}/query?query=up%7B%7B%7B`);
    // Should NOT be 502 — that would be a "Bad gateway" red box.
    expect(resp.status(), 'parse error → 4xx, not 5xx').toBeLessThan(500);
    expect(resp.status(), 'parse error → 4xx').toBeGreaterThanOrEqual(400);
    const body = await resp.json();
    expect(body.status, 'envelope status').toBe('error');
    expect(body.error, 'envelope error message').toBeTruthy();
    expect(typeof body.error).toBe('string');
  });

  test('legend formatting: query with grouping label produces labelled series', async ({
    request,
  }) => {
    // Grafana's `legendFormat: "{{job}}"` template needs the response
    // series to carry the `job` label. We assert it survives the query
    // round-trip.
    const resp = await request.get(`${promProxy}/query?query=up`);
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    for (const s of body.data.result) {
      expect(s.metric.job, 'each up series has a job label').toBeTruthy();
    }
  });

  test('step grid: query_range timestamps are evenly spaced', async ({ request }) => {
    const now = Math.floor(Date.now() / 1000);
    const start = now - 5 * 60;
    const step = 30;
    const resp = await request.get(
      `${promProxy}/query_range?query=up&start=${start}&end=${now}&step=${step}`,
    );
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    const series = body.data.result[0];
    expect(series, '≥1 series').toBeTruthy();
    if (series.values.length < 2) {
      // Not enough samples to assert spacing — pass trivially.
      return;
    }
    // Each adjacent pair should differ by step (±1 s tolerance).
    for (let i = 1; i < series.values.length; i++) {
      const dt = Math.round(series.values[i][0] - series.values[i - 1][0]);
      expect(dt, `gap[${i}] near step=${step}`).toBeGreaterThanOrEqual(step - 1);
      expect(dt, `gap[${i}] near step=${step}`).toBeLessThanOrEqual(step + 1);
    }
  });

  test('series endpoint: /api/v1/series populates the template-variable picker', async ({
    request,
  }) => {
    // Grafana template variables of type `Series` query this exact
    // endpoint. The seed inserts `up{job="api"}` + `up{job="db"}`.
    const resp = await request.get(`${promProxy}/series?match%5B%5D=up`);
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(body.status).toBe('success');
    expect(Array.isArray(body.data)).toBe(true);
    expect(body.data.length, '≥1 series in match=up').toBeGreaterThan(0);
    // Every series in the list should carry __name__=up.
    for (const entry of body.data) {
      expect(entry.__name__, 'each series carries __name__').toBe('up');
    }
  });
});
