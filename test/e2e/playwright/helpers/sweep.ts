/**
 * Self-traffic generator.
 *
 * Several Grafana dashboards (notably cerberus) only render
 * meaningfully when cerberus has just served real queries — the
 * `cerberus_queries_total` counter, the `cerberus_query_duration_*`
 * histogram, and the by-language partition all stay flat at 0 on a
 * fresh compose stack. Without seed traffic the phase-1 label-shape
 * rule would fire false positives: "panel collapsed to 0 series" is
 * indistinguishable from "no traffic yet" on the wire.
 *
 * The pre-step fires N curl-equivalent HTTP requests against
 * cerberus's three heads in a tight loop for `durationSec`. The
 * traffic shape is deliberately broad: one PromQL `instant_query`,
 * one LogQL `query_range`, one TraceQL `search` per iteration —
 * enough to populate every by-language bucket. Each iteration also
 * fires one deliberately-malformed query per head so the
 * `cerberus_queries_total{result="error"}` series ticks too. Without
 * the error-path probes the "Error rate by language" panel on
 * `cerberus-self` evaluates `numerator / clamp_min(denominator, 1e-9)`
 * with numerator=0 over the warmup window, producing an empty result
 * the iterate-all-dashboards sweep now hard-asserts against.
 *
 * Before the steady-state loop the helper also fires an
 * admission-burst phase — ADMIT_BURST_CONCURRENCY parallel requests
 * per head — so the per-head admit semaphores in
 * internal/api/admit saturate and cerberus ticks
 * `cerberus_admit_rejected_total{reason="cap_exceeded"}` on every
 * (cerberus_ql, reason) bucket. Without the burst the cerberus-self
 * "Admission rejections" panel has nothing to graph and the sweep's
 * empty-result guard fires.
 *
 * We use Playwright's `APIRequestContext` (passed in by the spec)
 * rather than `fetch` so the helper inherits the Playwright proxy /
 * CI-network plumbing.
 */

import type { APIRequestContext } from '@playwright/test';

const DEFAULT_CERBERUS_URL = 'http://localhost:8080';

// ADMIT_BURST_CONCURRENCY is the number of parallel requests we fan
// out per head in the upfront admission-burst phase. The cerberus
// admit middleware (internal/api/admit) caps in-flight requests per
// head — defaults are 64 (prom), 64 (loki), 32 (tempo). Firing this
// many in one Promise.allSettled batch guarantees we exceed every
// cap on every head, so cerberus emits cerberus_admit_rejected_total
// with reason=cap_exceeded (the only reason value the limiter has
// today; if a future rejection path appears in internal/api/admit
// it should grow its own burst phase below).
//
// Without this phase the cerberus-self "Admission rejections" panel
// (sum by (cerberus_ql, reason) (rate(cerberus_admit_rejected_total[5m])))
// stays empty over the warmup window — healthy compose traffic never
// approaches the cap.
const ADMIT_BURST_CONCURRENCY = 128;

/**
 * Generate self-traffic against cerberus for `durationSec` seconds.
 *
 * Returns when the duration elapses. The helper's job is to nudge
 * metrics, not to assert correctness — individual request errors
 * are discarded so the caller's spec sweep (the assertion layer)
 * sees the resulting state of the counters rather than per-call
 * flake.
 *
 * Reads `CERBERUS_URL` env var (default http://localhost:8080) to
 * find cerberus directly. The compose stack publishes that port
 * alongside Grafana's 3000.
 */
