/**
 * Comprehensive dashboard sweep — every provisioned dashboard, every
 * panel, every target.
 *
 * This is the safety net for the rich-observability rollout: as new
 * dashboards land under `test/e2e/grafana/compose/dashboards/`, the
 * spec auto-discovers them by reading the provisioning directory at
 * spec-build time and asserting each panel renders against REAL data
 * (no synthetic seeds). When the rich-observability PR ships the
 * clickhouse / otelcol / host dashboards, this sweep starts covering
 * them on the next CI run.
 *
 * For each dashboard we:
 *   1. Navigate to `/d/<uid>` and let Grafana render the panels.
 *   2. For each panel target with a non-empty `expr`, fire the query
 *      through the datasource proxy
 *      (`/api/datasources/proxy/uid/<uid>/api/v1/query_range` for Prom,
 *       `/loki/api/v1/query_range` for Loki, `/api/search` for Tempo)
 *      and assert:
 *        - HTTP status 200.
 *        - Response envelope has no top-level `error` / `errorType` /
 *          `message` field.
 *        - The result envelope carries at least one series / stream /
 *          trace. An empty result is a real bug to fix at the source
 *          (cerberus code, seed, dashboard, or panel) — not a state
 *          to mask.
 *      Panels whose datasource type isn't one of {prometheus, loki,
 *      tempo} are excluded from the probe loop.
 *   3. Capture every Grafana → datasource fetch fired during the page
 *      navigation (the `/api/ds/query` proxy + `/api/datasources/proxy/uid/`
 *      + `/api/datasources/uid/<uid>/resources/` shapes). Assert HTTP
 *      status was 2xx on every one, and that no `/api/ds/query`
 *      response body carries a per-target error in
 *      `.results.<refId>.error`.
 *
 * Dashboard catalog assertion: the spec reads
 * `test/e2e/grafana/compose/dashboards/*.json` at runtime, so it
 * automatically picks up any new dashboard the maintainer drops in. It
 * asserts at least one dashboard exists (the catalog is non-empty),
 * NOT a hardcoded count — so the rich-observability PR can land
 * independently and this spec absorbs the new dashboards without an
 * edit. The spec emits an info log listing the discovered set so the
 * CI run record makes the catalog visible.
 *
 * Env:
 *   GRAFANA_URL       default http://localhost:3000
 *   GRAFANA_BASE_URL  honoured as fallback for parity with the rest of
 *                     the compose-smoke specs.
 *   CERBERUS_URL      default http://localhost:8080
 */

import { readFileSync, readdirSync } from 'node:fs';
import { join, resolve } from 'node:path';
import {
  expect,
  test,
  type APIRequestContext,
  type Page,
  type Response,
} from '@playwright/test';

import {
  type VariableJSON,
  checkDashboardVariable,
  describeSweepDepth,
  enforceExpectation,
  generateSelfTraffic,
  readPanelExpectation,
  sweepDepth,
} from './helpers/index.js';

// Self-traffic warmup so cerberus panels have populated counters
// before we sweep them. Same value the other phase specs use.
const SEED_TRAFFIC_SECONDS = 30;

// /api/v1/query_range / /loki/api/v1/query_range query window. 5min
// covers the warmup plus the rate(...[5m]) windows the dashboards use.
const QUERY_WINDOW_SECONDS = 5 * 60;
const QUERY_STEP_SECONDS = 15;

// Path to the provisioning directory the stack under test mounts into
// Grafana. Defaults to the compose stack's directory (the spec lives
// at test/e2e/playwright/iterate-all-dashboards.spec.ts, so that's one
// level up + /grafana/compose/dashboards); the k3d dashboard job
// overrides via DASHBOARDS_DIR because the k3d stack provisions a
// different (smaller) dashboard set — its collector intentionally
// doesn't run the compose-only sqlquery/hostmetrics/self-scrape
// receivers, so probing the compose-only dashboards against it
// reports empty panels for data the stack never ships. Each stack's
// sweep must cover exactly the dashboards that stack provisions; the
// compose set stays covered per-PR by the required compose-smoke job.
const DASHBOARDS_DIR =
  process.env.DASHBOARDS_DIR ??
  resolve(__dirname, '..', 'grafana', 'compose', 'dashboards');

/**
 * Local types — kept inline since this spec uses the on-disk JSON
 * directly rather than the Grafana API (the API requires Grafana to
 * be up and provisioning to have completed; reading the JSON on disk
 * means the spec build-time list matches what's mounted into the
 * compose stack, and a missing dashboard is a provisioning failure
 * the request loop will catch independently).
 */
