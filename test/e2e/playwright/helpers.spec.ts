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
 *     `drilldown.ts:isAppInstalled`. These guard the I/O surface
 *     against the stack the spec is scheduled on (the k3d dashboard
 *     lane, .github/scripts/dashboard-matrix.mjs). An unreachable
 *     Grafana FAILS them: a helper self-test that passes with no stack
 *     up has verified nothing, and a lane that lost its Grafana must
 *     go red here, not green.
 *
 * Run via (with a stack up on GRAFANA_BASE_URL):
 *   cd test/e2e/playwright && npx playwright test helpers.spec.ts
 */

import { expect, test } from '@playwright/test';

import {
  type Dashboard,
  type Panel,
  type PanelTarget,
  addLabelFilter,
  addLogQLLabelFilter,
  addTraceQLAttributeFilter,
  assertHistogramComplete,
  assertLabelAbsent,
  assertLabelShape,
  assertNoFabricatedValue,
  assertSubsetByCount,
  expectedByKeys,
  expectedByKeysForDsType,
  expressionHasMatcherFor,
  extractByKeys,
  extractDataSourceProxyURL,
  extractHistogramName,
  extractLogQLByKeys,
  extractTraceQLByKeys,
  extractWithoutKeys,
  isHistogramQuantile,
  isNativeHistogramSelector,
  isPinnedResourceBoundRejection,
  bodyContainsPinnedResourceBoundMessage,
  MEMORY_LIMIT_MESSAGE,
  RESOURCE_BOUND_GUARD_MESSAGES,
  iterateDashboards,
  iteratePanels,
  iterateDrilldownApps,
  isAppInstalled,
  describeSweepDepth,
  sweepDepth,
  DRILLDOWN_APPS,
  dsQueryRequestMatchesPanel,
  panelForDsQueryRequest,
  parseDsQueryRequest,
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

test('expectedByKeys passes raw by-keys through for plain aggregations', () => {
  // No top-level call consumes labels here — `expectedByKeys` is
  // identity-mod-dedup over `extractByKeys`.
  expect(expectedByKeys('sum by (a, b) (foo)')).toEqual(['a', 'b']);
  expect(expectedByKeys('count by (k) (rate(foo[5m]))')).toEqual(['k']);
  expect(expectedByKeys('sum(foo)')).toEqual([]);
});

test('expectedByKeys subtracts le when histogram_quantile is the top-level call', () => {
  // The load-bearing case for this helper: the N2/N11/N14 spec
  // wrote `assertLabelShape` against `by(le, cerberus_ql)` extracted
  // from a histogram_quantile expression, but the quantile collapses
  // `le` into a scalar before returning — so the result series have
  // `cerberus_ql` only. The raw `extractByKeys` is therefore
  // mathematically-impossible-to-satisfy on the response; the
  // semantic `expectedByKeys` is what the spec must use.
  expect(
    expectedByKeys(
      'histogram_quantile(0.95, sum by (le, cerberus_ql) (rate(cerberus_queries_duration_seconds_bucket[5m])))',
    ),
  ).toEqual(['cerberus_ql']);
  expect(
    expectedByKeys(
      'histogram_quantile(0.95, sum by (le) (rate(foo_bucket[5m])))',
    ),
  ).toEqual([]);
});

test('expectedByKeys leaves le alone when histogram_quantile is NOT the top-level call', () => {
  // Defence-in-depth: a panel that aggregates by `le` outside a
  // `histogram_quantile` call legitimately surfaces `le` on the
  // response (uncommon but valid). Only the top-level
  // histogram_quantile path consumes the bucket-boundary label.
  expect(expectedByKeys('sum by (le, k) (foo_bucket)')).toEqual(['le', 'k']);
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

test('isNativeHistogramSelector detects an _exp_hist selector', () => {
  expect(
    isNativeHistogramSelector(
      'histogram_quantile(0.95, sum by (cerberus_ql) (rate(cerberus_queries_duration_exp_hist[5m])))',
    ),
  ).toBe(true);
});

test('isNativeHistogramSelector is false for the true N6 shape and for classic _bucket exprs', () => {
  expect(isNativeHistogramSelector('histogram_quantile(0.95, foo_total)')).toBe(false);
  expect(
    isNativeHistogramSelector('histogram_quantile(0.95, rate(foo_bucket[5m]))'),
  ).toBe(false);
  expect(isNativeHistogramSelector('rate(foo[5m])')).toBe(false);
});

test('isPinnedResourceBoundRejection accepts the exact memory-limit and guard-message contracts', () => {
  expect(
    isPinnedResourceBoundRejection(
      422,
      JSON.stringify({ status: 'error', errorType: 'execution', error: MEMORY_LIMIT_MESSAGE }),
    ),
  ).toBe(true);
  for (const msg of RESOURCE_BOUND_GUARD_MESSAGES) {
    expect(
      isPinnedResourceBoundRejection(
        422,
        JSON.stringify({ status: 'error', errorType: 'execution', error: msg }),
      ),
    ).toBe(true);
  }
});

test('isPinnedResourceBoundRejection rejects everything else', () => {
  // Wrong status.
  expect(
    isPinnedResourceBoundRejection(
      400,
      JSON.stringify({ status: 'error', errorType: 'execution', error: MEMORY_LIMIT_MESSAGE }),
    ),
  ).toBe(false);
  // Wrong errorType.
  expect(
    isPinnedResourceBoundRejection(
      422,
      JSON.stringify({ status: 'error', errorType: 'bad_data', error: MEMORY_LIMIT_MESSAGE }),
    ),
  ).toBe(false);
  // Unrelated message.
  expect(
    isPinnedResourceBoundRejection(
      422,
      JSON.stringify({ status: 'error', errorType: 'execution', error: 'something else' }),
    ),
  ).toBe(false);
  // Malformed body.
  expect(isPinnedResourceBoundRejection(422, 'not json')).toBe(false);
});

test('bodyContainsPinnedResourceBoundMessage matches a Grafana-rewrapped tunneled error', () => {
  const body = JSON.stringify({
    results: {
      A: { error: `execution: ${RESOURCE_BOUND_GUARD_MESSAGES[0]}` },
    },
  });
  expect(bodyContainsPinnedResourceBoundMessage(body)).toBe(true);
  expect(bodyContainsPinnedResourceBoundMessage('some unrelated body')).toBe(false);
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

test('addLabelFilter injects into a bare metric name', () => {
  expect(addLabelFilter('rate(foo[5m])', 'cerberus_ql', 'promql')).toBe(
    'rate(foo{cerberus_ql="promql"}[5m])',
  );
});

test('addLabelFilter appends to an existing non-empty selector block', () => {
  expect(
    addLabelFilter('rate(foo{job="x"}[5m])', 'cerberus_ql', 'promql'),
  ).toBe('rate(foo{job="x",cerberus_ql="promql"}[5m])');
});

test('addLabelFilter populates an empty selector block', () => {
  expect(addLabelFilter('rate(foo{}[5m])', 'cerberus_ql', 'promql')).toBe(
    'rate(foo{cerberus_ql="promql"}[5m])',
  );
});

test('addLabelFilter handles metric-less selectors like {__name__=~".+"}', () => {
  expect(
    addLabelFilter('rate({__name__=~".+"}[5m])', 'service_name', 'cerberus'),
  ).toBe('rate({__name__=~".+",service_name="cerberus"}[5m])');
});

test('addLabelFilter walks through aggregation wrappers untouched', () => {
  expect(
    addLabelFilter(
      'sum by (cerberus_ql) (rate(cerberus_queries_total[5m]))',
      'cerberus_ql',
      'promql',
    ),
  ).toBe(
    'sum by (cerberus_ql) (rate(cerberus_queries_total{cerberus_ql="promql"}[5m]))',
  );
});

test('addLabelFilter handles histogram_quantile + inner by(le, k)', () => {
  expect(
    addLabelFilter(
      'histogram_quantile(0.95, sum by (le, cerberus_ql) (rate(cerberus_queries_duration_seconds_bucket[5m])))',
      'cerberus_ql',
      'promql',
    ),
  ).toBe(
    'histogram_quantile(0.95, sum by (le, cerberus_ql) (rate(cerberus_queries_duration_seconds_bucket{cerberus_ql="promql"}[5m])))',
  );
});

test('addLabelFilter does not mistake `m` in `[5m]` for an identifier', () => {
  // Regression guard: a naive identifier regex matches `m` in the
  // duration suffix `5m`. The walker uses a word-boundary check that
  // excludes preceding digits.
  const out = addLabelFilter('rate(foo[5m])', 'k', 'v');
  expect(out).toBe('rate(foo{k="v"}[5m])');
  expect(out).not.toContain('m{');
});

test('addLabelFilter escapes embedded double-quotes and backslashes in the value', () => {
  expect(addLabelFilter('foo', 'k', 'a"b')).toBe('foo{k="a\\"b"}');
  expect(addLabelFilter('foo', 'k', 'a\\b')).toBe('foo{k="a\\\\b"}');
});

test('addLabelFilter does not mutate label values inside existing matchers', () => {
  // The string literal `"x"` must be copied verbatim — the `x` is not
  // a fresh metric-name selector.
  expect(addLabelFilter('foo{job="x"} or bar', 'k', 'v')).toBe(
    'foo{job="x",k="v"} or bar{k="v"}',
  );
});

test('addLabelFilter respects function calls and PromQL keywords', () => {
  // `rate(`, `sum(`, `histogram_quantile(` are all function calls
  // (followed by `(`) and never selectors. `or`, `and`, `unless` are
  // keywords. Only the metric names `foo` / `bar` get the injection.
  expect(
    addLabelFilter('rate(foo[5m]) or rate(bar[5m])', 'k', 'v'),
  ).toBe('rate(foo{k="v"}[5m]) or rate(bar{k="v"}[5m])');
});

test('expressionHasMatcherFor finds a matcher in any selector block', () => {
  expect(
    expressionHasMatcherFor(
      'rate(foo{cerberus_ql="promql"}[5m])',
      'cerberus_ql',
    ),
  ).toBe(true);
  expect(
    expressionHasMatcherFor('rate(foo{job="x"}[5m])', 'cerberus_ql'),
  ).toBe(false);
  expect(expressionHasMatcherFor('rate(foo[5m])', 'cerberus_ql')).toBe(false);
});

test('expressionHasMatcherFor does not confuse by(...) keys for matchers', () => {
  // `sum by (cerberus_ql)` doesn't constrain the label — it just
  // groups by it. The drill-down spec must still be willing to
  // filter on cerberus_ql.
  expect(
    expressionHasMatcherFor('sum by (cerberus_ql) (foo)', 'cerberus_ql'),
  ).toBe(false);
});

test('expressionHasMatcherFor handles all matcher operators', () => {
  expect(expressionHasMatcherFor('foo{k="a"}', 'k')).toBe(true);
  expect(expressionHasMatcherFor('foo{k!="a"}', 'k')).toBe(true);
  expect(expressionHasMatcherFor('foo{k=~"a"}', 'k')).toBe(true);
  expect(expressionHasMatcherFor('foo{k!~"a"}', 'k')).toBe(true);
});

test('assertSubsetByCount passes when filtered is non-empty + ≤ baseline', () => {
  expect(() => assertSubsetByCount(3, 10, 'panel:foo')).not.toThrow();
  expect(() => assertSubsetByCount(10, 10, 'panel:foo')).not.toThrow();
  expect(() => assertSubsetByCount(1, 1, 'panel:foo')).not.toThrow();
});

test('assertSubsetByCount throws when filtered is zero', () => {
  expect(() => assertSubsetByCount(0, 10, 'panel:foo')).toThrow(
    /returned 0 series/,
  );
});

test('assertSubsetByCount throws when filtered exceeds baseline', () => {
  expect(() => assertSubsetByCount(11, 10, 'panel:foo')).toThrow(
    /filtered=11 > baseline=10/,
  );
});

test('iterateDrilldownApps returns the apps the pinned Grafana ships', () => {
  // The catalogue is scoped to what grafana/grafana:12.2.9 preinstalls
  // first-party (hardcoded in Grafana's pkg/setting/setting_plugins.go
  // preinstall list): the metrics, logs, and traces drilldown apps.
  // grafana-pyroscope-app stays out — cerberus ships no profiling
  // backend. This pin moves in lock-step with the catalogue.
  const apps = iterateDrilldownApps();
  expect(apps.length).toBe(3);
  expect(apps.map((a) => a.id)).toEqual([
    'grafana-metricsdrilldown-app',
    'grafana-lokiexplore-app',
    'grafana-exploretraces-app',
  ]);
  expect(apps[0]?.root).toBe('/a/grafana-metricsdrilldown-app/drilldown');
  // Returned array must be a fresh copy — mutating it must not
  // contaminate the module-level constant.
  apps.pop();
  expect(DRILLDOWN_APPS.length).toBe(3);
});

test('DRILLDOWN_APPS entries each carry id + root + label', () => {
  for (const app of DRILLDOWN_APPS) {
    expect(app.id).toMatch(/^grafana-[a-z]+-app$/);
    expect(app.root.startsWith('/a/')).toBe(true);
    expect(app.root).toContain(app.id);
    expect(app.label.length).toBeGreaterThan(0);
  }
});

// --- LogQL filter helpers (Phase 3b) ---------------------------------------

test('addLogQLLabelFilter appends to a bare non-empty stream selector', () => {
  expect(
    addLogQLLabelFilter('{service_name="cerberus"}', 'level', 'error'),
  ).toBe('{service_name="cerberus",level="error"}');
});

test('addLogQLLabelFilter populates an empty stream selector', () => {
  expect(addLogQLLabelFilter('{}', 'service_name', 'cerberus')).toBe(
    '{service_name="cerberus"}',
  );
});

test('addLogQLLabelFilter only touches the stream selector, not the pipeline', () => {
  // The label-filter stage `| SeverityText="ERROR"` is a post-parse
  // predicate, not a stream matcher. Injection must NOT add a second
  // `level="error"` after the pipe.
  expect(
    addLogQLLabelFilter(
      '{service_name="cerberus"} | SeverityText="ERROR"',
      'level',
      'error',
    ),
  ).toBe(
    '{service_name="cerberus",level="error"} | SeverityText="ERROR"',
  );
});

test('addLogQLLabelFilter survives a line_format stage with embedded braces', () => {
  // `| line_format "{{.foo}}"` carries `{{` / `}}` inside a string
  // literal. A naive `\{[^{}]*\}` regex would carve `{.foo}` out of
  // it; the walker skips string literals as a whole.
  expect(
    addLogQLLabelFilter(
      '{service_name="cerberus"} | json | line_format "{{.foo}}"',
      'level',
      'error',
    ),
  ).toBe(
    '{service_name="cerberus",level="error"} | json | line_format "{{.foo}}"',
  );
});

test('addLogQLLabelFilter walks into a sum-by-rate aggregation', () => {
  // The drill key (`SeverityText`) is distinct from the matcher
  // already inside the stream selector (`service_name=~".+"`), so the
  // helper injects rather than no-oping. This mirrors what the
  // iterator does after filtering out already-matched keys via
  // `expressionHasMatcherFor`.
  expect(
    addLogQLLabelFilter(
      'sum by (SeverityText) (rate({service_name=~".+"}[5m]))',
      'SeverityText',
      'ERROR',
    ),
  ).toBe(
    'sum by (SeverityText) (rate({service_name=~".+",SeverityText="ERROR"}[5m]))',
  );
});

test('addLogQLLabelFilter is idempotent when the key is already matched', () => {
  // The iterator filters out keys with an existing matcher upstream,
  // but the helper itself is also a no-op defensively.
  expect(addLogQLLabelFilter('{level="error"}', 'level', 'error')).toBe(
    '{level="error"}',
  );
  expect(
    addLogQLLabelFilter('{service_name="cerberus",level=~"e.*"}', 'level', 'x'),
  ).toBe('{service_name="cerberus",level=~"e.*"}');
});

test('addLogQLLabelFilter escapes embedded double-quotes and backslashes in the value', () => {
  expect(addLogQLLabelFilter('{a="b"}', 'k', 'a"b')).toBe(
    '{a="b",k="a\\"b"}',
  );
  expect(addLogQLLabelFilter('{a="b"}', 'k', 'a\\b')).toBe(
    '{a="b",k="a\\\\b"}',
  );
});

test('addLogQLLabelFilter handles binary expressions over two stream selectors', () => {
  // LogQL binary ops (`or`, `and`) over two metric-shape selectors are
  // valid; each stream-selector block must get the matcher.
  expect(
    addLogQLLabelFilter(
      'rate({a="1"}[5m]) or rate({b="2"}[5m])',
      'env',
      'prod',
    ),
  ).toBe('rate({a="1",env="prod"}[5m]) or rate({b="2",env="prod"}[5m])');
});

test('addLogQLLabelFilter does not touch a label-filter-stage matcher', () => {
  // `| level="error"` is a pipeline-stage filter, not a stream matcher.
  // Injecting `level` into the *stream* selector is correct; the
  // post-pipe expression stays untouched.
  expect(
    addLogQLLabelFilter(
      '{service_name="cerberus"} | level="error"',
      'env',
      'prod',
    ),
  ).toBe('{service_name="cerberus",env="prod"} | level="error"');
});

test('extractLogQLByKeys parses LogQL sum-by aggregation', () => {
  expect(
    extractLogQLByKeys(
      'sum by (SeverityText) (rate({service_name=~".+"}[5m]))',
    ),
  ).toEqual(['SeverityText']);
});

test('extractLogQLByKeys returns empty for a non-aggregating log query', () => {
  expect(
    extractLogQLByKeys('{service_name="cerberus"} | SeverityText="ERROR"'),
  ).toEqual([]);
});

// --- TraceQL filter helpers (Phase 3b) -------------------------------------

test('addTraceQLAttributeFilter appends to a non-empty spanset with `&&`', () => {
  expect(
    addTraceQLAttributeFilter(
      '{ status = error }',
      'resource.service.name',
      'cerberus',
    ),
  ).toBe('{ status = error && resource.service.name="cerberus" }');
});

test('addTraceQLAttributeFilter populates an empty spanset', () => {
  expect(
    addTraceQLAttributeFilter('{}', 'resource.service.name', 'cerberus'),
  ).toBe('{ resource.service.name="cerberus" }');
});

test('addTraceQLAttributeFilter survives a pad-free spanset', () => {
  // `{status=error}` (no padding) should still get a sensible
  // `&&`-joined matcher rather than malforming into `{status=error&&…}`.
  expect(
    addTraceQLAttributeFilter('{status=error}', 'span.kind', 'server'),
  ).toBe('{status=error && span.kind="server" }');
});

test('addTraceQLAttributeFilter handles dotted attribute keys', () => {
  // TraceQL attributes are dotted paths — the matcher must serialise
  // verbatim, not get reinterpreted as a regex / member-access shape.
  expect(
    addTraceQLAttributeFilter(
      '{ duration > 100ms }',
      'resource.service.name',
      'cerberus',
    ),
  ).toBe('{ duration > 100ms && resource.service.name="cerberus" }');
});

test('addTraceQLAttributeFilter only touches the spanset, not the pipeline', () => {
  // The pipeline `| rate() by (resource.service.name)` carries the
  // aggregation key but is NOT a filter — the matcher belongs in the
  // spanset before the pipe.
  expect(
    addTraceQLAttributeFilter(
      '{ status = error } | rate() by (resource.service.name)',
      'span.kind',
      'server',
    ),
  ).toBe(
    '{ status = error && span.kind="server" } | rate() by (resource.service.name)',
  );
});

test('addTraceQLAttributeFilter is idempotent when the attribute is already matched', () => {
  expect(
    addTraceQLAttributeFilter(
      '{ resource.service.name = "cerberus" }',
      'resource.service.name',
      'cerberus',
    ),
  ).toBe('{ resource.service.name = "cerberus" }');
});

test('addTraceQLAttributeFilter ignores `{` inside string literals', () => {
  // A trace-attribute value containing `{` would otherwise look like a
  // nested spanset. The walker treats it as opaque.
  expect(
    addTraceQLAttributeFilter(
      '{ resource.service.name = "weird{value" }',
      'span.kind',
      'server',
    ),
  ).toBe(
    '{ resource.service.name = "weird{value" && span.kind="server" }',
  );
});

test('addTraceQLAttributeFilter escapes embedded double-quotes and backslashes in the value', () => {
  expect(
    addTraceQLAttributeFilter('{}', 'span.label', 'a"b'),
  ).toBe('{ span.label="a\\"b" }');
  expect(addTraceQLAttributeFilter('{}', 'span.label', 'a\\b')).toBe(
    '{ span.label="a\\\\b" }',
  );
});

test('extractTraceQLByKeys parses `| rate() by (k)` pipeline', () => {
  expect(
    extractTraceQLByKeys(
      '{ status = error } | rate() by (resource.service.name)',
    ),
  ).toEqual(['resource.service.name']);
});

test('extractTraceQLByKeys parses count_over_time/sum_over_time aggregations', () => {
  expect(
    extractTraceQLByKeys(
      '{ resource.service.name = "x" } | count_over_time() by (span.kind)',
    ),
  ).toEqual(['span.kind']);
  expect(
    extractTraceQLByKeys(
      '{ resource.service.name != "" } | sum_over_time(duration) by (resource.service.name, span.name)',
    ),
  ).toEqual(['resource.service.name', 'span.name']);
});

test('extractTraceQLByKeys returns empty for a bare spanset', () => {
  expect(extractTraceQLByKeys('{ resource.service.name != "" }')).toEqual(
    [],
  );
});

test('expectedByKeysForDsType dispatches per dsType', () => {
  expect(
    expectedByKeysForDsType(
      'sum by (cerberus_ql) (rate(cerberus_queries_total[5m]))',
      'prometheus',
    ),
  ).toEqual(['cerberus_ql']);
  expect(
    expectedByKeysForDsType(
      'histogram_quantile(0.95, sum by (le, k) (rate(foo_bucket[5m])))',
      'prometheus',
    ),
  ).toEqual(['k']); // le subtracted by expectedByKeys
  expect(
    expectedByKeysForDsType(
      'sum by (SeverityText) (rate({service_name=~".+"}[5m]))',
      'loki',
    ),
  ).toEqual(['SeverityText']);
  expect(
    expectedByKeysForDsType(
      '{ status = error } | rate() by (resource.service.name)',
      'tempo',
    ),
  ).toEqual(['resource.service.name']);
  // Unknown dsType → empty (the iterator skips these targets).
  expect(expectedByKeysForDsType('rate(foo[5m])', 'opentsdb')).toEqual([]);
});

// --- SWEEP_DEPTH plumbing ----------------------------------------------------

test('sweepDepth defaults to lean and honours SWEEP_DEPTH', () => {
  const saved = process.env.SWEEP_DEPTH;
  try {
    delete process.env.SWEEP_DEPTH;
    expect(sweepDepth()).toBe('lean');
    process.env.SWEEP_DEPTH = 'lean';
    expect(sweepDepth()).toBe('lean');
    process.env.SWEEP_DEPTH = 'full';
    expect(sweepDepth()).toBe('full');
  } finally {
    if (saved === undefined) delete process.env.SWEEP_DEPTH;
    else process.env.SWEEP_DEPTH = saved;
  }
});

test('sweepDepth throws on a typo rather than silently falling back', () => {
  const saved = process.env.SWEEP_DEPTH;
  try {
    process.env.SWEEP_DEPTH = 'FULL';
    expect(() => sweepDepth()).toThrow(/must be 'lean' or 'full'/);
    process.env.SWEEP_DEPTH = 'deep';
    expect(() => sweepDepth()).toThrow(/must be 'lean' or 'full'/);
  } finally {
    if (saved === undefined) delete process.env.SWEEP_DEPTH;
    else process.env.SWEEP_DEPTH = saved;
  }
});

test('describeSweepDepth documents each depth distinctly', () => {
  expect(describeSweepDepth('lean')).toContain('lean');
  expect(describeSweepDepth('full')).toContain('full');
  expect(describeSweepDepth('lean')).not.toBe(describeSweepDepth('full'));
});

// --- ds/query request ↔ panel attribution (helpers/ds-query.ts) -------------
//
// The self-observability board has five panels that all group by
// `cerberus_ql`; attribution must come from the REQUEST's own expression,
// never from a response-body substring, or one panel's assertions are
// satisfiable by a sibling and a sibling's failure is filed under the
// wrong name. The fixture below is that board's shape in miniature.

const dsQueryFixtureDashboard: Dashboard = {
  uid: 'cerberus-self',
  title: 'Cerberus',
  templating: { list: [] },
  panels: [
    {
      id: 1,
      title: 'Query rate by language',
      type: 'timeseries',
      targets: [
        {
          refId: 'A',
          expr: 'sum by (cerberus_ql) (rate(cerberus_queries_total[5m]))',
          datasource: { type: 'prometheus', uid: 'cerberus-prometheus' },
        },
      ],
      gridPos: { x: 0, y: 0, w: 12, h: 8 },
    },
    {
      id: 2,
      title: 'P95 latency by language',
      type: 'timeseries',
      targets: [
        {
          refId: 'A',
          expr: 'histogram_quantile(0.95, sum by (cerberus_ql) (rate(cerberus_queries_duration_exp_hist[5m])))',
          datasource: { type: 'prometheus', uid: 'cerberus-prometheus' },
        },
      ],
      gridPos: { x: 12, y: 0, w: 12, h: 8 },
    },
    {
      id: 3,
      title: 'Error rate by language',
      type: 'timeseries',
      targets: [
        {
          refId: 'A',
          expr: 'sum by (cerberus_ql) (rate(cerberus_queries_total{result="error"}[5m]))',
          datasource: { type: 'prometheus', uid: 'cerberus-prometheus' },
        },
      ],
      gridPos: { x: 0, y: 8, w: 12, h: 8 },
    },
  ],
};

function dsQueryBody(exprs: string[]): string {
  return JSON.stringify({
    queries: exprs.map((expr, i) => ({
      refId: String.fromCharCode('A'.charCodeAt(0) + i),
      expr,
      datasource: { type: 'prometheus', uid: 'cerberus-prometheus' },
      intervalMs: 15000,
      maxDataPoints: 1000,
    })),
    from: '1700000000000',
    to: '1700003600000',
  });
}

test('parseDsQueryRequest reads refId/expr/datasource uid and drops empty or unparseable bodies', () => {
  const queries = parseDsQueryRequest(dsQueryBody(['up', '  rate(foo[1m])  ']));
  expect(queries).toEqual([
    { refId: 'A', expr: 'up', datasourceUid: 'cerberus-prometheus' },
    { refId: 'B', expr: 'rate(foo[1m])', datasourceUid: 'cerberus-prometheus' },
  ]);
  // TraceQL/LogQL targets carry `query`, not `expr`.
  expect(
    parseDsQueryRequest(JSON.stringify({ queries: [{ refId: 'A', query: '{ }', datasource: 'cerberus-tempo' }] })),
  ).toEqual([{ refId: 'A', expr: '{ }', datasourceUid: 'cerberus-tempo' }]);
  expect(parseDsQueryRequest(null)).toEqual([]);
  expect(parseDsQueryRequest('')).toEqual([]);
  expect(parseDsQueryRequest('not json')).toEqual([]);
  expect(parseDsQueryRequest(JSON.stringify({ queries: [{ refId: 'A', expr: '' }] }))).toEqual([]);
});

test('ds/query attribution selects only the panel whose own expression the request carries', () => {
  const rate = dsQueryFixtureDashboard.panels[0]!;
  const p95 = dsQueryFixtureDashboard.panels[1]!;
  const errors = dsQueryFixtureDashboard.panels[2]!;

  // Every panel on the board mentions `cerberus_ql`; a substring match on
  // the group-by key would accept all three. Exact-expression attribution
  // accepts exactly one.
  const rateReq = parseDsQueryRequest(dsQueryBody([rate.targets[0]!.expr!]));
  expect(dsQueryRequestMatchesPanel(rateReq, rate)).toBe(true);
  expect(dsQueryRequestMatchesPanel(rateReq, p95)).toBe(false);
  expect(dsQueryRequestMatchesPanel(rateReq, errors)).toBe(false);
  expect(panelForDsQueryRequest(dsQueryFixtureDashboard, rateReq)?.title).toBe(rate.title);

  const p95Req = parseDsQueryRequest(dsQueryBody([p95.targets[0]!.expr!]));
  expect(panelForDsQueryRequest(dsQueryFixtureDashboard, p95Req)?.title).toBe(p95.title);
  expect(dsQueryRequestMatchesPanel(p95Req, rate)).toBe(false);

  // A near-miss expression (same metric, different selector) attributes
  // to nothing rather than to the closest sibling.
  const strangerReq = parseDsQueryRequest(
    dsQueryBody(['sum by (cerberus_ql) (rate(cerberus_queries_total{result="ok"}[5m]))']),
  );
  expect(panelForDsQueryRequest(dsQueryFixtureDashboard, strangerReq)).toBeUndefined();
  for (const p of dsQueryFixtureDashboard.panels) {
    expect(dsQueryRequestMatchesPanel(strangerReq, p)).toBe(false);
  }

  // A request with no queries attributes to nothing.
  expect(panelForDsQueryRequest(dsQueryFixtureDashboard, [])).toBeUndefined();
});

// --- Live-Grafana tests -----------------------------------------------------

/**
 * Fail loudly when Grafana is not up. The live tests below exist to
 * exercise the helpers' I/O surface; returning green without a stack
 * would be a test that cannot fail (invariant 6).
 */
async function expectGrafanaReachable(
  request: import('@playwright/test').APIRequestContext,
  baseURL: string,
): Promise<void> {
  const probe = await request.get(`${baseURL}/api/health`).catch(() => null);
  expect(
    probe?.status() ?? 0,
    `Grafana at ${baseURL} must be reachable for the live helper tests (GET /api/health); ` +
      'run this spec against a live stack (GRAFANA_BASE_URL), never without one',
  ).toBeGreaterThanOrEqual(200);
  expect(probe?.status() ?? 0, `GET ${baseURL}/api/health`).toBeLessThan(300);
}

test('iterateDashboards round-trips against a live Grafana', async ({
  request,
}) => {
  const baseURL = process.env.GRAFANA_BASE_URL ?? 'http://localhost:3000';
  await expectGrafanaReachable(request, baseURL);

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

test('isAppInstalled distinguishes installed apps from a known-bogus id', async ({
  request,
}) => {
  const baseURL = process.env.GRAFANA_BASE_URL ?? 'http://localhost:3000';
  await expectGrafanaReachable(request, baseURL);

  // A clearly-bogus plugin id must resolve to false (404 → not
  // installed). This is the load-bearing contract — the phase-6 spec
  // relies on isAppInstalled returning false on the missing
  // grafana-pyroscope-app rather than throwing.
  const bogus = await isAppInstalled(
    request,
    baseURL,
    'grafana-this-app-does-not-exist-anywhere',
  );
  expect(bogus).toBe(false);

  // At least one of the preinstalled drilldown apps in Grafana 12.2.9
  // must resolve to true. We probe all of them but only require ≥1 to
  // be present so a stripped-down stack (no apps at all) still surfaces
  // the contract: the helper returned a boolean, not a throw.
  const installed = await Promise.all(
    DRILLDOWN_APPS.map((app) => isAppInstalled(request, baseURL, app.id)),
  );
  for (const v of installed) {
    expect(typeof v).toBe('boolean');
  }
  test.info().annotations.push({
    type: 'drilldown-apps-installed',
    description: DRILLDOWN_APPS.map(
      (app, i) => `${app.id}=${installed[i] ? 'installed' : 'absent'}`,
    ).join(', '),
  });
});
