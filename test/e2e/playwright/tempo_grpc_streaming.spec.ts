import { test, expect, type APIRequestContext } from '@playwright/test';

/**
 * Tempo gRPC/h2c StreamingQuerier coverage (#1454).
 *
 * Every other tempo_*.spec.ts in this suite drives cerberus's Tempo
 * HTTP handlers through Grafana's datasource PROXY
 * (`/api/datasources/proxy/uid/cerberus-tempo/...`) — a plain HTTP
 * request that never touches the gRPC/h2c `StreamingQuerier` listener
 * cerberus also serves on the same `:8080` socket (see
 * cmd/cerberus/main.go's dual-stack HTTP/1.1 + h2c/gRPC listener, and
 * internal/api/tempo/grpc/server.go for the server-side wiring). Before
 * this spec, that listener had zero coverage above its own bufconn unit
 * tests: no provisioned datasource ever set `jsonData.streamingEnabled`,
 * so Grafana's Tempo plugin never had a reason to open it.
 *
 * The trigger. Grafana's Tempo *frontend* datasource (not a raw HTTP
 * call this suite can hand-roll) is what opens the gRPC path: when
 * `jsonData.streamingEnabled.search` is true, `TempoDatasource.query()`
 * routes a TraceQL search through `handleStreamingQuery()`, which opens
 * a Grafana Live subscription; Grafana's own backend Go plugin
 * (`pkg/tsdb/tempo/grpc.go`) then dials cerberus over h2c and streams
 * the response back. That whole chain only runs inside a real Grafana
 * frontend session — there is no REST shortcut for it — so this spec
 * navigates an actual Explore page (mirroring the `exploreLeft` URL-state
 * pattern `compose_grafana_smoke.spec.ts` already uses for its
 * `explore:tempo` surface) instead of using the `request` fixture the
 * rest of this suite prefers.
 *
 * The oracle. cerberus's gRPC server is wrapped in
 * `otelgrpc.NewServerHandler()`, which (pinned contrib v0.69.0, default
 * semconv mode) records the STABLE v1.41.0 `rpc.server.call.duration`
 * histogram per RPC — verified against the pinned module's own
 * `semconv/v1.41.0/rpcconv/metric.go` source, not assumed from the
 * (stale) `rpc.server.duration` name an older doc comment in
 * server.go used to cite. The e2e/compose otel-collector's
 * `transform/metric_names` processor (test/e2e/otel-collector/
 * compose-config.yaml) rewrites every dotted OTel metric name to
 * underscores before it reaches ClickHouse, so the series cerberus's
 * own Prom head answers is `rpc_server_call_duration_count` (companion
 * of `_sum` / `_bucket{le=...}`, following the same `<base>_count`
 * convention `internal/promql/histogram_synthetic_names.go` already
 * serves for e.g. `http_server_request_duration_count`).
 *
 * Baseline discipline (same shape as #1502): rather than hard-asserting
 * the counter is exactly absent/zero at the top of THIS test — which
 * would flake depending on worker/file scheduling, since
 * `compose_grafana_smoke.spec.ts`'s own `explore:tempo` surface targets
 * the same now-streaming-enabled Cerberus-Tempo datasource and may have
 * already ticked it — the test captures whatever the counter reads
 * before its own trigger and then asserts a genuine DELTA after. That
 * keeps the assertion bidirectional (it fails exactly when the gRPC
 * listener isn't actually being exercised) without depending on test
 * order for correctness.
 */

const promProxy = '/api/datasources/proxy/uid/cerberus-prometheus/api/v1';

// See the file-level comment: verified against the pinned otelgrpc
// v0.69.0 module + its default semconv v1.41.0 rpcconv package, then
// underscored by the e2e collector's transform/metric_names processor.
const grpcDurationCountMetric = 'rpc_server_call_duration_count';

// One OTel export cycle is 10s (CERBERUS_OTLP_EXPORT_INTERVAL); the
// deadline gives several cycles of headroom plus room for a cold
// cerberus->CH circuit breaker (see project note on passive breaker
// recovery) without ever masking a real regression as a timeout.
const GRPC_METRIC_POLL_DEADLINE_MS = 90_000;
const GRPC_METRIC_POLL_INTERVAL_MS = 3_000;

/** Sum of an instant PromQL query's vector, via the Cerberus-Prometheus
 * Grafana datasource proxy (self-dogfooding cerberus's own gRPC-server
 * telemetry through its own Prom head — see #1454's triage decision).
 * A metric with no series yet (absent, not merely zero) sums to 0,
 * which is exactly the "not exercised yet" baseline this spec needs.
 */
async function queryInstantSum(
  request: APIRequestContext,
  expr: string,
): Promise<number> {
  const resp = await request.get(
    `${promProxy}/query?query=${encodeURIComponent(expr)}`,
  );
  expect(resp.status(), `instant query ${expr} status`).toBe(200);
  const body = (await resp.json()) as {
    status?: string;
    data?: { result?: Array<{ value?: [number, string] }> };
  };
  expect(body.status, `instant query ${expr} envelope status`).toBe('success');
  const result = body.data?.result ?? [];
  return result.reduce((sum, s) => sum + Number(s.value?.[1] ?? 0), 0);
}

/** Builds a Grafana Explore URL that pre-selects the Cerberus-Tempo
 * datasource with a raw TraceQL query and auto-runs it on load — the
 * same `left`-state mechanism compose_grafana_smoke.spec.ts's
 * `exploreLeft` helper uses. `queryType: 'traceql'` + a non-empty
 * `query` string is exactly the shape TempoDatasource.query() routes
 * through `handleTraceQlQuery` -> `handleStreamingQuery` when
 * `streamingEnabled.search` is on.
 */
function exploreTraceQlURL(query: string): string {
  const state = {
    datasource: 'cerberus-tempo',
    queries: [
      {
        refId: 'A',
        queryType: 'traceql',
        query,
        datasource: { type: 'tempo', uid: 'cerberus-tempo' },
      },
    ],
    range: { from: 'now-1h', to: 'now' },
  };
  return `/explore?orgId=1&left=${encodeURIComponent(JSON.stringify(state))}`;
}

test('Tempo streaming search exercises the gRPC/h2c StreamingQuerier listener', async ({
  page,
  request,
}) => {
  test.setTimeout(GRPC_METRIC_POLL_DEADLINE_MS + 30_000);

  const baseline = await queryInstantSum(request, grpcDurationCountMetric);

  // Trigger: a real Explore navigation against the streaming-enabled
  // Cerberus-Tempo datasource. `{}` is the match-everything TraceQL
  // search the rest of this suite already seeds data for (see
  // tempo_ux.spec.ts), so it always has something to stream back.
  await page.goto(exploreTraceQlURL('{}'));
  await page.waitForLoadState('networkidle');

  const deadline = Date.now() + GRPC_METRIC_POLL_DEADLINE_MS;
  let latest = baseline;
  for (;;) {
    latest = await queryInstantSum(request, grpcDurationCountMetric);
    if (latest > baseline) break;
    if (Date.now() >= deadline) break;
    await new Promise((r) => setTimeout(r, GRPC_METRIC_POLL_INTERVAL_MS));
  }

  expect(
    latest,
    `${grpcDurationCountMetric} did not increase after a Tempo streaming search ` +
      `(baseline=${baseline}, latest=${latest} after ${GRPC_METRIC_POLL_DEADLINE_MS}ms) — ` +
      `the gRPC/h2c StreamingQuerier listener was not exercised`,
  ).toBeGreaterThan(baseline);
});
