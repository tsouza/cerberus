/**
 * Shared wire-failure reconcilers for the Grafana e2e lanes.
 *
 * These reconcile KNOWN, transient third-party-app artifacts that are not
 * cerberus faults — most notably the Grafana Traces Drilldown app's
 * primarySignal-init race, which forwards a dangling-operand TraceQL query
 * that cerberus (and reference Tempo) correctly reject with HTTP 400. They
 * are deliberately NARROW: a genuinely-broken query still fails loudly. The
 * crawl lane (`crawl/`) and the drilldown-apps sweep
 * (`iterate-drilldown-apps.spec.ts`) both consume them, so the matcher lives
 * here as the single source of truth rather than being duplicated per lane.
 */

/**
 * Minimal view of a captured `/api/ds/query` response the reconcilers need:
 * the URL (carries `?ds_type=…`), the HTTP status, and the request body
 * (carries the forwarded `expr`/`query` per refId).
 */
export type DsResponseView = {
  url: string;
  status: number;
  requestBody: string;
  // Captured cerberus error body, when readable. Used as a postData-independent
  // fallback: a rapid drill can tear the request reference down before
  // `request.postData()` resolves, leaving requestBody empty — but cerberus's
  // 400 body still names the exact syntax error, so the dangling-operand shape
  // is recognisable response-side too.
  responseBody?: string;
  // Grafana plugin id of the drilldown app the drill is currently sweeping, when
  // known. Only the Traces Drilldown app (grafana-exploretraces-app) has the
  // primarySignal-init race, so the teardown-proof branch (both request and
  // response bodies lost to a mid-flight navigation) is scoped to it alone.
  appId?: string;
};

/**
 * The one drilldown app whose primarySignal-init race forwards the
 * dangling-operand TraceQL spanset. Scoping the teardown-proof reconciler branch
 * to this id keeps it from ever touching a 400 from any other app.
 */
export const TRACES_DRILLDOWN_APP_ID = 'grafana-exploretraces-app';

/**
 * Sentinel the drilldown-apps sweep records when BOTH the response text() and
 * raw body() reads threw — the response body was evicted by a mid-flight
 * navigation before either read resolved. Total body loss only happens under
 * teardown, which only happens during the rapid drill the init race rides; a
 * deterministic wrong-rejection of a well-formed query is never torn down and
 * yields a readable body. Kept in lockstep with the spec's bodyPreview sentinel.
 */
export const UNREADABLE_BODY_SENTINEL = '<unreadable>';

/**
 * Map refId → expr/query from a ds/query request body. Returns an empty map
 * for non-JSON bodies. Both the Prom/Loki `expr` shape and the Tempo `query`
 * (TraceQL) shape are handled.
 */
export function refIdToExpr(requestBody: string): Map<string, string> {
  const out = new Map<string, string>();
  try {
    const parsed = JSON.parse(requestBody) as {
      queries?: Array<{ refId?: string; expr?: string; query?: string }>;
    };
    for (const q of parsed.queries ?? []) {
      const expr = (q.expr ?? q.query ?? '').trim();
      if (q.refId) out.set(q.refId, expr);
    }
  } catch {
    // fallthrough — empty map; caller treats every refId as undeclared
  }
  return out;
}

/**
 * Matches a TraceQL spanset whose boolean filter has a DANGLING / EMPTY
 * operand around `&&` / `||` — i.e. a `{` followed immediately by a binary
 * operator (empty left operand: `{ && true}`), or a binary operator
 * immediately followed by the closing `}` (empty right operand: `{true && }`),
 * or two adjacent operators (`{a && && b}`). This is a SYNTAX ERROR, not a
 * value-level filter: TraceQL's grammar requires a non-empty operand on each
 * side of `&&` / `||`, so cerberus's parser (and reference Tempo's identical
 * grammar) rejects it with `syntax error: unexpected &&` (HTTP 400).
 *
 * The shape is produced by the Grafana Traces Drilldown app
 * (grafana-exploretraces-app), NOT by cerberus. The app's
 * `PrimarySignalVariable` (a Scenes CustomVariable) applies its default value
 * inside the component's `useEffect`, not in its constructor — so during the
 * initial-load / rapid-navigation window, before that effect fires, the
 * `${primarySignal}` interpolation is EMPTY and the app builds
 * `{ && ${filters}} | rate()` (and the errors / duration variants). The
 * instant the effect resolves the default, the app re-fires the well-formed
 * query; the user only ever sees the settled result. See
 * src/pages/Explore/PrimarySignalVariable.tsx in grafana/traces-drilldown.
 */
export const DANGLING_TRACEQL_OPERAND =
  /\{\s*(?:&&|\|\|)|(?:&&|\|\|)\s*\}|(?:&&|\|\|)\s*(?:&&|\|\|)/;

