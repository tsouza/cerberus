/**
 * Stack-free unit tests for the validity-oracle library
 * (helpers/validity.ts). Table-driven: every fixture is a captured-
 * response-shaped literal; valid fixtures must produce zero
 * violations, and each violation class must produce its named
 * violation string. No Grafana / cerberus stack is required — run
 * anywhere via:
 *
 *   cd test/e2e/playwright && npx playwright test helpers-validity.spec.ts
 */

import { expect, test } from '@playwright/test';

import {
  type ValidityContext,
  parseSampleValue,
  validateLokiResponse,
  validatePromResponse,
  validateTempoResponse,
} from './helpers/index.js';

// A fixed window every fixture below stays inside: [1000, 1300] with
// step 15 — i.e. valid matrix timestamps are 1000, 1015, 1030, …
const CTX: ValidityContext = { fromSec: 1000, toSec: 1300, stepSec: 15 };

function promMatrix(
  series: Array<{ metric: Record<string, string>; values: Array<[number, string]> }>,
): unknown {
  return { status: 'success', data: { resultType: 'matrix', result: series } };
}

function promVector(
  series: Array<{ metric: Record<string, string>; value: [number, string] }>,
): unknown {
  return { status: 'success', data: { resultType: 'vector', result: series } };
}

// --- parseSampleValue --------------------------------------------------------

test('parseSampleValue classifies finite, non-finite, and garbage tokens', () => {
  expect(parseSampleValue('1.5')).toEqual({ kind: 'finite', value: 1.5 });
  expect(parseSampleValue('-2e3')).toEqual({ kind: 'finite', value: -2000 });
  expect(parseSampleValue(42)).toEqual({ kind: 'finite', value: 42 });
  expect(parseSampleValue('NaN')).toEqual({ kind: 'nonfinite', token: 'NaN' });
  expect(parseSampleValue('+Inf')).toEqual({ kind: 'nonfinite', token: '+Inf' });
  expect(parseSampleValue('-Inf')).toEqual({ kind: 'nonfinite', token: '-Inf' });
  expect(parseSampleValue('bogus')).toEqual({ kind: 'garbage', token: 'bogus' });
  expect(parseSampleValue('')).toEqual({ kind: 'garbage', token: '' });
  expect(parseSampleValue(null)).toEqual({ kind: 'garbage', token: 'null' });
});

// --- validatePromResponse ----------------------------------------------------

test('prom: a well-formed matrix passes', () => {
  const body = promMatrix([
    {
      metric: { __name__: 'up', job: 'cerberus' },
      values: [
        [1000, '1'],
        [1015, '1'],
        [1300, '0'],
      ],
    },
  ]);
  expect(validatePromResponse(body, CTX)).toEqual([]);
});

test('prom: a well-formed vector passes', () => {
  const body = promVector([
    { metric: { job: 'cerberus' }, value: [1234, '0.5'] },
  ]);
  expect(
    validatePromResponse(body, { ...CTX, expectResultType: 'vector' }),
  ).toEqual([]);
});

test('prom: an empty matrix passes (shows-data is a different oracle level)', () => {
  expect(validatePromResponse(promMatrix([]), CTX)).toEqual([]);
});

test('prom: error envelope produces a status violation', () => {
  const body = {
    status: 'error',
    errorType: 'bad_data',
    error: 'parse error',
  };
  const v = validatePromResponse(body, CTX);
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('status="error"');
  expect(v[0]).toContain('parse error');
});

test('prom: non-object body produces an envelope violation', () => {
  const v = validatePromResponse('not json', CTX);
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('body is not a JSON object');
});

test('prom: resultType mismatch against the endpoint expectation', () => {
  const v = validatePromResponse(promMatrix([]), {
    ...CTX,
    expectResultType: 'vector',
  });
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('resultType "matrix" (want "vector")');
});

test('prom: NaN sample is rejected by default', () => {
  const body = promMatrix([
    { metric: {}, values: [[1000, 'NaN']] },
  ]);
  const v = validatePromResponse(body, CTX);
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('non-finite sample value "NaN"');
});

