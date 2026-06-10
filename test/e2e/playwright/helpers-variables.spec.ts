/**
 * Stack-free unit tests for the template-variable contracts
 * (helpers/variables.ts): pin parsing, the set-equality matcher, the
 * variable-query parser for all three heads, the proxy-path builder,
 * and the response-shape extractors. The live resolution path
 * (resolveVariableOptions / checkDashboardVariable) is exercised by
 * iterate-all-dashboards.spec.ts once dashboards gain variables (P3);
 * the pure halves are pinned here. Run via:
 *
 *   cd test/e2e/playwright && npx playwright test helpers-variables.spec.ts
 */

import { expect, test } from '@playwright/test';

import {
  checkVariableOptions,
  compareOptionSets,
  extractVariableOptions,
  parseVariableQuery,
  readVariablePin,
  variableOptionsPath,
} from './helpers/index.js';

// --- readVariablePin ---------------------------------------------------------

test('readVariablePin returns null when no pin is declared', () => {
  expect(readVariablePin({ name: 'ql', type: 'query' })).toBeNull();
  expect(readVariablePin({ name: 'ql', cerberus: {} })).toBeNull();
  expect(readVariablePin(undefined)).toBeNull();
});

test('readVariablePin returns the declared option pin', () => {
  expect(
    readVariablePin({
      name: 'ql',
      cerberus: { expectOptions: ['promql', 'logql', 'traceql'] },
    }),
  ).toEqual(['promql', 'logql', 'traceql']);
});

test('readVariablePin throws on malformed pins', () => {
  expect(() => readVariablePin({ cerberus: 'pin' })).toThrow(
    /must be an object/,
  );
  expect(() => readVariablePin({ cerberus: { expectOptions: [] } })).toThrow(
    /non-empty array/,
  );
  expect(() =>
    readVariablePin({ cerberus: { expectOptions: ['ok', 42] } }),
  ).toThrow(/non-empty strings/);
  expect(() =>
    readVariablePin({ cerberus: { expectOptions: 'promql' } }),
  ).toThrow(/non-empty array/);
});

// --- compareOptionSets / checkVariableOptions --------------------------------

test('compareOptionSets passes on set equality regardless of order', () => {
  expect(compareOptionSets(['b', 'a'], ['a', 'b'])).toEqual([]);
  expect(compareOptionSets(['x'], ['x'])).toEqual([]);
});

test('compareOptionSets reports pinned options missing from the live set', () => {
  const v = compareOptionSets(['promql'], ['promql', 'logql']);
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('pinned options missing from the live set: [logql]');
});

test('compareOptionSets reports live options the pin does not expect', () => {
  const v = compareOptionSets(['promql', 'sqlql'], ['promql']);
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('live options not in the pin: [sqlql]');
});

test('compareOptionSets reports both directions at once', () => {
  const v = compareOptionSets(['a', 'c'], ['a', 'b']);
  expect(v).toHaveLength(2);
  expect(v.join('\n')).toContain('missing from the live set: [b]');
  expect(v.join('\n')).toContain('not in the pin: [c]');
});

test('compareOptionSets rejects duplicated live options', () => {
  const v = compareOptionSets(['a', 'a', 'b'], ['a', 'b']);
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('duplicate options [a]');
});

test('checkVariableOptions applies the pin when present', () => {
  expect(checkVariableOptions(['a', 'b'], ['b', 'a'])).toEqual([]);
  expect(checkVariableOptions(['a', 'b'], ['a'])).toHaveLength(1);
});

test('checkVariableOptions requires non-empty options when unpinned', () => {
  expect(checkVariableOptions(null, ['anything'])).toEqual([]);
  const v = checkVariableOptions(null, []);
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('zero options');
});

// --- parseVariableQuery ------------------------------------------------------

test('parseVariableQuery: prom label_values without selector', () => {
  expect(parseVariableQuery('prometheus', 'label_values(cerberus_ql)')).toEqual(
    { kind: 'prom-label-values', label: 'cerberus_ql' },
  );
});

test('parseVariableQuery: prom label_values with selector', () => {
  expect(
    parseVariableQuery(
      'prometheus',
      'label_values(cerberus_queries_total{result="ok"}, cerberus_ql)',
    ),
  ).toEqual({
    kind: 'prom-label-values',
    label: 'cerberus_ql',
    selector: 'cerberus_queries_total{result="ok"}',
  });
});

test('parseVariableQuery: prom label_names + object-wrapped query', () => {
  expect(parseVariableQuery('prometheus', 'label_names()')).toEqual({
    kind: 'prom-label-names',
  });
  expect(
    parseVariableQuery('prometheus', {
      query: 'label_values(up, job)',
      refId: 'PrometheusVariableQueryEditor-VariableQuery',
    }),
  ).toEqual({ kind: 'prom-label-values', label: 'job', selector: 'up' });
});

test('parseVariableQuery: prom rejects unrecognised query strings', () => {
  expect(() => parseVariableQuery('prometheus', 'metrics(up)')).toThrow(
    /unrecognised prometheus variable query/,
  );
  expect(() => parseVariableQuery('prometheus', 42)).toThrow(
    /neither a string nor \{query\}/,
  );
});

