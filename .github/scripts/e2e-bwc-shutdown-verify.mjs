// e2e-bwc-shutdown-verify.mjs — real-cluster proof of the bundled ClickHouse
// graceful-shutdown contract (docs/helm-clickhouse.md § "Graceful shutdown").
// Run by `just e2e-bwc-shutdown-verify` after `just e2e-bwc-replicated-up`.
//
// What it asserts, against the live StatefulSet the chart rendered:
//
//   1. Budget. The pods' terminationGracePeriodSeconds exceeds the server's
//      own shutdown_wait_unfinished, read from system.server_settings on a
//      running pod, and shutdown_wait_unfinished_queries is on. BOUNDED_QUERY_SECONDS
//      must fit inside that wait, or the drain below would prove nothing.
//   2. Drain. A bounded query runs against the last pod from a client in
//      another pod (as cerberus would); once the query is in
//      system.processes the pod is deleted. The query must return its full
//      result, and the old pod must be gone before its grace period ends —
//      a pod that outlives its grace is SIGKILLed, so disappearing earlier
//      means the server exited on its own.
//   3. Recovery. The replacement pod turns Ready and its replicated tables
//      leave read-only mode.
//   4. Rolling update. `kubectl rollout restart` of the StatefulSet completes
//      and every pod is Ready and writable again.
//
// Env contract:
//   NAMESPACE                  k8s namespace                          (default cerberus)
//   STATEFULSET                the bundled ClickHouse StatefulSet     (default cerberus-clickhouse)
//   HEADLESS_SERVICE           its headless Service                   (default cerberus-clickhouse-headless)
//   CH_USER / CH_PASSWORD      ClickHouse credentials                 (default cerberus/cerberus)
//   BOUNDED_QUERY_SECONDS      duration of the in-flight query        (default 20)
//   RECOVERY_SECONDS           bounded wait for Ready + writable      (default 300)
//   ROLLOUT_SECONDS            bounded wait for the rolling update    (default 900)
//
// Exit 0 = every assertion passed; 1 = any failed (with ::error:: annotation).
//
// node: builtins only (via lib/gh.mjs / lib/k8s.mjs / lib/poll.mjs).

import { spawn } from 'node:child_process';
import process from 'node:process';
import { pathToFileURL } from 'node:url';
import { error, notice, log, capture } from './lib/gh.mjs';
import { makeKubectl, chQuery, chQueryRaw } from './lib/k8s.mjs';
import { pollUntil } from './lib/poll.mjs';

// The bounded query sleeps this long per row, one row per block, so its
// duration is BOUNDED_QUERY_SECONDS whatever the server's block size.
export const SLEEP_PER_ROW_SECONDS = 0.5;

// POLL_INTERVAL_MS paces the two sub-second waits: the query appearing in
// system.processes and the terminating pod disappearing, whose timing is the
// measurement.
const POLL_INTERVAL_MS = 500;

// shutdownBudgetDefects checks what the rendered pods and the running server
// say about the shutdown budget, before anything is terminated.
export function shutdownBudgetDefects({ grace, wait, drain, querySeconds }) {
  const defects = [];
  if (!Number.isInteger(grace) || !Number.isInteger(wait)) {
    return [`could not read the budget: terminationGracePeriodSeconds=${grace}, shutdown_wait_unfinished=${wait}`];
  }
  if (drain !== 1) {
    defects.push(`shutdown_wait_unfinished_queries=${drain}: the server cancels running queries on SIGTERM instead of letting them finish`);
  }
  if (grace <= wait) {
    defects.push(`terminationGracePeriodSeconds=${grace} does not exceed shutdown_wait_unfinished=${wait}: Kubernetes kills the server while it still waits for queries`);
  }
  if (querySeconds >= wait) {
    defects.push(`BOUNDED_QUERY_SECONDS=${querySeconds} does not fit in shutdown_wait_unfinished=${wait}; the drain check would prove nothing`);
  }
  return defects;
}

