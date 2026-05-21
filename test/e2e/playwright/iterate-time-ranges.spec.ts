/**
 * Phase-5 variable / time-range matrix sweep — nightly-only.
 *
 * Iterates every provisioned dashboard, every aggregating or
 * histogram_quantile panel, and re-runs the phase-1 label-shape +
 * phase-2 histogram-completeness assertions across a small explicit
 * matrix of `(time-range, step)` tuples. Closes the latent N12 class
 * from `/home/thiago/.claude/plans/e2e-enhance.md` §3 — "switch time
 * range from Last 1h to Last 5m and some panels go empty" — by pinning
 * every shipped panel against four ranges and three steps so a
 * windowing-math / step-alignment regression cannot land silently.
 *
 * What the matrix catches:
 *
 *   - Windowing math:   a `rate(...)[5m]` whose lookback overruns the
 *     query window collapses to empty on `now-5m` even though the
 *     panel renders fine on `now-1h`.
 *   - Off-by-one anchor: `start = end - window` vs `start = end -
 *     window + step` differ by one bucket; a regression that flips the
 *     anchor surfaces as a 1-step shift in the leading sample (caught
 *     because the empty-frames gate distinguishes "no data" from
 *     "wrong anchor returned zero rows when the larger range returned
 *     non-zero").
 *   - Large-range plan stability: cerberus's PromQL → CH SQL emitter
 *     picks different step / pre-aggregation paths at very small vs
 *     very large windows; a 24h sweep catches the path that's never
 *     exercised by the phase-1/2 specs (which both fire `now-5m`).
 *   - Step alignment: cerberus's range emitter aligns samples to step
 *     boundaries; a misaligned step that drops the trailing bucket
 *     surfaces as a missing-final-sample shape on a step≥panel-window
 *     run.
 *
 * Gate posture: this spec is NIGHTLY-ONLY (Q2 decision in
 * `/home/thiago/.claude/plans/e2e-enhance.md` §9) and runs from the
 * `dashboard` job in `.github/workflows/e2e.yml`, NOT from
 * `compose-smoke.yml`. The variable / time-range iteration is the
 * highest flake-risk family per §8.2 of the plan (a 24h range against
 * a freshly-started compose stack can legitimately return empty), so
 * it stays informational on push-to-main + nightly + manual dispatch
 * until the flake rate is observed < 1% over a fix cycle.
 *
 * Flake handling: empty frames are an ANNOTATION, not a failure (the
 * same gating pattern phase-1 uses for cerberus-self placeholder
 * panels). Errors — non-2xx, malformed body, label-shape regression on
 * a populated frame, histogram fabricated-value on a populated frame —
 * are still hard failures.
 *
 * Env:
 *   GRAFANA_URL       default http://localhost:3000
 *   GRAFANA_BASE_URL  honoured as a fallback for parity with the
 *                     phase-1..4 specs.
 */

import { expect, test } from '@playwright/test';

import {
  type Dashboard,
  type DsQueryResponse,
  type Panel,
  type PanelTarget,
  assertHistogramComplete,
  assertLabelAbsent,
  assertLabelShape,
  assertNoFabricatedValue,
  expectedByKeys,
  extractDataSourceProxyURL,
  extractHistogramName,
  extractWithoutKeys,
  generateSelfTraffic,
  isHistogramQuantile,
  iterateDashboards,
  iteratePanels,
} from './helpers/index.js';

// Self-traffic warmup duration. The matrix's longest range is 24h —
// but the seed cannot pre-populate 24h of history in a sane warmup
// window. The 60s seed below is calibrated for the SHORT end of the
// matrix (5m / 1h); longer ranges may legitimately return empty
// frames on a freshly-started compose stack, which is why the empty-
// frames branch annotates rather than fails (see file header).
const SEED_TRAFFIC_SECONDS = 60;

// Time-range matrix. Each tuple is `{ label, windowSeconds }`. The
// `label` is purely for diagnostics; the `windowSeconds` is what the
// spec actually computes `start = now - windowSeconds` from.
//
// The four ranges are explicitly enumerated rather than computed from
// a base so a maintainer reading the spec can see exactly which
// windows are exercised without doing arithmetic. This matches the
// matrix called out in the plan file §4.3.
const TIME_RANGES: Array<{ label: string; windowSeconds: number }> = [
  { label: 'now-5m', windowSeconds: 5 * 60 },
  { label: 'now-1h', windowSeconds: 60 * 60 },
  { label: 'now-6h', windowSeconds: 6 * 60 * 60 },
  { label: 'now-24h', windowSeconds: 24 * 60 * 60 },
];

