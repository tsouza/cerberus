import { test, expect } from '@playwright/test';

/**
 * Service Graph spec.
 *
 * Asserts the OTel-Collector `servicegraph` connector + the
 * cerberus-side `peer.service="clickhouse"` execute-span attribute +
 * the Grafana Tempo datasource's `serviceMap.datasourceUid` wiring all
 * compose into at least one edge (cerberus -> clickhouse) showing up
 * on the Tempo datasource Service Graph tab.
 *
 * The chain under test:
 *
 *   1. cerberus serves a query, opening an `execute` CLIENT-kind span
 *      with peer.service="clickhouse" against the in-stack CH server.
 *   2. The OTLP exporter ships the span to the otel-collector at
 *      otel-collector:4317.
 *   3. The collector's `servicegraph` connector taps the traces
 *      pipeline, derives the
 *      traces_service_graph_request_total{client="cerberus",
 *      server="clickhouse"} series.
 *   4. Those metrics flow back through the metrics/servicegraph
 *      pipeline -> clickhouseexporter -> CH.
 *   5. Grafana's Tempo datasource Service Graph tab queries cerberus's
 *      Prom head for `traces_service_graph_request_total` and renders
 *      the edge.
 *
 * The spec validates step 5 by issuing the same PromQL the Grafana
 * plugin uses; passing means every prior link in the chain
 * round-tripped correctly.
 */

// urlEncodeQuery percent-encodes a PromQL expression for inclusion in
// the api/v1/query querystring. The shorthand keeps the test bodies
// readable while still emitting a strictly-conformant URL.
const enc = (q: string) => encodeURIComponent(q);

// Grafana's Tempo datasource Service Graph tab pivots on this exact
// metric name + label set. Pin the name as a constant so a future
// upstream rename can be caught by failing one test instead of
// silently rendering an empty graph.
const SERVICE_GRAPH_METRIC = 'traces_service_graph_request_total';

test('cerberus prom head serves traces_service_graph_request_total', async ({ request }) => {
  // Drive at least one query through cerberus first so its execute
  // span hits the collector and the servicegraph connector has
  // something to derive an edge from. A scalar Prom query is the
  // cheapest shape that still exercises the CH client (the cerberus
  // engine folds the constant in Go without a real CH round-trip for
  // some shapes — `up` always falls through to CH).
  for (let i = 0; i < 3; i++) {
    const warm = await request.get(
      `/api/datasources/proxy/uid/cerberus-prometheus/api/v1/query?query=${enc('up')}`,
    );
    expect(warm.status(), 'cerberus-prometheus warm-up').toBe(200);
  }

  // metrics_flush_interval on the servicegraph connector is 15s; give
  // the connector + clickhouseexporter time to round-trip the first
  // batch into CH. The test polls instead of sleeping a fixed
  // duration so a fast collector path passes immediately.
  const deadline = Date.now() + 60_000;
  let lastBody: unknown = null;
  let lastStatus = 0;
  while (Date.now() < deadline) {
    const resp = await request.get(
      `/api/datasources/proxy/uid/cerberus-prometheus/api/v1/query?query=${enc(SERVICE_GRAPH_METRIC)}`,
    );
    lastStatus = resp.status();
    if (lastStatus === 200) {
      lastBody = await resp.json();
      const body = lastBody as { status?: string; data?: { result?: unknown[] } };
      if (body.status === 'success' && Array.isArray(body.data?.result) && body.data.result.length > 0) {
        // Found at least one series — at minimum cerberus -> clickhouse.
        // Assert the edge label shape so a rename of the connector's
        // output labels (client / server) is caught here rather than
        // surfacing as an empty Grafana graph.
        const series = body.data.result as Array<{ metric: Record<string, string> }>;
        const labels = series.map((s) => s.metric);
        const hasClickhouseEdge = labels.some(
          (m) => m.server === 'clickhouse' || m.client === 'clickhouse',
        );
        expect(hasClickhouseEdge, `series labels: ${JSON.stringify(labels)}`).toBe(true);
        return;
      }
    }
    await new Promise((r) => setTimeout(r, 2000));
  }
  throw new Error(
    `${SERVICE_GRAPH_METRIC} never returned a non-empty series within 60s; last status=${lastStatus}, body=${JSON.stringify(lastBody)}`,
  );
});

test('tempo datasource is wired to the prometheus datasource for service-graph', async ({ request }) => {
  // Surface mis-provisioning early: if a future edit drops the
  // serviceMap.datasourceUid field, this test fails before any
  // service-graph rendering is attempted. Grafana exposes the
  // datasource definition via /api/datasources/uid/<uid>; jsonData
  // round-trips through the API.
  const resp = await request.get('/api/datasources/uid/cerberus-tempo');
  expect(resp.status(), 'cerberus-tempo datasource GET status').toBe(200);
  const body = (await resp.json()) as { jsonData?: { serviceMap?: { datasourceUid?: string } } };
  expect(body.jsonData?.serviceMap?.datasourceUid, 'serviceMap.datasourceUid').toBe(
    'cerberus-prometheus',
  );
});