test('prom: +Inf sample is rejected by default', () => {
  const body = promVector([{ metric: {}, value: [1000, '+Inf'] }]);
  const v = validatePromResponse(body, CTX);
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('non-finite sample value "+Inf"');
});

test('prom: NaN/Inf accepted when ctx.allowNaN is set', () => {
  const body = promMatrix([
    {
      metric: {},
      values: [
        [1000, 'NaN'],
        [1015, '+Inf'],
        [1030, '-Inf'],
      ],
    },
  ]);
  expect(validatePromResponse(body, { ...CTX, allowNaN: true })).toEqual([]);
});

test('prom: garbage value string is rejected even under allowNaN', () => {
  const body = promMatrix([{ metric: {}, values: [[1000, 'wat']] }]);
  const v = validatePromResponse(body, { ...CTX, allowNaN: true });
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('sample value "wat" is not numeric');
});

test('prom: timestamp before fromSec violates the window rule', () => {
  const body = promMatrix([{ metric: {}, values: [[985, '1']] }]);
  const v = validatePromResponse(body, CTX);
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('timestamp 985 outside query window [1000, 1300]');
});

test('prom: timestamp after toSec violates the window rule', () => {
  const body = promVector([{ metric: {}, value: [1301, '1'] }]);
  const v = validatePromResponse(body, CTX);
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('timestamp 1301 outside query window');
});

test('prom: window bounds are inclusive on both ends', () => {
  const body = promMatrix([
    {
      metric: {},
      values: [
        [1000, '1'],
        [1300, '1'],
      ],
    },
  ]);
  expect(validatePromResponse(body, CTX)).toEqual([]);
});

test('prom: matrix timestamp off the step grid is a violation', () => {
  const body = promMatrix([{ metric: {}, values: [[1007, '1']] }]);
  const v = validatePromResponse(body, CTX);
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('not step-aligned to anchor 1000 (step=15s)');
});

test('prom: vector timestamps are not step-checked', () => {
  // Instant queries answer at the evaluation time, not on a grid.
  const body = promVector([{ metric: {}, value: [1007, '1'] }]);
  expect(validatePromResponse(body, CTX)).toEqual([]);
});

test('prom: counter-rate expr class rejects negative values', () => {
  const body = promMatrix([
    {
      metric: {},
      values: [
        [1000, '0.5'],
        [1015, '-0.25'],
      ],
    },
  ]);
  const v = validatePromResponse(body, { ...CTX, exprClass: 'counter-rate' });
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('counter-shaped expression produced negative value -0.25');
});

test('prom: counter-rate expr class accepts zero and positive values', () => {
  const body = promMatrix([
    {
      metric: {},
      values: [
        [1000, '0'],
        [1015, '3.5'],
      ],
    },
  ]);
  expect(
    validatePromResponse(body, { ...CTX, exprClass: 'counter-rate' }),
  ).toEqual([]);
});

test('prom: unconstrained expr class accepts negative values', () => {
  const body = promMatrix([{ metric: {}, values: [[1000, '-3']] }]);
  expect(
    validatePromResponse(body, { ...CTX, exprClass: 'unconstrained' }),
  ).toEqual([]);
});

test('prom: byKeys exact keyset passes when every series matches', () => {
  const body = promMatrix([
    { metric: { cerberus_ql: 'promql' }, values: [[1000, '1']] },
    { metric: { __name__: 'x', cerberus_ql: 'logql' }, values: [[1000, '2']] },
  ]);
  expect(
    validatePromResponse(body, { ...CTX, byKeys: ['cerberus_ql'] }),
  ).toEqual([]);
});

test('prom: byKeys missing key produces a keyset violation', () => {
  const body = promMatrix([
    { metric: { other: 'x' }, values: [[1000, '1']] },
  ]);
  const v = validatePromResponse(body, { ...CTX, byKeys: ['cerberus_ql'] });
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('missing=[cerberus_ql]');
  expect(v[0]).toContain('extra=[other]');
});