test('parseVariableQuery: loki object + string forms', () => {
  expect(
    parseVariableQuery('loki', { type: 1, label: 'service_name', refId: 'x' }),
  ).toEqual({ kind: 'loki-label-values', label: 'service_name' });
  expect(parseVariableQuery('loki', { type: 0, refId: 'x' })).toEqual({
    kind: 'loki-label-names',
  });
  expect(parseVariableQuery('loki', 'label_values(service_name)')).toEqual({
    kind: 'loki-label-values',
    label: 'service_name',
  });
  expect(parseVariableQuery('loki', 'label_names()')).toEqual({
    kind: 'loki-label-names',
  });
});

test('parseVariableQuery: loki rejects unrecognised shapes', () => {
  expect(() => parseVariableQuery('loki', { type: 9 })).toThrow(
    /unrecognised loki variable query/,
  );
  expect(() => parseVariableQuery('loki', 7)).toThrow(
    /unrecognised loki variable query/,
  );
});

test('parseVariableQuery: tempo tag names + tag values', () => {
  expect(parseVariableQuery('tempo', { type: 0, refId: 'x' })).toEqual({
    kind: 'tempo-tag-names',
  });
  expect(
    parseVariableQuery('tempo', { type: 1, label: 'service.name', refId: 'x' }),
  ).toEqual({ kind: 'tempo-tag-values', tag: 'service.name' });
  expect(() => parseVariableQuery('tempo', 'tags()')).toThrow(
    /unrecognised tempo variable query/,
  );
});

test('parseVariableQuery: unknown datasource type throws', () => {
  expect(() => parseVariableQuery('opentsdb', 'label_values(x)')).toThrow(
    /has no variable-query lookup/,
  );
});

// --- variableOptionsPath -----------------------------------------------------

test('variableOptionsPath builds the per-head proxy lookups', () => {
  expect(
    variableOptionsPath('prom-uid', { kind: 'prom-label-names' }),
  ).toBe('/api/datasources/proxy/uid/prom-uid/api/v1/labels');
  expect(
    variableOptionsPath('prom-uid', {
      kind: 'prom-label-values',
      label: 'cerberus_ql',
    }),
  ).toBe(
    '/api/datasources/proxy/uid/prom-uid/api/v1/label/cerberus_ql/values',
  );
  expect(
    variableOptionsPath('prom-uid', {
      kind: 'prom-label-values',
      label: 'job',
      selector: 'up{a="b"}',
    }),
  ).toBe(
    '/api/datasources/proxy/uid/prom-uid/api/v1/label/job/values?match[]=' +
      encodeURIComponent('up{a="b"}'),
  );
  expect(variableOptionsPath('loki-uid', { kind: 'loki-label-names' })).toBe(
    '/api/datasources/proxy/uid/loki-uid/loki/api/v1/labels',
  );
  expect(
    variableOptionsPath('loki-uid', {
      kind: 'loki-label-values',
      label: 'service_name',
    }),
  ).toBe(
    '/api/datasources/proxy/uid/loki-uid/loki/api/v1/label/service_name/values',
  );
  expect(variableOptionsPath('tempo-uid', { kind: 'tempo-tag-names' })).toBe(
    '/api/datasources/proxy/uid/tempo-uid/api/v2/search/tags',
  );
  expect(
    variableOptionsPath('tempo-uid', {
      kind: 'tempo-tag-values',
      tag: 'service.name',
    }),
  ).toBe(
    '/api/datasources/proxy/uid/tempo-uid/api/v2/search/tag/service.name/values',
  );
});

// --- extractVariableOptions --------------------------------------------------

test('extractVariableOptions: prom/loki success-data envelopes', () => {
  const body = { status: 'success', data: ['promql', 'logql'] };
  expect(
    extractVariableOptions({ kind: 'prom-label-values', label: 'x' }, body),
  ).toEqual(['promql', 'logql']);
  expect(
    extractVariableOptions({ kind: 'loki-label-names' }, body),
  ).toEqual(['promql', 'logql']);
});

test('extractVariableOptions: prom envelope mismatch throws', () => {
  expect(() =>
    extractVariableOptions(
      { kind: 'prom-label-values', label: 'x' },
      { status: 'error', error: 'boom' },
    ),
  ).toThrow(/not a success\/data envelope/);
});

test('extractVariableOptions: tempo v2 tags scopes union', () => {
  const body = {
    scopes: [
      { name: 'resource', tags: ['service.name', 'host.name'] },
      { name: 'span', tags: ['http.method', 'service.name'] },
    ],
  };
  expect(
    extractVariableOptions({ kind: 'tempo-tag-names' }, body).sort(),
  ).toEqual(['host.name', 'http.method', 'service.name']);
});

test('extractVariableOptions: tempo v2 tag values', () => {
  const body = {
    tagValues: [
      { type: 'string', value: 'cerberus' },
      { type: 'string', value: 'sample-app' },
    ],
  };
  expect(
    extractVariableOptions({ kind: 'tempo-tag-values', tag: 't' }, body),
  ).toEqual(['cerberus', 'sample-app']);
});

test('extractVariableOptions: tempo shape mismatch throws', () => {
  expect(() =>
    extractVariableOptions({ kind: 'tempo-tag-names' }, { tagValues: [] }),
  ).toThrow(/no scopes array/);
  expect(() =>
    extractVariableOptions({ kind: 'tempo-tag-values', tag: 't' }, { scopes: [] }),
  ).toThrow(/no tagValues array/);
});
