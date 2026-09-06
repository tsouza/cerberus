// e2e-datashard-verify.mjs — real-cluster admission-control + memory-bound
// verification for the multi-data-shard e2e lane (cerberus issue #3079,
// epic #3074). Run by `just e2e-datashard-verify <count>` AFTER
// `just e2e-seed-rolling` (and, for the correctness leg, `just e2e-run` —
// see that recipe's own doc; this script does NOT re-check correctness,
// which is `test/e2e/e2e_datashard_subquery_test.go` + the rest of the Go
// e2e suite's job).
//
// This script proves what `system.query_log` — not cerberus's own HTTP
// responses — actually observed happen inside ClickHouse while a burst of
// concurrent, solver-splitting PromQL/LogQL/TraceQL requests ran against the
// `Distributed` target:
//
//   1. A genuine solver-split (kEff > 1) query reached the Distributed
//      target at all — otherwise the whole leg would be vacuous (it would
//      "pass" whether or not DataShardFanoutGate does anything).
//   2. The real, concurrent, cluster-wide per-shard statement count
//      (system.query_log rows with is_initial_query=0, i.e. what
//      `Distributed` actually dispatched to each data shard) never exceeded
//      DATA_SHARD_FANOUT_CAP at any instant during the burst —
//      DataShardFanoutGate's own real, unconditional ceiling
//      (Σ kEff_i × DataShardCount ≤ DataShardFanoutCap), not merely the
//      trivially-safe N=2 case.
//   3. No 5xx from cerberus during the burst — the admission-control path is
//      "degrade parallelism, never reject" (docs/solver.md point 3); a 503
//      here would mean that promise broke once a REAL data-shard fan-out
//      was layered on top, which chDB-only testing could never observe.
//   4. Every per-shard statement's own `max_memory_usage` setting (as
//      ClickHouse itself recorded it in system.query_log.Settings) sits at
//      or below cap/(kEff*DataShardCount) for ITS OWN kEff (joined back via
//      its initial_query_id's trace prefix, not a group-wide bound) — the
//      solver's perShardMemoryBytes formula's live prediction
//      (docs/operations.md's "solver's perShardMemoryBytes setting"
//      section) — and no MEMORY_LIMIT_EXCEEDED / OOM exception appears
//      anywhere in the cluster's query_log for the burst window.
//
// kEff > 1 evidence (point 1) and the concurrent fan-out count (point 2) are
// both read directly off system.query_log rather than inferred, using two
// facts about how cerberus dispatches a solver-split query
// (internal/chclient/client.go's mintQueryID, internal/solver/executor.go's
// ShardQueryIDs):
//   - every per-dispatch ClickHouse query_id cerberus mints has the shape
//     "<traceID>-<spanID>-<counter>", so query_ids from the SAME logical
//     HTTP request share the same leading 32-hex-char traceID whether or
//     not the request carried an incoming `traceparent` (otelhttp still
//     starts a trace per request); a driver-self-generated id (the
//     no-trace fallback, or any query NOT dispatched through chclient, e.g.
//     the rolling seeder's own direct INSERTs) is astronomically unlikely
//     to collide with another query's traceID prefix, so grouping by that
//     prefix with HAVING count() > 1 needs no separate exclusion filter;
//   - `Distributed` fans each SUCH is_initial_query=1 statement out into
//     DataShardCount is_initial_query=0 children carrying that statement's
//     query_id as their own initial_query_id — exactly the rows
//     DataShardFanoutGate's own weight (kEff × DataShardCount) bounds the
//     live concurrent count of.
//
// Env contract:
//   NAMESPACE               k8s namespace                    (default cerberus)
//   CERBERUS_URL            cerberus HTTP endpoint            (default http://localhost:8080)
//   DB                      ClickHouse database                (default otel)
//   CH_USER / CH_PASSWORD   ClickHouse credentials             (default cerberus/cerberus)
//   CH_CLUSTER              ClickHouse cluster name             (default bwc_cluster)
//   DATA_SHARD_COUNT        expected DataShardCount             (required)
//   DATA_SHARD_FANOUT_CAP   expected DataShardFanoutCap         (required)
//   BURST_SECONDS           sustained concurrent-load duration  (default 20)
//   BURST_CONCURRENCY       concurrent requests in flight       (default 6)
//   FLUSH_WAIT_SECONDS      settle time before SYSTEM FLUSH LOGS (default 10)
//
// Exit 0 = every assertion passed; 1 = any failed (with ::error:: annotation).