test('prom: byKeys extra key produces a keyset violation', () => {
  const body = promMatrix([
    {
      metric: { cerberus_ql: 'promql', instance: 'host-1' },
      values: [[1000, '1']],
    },
  ]);
  const v = validatePromResponse(body, { ...CTX, byKeys: ['cerberus_ql'] });
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('extra=[instance]');
});

test('prom: byKeys ignores __name__ in the keyset comparison', () => {
  const body = promVector([
    { metric: { __name__: 'foo', a: '1', b: '2' }, value: [1000, '1'] },
  ]);
  expect(validatePromResponse(body, { ...CTX, byKeys: ['a', 'b'] })).toEqual([]);
});

test('prom: well-formed histogram bucket frames pass', () => {
  const body = promMatrix([
    {
      metric: { le: '0.1', job: 'a' },
      values: [
        [1000, '1'],
        [1015, '2'],
      ],
    },
    {
      metric: { le: '0.5', job: 'a' },
      values: [
        [1000, '3'],
        [1015, '3'],
      ],
    },
    {
      metric: { le: '+Inf', job: 'a' },
      values: [
        [1000, '4'],
        [1015, '5'],
      ],
    },
  ]);
  expect(validatePromResponse(body, CTX)).toEqual([]);
});

test('prom: duplicate le boundary is a violation', () => {
  const body = promMatrix([
    { metric: { le: '0.5', job: 'a' }, values: [[1000, '1']] },
    { metric: { le: '0.5', job: 'a' }, values: [[1000, '2']] },
  ]);
  const v = validatePromResponse(body, CTX);
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('duplicate le boundary "0.5"');
});

test('prom: cumulative count decreasing across ascending le is a violation', () => {
  const body = promMatrix([
    { metric: { le: '0.1', job: 'a' }, values: [[1000, '5']] },
    { metric: { le: '+Inf', job: 'a' }, values: [[1000, '3']] },
  ]);
  const v = validatePromResponse(body, CTX);
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('cumulative count decreasing at ts=1000');
  expect(v[0]).toContain('le="0.1"');
  expect(v[0]).toContain('le="+Inf"');
});

test('prom: histogram groups are partitioned by the non-le labels', () => {
  // Two different jobs each carry a le=0.5 bucket — NOT a duplicate,
  // and the cumulative rule applies within each group only.
  const body = promMatrix([
    { metric: { le: '0.5', job: 'a' }, values: [[1000, '5']] },
    { metric: { le: '0.5', job: 'b' }, values: [[1000, '1']] },
    { metric: { le: '+Inf', job: 'a' }, values: [[1000, '5']] },
    { metric: { le: '+Inf', job: 'b' }, values: [[1000, '2']] },
  ]);
  expect(validatePromResponse(body, CTX)).toEqual([]);
});

test('prom: violations aggregate rather than throw-first', () => {
  const body = promMatrix([
    {
      metric: {},
      values: [
        [985, 'NaN'],
        [1007, 'wat'],
      ],
    },
  ]);
  const v = validatePromResponse(body, CTX);
  // ts-out-of-window + non-finite + step-misaligned + garbage = 4.
  expect(v.length).toBe(4);
});

test('prom: where prefix lands in every violation string', () => {
  const body = promMatrix([{ metric: {}, values: [[985, '1']] }]);
  const v = validatePromResponse(body, { ...CTX, where: 'panel="QPS"' });
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('panel="QPS"');
});

// --- validateLokiResponse ----------------------------------------------------

function lokiStreams(
  streams: Array<{ stream: Record<string, string>; values: Array<[string, string]> }>,
): unknown {
  return { status: 'success', data: { resultType: 'streams', result: streams } };
}

test('loki: a well-formed streams response passes', () => {
  const body = lokiStreams([
    {
      stream: { service_name: 'cerberus', detected_level: 'info' },
      values: [
        ['1000000000000', 'request served'],
        ['1300000000000', 'another line'],
      ],
    },
  ]);
  expect(validateLokiResponse(body, CTX)).toEqual([]);
});