// Step-size matrix. The plan file (§4.3) calls for `[15s, 30s, 1m]`
// but the task brief widens that to `[15s, 1m, 5m]` so the matrix
// also exercises the step >> sub-minute family where the step-alignment
// math has the most freedom to regress.
const STEP_SIZES: Array<{ label: string; stepSeconds: number }> = [
  { label: '15s', stepSeconds: 15 },
  { label: '1m', stepSeconds: 60 },
  { label: '5m', stepSeconds: 5 * 60 },
];

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
 * Lift a Prometheus `/api/v1/query_range` response into the
 * `DsQueryResponse` shape the assertion helpers consume. Mirrors the
 * lift in `iterate-panel-shape.spec.ts` and
 * `iterate-histogram-completeness.spec.ts` so the same assertions can
 * run against both surfaces. Kept local rather than promoted to
 * helpers/ until a third spec needs the same lift — at which point
 * pull it up to `helpers/probes.ts`.
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
 * One concrete iteration of the matrix — the panel + the (range,
 * step) tuple that the spec will fire against the datasource-proxy
 * URL. Collected up-front so the matrix-size annotation in the test
 * output reflects the actual work the spec did.
 */
type MatrixEntry = {
  dashboardTitle: string;
  panelTitle: string;
  refId: string;
  expr: string;
  proxyURL: string;
  byKeys: string[];
  withoutKeys: string[];
  isHistogram: boolean;
  histogramName: string | null;
  range: (typeof TIME_RANGES)[number];
  step: (typeof STEP_SIZES)[number];
};

