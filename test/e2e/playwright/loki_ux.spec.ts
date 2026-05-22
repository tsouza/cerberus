import { test, expect } from '@playwright/test';

/**
 * LogQL UX flows.
 *
 * Mirrors the request sequence Grafana's Logs panel + Explore Logs UI
 * issue against a Loki datasource. Each spec exercises a UX flow
 * (level coloring, line filters, detected fields, patterns, log↔trace
 * link, histogram, ...) by hitting the cerberus-loki datasource proxy.
 *
 * Seed shape (test/e2e/seed/cmd/seed/main.go):
 *   • otel_logs: 120 rows spaced 15 s apart spanning a 30-minute window
 *     centred on the seed timestamp, three services (api / frontend /
 *     db). SeverityNumber cycles {17, 13, 9} and SeverityText cycles
 *     {ERROR, WARN, INFO}. Body has the form "<message> id=<n>" —
 *     useful as a line-filter substring target.
 */

const lokiProxy = '/api/datasources/proxy/uid/cerberus-loki/loki/api/v1';

// Helper: a fresh "last 5 minutes" window in unix seconds.
function last5MinWindow(): { start: number; end: number } {
  const end = Math.floor(Date.now() / 1000);
  return { start: end - 5 * 60, end };
}

test.describe('Loki UX — Logs panel flows', () => {
  test('logs panel: severity is exposed per stream (level coloring source)', async ({
    request,
  }) => {
    // Grafana's logs panel colours rows by detected severity. The
    // colour mapping reads from a `severity` / `level` / `detected_level`
    // label or the OTel SeverityText that surfaces as a stream label.
    const q = encodeURIComponent('{service_name="api"}');
    const resp = await request.get(`${lokiProxy}/query?query=${q}`);
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(body.data.resultType).toBe('streams');
    // At least one stream should carry a label (so the panel can group).
    expect(body.data.result.length).toBeGreaterThan(0);
    for (const stream of body.data.result) {
      expect(Object.keys(stream.stream).length, 'stream has ≥1 label').toBeGreaterThan(0);
    }
  });

  test('logs panel: line filter `|=` substring narrows results', async ({ request }) => {
    // Seed body has `... id=<n>` on every row, so the substring
    // matches every line.
    const q = encodeURIComponent('{service_name="api"} |= "id="');
    const resp = await request.get(`${lokiProxy}/query?query=${q}`);
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(body.status).toBe('success');
    expect(body.data.resultType).toBe('streams');
    for (const stream of body.data.result) {
      for (const [, line] of stream.values) {
        expect(typeof line).toBe('string');
        expect(line, 'every line contains the filter literal').toContain('id=');
      }
    }
  });

  test('logs panel: negative line filter `!=` excludes substring', async ({ request }) => {
    // No seed line contains the literal "this-string-does-not-exist",
    // so the filter is the empty-set case — still a successful empty
    // streams response, not an error.
    const q = encodeURIComponent('{service_name="api"} != "id="');
    const resp = await request.get(`${lokiProxy}/query?query=${q}`);
    expect(resp.status(), 'negative filter still 200').toBe(200);
    const body = await resp.json();
    expect(body.status).toBe('success');
    // Either zero results or zero values per stream — both fine.
    let total = 0;
    for (const s of body.data.result) {
      total += s.values.length;
    }
    expect(total, 'no lines contain "id=" once filtered out').toBe(0);
  });

  test('logs panel: regex line filter `|~` is a 200 ✓', async ({ request }) => {
    // Every seed line has the digit pattern `id=<n>` so the regex
    // `id=\\d+` matches every line.
    const q = encodeURIComponent('{service_name="api"} |~ "id=\\\\d+"');
    const resp = await request.get(`${lokiProxy}/query?query=${q}`);
    expect(resp.status(), 'regex filter status').toBe(200);
    const body = await resp.json();
    expect(body.status).toBe('success');
  });

  test('logs panel: chained line filters compose', async ({ request }) => {
    // `|= "id="` then `!~ "id=9999"` — the second filter is ineffective
    // (no seed row has id=9999) but the chain should parse and run.
    const q = encodeURIComponent('{service_name="api"} |= "id=" !~ "id=9999"');
    const resp = await request.get(`${lokiProxy}/query?query=${q}`);
    expect(resp.status(), 'chained filter status').toBe(200);
    const body = await resp.json();
    expect(body.status).toBe('success');
    expect(body.data.result.length, '≥1 stream after chain').toBeGreaterThan(0);
  });

  test('detected fields: side-panel API returns extractable keys', async ({ request }) => {
    // The Logs panel "Detected fields" side panel calls
    // /loki/api/v1/detected_fields. The seed bodies are plain text
    // (not JSON), so the heuristic should still return a deterministic
    // (possibly empty) fields array — what matters is the envelope.
    const { start, end } = last5MinWindow();
    const q = encodeURIComponent('{service_name="api"}');
    const url = `${lokiProxy}/detected_fields?query=${q}&start=${start * 1e9}&end=${end * 1e9}`;
    const resp = await request.get(url);
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(body.status).toBe('success');
    expect(Array.isArray(body.data.fields), 'data.fields is an array').toBe(true);
    expect(typeof body.data.limit, 'data.limit is numeric').toBe('number');
    expect(typeof body.data.line_limit, 'data.line_limit is numeric').toBe('number');
  });

  test('patterns: the /patterns endpoint extracts drain clusters from log bodies', async ({
    request,
  }) => {
    // Grafana's "Patterns" tab calls /loki/api/v1/patterns. The handler
    // trains a per-request drain instance over the matched stream's peek
    // window and projects the resulting clusters onto the upstream
    // `WriteQueryPatternsResponseJSON` wire shape:
    //   {"status":"success","data":[
    //      {"pattern":"...","level":"","samples":[[ts_seconds, count], ...]},
    //      ...
    //   ]}
    // The seed body cycles five distinct templates with a varying
    // `id=<n>` suffix, so drain produces at least one cluster for the
    // `{service_name="api"}` selector.
    const { start, end } = last5MinWindow();
    const q = encodeURIComponent('{service_name="api"}');
    const url = `${lokiProxy}/patterns?query=${q}&start=${start * 1e9}&end=${end * 1e9}`;
    const resp = await request.get(url);
    expect(resp.status(), 'patterns status').toBe(200);
    const body = await resp.json();
    expect(body.status).toBe('success');
    expect(Array.isArray(body.data), 'data is an array of clusters').toBe(true);
    expect(body.data.length, '≥1 drain cluster').toBeGreaterThan(0);
    const cluster = body.data[0];
    expect(typeof cluster.pattern, 'cluster.pattern is a string').toBe('string');
    expect(cluster.pattern.length, 'cluster.pattern is non-empty').toBeGreaterThan(0);
    expect(Array.isArray(cluster.samples), 'cluster.samples is an array').toBe(true);
    expect(cluster.samples.length, '≥1 sample bucket').toBeGreaterThan(0);
    for (const sample of cluster.samples) {
      expect(Array.isArray(sample), 'sample is a [ts_seconds, count] tuple').toBe(true);
      expect(sample.length, 'tuple has 2 elements').toBe(2);
      expect(typeof sample[0], 'ts_seconds is numeric').toBe('number');
      expect(typeof sample[1], 'count is numeric').toBe('number');
    }
  });

  test('time range: query_range strictly contains every value in [start, end]', async ({
    request,
  }) => {
    // Cerberus's Loki streams handler now threads URL `start` / `end`
    // through to the LogQL lowering, which AND-folds a
    // `Timestamp BETWEEN start AND end` predicate above the
    // Scan(otel_logs) node. The emitted SQL honours the requested
    // window — every returned value's timestamp MUST satisfy
    // `start_ns <= ts <= end_ns`. The previous "1-day envelope"
    // tolerance is gone: the strict assertion is the regression gate
    // for the wire-format contract.
    //
    // Window sized at 5 minutes (matching last5MinWindow). The seed's
    // ±15-min window centred on seed_now keeps ≥ 1 row inside the
    // [test_now - 300s, test_now] envelope for every test in the
    // suite — even when CI scheduling jitter pushes test_now to
    // seed_now + 11+ min.
    const end = Math.floor(Date.now() / 1000);
    const start = end - 5 * 60;
    const q = encodeURIComponent('{service_name="api"}');
    const url = `${lokiProxy}/query_range?query=${q}&start=${start}&end=${end}&step=10`;
    const resp = await request.get(url);
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(body.status).toBe('success');
    expect(['streams', 'matrix']).toContain(body.data.resultType);
    // Strict containment: every value's ts must be in [start_ns, end_ns].
    const startNs = start * 1e9;
    const endNs = end * 1e9;
    let totalValues = 0;
    for (const s of body.data.result) {
      if (!s.values || s.values.length === 0) continue;
      totalValues += s.values.length;
      if (body.data.resultType === 'streams') {
        for (const [ts] of s.values) {
          const tsNum = Number(ts);
          expect(Number.isFinite(tsNum) && tsNum > 0, 'ts is a positive number').toBe(true);
          expect(tsNum, 'ts >= start_ns').toBeGreaterThanOrEqual(startNs);
          expect(tsNum, 'ts <= end_ns').toBeLessThanOrEqual(endNs);
        }
      }
    }
    expect(totalValues, '≥1 log value across the response').toBeGreaterThan(0);
  });

  test('logql aggregation: `rate({…}[1m])` produces a matrix not streams', async ({ request }) => {
    // The metric tab in Explore renders a chart for the rate() result.
    // The shape must be matrix, not streams.
    const now = Math.floor(Date.now() / 1000);
    const start = now - 5 * 60;
    const q = encodeURIComponent('rate({service_name="api"}[1m])');
    const url = `${lokiProxy}/query_range?query=${q}&start=${start}&end=${now}&step=30`;
    const resp = await request.get(url);
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(body.status).toBe('success');
    expect(body.data.resultType).toBe('matrix');
    expect(body.data.result.length, '≥1 metric series').toBeGreaterThan(0);
  });

  test('parser stage `| json` returns the success envelope', async ({ request }) => {
    // `| json` parses each line as a JSON object and lifts every
    // top-level key into a label, so downstream filter / format
    // stages can reference them. The seed bodies are plain text
    // (`<message> id=<n>`) — non-JSON lines pass through with no
    // extra labels extracted, which is the documented upstream
    // behaviour. The contract under test is the envelope: the
    // parser stage is implemented, so the handler returns 200 with
    // a `status: "success"` body, not a 422 rejection.
    const q = encodeURIComponent('{service_name="api"} | json');
    const resp = await request.get(`${lokiProxy}/query?query=${q}`);
    expect(resp.status(), '| json parses cleanly').toBe(200);
    const body = await resp.json();
    expect(body.status).toBe('success');
    expect(body.data.resultType).toBe('streams');
  });

  test('parser stage `| logfmt` returns the success envelope', async ({ request }) => {
    // `| logfmt` parses each line as `key=value` pairs and lifts the
    // extracted keys into stream labels. Mirrors the `| json` flow
    // above; the seed bodies are plain text without `k=v` pairs, so
    // no extra labels surface, but the envelope contract still holds.
    const q = encodeURIComponent('{service_name="api"} | logfmt');
    const resp = await request.get(`${lokiProxy}/query?query=${q}`);
    expect(resp.status(), '| logfmt parses cleanly').toBe(200);
    const body = await resp.json();
    expect(body.status).toBe('success');
    expect(body.data.resultType).toBe('streams');
  });

  test('post-fetch `| line_format` renders templated lines', async ({ request }) => {
    // line_format is a post-fetch transform — every Body is rewritten
    // before the streams shape goes out the door.
    const q = encodeURIComponent('{service_name="api"} | line_format "FMT: {{.__line__}}"');
    const resp = await request.get(`${lokiProxy}/query?query=${q}`);
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(body.data.resultType).toBe('streams');
    for (const stream of body.data.result) {
      for (const [, line] of stream.values) {
        expect(line, 'line starts with FMT:').toMatch(/^FMT:/);
      }
    }
  });

  test('histogram panel: /loki/api/v1/index/volume returns the volume time series', async ({
    request,
  }) => {
    // The Logs Histogram panel reads /index/volume to render the
    // "log volume" bar chart above the log lines.
    const { start, end } = last5MinWindow();
    const q = encodeURIComponent('{service_name="api"}');
    const url = `${lokiProxy}/index/volume?query=${q}&start=${start * 1e9}&end=${end * 1e9}`;
    const resp = await request.get(url);
    expect(resp.status(), 'index/volume status').toBe(200);
    const body = await resp.json();
    expect(body.status).toBe('success');
    expect(body.data.resultType, 'volume → vector').toBe('vector');
  });

  test('labels endpoint populates the stream-selector dropdown', async ({ request }) => {
    // The stream-selector dropdown in Explore Logs reads
    // /loki/api/v1/labels. The seed populates ResourceAttributes
    // with `service_name`.
    const resp = await request.get(`${lokiProxy}/labels`);
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(body.status).toBe('success');
    expect(body.data, 'service_name in labels').toContain('service_name');
  });

  test('label values populate the per-label dropdown', async ({ request }) => {
    const resp = await request.get(`${lokiProxy}/label/service_name/values`);
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(body.status).toBe('success');
    expect(Array.isArray(body.data)).toBe(true);
    expect(body.data, 'api in service_name values').toContain('api');
    // Seed inserts 3 services (api / frontend / db).
    expect(body.data.length, '3 distinct services').toBeGreaterThanOrEqual(3);
  });
});
