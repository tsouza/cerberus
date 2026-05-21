/**
 * Phase-3 filter drill-down sweep.
 *
 * For every provisioned dashboard / panel / PromQL target with a
 * grouping `by(...)` clause, fire the baseline query against cerberus
 * via the Grafana datasource-proxy URL, pick one (label, value) pair
 * observed in the response, re-fire the same query with a
 * `{<label>="<value>"}` matcher tacked onto every selector, and assert
 * the filtered result is non-empty AND has at most `baseline` series
 * (the Q3-resolved subset rule from `~/.claude/plans/e2e-enhance.md`).
 *
 * What this catches (resolved on main; this is a regression pin):
 *
 *   - N3: a click on a stream-label value in Explore-Logs returns
 *     empty even though the unfiltered query exposed that label.
 *     Same root cause for the Prom-side variant: a matcher path that
 *     drops series the unfiltered path returned.
 *   - N15: drill-down chain breaks — the second-level drill returns
 *     empty (`filteredSeriesCount === 0`) on a real, observed value.
 *
 * Subset semantics (Q3, plan §9): **count-based, not element-wise**.
 * Element-wise strict-subset is order-dependent and flakes under
 * series re-orderings — the count comparator is the load-bearing
 * gate. `assertSubsetByCount(filtered, baseline)` enforces both
 * `filtered > 0` and `filtered ≤ baseline`.
 *
 * Scope caveats (from the task brief):
 *
 *   - PromQL only. LogQL / TraceQL filter-drill is filed as a sibling
 *     task; the `addLabelFilter` helper is PromQL-specific. Loki /
 *     Tempo targets are skipped in this phase via the `dsType` check.
 *   - Targets whose expression already constrains the drill label
 *     via a hardcoded matcher (e.g.
 *     `cerberus_queries_total{cerberus_ql="promql"}`) are skipped —
 *     drilling there is either a no-op or contradicts the existing
 *     matcher, neither of which is informative.
 *   - `histogram_quantile(...)` panels: the drill keys come from the
 *     *output* labels of the quantile (i.e. `expectedByKeys`, which
 *     subtracts the `le` bucket-boundary label consumed by the
 *     top-level quantile), not the inner `le`. Drilling on `le`
 *     would produce a single-bucket scan whose subset comparison is
 *     meaningless.
 *
 * Env:
 *   GRAFANA_URL       default http://localhost:3000
 *   GRAFANA_BASE_URL  honoured as a fallback for parity with
 *                     compose_grafana_smoke.spec.ts
 */

import { expect, test } from '@playwright/test';

import {
  addLabelFilter,
  assertSubsetByCount,
  expectedByKeys,
  expressionHasMatcherFor,
  extractDataSourceProxyURL,
  generateSelfTraffic,
  iterateDashboards,
  iteratePanels,
} from './helpers/index.js';

// Self-traffic warmup. Same envelope as the panel-shape spec — the
// drill is meaningless against an empty baseline, so we seed first.
const SEED_TRAFFIC_SECONDS = 30;

// /api/v1/query_range window. Mirrors iterate-panel-shape.spec.ts so
// the per-spec timing assumptions stay aligned (panel-shape's
// rate(... [5m]) expressions need ≥ 5 min of populated data).
const QUERY_WINDOW_SECONDS = 5 * 60;
const QUERY_STEP_SECONDS = 15;