type DashboardJSON = {
  uid: string;
  title: string;
  panels?: PanelJSON[];
  templating?: { list?: VariableJSON[] };
};

type PanelJSON = {
  id?: number;
  title?: string;
  type?: string;
  datasource?: { type?: string; uid?: string } | string;
  targets?: TargetJSON[];
  panels?: PanelJSON[]; // nested under rows
  /** cerberus.expect contract block — see helpers/expectations.ts. */
  cerberus?: unknown;
};

type TargetJSON = {
  refId?: string;
  expr?: string;
  query?: string;
  datasource?: { type?: string; uid?: string } | string;
  /** Grafana per-query min interval (e.g. "1ms", "30s"). */
  interval?: string;
};

type FlatPanel = {
  id: number;
  title: string;
  type: string;
  dsType: string;
  dsUid: string;
  targets: FlatTarget[];
  /** Raw cerberus.expect contract block, parsed at probe time. */
  cerberus?: unknown;
};

type FlatTarget = {
  refId: string;
  expr: string;
  dsType: string;
  dsUid: string;
  /**
   * Probe step in seconds, derived from the target's Grafana
   * `interval` field when present (a panel that pins a min interval
   * is asking every consumer — Grafana AND this sweep — to honour
   * it). Falls back to QUERY_STEP_SECONDS. The showcase-promql
   * resolution-cap panel relies on this: its 1ms interval makes the
   * probe grid exceed cerberus's 11k-points cap so the declared
   * error contract actually fires.
   */
  stepSeconds: number;
};

type DiscoveredDashboard = {
  uid: string;
  title: string;
  filename: string;
  panels: FlatPanel[];
  variables: VariableJSON[];
};

/**
 * Read every `*.json` under DASHBOARDS_DIR at spec-build time. This
 * runs once, BEFORE the test functions register, so the per-dashboard
 * `test()` calls below are statically known by Playwright.
 */
function discoverDashboards(): DiscoveredDashboard[] {
  let entries: string[];
  try {
    entries = readdirSync(DASHBOARDS_DIR);
  } catch (err) {
    throw new Error(
      `iterate-all-dashboards: could not read ${DASHBOARDS_DIR}: ${
        (err as Error).message
      }`,
    );
  }
  const out: DiscoveredDashboard[] = [];
  for (const entry of entries) {
    if (!entry.endsWith('.json')) continue;
    const full = join(DASHBOARDS_DIR, entry);
    const raw = readFileSync(full, 'utf8');
    let json: DashboardJSON;
    try {
      json = JSON.parse(raw) as DashboardJSON;
    } catch (err) {
      throw new Error(
        `iterate-all-dashboards: ${entry} is not valid JSON: ${
          (err as Error).message
        }`,
      );
    }
    if (!json.uid || !json.title) {
      throw new Error(
        `iterate-all-dashboards: ${entry} missing uid or title`,
      );
    }
    out.push({
      uid: json.uid,
      title: json.title,
      filename: entry,
      panels: flattenPanels(json.panels ?? []),
      variables: json.templating?.list ?? [],
    });
  }
  // Sort for deterministic test order regardless of readdir() ordering.
  out.sort((a, b) => a.uid.localeCompare(b.uid));
  return out;
}

function flattenPanels(raw: PanelJSON[]): FlatPanel[] {
  const out: FlatPanel[] = [];
  for (const p of raw) {
    if (p.type === 'row') {
      out.push(...flattenPanels(p.panels ?? []));
      continue;
    }
    out.push(normalisePanel(p));
  }
  return out;
}

function normalisePanel(p: PanelJSON): FlatPanel {
  const panelDs = normaliseDs(p.datasource);
  const targets: FlatTarget[] = (p.targets ?? []).map((t) => {
    const tDs = normaliseDs(t.datasource);
    return {
      refId: t.refId ?? '',
      expr: (t.expr ?? t.query ?? '').trim(),
      dsType: tDs.type || panelDs.type,
      dsUid: tDs.uid || panelDs.uid,
      stepSeconds: parseIntervalSeconds(t.interval) ?? QUERY_STEP_SECONDS,
    };
  });
  return {
    id: p.id ?? 0,
    title: p.title ?? '<untitled>',
    type: p.type ?? 'unknown',
    dsType: panelDs.type,
    dsUid: panelDs.uid,
    targets,
    cerberus: p.cerberus,
  };
}

