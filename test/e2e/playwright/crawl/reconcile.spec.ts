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
import {
  isClientSideFetchAbort,
  isResourceStatusTwin,
  reportableConsoleErrors,
  TRACES_DRILLDOWN_APP_ID,
  UNREADABLE_BODY_SENTINEL,
} from '../helpers/reconcile.js';

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

  // The groupBy-init race (run 29392064642, reproduced locally 2026-07-15):
  // before the click handler's effect commits the clicked facet name, the
  // breakdown view forwards the JS `undefined` value string-interpolated in
  // as the group-by attribute / a nil-comparison operand.
  test('reconciles the undefined-groupBy breakdown-view race', () => {
    for (const q of [
      '{nestedSetParent<0 && true && undefined != nil} | rate() by(undefined)',
      '{nestedSetParent<0 && true} | rate() by(undefined)',
      '{true && undefined != nil} | rate()',
    ]) {
      expect(isTransientMalformedTraceQLFailure(dsqTempo(q, 400))).toBe(true);
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

  test('does NOT reconcile an empty/unparseable request body (no response body)', () => {
    expect(
      isTransientMalformedTraceQLFailure({
        url: '/api/ds/query?ds_type=tempo&requestId=SQR1',
        status: 400,
        requestBody: '<streamed>',
      }),
    ).toBe(false);
  });

  // Response-side fallback: a rapid drill can tear the request reference down
  // before postData resolves, leaving requestBody empty. cerberus's 400 body
  // names the dangling-operand syntax error verbatim, so the SAME proven shape
  // is recognisable response-side. Live-captured body (k3d, 2026-06-16):
  //   {"error":true,"message":"parse error at line 0, col 3: syntax error: unexpected &&"}
  test('reconciles via the cerberus 400 body when the request body is unavailable', () => {
    for (const body of [
      // raw HTML-escaped `&` (as cerberus emits it on the wire)
      '{"error":true,"message":"parse error at line 0, col 3: syntax error: unexpected \\u0026\\u0026"}',
      // decoded form (defensive — if the harness ever JSON-parses first)
      'parse error: syntax error: unexpected &&',
      // the `||` variant
      'parse error: syntax error: unexpected ||',
    ]) {
      expect(
        isTransientMalformedTraceQLFailure({
          url: '/api/ds/query?ds_type=tempo&requestId=SQR1',
          status: 400,
          requestBody: '<streamed>',
          responseBody: body,
        }),
      ).toBe(true);
    }
  });

  // Response-side fallback for the undefined-groupBy shape. Live-captured
  // body (compose stack, 2026-07-15):
  //   {"results":{"A":{"error":"failed to execute TraceQL query: {nestedSetParent<0 && true && undefined != nil} | rate() by(undefined) Status: 400 Bad Request Body: {\"traceID\":\"\",\"spanID\":\"\",\"error\":true,\"message\":\"parse error at line 1, col 31: unknown identifier: undefined\"}\n","errorSource":"plugin","status":500}}}
  test('reconciles the undefined-groupBy race via the cerberus 400 body', () => {
    expect(
      isTransientMalformedTraceQLFailure({
        url: '/api/ds/query?ds_type=tempo&requestId=SQR108',
        status: 400,
        requestBody: '<streamed>',
        responseBody:
          '{"results":{"A":{"error":"failed to execute TraceQL query: {nestedSetParent<0 && true && undefined != nil} | rate() by(undefined) Status: 400 Bad Request Body: {\\"traceID\\":\\"\\",\\"spanID\\":\\"\\",\\"error\\":true,\\"message\\":\\"parse error at line 1, col 31: unknown identifier: undefined\\"}\\n","errorSource":"plugin","status":500}}}',
      }),
    ).toBe(true);
  });

  test('does NOT reconcile a DIFFERENT syntax error via the response body', () => {
    // A genuine wrong-rejection of some other malformed/valid query carries a
    // different `unexpected <token>` and must still fail loudly.
    for (const body of [
      'parse error: syntax error: unexpected }',
      'parse error: syntax error: unexpected identifier',
      'parse error: syntax error: unexpected (',
      '{"error":true,"message":"internal error"}',
    ]) {
      expect(
        isTransientMalformedTraceQLFailure({
          url: '/api/ds/query?ds_type=tempo&requestId=SQR1',
          status: 400,
          requestBody: '<streamed>',
          responseBody: body,
        }),
      ).toBe(false);
    }
  });

  // Teardown-proof fallback: the rapid drill tore the request down so fast that
  // BOTH bodies were lost — postData empty AND the response body evicted before
  // text()/body() resolved, so the sweep recorded the unreadable sentinel. With
  // no body to vet, the 400 is attributed by the teardown signature itself, but
  // ONLY for the one app that has the init race. Observed: run 28104559120.
  test('reconciles the Traces-Drilldown 400 when BOTH bodies are lost (teardown)', () => {
    expect(
      isTransientMalformedTraceQLFailure({
        url: '/api/ds/query?ds_type=tempo&requestId=SQR108',
        status: 400,
        requestBody: '',
        responseBody: UNREADABLE_BODY_SENTINEL,
        appId: TRACES_DRILLDOWN_APP_ID,
      }),
    ).toBe(true);
  });

  test('teardown fallback is scoped to the Traces-Drilldown app only', () => {
    // Same unreadable-body 400, but a DIFFERENT (or absent) app id: no init-race
    // exists there, so an unreadable 400 is a real failure and must report.
    for (const appId of [
      undefined,
      'grafana-lokiexplore-app',
      'grafana-metricsdrilldown-app',
    ]) {
      expect(
        isTransientMalformedTraceQLFailure({
          url: '/api/ds/query?ds_type=tempo&requestId=SQR108',
          status: 400,
          requestBody: '',
          responseBody: UNREADABLE_BODY_SENTINEL,
          appId,
        }),
      ).toBe(false);
    }
  });

  test('teardown fallback fires ONLY on the unreadable sentinel, not any empty body', () => {
    // A readable-but-empty or partially-captured body is NOT proof of teardown,
    // so the narrow branch must not fire — only the exact sentinel the sweep
    // records when both reads threw counts as teardown-proof.
    for (const responseBody of [undefined, '', '{}', 'some other error']) {
      expect(
        isTransientMalformedTraceQLFailure({
          url: '/api/ds/query?ds_type=tempo&requestId=SQR108',
          status: 400,
          requestBody: '',
          responseBody,
          appId: TRACES_DRILLDOWN_APP_ID,
        }),
      ).toBe(false);
    }
  });

  test('teardown fallback does NOT fire on a non-400 unreadable response', () => {
    // A 500/502 with an unreadable body is a real server failure, not the
    // parse-time init-race 400 — it must still report.
    for (const status of [500, 502, 503]) {
      expect(
        isTransientMalformedTraceQLFailure({
          url: '/api/ds/query?ds_type=tempo&requestId=SQR108',
          status,
          requestBody: '',
          responseBody: UNREADABLE_BODY_SENTINEL,
          appId: TRACES_DRILLDOWN_APP_ID,
        }),
      ).toBe(false);
    }
  });
});

/**
 * isClientSideFetchAbort — the lokiexplore "Detected fields" / RxJS
 * network-abort class (k3d dashboard-shard, 2026-06-16; ~60% of runs).
 *
 * A browser `TypeError: Failed to fetch` is a CLIENT-SIDE abort — the fetch
 * never completed (teardown of a third-party app's background fetch as the
 * rapid drill unmounts its scene). cerberus errors are HTTP non-2xx (a resolved
 * fetch the wire-sweep owns). Verified live: cerberus serves
 * /loki/api/v1/detected_fields + /detected_labels with HTTP 200, and no
 * Playwright requestfailed fires for them.
 */
test.describe('isClientSideFetchAbort', () => {
  test('matches the network-abort TypeError (with + without app context)', () => {
    expect(isClientSideFetchAbort('TypeError: Failed to fetch')).toBe(true);
    expect(
      isClientSideFetchAbort(
        'TypeError: Failed to fetch {app: grafana-lokiexplore-app, version: 2.1.2, msg: Detected fields error}',
      ),
    ).toBe(true);
  });

  test('does NOT match real application errors', () => {
    for (const m of [
      'ChunkLoadError: Loading chunk 3399 failed',
      'TypeError: x is undefined',
      'Failed to load resource: the server responded with a status of 500',
      'Error: panic rendering scene',
    ]) {
      expect(isClientSideFetchAbort(m)).toBe(false);
    }
  });
});

test.describe('isResourceStatusTwin', () => {
  test('matches the browser non-2xx resource twin', () => {
    expect(
      isResourceStatusTwin(
        'Failed to load resource: the server responded with a status of 400 (Bad Request)',
      ),
    ).toBe(true);
  });

  test('does NOT match a real application error', () => {
    expect(isResourceStatusTwin('ChunkLoadError: boom')).toBe(false);
    expect(isResourceStatusTwin('TypeError: Failed to fetch')).toBe(false);
  });
});

test.describe('reportableConsoleErrors', () => {
  test('resolves one empty-text twin per reconciled init-race 400', () => {
    // The observed flake: 1 reconciled dangling-query 400 → 1 empty-text
    // console error. Fully explained → nothing reportable.
    expect(reportableConsoleErrors([''], 1)).toEqual([]);
    expect(reportableConsoleErrors(['', ''], 2)).toEqual([]);
  });

  test('keeps empty-text errors BEYOND the reconciled-400 count', () => {
    // 2 empty errors but only 1 reconciled 400 → 1 stays reportable.
    expect(reportableConsoleErrors(['', ''], 1)).toEqual([
      '<empty-text console error>',
    ]);
  });

  test('NEVER masks a substantive console error', () => {
    // A real, non-empty console error always reports, even when an
    // init-race 400 was reconciled in the same window.
    expect(reportableConsoleErrors(['ChunkLoadError: boom'], 1)).toEqual([
      'ChunkLoadError: boom',
    ]);
    expect(
      reportableConsoleErrors(['', 'TypeError: x is undefined'], 1),
    ).toEqual(['TypeError: x is undefined']);
  });

  test('with zero reconciled races, every console error reports', () => {
    expect(reportableConsoleErrors([''], 0)).toEqual([
      '<empty-text console error>',
    ]);
    expect(reportableConsoleErrors(['boom'], 0)).toEqual(['boom']);
  });

  test('whitespace-only errors count as empty (twins), not substantive', () => {
    expect(reportableConsoleErrors(['   ', '\n'], 2)).toEqual([]);
  });

  test('a clean window stays clean', () => {
    expect(reportableConsoleErrors([], 0)).toEqual([]);
    expect(reportableConsoleErrors([], 3)).toEqual([]);
  });

  test('drops the client-side fetch-abort class (lokiexplore teardown race)', () => {
    expect(reportableConsoleErrors(['TypeError: Failed to fetch'], 0)).toEqual(
      [],
    );
    expect(
      reportableConsoleErrors(
        [
          'TypeError: Failed to fetch {app: grafana-lokiexplore-app, version: 2.1.2, msg: Detected fields error}',
        ],
        0,
      ),
    ).toEqual([]);
  });

  test('keeps a real error sitting next to a fetch-abort', () => {
    expect(
      reportableConsoleErrors(
        ['TypeError: Failed to fetch', 'ChunkLoadError: boom'],
        0,
      ),
    ).toEqual(['ChunkLoadError: boom']);
  });

  test('resolves the resource-status twin of a reconciled 400, up to budget', () => {
    // 1 reconciled dangling-operand 400 → its browser twin
    // "Failed to load resource: … status of 400" is resolved.
    expect(
      reportableConsoleErrors(
        ['Failed to load resource: the server responded with a status of 400 (Bad Request)'],
        1,
      ),
    ).toEqual([]);
  });

  test('keeps a resource-status twin BEYOND the reconciled-400 budget', () => {
    // 2 twins but only 1 reconciled 400 → 1 stays reportable (a real,
    // un-reconciled non-2xx still surfaces here as well as in the wire-sweep).
    const twin =
      'Failed to load resource: the server responded with a status of 400 (Bad Request)';
    expect(reportableConsoleErrors([twin, twin], 1)).toEqual([twin]);
  });

  test('a non-400 resource twin with no reconciled race still reports', () => {
    // A 500 resource twin with zero reconciled races is NOT resolved — it is a
    // real failure twin (also caught by the wire-sweep).
    const twin500 =
      'Failed to load resource: the server responded with a status of 500';
    expect(reportableConsoleErrors([twin500], 0)).toEqual([twin500]);
  });
});