/**
 * Subset of the Prometheus /api/v1/query_range envelope we read.
 * The drill-down spec only needs the per-series `metric` map and a
 * top-level `status` field; sample values are irrelevant here.
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

type DrillCandidate = {
  dashboardTitle: string;
  panelTitle: string;
  refId: string;
  expr: string;
  byKeys: string[];
  proxyURL: string;
};

type DrillPair = {
  surface: string;
  baselineExpr: string;
  filteredExpr: string;
  key: string;
  value: string;
};

test('filter-drill: every aggregating panel produces a non-empty subset when filtered on a real observed label value', async ({
  request,
}, testInfo) => {
  // Drill = baseline query + N filtered queries per panel. Give the
  // spec a 5-min budget; the panel-shape spec uses the same envelope.
  testInfo.setTimeout(300_000);

  const baseURL =
    process.env.GRAFANA_URL ??
    process.env.GRAFANA_BASE_URL ??
    'http://localhost:3000';

  // Seed self-traffic so the by-language / by-severity panels have
  // data to drill on. The seed helper swallows individual request
  // errors — this is a nudge, not an assertion.
  await generateSelfTraffic(request, SEED_TRAFFIC_SECONDS);

  const dashboards = await iterateDashboards(request, baseURL);
  expect(
    dashboards.length,
    'at least one provisioned dashboard',
  ).toBeGreaterThan(0);

  // First pass: collect every Prom target with at least one by(...)
  // key that doesn't already have a hardcoded matcher for that key.
  const candidates: DrillCandidate[] = [];
  const skipped: Array<{ surface: string; reason: string }> = [];

  for (const dashboard of dashboards) {
    for (const panel of iteratePanels(dashboard)) {
      for (const target of panel.targets) {
        const expr = target.expr;
        if (!expr || expr.trim() === '') continue;

        const dsType =
          target.datasource?.type ?? panel.datasource?.type ?? '';
        const surface = `${dashboard.title} :: ${panel.title} :: ${target.refId}`;

        // LogQL / TraceQL — out of scope per the brief. Filed for a
        // sibling task; the `addLabelFilter` helper is PromQL-only.
        if (dsType !== 'prometheus') {
          skipped.push({
            surface,
            reason: `non-prometheus datasource (type=${dsType || '<unset>'})`,
          });
          continue;
        }

        // `expectedByKeys` subtracts labels the top-level call
        // consumes (currently only `le` for `histogram_quantile`);
        // drilling on `le` would scan a single histogram bucket
        // which is not what the drill semantic targets.
        const byKeys = expectedByKeys(expr);
        if (byKeys.length === 0) {
          // Not an aggregating panel — no drill candidate.
          continue;
        }

        // Skip the target if every byKey already has a hardcoded
        // matcher in the expression — drilling would be a no-op or
        // contradict the existing matcher, neither informative.
        const drillableKeys = byKeys.filter(
          (k) => !expressionHasMatcherFor(expr, k),
        );
        if (drillableKeys.length === 0) {
          skipped.push({
            surface,
            reason: `every by-key already has a hardcoded matcher (byKeys=[${byKeys.join(
              ', ',
            )}])`,
          });
          continue;
        }

        const proxyURL = extractDataSourceProxyURL(dashboard, panel, target);
        candidates.push({
          dashboardTitle: dashboard.title,
          panelTitle: panel.title,
          refId: target.refId,
          expr,
          byKeys: drillableKeys,
          proxyURL,
        });
      }
    }
  }

  testInfo.annotations.push({
    type: 'filter-drill-candidates',
    description: `collected ${candidates.length} drill candidate(s) across ${dashboards.length} dashboard(s); skipped ${skipped.length} target(s)`,
  });
  for (const s of skipped) {
    testInfo.annotations.push({
      type: 'filter-drill-skip',
      description: `[${s.surface}] ${s.reason}`,
    });
  }

  expect(
    candidates.length,
    'at least one drillable Prometheus panel target across all provisioned dashboards',
  ).toBeGreaterThan(0);

  // Second pass: fire baselines, extract observed (key, value)
  // pairs, then fire one filtered query per pair. We dedupe by
  // (proxyURL, expr, key, value) so the same drill isn't fired twice
  // across panels that share an expression.
  const now = Math.floor(Date.now() / 1000);
  const start = now - QUERY_WINDOW_SECONDS;
  const end = now;

  const failures: string[] = [];
  const drillPairs: DrillPair[] = [];
  const seenPair = new Set<string>();

  for (const cand of candidates) {
    const surface = `${cand.dashboardTitle} :: ${cand.panelTitle} :: ${cand.refId}`;
    const baselineURL = buildQueryRangeURL(
      baseURL,
      cand.proxyURL,
      cand.expr,
      start,
      end,
    );

    // Fire baseline.
    let baseline: PromQueryRangeResponse;
    try {
      const resp = await request.get(baselineURL);
      if (resp.status() < 200 || resp.status() > 299) {
        const body = await resp.text().catch(() => '<unreadable>');
        failures.push(
          `[${surface}] baseline query → ${resp.status()}\n  url: ${baselineURL}\n  body: ${body.slice(
            0,
            600,
          )}`,
        );
        continue;
      }
      baseline = (await resp.json()) as PromQueryRangeResponse;
    } catch (err) {
      failures.push(
        `[${surface}] baseline probe threw: ${(err as Error).message}\n  url: ${baselineURL}`,
      );
      continue;
    }

    if (baseline.status !== 'success') {
      failures.push(
        `[${surface}] baseline returned status=${baseline.status ?? '<missing>'}\n  expr: ${cand.expr}`,
      );
      continue;
    }

    const baselineSeries = baseline.data?.result ?? [];
    const baselineCount = baselineSeries.length;
    if (baselineCount === 0) {
      // No baseline data — nothing to drill on. The panel-shape spec
      // already annotates empty panels; this isn't a filter-drill
      // failure (the drill is only well-defined when there's data).
      testInfo.annotations.push({
        type: 'filter-drill-empty-baseline',
        description: `[${surface}] baseline returned 0 series — drill skipped (no observed values to pick from)`,
      });
      continue;
    }

    // For each drillable byKey, collect the set of observed values
    // from baseline.data.result[].metric[key]. Pick the first
    // non-empty value — the first iteration order is deterministic
    // across runs since the response is sorted by Prometheus.
    for (const key of cand.byKeys) {
      const observedValues = new Set<string>();
      for (const series of baselineSeries) {
        const v = series.metric?.[key];
        if (typeof v === 'string' && v !== '') observedValues.add(v);
      }
      if (observedValues.size === 0) {
        // The by-key wasn't actually populated on any frame — the
        // panel-shape rule (#678) catches this shape under
        // `assertLabelShape`; we don't double-report here.
        testInfo.annotations.push({
          type: 'filter-drill-empty-key',
          description: `[${surface}] by-key "${key}" had no observed values across ${baselineCount} baseline frame(s) — drill not applicable`,
        });
        continue;
      }

      // Pick the first observed value deterministically (sorted).
      const value = [...observedValues].sort()[0]!;

      const filteredExpr = addLabelFilter(cand.expr, key, value);
      const dedupKey = `${cand.proxyURL}\x00${filteredExpr}`;
      if (seenPair.has(dedupKey)) continue;
      seenPair.add(dedupKey);

      drillPairs.push({
        surface: `${surface} | drill ${key}=${value}`,
        baselineExpr: cand.expr,
        filteredExpr,
        key,
        value,
      });

      const filteredURL = buildQueryRangeURL(
        baseURL,
        cand.proxyURL,
        filteredExpr,
        start,
        end,
      );

      try {
        const resp = await request.get(filteredURL);
        if (resp.status() < 200 || resp.status() > 299) {
          const body = await resp.text().catch(() => '<unreadable>');
          failures.push(
            `[${surface}] filtered query (${key}="${value}") → ${resp.status()}\n  url: ${filteredURL}\n  body: ${body.slice(
              0,
              600,
            )}`,
          );
          continue;
        }
        const filtered = (await resp.json()) as PromQueryRangeResponse;
        if (filtered.status !== 'success') {
          failures.push(
            `[${surface}] filtered query (${key}="${value}") status=${filtered.status ?? '<missing>'}\n  expr: ${filteredExpr}`,
          );
          continue;
        }
        const filteredCount = (filtered.data?.result ?? []).length;
        try {
          assertSubsetByCount(
            filteredCount,
            baselineCount,
            `${surface} | drill ${key}="${value}" | baselineExpr: ${cand.expr} | filteredExpr: ${filteredExpr}`,
          );
        } catch (err) {
          failures.push(`[${surface}] ${(err as Error).message}`);
        }
      } catch (err) {
        failures.push(
          `[${surface}] filtered probe threw: ${(err as Error).message}\n  url: ${filteredURL}`,
        );
      }
    }
  }

  testInfo.annotations.push({
    type: 'filter-drill-pairs',
    description: `exercised ${drillPairs.length} baseline→filtered drill pair(s)`,
  });

  expect(
    drillPairs.length,
    'at least one (baseline → filtered) drill pair exercised across all candidates',
  ).toBeGreaterThan(0);

  if (failures.length > 0) {
    throw new Error(
      `filter-drill rule violated for ${failures.length} drill(s):\n\n${failures.join('\n\n')}`,
    );
  }
});

function buildQueryRangeURL(
  baseURL: string,
  proxyURL: string,
  expr: string,
  start: number,
  end: number,
): string {
  return `${baseURL}${proxyURL}/api/v1/query_range?query=${encodeURIComponent(
    expr,
  )}&start=${start}&end=${end}&step=${QUERY_STEP_SECONDS}`;
}