// terminationDefects judges one delete-under-load: the client's view of its
// query and how long the old pod took to disappear.
export function terminationDefects({ clientStatus, clientOutput, expectedRows, goneAfterSeconds, grace }) {
  const defects = [];
  if (clientStatus !== 0 || clientOutput.trim() !== String(expectedRows)) {
    defects.push(`the in-flight query did not complete across the termination: exit ${clientStatus}, output ${JSON.stringify(clientOutput.trim())}, want ${expectedRows}`);
  }
  if (!(goneAfterSeconds < grace)) {
    defects.push(`the terminating pod took ${goneAfterSeconds}s to go, not less than its ${grace}s grace: it was killed, not shut down`);
  }
  return defects;
}

const NS = process.env.NAMESPACE || 'cerberus';
const STATEFULSET = process.env.STATEFULSET || 'cerberus-clickhouse';
const HEADLESS = process.env.HEADLESS_SERVICE || 'cerberus-clickhouse-headless';
const CH_USER = process.env.CH_USER || 'cerberus';
const CH_PASSWORD = process.env.CH_PASSWORD || 'cerberus';
const BOUNDED_QUERY_SECONDS = Number(process.env.BOUNDED_QUERY_SECONDS || '20');
const RECOVERY_SECONDS = Number(process.env.RECOVERY_SECONDS || '300');
const ROLLOUT_SECONDS = Number(process.env.ROLLOUT_SECONDS || '900');

const kubectl = makeKubectl(capture, NS);
const CH_OPTS = { user: CH_USER, password: CH_PASSWORD };
const QUERY_MARKER = 'e2e-bwc-shutdown-verify';

function fail(lines) {
  for (const line of lines) error(line);
  process.exit(1);
}

function jsonpath(args, path) {
  const res = kubectl([...args, '-o', `jsonpath=${path}`]);
  if (res.status !== 0) fail([`kubectl ${args.join(' ')}: ${res.stderr.trim()}`]);
  return res.stdout.trim();
}

function clickhousePods() {
  return jsonpath(['get', 'pod', '-l', 'app.kubernetes.io/component=clickhouse'], '{.items[*].metadata.name}')
    .split(/\s+/)
    .filter((n) => n.length > 0)
    .sort();
}

function serverSetting(pod, name) {
  return Number(chQuery(kubectl, pod, CH_OPTS, `SELECT value FROM system.server_settings WHERE name = '${name}'`));
}

function podUID(pod) {
  const res = kubectl(['get', 'pod', pod, '-o', 'jsonpath={.metadata.uid}']);
  return res.status === 0 ? res.stdout.trim() : '';
}

// startBoundedQuery runs the bounded query against target from inside client,
// asynchronously, and resolves with the client's exit status and output.
function startBoundedQuery(client, target, rows) {
  const sql = `SELECT count() FROM (SELECT sleepEachRow(${SLEEP_PER_ROW_SECONDS}) FROM numbers(${rows})) ` +
    `SETTINGS max_block_size = 1, log_comment = '${QUERY_MARKER}'`;
  const child = spawn('kubectl', ['-n', NS, 'exec', client, '--', 'clickhouse-client',
    '--host', `${target}.${HEADLESS}`, '--user', CH_USER, '--password', CH_PASSWORD, '--query', sql]);
  let output = '';
  child.stdout.on('data', (d) => { output += d; });
  child.stderr.on('data', (d) => { output += d; });
  return new Promise((resolve) => child.on('close', (status) => resolve({ status, output })));
}

async function waitReadyAndWritable(pod, label) {
  const ready = kubectl(['wait', '--for=condition=Ready', `pod/${pod}`, `--timeout=${RECOVERY_SECONDS}s`]);
  if (ready.status !== 0) fail([`${label}: pod ${pod} did not turn Ready within ${RECOVERY_SECONDS}s: ${ready.stderr.trim()}`]);
  const writable = await pollUntil(
    async () => {
      const res = chQueryRaw(kubectl, pod, CH_OPTS, 'SELECT count() FROM system.replicas WHERE is_readonly OR is_session_expired');
      return res.status === 0 && res.stdout.trim() === '0';
    },
    { deadlineMs: RECOVERY_SECONDS * 1000, label: `writable ${pod}` },
  );
  if (!writable) fail([`${label}: replicated tables on ${pod} stayed read-only for ${RECOVERY_SECONDS}s`]);
}

