/**
 * Traces Drilldown — trace-list regression spec.
 *
 * Pins the consumer-grade contract behind Grafana's Traces Drilldown
 * (grafana-exploretraces-app) "Traces" tab: the tab badge must be
 * non-zero and the span table must render rows whenever the backing
 * `/api/search` window contains traces.
 *
 * Why this exists: the drilldown's trace list queries the Tempo
 * datasource with queryType='traceql' / tableType='spans', and
 * Grafana's tempo resultTransformer builds the spans table EXCLUSIVELY
 * from `trace.spanSets[].spans`. Cerberus used to return trace
 * summaries without `spanSets` (and ignored the request's `limit` +
 * `spss` params), so every drilldown request got HTTP 200 with
 * thousands of summaries — and the Traces tab still rendered badge 0 +
 * "No data for selected query". API-status-only checks were blind to
 * it; this spec asserts what the UI actually renders plus the wire
 * field the transform consumes.
 *
 * Gate posture: NIGHTLY ONLY (auto-discovered by the `dashboard` job's
 * unfiltered `npx playwright test` run in .github/workflows/e2e.yml;
 * deliberately NOT in the compose-smoke job's explicit spec list —
 * same pattern as iterate-drilldown-apps.spec.ts, because drilldown-app
 * UIs are the highest UI-churn surface in Grafana).
 *
 * Grafana version pinning: selectors are tied to grafana/grafana:12.2.9
 * + traces-drilldown 2.0.4 (tab accessible name is "Traces" with the
 * count badge concatenated, e.g. "Traces20"). Re-audit on Grafana
 * bumps — see helpers/README.md.
 *
 * Env:
 *   GRAFANA_URL       default http://localhost:3000
 *   GRAFANA_BASE_URL  honoured as a fallback for parity with
 *                     compose_grafana_smoke.spec.ts
 */

import { test, expect, type Response } from '@playwright/test';

const baseURL =
  process.env.GRAFANA_URL ??
  process.env.GRAFANA_BASE_URL ??
  'http://localhost:3000';

// The drilldown app root with the trace-list action view preselected.
// var-ds pins the cerberus Tempo datasource so the spec doesn't depend
// on it being the default; the time range matches the app's default
// (last 30 minutes — the e2e stacks emit self-telemetry traces
// continuously, so the window is never empty).
const traceListURL =
  '/a/grafana-exploretraces-app/explore' +
  '?actionView=traceList&var-ds=cerberus-tempo&from=now-30m&to=now';

test.describe('Traces Drilldown — trace list (tableType=spans)', () => {
  test('Traces tab badge > 0 and span table renders rows', async ({ page }) => {
    // The app boots its own React tree + fires a wave of datasource
    // queries; budget generously for a cold ClickHouse.
    test.setTimeout(180_000);

    // Capture the drilldown's proxied /api/search responses so the
    // wire shape is asserted alongside the rendered DOM — the badge
    // and the table are both derived from spanSets, so a UI-only
    // assertion could go stale silently if the app reshapes its DOM.
    const searchBodies: unknown[] = [];
    const onResponse = async (resp: Response) => {
      const url = resp.url();
      if (!url.includes('/api/search?') || resp.status() !== 200) {
        return;
      }
      try {
        searchBodies.push(await resp.json());
      } catch {
        // Non-JSON body on a search URL — leave it to the DOM
        // assertions below to fail loudly if the list is empty.
      }
    };
    page.on('response', onResponse);

    await page.goto(`${baseURL}${traceListURL}`, {
      waitUntil: 'domcontentloaded',
    });

    // The Traces tab renders its accessible name as "Traces<count>"
    // (count badge concatenated). Poll until the badge leaves 0 —
    // the trace-list query round-trips through cerberus + ClickHouse,
    // so the first paint can legitimately show 0 while loading.
    const tracesTab = page.getByRole('tab', { name: /^Traces/ });
    await expect(tracesTab, 'Traces tab is present').toBeVisible({
      timeout: 60_000,
    });
    await expect
      .poll(
        async () => {
          const text = (await tracesTab.innerText()).replace(/\s+/g, '');
          const m = text.match(/^Traces(\d+)$/);
          return m ? Number(m[1]) : 0;
        },
        {
          message:
            'Traces tab badge must become non-zero (badge = rows of the ' +
            'tableType=spans frame, built from /api/search spanSets)',
          timeout: 90_000,
        },
      )
      .toBeGreaterThan(0);

    // The trace-list action view must NOT show the empty-state copy.
    await expect(
      page.getByText('No data for selected query'),
      'trace list renders data, not the empty-state message',
    ).toHaveCount(0);

    // The span table itself renders at least one data row. Grafana's
    // table panel exposes role=row including a header row, hence > 1.
    const rows = page.getByRole('row');
    await expect
      .poll(async () => rows.count(), {
        message: 'span table renders at least one data row',
        timeout: 30_000,
      })
      .toBeGreaterThan(1);

    page.off('response', onResponse);

    // Wire-shape pin: at least one captured /api/search response must
    // carry per-trace spanSets (and the legacy spanSet mirror) — the
    // exact fields Grafana's resultTransformer reads. An empty traces
    // window would have failed the badge assertion already.
    type SearchBody = {
      traces?: Array<{
        spanSets?: Array<{ spans?: unknown[] }>;
        spanSet?: { spans?: unknown[] };
      }>;
    };
    const withTraces = (searchBodies as SearchBody[]).filter(
      (b) => Array.isArray(b?.traces) && b.traces.length > 0,
    );
    expect(
      withTraces.length,
      'at least one /api/search response with traces was captured',
    ).toBeGreaterThan(0);
    for (const body of withTraces) {
      for (const trace of body.traces ?? []) {
        expect(
          Array.isArray(trace.spanSets) && trace.spanSets.length > 0,
          `every trace summary carries spanSets (got: ${JSON.stringify(trace).slice(0, 200)})`,
        ).toBe(true);
        expect(
          (trace.spanSets?.[0]?.spans?.length ?? 0) > 0,
          'spanSets[0].spans is non-empty',
        ).toBe(true);
        expect(
          (trace.spanSet?.spans?.length ?? 0) > 0,
          'legacy spanSet mirror is populated',
        ).toBe(true);
      }
    }
  });
});