/**
 * Parse a Grafana interval string ("1ms", "500ms", "15s", "2m", "1h")
 * into seconds. Returns null for absent/unrecognised input so the
 * caller falls back to the sweep default. Sub-second values are
 * preserved as decimals — the Prometheus step parameter accepts float
 * seconds.
 */
function parseIntervalSeconds(raw: string | undefined): number | null {
  if (!raw) return null;
  const m = /^(\d+(?:\.\d+)?)(ms|s|m|h)$/.exec(raw.trim());
  if (!m) return null;
  const n = Number(m[1]);
  switch (m[2]) {
    case 'ms':
      return n / 1000;
    case 's':
      return n;
    case 'm':
      return n * 60;
    case 'h':
      return n * 3600;
    default:
      return null;
  }
}

function normaliseDs(
  ds: { type?: string; uid?: string } | string | undefined,
): { type: string; uid: string } {
  if (ds === undefined) return { type: '', uid: '' };
  if (typeof ds === 'string') return { type: '', uid: ds };
  return { type: ds.type ?? '', uid: ds.uid ?? '' };
}

/**
 * Build the datasource-proxy URL that hits the same cerberus endpoint
 * Grafana itself would use for this target. Returns null for target
 * types we don't sweep (alerting expressions, dashboard variables…).
 */
function buildProbeURL(
  baseURL: string,
  t: FlatTarget,
  nowSec: number,
): string | null {
  if (!t.expr || !t.dsUid) return null;
  const start = nowSec - QUERY_WINDOW_SECONDS;
  const end = nowSec;
  const dsType = t.dsType.toLowerCase();
  if (dsType === 'prometheus') {
    const q = encodeURIComponent(t.expr);
    return (
      `${baseURL}/api/datasources/proxy/uid/${t.dsUid}` +
      `/api/v1/query_range?query=${q}` +
      `&start=${start}&end=${end}&step=${t.stepSeconds}`
    );
  }
  if (dsType === 'loki') {
    const q = encodeURIComponent(t.expr);
    return (
      `${baseURL}/api/datasources/proxy/uid/${t.dsUid}` +
      `/loki/api/v1/query_range?query=${q}` +
      `&start=${start * 1e9}&end=${end * 1e9}&limit=10`
    );
  }
  if (dsType === 'tempo') {
    // Tempo search uses /api/search with a TraceQL expression.
    const q = encodeURIComponent(t.expr || '{}');
    return (
      `${baseURL}/api/datasources/proxy/uid/${t.dsUid}` +
      `/api/search?q=${q}&start=${start}&end=${end}&limit=10`
    );
  }
  return null;
}

/**
 * Inspect a parsed response body and return a non-null error string
 * if the envelope carries an upstream error. Returns null on success.
 */
function envelopeError(body: unknown): string | null {
  if (body === null || typeof body !== 'object') return null;
  const b = body as Record<string, unknown>;
  // Prometheus error envelope: { status: "error", errorType, error }.
  if (typeof b.status === 'string' && b.status === 'error') {
    return `prom error: type=${String(b.errorType)} msg=${String(b.error)}`;
  }
  // Loki / Tempo error envelopes typically surface `message` or `error`.
  if (typeof b.error === 'string' && b.error.length > 0) {
    return `error field: ${b.error}`;
  }
  if (typeof b.message === 'string' && b.message.length > 0) {
    // Tempo /api/search returns `{ traces: [], metrics: {...} }` on
    // success, no `message` field. A populated `message` always means
    // an upstream issue.
    return `message field: ${b.message}`;
  }
  return null;
}

/**
 * Number of series (Prom) / streams (Loki) / traces (Tempo) the body
 * carries. Returns -1 if the body is a shape we don't recognise (the
 * caller should treat that as "non-empty" / bypass the empty check).
 */
function resultCount(body: unknown, dsType: string): number {
  if (body === null || typeof body !== 'object') return -1;
  const b = body as Record<string, unknown>;
  const t = dsType.toLowerCase();
  if (t === 'prometheus') {
    const data = b.data as Record<string, unknown> | undefined;
    const result = data?.result;
    return Array.isArray(result) ? result.length : -1;
  }
  if (t === 'loki') {
    const data = b.data as Record<string, unknown> | undefined;
    const result = data?.result;
    return Array.isArray(result) ? result.length : -1;
  }
  if (t === 'tempo') {
    const traces = b.traces;
    return Array.isArray(traces) ? traces.length : -1;
  }
  return -1;
}

