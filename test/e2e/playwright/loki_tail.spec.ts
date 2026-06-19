import { test, expect } from '@playwright/test';

/**
 * Direct-WebSocket Loki live-tail no-loss / no-dup regression spec.
 *
 * This is the end-to-end oracle for PR #1011's cursor-overflow fix in
 * internal/api/loki/tail.go: when a single poll window holds MORE rows
 * than the tail `limit` (default 100), the LIMITed batch is truncated,
 * and the cursor must advance only PAST the last row it actually sent so
 * the overflow is re-queried on the next poll. The pre-#1011 code seeded
 * `nextCursor` at `end` and only bumped it UP — but every row is bounded
 * `<= end`, so the bump never fired and a truncated window silently
 * dropped its tail. This spec forces that exact overflow and asserts the
 * stream delivers every row with NO drop and NO duplicate.
 *
 * Why a DIRECT WebSocket and not the Grafana UI (Option B):
 *   - Grafana Explore "Live" is a noisy DOM oracle: virtualised log rows,
 *     scroll-windowing, and de-dup in the panel make "every row arrived"
 *     unobservable without reverse-engineering the renderer.
 *   - Logs Drilldown (grafana-lokiexplore-app) can't stream at all.
 * Opening cerberus's `/loki/api/v1/tail` WebSocket directly gives a
 * DETERMINISTIC wire oracle: we read every `streams[].values[i][1]` line
 * cerberus pushes and diff it against the burst we inserted + the
 * `/loki/api/v1/query_range` ground truth for the same window.
 *
 * No `ws` npm dep is available in this suite (see package.json), so the
 * WebSocket is opened with the browser-native `WebSocket` via
 * `page.evaluate` rather than a Node ws client. The compose stack's
 * tail upgrader permits every origin (CheckOrigin returns true), so the
 * about:blank page context can connect cross-origin to ws://cerberus.
 *
 * Burst shape mirrors test/e2e/seed/cmd/seed/main.go's `insertLogsSQL`
 * column set EXACTLY — (Timestamp, TraceId, SpanId, SeverityText,
 * SeverityNumber, ServiceName, Body, ResourceAttributes, LogAttributes)
 * — so cerberus's LogQL selector matches the rows. The `service_name`
 * label resolves to `coalesce(nullIf(ServiceName,''),
 * ResourceAttributes['service_name'])` (internal/logql/lower.go
 * matcherLHS), and we populate BOTH, so a `{service_name="<unique>"}`
 * selector hits the dedicated top-level column.
 */

const CERBERUS_URL = process.env.CERBERUS_URL ?? 'http://localhost:8080';

// ClickHouse HTTP endpoint + creds + db, straight from the repo-root
// docker-compose.yml clickhouse service (8123 host-mapped, db `otel`,
// user/pass `cerberus`).
const CH_URL = process.env.CH_URL ?? 'http://localhost:8123';
const CH_USER = 'cerberus';
const CH_PASSWORD = 'cerberus';
const CH_DATABASE = 'otel';

// N must exceed the default tail limit (100) so a SINGLE poll window is
// guaranteed to overflow the LIMIT and exercise the #1011 cursor-advance
// path. 250 gives >2 full truncated batches of headroom even if the
// poll cadence happens to split the burst across ticks.
const BURST_N = 250;
const DEFAULT_TAIL_LIMIT = 100;

// Bounded settle: the poll interval is ~1s (+ delay_for, which we leave
// at its 0 default). We collect until all BURST_N lines are seen, with a
// hard deadline so a real drop fails LOUD instead of hanging. ~250 rows
// over 100-row chunks at 1s/tick needs ~3 ticks; 20s is generous but
// bounded. Cross-checked against the 60s per-test timeout in
// playwright.config.ts.
const TAIL_SETTLE_MS = 20_000;
// Poll cadence while draining the in-page line buffer from the test
// side. 250ms keeps the deadline loop responsive without spinning.
const DRAIN_POLL_MS = 250;

// Shape of the in-page tail collector exposed back to the test via
// page.evaluate. `lines` is every streamed line (in arrival order, with
// duplicates preserved so the no-dup assertion can see them); `errored`
// carries any WS-level failure so a handshake/abort surfaces as a test
// failure rather than an empty-but-green stream.
type TailResult = {
  lines: string[];
  errored: string | null;
};

