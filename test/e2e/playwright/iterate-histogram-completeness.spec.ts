/**
 * Phase-2 histogram-completeness sweep.
 *
 * Iterates every provisioned dashboard, every panel, every target,
 * and — for any PromQL expression whose top-level call is
 * `histogram_quantile(...)` — runs the histogram-completeness rule
 * (e2e-enhance.md §4.2 rule 2) against cerberus's Prom-flavoured
 * endpoints via Grafana's datasource-proxy URL
 * (`/api/datasources/proxy/uid/<uid>/api/v1/...`).
 *
 * The rule itself is two-pronged because the same call shape carries
 * two regression classes that have opposite "what's a healthy
 * response":
 *
 *   - N5 (`<name>_bucket` series MUST exist when the panel is meant
 *     to render). The cerberus dashboard's "P95 latency by language" panel
 *     went flat at 0 because the underlying bucket series were
 *     emitted under a sibling metric root (cerberus_pipeline vs
 *     cerberus_queries_duration_seconds_bucket), and
 *     `histogram_quantile` over an absent bucket resolved to nothing
 *     visible on the wire (no tunneled error, just 200 + empty).
 *     The pin: probe `/api/v1/series?match[]=<name>_bucket` returns
 *     ≥ 1 series; AND when the buckets exist, the
 *     `histogram_quantile` response itself is non-empty.
 *
 *   - N6 (`histogram_quantile` over a non-bucket metric used to
 *     fabricate a value). Typing `histogram_quantile(0.95, foo_total)`
 *     in Prom Explore returned a synthetic non-empty float because
 *     the optimiser wrapped the inner Sample scan in `toFloat64`
 *     unconditionally. The fix (#644) wraps in `toFloat64` only when
 *     the bucket series is present; without buckets the quantile
 *     must legitimately resolve to nothing. The pin: when the
 *     panel's `histogram_quantile` references no `_bucket` metric
 *     root (or references one that doesn't exist), the response
 *     MUST be empty.
 *
 * Companion-series check: `assertHistogramComplete` only requires
 * `<name>_bucket`, but a well-formed histogram emits `<name>_count`
 * and `<name>_sum` alongside. The spec asserts all three exist so a
 * future regression that drops the sum/count partners (e.g. the
 * scope-attribute aggregation regressing back to bucket-only) is
 * caught at this layer rather than waiting for a separate sweep.
 *
 * The spec wires into the existing compose-smoke job (PR-blocking),
 * not nightly. Performance budget: +30s incremental over compose-
 * smoke + phase-1 baseline (one `/api/v1/series` triple-probe per
 * histogram target, dedup'd by metric root, plus the per-target
 * `query_range` fire).
 *
 * What this catches (resolved on main; this is a pin, not a hunt):
 *   - N5: P95 latency by language flat at 0 (bucket series missing
 *     from cerberus_queries_duration_seconds_bucket scope).
 *   - N6: histogram_quantile over foo_total fabricates a float
 *     (resolved by PR #644 + #642).
 *
 * Env:
 *   GRAFANA_URL       default http://localhost:3000
 *   GRAFANA_BASE_URL  honoured as a fallback for parity with
 *                     compose_grafana_smoke.spec.ts
 */

import { expect, test } from '@playwright/test';

import {
  type Dashboard,
  type DsQueryResponse,
  type Panel,
  type PanelTarget,
  assertHistogramComplete,
  assertNoFabricatedValue,
  extractDataSourceProxyURL,
  extractHistogramName,
  generateSelfTraffic,
  isHistogramQuantile,
  iterateDashboards,
  iteratePanels,
} from './helpers/index.js';

// Self-traffic warmup duration. Picked at the low end of "long enough
// to populate cerberus_queries_duration_seconds_bucket across the
// three heads" — without traffic the histogram's _bucket series is
// legitimately absent and the N5 pin can't distinguish "regressed
// scope" from "no traffic yet".
const SEED_TRAFFIC_SECONDS = 30;

// The /api/v1/query_range window. 5 minutes covers the
// SEED_TRAFFIC_SECONDS warmup plus the time it takes to fire.
const QUERY_WINDOW_SECONDS = 5 * 60;
const QUERY_STEP_SECONDS = 15;

/**
 * Prometheus `/api/v1/query_range` response shape (the subset we
 * read). Sample columns live under `data.result[].values`.
 */
