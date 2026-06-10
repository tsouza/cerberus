/**
 * Dashboard template-variable contracts.
 *
 * Same contract philosophy as the panel-level cerberus.expect block
 * (helpers/expectations.ts): a variable may pin its EXACT option set
 * via a custom field on its templating.list entry —
 *
 *   { "name": "ql", "type": "query", …,
 *     "cerberus": { "expectOptions": ["promql", "logql", "traceql"] } }
 *
 * Pinned variables are asserted for SET EQUALITY with the live
 * options (order-insensitive, exact membership both ways — a missing
 * option means the data feeding it dried up, an unexpected option
 * means the variable query broadened). Variables without a pin are
 * asserted non-empty only: a variable with zero options is always a
 * bug (the dropdown renders blank and every dependent panel
 * collapses).
 *
 * Live resolution mirrors what Grafana 12 does when it opens the
 * variable dropdown: for `query` variables it evaluates the variable
 * query against the datasource. We resolve through the datasource
 * proxy — the same wire Grafana's own label_values()/label/tag
 * lookups travel — implementing the three cerberus heads:
 *
 *   - prometheus: `label_values(label)` / `label_values(selector,
 *     label)` / `label_names()` → /api/v1/label/<label>/values
 *     (+ match[]) / /api/v1/labels;
 *   - loki: the LokiVariableQuery object ({type: 0|1, label}) or the
 *     equivalent string forms → /loki/api/v1/labels /
 *     /loki/api/v1/label/<label>/values;
 *   - tempo: the TempoVariableQuery object ({type: 0|1, label}) →
 *     /api/v2/search/tags / /api/v2/search/tag/<tag>/values.
 *
 * Static variable types (custom / interval / constant / textbox)
 * resolve from the dashboard JSON itself; `datasource` variables
 * resolve via /api/datasources.
 */

import type { APIRequestContext } from '@playwright/test';

import { diffLabelKeys } from './assertions.js';

/** templating.list entry shape (the slice the contracts consume). */
export type VariableJSON = {
  name?: string;
  type?: string;
  query?: unknown;
  datasource?: { type?: string; uid?: string } | string | null;
  /** cerberus contract block: { expectOptions: string[] }. */
  cerberus?: unknown;
};

/**
 * Read a variable's declared option pin. Returns null when the
 * variable carries no pin; THROWS on a malformed declaration — a
 * contract that cannot be parsed must fail loudly, never degrade.
 */
export function readVariablePin(variableJson: unknown): string[] | null {
  if (variableJson === null || typeof variableJson !== 'object') return null;
  const cerberus = (variableJson as Record<string, unknown>).cerberus;
  if (cerberus === undefined) return null;
  if (cerberus === null || typeof cerberus !== 'object') {
    throw new Error(
      `readVariablePin: variable.cerberus must be an object, got ${JSON.stringify(cerberus)}`,
    );
  }
  const expectOptions = (cerberus as Record<string, unknown>).expectOptions;
  if (expectOptions === undefined) return null;
  if (
    !Array.isArray(expectOptions) ||
    expectOptions.length === 0 ||
    expectOptions.some((o) => typeof o !== 'string' || o === '')
  ) {
    throw new Error(
      `readVariablePin: cerberus.expectOptions must be a non-empty array of ` +
        `non-empty strings, got ${JSON.stringify(expectOptions)}`,
    );
  }
  return expectOptions as string[];
}

/**
 * Order-insensitive set-equality check between the live options and
 * a pin. Returns violations for BOTH directions: pinned options the
 * live set is missing, and live options the pin doesn't expect.
 * Duplicated live options are also a violation (a variable dropdown
 * must not repeat entries).
 */
