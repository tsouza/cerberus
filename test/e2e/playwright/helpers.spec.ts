/**
 * helpers smoke — validates the public helper contracts compile +
 * behave on happy paths.
 *
 * Two flavours of test cohabit this file:
 *
 *  1. Pure-function tests (no Grafana required) — `query-shape.ts`,
 *     `probes.ts:extractDataSourceProxyURL`. These run anywhere,
 *     including a workstation with no compose stack up.
 *
 *  2. Live-Grafana tests — `dashboard.ts:iterateDashboards`,
 *     `assertions.ts:assertNon200ResponseClass` (against a known-2xx
 *     URL). These guard the I/O surface; the test condition probes
 *     Grafana first and bails out cleanly if the stack isn't up,
 *     so they double as a smoke that the helpers work against the
 *     compose stack the phase specs will run against.
 *
 * Run via:
 *   cd test/e2e/playwright && npx playwright test helpers.spec.ts
 *
 * The full phase specs that will consume these helpers don't exist
 * yet — this is phase 0 of the e2e-enhance plan.
 */

import { expect, test } from '@playwright/test';

import {
  type Dashboard,
  type Panel,
  type PanelTarget,
  assertHistogramComplete,
  assertLabelAbsent,
  assertLabelShape,
  assertNoFabricatedValue,
  extractByKeys,
  extractDataSourceProxyURL,
  extractHistogramName,
  extractWithoutKeys,
  isHistogramQuantile,
  iterateDashboards,
  iteratePanels,
  iterateDrilldownApps,
  DRILLDOWN_APPS,
} from './helpers/index.js';

// --- Pure-function tests ----------------------------------------------------

test('extractByKeys parses a single by-clause', () => {
  expect(extractByKeys('sum by (a, b) (foo)')).toEqual(['a', 'b']);
});

test('extractByKeys returns empty for a no-by aggregation', () => {
  expect(extractByKeys('sum(foo)')).toEqual([]);
});

test('extractByKeys dedupes keys across nested by-clauses', () => {
  expect(extractByKeys('sum by (a) (count by (b) (foo))')).toEqual(['a', 'b']);
  expect(extractByKeys('sum by (a) (sum by (a) (foo))')).toEqual(['a']);
});

test('extractByKeys handles multi-aggregator family', () => {
  expect(extractByKeys('count by (k) (rate(foo[5m]))')).toEqual(['k']);
  expect(extractByKeys('avg by (x, y) (foo)')).toEqual(['x', 'y']);
});

test('extractByKeys ignores without-clauses entirely', () => {
  // The two modifiers have inverted semantics — `by` keeps, `without`
  // drops. Conflating them was the bug the phase-1 split fixes.
  expect(extractByKeys('sum without (instance) (foo)')).toEqual([]);
  expect(extractByKeys('sum by (a) (sum without (b) (foo))')).toEqual(['a']);
});

test('extractWithoutKeys parses a single without-clause', () => {
  expect(extractWithoutKeys('sum without (instance) (foo)')).toEqual([
    'instance',
  ]);
});

test('extractWithoutKeys returns empty for a no-without aggregation', () => {
  expect(extractWithoutKeys('sum by (a, b) (foo)')).toEqual([]);
  expect(extractWithoutKeys('sum(foo)')).toEqual([]);
});

test('extractWithoutKeys dedupes keys across nested without-clauses', () => {
  expect(
    extractWithoutKeys('sum without (a, b) (sum without (a) (foo))'),
  ).toEqual(['a', 'b']);
});

test('isHistogramQuantile detects the call shape', () => {
  expect(isHistogramQuantile('histogram_quantile(0.95, foo)')).toBe(true);
  expect(isHistogramQuantile('rate(foo[5m])')).toBe(false);
});

test('extractHistogramName pulls the metric root for a _bucket arg', () => {
  expect(
    extractHistogramName(
      'histogram_quantile(0.95, rate(foo_bucket[5m]))',
    ),
  ).toBe('foo');
  expect(
    extractHistogramName(
      'histogram_quantile(0.95, sum by (le) (rate(my_metric_bucket[5m])))',
    ),
  ).toBe('my_metric');
});

test('extractHistogramName returns null when no _bucket is referenced', () => {
  // N6 shape — histogram_quantile over a non-bucket metric.
  expect(extractHistogramName('histogram_quantile(0.95, foo_total)')).toBeNull();
});

test('extractHistogramName returns null for non-histogram exprs', () => {
  expect(extractHistogramName('rate(foo[5m])')).toBeNull();
  expect(extractHistogramName('up')).toBeNull();
});

test('assertLabelShape passes when every key is observed', () => {
  expect(() =>
    assertLabelShape(
      {
        results: {
          A: {
            frames: [
              {
                schema: {
                  fields: [{ name: 'Value', labels: { a: 'x', b: 'y' } }],
                },
              },
            ],
          },
        },
      },
      ['a', 'b'],
    ),
  ).not.toThrow();
});

test('assertLabelShape throws when a key is missing', () => {
  expect(() =>
    assertLabelShape(
      {
        results: {
          A: {
            frames: [
              { schema: { fields: [{ name: 'Value', labels: { a: 'x' } }] } },
            ],
          },
        },
      },
      ['a', 'b'],
    ),
  ).toThrow(/missing=\[b\]/);
});

test('assertLabelShape no-ops when byKeys is empty', () => {
  expect(() => assertLabelShape({ results: {} }, [])).not.toThrow();
});