type PromQueryRangeResponse = {
  status?: string;
  data?: {
    resultType?: string;
    result?: Array<{
      metric?: Record<string, string>;
      values?: Array<[number, string]>;
    }>;
  };
};

/**
 * Prometheus `/api/v1/series` response shape.
 *
 *   { status: "success", data: [ { __name__: "foo_bucket", … }, … ] }
 */
type PromSeriesResponse = {
  status?: string;
  data?: Array<Record<string, string>>;
};

/**
 * Lift a Prometheus query_range response into the DsQueryResponse
 * shape the assertion helpers consume. Each Prom result becomes one
 * frame; the result's per-sample `[t, v]` tuples become a single
 * column of timestamps under `data.values[0]`. The
 * histogram-completeness assertions only inspect column COUNT and
 * row COUNT (not value content), so the simplest faithful lift
 * suffices.
 */
function promToDsEnvelope(
  refId: string,
  prom: PromQueryRangeResponse,
): DsQueryResponse {
  const frames = (prom.data?.result ?? []).map((series) => ({
    schema: {
      fields: [
        {
          name: 'Value',
          labels: { ...(series.metric ?? {}) },
        },
      ],
    },
    data: {
      values: [(series.values ?? []).map(([t]) => t)] as unknown[][],
    },
  }));
  return { results: { [refId]: { frames } } };
}

/**
 * True iff `/api/v1/series?match[]=<name>` returned at least one
 * series. The check is intentionally permissive on the wire-class
 * (any 2xx with `status: success` and a non-empty `data` counts).
 */
async function seriesExists(
  request: import('@playwright/test').APIRequestContext,
  baseURL: string,
  proxyURL: string,
  matchExpr: string,
  start: number,
  end: number,
): Promise<{ exists: boolean; diag: string }> {
  const url = `${baseURL}${proxyURL}/api/v1/series?match[]=${encodeURIComponent(
    matchExpr,
  )}&start=${start}&end=${end}`;
  const resp = await request.get(url);
  if (resp.status() < 200 || resp.status() > 299) {
    const body = await resp.text().catch(() => '<unreadable>');
    return {
      exists: false,
      diag: `/api/v1/series → ${resp.status()}: ${body.slice(0, 300)}`,
    };
  }
  const series = (await resp.json()) as PromSeriesResponse;
  if (series.status !== 'success') {
    return {
      exists: false,
      diag: `/api/v1/series returned status=${series.status ?? '<missing>'}`,
    };
  }
  const count = (series.data ?? []).length;
  return { exists: count > 0, diag: `series count=${count}` };
}

