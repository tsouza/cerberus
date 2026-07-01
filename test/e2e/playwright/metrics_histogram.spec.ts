import { test, expect, type APIRequestContext } from '@playwright/test';

/**
 * Histogram write-path spec (native exponential + classic explicit-bucket).
 *
 * The compose stack makes the OTel collector the SOLE schema authority: its
 * clickhouseexporter creates the metric tables, and two telemetrygen sidecars
 * feed the collector both histogram shapes cerberus reads back —
 *
 *   • telemetrygen-exphist     → OTLP ExponentialHistogram points named
 *                                `e2e_latency_exp_hist`, persisted into
 *                                otel_metrics_exponential_histogram; cerberus's
 *                                native path resolves them.
 *   • telemetrygen-classichist → OTLP classic Histogram points named
 *                                `e2e_latency_classic_hist`, persisted into
 *                                otel_metrics_histogram; cerberus reconstructs
 *                                `<name>_bucket` series with `le` labels and
 *                                resolves the classic quantile idiom.
 *
 * A readiness probe proves the tables EXIST with the right shape; these specs
 * close the loop it can't — that data written by the collector is actually
 * READABLE through cerberus's Prometheus HTTP API. Each spec drives cerberus
 * directly (bypassing Grafana) with the quantile form for its histogram shape
 * and asserts a finite value comes back.
 *
 * telemetrygen's payload is nondeterministic, so the assertion is tolerant: a
 * finite quantile, not an exact value. telemetrygen's first batch, the
 * collector's insert, and CH visibility lag `up --wait` by a few seconds, so
 * each spec polls until a finite value appears or the budget elapses.
 */

// Cerberus HTTP base URL — queried directly (not through the Grafana
// datasource proxy) so this spec asserts the write path itself, not the
// Grafana wiring. Absolute URLs passed to request.get override the
// Grafana baseURL from playwright.config.ts (see smoke.spec.ts).
const CERBERUS_URL = process.env.CERBERUS_URL ?? 'http://localhost:8080';

// Quantile both shapes are probed at. A single meaning-bearing literal, named
// so a future change is a one-line edit rather than a scattered magic number.
const QUANTILE = 0.9;

// Rate window over the classic `_bucket` counters — wide enough that a
// continuously emitting telemetrygen source retains at least two samples for
// `rate()`.
const RATE_WINDOW = '5m';

// Metric names the telemetrygen sidecars emit. The suffixes route each query
// onto the matching cerberus lowering: `_exp_hist` → native exponential
// (schema.ExpHistogramSuffix), `_classic_hist` → classic `_bucket`/`le`.
const EXP_HIST_METRIC = 'e2e_latency_exp_hist';
const CLASSIC_HIST_METRIC = 'e2e_latency_classic_hist';

// Native exponential-histogram quantile: the metric is itself the histogram, so
// histogram_quantile reads it without a `rate(...)_bucket` reconstruction.
const EXP_HIST_QUERY = `histogram_quantile(${QUANTILE}, ${EXP_HIST_METRIC})`;

// Classic explicit-bucket quantile: reconstruct `<name>_bucket` series with
// `le` labels and take the rate over them.
const CLASSIC_HIST_QUERY = `histogram_quantile(${QUANTILE}, rate(${CLASSIC_HIST_METRIC}_bucket[${RATE_WINDOW}]))`;

// Poll budget: telemetrygen → collector insert → CH visibility lags
// `docker compose up --wait` by a few seconds. Mirrors the retry budget the
// bespoke verifier used before this moved into the Playwright suite.
const POLL_BUDGET_MS = 120_000;
const POLL_INTERVAL_MS = 3_000;

// queryFiniteValue runs one instant query against cerberus and returns the
// first sample value as a finite number, or null if the API errored, returned
// no series, or returned a non-finite value (NaN/±Inf). It never throws on a
// transient network/HTTP failure — those are expected during the boot race and
// drive a retry.
async function queryFiniteValue(
  request: APIRequestContext,
  query: string,
): Promise<number | null> {
  const url = `${CERBERUS_URL}/api/v1/query?query=${encodeURIComponent(query)}`;
  const res = await request.get(url);
  if (!res.ok()) {
    return null;
  }
  const body = (await res.json()) as {
    status?: string;
    data?: { result?: Array<{ value?: [number, string] }> };
  };
  if (body.status !== 'success') {
    return null;
  }
  const result = body.data?.result;
  if (!Array.isArray(result) || result.length === 0) {
    return null;
  }
  // Instant vector: each series carries `value: [ts, "stringValue"]`.
  const raw = result[0].value?.[1];
  const num = Number(raw);
  return Number.isFinite(num) ? num : null;
}

// pollFiniteQuantile retries the query until it yields a finite value or the
// budget elapses. Uses expect(...).toPass so a persistent empty/non-finite
// result surfaces as a red assertion (never a masked skip).
async function pollFiniteQuantile(
  request: APIRequestContext,
  query: string,
): Promise<void> {
  await expect(async () => {
    const value = await queryFiniteValue(request, query);
    expect(value, `${query} resolved to a finite value`).not.toBeNull();
  }).toPass({ timeout: POLL_BUDGET_MS, intervals: [POLL_INTERVAL_MS] });
}

test('cerberus resolves the native exponential-histogram quantile to a finite value', async ({
  request,
}) => {
  await pollFiniteQuantile(request, EXP_HIST_QUERY);
});

test('cerberus resolves the classic explicit-bucket quantile to a finite value', async ({
  request,
}) => {
  await pollFiniteQuantile(request, CLASSIC_HIST_QUERY);
});