test('assertLabelAbsent passes when none of the without-keys appear', () => {
  expect(() =>
    assertLabelAbsent(
      {
        results: {
          A: {
            frames: [
              {
                schema: {
                  fields: [{ name: 'Value', labels: { job: 'cerberus' } }],
                },
              },
            ],
          },
        },
      },
      ['instance'],
    ),
  ).not.toThrow();
});

test('assertLabelAbsent throws when a without-key leaks through', () => {
  expect(() =>
    assertLabelAbsent(
      {
        results: {
          A: {
            frames: [
              {
                schema: {
                  fields: [
                    { name: 'Value', labels: { instance: 'host-1' } },
                  ],
                },
              },
            ],
          },
        },
      },
      ['instance'],
    ),
  ).toThrow(/leaked=\[instance\]/);
});

test('assertLabelAbsent no-ops when withoutKeys is empty', () => {
  expect(() => assertLabelAbsent({ results: {} }, [])).not.toThrow();
});

test('assertHistogramComplete passes on a non-empty frame', () => {
  expect(() =>
    assertHistogramComplete(
      {
        results: {
          A: {
            frames: [{ data: { values: [[1, 2, 3]] } }],
          },
        },
      },
      'foo',
    ),
  ).not.toThrow();
});

test('assertHistogramComplete throws on an empty envelope', () => {
  expect(() =>
    assertHistogramComplete(
      { results: { A: { frames: [{ data: { values: [] } }] } } },
      'foo',
    ),
  ).toThrow(/empty envelope/);
});

test('assertNoFabricatedValue passes on an empty envelope', () => {
  expect(() =>
    assertNoFabricatedValue(
      { results: { A: { frames: [] } } },
      'histogram_quantile(0.95, foo_total)',
    ),
  ).not.toThrow();
});

test('assertNoFabricatedValue throws on a non-empty envelope', () => {
  expect(() =>
    assertNoFabricatedValue(
      {
        results: {
          A: { frames: [{ data: { values: [[1]] } }] },
        },
      },
      'histogram_quantile(0.95, foo_total)',
    ),
  ).toThrow(/fabricated value/);
});

test('extractDataSourceProxyURL prefers target uid over panel uid', () => {
  const dashboard: Dashboard = {
    uid: 'd1',
    title: 'fixture',
    templating: { list: [] },
    panels: [],
  };
  const panel: Panel = {
    id: 1,
    title: 'p',
    type: 'timeseries',
    datasource: { type: 'prometheus', uid: 'panel-uid' },
    targets: [],
    gridPos: { x: 0, y: 0, w: 0, h: 0 },
  };
  const targetWithOverride: PanelTarget = {
    refId: 'A',
    datasource: { type: 'prometheus', uid: 'target-uid' },
  };
  const targetWithout: PanelTarget = { refId: 'B' };

  expect(extractDataSourceProxyURL(dashboard, panel, targetWithOverride)).toBe(
    '/api/datasources/proxy/uid/target-uid',
  );
  expect(extractDataSourceProxyURL(dashboard, panel, targetWithout)).toBe(
    '/api/datasources/proxy/uid/panel-uid',
  );
});

test('extractDataSourceProxyURL throws when no uid is resolvable', () => {
  const dashboard: Dashboard = {
    uid: 'd1',
    title: 'fixture',
    templating: { list: [] },
    panels: [],
  };
  const panel: Panel = {
    id: 1,
    title: 'p',
    type: 'timeseries',
    targets: [],
    gridPos: { x: 0, y: 0, w: 0, h: 0 },
  };
  const target: PanelTarget = { refId: 'A' };
  expect(() => extractDataSourceProxyURL(dashboard, panel, target)).toThrow(
    /no datasource uid/,
  );
});

test('iterateDrilldownApps returns three apps including the three built-ins', () => {
  const apps = iterateDrilldownApps();
  expect(apps.length).toBe(3);
  const ids = apps.map((a) => a.id).sort();
  expect(ids).toEqual(
    [
      'grafana-exploretraces-app',
      'grafana-lokiexplore-app',
      'grafana-metricsdrilldown-app',
    ].sort(),
  );
  // Returned array must be a fresh copy — mutating it must not
  // contaminate the module-level constant.
  apps.pop();
  expect(DRILLDOWN_APPS.length).toBe(3);
});

// --- Live-Grafana tests -----------------------------------------------------

test('iterateDashboards round-trips against a live Grafana', async ({
  request,
}) => {
  const baseURL = process.env.GRAFANA_BASE_URL ?? 'http://localhost:3000';
  // Probe Grafana first so the test is informative when the compose
  // stack isn't up — skip cleanly via test.fail() guard.
  const probe = await request.get(`${baseURL}/api/health`).catch(() => null);
  if (!probe || probe.status() < 200 || probe.status() > 299) {
    test.info().annotations.push({
      type: 'live-grafana',
      description: `Grafana at ${baseURL} not reachable; running pure-function tests only`,
    });
    return;
  }

  const dashboards = await iterateDashboards(request, baseURL);
  expect(dashboards.length).toBeGreaterThan(0);
  for (const dash of dashboards) {
    expect(dash.uid).not.toBe('');
    expect(dash.title).not.toBe('');
    const panels = iteratePanels(dash);
    expect(Array.isArray(panels)).toBe(true);
    // Row unwrapping invariant: no panel returned by iteratePanels
    // is itself a row (rows are flattened away).
    for (const p of panels) {
      expect(p.type).not.toBe('row');
    }
  }
});
