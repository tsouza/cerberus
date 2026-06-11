/**
 * Consumer-grade /api/ds/query replays — the Grafana PLUGIN BACKEND
 * path, not the datasource proxy.
 *
 * Why this exists on top of the proxy-level probes
 * (iterate-all-dashboards fires `/api/datasources/proxy/uid/…`): the
 * 2026-06-09 AI sweep found two bug layers that are INVISIBLE at the
 * proxy level. `POST /api/ds/query` routes through each datasource
 * plugin's Go backend, which parses cerberus's wire responses into
 * data frames — a response that is valid enough for the proxy
 * pass-through can still fail the plugin's stricter decode (frame
 * schema, RFC3339 shapes, enum values), and Grafana then tunnels the
 * failure into `.results.<refId>.error` or a 4xx/5xx envelope.
 *
 * The replay set per provisioned datasource:
 *   - prometheus: one instant query + one range query.
 *   - loki: one log-stream range query + one metric range query.
 *   - tempo: one trace-by-id lookup via `queryType: "traceId"`.
 *     The Tempo plugin backend supports ONLY trace-by-id: it rejects
 *     `traceql` / `traceqlSearch` query types with "backend TraceQL
 *     search queries are not supported" / "unsupported query type"
 *     (verified against grafana/grafana:12.2.9 — search runs through
 *     the frontend + datasource proxy instead, which the crawler and
 *     iterate-* specs cover). The trace id is harvested live from a
 *     proxy `/api/search` probe so the replay always targets a trace
 *     the stack actually ingested.
 *
 * Every replay asserts: HTTP 2xx, no `.results.<refId>.error`, no
 * `.results.<refId>.errorSource`, and ≥1 frame with a non-empty
 * value column — these queries target self-telemetry the seed
 * traffic guarantees is present, so an empty frame set is a real
 * regression (cerberus wire shape, plugin decode, or seed), never a
 * state to mask.
 *
 * Env:
 *   GRAFANA_URL / GRAFANA_BASE_URL   default http://localhost:3000
 *   CERBERUS_URL                     default http://localhost:8080
 */

import {
  expect,
  test,
  type APIRequestContext,
} from '@playwright/test';

import {
  awaitSelfTelemetryRangeSignal,
  generateSelfTraffic,
} from '../helpers/index.js';
import { truncate } from './lib.js';

const SEED_TRAFFIC_SECONDS = 20;

type DataSourceEntry = {
  uid: string;
  type: string;
  name: string;
};

type DsQueryResults = {
  results?: Record<
    string,
    {
      error?: string;
      errorSource?: string;
      status?: number;
      frames?: Array<{
        schema?: { fields?: Array<{ name?: string }> };
        data?: { values?: unknown[][] };
      }>;
    }
  >;
};

type Replay = {
  label: string;
  /** The single query object POSTed under `queries[]`. */
  query: Record<string, unknown>;
  /** Whether ≥1 frame with ≥1 value row is required. */
  requireData: boolean;
};

function baseURL(): string {
  return (
    process.env.GRAFANA_URL ??
    process.env.GRAFANA_BASE_URL ??
    'http://localhost:3000'
  );
}

async function listDatasources(
  request: APIRequestContext,
): Promise<DataSourceEntry[]> {
  const resp = await request.get(`${baseURL()}/api/datasources`);
  expect(resp.status(), '/api/datasources status').toBe(200);
  const all = (await resp.json()) as DataSourceEntry[];
  return all.filter((d) =>
    ['prometheus', 'loki', 'tempo'].includes(d.type),
  );
}

/**
 * Harvest a real trace id from the stack via the datasource proxy
 * search endpoint — the replay must target a trace that exists.
 */
async function harvestTraceID(
  request: APIRequestContext,
  tempoUid: string,
): Promise<string> {
  const resp = await request.get(
    `${baseURL()}/api/datasources/proxy/uid/${tempoUid}/api/search?q=${encodeURIComponent('{}')}&limit=5`,
  );
  expect(
    resp.status(),
    `proxy /api/search via ${tempoUid} (trace-id harvest)`,
  ).toBe(200);
  const body = (await resp.json()) as {
    traces?: Array<{ traceID?: string }>;
  };
  const id = body.traces?.[0]?.traceID ?? '';
  expect(
    id,
    'at least one trace present after seed traffic (empty search = real bug)',
  ).toMatch(/^[0-9a-f]{32}$/);
  return id;
}

