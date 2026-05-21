/**
 * Phase-3 / Phase-3b filter drill-down sweep.
 *
 * For every provisioned dashboard / panel target with an aggregation
 * `by(...)` clause, fire the baseline query against cerberus via the
 * Grafana datasource-proxy URL, pick one (label, value) pair observed
 * in the response, re-fire the same query with a `<key>="<value>"`
 * matcher injected into every selector / spanset, and assert the
 * filtered result is non-empty AND has at most `baseline` series (the
 * Q3-resolved subset rule from `~/.claude/plans/e2e-enhance.md`).
 *
 * The spec dispatches per-target dsType so all three heads exercise
 * the same drill-down semantic against their own matcher grammar:
 *
 *   - `prometheus` → `addLabelFilter` (Phase 3).
 *   - `loki`       → `addLogQLLabelFilter` (Phase 3b).
 *   - `tempo`      → `addTraceQLAttributeFilter` (Phase 3b).
 *
 * What this catches (resolved on main; this is a regression pin):
 *
 *   - N3: a click on a stream-label value in Explore-Logs returns
 *     empty even though the unfiltered query exposed that label.
 *     Same root cause for the Prom-side variant: a matcher path that
 *     drops series the unfiltered path returned.
 *   - N15: drill-down chain breaks — the second-level drill returns
 *     empty (`filteredSeriesCount === 0`) on a real, observed value.
 *   - N7/N8/N16: `service_name=cerberus` invisible on a LogQL panel.
 *     Phase 3 PromQL-only drill missed this because LogQL panels were
 *     unconditionally skipped; Phase 3b's LogQL path covers it.
 *
 * Subset semantics (Q3, plan §9): **count-based, not element-wise**.
 * Element-wise strict-subset is order-dependent and flakes under
 * series re-orderings — the count comparator is the load-bearing
 * gate. `assertSubsetByCount(filtered, baseline)` enforces both
 * `filtered > 0` and `filtered ≤ baseline`.
 *
 * Scope caveats:
 *
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
 *   - The Prom query_range envelope is used for all three heads via
 *     the Grafana datasource-proxy URL; cerberus's Loki / Tempo HTTP
 *     handlers route `instant`/`range` requests through the same
 *     wire-format the proxy sends, so the per-head response parsing
 *     stays uniform.
 *
 * Env:
 *   GRAFANA_URL       default http://localhost:3000
 *   GRAFANA_BASE_URL  honoured as a fallback for parity with
 *                     compose_grafana_smoke.spec.ts
 */

import { expect, test } from '@playwright/test';