async function main() {
  const pods = clickhousePods();
  if (pods.length < 2) fail([`need >= 2 ClickHouse pods to run a client beside the terminated one, found ${pods.join(', ')}`]);
  const target = pods[pods.length - 1];
  const client = pods[0];

  // ---- 1. budget ----------------------------------------------------------
  const grace = Number(jsonpath(['get', 'statefulset', STATEFULSET], '{.spec.template.spec.terminationGracePeriodSeconds}'));
  const wait = serverSetting(target, 'shutdown_wait_unfinished');
  const drain = serverSetting(target, 'shutdown_wait_unfinished_queries');
  log(`shutdown verify: statefulset=${STATEFULSET} grace=${grace}s shutdown_wait_unfinished=${wait}s drain=${drain} target=${target} client=${client}`);
  const budget = shutdownBudgetDefects({ grace, wait, drain, querySeconds: BOUNDED_QUERY_SECONDS });
  if (budget.length > 0) fail(budget);

  // ---- 2. drain -------------------------------------------------------------
  const rows = Math.round(BOUNDED_QUERY_SECONDS / SLEEP_PER_ROW_SECONDS);
  const oldUID = podUID(target);
  const query = startBoundedQuery(client, target, rows);
  const running = await pollUntil(
    async () => {
      const res = chQueryRaw(kubectl, target, CH_OPTS,
        `SELECT count() FROM system.processes WHERE query LIKE '%${QUERY_MARKER}%' AND query NOT LIKE '%system.processes%'`);
      return res.status === 0 && Number(res.stdout.trim()) > 0;
    },
    { deadlineMs: BOUNDED_QUERY_SECONDS * 1000, intervalMs: POLL_INTERVAL_MS, label: 'query running' },
  );
  if (!running) fail([`the bounded query never showed up in system.processes on ${target}`]);

  const deletedAt = Date.now();
  const del = kubectl(['delete', 'pod', target, '--wait=false']);
  if (del.status !== 0) fail([`kubectl delete pod ${target}: ${del.stderr.trim()}`]);
  const gone = await pollUntil(async () => podUID(target) !== oldUID, {
    deadlineMs: (grace + RECOVERY_SECONDS) * 1000,
    intervalMs: POLL_INTERVAL_MS,
    label: `pod ${target} replaced`,
  });
  const goneAfterSeconds = (Date.now() - deletedAt) / 1000;
  if (!gone) fail([`pod ${target} (uid ${oldUID}) was still present ${goneAfterSeconds}s after delete`]);
  const { status, output } = await query;
  const drainDefects = terminationDefects({ clientStatus: status, clientOutput: output, expectedRows: rows, goneAfterSeconds, grace });
  if (drainDefects.length > 0) fail(drainDefects);
  log(`  in-flight ${BOUNDED_QUERY_SECONDS}s query completed; ${target} exited ${goneAfterSeconds.toFixed(1)}s after delete (grace ${grace}s)`);

  // ---- 3. recovery ----------------------------------------------------------
  await waitReadyAndWritable(target, 'restart');
  log(`  ${target} Ready and writable again`);

  // ---- 4. rolling update ------------------------------------------------------
  const restart = kubectl(['rollout', 'restart', `statefulset/${STATEFULSET}`]);
  if (restart.status !== 0) fail([`kubectl rollout restart: ${restart.stderr.trim()}`]);
  const status4 = kubectl(['rollout', 'status', `statefulset/${STATEFULSET}`, `--timeout=${ROLLOUT_SECONDS}s`]);
  if (status4.status !== 0) fail([`rolling update of statefulset/${STATEFULSET} did not complete within ${ROLLOUT_SECONDS}s: ${status4.stderr.trim()}`]);
  for (const pod of clickhousePods()) await waitReadyAndWritable(pod, 'rolling update');
  log('  rolling update completed; every pod Ready and writable');

  notice(
    `bwc shutdown verify PASSED: grace ${grace}s > shutdown_wait_unfinished ${wait}s, an in-flight query survived ` +
      `the termination of ${target} (gone after ${goneAfterSeconds.toFixed(1)}s), and a rolling update completed`,
    { title: 'e2e-bwc-shutdown-verify' },
  );
}

// Only dispatch when run as a script — importing for the unit test must not
// exit the test runner or shell out to kubectl.
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch((e) => {
    error(String(e?.stack || e));
    process.exit(1);
  });
}