import process from 'node:process';
import { setTimeout as sleep } from 'node:timers/promises';
import { error, notice, log, capture } from './lib/gh.mjs';
import { makeKubectl } from './lib/k8s.mjs';

const NS = process.env.NAMESPACE || 'cerberus';
const CERBERUS_URL = process.env.CERBERUS_URL || 'http://localhost:8080';
const DB = process.env.DB || 'otel';
const CH_USER = process.env.CH_USER || 'cerberus';
const CH_PASSWORD = process.env.CH_PASSWORD || 'cerberus';
const CH_CLUSTER = process.env.CH_CLUSTER || 'bwc_cluster';
const DATA_SHARD_COUNT = Number(process.env.DATA_SHARD_COUNT || '0');
const DATA_SHARD_FANOUT_CAP = Number(process.env.DATA_SHARD_FANOUT_CAP || '0');
const BURST_SECONDS = Number(process.env.BURST_SECONDS || '20');
const BURST_CONCURRENCY = Number(process.env.BURST_CONCURRENCY || '6');
const FLUSH_WAIT_SECONDS = Number(process.env.FLUSH_WAIT_SECONDS || '10');

if (!DATA_SHARD_COUNT || DATA_SHARD_COUNT < 2) {
  error(`DATA_SHARD_COUNT must be a real data-shard count (>= 2), got ${process.env.DATA_SHARD_COUNT}`);
  process.exit(1);
}
if (!DATA_SHARD_FANOUT_CAP || DATA_SHARD_FANOUT_CAP < 1) {
  error(`DATA_SHARD_FANOUT_CAP must be a positive int, got ${process.env.DATA_SHARD_FANOUT_CAP}`);
  process.exit(1);
}

const kubectl = makeKubectl(capture, NS);

// Any one shard's own ClickHouse pod can run the cluster-wide
// clusterAllReplicas() queries below — the Distributed wrapper + the named
// cluster exist identically on every node of every shard.
function anyClickhousePodName() {
  const res = kubectl([
    'get', 'pod',
    '-l', 'app.kubernetes.io/component=clickhouse',
    '-o', 'jsonpath={.items[0].metadata.name}',
  ]);
  const name = res.stdout.trim();
  if (res.status !== 0 || !name) {
    error(`could not resolve any bundled ClickHouse pod in namespace ${NS}: ${res.stderr.trim()}`);
    process.exit(1);
  }
  return name;
}

function allClickhousePodNames() {
  const res = kubectl([
    'get', 'pod',
    '-l', 'app.kubernetes.io/component=clickhouse',
    '-o', 'jsonpath={.items[*].metadata.name}',
  ]);
  const names = res.stdout.trim().split(/\s+/).filter(Boolean);
  if (res.status !== 0 || names.length === 0) {
    error(`could not list bundled ClickHouse pods in namespace ${NS}: ${res.stderr.trim()}`);
    process.exit(1);
  }
  return names;
}

// TSV output (--format TSVRaw) so multi-column rows split unambiguously
// without a JSON round trip for a bare Node http client to parse.
function chQueryTSV(pod, sql) {
  const res = kubectl([
    'exec', pod, '--',
    'clickhouse-client',
    '--user', CH_USER,
    '--password', CH_PASSWORD,
    '--database', DB,
    '--format', 'TSVRaw',
    '--query', sql,
  ]);
  if (res.status !== 0) {
    error(`clickhouse query failed: ${sql}\n${res.stderr.trim()}`);
    process.exit(1);
  }
  return res.stdout.split('\n').map((l) => l.trimEnd()).filter((l) => l.length > 0);
}