export function compareOptionSets(
  observed: string[],
  expected: string[],
): string[] {
  const out: string[] = [];
  const dupes = observed.filter((o, i) => observed.indexOf(o) !== i);
  if (dupes.length > 0) {
    out.push(`duplicate options [${[...new Set(dupes)].sort().join(', ')}]`);
  }
  const { missing, extra } = diffLabelKeys(observed, expected);
  if (missing.length > 0) {
    out.push(
      `pinned options missing from the live set: [${missing.join(', ')}]`,
    );
  }
  if (extra.length > 0) {
    out.push(
      `live options not in the pin: [${extra.join(', ')}] — extend the pin ` +
        `or fix the variable query`,
    );
  }
  return out;
}

/**
 * Apply the variable contract to a resolved option set: pinned →
 * exact set equality; unpinned → non-empty (zero options is always a
 * bug). Pure — unit-tested stack-free; the live path feeds it from
 * `resolveVariableOptions`.
 */
export function checkVariableOptions(
  pin: string[] | null,
  observed: string[],
): string[] {
  if (pin !== null) return compareOptionSets(observed, pin);
  if (observed.length === 0) {
    return ['variable resolved zero options (a blank dropdown is always a bug)'];
  }
  return [];
}

// ---------------------------------------------------------------------------
// Variable-query parsing (pure)
// ---------------------------------------------------------------------------

export type ParsedVariableQuery =
  | { kind: 'prom-label-names' }
  | { kind: 'prom-label-values'; label: string; selector?: string }
  | { kind: 'loki-label-names' }
  | { kind: 'loki-label-values'; label: string }
  | { kind: 'tempo-tag-names' }
  | { kind: 'tempo-tag-values'; tag: string };

const LABEL_VALUES_RE = /^label_values\(\s*(?:(.+?)\s*,\s*)?([a-zA-Z_][a-zA-Z0-9_]*)\s*\)$/;
const LABEL_NAMES_RE = /^label_names\(\s*\)$/;

/**
 * Parse a templating.list entry's `query` for the given datasource
 * type into a typed lookup. Grafana 12 persists:
 *
 *   - prometheus: a string (`label_values(up, job)`) or an object
 *     `{ query: '<string>', refId }`;
 *   - loki:  `{ type: 0|1, label?, stream?, refId }` (0 = label
 *     names, 1 = label values) or the legacy string forms;
 *   - tempo: `{ type: 0|1, label?, refId }` (0 = tag/label names,
 *     1 = tag values for `label`).
 *
 * Throws on anything it cannot classify — an unparseable variable
 * query must fail the sweep, not silently resolve to nothing.
 */
export function parseVariableQuery(
  dsType: string,
  query: unknown,
): ParsedVariableQuery {
  const t = dsType.toLowerCase();
  if (t === 'prometheus') {
    const q =
      typeof query === 'string'
        ? query
        : query !== null &&
            typeof query === 'object' &&
            typeof (query as Record<string, unknown>).query === 'string'
          ? ((query as Record<string, unknown>).query as string)
          : null;
    if (q === null) {
      throw new Error(
        `parseVariableQuery: prometheus variable query is neither a string nor {query}: ${JSON.stringify(query)}`,
      );
    }
    const trimmed = q.trim();
    if (LABEL_NAMES_RE.test(trimmed)) return { kind: 'prom-label-names' };
    const m = LABEL_VALUES_RE.exec(trimmed);
    if (m !== null) {
      const selector = m[1];
      const label = m[2]!;
      return selector !== undefined
        ? { kind: 'prom-label-values', label, selector }
        : { kind: 'prom-label-values', label };
    }
    throw new Error(
      `parseVariableQuery: unrecognised prometheus variable query ${JSON.stringify(trimmed)} ` +
        `(expected label_values(...) or label_names())`,
    );
  }

  if (t === 'loki') {
    if (query !== null && typeof query === 'object') {
      const o = query as Record<string, unknown>;
      if (o.type === 0) return { kind: 'loki-label-names' };
      if (o.type === 1 && typeof o.label === 'string' && o.label !== '') {
        return { kind: 'loki-label-values', label: o.label };
      }
      throw new Error(
        `parseVariableQuery: unrecognised loki variable query object ${JSON.stringify(query)}`,
      );
    }
    if (typeof query === 'string') {
      const trimmed = query.trim();
      if (LABEL_NAMES_RE.test(trimmed)) return { kind: 'loki-label-names' };
      const m = LABEL_VALUES_RE.exec(trimmed);
      if (m !== null && m[1] === undefined) {
        return { kind: 'loki-label-values', label: m[2]! };
      }
    }
    throw new Error(
      `parseVariableQuery: unrecognised loki variable query ${JSON.stringify(query)}`,
    );
  }

  if (t === 'tempo') {
    if (query !== null && typeof query === 'object') {
      const o = query as Record<string, unknown>;
      if (o.type === 0) return { kind: 'tempo-tag-names' };
      if (o.type === 1 && typeof o.label === 'string' && o.label !== '') {
        return { kind: 'tempo-tag-values', tag: o.label };
      }
    }
    throw new Error(
      `parseVariableQuery: unrecognised tempo variable query ${JSON.stringify(query)} ` +
        `(expected {type: 0|1, label})`,
    );
  }

  throw new Error(
    `parseVariableQuery: datasource type "${dsType}" has no variable-query lookup`,
  );
}