/**
 * Fire `/api/ds/query` capture for the duration of the page navigation
 * and assert no per-target error landed in any response envelope.
 *
 * Returns the list of failure descriptions found; an empty list means
 * everything was green.
 */
async function captureAndAssertDsQuery(
  page: Page,
  url: string,
): Promise<string[]> {
  const failures: string[] = [];
  const captured: Response[] = [];

  const onResponse = (resp: Response) => {
    const u = resp.url();
    if (
      u.includes('/api/ds/query') ||
      u.includes('/api/datasources/proxy/uid/') ||
      (u.includes('/api/datasources/uid/') && u.includes('/resources/'))
    ) {
      captured.push(resp);
    }
  };
  page.on('response', onResponse);

  try {
    await page.goto(url, { waitUntil: 'networkidle', timeout: 45_000 });
  } catch (err) {
    failures.push(
      `page.goto(${url}) failed: ${(err as Error).message}`,
    );
  }
  page.off('response', onResponse);

  for (const resp of captured) {
    const status = resp.status();
    if (status < 200 || status > 299) {
      failures.push(
        `${resp.request().method()} ${resp.url()} → ${status}`,
      );
      continue;
    }
    if (resp.url().includes('/api/ds/query')) {
      let body: unknown;
      try {
        body = await resp.json();
      } catch {
        // Some /api/ds/query responses are streamed / chunked; bypass
        // the per-target check rather than fail on a parse error.
        continue;
      }
      if (body && typeof body === 'object' && 'results' in body) {
        const results = (body as Record<string, unknown>).results as
          | Record<string, unknown>
          | undefined;
        if (results) {
          for (const [refId, frame] of Object.entries(results)) {
            if (
              frame &&
              typeof frame === 'object' &&
              'error' in frame &&
              typeof (frame as Record<string, unknown>).error === 'string' &&
              (frame as Record<string, unknown>).error !== ''
            ) {
              failures.push(
                `/api/ds/query refId=${refId} carried error: ${
                  (frame as Record<string, unknown>).error as string
                }`,
              );
            }
          }
        }
      }
    }
  }
  return failures;
}

// ---------------------------------------------------------------------------
// Test registration. We discover dashboards once, log the catalog, and
// register one test per dashboard so failures are scoped per-board in
// the Playwright report.
// ---------------------------------------------------------------------------

const dashboards = discoverDashboards();

// SWEEP_DEPTH gates how many STATES the sweep visits, never which
// rules run: at 'lean' (the per-PR default) the browser render is
// restricted to ops-family dashboards — showcase-prefixed boards get
// API-layer probes only; at 'full' (nightly) every dashboard also
// renders in the browser. Today no showcase- dashboard exists, so
// both depths execute the exact same set of checks.
const depth = sweepDepth();