function chExec(pod, sql) {
  const res = kubectl(['exec', pod, '--', 'clickhouse-client', '--user', CH_USER, '--password', CH_PASSWORD, '--query', sql]);
  if (res.status !== 0) {
    error(`clickhouse exec failed: ${sql}\n${res.stderr.trim()}`);
    process.exit(1);
  }
}

// ---- OOM/restart baseline (captured BEFORE the burst) ----
function restartCounts(pods) {
  const out = {};
  for (const p of pods) {
    const res = kubectl(['get', 'pod', p, '-o', 'jsonpath={.status.containerStatuses[0].restartCount}']);
    out[p] = Number(res.stdout.trim() || '0');
  }
  return out;
}

function oomKilledPods(pods) {
  const oomed = [];
  for (const p of pods) {
    const res = kubectl(['get', 'pod', p, '-o', 'jsonpath={.status.containerStatuses[0].lastState.terminated.reason}']);
    if (res.stdout.trim() === 'OOMKilled') oomed.push(p);
  }
  return oomed;
}

// ---- concurrent HTTP load ----
// A wide range (3h) at a fine step (5s) yields ~2160 anchors — combined with
// cerberus-values-datashard.yaml's collapsed MinFanout/MinAnchorPairs/
// MinAnchorsPerSlice, this reliably reaches K=CERBERUS_SHARD_MAX_K (8)
// regardless of the seed's own (modest) row volume, which is exactly what
// this lane's own values overlay documents doing and why.
function wideRangeParams(stepSeconds) {
  const now = Math.floor(Date.now() / 1000);
  const start = now - 3 * 60 * 60;
  return `start=${start}&end=${now}&step=${stepSeconds}`;
}

function loadRequests() {
  const promQ = encodeURIComponent('rate(http_server_request_duration_count[5m])');
  const logQ = encodeURIComponent('count_over_time({service_name=~".+"}[5m])');
  const traceQ = encodeURIComponent('{ resource.service.name =~ ".+" }');
  return [
    `${CERBERUS_URL}/api/v1/query_range?query=${promQ}&${wideRangeParams(5)}`,
    `${CERBERUS_URL}/loki/api/v1/query_range?query=${logQ}&${wideRangeParams(30)}`,
    `${CERBERUS_URL}/api/search?q=${traceQ}`,
  ];
}

async function fireOne(url) {
  try {
    const resp = await fetch(url, { signal: AbortSignal.timeout(15000) });
    return resp.status;
  } catch (e) {
    return `error:${e.message}`;
  }
}

async function runBurst() {
  const urls = loadRequests();
  const statuses = [];
  const deadline = Date.now() + BURST_SECONDS * 1000;
  log(`firing concurrent load (concurrency=${BURST_CONCURRENCY}) for ${BURST_SECONDS}s against ${urls.length} query shapes`);
  const workers = Array.from({ length: BURST_CONCURRENCY }, async (_, i) => {
    let n = 0;
    while (Date.now() < deadline) {
      const url = urls[(n + i) % urls.length];
      statuses.push(await fireOne(url));
      n++;
    }
  });
  await Promise.all(workers);
  return statuses;
}

// ---- sweep-line: max concurrently-overlapping [start, start+duration] intervals ----
function maxConcurrent(intervals) {
  const events = [];
  for (const [startUs, durUs] of intervals) {
    events.push([startUs, 1]);
    events.push([startUs + Math.max(durUs, 1), -1]);
  }
  events.sort((a, b) => (a[0] - b[0]) || (a[1] - b[1]));
  let cur = 0;
  let max = 0;
  for (const [, delta] of events) {
    cur += delta;
    if (cur > max) max = cur;
  }
  return max;
}