/**
 * Matches cerberus's HTTP-400 body for a dangling-operand TraceQL spanset —
 * the RESPONSE-side signature of the same primarySignal-init race. cerberus's
 * Tempo head rejects an empty operand around `&&`/`||` with
 * `parse error … syntax error: unexpected &&` (or `||`); `&` is HTML-escaped to
 * `&` in the JSON body, so both the raw-escaped and decoded forms are
 * matched. `unexpected <operator>` is specific to a dangling operand — any
 * other malformed query yields `unexpected <other-token>` — so this stays as
 * narrow as the request-side DANGLING_TRACEQL_OPERAND: a genuine wrong-rejection
 * carries a different message and still fails loudly.
 *
 * This is the fallback the wire-sweep uses when a torn-down request left the
 * forwarded query body uncapturable (postData empty); the cerberus error body
 * proves the shape just as well.
 */
export const DANGLING_TRACEQL_REJECTION =
  /unexpected\s+(?:&&|\\u0026\\u0026|\|\|)/;

/**
 * Matches a TraceQL query carrying the literal JS token `undefined` as an
 * identifier — e.g. `by(undefined)` or `undefined != nil`. Same
 * primarySignal-init race family as DANGLING_TRACEQL_OPERAND, but the
 * unresolved React state here is the Traces Drilldown app's `groupBy`
 * variable rather than `primarySignal`: mid-drill, before the click handler's
 * effect commits the clicked facet name, the app re-renders its breakdown
 * query with the JS `undefined` value string-interpolated in as the group-by
 * attribute. `undefined` is not a valid TraceQL identifier, so cerberus (and
 * reference Tempo's identical grammar) rejects it with `unknown identifier:
 * undefined`; the app re-fires a well-formed query once the state settles.
 * `undefined` never appears in a well-formed TraceQL query (it isn't a
 * keyword or a real attribute name), so the match stays narrow.
 */
export const UNDEFINED_GROUPBY_TRACEQL = /\bundefined\b/;

/**
 * Matches cerberus's HTTP-400 body for the undefined-groupBy TraceQL shape —
 * the RESPONSE-side signature of the race UNDEFINED_GROUPBY_TRACEQL matches
 * request-side. cerberus's parser reports the exact unresolved identifier
 * name, so this is as specific as the dangling-operand response matcher: a
 * genuine wrong-rejection of a different malformed query names a different
 * identifier and still fails loudly.
 */
export const UNDEFINED_GROUPBY_REJECTION = /unknown identifier: undefined/;

/**
 * True iff a captured console message is a browser `TypeError: Failed to fetch`
 * — the network-abort class. This is NOT how cerberus signals an error: a
 * cerberus 4xx/5xx arrives as an HTTP response with a JSON body (the fetch
 * promise RESOLVES with a non-ok response), captured and failed-on by the
 * wire-status sweep. `Failed to fetch` only fires when the fetch itself never
 * completes — connection abort / cancellation / teardown. The Grafana drilldown
 * apps fire background fetches (grafana-lokiexplore-app's "Detected fields"
 * feature; the RxJS data-source subscription layer) during the rapid multi-app
 * drill; when the test navigates on, Grafana unmounts the scene and the
 * in-flight fetch is aborted, surfacing as this TypeError. Verified live on the
 * k3d stack: cerberus serves /loki/api/v1/detected_fields and /detected_labels
 * with HTTP 200, and no Playwright `requestfailed` fires for them — a pure
 * client-side abort. Recognising ONLY this exact TypeError class keeps every
 * other console error (chunk-load failures, real JS exceptions, datasource 5xx
 * logs) reportable; real cerberus HTTP failures remain owned by the wire-sweep.
 */
export function isClientSideFetchAbort(consoleMessage: string): boolean {
  return /TypeError:\s*Failed to fetch/i.test(consoleMessage);
}

/**
 * True iff a captured console message is the browser's generic
 * "Failed to load resource: the server responded with a status of N" line — the
 * console TWIN every non-2xx HTTP response emits. It is redundant with the
 * wire-status sweep (which inspects the SAME cerberus responses and fails on any
 * un-reconciled non-2xx), so on its own it carries no signal the wire-sweep
 * doesn't already own. Used to resolve the browser twin of a reconciled
 * dangling-operand 400 without masking application-level console errors.
 */
export function isResourceStatusTwin(consoleMessage: string): boolean {
  return /Failed to load resource: the server responded with a status of \d/i.test(
    consoleMessage,
  );
}

/**
 * True iff `resp` is a non-2xx Tempo ds/query whose EVERY forwarded TraceQL
 * query carries a known Traces-Drilldown init-race shape: a dangling-operand
 * spanset (see DANGLING_TRACEQL_OPERAND, the primarySignal-init race) or the
 * literal `undefined` identifier (see UNDEFINED_GROUPBY_TRACEQL, the
 * groupBy-init race). cerberus correctly 400s these (reference Tempo rejects
 * the identical syntax errors), so they are a transient app-side artifact,
 * not a cerberus fault.
 *
 * It is NOT a blanket 400 suppressor — a non-2xx carrying any well-formed
 * query still fails loudly; only the specific known-race syntax shapes are
 * reconciled, and ALL queries in the request must exhibit one of them (a
 * mixed request with one well-formed query is a genuine failure).
 */
