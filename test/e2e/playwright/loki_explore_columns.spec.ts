import { test, expect } from '@playwright/test';

/**
 * Loki Explore (Logs Drilldown) column validation — the PR #903
 * follow-up regression gate.
 *
 * Background: PR #903 wired the compose collector to emit
 * severity_text / duration / bytes structured-metadata attributes for the
 * ClickHouse `system.query_log` logs, but was NOT validated against the
 * live Grafana UI. The maintainer then observed three regressions on the
 * `grafana-lokiexplore-app` logs view for `{service_name="clickhouse"}`:
 *
 *   1. `index_granularity` rendered as a malformed `8192,` (trailing
 *      comma) — a Drilldown-app line-parse artefact of the SQL Body, not
 *      a column cerberus advertises.
 *   2. Nonsense empty columns `_method` / `_level` / `_status` / `_` /
 *      `_id` appeared — likewise the app's client-side logfmt/pattern
 *      parse of the SQL Body.
 *   3. The genuinely useful query_log columns (duration, read bytes/rows,
 *      query_id, …) did NOT surface — cerberus's query_range response
 *      dropped the per-line LogAttributes entirely, so the app had no
 *      structured metadata to render and fell back to noisy line parsing.
 *
 * The fix: cerberus's log-stream query_range now surfaces the OTel-CH
 * LogAttributes map as Loki structured metadata — the optional third
 * element of each `[ts, line, {metadata}]` value tuple — with empty
 * values dropped and keys normalised to the Loki/Prom grammar. The
 * Drilldown app reads that structured metadata to render clean, named,
 * well-formed per-line columns.
 *
 * This spec drives the SAME `clickhouse`-service log query the Drilldown
 * app fires (through Grafana's datasource proxy) and asserts cerberus's
 * RAW wire response — the layer #903 skipped. It is API-first so it runs
 * deterministically on the Linux compose-smoke path; the assertions pin
 * each of the three symptoms at the source (cerberus's response), which
 * is where the only fixable bugs live.
 */

const lokiProxy = '/api/datasources/proxy/uid/cerberus-loki/loki/api/v1';

// Grafana's Loki datasource (and the Logs Drilldown app on top of it)
// always sends this request header so its promlib response converter takes
// the categorized-stream branch and reads per-line structured metadata.
// cerberus gates the optional third `[ts, line, {metadata}]` tuple element
// on this exact flag (strict two-element reference-Loki parity otherwise),
// so any spec asserting structured-metadata columns MUST send it — it is
// what the real Drilldown client this gate stands in for sends on the wire.
const categorizeLabelsHeaders = {
  'X-Loki-Response-Encoding-Flags': 'categorize-labels',
};

// The compose collector tails ClickHouse system.query_log into the
// `clickhouse` service stream (test/e2e/otel-collector/compose-config.yaml,
// sqlquery/clickhouse-logs receiver). Its LogAttributes carry the useful
// query_log columns; its Body is the raw SQL query string.
const clickhouseSelector = '{service_name="clickhouse"}';

// Keys cerberus MUST surface as clean structured-metadata columns — the
// standard-named duplicates the collector emits so the Drilldown app's
// generic columns populate, plus the always-present query_id.
const usefulKeys = ['duration', 'read_bytes', 'query_id'];

// Garbage column names the Drilldown app's line-parse heuristics produce
// from the SQL Body. cerberus must NEVER advertise these via structured
// metadata; this gate fails if any leak into cerberus's own response.
const garbageKeys = ['_method', '_level', '_status', '_', '_id', 'index_granularity'];

function last15MinWindow(): { start: number; end: number } {
  const end = Math.floor(Date.now() / 1000);
  return { start: end - 15 * 60, end };
}

