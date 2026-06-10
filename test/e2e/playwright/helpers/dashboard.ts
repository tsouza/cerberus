/**
 * Dashboard enumeration + panel flattening helpers.
 *
 * These wrap Grafana's HTTP API:
 *   - GET /api/search?type=dash-db          → list of dashboard summaries
 *   - GET /api/dashboards/uid/<uid>         → full dashboard JSON
 *
 * The full JSON is what carries `panels[]` (with their `targets[]` and
 * `gridPos`) and `templating.list[]` (the variable matrix). The
 * /api/search response only carries the title + uid.
 *
 * Grafana version pinned in `docker-compose.yml` at the time of writing
 * is `grafana/grafana:12.2.9`. The panel schema shape (rows containing
 * nested panels[]) is unchanged from 11.x through 12.x; if/when the
 * compose stack bumps Grafana, audit this file for schema drift and
 * update the spec phases in the same PR. See helpers/README.md for the
 * version-bump checklist.
 */

import type { APIRequestContext } from '@playwright/test';

export type DashboardEntry = {
  uid: string;
  title: string;
  type: string;
};

export type PanelTarget = {
  refId: string;
  expr?: string; // PromQL / LogQL expression
  query?: string; // TraceQL / generic query string
  datasource?: { type: string; uid: string };
  legendFormat?: string;
  queryType?: string;
};

export type Panel = {
  id: number;
  title: string;
  type: string;
  datasource?: { type: string; uid: string };
  targets: PanelTarget[];
  gridPos: { x: number; y: number; w: number; h: number };
  /**
   * Raw cerberus.expect contract block, forwarded verbatim from the
   * dashboard JSON (Grafana persists unknown panel fields). Parsed on
   * demand via readPanelExpectation — see helpers/expectations.ts.
   */
  cerberus?: unknown;
};

export type TemplateVariable = {
  name: string;
  type: string;
  current?: { value: string | string[] };
};

export type Dashboard = {
  uid: string;
  title: string;
  templating: { list: TemplateVariable[] };
  panels: Panel[];
};

/**
 * Fetch every provisioned dashboard's full JSON. Returns the
 * `Dashboard` shape (panels already flattened — rows unwrapped).
 *
 * `grafanaURL` is the Grafana base URL (e.g. http://localhost:3000),
 * not a path. An anonymous-auth probe is assumed (the compose stack
 * disables auth); when running against an authed Grafana, the caller
 * is responsible for configuring `APIRequestContext` with the right
 * headers — pass `request` from the Playwright fixture, do not
 * construct a bare fetcher here.
 */
export async function iterateDashboards(
  request: APIRequestContext,
  grafanaURL: string,
): Promise<Dashboard[]> {
  const searchResp = await request.get(`${grafanaURL}/api/search?type=dash-db`);
  if (searchResp.status() < 200 || searchResp.status() > 299) {
    throw new Error(
      `iterateDashboards: /api/search → ${searchResp.status()}`,
    );
  }
  const entries = (await searchResp.json()) as DashboardEntry[];

  const dashboards: Dashboard[] = [];
  for (const entry of entries) {
    const detailResp = await request.get(
      `${grafanaURL}/api/dashboards/uid/${entry.uid}`,
    );
    if (detailResp.status() < 200 || detailResp.status() > 299) {
      throw new Error(
        `iterateDashboards: /api/dashboards/uid/${entry.uid} → ${detailResp.status()}`,
      );
    }
    const detail = (await detailResp.json()) as {
      dashboard?: {
        uid?: string;
        title?: string;
        templating?: { list?: TemplateVariable[] };
        panels?: RawPanel[];
      };
    };
    const dash = detail.dashboard;
    if (!dash || !dash.uid) {
      throw new Error(
        `iterateDashboards: /api/dashboards/uid/${entry.uid} returned no dashboard body`,
      );
    }
    dashboards.push({
      uid: dash.uid,
      title: dash.title ?? entry.title,
      templating: { list: dash.templating?.list ?? [] },
      panels: flattenRawPanels(dash.panels ?? []),
    });
  }
  return dashboards;
}

/**
 * Iterate every panel of a single dashboard, in the order Grafana
 * returns them. Rows are already unwrapped by `iterateDashboards`, so
 * this is just `dashboard.panels`. The function exists primarily so
 * callers can write `for (const p of iteratePanels(d))` without
 * reaching into the JSON shape.
 */
export function iteratePanels(dashboard: Dashboard): Panel[] {
  return dashboard.panels;
}

/** Internal raw shape — Grafana 11.x dashboard JSON. */
type RawPanel = {
  id?: number;
  title?: string;
  type?: string;
  datasource?: { type?: string; uid?: string } | string;
  targets?: Array<{
    refId?: string;
    expr?: string;
    query?: string;
    datasource?: { type?: string; uid?: string } | string;
    legendFormat?: string;
    queryType?: string;
  }>;
  gridPos?: { x?: number; y?: number; w?: number; h?: number };
  panels?: RawPanel[]; // for rows
  cerberus?: unknown;
};

function flattenRawPanels(raw: RawPanel[]): Panel[] {
  const out: Panel[] = [];
  for (const p of raw) {
    if (p.type === 'row') {
      // Grafana 11.x rows nest their contents under `panels[]`.
      out.push(...flattenRawPanels(p.panels ?? []));
      continue;
    }
    out.push(normalisePanel(p));
  }
  return out;
}

function normalisePanel(p: RawPanel): Panel {
  return {
    id: p.id ?? 0,
    title: p.title ?? '<untitled>',
    type: p.type ?? 'unknown',
    datasource: normaliseDatasource(p.datasource),
    targets: (p.targets ?? []).map((t) => ({
      refId: t.refId ?? '',
      expr: t.expr,
      query: t.query,
      datasource: normaliseDatasource(t.datasource),
      legendFormat: t.legendFormat,
      queryType: t.queryType,
    })),
    gridPos: {
      x: p.gridPos?.x ?? 0,
      y: p.gridPos?.y ?? 0,
      w: p.gridPos?.w ?? 0,
      h: p.gridPos?.h ?? 0,
    },
    cerberus: p.cerberus,
  };
}

function normaliseDatasource(
  ds: { type?: string; uid?: string } | string | undefined,
): { type: string; uid: string } | undefined {
  if (ds === undefined) return undefined;
  if (typeof ds === 'string') {
    // Pre-Grafana-10 dashboards encoded the datasource as a bare uid
    // string. Surface that as { type: '', uid: <string> } so downstream
    // classification logic can still key off the uid.
    return { type: '', uid: ds };
  }
  return { type: ds.type ?? '', uid: ds.uid ?? '' };
}