/**
 * Datasource-proxy path (relative to the Grafana base URL) that
 * resolves a parsed variable query. Pure — unit-testable; the live
 * resolver concatenates it with the base URL.
 */
export function variableOptionsPath(
  dsUid: string,
  parsed: ParsedVariableQuery,
): string {
  const proxy = `/api/datasources/proxy/uid/${dsUid}`;
  switch (parsed.kind) {
    case 'prom-label-names':
      return `${proxy}/api/v1/labels`;
    case 'prom-label-values':
      return (
        `${proxy}/api/v1/label/${encodeURIComponent(parsed.label)}/values` +
        (parsed.selector !== undefined
          ? `?match[]=${encodeURIComponent(parsed.selector)}`
          : '')
      );
    case 'loki-label-names':
      return `${proxy}/loki/api/v1/labels`;
    case 'loki-label-values':
      return `${proxy}/loki/api/v1/label/${encodeURIComponent(parsed.label)}/values`;
    case 'tempo-tag-names':
      return `${proxy}/api/v2/search/tags`;
    case 'tempo-tag-values':
      return `${proxy}/api/v2/search/tag/${encodeURIComponent(parsed.tag)}/values`;
  }
}

/**
 * Extract the option strings from a lookup response body. Pure —
 * unit-testable against captured fixtures. Throws on a body that
 * doesn't match the endpoint's wire shape.
 */
export function extractVariableOptions(
  parsed: ParsedVariableQuery,
  body: unknown,
): string[] {
  const b = (body ?? {}) as Record<string, unknown>;
  switch (parsed.kind) {
    case 'prom-label-names':
    case 'prom-label-values':
    case 'loki-label-names':
    case 'loki-label-values': {
      // {status: 'success', data: [string, …]}
      if (b.status !== 'success' || !Array.isArray(b.data)) {
        throw new Error(
          `extractVariableOptions: ${parsed.kind} body is not a success/data envelope: ${JSON.stringify(body).slice(0, 300)}`,
        );
      }
      return (b.data as unknown[]).map((v) => String(v));
    }
    case 'tempo-tag-names': {
      // v2: {scopes: [{name, tags: [string, …]}, …]}
      if (!Array.isArray(b.scopes)) {
        throw new Error(
          `extractVariableOptions: tempo-tag-names body carries no scopes array: ${JSON.stringify(body).slice(0, 300)}`,
        );
      }
      const tags = new Set<string>();
      for (const scope of b.scopes as Array<Record<string, unknown>>) {
        if (!Array.isArray(scope.tags)) continue;
        for (const tag of scope.tags as unknown[]) tags.add(String(tag));
      }
      return [...tags];
    }
    case 'tempo-tag-values': {
      // v2: {tagValues: [{type, value}, …]}
      if (!Array.isArray(b.tagValues)) {
        throw new Error(
          `extractVariableOptions: tempo-tag-values body carries no tagValues array: ${JSON.stringify(body).slice(0, 300)}`,
        );
      }
      return (b.tagValues as Array<Record<string, unknown>>).map((v) =>
        String(v.value),
      );
    }
  }
}