import {
  addLabelFilter,
  addLogQLLabelFilter,
  addTraceQLAttributeFilter,
  assertSubsetByCount,
  expectedByKeysForDsType,
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

type DrillCandidate = {
  dashboardTitle: string;
  panelTitle: string;
  refId: string;
  dsType: 'prometheus' | 'loki' | 'tempo';
  expr: string;
  byKeys: string[];
  proxyURL: string;
};

type DrillPair = {
  surface: string;
  dsType: 'prometheus' | 'loki' | 'tempo';
  baselineExpr: string;
  filteredExpr: string;
  key: string;
  value: string;
};

// Per-head matcher injection. Each rewriter operates on the head's
// native selector grammar — see `helpers/query-shape.ts` for the
// per-grammar behaviour and idempotence guarantees.
function injectMatcher(
  dsType: 'prometheus' | 'loki' | 'tempo',
  expr: string,
  key: string,
  value: string,
): string {
  if (dsType === 'prometheus') return addLabelFilter(expr, key, value);
  if (dsType === 'loki') return addLogQLLabelFilter(expr, key, value);
  return addTraceQLAttributeFilter(expr, key, value);
}

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
        const rawDsType =
          target.datasource?.type ?? panel.datasource?.type ?? '';
        const surface = `${dashboard.title} :: ${panel.title} :: ${target.refId}`;

        // Per-head expression slot: PromQL + LogQL targets carry the
        // expression in `target.expr`; TraceQL carries it in
        // `target.query`. Other heads fall through to the "unknown"
        // skip below.
        let expr: string | undefined;
        let dsType: 'prometheus' | 'loki' | 'tempo';
        if (rawDsType === 'prometheus') {
          expr = target.expr;
          dsType = 'prometheus';
        } else if (rawDsType === 'loki') {
          expr = target.expr;
          dsType = 'loki';
        } else if (rawDsType === 'tempo') {
          // Some Tempo panel targets (Search-mode panels) carry the
          // search filter on `target.expr` instead of `target.query`.
          // Prefer whichever slot is populated.
          expr = target.query ?? target.expr;
          dsType = 'tempo';
        } else {
          skipped.push({
            surface,
            reason: `unsupported datasource type=${rawDsType || '<unset>'}`,
          });
          continue;
        }
        if (!expr || expr.trim() === '') continue;

        // `expectedByKeysForDsType` dispatches on dsType — see the
        // helper for the per-grammar extraction rules (PromQL's
        // `histogram_quantile`-consumes-`le` subtraction lives there).
        const byKeys = expectedByKeysForDsType(expr, dsType);
        if (byKeys.length === 0) {
          // Not an aggregating panel — no drill candidate.
          continue;
        }

        // Skip the target if every byKey already has a hardcoded
        // matcher in the expression — drilling would be a no-op or
        // contradict the existing matcher, neither informative.
        // `expressionHasMatcherFor` reads brace-block contents and
        // covers both LogQL stream selectors and TraceQL spansets in
        // addition to PromQL label selectors.
        const drillableKeys = byKeys.filter(
          (k) => !expressionHasMatcherFor(expr!, k),
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
          dsType,
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
    'at least one drillable panel target across all provisioned dashboards',
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
  // Per-dsType pair counter for the test-info annotation.
  const drillPairsByDsType: Record<'prometheus' | 'loki' | 'tempo', number> = {
    prometheus: 0,
    loki: 0,
    tempo: 0,
  };
  const seenPair = new Set<string>();

  for (const cand of candidates) {
    const surface = `${cand.dashboardTitle} :: ${cand.panelTitle} :: ${cand.refId}`;
    const baselineURL = buildQueryRangeURL(
      baseURL,
      cand.proxyURL,
      cand.dsType,
      cand.expr,
      start,
      end,
    );

    // Fire baseline.
    let baselineSeriesLabels: Array<Record<string, string>>;
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
      const body = await resp.json();
      baselineSeriesLabels = parseSeriesLabels(cand.dsType, body);
    } catch (err) {
      failures.push(
        `[${surface}] baseline probe threw: ${(err as Error).message}\n  url: ${baselineURL}`,
      );
      continue;
    }

    const baselineCount = baselineSeriesLabels.length;
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
    // from baseline labels. Pick the first non-empty value — the
    // first iteration order is deterministic across runs since the
    // response is sorted by the upstream backend.
    for (const key of cand.byKeys) {
      const observedValues = new Set<string>();
      for (const labels of baselineSeriesLabels) {
        const v = labels[key];
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

      const filteredExpr = injectMatcher(cand.dsType, cand.expr, key, value);
      const dedupKey = `${cand.proxyURL}\x00${filteredExpr}`;
      if (seenPair.has(dedupKey)) continue;
      seenPair.add(dedupKey);

      drillPairs.push({
        surface: `${surface} | drill ${key}=${value}`,
        dsType: cand.dsType,
        baselineExpr: cand.expr,
        filteredExpr,
        key,
        value,
      });
      drillPairsByDsType[cand.dsType]++;

      const filteredURL = buildQueryRangeURL(
        baseURL,
        cand.proxyURL,
        cand.dsType,
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
        const filtered = await resp.json();
        const filteredCount = parseSeriesLabels(cand.dsType, filtered).length;
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
    description: `exercised ${drillPairs.length} baseline→filtered drill pair(s) (prometheus=${drillPairsByDsType.prometheus}, loki=${drillPairsByDsType.loki}, tempo=${drillPairsByDsType.tempo})`,
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

// Build the per-head range-query URL. Each upstream HTTP API has its
// own path + parameter names; cerberus mirrors them so the panel
// targets that Grafana fires go through the same path the spec uses
// here.
//
//   - Prometheus: `<proxy>/api/v1/query_range?query=…&start=…&end=…&step=…`
//   - Loki:       `<proxy>/loki/api/v1/query_range?query=…&start=…&end=…&step=…`
//                 (seconds-resolution start/end; cerberus handles the
//                 ns-resolution form too)
//   - Tempo:      `<proxy>/api/metrics/query_range?q=…&start=…&end=…&step=…s`
//                 (TraceQL `q=` rather than `query=`, step duration
//                 carries the `s` suffix per Tempo's flag-style parser)
function buildQueryRangeURL(
  baseURL: string,
  proxyURL: string,
  dsType: 'prometheus' | 'loki' | 'tempo',
  expr: string,
  start: number,
  end: number,
): string {
  const enc = encodeURIComponent(expr);
  if (dsType === 'prometheus') {
    return `${baseURL}${proxyURL}/api/v1/query_range?query=${enc}&start=${start}&end=${end}&step=${QUERY_STEP_SECONDS}`;
  }
  if (dsType === 'loki') {
    return `${baseURL}${proxyURL}/loki/api/v1/query_range?query=${enc}&start=${start}&end=${end}&step=${QUERY_STEP_SECONDS}`;
  }
  // tempo
  return `${baseURL}${proxyURL}/api/metrics/query_range?q=${enc}&start=${start}&end=${end}&step=${QUERY_STEP_SECONDS}s`;
}

// Parse the per-head range-query response body into a uniform
// "series labels" array. Each entry is the label map for one series;
// the drill semantic only cares about the labels (to pick a value to
// filter on) and the count (for subset assertion).
//
//   - Prometheus + Loki share the `{status, data: {result: [{metric}]}}`
//     envelope; both expose labels under `result[].metric`.
//   - Tempo's `/api/metrics/query_range` returns `{series: [{labels:
//     [{key, value: {stringValue}}], samples}]}` — labels are an array
//     of KeyValue/AnyValue entries, not a map.
//
// Returns `[]` on any parse mismatch — the caller treats that as
// "empty baseline" and annotates rather than failing, matching the
// existing Phase-3 behaviour for the Prom-only path.
function parseSeriesLabels(
  dsType: 'prometheus' | 'loki' | 'tempo',
  body: unknown,
): Array<Record<string, string>> {
  if (body === null || typeof body !== 'object') return [];
  if (dsType === 'prometheus' || dsType === 'loki') {
    const env = body as {
      status?: string;
      data?: { result?: Array<{ metric?: Record<string, string> }> };
    };
    if (env.status !== 'success') return [];
    return (env.data?.result ?? []).map((r) => r.metric ?? {});
  }
  // tempo
  const env = body as {
    series?: Array<{
      labels?: Array<{
        key?: string;
        value?: string | { stringValue?: string };
      }>;
    }>;
  };
  return (env.series ?? []).map((s) => {
    const out: Record<string, string> = {};
    for (const l of s.labels ?? []) {
      if (typeof l.key !== 'string' || l.key === '') continue;
      // Tempo serialises AnyValue; the typical shape carries a
      // `stringValue` child, but the legacy flat-string form is also
      // accepted by cerberus's UnmarshalJSON path (see metrics_query_range.go).
      const v = l.value;
      if (typeof v === 'string') {
        out[l.key] = v;
      } else if (v && typeof v === 'object' && typeof v.stringValue === 'string') {
        out[l.key] = v.stringValue;
      }
    }
    return out;
  });
}