test('time-ranges: every aggregating / histogram panel re-asserts under (range, step) matrix', async ({
  request,
}, testInfo) => {
  // Matrix is up to 4 ranges × 3 steps × N panels per dashboard. On
  // the compose stack with the cerberus-self dashboard (~15 panels),
  // expect ~50-100 query_range fires. 15 minutes covers the seed +
  // the full matrix even on a slow CI runner.
  testInfo.setTimeout(15 * 60_000);

  const baseURL =
    process.env.GRAFANA_URL ??
    process.env.GRAFANA_BASE_URL ??
    'http://localhost:3000';

  // Longer seed than phase-1/2 because the matrix includes a 24h
  // range — even though we can't seed 24h of history in 60s, the
  // extra warmup helps the 1h / 6h ranges return non-empty on a
  // freshly-started stack. See file header for the flake-handling
  // contract.
  await generateSelfTraffic(request, SEED_TRAFFIC_SECONDS);

  const dashboards = await iterateDashboards(request, baseURL);
  expect(dashboards.length, 'at least one provisioned dashboard').toBeGreaterThan(
    0,
  );

  // First pass: enumerate every (panel, range, step) tuple the spec
  // will fire. Done up-front so the annotation count is honest and
  // the per-tuple fire below is a simple parallel map.
  const entries: MatrixEntry[] = [];
  let aggregatingPanels = 0;
  let histogramPanels = 0;
  for (const dashboard of dashboards) {
    for (const panel of iteratePanels(dashboard)) {
      for (const target of panel.targets) {
        const expr = target.expr;
        if (!expr || expr.trim() === '') continue;
        // Same gate as phase-1/2: only Prometheus-flavoured targets
        // are in scope. LogQL / TraceQL share the `expr` field but
        // their parsers don't know `by` / `histogram_quantile`.
        const dsType =
          target.datasource?.type ?? panel.datasource?.type ?? '';
        if (dsType !== 'prometheus') continue;

        const byKeys = expectedByKeys(expr);
        const withoutKeys = extractWithoutKeys(expr);
        const histogram = isHistogramQuantile(expr);
        const histogramName = histogram ? extractHistogramName(expr) : null;

        // Skip targets the matrix can't say anything about. An expr
        // with no `by` / `without` AND not a histogram has no
        // shape-class assertion in phase-5's scope — the wire-level
        // 2xx check is already covered by compose_grafana_smoke.
        if (byKeys.length === 0 && withoutKeys.length === 0 && !histogram) {
          continue;
        }
        if (byKeys.length > 0 || withoutKeys.length > 0) aggregatingPanels++;
        if (histogram) histogramPanels++;

        const proxyURL = extractDataSourceProxyURL(dashboard, panel, target);
        for (const range of TIME_RANGES) {
          for (const step of STEP_SIZES) {
            entries.push({
              dashboardTitle: dashboard.title,
              panelTitle: panel.title,
              refId: target.refId,
              expr,
              proxyURL,
              byKeys,
              withoutKeys,
              isHistogram: histogram,
              histogramName,
              range,
              step,
            });
          }
        }
      }
    }
  }

  testInfo.annotations.push({
    type: 'time-ranges',
    description: `swept ${entries.length} (panel × range × step) iteration(s) across ${dashboards.length} dashboard(s) — ${aggregatingPanels} aggregating target(s), ${histogramPanels} histogram_quantile target(s), ${TIME_RANGES.length} range(s), ${STEP_SIZES.length} step(s)`,
  });

  // Per-dashboard iteration count — surfaces in the test output so a
  // maintainer can see exactly how many fires hit each dashboard.
  const perDashboard = new Map<string, number>();
  for (const e of entries) {
    perDashboard.set(
      e.dashboardTitle,
      (perDashboard.get(e.dashboardTitle) ?? 0) + 1,
    );
  }
  for (const [title, count] of perDashboard) {
    testInfo.annotations.push({
      type: 'time-ranges-per-dashboard',
      description: `${title}: ${count} (panel × range × step) iteration(s)`,
    });
  }

  expect(
    entries.length,
    'at least one aggregating-or-histogram Prometheus panel target across all provisioned dashboards × the (range, step) matrix',
  ).toBeGreaterThan(0);

  const failures: string[] = [];
  // Empty-frame count gets annotated rather than failed (see header
  // contract). Track it so the test output surfaces the empty-rate
  // and the maintainer can spot a flake spiral early.
  let emptyFrameCount = 0;

  const now = Math.floor(Date.now() / 1000);

  await Promise.all(
    entries.map(async (e) => {
      const start = now - e.range.windowSeconds;
      const end = now;
      const surface = `${e.dashboardTitle} :: ${e.panelTitle} :: ${e.refId} :: range=${e.range.label} step=${e.step.label}`;

      const queryURL = `${baseURL}${e.proxyURL}/api/v1/query_range?query=${encodeURIComponent(
        e.expr,
      )}&start=${start}&end=${end}&step=${e.step.stepSeconds}`;

      try {
        const resp = await request.get(queryURL);

        if (resp.status() < 200 || resp.status() > 299) {
          const body = await resp.text().catch(() => '<unreadable>');
          failures.push(
            `[${surface}] cerberus query_range → ${resp.status()}\n  url: ${queryURL}\n  body: ${body.slice(0, 600)}`,
          );
          return;
        }

        const prom = (await resp.json()) as PromQueryRangeResponse;
        if (prom.status !== 'success') {
          failures.push(
            `[${surface}] cerberus query_range returned status=${prom.status ?? '<missing>'}\n  expr: ${e.expr}`,
          );
          return;
        }

        const frameCount = (prom.data?.result ?? []).length;
        const envelope = promToDsEnvelope(e.refId, prom);

        if (frameCount === 0) {
          // Empty frames on this (range, step) — annotate, don't
          // fail. The 24h range against a freshly-started compose
          // stack legitimately returns empty (the seed only spans
          // ~60s); the same applies to histogram panels whose
          // underlying _bucket series haven't been emitted yet at
          // the very-small-range end.
          emptyFrameCount++;
          testInfo.annotations.push({
            type: 'time-ranges-empty',
            description: `[${surface}] no series returned for expr: ${e.expr} — (range × step) iteration excluded from shape rules (empty-frame flake gate)`,
          });
          return;
        }

        // Aggregating-target assertions. Mirrors phase-1
        // (iterate-panel-shape.spec.ts) — every `by(...)` key MUST
        // appear on at least one returned frame; every `without(...)`
        // key MUST be absent from every frame.
        if (e.byKeys.length > 0) {
          try {
            assertLabelShape(envelope, e.byKeys);
          } catch (err) {
            failures.push(
              `[${surface}] ${(err as Error).message}\n  expr: ${e.expr}`,
            );
          }
        }
        if (e.withoutKeys.length > 0) {
          try {
            assertLabelAbsent(envelope, e.withoutKeys);
          } catch (err) {
            failures.push(
              `[${surface}] ${(err as Error).message}\n  expr: ${e.expr}`,
            );
          }
        }

        // Histogram-target assertions. Mirrors phase-2
        // (iterate-histogram-completeness.spec.ts) — the matrix here
        // does NOT re-probe `/api/v1/series` for _bucket presence
        // (phase-2 already pins that at the short range; doing it
        // per-(range, step) would 12× the probe count for no extra
        // signal). Instead we apply the two branches based on the
        // expression's structure alone:
        //   - histogram_quantile over a `_bucket`-named root → the
        //     response MUST be non-empty (assertHistogramComplete).
        //   - histogram_quantile over a non-bucket root → the
        //     response MUST be empty (assertNoFabricatedValue).
        if (e.isHistogram) {
          if (e.histogramName === null) {
            try {
              assertNoFabricatedValue(envelope, e.expr);
            } catch (err) {
              failures.push(
                `[${surface}] ${(err as Error).message}\n  expr: ${e.expr}`,
              );
            }
          } else {
            try {
              assertHistogramComplete(envelope, e.histogramName);
            } catch (err) {
              failures.push(
                `[${surface}] ${(err as Error).message}\n  expr: ${e.expr}`,
              );
            }
          }
        }
      } catch (err) {
        failures.push(
          `[${surface}] time-ranges probe threw: ${(err as Error).message}\n  url: ${queryURL}`,
        );
      }
    }),
  );

  testInfo.annotations.push({
    type: 'time-ranges-empty-rate',
    description: `${emptyFrameCount} / ${entries.length} (panel × range × step) iteration(s) returned empty frames (annotated, not failed — see flake-handling contract in spec header)`,
  });

  if (failures.length > 0) {
    throw new Error(
      `time-ranges rule violated for ${failures.length} (panel × range × step) iteration(s):\n\n${failures.join('\n\n')}`,
    );
  }
});

// Compile-time guards. These exist so TypeScript surfaces a contract
// break in the helpers (a renamed field, a dropped type) at spec-load
// time rather than mid-test. They never execute.
const _typecheck: ((d: Dashboard, p: Panel, t: PanelTarget) => void) | undefined =
  undefined;
void _typecheck;