// ---------------------------------------------------------------------------
// Live resolution + the full per-variable check
// ---------------------------------------------------------------------------

function normaliseDs(
  ds: { type?: string; uid?: string } | string | null | undefined,
): { type: string; uid: string } {
  if (ds === undefined || ds === null) return { type: '', uid: '' };
  if (typeof ds === 'string') return { type: '', uid: ds };
  return { type: ds.type ?? '', uid: ds.uid ?? '' };
}

/**
 * Resolve a variable's live option set. `query` variables go through
 * the datasource proxy (the three head-specific lookups above);
 * static types resolve from the dashboard JSON; `datasource`
 * variables enumerate /api/datasources.
 */
export async function resolveVariableOptions(
  request: APIRequestContext,
  baseURL: string,
  variable: VariableJSON,
): Promise<string[]> {
  const vType = variable.type ?? 'query';

  if (vType === 'custom' || vType === 'interval') {
    const q = typeof variable.query === 'string' ? variable.query : '';
    return q
      .split(',')
      .map((s) => s.trim())
      .filter((s) => s !== '');
  }
  if (vType === 'constant' || vType === 'textbox') {
    const q = typeof variable.query === 'string' ? variable.query.trim() : '';
    return q === '' ? [] : [q];
  }
  if (vType === 'datasource') {
    const wantType = typeof variable.query === 'string' ? variable.query : '';
    const resp = await request.get(`${baseURL}/api/datasources`);
    if (resp.status() < 200 || resp.status() > 299) {
      throw new Error(
        `resolveVariableOptions: GET /api/datasources → ${resp.status()}`,
      );
    }
    const list = (await resp.json()) as Array<{ name?: string; type?: string }>;
    return list
      .filter((d) => wantType === '' || d.type === wantType)
      .map((d) => d.name ?? '');
  }
  if (vType !== 'query') {
    throw new Error(
      `resolveVariableOptions: variable "${variable.name ?? '<unnamed>'}" has ` +
        `type "${vType}", which has no option-resolution path here — add one ` +
        `before provisioning such a variable`,
    );
  }

  const ds = normaliseDs(variable.datasource);
  if (ds.uid === '') {
    throw new Error(
      `resolveVariableOptions: query variable "${variable.name ?? '<unnamed>'}" carries no datasource uid`,
    );
  }
  const parsed = parseVariableQuery(ds.type, variable.query);
  const path = variableOptionsPath(ds.uid, parsed);
  const resp = await request.get(`${baseURL}${path}`);
  if (resp.status() < 200 || resp.status() > 299) {
    throw new Error(
      `resolveVariableOptions: GET ${path} → ${resp.status()} for variable ` +
        `"${variable.name ?? '<unnamed>'}"`,
    );
  }
  return extractVariableOptions(parsed, await resp.json());
}

/**
 * The full per-variable contract the dashboard sweeps call: resolve
 * the live options, then apply the pin (set equality) or the
 * non-empty default. Returns violations prefixed with the variable
 * name so the caller can aggregate across a dashboard.
 */
export async function checkDashboardVariable(
  request: APIRequestContext,
  baseURL: string,
  variable: VariableJSON,
): Promise<string[]> {
  const pin = readVariablePin(variable);
  const options = await resolveVariableOptions(request, baseURL, variable);
  return checkVariableOptions(pin, options).map(
    (v) => `variable "${variable.name ?? '<unnamed>'}": ${v}`,
  );
}
