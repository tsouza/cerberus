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
};

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
 * True iff `resp` is a non-2xx Tempo ds/query whose EVERY forwarded TraceQL
 * query carries a dangling-operand spanset (see DANGLING_TRACEQL_OPERAND) —
 * the Traces Drilldown app's primarySignal-init-race shape. cerberus correctly
 * 400s these (reference Tempo rejects the identical syntax-error), so they are
 * a transient app-side artifact, not a cerberus fault.
 *
 * It is NOT a blanket 400 suppressor — a non-2xx carrying any well-formed
 * query still fails loudly; only the specific dangling-`&&`/`||` syntax shape
 * the app's known init race emits is reconciled, and ALL queries in the
 * request must exhibit it (a mixed request with one well-formed query is a
 * genuine failure).
 */
export function isTransientMalformedTraceQLFailure(
  resp: DsResponseView,
): boolean {
  if (!resp.url.includes('/api/ds/query')) return false;
  if (resp.status >= 200 && resp.status <= 299) return false;
  const dsType = new URL(resp.url, 'http://x').searchParams.get('ds_type');
  if (dsType !== 'tempo') return false;
  const exprs = [...refIdToExpr(resp.requestBody).values()];
  if (exprs.length === 0) return false;
  return exprs.every((expr) => DANGLING_TRACEQL_OPERAND.test(expr));
}

/**
 * Resolve the browser-side twins of the reconciled primarySignal-init-race
 * 400s and return the console errors that remain REPORTABLE.
 *
 * When an interactive drill drives the Traces Drilldown app through its
 * primarySignal-init race, each dangling-operand TraceQL 400 that
 * isTransientMalformedTraceQLFailure reconciled on the wire ALSO surfaces to
 * the browser as a single EMPTY-text console error — Grafana logs the failed
 * background fetch, and the captured `msg.text()` is empty. Given
 * `reconciledInitRace` such reconciled 400s, up to that many empty-text console
 * errors are those twins and are resolved away; EVERY substantive (non-empty)
 * console error is kept (mask nothing of value), and any empty-text error
 * BEYOND the reconciled-400 count is kept too (placeholder text), so a
 * genuinely-broken app — which produces either a non-reconciled wire failure or
 * more console noise than the init race explains — still reports.
 */
export function reportableConsoleErrors(
  consoleErrors: ReadonlyArray<string>,
  reconciledInitRace: number,
): string[] {
  const substantive = consoleErrors.filter((m) => m.trim() !== '');
  const emptyCount = consoleErrors.length - substantive.length;
  const unexplainedEmpty = Math.max(0, emptyCount - Math.max(0, reconciledInitRace));
  return [
    ...substantive,
    ...Array.from(
      { length: unexplainedEmpty },
      () => '<empty-text console error>',
    ),
  ];
}