test('loki: severity outside the vocabulary is a violation', () => {
  const body = lokiStreams([
    {
      stream: { detected_level: 'noisy' },
      values: [['1000000000000', 'x']],
    },
  ]);
  const v = validateLokiResponse(body, CTX);
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('severity "noisy" outside');
});

test('loki: severity matching is case-insensitive across both label names', () => {
  const ok = lokiStreams([
    { stream: { detected_level: 'warn' }, values: [['1000000000000', 'x']] },
    { stream: { SeverityText: 'Error' }, values: [['1000000000000', 'y']] },
    { stream: { SeverityText: 'FATAL' }, values: [['1000000000000', 'z']] },
  ]);
  expect(validateLokiResponse(ok, CTX)).toEqual([]);
});

test('loki: a stream with no severity label passes (label is optional)', () => {
  const body = lokiStreams([
    { stream: { service_name: 'cerberus' }, values: [['1000000000000', 'x']] },
  ]);
  expect(validateLokiResponse(body, CTX)).toEqual([]);
});

test('loki: log timestamp outside the window is a violation', () => {
  const body = lokiStreams([
    { stream: {}, values: [['999999999999', 'too early']] },
  ]);
  const v = validateLokiResponse(body, CTX);
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('outside query window');
});

test('loki: non-nanosecond timestamp string is a violation', () => {
  const body = lokiStreams([{ stream: {}, values: [['abc', 'x']] }]);
  const v = validateLokiResponse(body, CTX);
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('is not a nanosecond string');
});

test('loki: empty log line body is a violation', () => {
  const body = lokiStreams([{ stream: {}, values: [['1000000000000', '']] }]);
  const v = validateLokiResponse(body, CTX);
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('log line body is empty');
});

test('loki: metric matrix results reuse the prom numeric/ts rules', () => {
  const ok = {
    status: 'success',
    data: {
      resultType: 'matrix',
      result: [{ metric: { SeverityText: 'ERROR' }, values: [[1015, '0.2']] }],
    },
  };
  expect(validateLokiResponse(ok, CTX)).toEqual([]);

  const bad = {
    status: 'success',
    data: {
      resultType: 'matrix',
      result: [{ metric: {}, values: [[1007, 'NaN']] }],
    },
  };
  const v = validateLokiResponse(bad, CTX);
  expect(v.length).toBe(2); // non-finite + step-misaligned
  expect(v.join('\n')).toContain('non-finite sample value');
  expect(v.join('\n')).toContain('not step-aligned');
});

test('loki: error envelope produces a status violation', () => {
  const v = validateLokiResponse({ status: 'error', error: 'boom' }, CTX);
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('status="error"');
});

test('loki: unrecognised resultType is a violation', () => {
  const v = validateLokiResponse(
    { status: 'success', data: { resultType: 'wobble', result: [] } },
    CTX,
  );
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('unrecognised loki resultType "wobble"');
});

// --- validateTempoResponse ---------------------------------------------------

test('tempo search: well-formed results pass', () => {
  const body = {
    traces: [
      {
        traceID: '0123456789abcdef0123456789abcdef',
        rootServiceName: 'cerberus',
        durationMs: 12,
        startTimeUnixNano: '1000000000000',
      },
    ],
  };
  expect(validateTempoResponse(body, CTX)).toEqual([]);
});

test('tempo search: empty traces array passes (shows-data is level 1)', () => {
  expect(validateTempoResponse({ traces: [] }, CTX)).toEqual([]);
});

test('tempo search: non-canonical traceID is a violation', () => {
  const cases = [
    'ABCDEF0123456789ABCDEF0123456789', // uppercase
    'abc123', // short / un-padded
    '0123456789abcdef0123456789abcdeg', // non-hex
  ];
  for (const traceID of cases) {
    const v = validateTempoResponse({ traces: [{ traceID }] }, CTX);
    expect(v).toHaveLength(1);
    expect(v[0]).toContain('not 32-lowercase-hex');
  }
});