export async function generateSelfTraffic(
  request: APIRequestContext,
  durationSec: number,
): Promise<void> {
  const cerberusURL = process.env.CERBERUS_URL ?? DEFAULT_CERBERUS_URL;
  const deadline = Date.now() + durationSec * 1000;

  // Admission-burst phase — fire ADMIT_BURST_CONCURRENCY parallel
  // requests per head so the per-head admit semaphores saturate and
  // cerberus rejects the overflow with HTTP 503 + ticks
  // cerberus_admit_rejected_total{reason="cap_exceeded"}. We use the
  // cheapest endpoint per head (a constant PromQL `up` query, the
  // Loki label-values endpoint, the TraceQL "all-traces" search) so
  // the burst dominantly stresses the limiter rather than the
  // ClickHouse query path. Promise.allSettled swallows both the
  // success and the 503 responses — the goal is to nudge the
  // rejection counter, not to assert.
  const burst: Promise<unknown>[] = [];
  for (let k = 0; k < ADMIT_BURST_CONCURRENCY; k++) {
    burst.push(
      request.get(`${cerberusURL}/api/v1/query?query=${encodeURIComponent('up')}`),
      request.get(`${cerberusURL}/loki/api/v1/labels`),
      request.get(`${cerberusURL}/api/search?q=${encodeURIComponent('{}')}`),
    );
  }
  await Promise.allSettled(burst);

  // Broad probes against each head — these need only succeed
  // *sometimes*, to nudge the counters. We don't await each
  // round-trip; we sleep ~250ms between iterations so the loop
  // doesn't saturate the box.
  const promQueries = [
    'up',
    'cerberus_queries_total',
    'rate(cerberus_queries_total[1m])',
    'sum by (cerberus_ql) (rate(cerberus_queries_total[1m]))',
  ];
  const logQueries = ['{service_name=~".+"}'];
  const traceQueries = ['{}'];

  // Deliberately-malformed probes — one per QL head — so the
  // `cerberus_queries_total{result="error"}` series populates every
  // by(cerberus_ql) bucket. Each expression is invalid syntax that
  // every upstream reference parser rejects at the parse stage, so
  // cerberus returns 4xx and the telemetry middleware records the
  // request under result="error". Without these the "Error rate by
  // language" cerberus-self panel evaluates with a zero numerator
  // over the warmup window and the panel comes back empty.
  const promErrorQueries = ['sum by (', 'rate(cerberus_queries_total[no'];
  const logErrorQueries = ['{ unterminated', 'count_over_time({a="b"}['];
  const traceErrorQueries = ['{', '{ .foo = '];

  let i = 0;
  while (Date.now() < deadline) {
    const prom = promQueries[i % promQueries.length] ?? promQueries[0]!;
    const log = logQueries[i % logQueries.length] ?? logQueries[0]!;
    const trace = traceQueries[i % traceQueries.length] ?? traceQueries[0]!;
    const promErr =
      promErrorQueries[i % promErrorQueries.length] ?? promErrorQueries[0]!;
    const logErr =
      logErrorQueries[i % logErrorQueries.length] ?? logErrorQueries[0]!;
    const traceErr =
      traceErrorQueries[i % traceErrorQueries.length] ?? traceErrorQueries[0]!;
    i++;

    await Promise.allSettled([
      request.get(
        `${cerberusURL}/api/v1/query?query=${encodeURIComponent(prom)}`,
      ),
      request.get(
        `${cerberusURL}/loki/api/v1/query_range?query=${encodeURIComponent(log)}`,
      ),
      request.get(
        `${cerberusURL}/api/search?q=${encodeURIComponent(trace)}`,
      ),
      // Error-path probes — drive the result="error" bucket so the
      // cerberus-self "Error rate by language" panel has a non-zero
      // numerator. Promise.allSettled() swallows the 4xx response
      // bodies just like the success-path probes above.
      request.get(
        `${cerberusURL}/api/v1/query?query=${encodeURIComponent(promErr)}`,
      ),
      request.get(
        `${cerberusURL}/loki/api/v1/query_range?query=${encodeURIComponent(logErr)}`,
      ),
      request.get(
        `${cerberusURL}/api/search?q=${encodeURIComponent(traceErr)}`,
      ),
    ]);

    // Yield between iterations so the loop doesn't pin a CPU and so
    // the CH writer has time to flush.
    await new Promise((r) => setTimeout(r, 250));
  }
}

// Each probe expression must yield at least this many range points
// before we treat the underlying series as rate()-able. Two samples
// are the arithmetic floor for `rate(x[5m])` to emit a single point;
// we demand a third as step-alignment margin (see the comment in
// awaitSelfTelemetryExprSignal) so "probe passed" implies "any
// consumer window over the same range has data".
const MIN_SELF_TELEMETRY_POINTS = 3;

// How far in the PAST the probe window ends. Grafana's Prometheus
// plugin backend step-aligns the replay window, which can exclude the
// single newest evaluation step — ending the probe window here keeps
// the probe and any consumer panel looking at the same settled range.
const SELF_TELEMETRY_PROBE_LAG_SEC = 30;

// Width of the probe's range window, matching the panel's [5m] rate
// lookback so the probe exercises the same number of in-window samples
// the consumer panel needs.
const SELF_TELEMETRY_PROBE_WINDOW_SEC = 300;

// Poll cadence while waiting for a self-telemetry expression to become
// rate()-able. One OTel export cycle is 10s; 5s keeps the loop
// responsive without hammering cerberus.
const SELF_TELEMETRY_POLL_INTERVAL_MS = 5_000;

/**
 * Block until a single cerberus self-telemetry expression supports
 * rate()-over-range queries (>= MIN_SELF_TELEMETRY_POINTS points), or
 * throw after `deadlineSec`.
 *
 * Polls cerberus DIRECTLY (not through Grafana) on purpose: it
 * isolates "the data exists on the wire" from "the consumer decodes
 * it". The deadline failure is loud and actionable, never a skip.
 */