test('histogram-completeness: every histogram_quantile panel has its _bucket / _count / _sum companions and no fabricated values', async ({
  request,
}, testInfo) => {
  // Self-traffic seed + the per-panel sweep is meaningfully heavier
  // than the smoke; give it a 5 min budget.
  testInfo.setTimeout(300_000);

  const baseURL =
    process.env.GRAFANA_URL ??
    process.env.GRAFANA_BASE_URL ??
    'http://localhost:3000';

  // Seed traffic so the cerberus dashboard's histogram panels have something
  // to render. generateSelfTraffic swallows individual request errors
  // — this is a nudge, not an assertion.
  await generateSelfTraffic(request, SEED_TRAFFIC_SECONDS);

  const dashboards = await iterateDashboards(request, baseURL);
  expect(dashboards.length, 'at least one provisioned dashboard').toBeGreaterThan(
    0,
  );

  type SweptTarget = {
    dashboardTitle: string;
    panelTitle: string;
    refId: string;
    expr: string;
    histogramName: string | null;
    proxyURL: string;
  };

  // First pass: collect every histogram_quantile target. We collect
  // up-front rather than asserting mid-iteration so we can emit a
  // clean count annotation in the test output (matches phase-1
  // shape).
  const targets: SweptTarget[] = [];
  for (const dashboard of dashboards) {
    for (const panel of iteratePanels(dashboard)) {
      for (const target of panel.targets) {
        const expr = target.expr;
        if (!expr || expr.trim() === '') continue;
        // Only Prometheus-flavoured targets carry PromQL
        // histogram_quantile. LogQL / TraceQL panels share the
        // `expr` / `query` slot but their parsers don't know the
        // call; gate explicitly so a future LogQL panel that
        // happens to mention "histogram_quantile" in a label
        // doesn't trip the wire.
        const dsType =
          target.datasource?.type ?? panel.datasource?.type ?? '';
        if (dsType !== 'prometheus') continue;
        if (!isHistogramQuantile(expr)) continue;
        const histogramName = extractHistogramName(expr);
        const proxyURL = extractDataSourceProxyURL(dashboard, panel, target);
        targets.push({
          dashboardTitle: dashboard.title,
          panelTitle: panel.title,
          refId: target.refId,
          expr,
          histogramName,
          proxyURL,
        });
      }
    }
  }

  testInfo.annotations.push({
    type: 'histogram-completeness',
    description: `swept ${targets.length} histogram_quantile target(s) across ${dashboards.length} dashboard(s)`,
  });

  expect(
    targets.length,
    'at least one histogram_quantile Prometheus panel target across all provisioned dashboards',
  ).toBeGreaterThan(0);

  const now = Math.floor(Date.now() / 1000);
  const start = now - QUERY_WINDOW_SECONDS;
  const end = now;

  // Dedupe the /api/v1/series probes by (proxyURL, metric-root) so a
  // dashboard with three quantile panels over the same histogram (P50
  // / P95 / P99) fires only one bucket-presence probe, not three.
  // The query_range probe is per-target — that's the spec-asserted
  // shape and can't be dedup'd without losing per-panel diagnostics.
  type PresenceKey = string;
  const presenceCache = new Map<
    PresenceKey,
    Promise<{ bucket: boolean; count: boolean; sum: boolean; diag: string }>
  >();
  const presenceKey = (proxyURL: string, name: string): PresenceKey =>
    `${proxyURL}::${name}`;

  const resolvePresence = (
    proxyURL: string,
    name: string,
  ): Promise<{ bucket: boolean; count: boolean; sum: boolean; diag: string }> => {
    const key = presenceKey(proxyURL, name);
    const cached = presenceCache.get(key);
    if (cached !== undefined) return cached;
    const probe = (async () => {
      const [bucket, count, sum] = await Promise.all([
        seriesExists(request, baseURL, proxyURL, `${name}_bucket`, start, end),
        seriesExists(request, baseURL, proxyURL, `${name}_count`, start, end),
        seriesExists(request, baseURL, proxyURL, `${name}_sum`, start, end),
      ]);
      return {
        bucket: bucket.exists,
        count: count.exists,
        sum: sum.exists,
        diag: `bucket: ${bucket.diag}; count: ${count.diag}; sum: ${sum.diag}`,
      };
    })();
    presenceCache.set(key, probe);
    return probe;
  };

  const failures: string[] = [];

  await Promise.all(
    targets.map(async (t) => {
      // Step 1: fire the panel's actual query against cerberus. We
      // need the response either way — the N6 branch needs to assert
      // it's empty, the N5 branch needs to assert it's non-empty.
      const queryURL = `${baseURL}${t.proxyURL}/api/v1/query_range?query=${encodeURIComponent(
        t.expr,
      )}&start=${start}&end=${end}&step=${QUERY_STEP_SECONDS}`;
      let prom: PromQueryRangeResponse;
      try {
        const resp = await request.get(queryURL);
        if (resp.status() < 200 || resp.status() > 299) {
          const body = await resp.text().catch(() => '<unreadable>');
          failures.push(
            `[${t.dashboardTitle} :: ${t.panelTitle} :: ${t.refId}] cerberus query_range → ${resp.status()}\n  url: ${queryURL}\n  body: ${body.slice(0, 600)}`,
          );
          return;
        }
        prom = (await resp.json()) as PromQueryRangeResponse;
        if (prom.status !== 'success') {
          failures.push(
            `[${t.dashboardTitle} :: ${t.panelTitle} :: ${t.refId}] cerberus query_range returned status=${prom.status ?? '<missing>'}\n  expr: ${t.expr}`,
          );
          return;
        }
      } catch (err) {
        failures.push(
          `[${t.dashboardTitle} :: ${t.panelTitle} :: ${t.refId}] query_range probe threw: ${(err as Error).message}\n  url: ${queryURL}`,
        );
        return;
      }

      const envelope = promToDsEnvelope(t.refId, prom);

      // Step 2: branch on whether the expression references a
      // `<name>_bucket` series. `extractHistogramName` returns null
      // for the N6 shape (`histogram_quantile(0.95, foo_total)` —
      // no `_bucket` suffix in the inner expression).
      if (t.histogramName === null) {
        // N6 branch: the inner expression doesn't even *reference* a
        // `_bucket` series. The response MUST be empty —
        // assertNoFabricatedValue fires if it isn't.
        try {
          assertNoFabricatedValue(envelope, t.expr);
        } catch (err) {
          failures.push(
            `[${t.dashboardTitle} :: ${t.panelTitle} :: ${t.refId}] ${(err as Error).message}\n  expr: ${t.expr}`,
          );
        }
        return;
      }

      // Step 3: the expression references a `<name>_bucket` series.
      // Probe `/api/v1/series` for the bucket + count + sum
      // companions. The bucket presence is the load-bearing pin for
      // N5; the count/sum companions catch a future regression that
      // drops the sum/count partners.
      const presence = await resolvePresence(t.proxyURL, t.histogramName);

      if (!presence.bucket) {
        // Bucket series ABSENT despite the expression naming it.
        // Two sub-cases:
        //   a) the query response is also empty → this is the N6
        //      shape on a `_bucket`-shaped name: no fabricated
        //      value, no real data either. assertNoFabricatedValue
        //      passes; we still want to surface the missing-bucket
        //      condition because the dashboard is silently broken.
        //   b) the query response is NON-empty → this is the
        //      original N6 fabricated-value class: the optimiser
        //      synthesised a float from an empty scan. Hard fail.
        try {
          assertNoFabricatedValue(envelope, t.expr);
        } catch (err) {
          failures.push(
            `[${t.dashboardTitle} :: ${t.panelTitle} :: ${t.refId}] ${(err as Error).message}\n  expr: ${t.expr}\n  series-probe: ${presence.diag}`,
          );
          return;
        }
        // Sub-case (a) — empty response, missing bucket. The panel
        // will render flat; surface as a failure so the dashboard
        // gets a real `_bucket` source or the panel gets removed.
        failures.push(
          `[${t.dashboardTitle} :: ${t.panelTitle} :: ${t.refId}] histogram_quantile references ${t.histogramName}_bucket but no such series exists in the dataset (N5 regression class)\n  expr: ${t.expr}\n  series-probe: ${presence.diag}`,
        );
        return;
      }

      // Step 4: bucket exists; assert the count + sum companions
      // also exist (the well-formed-histogram contract).
      const missingCompanions: string[] = [];
      if (!presence.count) missingCompanions.push(`${t.histogramName}_count`);
      if (!presence.sum) missingCompanions.push(`${t.histogramName}_sum`);
      if (missingCompanions.length > 0) {
        failures.push(
          `[${t.dashboardTitle} :: ${t.panelTitle} :: ${t.refId}] histogram ${t.histogramName} is missing companion series [${missingCompanions.join(', ')}]; a well-formed histogram exports _bucket + _count + _sum together\n  expr: ${t.expr}\n  series-probe: ${presence.diag}`,
        );
        // Fall through to the completeness assertion anyway —
        // multiple complaints about the same panel are more useful
        // than gating one behind the other.
      }

      // Step 5: bucket exists; the response MUST be non-empty.
      // This is the N5 pin proper — `assertHistogramComplete`
      // surfaces "buckets exist but the quantile resolved to
      // nothing visible" as a failure.
      try {
        assertHistogramComplete(envelope, t.histogramName);
      } catch (err) {
        failures.push(
          `[${t.dashboardTitle} :: ${t.panelTitle} :: ${t.refId}] ${(err as Error).message}\n  expr: ${t.expr}\n  series-probe: ${presence.diag}`,
        );
      }
    }),
  );

  if (failures.length > 0) {
    throw new Error(
      `histogram-completeness rule violated for ${failures.length} target(s):\n\n${failures.join('\n\n')}`,
    );
  }
});

// Compile-time guards. These exist so TypeScript surfaces a contract
// break in the helpers (a renamed field, a dropped type) at spec-load
// time rather than mid-test. They never execute.
const _typecheck: ((d: Dashboard, p: Panel, t: PanelTarget) => void) | undefined =
  undefined;
void _typecheck;