test('tempo search: negative duration is a violation', () => {
  const body = {
    traces: [
      { traceID: '0123456789abcdef0123456789abcdef', durationMs: -5 },
    ],
  };
  const v = validateTempoResponse(body, CTX);
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('durationMs -5 is negative or non-numeric');
});

test('tempo search: unparseable startTimeUnixNano is a violation', () => {
  const body = {
    traces: [
      {
        traceID: '0123456789abcdef0123456789abcdef',
        startTimeUnixNano: 'soon',
      },
    ],
  };
  const v = validateTempoResponse(body, CTX);
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('does not parse as a positive integer');
});

test('tempo trace-by-id: consistent parent links + kinds pass', () => {
  const body = {
    batches: [
      {
        resource: {},
        scopeSpans: [
          {
            spans: [
              { spanId: 'aaaa', kind: 'SPAN_KIND_SERVER' },
              { spanId: 'bbbb', parentSpanId: 'aaaa', kind: 'SPAN_KIND_CLIENT' },
              { spanId: 'cccc', parentSpanId: 'bbbb', kind: 3 },
            ],
          },
        ],
      },
    ],
  };
  expect(validateTempoResponse(body, CTX)).toEqual([]);
});

test('tempo trace-by-id: orphaned parentSpanId is a violation', () => {
  const body = {
    batches: [
      {
        scopeSpans: [
          {
            spans: [{ spanId: 'aaaa', parentSpanId: 'ffff' }],
          },
        ],
      },
    ],
  };
  const v = validateTempoResponse(body, CTX);
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('parentSpanId "ffff" not present');
});

test('tempo trace-by-id: empty parentSpanId means root span (valid)', () => {
  const body = {
    resourceSpans: [
      { scopeSpans: [{ spans: [{ spanId: 'aaaa', parentSpanId: '' }] }] },
    ],
  };
  expect(validateTempoResponse(body, CTX)).toEqual([]);
});

test('tempo trace-by-id: span kind outside the OTLP enum is a violation', () => {
  const body = {
    batches: [
      { scopeSpans: [{ spans: [{ spanId: 'aaaa', kind: 'SPAN_KIND_BOGUS' }] }] },
    ],
  };
  const v = validateTempoResponse(body, CTX);
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('span kind "SPAN_KIND_BOGUS" outside the OTLP enum');

  const numeric = {
    batches: [{ scopeSpans: [{ spans: [{ spanId: 'aaaa', kind: 7 }] }] }],
  };
  const v2 = validateTempoResponse(numeric, CTX);
  expect(v2).toHaveLength(1);
  expect(v2[0]).toContain('span kind 7 outside the OTLP enum');
});

test('tempo trace-by-id: zero spans is a violation (a trace cannot be empty)', () => {
  const v = validateTempoResponse({ batches: [] }, CTX);
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('no spans found');
});

test('tempo metrics: well-formed series pass', () => {
  const body = {
    series: [
      {
        labels: [{ key: 'resource.service.name', value: { stringValue: 'cerberus' } }],
        samples: [
          { timestampMs: 1000_000, value: 1.5 },
          { timestampMs: 1300_000, value: 0 },
        ],
      },
    ],
  };
  expect(validateTempoResponse(body, CTX)).toEqual([]);
});

test('tempo metrics: timestampMs outside the window is a violation', () => {
  const body = {
    series: [{ samples: [{ timestampMs: 1300_001, value: 1 }] }],
  };
  const v = validateTempoResponse(body, CTX);
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('timestampMs 1300001 outside query window');
});

test('tempo metrics: non-finite value is rejected unless allowNaN', () => {
  const body = {
    series: [{ samples: [{ timestampMs: 1000_000, value: Number.NaN }] }],
  };
  const v = validateTempoResponse(body, CTX);
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('non-finite sample value');
  expect(validateTempoResponse(body, { ...CTX, allowNaN: true })).toEqual([]);
});

test('tempo: unrecognised body shape is a violation', () => {
  const v = validateTempoResponse({ surprise: true }, CTX);
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('unrecognised tempo response shape');
});