test.describe('Loki Explore columns — clickhouse query_log', () => {
  test('query_range surfaces useful structured-metadata columns, clean + well-formed', async ({
    request,
  }) => {
    const { start, end } = last15MinWindow();
    const q = encodeURIComponent(clickhouseSelector);
    const url = `${lokiProxy}/query_range?query=${q}&start=${start}&end=${end}&limit=200&direction=backward`;

    // Send the categorize-labels flag the real Logs Drilldown app sends, so
    // cerberus emits the categorized three-element value tuples this gate
    // inspects (without it, cerberus is correct to return strict two-element
    // reference-Loki tuples and no structured metadata surfaces at all).
    const resp = await request.get(url, { headers: categorizeLabelsHeaders });
    expect(resp.status(), 'loki /query_range status').toBe(200);

    const body = await resp.json();
    expect(body.status, 'loki response status').toBe('success');
    expect(body.data.resultType, 'loki resultType').toBe('streams');
    // Under the categorize-labels request flag cerberus must advertise the
    // matching encodingFlags on the envelope so Grafana's converter takes
    // the categorized-stream reader (a categorized body without the flag
    // 400s that parser). This pins the request-driven wire contract.
    expect(
      body.data.encodingFlags,
      'categorize-labels advertised on the streams envelope',
    ).toContain('categorize-labels');
    expect(
      body.data.result.length,
      'clickhouse query_log logs present (collector seeded them)',
    ).toBeGreaterThan(0);

    // Walk every entry's third tuple element and collect the union of
    // advertised structured-metadata keys + a sample of each key's value.
    //
    // Under categorize-labels cerberus emits the categorized wire shape
    // reference Loki uses: each value is `[ts, line, {"structuredMetadata":
    // {...}}]`, with the object present (possibly empty `{}`) on EVERY value
    // so Grafana's readCategorizedStream parser is satisfied. The Drilldown
    // app renders columns from the `structuredMetadata` sub-object, so that
    // is the layer this gate inspects — unwrap it and count only the entries
    // carrying real per-line metadata.
    const seenKeys = new Set<string>();
    const sampleValue = new Map<string, string>();
    let entriesWithMetadata = 0;

    for (const stream of body.data.result) {
      for (const value of stream.values) {
        // Categorized value tuple: [ts, line, {structuredMetadata: {...}}].
        expect(value.length, 'categorized value tuple has 3 elements').toBe(3);
        const [, line, categorized] = value;
        expect(typeof line, 'line is a string').toBe('string');
        expect(
          typeof categorized,
          'categorized element is an object, not a string',
        ).toBe('object');
        const metadata = (categorized as Record<string, unknown>).structuredMetadata as
          | Record<string, string>
          | undefined;
        expect(metadata, 'structuredMetadata sub-object present').toBeDefined();
        expect(typeof metadata, 'structuredMetadata is an object').toBe('object');
        const keys = Object.keys(metadata as Record<string, string>);
        if (keys.length === 0) {
          // An empty `{}` is a well-formed categorized tuple for a
          // metadata-free row; it just carries no columns to inspect.
          continue;
        }
        entriesWithMetadata += 1;
        for (const [k, v] of Object.entries(metadata as Record<string, string>)) {
          seenKeys.add(k);
          if (!sampleValue.has(k)) {
            sampleValue.set(k, v as string);
          }
          // (b) No empty-valued column ever surfaces.
          expect(v, `structured-metadata[${k}] is non-empty`).not.toBe('');
        }
      }
    }

    expect(
      entriesWithMetadata,
      'at least one log entry carries structured metadata (3-tuple)',
    ).toBeGreaterThan(0);

    // (3) The useful query_log columns surface.
    for (const key of usefulKeys) {
      expect(
        seenKeys.has(key),
        `useful column "${key}" present in structured metadata (keys: ${[...seenKeys].join(', ')})`,
      ).toBe(true);
    }

    // (3 cont.) `duration` looks like a duration — "<n>ms" per the
    // collector's `concat(toString(query_duration_ms), 'ms')`.
    const dur = sampleValue.get('duration');
    expect(dur, 'duration value present').toBeTruthy();
    expect(dur, `duration "${dur}" looks like a duration`).toMatch(/^\d+(\.\d+)?(ns|us|µs|ms|s|m|h)$/);

    // `read_bytes` is a clean integer — no trailing comma / unit cruft
    // (the `8192,` malformation class).
    const rb = sampleValue.get('read_bytes');
    expect(rb, 'read_bytes value present').toBeTruthy();
    expect(rb, `read_bytes "${rb}" is a clean integer`).toMatch(/^\d+$/);

    // (1) + (2) None of the garbage keys appear in cerberus's advertised
    // structured metadata. The `8192,` artefact and the `_method`/`_`/…
    // noise are Drilldown-app line-parse output; cerberus must not be
    // their source.
    for (const key of garbageKeys) {
      expect(
        seenKeys.has(key),
        `garbage column "${key}" must NOT appear in cerberus structured metadata`,
      ).toBe(false);
    }
    // No leading-underscore garbage of any shape leaked.
    for (const key of seenKeys) {
      expect(key, `structured-metadata key "${key}" is not underscore garbage`).not.toMatch(
        /^_/,
      );
      // No value carries a stray trailing comma (the `8192,` shape).
      const v = sampleValue.get(key) ?? '';
      expect(v, `structured-metadata[${key}]="${v}" has no trailing comma`).not.toMatch(/,$/);
    }
  });

  test('detected_level resolves to real levels, never collapses to all-unknown', async ({
    request,
  }) => {
    // The collector derives severity_text (info / error) from the
    // query_log row's terminal type, and cerberus's detected_level
    // cascade resolves it. Reference Loki splits a bare selector into one
    // stream per detected_level value; assert the clickhouse logs carry a
    // real level and are not uniformly "unknown".
    const { start, end } = last15MinWindow();
    const q = encodeURIComponent(clickhouseSelector);
    const url = `${lokiProxy}/query_range?query=${q}&start=${start}&end=${end}&limit=200`;

    const resp = await request.get(url);
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(body.data.resultType).toBe('streams');
    expect(body.data.result.length).toBeGreaterThan(0);

    const levels = new Set<string>();
    for (const stream of body.data.result) {
      const lvl = stream.stream.detected_level;
      if (lvl) {
        levels.add(lvl);
      }
    }
    expect(levels.size, 'detected_level is surfaced as a stream label').toBeGreaterThan(0);
    // Must include at least one genuine level — a clean QueryFinish maps
    // to "info" — so the cascade is not uniformly collapsing to unknown.
    const real = [...levels].filter((l) => l !== 'unknown');
    expect(
      real.length,
      `detected_level carries a real level (got: ${[...levels].join(', ')})`,
    ).toBeGreaterThan(0);
  });

  test('detected_fields advertises the useful structured-metadata keys', async ({ request }) => {
    // The Drilldown app calls /detected_fields to populate its field
    // sidebar. cerberus must advertise the real LogAttributes keys
    // (duration / read_bytes / query_id) — these come from structured
    // metadata, not a line parse, so they always surface for the
    // clickhouse query_log stream.
    //
    // NOTE on body-parse fields: cerberus's detected_fields mirrors
    // reference Loki's non-strict logfmt/json body parse exactly (the
    // required `compatibility/loki` gate diffs the field sets with no
    // allow-list). A free-form SQL Body can therefore still yield an
    // incidental logfmt fragment (`index_granularity`, a `key = value`
    // sliver) in BOTH backends — that parity is intentional and lives in
    // the field sidebar, not in the rendered COLUMN set. The column set
    // is driven by structured metadata (asserted clean in the
    // query_range test above), so `index_granularity` never renders as a
    // column. This test therefore pins the useful keys' presence without
    // asserting body-parse absence, which would break reference parity.
    const { start, end } = last15MinWindow();
    const q = encodeURIComponent(clickhouseSelector);
    const url = `${lokiProxy}/detected_fields?query=${q}&start=${start * 1e9}&end=${end * 1e9}`;

    const resp = await request.get(url);
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(Array.isArray(body.fields), 'top-level fields array').toBe(true);

    const labels = new Set<string>((body.fields as Array<{ label: string }>).map((f) => f.label));
    for (const key of usefulKeys) {
      expect(labels.has(key), `detected_fields advertises "${key}"`).toBe(true);
    }
  });
});
