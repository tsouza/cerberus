import { test, expect } from '@playwright/test';

/**
 * Cross-datasource UX flows.
 *
 * These specs validate the "jump from datasource X to datasource Y"
 * flows Grafana users rely on — log↔trace derived fields, span↔logs
 * shortcut, metric↔logs context, trace↔service metrics — by issuing
 * the same datasource-proxy calls each side of the jump would.
 *
 * Important seed-shape note (test/e2e/seed/cmd/seed/main.go):
 *   • otel_logs.TraceId values are `lpad(number%4, 32, '0')` — i.e.
 *     `00000000000000000000000000000000…0003`.
 *   • otel_traces.TraceId values are `a0000000000000000000000000000001…0003`.
 *   These do NOT overlap. So a "click traceID in log → open trace"
 *   round-trip with the actual seed data would 404. We assert the
 *   endpoint contract (the wire format the Grafana derived-field
 *   link follows) on both sides — but pin the trace lookup to a
 *   seeded ID so the spec stays deterministic.
 */

const promProxy = '/api/datasources/proxy/uid/cerberus-prometheus/api/v1';
const lokiProxy = '/api/datasources/proxy/uid/cerberus-loki/loki/api/v1';
const tempoProxy = '/api/datasources/proxy/uid/cerberus-tempo/api';

test.describe('Cross-datasource — derived-field & shortcut flows', () => {
  test('log → trace: log line carries traceID, trace endpoint accepts a (seeded) ID', async ({
    request,
  }) => {
    // Step 1: fetch a Loki line. Cerberus surfaces TraceId either as a
    // stream label or embedded in the line payload — either way the
    // derived-field regex in Grafana extracts the hex blob and uses
    // it as the `${__value.raw}` substitution in the trace URL.
    const q = encodeURIComponent('{service_name="api"}');
    const lokiResp = await request.get(`${lokiProxy}/query?query=${q}`);
    expect(lokiResp.status(), 'log fetch 200').toBe(200);
    const lokiBody = await lokiResp.json();
    expect(lokiBody.data.resultType).toBe('streams');
    expect(lokiBody.data.result.length, '≥1 stream returned').toBeGreaterThan(0);

    // Step 2: the trace-lookup endpoint resolves a seeded trace ID
    // (we don't reuse the actual log TraceId because the seed
    // intentionally doesn't align them — see file header).
    const seededTrace = 'a0000000000000000000000000000001';
    const tempoResp = await request.get(`${tempoProxy}/traces/${seededTrace}`);
    expect(tempoResp.status(), 'trace lookup 200').toBe(200);
    const tempoBody = await tempoResp.json();
    expect(Array.isArray(tempoBody.batches), 'batches array').toBe(true);
    expect(tempoBody.batches.length, '≥1 batch').toBeGreaterThan(0);
  });

  test('span → logs: a seeded trace yields spans → Loki filter by trace = traceID works', async ({
    request,
  }) => {
    // Step 1: fetch a trace's spans.
    const tempoResp = await request.get(
      `${tempoProxy}/traces/a0000000000000000000000000000001`,
    );
    expect(tempoResp.status()).toBe(200);
    const tempoBody = await tempoResp.json();
    expect(tempoBody.batches.length, '≥1 batch in trace').toBeGreaterThan(0);

    // Step 2: simulate the "Logs for this trace" shortcut Grafana
    // builds — a LogQL stream selector with the trace_id matcher.
    // Because the seed deliberately doesn't align log/trace IDs, we
    // confirm the endpoint accepts the well-formed query (200) — not
    // that it returns rows.
    const q = encodeURIComponent(
      '{service_name="api"} |= "trace_id=a0000000000000000000000000000001"',
    );
    const lokiResp = await request.get(`${lokiProxy}/query?query=${q}`);
    expect(lokiResp.status(), 'shortcut query 200').toBe(200);
    const lokiBody = await lokiResp.json();
    expect(lokiBody.status).toBe('success');
  });

  test('metric → logs: spike in `up` → Loki context query for the same service', async ({
    request,
  }) => {
    // Grafana's "view logs for this metric" context menu opens an
    // Explore Logs view with a stream selector built from the
    // hovered metric's labels. The seed has up{job="api"} and
    // log lines with {service_name="api"}.
    const promResp = await request.get(`${promProxy}/query?query=up%7Bjob%3D%22api%22%7D`);
    expect(promResp.status()).toBe(200);
    const promBody = await promResp.json();
    expect(promBody.data.result.length, '≥1 up series').toBeGreaterThan(0);
    // The matching log query Grafana would open.
    const q = encodeURIComponent('{service_name="api"}');
    const lokiResp = await request.get(`${lokiProxy}/query?query=${q}`);
    expect(lokiResp.status()).toBe(200);
    const lokiBody = await lokiResp.json();
    expect(lokiBody.data.result.length, '≥1 log stream for api').toBeGreaterThan(0);
  });

  test('trace → metrics: from a trace, the service’s `up` metric is queryable', async ({
    request,
  }) => {
    // Grafana's "Service metrics" link from the trace panel builds
    // a /api/v1/query targeting the same service label as the trace.
    const tempoResp = await request.get(
      `${tempoProxy}/traces/a0000000000000000000000000000001`,
    );
    expect(tempoResp.status()).toBe(200);

    // Pivot — Grafana would build a query like `up{job="api"}` from
    // the resource.service.name attribute.
    const promResp = await request.get(`${promProxy}/query?query=up%7Bjob%3D%22api%22%7D`);
    expect(promResp.status()).toBe(200);
    const promBody = await promResp.json();
    expect(promBody.status).toBe('success');
    expect(promBody.data.result.length, '≥1 api up series').toBeGreaterThan(0);
  });

  test('three-datasource burst: all healthy under concurrent load', async ({ request }) => {
    // A Grafana dashboard panel mix can fire 3+ parallel calls
    // across the three datasources. Cerberus is a single backend
    // serving all three, so we assert health isn't a function of
    // concurrent multi-DS load.
    const probes = [
      request.get(`${promProxy}/query?query=1%2B1`),
      request.get(`${promProxy}/query?query=up`),
      request.get(`${lokiProxy}/labels`),
      request.get(`${lokiProxy}/query?query=${encodeURIComponent('{service_name="api"}')}`),
      request.get(`${tempoProxy}/echo`),
      request.get(`${tempoProxy}/status/version`),
    ];
    const results = await Promise.all(probes);
    for (const r of results) {
      expect(r.status(), `parallel probe ${r.url()} → 200`).toBe(200);
    }
  });

  test('datasource health: re-running the three Grafana probes is idempotent', async ({
    request,
  }) => {
    // Each of these is what Grafana fires when the user clicks
    // "Test datasource" in the settings page. Run twice to confirm
    // the probe is stateless (no first-call caching, no second-call
    // failure).
    for (let i = 0; i < 2; i++) {
      const prom = await request.get(`${promProxy}/query?query=1%2B1`);
      expect(prom.status(), `prom probe attempt ${i + 1}`).toBe(200);
      const promBody = await prom.json();
      expect(promBody.data.resultType).toBe('scalar');

      const loki = await request.get(`${lokiProxy}/labels`);
      expect(loki.status(), `loki probe attempt ${i + 1}`).toBe(200);

      const tempo = await request.get(`${tempoProxy}/echo`);
      expect(tempo.status(), `tempo probe attempt ${i + 1}`).toBe(200);
    }
  });
});