function replaysFor(ds: DataSourceEntry, traceID: string): Replay[] {
  const datasource = { type: ds.type, uid: ds.uid };
  switch (ds.type) {
    case 'prometheus':
      return [
        {
          label: 'prom-instant',
          query: {
            refId: 'A',
            datasource,
            expr: 'cerberus_queries_total',
            instant: true,
            range: false,
            format: 'time_series',
            intervalMs: 15_000,
            maxDataPoints: 500,
          },
          requireData: true,
        },
        {
          label: 'prom-range',
          query: {
            refId: 'A',
            datasource,
            expr: 'sum by (cerberus_ql) (rate(cerberus_queries_total[5m]))',
            instant: false,
            range: true,
            format: 'time_series',
            intervalMs: 15_000,
            maxDataPoints: 500,
          },
          requireData: true,
        },
      ];
    case 'loki':
      return [
        {
          label: 'loki-stream-range',
          query: {
            refId: 'A',
            datasource,
            expr: '{service_name=~".+"}',
            queryType: 'range',
            maxLines: 100,
          },
          requireData: true,
        },
        {
          label: 'loki-metric-range',
          query: {
            refId: 'A',
            datasource,
            expr: 'sum(count_over_time({service_name=~".+"}[5m]))',
            queryType: 'range',
          },
          requireData: true,
        },
      ];
    case 'tempo':
      return [
        {
          label: 'tempo-trace-by-id',
          query: {
            refId: 'A',
            datasource,
            // queryType traceId is the ONLY backend-supported Tempo
            // query type — see the file header.
            queryType: 'traceId',
            query: traceID,
          },
          requireData: true,
        },
      ];
    default:
      throw new Error(`replaysFor: unhandled datasource type ${ds.type}`);
  }
}

test('ds-query replays: every provisioned datasource answers through the plugin backend with data and no tunneled errors', async ({
  request,
}, testInfo) => {
  testInfo.setTimeout(5 * 60_000);

  await generateSelfTraffic(request, SEED_TRAFFIC_SECONDS);
  // rate()-over-range replays need ≥2 exported telemetry samples in
  // the lookback window — on a freshly-booted stack the seed traffic
  // alone doesn't guarantee that yet (see the helper doc; verified
  // red on a healthy stack at ~2min uptime, 2026-06-10). Bounded
  // wait, loud deadline failure — never a skip.
  await awaitSelfTelemetryRangeSignal(request);

  const datasources = await listDatasources(request);
  expect(
    datasources.length,
    'at least one prometheus/loki/tempo datasource provisioned',
  ).toBeGreaterThan(0);

  const tempoUid = datasources.find((d) => d.type === 'tempo')?.uid;
  const traceID = tempoUid ? await harvestTraceID(request, tempoUid) : '';

  const failures: string[] = [];
  const nowMs = Date.now();
  const fromMs = nowMs - 5 * 60_000;

  for (const ds of datasources.sort((a, b) => a.uid.localeCompare(b.uid))) {
    for (const replay of replaysFor(ds, traceID)) {
      const where = `[${ds.uid}:${replay.label}]`;
      const resp = await request.post(`${baseURL()}/api/ds/query`, {
        data: {
          queries: [replay.query],
          from: String(fromMs),
          to: String(nowMs),
        },
      });
      const status = resp.status();
      const bodyText = await resp.text();
      if (status < 200 || status > 299) {
        failures.push(
          `${where} POST /api/ds/query → ${status}\n  body: ${truncate(bodyText, 600)}`,
        );
        continue;
      }
      let parsed: DsQueryResults;
      try {
        parsed = JSON.parse(bodyText) as DsQueryResults;
      } catch (err) {
        failures.push(
          `${where} 2xx body is not JSON: ${(err as Error).message}\n  body: ${truncate(bodyText, 300)}`,
        );
        continue;
      }
      const result = parsed.results?.A;
      if (!result) {
        failures.push(`${where} response carries no results.A entry`);
        continue;
      }
      if (typeof result.error === 'string' && result.error !== '') {
        failures.push(
          `${where} tunneled results.A.error: ${truncate(result.error, 600)}` +
            (result.errorSource ? ` (errorSource=${result.errorSource})` : ''),
        );
        continue;
      }
      const frames = result.frames ?? [];
      if (replay.requireData) {
        const valueRows = frames.reduce(
          (acc, f) =>
            acc +
            (f.data?.values?.reduce(
              (m, col) => Math.max(m, Array.isArray(col) ? col.length : 0),
              0,
            ) ?? 0),
          0,
        );
        if (frames.length === 0 || valueRows === 0) {
          failures.push(
            `${where} expected ≥1 frame with data, got frames=${frames.length} valueRows=${valueRows} — ` +
              `the seed traffic guarantees this query returns data; an empty frame set is a real bug ` +
              `(cerberus wire shape, plugin decode, or seed)`,
          );
        }
      }
    }
  }

  if (failures.length > 0) {
    throw new Error(
      `ds-query replay failures (${failures.length}):\n\n${failures.join('\n\n')}`,
    );
  }
});