async function awaitSelfTelemetryExprSignal(
  request: APIRequestContext,
  expr: string,
  deadlineSec: number,
): Promise<void> {
  const cerberusURL = process.env.CERBERUS_URL ?? DEFAULT_CERBERUS_URL;
  const deadline = Date.now() + deadlineSec * 1000;
  let lastBody = '';
  for (;;) {
    // The probe window deliberately ends SELF_TELEMETRY_PROBE_LAG_SEC
    // in the PAST and demands MIN_SELF_TELEMETRY_POINTS points: a
    // probe satisfied by one fresh point green-lit a replay that
    // still saw zero rows (reproduced on the 2026-06-10 cold-boot
    // verification). Requiring margin makes "probe passed" imply "any
    // consumer window over the same range has data".
    const nowSec = Math.floor(Date.now() / 1000) - SELF_TELEMETRY_PROBE_LAG_SEC;
    const url =
      `${cerberusURL}/api/v1/query_range?query=${encodeURIComponent(expr)}` +
      `&start=${nowSec - SELF_TELEMETRY_PROBE_WINDOW_SEC}&end=${nowSec}&step=15`;
    try {
      const resp = await request.get(url);
      lastBody = await resp.text();
      if (resp.status() === 200) {
        const parsed = JSON.parse(lastBody) as {
          data?: { result?: Array<{ values?: unknown[] }> };
        };
        const points = (parsed.data?.result ?? []).reduce(
          (acc, s) => acc + (s.values?.length ?? 0),
          0,
        );
        if (points >= MIN_SELF_TELEMETRY_POINTS) return;
      }
    } catch {
      // transient — the deadline below is the failure path
    }
    if (Date.now() >= deadline) {
      throw new Error(
        `awaitSelfTelemetryExprSignal: ${expr} returned <${MIN_SELF_TELEMETRY_POINTS} points within ${deadlineSec}s of seed traffic — ` +
          `cerberus self-telemetry is not reaching ClickHouse (seed, OTel export, or ingest regression). ` +
          `Last response: ${lastBody.slice(0, 400)}`,
      );
    }
    await new Promise((r) => setTimeout(r, SELF_TELEMETRY_POLL_INTERVAL_MS));
  }
}

/**
 * Block until cerberus's own self-telemetry supports rate()-over-range
 * queries, or throw after `deadlineSec`.
 *
 * Why generateSelfTraffic alone is not enough: it guarantees REQUESTS
 * happened, but the metrics they tick reach ClickHouse on the OTel
 * export cadence — and `rate(x[5m])` needs at least TWO exported
 * samples inside the lookback window before it yields a single point.
 * On a freshly-booted compose stack (the exact state the compose-smoke
 * CI job creates: `docker compose up --wait` then straight into
 * Playwright) the first range probe can land when only one sample
 * exists, and a range query over guaranteed-seeded traffic comes back
 * EMPTY — adversarial verification on 2026-06-10 reproduced exactly
 * that red on a healthy stack at ~2min uptime.
 *
 * We gate on TWO expressions, both of which the cerberus-self "Error
 * rate by language" panel depends on (flake #89):
 *   - the AGGREGATE denominator  sum(rate(cerberus_queries_total[5m]))
 *   - the sparser NUMERATOR
 *       sum by (cerberus_ql) (rate(cerberus_queries_total{result="error"}[5m]))
 * The numerator is partitioned by (cerberus_ql) AND filtered to the
 * error path, so a single (result="error", cerberus_ql) bucket can lag
 * the aggregate by an export cycle. Gating on the aggregate alone left
 * a residual race where the denominator was rate()-able but the
 * numerator wasn't yet — the panel's `numerator / clamp_min(denom, …)`
 * then yields no series and renders "No data". Probing the exact panel
 * numerator closes that gap: once this returns, an empty "Error rate
 * by language" panel is a real bug, not a boot race.
 *
 * The wait polls cerberus DIRECTLY (not through Grafana) on purpose:
 * it isolates "the data exists on the wire" from "the consumer decodes
 * it", so the caller's plugin-backend / lint assertions stay
 * unconditional — once this returns, an empty frame set or a vacuous
 * lint pass is a real bug, never a boot race. The deadline failure is
 * loud and actionable (a seed/export regression), never a skip.
 */
export async function awaitSelfTelemetryRangeSignal(
  request: APIRequestContext,
  deadlineSec = 120,
): Promise<void> {
  // The aggregate denominator the panel divides by, plus the strictly
  // sparser by-language error numerator. Both must be rate()-able
  // before the consumer panel can render a non-empty frame.
  const exprs = [
    'sum(rate(cerberus_queries_total[5m]))',
    'sum by (cerberus_ql) (rate(cerberus_queries_total{result="error"}[5m]))',
  ];
  for (const expr of exprs) {
    await awaitSelfTelemetryExprSignal(request, expr, deadlineSec);
  }
}
