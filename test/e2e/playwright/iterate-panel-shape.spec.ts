/**
 * Phase-1 panel-shape sweep.
 *
 * Iterates every provisioned dashboard, every panel, every target,
 * and — for any PromQL expression that carries a `by(...)` or
 * `without(...)` aggregation modifier — fires the panel's query
 * against the cerberus Prometheus endpoint via Grafana's
 * datasource-proxy URL (`/api/datasources/proxy/uid/<uid>/api/v1/...`)
 * and runs the label-shape rule against the response:
 *
 *   - `sum by (k1, k2) (...)` — every `kN` MUST appear on at least one
 *     returned series's label map. This is the load-bearing pin for
 *     sweep findings N2/N11/N14 (a `sum by (cerberus_ql)` panel
 *     silently collapsing to a single anonymous "Value" frame).
 *   - `sum without (kN) (...)` — none of the `kN` keys may appear on
 *     any returned series. The inverse semantic to `by`; an earlier
 *     draft of `extractByKeys` conflated the two and would have
 *     inverted this assertion.
 *
 * The Prometheus `/api/v1/query_range` envelope carries each series's
 * labels under `data.result[].metric` (a plain string→string map).
 * The spec lifts those maps into the DsQueryResponse-shaped envelope
 * the assertion helpers already know how to read, so the assertion
 * library stays single-shape.
 *
 * The spec wires into the existing compose-smoke job (PR-blocking),
 * not nightly. The performance budget is ~30s incremental over the
 * compose-smoke baseline.
 *
 * What this catches (resolved on main; this is a pin, not a hunt):
 *   - N2: `sum by (cerberus_ql)` panels collapsing to an anonymous
 *     bucket (the `normalizeDottedSelectors` parser-shape regression).
 *   - N11/N14: same shape exhibited by different panels (severity-
 *     partition, error-rate by language). The earlier ad-hoc
 *     `driveCerberusQLPartition` / `driveSeverityPartition` helpers in
 *     compose_grafana_smoke.spec.ts only checked one panel each; the
 *     generic sweep here covers every aggregating panel across every
 *     provisioned dashboard.
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
  assertLabelAbsent,
  assertLabelShape,
  expectedByKeys,
  extractDataSourceProxyURL,
  extractWithoutKeys,
  generateSelfTraffic,
  iterateDashboards,
  iteratePanels,
} from './helpers/index.js';

// Self-traffic warmup duration. Picked at the low end of "long enough
// to populate cerberus_queries_total bucketed by language" so the
// label-shape assertion doesn't false-positive on "no traffic yet".
const SEED_TRAFFIC_SECONDS = 30;

// The /api/v1/query_range window. 5 minutes covers the
// SEED_TRAFFIC_SECONDS warmup plus the few seconds it takes to fire.
// Anything shorter and a Prom-style range query like
// `rate(cerberus_queries_total[5m])` can legitimately resolve to an
// empty series — the assertion needs a populated window.
const QUERY_WINDOW_SECONDS = 5 * 60;
const QUERY_STEP_SECONDS = 15;

/**
 * Prometheus `/api/v1/query_range` response shape (the subset we
 * read). Labels live under `data.result[].metric`.
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
 * Lift a Prometheus query_range response into the DsQueryResponse
 * shape the assertion helpers consume. Each Prom result becomes one
 * frame; the result's `metric` map becomes a single field's `labels`
 * (the assertion helpers walk every field's labels keyset, so we
 * don't need to fan out one field per metric key).
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

test('panel-shape: every aggregating panel surfaces its by(...) and respects its without(...)', async ({
  request,
}, testInfo) => {
  // Self-traffic seed + the per-panel sweep is meaningfully heavier
  // than the smoke; give it a 5 min budget.
  testInfo.setTimeout(300_000);

  const baseURL =
    process.env.GRAFANA_URL ??
    process.env.GRAFANA_BASE_URL ??
    'http://localhost:3000';

  // Seed traffic so cerberus-self panels have something to render.
  // generateSelfTraffic swallows individual request errors — this is
  // a nudge, not an assertion.
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
    byKeys: string[];
    withoutKeys: string[];
    proxyURL: string;
  };

  // First pass: collect everything we want to assert. Doing the
  // collection up-front (rather than asserting inside the iteration)
  // keeps the per-panel work bounded and lets us emit a clean
  // diagnostic count in the test output.
  const targets: SweptTarget[] = [];
  for (const dashboard of dashboards) {
    for (const panel of iteratePanels(dashboard)) {
      for (const target of panel.targets) {
        const expr = target.expr;
        if (!expr || expr.trim() === '') continue;
        // Only Prometheus-flavoured targets are in scope for this rule.
        // LogQL / TraceQL use the same `expr` field but their parsers
        // don't share PromQL's `by`/`without` keywords; the regex
        // happens to misfire on neither, but we gate explicitly so a
        // future LogQL panel that legitimately uses the word `by` in
        // a label-filter doesn't trip the wire.
        const dsType =
          target.datasource?.type ?? panel.datasource?.type ?? '';
        if (dsType !== 'prometheus') continue;
        // `expectedByKeys` is the semantic extractor — for any
        // top-level call that consumes inner aggregation labels
        // (currently only `histogram_quantile`, which consumes `le`)
        // it subtracts those labels because they are gone from the
        // response series. Using the raw `extractByKeys` here would
        // surface a mathematically-impossible assertion for every
        // `histogram_quantile(... by (le, …) ...)` panel.
        const byKeys = expectedByKeys(expr);
        const withoutKeys = extractWithoutKeys(expr);
        if (byKeys.length === 0 && withoutKeys.length === 0) continue;
        const proxyURL = extractDataSourceProxyURL(dashboard, panel, target);
        targets.push({
          dashboardTitle: dashboard.title,
          panelTitle: panel.title,
          refId: target.refId,
          expr,
          byKeys,
          withoutKeys,
          proxyURL,
        });
      }
    }
  }

  testInfo.annotations.push({
    type: 'panel-shape',
    description: `swept ${targets.length} aggregating target(s) across ${dashboards.length} dashboard(s)`,
  });

  expect(
    targets.length,
    'at least one aggregating Prometheus panel target across all provisioned dashboards',
  ).toBeGreaterThan(0);

  // Fire each panel's query against cerberus via the datasource-proxy
  // URL in parallel. Parallelism is bounded only by Playwright's
  // APIRequestContext defaults; the spec runs against a local compose
  // stack so the cap is fine for the ~5-20 target count we expect.
  const now = Math.floor(Date.now() / 1000);
  const start = now - QUERY_WINDOW_SECONDS;
  const end = now;

  const failures: string[] = [];

  await Promise.all(
    targets.map(async (t) => {
      const queryURL = `${baseURL}${t.proxyURL}/api/v1/query_range?query=${encodeURIComponent(
        t.expr,
      )}&start=${start}&end=${end}&step=${QUERY_STEP_SECONDS}`;
      try {
        const resp = await request.get(queryURL);

        if (resp.status() < 200 || resp.status() > 299) {
          const body = await resp.text().catch(() => '<unreadable>');
          failures.push(
            `[${t.dashboardTitle} :: ${t.panelTitle} :: ${t.refId}] cerberus query_range → ${resp.status()}\n  url: ${queryURL}\n  body: ${body.slice(0, 600)}`,
          );
          return;
        }

        const prom = (await resp.json()) as PromQueryRangeResponse;
        if (prom.status !== 'success') {
          failures.push(
            `[${t.dashboardTitle} :: ${t.panelTitle} :: ${t.refId}] cerberus query_range returned status=${prom.status ?? '<missing>'}\n  expr: ${t.expr}`,
          );
          return;
        }

        const envelope = promToDsEnvelope(t.refId, prom);

        // N2/N11/N14 — the regressions this spec pins — are
        // shaped as "response carries frames, but the by-clause
        // keys are missing from those frames' labels" (a `sum by
        // (cerberus_ql)` panel collapsing to a single anonymous
        // "Value" frame). A truly empty response — zero frames —
        // is the N5-class shape and is out of scope here: the
        // compose stack ships placeholder panels backed by
        // metrics that don't exist yet (the cerberus-self
        // dashboard's In-flight / Admission-rejections panels are
        // labelled "declarative until the admission middleware
        // exports it" in their description text) and has a
        // 30-second self-traffic seed window that not every
        // panel's source data fully populates by query time.
        // Gating the label-shape assertion on at least one
        // returned frame keeps the load-bearing pin sharp without
        // false-positiving on data-sparsity.
        const frameCount = (prom.data?.result ?? []).length;
        if (frameCount === 0) {
          testInfo.annotations.push({
            type: 'panel-shape-empty',
            description: `[${t.dashboardTitle} :: ${t.panelTitle} :: ${t.refId}] no series returned for expr: ${t.expr} — label-shape rule excluded (N2/N11/N14 only applies to populated frames)`,
          });
        } else if (t.byKeys.length > 0) {
          try {
            assertLabelShape(envelope, t.byKeys);
          } catch (err) {
            failures.push(
              `[${t.dashboardTitle} :: ${t.panelTitle} :: ${t.refId}] ${(err as Error).message}\n  expr: ${t.expr}`,
            );
          }
        }
        if (frameCount > 0 && t.withoutKeys.length > 0) {
          try {
            assertLabelAbsent(envelope, t.withoutKeys);
          } catch (err) {
            failures.push(
              `[${t.dashboardTitle} :: ${t.panelTitle} :: ${t.refId}] ${(err as Error).message}\n  expr: ${t.expr}`,
            );
          }
        }
      } catch (err) {
        failures.push(
          `[${t.dashboardTitle} :: ${t.panelTitle} :: ${t.refId}] panel-shape probe threw: ${(err as Error).message}\n  url: ${queryURL}`,
        );
      }
    }),
  );

  if (failures.length > 0) {
    throw new Error(
      `panel-shape rule violated for ${failures.length} target(s):\n\n${failures.join('\n\n')}`,
    );
  }
});

// Compile-time guards. These exist so TypeScript surfaces a contract
// break in the helpers (a renamed field, a dropped type) at spec-load
// time rather than mid-test. They never execute.
const _typecheck: ((d: Dashboard, p: Panel, t: PanelTarget) => void) | undefined =
  undefined;
void _typecheck;
