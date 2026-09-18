/**
 * Grafana `/api/ds/query` request ↔ provisioned-panel matching.
 *
 * A dashboard sweep sees ds/query traffic as opaque POSTs; to say WHICH
 * panel a response belongs to it must read the request Grafana sent,
 * not guess from the response body. Grafana's POST body carries the
 * panel's own targets verbatim (`queries[].expr` / `.query`, `.refId`,
 * `.datasource.uid`), so a request is attributed to a panel by exact
 * expression match against the provisioned dashboard JSON. Substring
 * matching on the RESPONSE body (e.g. "does it mention `cerberus_ql`")
 * is not attribution: several panels on the same board share a
 * group-by key, so one panel's failure gets filed under another's
 * name and a named panel's assertions can be satisfied by a sibling.
 *
 * Pure functions, unit-tested in helpers.spec.ts.
 */

import type { Dashboard, Panel } from './dashboard.js';

/** One query of a Grafana `/api/ds/query` POST body (the subset read). */
export type DsQueryRequestQuery = {
  refId: string;
  expr: string;
  datasourceUid: string;
};

type RawDsQueryBody = {
  queries?: Array<{
    refId?: string;
    expr?: string;
    query?: string;
    datasource?: { uid?: string } | string;
  }>;
};

/**
 * Parse the queries out of a ds/query POST body. A body that is not
 * JSON, or carries no `queries` array, yields an empty list — the
 * caller then knows the request attributes to NO panel, which is a
 * distinct answer from "the wrong panel".
 */
export function parseDsQueryRequest(postData: string | null | undefined): DsQueryRequestQuery[] {
  if (!postData) return [];
  let parsed: RawDsQueryBody;
  try {
    parsed = JSON.parse(postData) as RawDsQueryBody;
  } catch {
    return [];
  }
  const queries = Array.isArray(parsed.queries) ? parsed.queries : [];
  const out: DsQueryRequestQuery[] = [];
  for (const q of queries) {
    const expr = (q.expr ?? q.query ?? '').trim();
    if (expr === '') continue;
    const ds = q.datasource;
    const datasourceUid =
      typeof ds === 'string' ? ds : ds?.uid ?? '';
    out.push({ refId: q.refId ?? '', expr, datasourceUid });
  }
  return out;
}

/** The trimmed expressions a panel's targets carry (`expr` or `query`). */
export function panelExprs(panel: Panel): string[] {
  return panel.targets
    .map((t) => (t.expr ?? t.query ?? '').trim())
    .filter((e) => e !== '');
}

/**
 * True iff the request carries at least one query whose expression is
 * exactly one of the panel's target expressions.
 */
export function dsQueryRequestMatchesPanel(
  queries: ReadonlyArray<DsQueryRequestQuery>,
  panel: Panel,
): boolean {
  const exprs = new Set(panelExprs(panel));
  return queries.some((q) => exprs.has(q.expr));
}

/**
 * The provisioned panel a ds/query request belongs to, by exact
 * expression match against the dashboard's panels, or `undefined` when
 * no panel's targets contain any of the request's expressions. When
 * two panels share an identical expression the FIRST in panel order
 * wins — the same ambiguity a reader of the board has, and one the
 * caller can pin on separately if it matters.
 */
export function panelForDsQueryRequest(
  dashboard: Dashboard,
  queries: ReadonlyArray<DsQueryRequestQuery>,
): Panel | undefined {
  return dashboard.panels.find((p) => dsQueryRequestMatchesPanel(queries, p));
}

/**
 * Grafana's own panel-title testid: the panel chrome's container carries
 * `data-testid="data-testid Panel header <title>"` (the literal
 * "data-testid " prefix is part of the value — @grafana/e2e-selectors'
 * flattening convention). The title is recovered by stripping the prefix
 * from the nearest ancestor that carries it.
 */
export const PANEL_HEADER_TESTID_PREFIX = 'data-testid Panel header ';