test.describe('iterate-all-dashboards: full provisioned-dashboard sweep', () => {
  test.describe.configure({ mode: 'serial' });

  test('dashboard catalog non-empty', () => {
    // Log the catalog so the CI run record shows which dashboards
    // were swept. Use `console.log` rather than `test.info().annotations`
    // because the latter doesn't render in the GitHub Actions summary.
    // eslint-disable-next-line no-console
    console.log(describeSweepDepth(depth));
    // eslint-disable-next-line no-console
    console.log(
      `iterate-all-dashboards: discovered ${dashboards.length} dashboards in ${DASHBOARDS_DIR}:`,
    );
    for (const d of dashboards) {
      // eslint-disable-next-line no-console
      console.log(`  - ${d.uid} (${d.filename}): ${d.title}`);
    }
    expect(dashboards.length, 'at least one provisioned dashboard').toBeGreaterThan(0);
  });

  test.beforeAll(async ({ request }) => {
    // Single warmup for the whole describe block — the per-dashboard
    // tests inherit the populated counters.
    await generateSelfTraffic(request, SEED_TRAFFIC_SECONDS);
  });

  for (const d of dashboards) {
    test(`dashboard ${d.uid} — render + per-target probe`, async ({
      page,
      request,
    }) => {
      const baseURL =
        process.env.GRAFANA_BASE_URL ?? process.env.GRAFANA_URL ?? 'http://localhost:3000';

      // 1. Navigate to /d/<uid> and capture all datasource traffic.
      //    Depth-gated state count (rules unchanged): 'lean' renders
      //    ops-family dashboards only — showcase-prefixed boards are
      //    covered by the API-layer probes below per PR and get their
      //    browser render on the nightly 'full' lane.
      if (depth === 'full' || !d.filename.startsWith('showcase-')) {
        const navFailures = await captureAndAssertDsQuery(
          page,
          `${baseURL}/d/${d.uid}`,
        );
        expect(
          navFailures,
          `dashboard ${d.uid}: navigation surfaced datasource errors:\n  - ${navFailures.join('\n  - ')}`,
        ).toEqual([]);
      }

      // 2. Per-target probe — fires the panel's PromQL/LogQL/TraceQL
      //    expression through the datasource proxy.
      const nowSec = Math.floor(Date.now() / 1000);
      let probedTargets = 0;
      let nonEmptyTargets = 0;
      for (const panel of d.panels) {
        for (const t of panel.targets) {
          const url = buildProbeURL(baseURL, t, nowSec);
          if (!url) continue;
          probedTargets++;
          await probeTarget(request, url, d, panel, t, (count) => {
            if (count > 0) nonEmptyTargets++;
          });
        }
      }
      // 3. Template-variable contracts — every variable's options
      //    resolve live (the same lookups Grafana fires for the
      //    dropdown); pinned variables (cerberus.expectOptions) get
      //    set equality, unpinned ones non-emptiness. Today no
      //    provisioned dashboard carries variables, so the loop is a
      //    no-op — the path goes live the moment one lands (P3).
      const variableViolations: string[] = [];
      for (const variable of d.variables) {
        variableViolations.push(
          ...(await checkDashboardVariable(request, baseURL, variable)),
        );
      }
      expect(
        variableViolations,
        `dashboard ${d.uid}: variable contracts violated:\n  - ${variableViolations.join('\n  - ')}`,
      ).toEqual([]);

      // eslint-disable-next-line no-console
      console.log(
        `iterate-all-dashboards: dashboard=${d.uid} probed=${probedTargets} non_empty=${nonEmptyTargets} variables=${d.variables.length}`,
      );
    });
  }
});

async function probeTarget(
  request: APIRequestContext,
  url: string,
  d: DiscoveredDashboard,
  panel: FlatPanel,
  t: FlatTarget,
  onCount: (count: number) => void,
): Promise<void> {
  // The panel's declared contract — absent declaration is the default
  // {expect: 'nonempty'} (every current dashboard panel; non-default
  // declarations are a showcase-family privilege enforced by
  // expectation-contracts.spec.ts). enforceExpectation is the
  // BIDIRECTIONAL gate: a declared-empty panel that returns series
  // fails just as loudly as a default panel that returns none.
  const expectation = readPanelExpectation(panel);
  const isErrorContract = expectation.expect.startsWith('error:');

  const resp = await request.get(url);
  const status = resp.status();
  const bodyText = await resp.text();

  let count = 0;
  if (status >= 200 && status <= 299) {
    let body: unknown;
    try {
      body = JSON.parse(bodyText);
    } catch (err) {
      throw new Error(
        `dashboard=${d.uid} panel="${panel.title}" target=${t.refId}: body not JSON: ${
          (err as Error).message
        }`,
      );
    }

    if (!isErrorContract) {
      // A 2xx body tunnelling an envelope error is a failure for the
      // nonempty/empty contracts; an error-declared panel is judged
      // on the error substring by enforceExpectation instead.
      const errMsg = envelopeError(body);
      expect(
        errMsg,
        `dashboard=${d.uid} panel="${panel.title}" target=${t.refId}: envelope error: ${errMsg ?? ''}`,
      ).toBeNull();
    }

    count = resultCount(body, t.dsType);
  }
  onCount(count);

  // An empty result on a default panel is a real bug in the cerberus
  // code, the seed, the dashboard, or the panel expression; mask
  // nothing. Equally, a declared expectation that no longer holds is
  // a broken showcase.
  const violations = enforceExpectation(expectation, {
    seriesCount: count,
    status,
    errorBody: bodyText,
  });
  expect(
    violations,
    `dashboard=${d.uid} panel="${panel.title}" target=${t.refId} expr="${t.expr}" ` +
      `(declared=${expectation.declared ? expectation.expect : 'default:nonempty'}): ` +
      violations.join('; '),
  ).toEqual([]);
}