async function main() {
  const initiatorPod = anyClickhousePodName();
  const allPods = allClickhousePodNames();
  log(`datashard verify: namespace=${NS} db=${DB} cluster=${CH_CLUSTER} dataShardCount=${DATA_SHARD_COUNT} fanoutCap=${DATA_SHARD_FANOUT_CAP} pods=${allPods.join(',')}`);
  let failures = 0;

  const restartsBefore = restartCounts(allPods);

  const burstStartMs = Date.now();
  const statuses = await runBurst();
  const burstEndMs = Date.now();

  const serverErrors = statuses.filter((s) => typeof s === 'number' && s >= 500);
  const transportErrors = statuses.filter((s) => typeof s === 'string');
  log(`burst complete: ${statuses.length} requests, ${serverErrors.length} 5xx, ${transportErrors.length} transport errors`);
  if (serverErrors.length > 0) {
    error(`cerberus returned ${serverErrors.length} 5xx response(s) during the concurrent burst — admission control must DEGRADE parallelism, never reject (docs/solver.md point 3)`);
    failures++;
  }

  // Let in-flight statements finish, then flush every shard's own
  // query_log buffer cluster-wide before reading it back.
  await sleep(FLUSH_WAIT_SECONDS * 1000);
  chExec(initiatorPod, `SYSTEM FLUSH LOGS ON CLUSTER ${CH_CLUSTER}`);

  const windowStart = Math.floor(burstStartMs / 1000) - 5;
  const windowEnd = Math.floor(burstEndMs / 1000) + FLUSH_WAIT_SECONDS + 10;

  // ---- point 1: a genuine solver-split (kEff > 1) query happened ----
  // clusterAllReplicas (not a local query, and not scoped to `initiatorPod`
  // specifically): cerberus's own connection lands on WHICHEVER shard the
  // chart wired as the Distributed entry point (shard 0), which need not be
  // the pod anyClickhousePodName() happened to resolve to. is_initial_query=1
  // rows exist only on the true entry node regardless of which pod runs the
  // query, so this is correct no matter which pod answers it.
  //
  // kEff per trace is the PEAK CONCURRENT overlap of that trace's own
  // is_initial_query=1 statements, not a flat count of how many it
  // dispatched in total (cerberus issue #3122's follow-up finding). The two
  // differ whenever a request's structural shard count K exceeds the
  // ADMISSION-CLAMPED effective concurrency kEff
  // (internal/solver/executor.go's admitAndGate: kEff = min(K, pEff,
  // gate/2)): launchShards still dispatches all K shards to ClickHouse —
  // sequentially, in waves of at most kEff at a time — so a flat count
  // measures K, while internal/solver/executor.go's own perShardMemoryBytes
  // formula divides by kEff. admitAndGate's own final clamp
  // (`if pEff > kEff { pEff = kEff }`) guarantees pEff == kEff always, and
  // pEff is exactly the errgroup concurrency limit (`g.SetLimit(pEff)`)
  // bounding how many of a trace's own shards can run AT ONCE — so the peak
  // overlap of a trace's own statements is exactly the kEff the divisor
  // used, which a flat count over the whole burst window is not whenever
  // concurrent burst load (BURST_CONCURRENCY) clamps kEff below K. This
  // was invisible before cerberus issue #3122's fix (every observed
  // max_memory_usage was the flat unapportioned cap regardless of kEff, so
  // getting kEff wrong never changed the verdict); it surfaced as a NEW,
  // spurious point-4 failure once the fix started actually apportioning by
  // kEff, confirmed by decoding several failures' own OBSERVED value back
  // to an integer real-kEff (cap/observed/DataShardCount) strictly ≤ the
  // flat count reported.
  // Full trace -> kEff map (every initiator trace, not just split ones):
  // point 4 below needs kEff for EVERY group, including kEff=1 (unsplit)
  // ones, to compute each observed statement's own perShardMemoryBytes
  // ceiling correctly.
  const traceIntervalRows = chQueryTSV(
    initiatorPod,
    `SELECT substring(query_id, 1, 32) AS trace,
            toUnixTimestamp64Micro(query_start_time_microseconds) AS start_us,
            query_duration_ms * 1000 AS dur_us
     FROM clusterAllReplicas('${CH_CLUSTER}', system.query_log)
     WHERE is_initial_query = 1 AND type = 'QueryFinish'
       AND event_time >= toDateTime(${windowStart}) AND event_time <= toDateTime(${windowEnd})`,
  );
  const traceIntervals = new Map();
  for (const row of traceIntervalRows) {
    const [trace, startUs, durUs] = row.split('\t');
    if (!traceIntervals.has(trace)) traceIntervals.set(trace, []);
    traceIntervals.get(trace).push([Number(startUs), Number(durUs)]);
  }
  const kEffByTrace = new Map();
  for (const [trace, intervals] of traceIntervals) {
    kEffByTrace.set(trace, maxConcurrent(intervals));
  }
  const splitTraceCount = [...kEffByTrace.values()].filter((k) => k > 1).length;
  const maxKEffObserved = Math.max(0, ...kEffByTrace.values());
  log(`observed solver-split groups (kEff>1): ${splitTraceCount}; max kEff observed=${maxKEffObserved}`);
  if (maxKEffObserved <= 1) {
    error(`no solver-split (kEff > 1) query was observed in system.query_log during the burst — this leg is vacuous without one (docs/solver.md's admission-control path was never actually exercised)`);
    failures++;
  }

  // ---- point 2: real concurrent per-shard statement count stays within cap ----
  const shardStmtRows = chQueryTSV(
    initiatorPod,
    `SELECT toUnixTimestamp64Micro(query_start_time_microseconds) AS start_us, query_duration_ms * 1000 AS dur_us
     FROM clusterAllReplicas('${CH_CLUSTER}', system.query_log)
     WHERE is_initial_query = 0 AND type = 'QueryFinish'
       AND event_time >= toDateTime(${windowStart}) AND event_time <= toDateTime(${windowEnd})`,
  );
  const intervals = shardStmtRows.map((r) => {
    const [s, d] = r.split('\t').map(Number);
    return [s, d];
  });
  const peakConcurrentShardStatements = maxConcurrent(intervals);
  log(`real per-shard statements observed: ${intervals.length}; peak concurrent=${peakConcurrentShardStatements}`);
  if (peakConcurrentShardStatements > DATA_SHARD_FANOUT_CAP) {
    error(`peak concurrent per-shard ClickHouse statement count ${peakConcurrentShardStatements} exceeded DataShardFanoutCap=${DATA_SHARD_FANOUT_CAP} — the admission-control ceiling did not hold under real load`);
    failures++;
  }
  if (peakConcurrentShardStatements <= DATA_SHARD_COUNT) {
    error(`peak concurrent per-shard statement count ${peakConcurrentShardStatements} never exceeded a single statement's own fan-out width (DataShardCount=${DATA_SHARD_COUNT}) — the burst never produced genuine CONCURRENT admitted requests, so DataShardFanoutGate's cross-request bound was never exercised`);
    failures++;
  }

  // ---- point 4: perShardMemoryBytes prediction + no OOM anywhere ----
  // Each child (is_initial_query=0) row's own initial_query_id names the
  // EXACT parent statement that dispatched it, so its trace prefix keys
  // straight into kEffByTrace above — this is what lets the ceiling below
  // be the real per-QUERY formula cap/(kEff*DataShardCount) rather than the
  // group-wide worst case, catching a regression that silently drops the
  // kEff term (which a flat cap/DataShardCount bound could never catch,
  // since every real kEff>=1 observation would still satisfy it).
  const CERBERUS_CH_QUERY_MAX_MEMORY_BYTES = 1073741824; // cerberus-values.yaml
  const memRows = chQueryTSV(
    initiatorPod,
    `SELECT DISTINCT substring(initial_query_id, 1, 32) AS trace, Settings['max_memory_usage'] AS mem
     FROM clusterAllReplicas('${CH_CLUSTER}', system.query_log)
     WHERE is_initial_query = 0 AND type = 'QueryFinish' AND mapContains(Settings, 'max_memory_usage')
       AND event_time >= toDateTime(${windowStart}) AND event_time <= toDateTime(${windowEnd})`,
  );
  log(`observed per-shard max_memory_usage settings: ${memRows.map((r) => r.split('\t')[1]).join(', ') || '(none)'}`);
  if (memRows.length === 0) {
    error(`no per-shard statement recorded a max_memory_usage setting — cannot confirm perShardMemoryBytes was ever stamped`);
    failures++;
  }
  for (const row of memRows) {
    const [trace, v] = row.split('\t');
    const n = Number(v);
    // A child row whose parent trace never showed up in kEffByTrace (e.g. it
    // fell just outside the initiator-side window) is treated as kEff=1 —
    // the loosest, most conservative ceiling — rather than silently
    // skipped, so a real formula regression is never masked by a windowing
    // edge case.
    const kEff = kEffByTrace.get(trace) || 1;
    const perQueryCeiling = Math.ceil(CERBERUS_CH_QUERY_MAX_MEMORY_BYTES / (kEff * DATA_SHARD_COUNT));
    if (!(n > 0) || n > perQueryCeiling) {
      error(`observed max_memory_usage=${v} (trace=${trace}, kEff=${kEff}) is not within (0, ${perQueryCeiling}] predicted by perShardMemoryBytes = cap/(kEff*DataShardCount) with cap=${CERBERUS_CH_QUERY_MAX_MEMORY_BYTES}, kEff=${kEff}, DataShardCount=${DATA_SHARD_COUNT}`);
      failures++;
    }
  }

  const exceptionRows = chQueryTSV(
    initiatorPod,
    `SELECT count() FROM clusterAllReplicas('${CH_CLUSTER}', system.query_log)
     WHERE type = 'ExceptionWhileProcessing'
       AND (exception_code = 241 OR exception ILIKE '%Memory limit%')
       AND event_time >= toDateTime(${windowStart}) AND event_time <= toDateTime(${windowEnd})`,
  );
  const exceptionCount = Number(exceptionRows[0] || '0');
  if (exceptionCount > 0) {
    error(`${exceptionCount} MEMORY_LIMIT_EXCEEDED exception(s) recorded in system.query_log during the burst — perShardMemoryBytes did not bound memory pressure as predicted`);
    failures++;
  }

  const restartsAfter = restartCounts(allPods);
  const oomed = oomKilledPods(allPods);
  for (const p of allPods) {
    if (restartsAfter[p] > restartsBefore[p]) {
      error(`ClickHouse pod ${p} restarted during the burst (restartCount ${restartsBefore[p]} -> ${restartsAfter[p]})`);
      failures++;
    }
  }
  if (oomed.length > 0) {
    error(`ClickHouse pod(s) OOMKilled during/after the burst: ${oomed.join(', ')}`);
    failures++;
  }

  if (failures > 0) {
    error(`e2e-datashard-verify: ${failures} assertion(s) failed`);
    process.exit(1);
  }
  notice(`e2e-datashard-verify: all assertions passed (dataShardCount=${DATA_SHARD_COUNT}, fanoutCap=${DATA_SHARD_FANOUT_CAP}, peakConcurrentShardStatements=${peakConcurrentShardStatements}, maxKEffObserved=${maxKEffObserved})`);
}

main().catch((e) => {
  error(`unhandled error: ${e.stack || e}`);
  process.exit(1);
});
