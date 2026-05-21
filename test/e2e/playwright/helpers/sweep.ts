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
 * enough to populate every by-language bucket. We use Playwright's
 * `APIRequestContext` (passed in by the spec) rather than `fetch` so
 * the helper inherits the Playwright proxy / CI-network plumbing.
 */

import type { APIRequestContext } from '@playwright/test';

const DEFAULT_CERBERUS_URL = 'http://localhost:8080';

/**
 * Generate self-traffic against cerberus for `durationSec` seconds.
 *
 * Returns when the duration elapses. Errors from individual requests
 * are tolerated — the helper's job is to nudge metrics, not to
 * assert correctness. The caller's spec sweep is the assertion layer.
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

  let i = 0;
  while (Date.now() < deadline) {
    const prom = promQueries[i % promQueries.length] ?? promQueries[0]!;
    const log = logQueries[i % logQueries.length] ?? logQueries[0]!;
    const trace = traceQueries[i % traceQueries.length] ?? traceQueries[0]!;
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
    ]);

    // Yield between iterations so the loop doesn't pin a CPU and so
    // the CH writer has time to flush.
    await new Promise((r) => setTimeout(r, 250));
  }
}
