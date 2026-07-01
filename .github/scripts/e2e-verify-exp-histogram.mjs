// e2e-verify-exp-histogram.mjs — assert the collector's exponential-histogram
// write path is queryable end to end through cerberus, run by the compose-smoke
// job AFTER `docker compose up --wait`.
//
// The compose stack makes the OTel collector the SOLE schema authority: its
// clickhouseexporter creates otel_metrics_exponential_histogram, and a
// telemetrygen sidecar feeds OTLP ExponentialHistogram points named
// `e2e_latency_exp_hist` through the collector into that table. This module
// closes the loop cerberus's readiness probe alone can't: readiness proves the
// table EXISTS with the right shape, but not that data written by the collector
// is actually READABLE. It drives cerberus's Prometheus HTTP API with a native
// exponential-histogram quantile and asserts a finite value comes back.
//
// The `_exp_hist` suffix on the metric name routes
// `histogram_quantile(φ, e2e_latency_exp_hist)` onto cerberus's native
// exponential-histogram lowering (schema.ExpHistogramSuffix), so a non-empty,
// finite result proves: collector -> exponential_histogram table -> cerberus
// native quantile all agree.
//
// Poll, don't sample once: telemetrygen's first batch, the collector's insert,
// and CH visibility lag `up --wait` by a few seconds. We retry until a finite
// value appears or the budget elapses, then fail with a clear annotation.
//
// Env contract:
//   CERBERUS_URL       base URL of cerberus's HTTP API   (default http://localhost:8080)
//   EXP_HIST_METRIC    metric name telemetrygen emits    (default e2e_latency_exp_hist)
//   POLL_SECONDS       total wait budget in seconds      (default 120)
//
// Exit 0 = the quantile returned a finite value; 1 = it did not within budget
// (with a ::error:: annotation).

import process from 'node:process';
import { setTimeout as sleep } from 'node:timers/promises';
import { error, notice, log } from './lib/gh.mjs';

const CERBERUS_URL = (process.env.CERBERUS_URL || 'http://localhost:8080').replace(/\/$/, '');
const METRIC = process.env.EXP_HIST_METRIC || 'e2e_latency_exp_hist';
const POLL_SECONDS = Number(process.env.POLL_SECONDS || '120');

// Gap between query attempts. Short enough that the step returns promptly once
// the data is visible, long enough not to hammer the API during the boot race.
const POLL_INTERVAL_MS = 3000;
const QUANTILE = 0.9;

const query = `histogram_quantile(${QUANTILE}, ${METRIC})`;

// queryValue runs one instant query and returns the first sample value as a
// finite number, or null if the API errored, returned no series, or returned a
// non-finite value (NaN/±Inf). It never throws on a transient network/HTTP
// failure — those are expected during the boot race and drive a retry.
async function queryValue() {
  const url = `${CERBERUS_URL}/api/v1/query?query=${encodeURIComponent(query)}`;
  let res;
  try {
    res = await fetch(url);
  } catch (e) {
    log(`query fetch failed (retrying): ${e.message}`);
    return null;
  }
  if (!res.ok) {
    log(`query HTTP ${res.status} (retrying)`);
    return null;
  }
  let body;
  try {
    body = await res.json();
  } catch (e) {
    log(`query body not JSON (retrying): ${e.message}`);
    return null;
  }
  if (body.status !== 'success') {
    log(`query status=${body.status} error=${body.error || ''} (retrying)`);
    return null;
  }
  const result = body.data && body.data.result;
  if (!Array.isArray(result) || result.length === 0) {
    return null;
  }
  // Instant vector: each series carries `value: [ts, "stringValue"]`.
  const raw = result[0].value && result[0].value[1];
  const num = Number(raw);
  if (!Number.isFinite(num)) {
    return null;
  }
  return num;
}

async function main() {
  const deadline = Date.now() + POLL_SECONDS * 1000;
  let attempts = 0;
  for (;;) {
    attempts += 1;
    const value = await queryValue();
    if (value !== null) {
      notice(
        `exp-histogram queryable through cerberus: ${query} = ${value} ` +
          `(after ${attempts} attempt(s))`,
      );
      return 0;
    }
    if (Date.now() >= deadline) {
      error(
        `exp-histogram NOT queryable through cerberus after ${POLL_SECONDS}s: ` +
          `${query} returned no finite value. The collector's clickhouseexporter ` +
          `should have written telemetrygen's ExponentialHistogram points to ` +
          `otel_metrics_exponential_histogram, and cerberus's native path should ` +
          `read them back. Check the telemetrygen-exphist + otel-collector logs ` +
          `and that cerberus's exp-histogram table name matches the exporter's.`,
      );
      return 1;
    }
    await sleep(POLL_INTERVAL_MS);
  }
}

process.exit(await main());
