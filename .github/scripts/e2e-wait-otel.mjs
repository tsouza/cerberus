// e2e-wait-otel.mjs — block until the OTel collector has populated every
// signal table in ClickHouse with a real spread of history, extracted from
// `just e2e-wait-otel`'s inline bash (cerberus issue #3096, epic #3091
// phase 5).
//
// Bootstraps the pipeline before tests rely on it: telemetrygen +
// kubeletstats take ~30-60s to flush a first batch through the gateway, and
// 1m-windowed queries (`rate(x[1m])`, `up[1m:30s]`) need a
// MIN_HISTORY_SECONDS span of TimeUnix values before they return a vector.
// Spread is asserted on whichever metric table (sum or gauge) carries
// non-zero rows first — the histogram companion (see below) is asserted
// independently regardless of which of those two is live.
//
// Uses `kubectl exec` against the ClickHouse Deployment directly (never a
// host-side port-forward, unlike e2e-seed.mjs) via lib/k8s.mjs's
// `chQueryRaw` — a poll iteration where ClickHouse briefly refuses a query
// (not ready yet) is deliberately NOT a hard failure, matching the
// extracted bash's `|| echo 0` on every query.
//
// Env contract:
//   NAMESPACE            k8s namespace                         (default cerberus)
//   CLICKHOUSE_TARGET    kubectl exec target                    (default deploy/clickhouse)
//   DATABASE             ClickHouse database                    (default otel)
//   CH_USER / CH_PASSWORD  ClickHouse credentials                (default cerberus/cerberus)
//   HISTOGRAM_METRIC     the histogram companion metric to require (default http_server_request_duration)
//   POLL_SECONDS         total poll budget                      (default 180)
//   POLL_INTERVAL_SECONDS  seconds between poll attempts         (default 5)
//   MIN_HISTORY_SECONDS  minimum required TimeUnix spread        (default 60)
//
// Exit: 0 once every signal table is populated with >= MIN_HISTORY_SECONDS
// of spread; 1 on timeout.

import process from 'node:process';
import { setTimeout as sleep } from 'node:timers/promises';

import { capture, error, log, notice } from './lib/gh.mjs';
import { makeKubectl, chQueryRaw } from './lib/k8s.mjs';

const NAMESPACE = process.env.NAMESPACE || 'cerberus';
const CLICKHOUSE_TARGET = process.env.CLICKHOUSE_TARGET || 'deploy/clickhouse';
const DATABASE = process.env.DATABASE || 'otel';
const CH_USER = process.env.CH_USER || 'cerberus';
const CH_PASSWORD = process.env.CH_PASSWORD || 'cerberus';
const HISTOGRAM_METRIC = process.env.HISTOGRAM_METRIC || 'http_server_request_duration';
const POLL_SECONDS = Number(process.env.POLL_SECONDS || '180');
const POLL_INTERVAL_SECONDS = Number(process.env.POLL_INTERVAL_SECONDS || '5');
const MIN_HISTORY_SECONDS = Number(process.env.MIN_HISTORY_SECONDS || '60');

// count — run `sql` against ClickHouse, returning 0 on any non-numeric or
// failed result (mirrors the extracted bash's `... || echo 0`: a transient
// exec failure while the pod is still starting is "no data yet", not a
// hard error the poll loop should abort on).
function count(kubectl, sql) {
  const res = chQueryRaw(kubectl, CLICKHOUSE_TARGET, { database: DATABASE, user: CH_USER, password: CH_PASSWORD }, sql);
  if (res.status !== 0) return 0;
  const n = Number(res.stdout.trim());
  return Number.isFinite(n) ? n : 0;
}

// pipelineReady — the exact success predicate the extracted bash checked.
// Pure so this decision is unit-testable without a cluster.
export function pipelineReady(c) {
  return (
    c.logs > 0 &&
    c.chlogs > 0 &&
    c.traces > 0 &&
    (c.sum > 0 || c.gauge > 0) &&
    c.spread >= MIN_HISTORY_SECONDS &&
    c.histogram > 0 &&
    c.histSpread >= MIN_HISTORY_SECONDS
  );
}

function pollOnce(kubectl) {
  const logs = count(kubectl, 'SELECT count() FROM otel_logs');
  const chlogs = count(kubectl, "SELECT count() FROM otel_logs WHERE ServiceName = 'clickhouse'");
  const traces = count(kubectl, 'SELECT count() FROM otel_traces');
  const sum = count(kubectl, 'SELECT count() FROM otel_metrics_sum');
  const gauge = count(kubectl, 'SELECT count() FROM otel_metrics_gauge');
  const histogram = count(
    kubectl,
    `SELECT count() FROM otel_metrics_histogram WHERE MetricName = '${HISTOGRAM_METRIC}'`,
  );
  const histSpread =
    histogram > 0
      ? count(
          kubectl,
          `SELECT toUInt64(dateDiff('second', min(TimeUnix), max(TimeUnix))) FROM otel_metrics_histogram WHERE MetricName = '${HISTOGRAM_METRIC}'`,
        )
      : 0;
  let spread = 0;
  if (sum > 0) {
    spread = count(kubectl, "SELECT toUInt64(dateDiff('second', min(TimeUnix), max(TimeUnix))) FROM otel_metrics_sum");
  } else if (gauge > 0) {
    spread = count(kubectl, "SELECT toUInt64(dateDiff('second', min(TimeUnix), max(TimeUnix))) FROM otel_metrics_gauge");
  }
  return { logs, chlogs, traces, sum, gauge, histogram, histSpread, spread };
}

async function main() {
  const kubectl = makeKubectl(capture, NAMESPACE);
  const deadline = Date.now() + POLL_SECONDS * 1000;

  while (Date.now() < deadline) {
    const c = pollOnce(kubectl);
    log(
      `    logs=${c.logs} chlogs=${c.chlogs} traces=${c.traces} metrics_sum=${c.sum} ` +
        `metrics_gauge=${c.gauge} metrics_histogram=${c.histogram} spread=${c.spread}s hist_spread=${c.histSpread}s`,
    );
    if (pipelineReady(c)) {
      notice(
        `OTel pipeline is live with >=${MIN_HISTORY_SECONDS}s of metric history ` +
          '(incl. histogram companion + clickhouse query_log stream)',
      );
      return true;
    }
    await sleep(POLL_INTERVAL_SECONDS * 1000);
  }
  return false;
}

function isMain() {
  const invoked = process.argv[1] || '';
  return invoked.endsWith('e2e-wait-otel.mjs');
}

if (isMain()) {
  log(
    `==> waiting for real OTel data (incl. clickhouse query_log stream) + >=${MIN_HISTORY_SECONDS}s ` +
      'metric history in ClickHouse',
  );
  const ready = await main();
  if (!ready) {
    error('timeout waiting for OTel data / metric history span');
    process.exit(1);
  }
  process.exit(0);
}
