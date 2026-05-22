/**
 * Self-traffic generator.
 *
 * Several Grafana dashboards (notably cerberus-self) only render
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