test('loki tail streams an overflowing burst with no loss and no duplicate', async ({
  page,
  request,
}) => {
  // 1. Anchor t0 = now in nanoseconds. The tail `start` lower bound is
  // inclusive (`Timestamp >= cursor`); anchoring just before the burst
  // means the very first poll window already contains every burst row.
  const t0Ns = BigInt(Date.now()) * 1_000_000n;
  const t0NsStr = t0Ns.toString();

  // Unique per-run service name so the selector narrows to THIS burst
  // only — the rolling 40-rows/30s seeder uses service_name in
  // {api,frontend,db} and cerberus self-telemetry uses its own service
  // name, so neither pollutes the assertion. A label-only narrowing
  // (not a body substring) keeps the selector cheap and exact.
  const svc = `tail-e2e-${process.pid}-${t0NsStr}`;
  const selector = `{service_name="${svc}"}`;

  const ws = CERBERUS_URL.replace(/^http/, 'ws');
  const tailURL = `${ws}/loki/api/v1/tail?query=${encodeURIComponent(
    selector,
  )}&start=${t0NsStr}`;

  // 2. Open the tail WebSocket IN-PAGE (browser-native WebSocket; no `ws`
  // npm dep) and start collecting every streamed line into an in-page
  // array. We expose a poll handle on window so the test can drain the
  // buffer from Node without serialising a long-lived socket across the
  // bridge.
  //
  // Navigate to a REAL cerberus HTTP page first (not about:blank): an
  // opaque-origin document (about:blank → `origin === 'null'`) has
  // Chromium reject the insecure `ws://localhost` upgrade with a
  // transport-level 1006 abort. Landing on `${CERBERUS_URL}/loki/api/v1/labels`
  // (a cheap JSON 200) gives the page a concrete `http://localhost:8080`
  // origin, so the SAME-origin `ws://localhost:8080` tail connection is
  // permitted.
  await page.goto(`${CERBERUS_URL}/loki/api/v1/labels`);
  await page.evaluate((url) => {
    const w = window as unknown as {
      __tail: { lines: string[]; errored: string | null; sock: WebSocket };
    };
    const lines: string[] = [];
    let errored: string | null = null;
    const sock = new WebSocket(url);
    sock.onmessage = (ev: MessageEvent) => {
      try {
        const frame = JSON.parse(String(ev.data)) as {
          streams?: Array<{ values?: Array<[string, string]> }>;
        };
        for (const stream of frame.streams ?? []) {
          for (const v of stream.values ?? []) {
            // Loki value tuple is [tsNanoString, line]; the tail wire
            // shape is the plain two-element form (no categorize-labels
            // over the socket — see runTailLoop's `false` arg).
            lines.push(v[1]);
          }
        }
      } catch (e) {
        errored = `tail frame parse failed: ${String(e)}`;
      }
    };
    sock.onerror = () => {
      errored = errored ?? 'tail websocket error';
    };
    w.__tail = { lines, errored, sock };
  }, tailURL);

  // Give the upgrade a beat to complete before the burst lands, so the
  // first poll's lower bound (t0) is already armed server-side. The poll
  // loop re-queries `[cursor, now]` every tick regardless, so this only
  // tightens the common case; correctness doesn't depend on it.
  await page.waitForFunction(
    () => {
      const w = window as unknown as {
        __tail?: { sock: WebSocket; errored: string | null };
      };
      return (
        !!w.__tail &&
        (w.__tail.sock.readyState === WebSocket.OPEN ||
          w.__tail.errored !== null)
      );
    },
    undefined,
    { timeout: 10_000 },
  );

  // 3. Burst: insert BURST_N uniquely-tagged rows at ~now into otel_logs
  // via the ClickHouse HTTP port. The INSERT mirrors insertLogsSQL's
  // column set exactly. Each row gets a DISTINCT Timestamp
  // (now64(9) + number nanoseconds) so the tail cursor's +1ns dedup
  // never collapses two rows onto the same nanosecond — the burst is a
  // clean distinct-timestamp overflow, which is exactly what #1011's
  // cursor-advance must preserve. Bodies are `tail-e2e id=<k>` so each
  // line is individually identifiable; ServiceName AND
  // ResourceAttributes['service_name'] both carry the unique svc so the
  // selector matches regardless of which the lowering reads.
  const insertSQL =
    `INSERT INTO otel_logs ` +
    `(Timestamp, TraceId, SpanId, SeverityText, SeverityNumber, ServiceName, Body, ResourceAttributes, LogAttributes) ` +
    `SELECT ` +
    `now64(9) + INTERVAL number NANOSECOND, ` +
    `lpad(toString(number % 4), 32, '0'), ` +
    `lpad(toString(number % 4), 16, '0'), ` +
    `'INFO', ` +
    `9, ` +
    `'${svc}', ` +
    `concat('tail-e2e id=', toString(number)), ` +
    `map('service_name', '${svc}'), ` +
    `map('thread', concat('worker-', toString(number % 4))) ` +
    `FROM numbers(${BURST_N})`;

  const chResp = await request.post(CH_URL, {
    params: { database: CH_DATABASE },
    headers: {
      'X-ClickHouse-User': CH_USER,
      'X-ClickHouse-Key': CH_PASSWORD,
      'Content-Type': 'text/plain',
    },
    data: insertSQL,
  });
  expect(
    chResp.status(),
    `clickhouse burst insert failed: ${await chResp.text()}`,
  ).toBeLessThan(300);

  const expected = new Set<string>();
  for (let k = 0; k < BURST_N; k++) expected.add(`tail-e2e id=${k}`);

  // 4. Drain for a BOUNDED settle: poll the in-page buffer until every
  // burst line is seen or the deadline trips. The seeder is bursty /
  // future-dated, but we narrowed the selector to `svc`, so the in-page
  // buffer only ever contains OUR rows — no filtering needed here.
  const deadline = Date.now() + TAIL_SETTLE_MS;
  let result: TailResult = { lines: [], errored: null };
  for (;;) {
    result = await page.evaluate(() => {
      const w = window as unknown as { __tail: TailResult };
      return { lines: [...w.__tail.lines], errored: w.__tail.errored };
    });
    expect(result.errored, 'tail websocket error').toBeNull();
    const uniqueSeen = new Set(result.lines);
    if (uniqueSeen.size >= BURST_N) break;
    if (Date.now() >= deadline) break;
    await new Promise((r) => setTimeout(r, DRAIN_POLL_MS));
  }

  // Close the socket now that we've drained (or timed out).
  await page.evaluate(() => {
    const w = window as unknown as { __tail: { sock: WebSocket } };
    w.__tail.sock.close();
  });

  const nowNs = BigInt(Date.now()) * 1_000_000n;

  // 5. Cross-check: query_range over [t0, now] for the SAME selector,
  // extracting the line set as the independent ground truth.
  const qrResp = await request.get(
    `${CERBERUS_URL}/loki/api/v1/query_range`,
    {
      params: {
        query: selector,
        start: t0NsStr,
        end: nowNs.toString(),
        limit: String(BURST_N * 2),
        direction: 'forward',
      },
    },
  );
  expect(qrResp.status(), 'query_range status').toBe(200);
  const qrBody = (await qrResp.json()) as {
    status?: string;
    data?: { resultType?: string; result?: Array<{ values?: Array<[string, string]> }> };
  };
  expect(qrBody.status, 'query_range envelope status').toBe('success');
  expect(qrBody.data?.resultType, 'query_range resultType').toBe('streams');
  const queryRangeLines = new Set<string>();
  for (const stream of qrBody.data?.result ?? []) {
    for (const v of stream.values ?? []) queryRangeLines.add(v[1]);
  }

  // 6. The regression oracle.
  const streamedUnique = new Set(result.lines);

  // (a) NO DROP — every burst line was streamed. This is the #1011 bug:
  // a truncated overflow window silently dropped its tail. With the fix
  // every one of the BURST_N distinct lines must arrive.
  const missing = [...expected].filter((l) => !streamedUnique.has(l));
  expect(
    missing.length,
    `tail dropped ${missing.length}/${BURST_N} burst lines (overflow lost — the #1011 regression). ` +
      `First few missing: ${missing.slice(0, 5).join(', ')}. ` +
      `Streamed ${streamedUnique.size} unique of ${result.lines.length} total.`,
  ).toBe(0);

  // Sanity: the burst overflowed the limit (proves we exercised the
  // truncation path, not a trivially-small window). Distinct burst lines
  // far exceed the default tail limit.
  expect(
    expected.size,
    'burst did not exceed the tail limit — overflow path not exercised',
  ).toBeGreaterThan(DEFAULT_TAIL_LIMIT);

  // (b) NO DUPLICATE — the cursor +1ns dedup must never re-send a line.
  // unique count == total count means no line arrived twice.
  const dupes = result.lines.filter((l, i) => result.lines.indexOf(l) !== i);
  expect(
    dupes.length,
    `tail re-sent ${dupes.length} duplicate line(s) (cursor dedup broken). ` +
      `Examples: ${[...new Set(dupes)].slice(0, 5).join(', ')}`,
  ).toBe(0);

  // (c) PARITY — the streamed burst set equals the query_range burst set
  // for the same window. Restrict to OUR tagged rows on both sides (the
  // selector already narrows, but this is belt-and-suspenders against an
  // unrelated row sneaking into the unique service name).
  const streamedBurst = [...streamedUnique]
    .filter((l) => expected.has(l))
    .sort();
  const queryRangeBurst = [...queryRangeLines]
    .filter((l) => expected.has(l))
    .sort();
  expect(
    streamedBurst,
    'streamed tail-e2e set != query_range tail-e2e set for the window',
  ).toEqual(queryRangeBurst);
});