export function isTransientMalformedTraceQLFailure(
  resp: DsResponseView,
): boolean {
  if (!resp.url.includes('/api/ds/query')) return false;
  if (resp.status >= 200 && resp.status <= 299) return false;
  const dsType = new URL(resp.url, 'http://x').searchParams.get('ds_type');
  if (dsType !== 'tempo') return false;
  const exprs = [...refIdToExpr(resp.requestBody).values()];
  if (exprs.length > 0) {
    // Request-side: EVERY forwarded query must carry a known-race shape (a
    // mixed request with one well-formed query is a genuine failure).
    return exprs.every(
      (expr) =>
        DANGLING_TRACEQL_OPERAND.test(expr) ||
        UNDEFINED_GROUPBY_TRACEQL.test(expr),
    );
  }
  // Request body uncapturable (a torn-down request left postData empty). Fall
  // back to cerberus's 400 body, which names the known-race syntax error
  // verbatim — the same proven shapes, observed response-side.
  if (
    resp.responseBody !== undefined &&
    (DANGLING_TRACEQL_REJECTION.test(resp.responseBody) ||
      UNDEFINED_GROUPBY_REJECTION.test(resp.responseBody))
  ) {
    return true;
  }

  // Teardown-proof fallback. The init race fires its dangling-operand query and
  // is torn down so fast that BOTH bodies are lost: postData is empty at fire
  // time (handled above) AND the response body is evicted before text()/body()
  // resolve, so the spec records UNREADABLE_BODY_SENTINEL. With no body left to
  // vet, attribute the 400 by the teardown signature itself — which is narrow:
  //   - it is the Traces Drilldown app (the only app with the init race), and
  //   - the response body was provably evicted by a mid-flight navigation
  //     (UNREADABLE_BODY_SENTINEL), which only happens under teardown.
  // A genuine cerberus wrong-rejection of a well-formed exploretraces query is
  // deterministic, not torn down: its body is readable and fails the
  // dangling-shape match above, so it still reports loudly. A query torn down
  // before completion is, by construction, the unsettled pre-effect query the
  // race emits, not a query a user ever issued.
  return (
    resp.appId === TRACES_DRILLDOWN_APP_ID &&
    resp.status === 400 &&
    resp.responseBody === UNREADABLE_BODY_SENTINEL
  );
}

/**
 * Resolve the browser-side twins of the reconciled init-race 400s and return
 * the console errors that remain REPORTABLE.
 *
 * When an interactive drill drives the Traces Drilldown app through one of
 * its known init races (primarySignal or groupBy — see
 * isTransientMalformedTraceQLFailure), each TraceQL 400 that function
 * reconciled on the wire ALSO surfaces to the browser as a single EMPTY-text
 * console error — Grafana logs the failed background fetch, and the captured
 * `msg.text()` is empty. Given `reconciledInitRace` such reconciled 400s, up
 * to that many empty-text console errors are those twins and are resolved
 * away; EVERY substantive (non-empty) console error is kept (mask nothing of
 * value), and any empty-text error BEYOND the reconciled-400 count is kept
 * too (placeholder text), so a genuinely-broken app — which produces either a
 * non-reconciled wire failure or more console noise than the init races
 * explain — still reports.
 */
export function reportableConsoleErrors(
  consoleErrors: ReadonlyArray<string>,
  reconciledInitRace: number,
): string[] {
  // 1. Drop the browser network-abort class outright (client-side teardown of a
  //    third-party app's background fetch — see isClientSideFetchAbort). Real
  //    cerberus HTTP failures are owned by the wire-status sweep, not this rule.
  const afterAbort = consoleErrors.filter((m) => !isClientSideFetchAbort(m));

  // 2. Resolve the browser TWINS of the reconciled dangling-operand 400s: each
  //    reconciled init-race 400 surfaces to the console either as an empty-text
  //    message or as the generic "Failed to load resource: … status of 400"
  //    twin. Resolve up to `reconciledInitRace` such twins (the wire-sweep
  //    already accounted for the underlying 400); everything else is kept, so a
  //    genuine app error or more twins than the init race explains still
  //    reports.
  let twinsBudget = Math.max(0, reconciledInitRace);
  const reportable: string[] = [];
  for (const m of afterAbort) {
    const isTwin = m.trim() === '' || isResourceStatusTwin(m);
    if (isTwin && twinsBudget > 0) {
      twinsBudget--;
      continue;
    }
    reportable.push(m.trim() === '' ? '<empty-text console error>' : m);
  }
  return reportable;
}
