/**
 * Unit coverage for the ds/query supersession reconciler in lib.ts.
 *
 * Pins the contract that drew the 2026-06-14 compose-smoke crawl
 * failure (run 27496476980): the Grafana Explore Traces "duration"
 * surface fires a burst of TraceQL metrics queries on load
 * (`{nestedSetParent<0 && true} | rate()`,
 * `… | quantile_over_time(duration, 0.9)`, `… | histogram_over_time(duration)`)
 * and, being a Scenes app, ABORTS each older in-flight request the
 * instant a variable resolves and a newer one supersedes it. The
 * aborted request surfaces to the browser as a transient
 * `plugin.requestFailureError` 500 (cerberus saw `context canceled`),
 * but the panel renders the newer request's result — the user never
 * sees a failure. cerberus itself returns a correct 200 for every one
 * of those queries (verified against chDB AND real ClickHouse).
 *
 * The reconciler resolves these superseded ghosts the same way the DOM
 * oracle already does (it inspects only the final rendered state):
 * last-write-wins. A non-2xx whose exact query signature also succeeded
 * (2xx) somewhere in the same capture window is a ghost and is
 * suppressed. A non-2xx with NO successful sibling is a genuine
 * failure and is NEVER suppressed — this is reconciliation, not an
 * escape hatch.
 *
 * Pure logic; no browser. Runs under CRAWL_STACK in the compose-smoke
 * and dashboard jobs alongside the crawl engine.
 */

import { expect, test } from '@playwright/test';

import {
  dsQuerySignature,
  isSupersededDsQueryFailure,
  isTransientMalformedTraceQLFailure,
  refIdToExpr,
  succeededDsQuerySignatures,
  type DsResponseView,
} from './lib.js';

const tempoBody = (query: string, refId = 'A'): string =>
  JSON.stringify({ queries: [{ refId, query, datasource: { type: 'tempo' } }] });

const promBody = (expr: string, refId = 'A'): string =>
  JSON.stringify({ queries: [{ refId, expr, datasource: { type: 'prometheus' } }] });

const dur = '{nestedSetParent<0 && true} | quantile_over_time(duration, 0.9)';
const rate = '{nestedSetParent<0 && true} | rate()';

test.describe('refIdToExpr', () => {
  test('reads the Tempo `query` (TraceQL) field', () => {
    expect(refIdToExpr(tempoBody(dur))).toEqual(new Map([['A', dur]]));
  });

  test('reads the Prom/Loki `expr` field', () => {
    expect(refIdToExpr(promBody('up'))).toEqual(new Map([['A', 'up']]));
  });

  test('returns an empty map for a non-JSON body', () => {
    expect(refIdToExpr('<streamed>')).toEqual(new Map());
  });
});

test.describe('dsQuerySignature', () => {
  test('ignores the per-request requestId nonce', () => {
    const a: DsResponseView = {
      url: '/api/ds/query?ds_type=tempo&requestId=SQR105',
      status: 500,
      requestBody: tempoBody(rate),
    };
    const b: DsResponseView = {
      url: '/api/ds/query?ds_type=tempo&requestId=SQR222',
      status: 200,
      requestBody: tempoBody(rate),
    };
    expect(dsQuerySignature(a)).toBe(dsQuerySignature(b));
  });

  test('distinguishes different queries on the same datasource', () => {
    const a: DsResponseView = {
      url: '/api/ds/query?ds_type=tempo&requestId=SQR1',
      status: 200,
      requestBody: tempoBody(rate),
    };
    const b: DsResponseView = {
      url: '/api/ds/query?ds_type=tempo&requestId=SQR2',
      status: 200,
      requestBody: tempoBody(dur),
    };
    expect(dsQuerySignature(a)).not.toBe(dsQuerySignature(b));
  });

  test('distinguishes the same query text across datasource types', () => {
    const tempo: DsResponseView = {
      url: '/api/ds/query?ds_type=tempo',
      status: 200,
      requestBody: tempoBody('up'),
    };
    const prom: DsResponseView = {
      url: '/api/ds/query?ds_type=prometheus',
      status: 200,
      requestBody: promBody('up'),
    };
    expect(dsQuerySignature(tempo)).not.toBe(dsQuerySignature(prom));
  });
});

test.describe('supersession reconciliation', () => {
  test('suppresses a superseded 500 that later succeeded (the crawl flake)', () => {
    // Exactly the run-27496476980 shape: the same duration-metric query
    // is aborted (500) once, then re-fired and answered 200.
    const captured: DsResponseView[] = [
      {
        url: '/api/ds/query?ds_type=tempo&requestId=SQR105',
        status: 500,
        requestBody: tempoBody(dur),
      },
      {
        url: '/api/ds/query?ds_type=tempo&requestId=SQR109',
        status: 500,
        requestBody: tempoBody(rate),
      },
      {
        url: '/api/ds/query?ds_type=tempo&requestId=SQR210',
        status: 200,
        requestBody: tempoBody(dur),
      },
      {
        url: '/api/ds/query?ds_type=tempo&requestId=SQR214',
        status: 200,
        requestBody: tempoBody(rate),
      },
    ];
    const sigs = succeededDsQuerySignatures(captured);
    for (const resp of captured.filter((r) => r.status >= 400)) {
      expect(isSupersededDsQueryFailure(resp, sigs)).toBe(true);
    }
  });

  test('does NOT suppress a 500 with no successful sibling (real bug)', () => {
    const captured: DsResponseView[] = [
      {
        url: '/api/ds/query?ds_type=tempo&requestId=SQR1',
        status: 500,
        requestBody: tempoBody(dur),
      },
      // A DIFFERENT query succeeded — does not rescue the broken one.
      {
        url: '/api/ds/query?ds_type=tempo&requestId=SQR2',
        status: 200,
        requestBody: tempoBody(rate),
      },
    ];
    const sigs = succeededDsQuerySignatures(captured);
    const broken = captured[0];
    expect(isSupersededDsQueryFailure(broken, sigs)).toBe(false);
  });

  test('order-independent: an earlier 200 also rescues a later 500', () => {
    const captured: DsResponseView[] = [
      {
        url: '/api/ds/query?ds_type=tempo&requestId=SQR1',
        status: 200,
        requestBody: tempoBody(dur),
      },
      {
        url: '/api/ds/query?ds_type=tempo&requestId=SQR2',
        status: 500,
        requestBody: tempoBody(dur),
      },
    ];
    const sigs = succeededDsQuerySignatures(captured);
    expect(isSupersededDsQueryFailure(captured[1], sigs)).toBe(true);
  });

  test('a non-ds/query non-2xx is never treated as superseded', () => {
    const resp: DsResponseView = {
      url: '/api/dashboards/uid/abc',
      status: 500,
      requestBody: '',
    };
    expect(isSupersededDsQueryFailure(resp, new Set())).toBe(false);
  });
});

/**
 * isTransientMalformedTraceQLFailure — the Traces Drilldown
 * primarySignal-init race (compose-smoke crawl run 27527911418).
 *
 * The app's PrimarySignalVariable applies its default inside the
 * component's useEffect, not its constructor, so during the
 * initial-load / rapid-nav window `${primarySignal}` interpolates to
 * EMPTY and the app forwards a dangling-operand spanset
 * `{ && ${filters}} | rate()` (and the errors / duration variants).
 * That is a TraceQL SYNTAX ERROR — cerberus 400s it with
 * `unexpected &&`, exactly as reference Tempo's identical grammar
 * does. The malformed expr differs from the settled one, so it has no
 * 2xx sibling and the supersession reconciler can't resolve it; this
 * reconciler keys on the malformed shape instead. Verified live on
 * the compose stack: `GET /api/metrics/query_range?q={ && true} |
 * rate()` → HTTP 400; the settled `{true && true} | rate() by(…)` →
 * HTTP 200.
 */
const dsqTempo = (
  query: string,
  status: number,
  requestId = 'SQR108',
): DsResponseView => ({
  url: `/api/ds/query?ds_type=tempo&requestId=${requestId}`,
  status,
  requestBody: tempoBody(query),
});

test.describe('isTransientMalformedTraceQLFailure', () => {
  test('reconciles the empty-primarySignal leading-`&&` rate query', () => {
    expect(
      isTransientMalformedTraceQLFailure(dsqTempo('{ && true} | rate()', 400)),
    ).toBe(true);
  });

  test('reconciles the errors + duration init-race variants', () => {
    for (const q of [
      '{ && true && status=error} | rate()',
      '{ && true} | quantile_over_time(duration, 0.9)',
      '{ && true} | rate() by(resource.service.version)',
    ]) {
      expect(isTransientMalformedTraceQLFailure(dsqTempo(q, 400))).toBe(true);
    }
  });

  test('reconciles `||` and trailing-operand dangling shapes', () => {
    for (const q of [
      '{ || true} | rate()',
      '{true && } | rate()',
      '{true && && false} | rate()',
    ]) {
      expect(isTransientMalformedTraceQLFailure(dsqTempo(q, 400))).toBe(true);
    }
  });

  test('does NOT reconcile a well-formed TraceQL 400 (real wrong-rejection)', () => {
    // A genuine cerberus wrong-rejection of a valid query must still
    // fail loudly — this reconciler is not a blanket 400 suppressor.
    for (const q of [
      '{true && true} | rate()',
      '{nestedSetParent<0 && true} | rate() by(resource.service.version)',
      '{true && true && resource.service.version != nil} | rate()',
      '{kind=server && true} | quantile_over_time(duration, 0.9)',
    ]) {
      expect(isTransientMalformedTraceQLFailure(dsqTempo(q, 400))).toBe(false);
    }
  });

  test('does NOT reconcile when one query in the request is well-formed', () => {
    const mixed: DsResponseView = {
      url: '/api/ds/query?ds_type=tempo&requestId=SQR1',
      status: 400,
      requestBody: JSON.stringify({
        queries: [
          { refId: 'A', query: '{ && true} | rate()', datasource: { type: 'tempo' } },
          { refId: 'B', query: '{true && true} | rate()', datasource: { type: 'tempo' } },
        ],
      }),
    };
    expect(isTransientMalformedTraceQLFailure(mixed)).toBe(false);
  });

  test('does NOT reconcile a 2xx, a non-ds/query, or a non-tempo ds_type', () => {
    expect(
      isTransientMalformedTraceQLFailure(dsqTempo('{ && true} | rate()', 200)),
    ).toBe(false);
    expect(
      isTransientMalformedTraceQLFailure({
        url: '/api/dashboards/uid/abc',
        status: 400,
        requestBody: '',
      }),
    ).toBe(false);
    expect(
      isTransientMalformedTraceQLFailure({
        url: '/api/ds/query?ds_type=prometheus&requestId=SQR1',
        status: 400,
        requestBody: promBody('{ && up'),
      }),
    ).toBe(false);
  });

  test('does NOT reconcile an empty/unparseable request body', () => {
    expect(
      isTransientMalformedTraceQLFailure({
        url: '/api/ds/query?ds_type=tempo&requestId=SQR1',
        status: 400,
        requestBody: '<streamed>',
      }),
    ).toBe(false);
  });
});
