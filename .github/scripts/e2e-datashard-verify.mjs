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
//   2. The real, concurrent per-shard SELECT statement count
//      (system.query_log rows with is_initial_query=0 AND
//      query_kind='Select', i.e. what `Distributed` actually dispatched to
//      each data shard for a query cerberus itself served) never exceeded
//      DataShardFanoutGate's own real, unconditional ceiling
//      (Σ kEff_i × DataShardCount ≤ DataShardFanoutCap) at any instant
//      during the burst — in BOTH of the gate's scopes (cerberus issue
//      #3128): per cerberus PROCESS (each child attributed to its pod via
//      query_log.client_hostname, bounded by the per-process cap read back
//      from the chart's env ConfigMap) and cluster-wide (bounded by
//      replicas x that cap, the chart's dataShards.fanoutCap budget), not
//      merely the trivially-safe N=2 case. Scoped to query_kind='Select' (cerberus
//      issue #3128 residual-gap round 3) because is_initial_query=0 also
//      counts Insert/Alter children from `just e2e-seed-rolling`'s rolling
//      seeder, which writes directly to ClickHouse over the native
//      protocol — bypassing cerberus, and so DataShardFanoutGate, entirely
//      — throughout this script's own burst window; the unfiltered total is
//      still logged for visibility but is informational only.
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
//   CERBERUS_DEPLOYMENT     cerberus Deployment name            (default cerberus)
//   CERBERUS_ENV_CONFIGMAP  the chart's env ConfigMap name      (default cerberus-env)
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
const CERBERUS_DEPLOYMENT = process.env.CERBERUS_DEPLOYMENT || 'cerberus';
const CERBERUS_ENV_CONFIGMAP = process.env.CERBERUS_ENV_CONFIGMAP || 'cerberus-env';
const BURST_SECONDS = Number(process.env.BURST_SECONDS || '20');
const BURST_CONCURRENCY = Number(process.env.BURST_CONCURRENCY || '6');
const FLUSH_WAIT_SECONDS = Number(process.env.FLUSH_WAIT_SECONDS || '10');

if (!DATA_SHARD_COUNT || DATA_SHARD_COUNT < 2) {
  error(`DATA_SHARD_COUNT must be a real data-shard count (>= 2), got ${process.env.DATA_SHARD_COUNT}`);
  process.exit(1);
}

const kubectl = makeKubectl(capture, NS);

// The admission-control contract has TWO halves (cerberus issue #3128):
// DataShardFanoutGate is a per-PROCESS semaphore, so each cerberus pod's own
// per-shard concurrency is bounded by the per-process cap, and the cluster
// as a whole by (replicas x per-process cap) — the chart's
// clickhouse.bundled.dataShards.fanoutCap budget apportioned across the
// replica count. Both numbers are read back from the LIVE deployment, not
// from a literal this script would have to keep in sync by hand: the
// per-process cap from the env ConfigMap the chart rendered, the replica
// count from the Deployment. A cap the chart did not render (unset key)
// is an error, not a default — the assertion below would otherwise be
// comparing against a number nothing in the cluster enforces.
function livePerProcessFanoutCap() {
  const res = kubectl(['get', 'configmap', CERBERUS_ENV_CONFIGMAP, '-o', 'jsonpath={.data.CERBERUS_SOLVER_DATA_SHARD_FANOUT_CAP}']);
  const raw = res.stdout.trim();
  const cap = Number(raw);
  if (res.status !== 0 || !raw || !Number.isInteger(cap) || cap < 1) {
    error(`could not read CERBERUS_SOLVER_DATA_SHARD_FANOUT_CAP from configmap/${CERBERUS_ENV_CONFIGMAP} in ${NS} (got ${JSON.stringify(raw)}): ${res.stderr.trim()}`);
    process.exit(1);
  }
  return cap;
}

function liveCerberusReplicas() {
  const res = kubectl(['get', 'deployment', CERBERUS_DEPLOYMENT, '-o', 'jsonpath={.spec.replicas}']);
  const replicas = Number(res.stdout.trim());
  if (res.status !== 0 || !Number.isInteger(replicas) || replicas < 1) {
    error(`could not read deployment/${CERBERUS_DEPLOYMENT} .spec.replicas in ${NS}: ${res.stderr.trim()}`);
    process.exit(1);
  }
  return replicas;
}

// Every cerberus pod name — the set query_log's client_hostname must fall
// in for a Select child to count as a gate-bound dispatch at all.
// clickhouse-go sends os.Hostname() as the client hostname on every query
// (lib/proto/query.go), and a pod's hostname is its own name.
function cerberusPodNames() {
  const res = kubectl(['get', 'pod', '-l', 'app.kubernetes.io/name=cerberus', '-o', 'jsonpath={.items[*].metadata.name}']);
  const names = res.stdout.trim().split(/\s+/).filter(Boolean);
  if (res.status !== 0 || names.length === 0) {
    error(`could not list cerberus pods in namespace ${NS}: ${res.stderr.trim()}`);
    process.exit(1);
  }
  return names;
}

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

// chQuerySingleRaw returns a single scalar/row result as ONE trimmed raw
// string, unlike chQueryTSV which splits stdout on '\n' into rows — that
// split is wrong for a query whose OWN result value could itself contain an
// embedded newline (e.g. reading back a full query TEXT verbatim, as the
// round-4 isolated-re-run diagnostic below does), which would otherwise
// masquerade as more than one row. Returns '' (never exits) when the query
// legitimately produced no result — a diagnostic-only lookup, so a miss is
// informational, not fatal.
function chQuerySingleRaw(pod, sql) {
  const res = kubectl([
    'exec', pod, '--',
    'clickhouse-client',
    '--user', CH_USER,
    '--password', CH_PASSWORD,
    '--database', DB,
    '--format', 'TSVRaw',
    '--query', sql,
  ]);
  return res.status === 0 ? res.stdout.trim() : '';
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

// ---- diagnostic (informational only, cerberus issue #3128 round 4) ----
// fanout_gate.go's own "Residual gap, round 3 findings" doc leaves finding 3
// ("real ClickHouse-side multiplication independent of cerberus's own
// dispatch code") open and names two concrete next steps: a resource-
// pressure / retry-event check, and getting ClickHouse's own planner
// (EXPLAIN PIPELINE / an isolated re-run) to say whether the extra per-shard
// children are load-dependent or structural. Everything below gathers
// evidence for that; NOTHING in this section ever increments `failures` —
// finding 3 is not yet proven, so nothing here asserts a threshold on it.

// retryPressureEvents — ClickHouse system.events counters that a connection-
// level retry or transport failure under load would increment (ClickHouse's
// own src/Common/ProfileEvents.cpp names each). Diffed before/after the
// burst per pod so a genuine increase (not a cumulative since-boot count) is
// what gets reported.
const retryPressureEvents = [
  'DistributedConnectionFailTry',
  'DistributedConnectionFailAtAll',
  'DistributedConnectionMissingTable',
  'DistributedConnectionStaleReplica',
  'NetworkErrors',
  'NetworkSendErrors',
  'NetworkReceiveErrors',
  'ReadBufferFromFileDescriptorReadFailed',
  'ZooKeeperHardwareExceptions',
];

function eventsSnapshot(pods, names) {
  const inList = names.map((n) => `'${n}'`).join(', ');
  const snap = {};
  for (const pod of pods) {
    const rows = chQueryTSV(pod, `SELECT event, value FROM system.events WHERE event IN (${inList})`);
    const perPod = {};
    for (const row of rows) {
      const [event, value] = row.split('\t');
      perPod[event] = Number(value);
    }
    snap[pod] = perPod;
  }
  return snap;
}

function logEventDiffs(before, after, pods, names) {
  for (const pod of pods) {
    const deltas = names
      .map((n) => [n, (after[pod]?.[n] || 0) - (before[pod]?.[n] || 0)])
      .filter(([, d]) => d > 0);
    log(
      deltas.length > 0
        ? `  ${pod}: ${deltas.map(([n, d]) => `${n}=+${d}`).join(', ')}`
        : `  ${pod}: no retry/pressure event increase`,
    );
  }
}

// diagnosticSettleSeconds bounds the grace period between an isolated
// diagnostic re-run's clickhouse-client call returning and this script's own
// SYSTEM FLUSH LOGS — the re-run's own query_log row is enqueued at query
// finish (before the client call returns) but the internal system-log
// buffer flush that makes it durably queryable is asynchronous, so a bare
// zero-wait FLUSH LOGS could race it. Named so the grace period is never a
// bare literal (invariant 13).
const diagnosticSettleSeconds = 2;

// roundFourDiagQueryIDPrefix tags the isolated re-run's own query_id so it
// can never collide with a real dispatch's mintQueryID-derived id (which is
// always exactly 32 hex chars + "-" + more hex, never this literal prefix).
const roundFourDiagQueryIDPrefix = 'issue3128-round4-diag';

async function main() {
  const initiatorPod = anyClickhousePodName();
  const allPods = allClickhousePodNames();
  const perProcessCap = livePerProcessFanoutCap();
  const cerberusReplicas = liveCerberusReplicas();
  const cerberusPods = cerberusPodNames();
  const clusterCap = perProcessCap * cerberusReplicas;
  log(`datashard verify: namespace=${NS} db=${DB} cluster=${CH_CLUSTER} dataShardCount=${DATA_SHARD_COUNT} perProcessFanoutCap=${perProcessCap} cerberusReplicas=${cerberusReplicas} clusterFanoutCap=${clusterCap} chPods=${allPods.join(',')} cerberusPods=${cerberusPods.join(',')}`);
  let failures = 0;
  if (cerberusPods.length > cerberusReplicas) {
    error(`${cerberusPods.length} cerberus pods present but deployment/${CERBERUS_DEPLOYMENT} declares ${cerberusReplicas} replicas — the cluster-wide ceiling (replicas x per-process cap) would be measured against a smaller process count than actually ran`);
    failures++;
  }
  const eventsBefore = eventsSnapshot(allPods, retryPressureEvents);

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

  // ---- diagnostic: retry/pressure events + CPU/memory pressure during the burst ----
  // See this file's "diagnostic (informational only, cerberus issue #3128
  // round 4)" section above — never gates the run.
  log('diagnostic: retry/connection-failure event deltas during the burst, per ClickHouse pod:');
  const eventsAfter = eventsSnapshot(allPods, retryPressureEvents);
  logEventDiffs(eventsBefore, eventsAfter, allPods, retryPressureEvents);

  const pressureMetricRows = chQueryTSV(
    initiatorPod,
    `SELECT hostName() AS host, metric, max(value) AS peak
     FROM clusterAllReplicas('${CH_CLUSTER}', system.asynchronous_metric_log)
     WHERE event_time >= toDateTime(${windowStart}) AND event_time <= toDateTime(${windowEnd})
       AND (metric ILIKE '%CPU%' OR metric ILIKE '%Memory%' OR metric ILIKE '%Throttl%')
     GROUP BY host, metric
     ORDER BY host, metric`,
  );
  log(`diagnostic: peak CPU/Memory/Throttle asynchronous_metric_log values during the burst window (cross-reference against cerberus-values-datashard.yaml's "sized down" pod resources — requests.cpu=100m, limits.memory=1Gi, no cpu limit configured):`);
  for (const row of pressureMetricRows) {
    log(`  ${row}`);
  }

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
  //
  // query_kind and initial_query_id (cerberus issue #3128 residual-gap round
  // 3) are pulled for every is_initial_query=0 row for two permanent reasons
  // beyond the original point-2 bound:
  //   - query_kind distinguishes a real DataShardFanoutGate-bound dispatch
  //     (query_kind='Select', reaching ClickHouse through
  //     internal/chclient's queryOpen/queryCursorColumnar, the ONLY seam
  //     this gate wraps) from an is_initial_query=0 row that was never
  //     subject to the gate AT ALL: `just e2e-seed-rolling`'s rolling
  //     seeder (test/e2e/seed/cmd/seed) connects to ClickHouse DIRECTLY
  //     over the native protocol (bypassing cerberus, and so
  //     acquireDataShardFanout, entirely) and keeps INSERTing into the same
  //     public `Distributed`-engine tables (otel_logs, otel_traces,
  //     otel_metrics_*) every 30s for the lane's whole lifetime, INCLUDING
  //     during this script's own burst window (`e2e-seed-stop` is not
  //     called until `just e2e-datashard-down`, well after this script
  //     returns). Each such INSERT fans out to DataShardCount
  //     is_initial_query=0 query_kind='Insert' children exactly like a
  //     Select does, and the original query with no query_kind filter
  //     counts them identically — inflating measured overlap with
  //     statements the gate was never designed to bound. CONFIRMED against
  //     real dispatch run 34043234494 to fully explain at least one N=2
  //     failure on its own (unfiltered peak 9 > cap 8; Select-only peak
  //     7 <= cap 8).
  //   - initial_query_id (the coordinator's own globally-unique
  //     "<traceID>-<spanID>-<counter>" query_id — mintQueryID's process-wide
  //     atomic counter makes two distinct dispatches sharing one id
  //     structurally impossible) groups every child back to the ONE
  //     dispatch that produced it, independent of timing. A group whose own
  //     child count exceeds DataShardCount is real evidence that dispatch's
  //     gate acquisition (a fixed weight of DataShardCount) under-charged
  //     its real ClickHouse-side fan-out width — internal/chclient/
  //     fanout_gate.go's own "Residual gap" doc has the full investigation
  //     trail for what this has and has not been narrowed down to so far.
  //     A short query snippet is kept alongside each sample so a future
  //     occurrence can be matched against a known query SHAPE without
  //     needing a fresh full-text capture.
  //   - cerberus_host attributes every child to the cerberus PROCESS whose
  //     gate admitted it (cerberus issue #3128's per-process finding: the
  //     gate is one semaphore per pod, so the contract is per pod first and
  //     replicas x cap cluster-wide second). The initiator row carries the
  //     client's hostname — clickhouse-go sends os.Hostname() on every
  //     query (lib/proto/query.go), which inside a pod is the pod name —
  //     joined back over initial_query_id (GLOBAL, so the initiator-side
  //     subquery is computed once and shipped to every replica the
  //     clusterAllReplicas scan runs on, instead of being re-issued as a
  //     double-distributed subquery ClickHouse rejects). The child's own
  //     client_hostname is the fallback should an initiator row fall
  //     outside the window; an unresolvable host is an assertion failure
  //     below, never a silent drop.
  const shardStmtRows = chQueryTSV(
    initiatorPod,
    `SELECT toUnixTimestamp64Micro(c.query_start_time_microseconds) AS start_us, c.query_duration_ms * 1000 AS dur_us,
            c.query_kind, c.initial_query_id,
            if(i.client_hostname != '', i.client_hostname, c.client_hostname) AS cerberus_host,
            replaceRegexpAll(substring(c.query, 1, 300), '[\\t\\n\\r]+', ' ') AS query_snippet
     FROM clusterAllReplicas('${CH_CLUSTER}', system.query_log) AS c
     GLOBAL LEFT JOIN (
       SELECT query_id, any(client_hostname) AS client_hostname
       FROM clusterAllReplicas('${CH_CLUSTER}', system.query_log)
       WHERE is_initial_query = 1 AND type = 'QueryFinish'
         AND event_time >= toDateTime(${windowStart}) AND event_time <= toDateTime(${windowEnd})
       GROUP BY query_id
     ) AS i ON c.initial_query_id = i.query_id
     WHERE c.is_initial_query = 0 AND c.type = 'QueryFinish'
       AND c.event_time >= toDateTime(${windowStart}) AND c.event_time <= toDateTime(${windowEnd})`,
  );
  const shardStmts = shardStmtRows.map((r) => {
    // TSVRaw does not escape tabs/newlines in a value, so query_snippet is
    // flattened to single spaces (above) BEFORE this split ever runs —
    // otherwise an embedded newline would masquerade as a row break here.
    const [s, d, kind, qid, host, snippet] = r.split('\t');
    return { startUs: Number(s), durUs: Number(d), kind, qid, host, snippet };
  });
  const intervals = shardStmts.map((r) => [r.startUs, r.durUs]);
  const peakConcurrentShardStatements = maxConcurrent(intervals);
  const kindCounts = {};
  for (const r of shardStmts) kindCounts[r.kind] = (kindCounts[r.kind] || 0) + 1;
  log(`real per-shard statements observed: ${intervals.length}; peak concurrent (ALL query_kind, informational only)=${peakConcurrentShardStatements}; by query_kind: ${JSON.stringify(kindCounts)}`);

  // Select-only overlap is the ceiling DataShardFanoutGate actually bounds
  // (cerberus issue #3128 residual-gap round 3, confirmed against real
  // dispatch run 34043234494): `is_initial_query=0` also counts children of
  // `just e2e-seed-rolling`'s rolling seeder (test/e2e/seed/cmd/seed),
  // which connects to ClickHouse DIRECTLY over the native protocol —
  // bypassing cerberus, and so acquireDataShardFanout, entirely — and keeps
  // INSERTing into the same public `Distributed`-engine tables throughout
  // this script's own burst window. Those Insert (and DDL/Alter, from the
  // seeder's own stale-row-pruning mutations) children were never subject
  // to the gate, so counting them against DataShardFanoutCap tests
  // something the gate was never built to bound. The real assertion is
  // scoped to query_kind='Select' — the only kind chclient's queryOpen /
  // queryCursorColumnar (this gate's one seam) ever dispatches.
  const selectRows = shardStmts.filter((r) => r.kind === 'Select');
  const selectIntervals = selectRows.map((r) => [r.startUs, r.durUs]);
  const peakConcurrentSelectOnly = maxConcurrent(selectIntervals);
  log(`Select-only per-shard statements observed: ${selectIntervals.length}; peak concurrent (Select-only)=${peakConcurrentSelectOnly}`);

  // Over-width dispatches: grouped by initial_query_id (see the point-2
  // query's own doc above for why this is a safe, exact join key). Kept as
  // a permanent, low-cost signal for cerberus issue #3128's still-open
  // finding 3 (internal/chclient/fanout_gate.go's own "Residual gap" doc) —
  // a real ClickHouse-side or transport-level effect, confirmed NOT caused
  // by any cerberus dispatch-multiplication mechanism, that produces more
  // real per-shard children than DataShardCount for some dispatches.
  const childrenByQid = new Map();
  for (const r of selectRows) {
    if (!childrenByQid.has(r.qid)) childrenByQid.set(r.qid, []);
    childrenByQid.get(r.qid).push(r);
  }
  const overWidthGroups = [...childrenByQid.entries()].filter(([, rows]) => rows.length > DATA_SHARD_COUNT);
  // Cap how many over-width groups get their own log line — the point is a
  // representative sample for manual correlation, not an exhaustive dump
  // that could run to hundreds of lines on a bad run.
  const overWidthSampleLimit = 10;
  log(`dispatches (by initial_query_id) whose own child count exceeds DataShardCount=${DATA_SHARD_COUNT}: ${overWidthGroups.length} of ${childrenByQid.size} total`);
  for (const [qid, rows] of overWidthGroups.slice(0, overWidthSampleLimit)) {
    // selfMaxConcurrent — this ONE group's own children, swept in isolation.
    // 1 means the group's own children never overlap EACH OTHER (a
    // sequential-round-trip pattern: N separate ClickHouse statements, one
    // after another, all sharing one query_id) — each already paid its own
    // separate gate acquire/release, so the group itself is not what
    // breaches the cap even though its own row COUNT exceeds DataShardCount.
    // > 1 means this group's own children genuinely ran concurrently WITH
    // EACH OTHER — real evidence of one gate acquisition under-charging a
    // dispatch that structurally fans out wider than DataShardCount.
    const selfMaxConcurrent = maxConcurrent(rows.map((r) => [r.startUs, r.durUs]));
    log(`  over-width dispatch ${qid}: ${rows.length} children (expected <= ${DATA_SHARD_COUNT}), own internal peak concurrency=${selfMaxConcurrent}; sample query: ${rows[0].snippet}`);
  }

  // ---- diagnostic: is one over-width dispatch's own SQL over-width even in
  // ISOLATION (no concurrent burst load)? ----
  // If ClickHouse's own planner deterministically fans this SQL out wider
  // than DataShardCount regardless of load, re-running it alone reproduces
  // the same child count. If the extra children only appear under the
  // burst's concurrent-connection pressure, the isolated re-run matches
  // DataShardCount exactly — evidence FOR (not proof of) the
  // resource-contention/retry hypothesis fanout_gate.go's own doc names as
  // the untested next step. See this file's "diagnostic (informational
  // only, cerberus issue #3128 round 4)" section above — never gates the run.
  if (overWidthGroups.length > 0) {
    const [sampleQid] = overWidthGroups[0];
    // chQuerySingleRaw, not chQueryTSV — the query TEXT being read back is
    // itself the result value here, and chQueryTSV's line-split would
    // corrupt it if it ever contained an embedded newline (see that
    // function's own doc).
    const fullQuery = chQuerySingleRaw(
      initiatorPod,
      `SELECT query FROM clusterAllReplicas('${CH_CLUSTER}', system.query_log)
       WHERE query_id = '${sampleQid}' AND is_initial_query = 1 AND type = 'QueryFinish'
       LIMIT 1`,
    );
    if (!fullQuery) {
      log(`diagnostic: could not recover full query text for over-width dispatch ${sampleQid} (parent row fell outside the window) — skipping isolated re-run`);
    } else {
      log(`diagnostic: FULL query text for over-width dispatch ${sampleQid}:\n${fullQuery}`);

      // parallel-replicas + table-engine + cluster-topology check: is the
      // extra fan-out actually replica-level parallelism (a DIFFERENT
      // mechanism from Distributed data-shard fan-out, invisible to
      // DataShardFanoutGate either way) rather than a self-referencing
      // subquery under distributed_product_mode=global?
      log('diagnostic: parallel-replicas settings on the connection cerberus actually uses:');
      log(chQuerySingleRaw(initiatorPod, `SELECT name, value, changed FROM system.settings WHERE name ILIKE '%parallel_replica%'`) || '(no rows)');
      log('diagnostic: otel_traces_local table engine:');
      log(chQuerySingleRaw(initiatorPod, `SELECT database, name, engine FROM system.tables WHERE name = 'otel_traces_local'`) || '(no rows)');
      log('diagnostic: system.clusters topology (shard_num, replica_num, host_name):');
      log(chQuerySingleRaw(initiatorPod, `SELECT cluster, shard_num, replica_num, host_name FROM system.clusters WHERE cluster = '${CH_CLUSTER}' ORDER BY shard_num, replica_num`) || '(no rows)');

      const distributedSettingsArgs = [
        '--skip_unavailable_shards=0',
        '--fallback_to_stale_replicas_for_distributed_queries=0',
        '--load_balancing=first_or_random',
        '--distributed_product_mode=global',
      ];
      log(`diagnostic: EXPLAIN PIPELINE for over-width dispatch ${sampleQid}'s own SQL (DataShardCount=${DATA_SHARD_COUNT} expected fan-out width):`);
      const explainRes = kubectl([
        'exec', initiatorPod, '--',
        'clickhouse-client', '--user', CH_USER, '--password', CH_PASSWORD, '--database', DB,
        ...distributedSettingsArgs,
        '--query', `EXPLAIN PIPELINE ${fullQuery}`,
      ]);
      log(explainRes.status === 0 ? explainRes.stdout.trim() || '(empty)' : `  (EXPLAIN PIPELINE failed, informational only: ${explainRes.stderr.trim()})`);

      const diagQueryID = `${roundFourDiagQueryIDPrefix}-${Date.now()}`;
      log(`diagnostic: re-running the SAME SQL in ISOLATION (query_id=${diagQueryID}, no concurrent burst load)`);
      const rerunRes = kubectl([
        'exec', initiatorPod, '--',
        'clickhouse-client', '--user', CH_USER, '--password', CH_PASSWORD, '--database', DB,
        ...distributedSettingsArgs,
        '--query_id', diagQueryID,
        '--query', fullQuery,
      ]);
      if (rerunRes.status !== 0) {
        log(`diagnostic: isolated re-run failed, informational only: ${rerunRes.stderr.trim()}`);
      } else {
        await sleep(diagnosticSettleSeconds * 1000);
        chExec(initiatorPod, `SYSTEM FLUSH LOGS ON CLUSTER ${CH_CLUSTER}`);
        const isolatedChildRows = chQueryTSV(
          initiatorPod,
          `SELECT count() FROM clusterAllReplicas('${CH_CLUSTER}', system.query_log)
           WHERE initial_query_id = '${diagQueryID}' AND is_initial_query = 0
             AND query_kind = 'Select' AND type = 'QueryFinish'`,
        );
        const isolatedChildCount = Number(isolatedChildRows[0] || '0');
        const verdict = isolatedChildCount > DATA_SHARD_COUNT
          ? 'STILL over-width even in isolation: the multiplication is NOT purely load-dependent — points at a structural planner/setting effect'
          : 'matches DataShardCount exactly in isolation: the over-width shape needs concurrent load/connection pressure to reproduce, consistent with a real ClickHouse-side retry/contention effect';
        log(`diagnostic: isolated re-run produced ${isolatedChildCount} per-shard Select children (DataShardCount=${DATA_SHARD_COUNT}) — ${verdict}`);
      }
    }
  }

  // Per-process contract: every Select child attributed to its cerberus
  // pod, each pod's own peak overlap within the per-process cap the chart
  // rendered. A child whose host is not a cerberus pod is a failure in its
  // own right — it means the attribution (and so every number below) is not
  // trustworthy, which must never pass quietly.
  const selectByHost = new Map();
  for (const r of selectRows) {
    if (!selectByHost.has(r.host)) selectByHost.set(r.host, []);
    selectByHost.get(r.host).push([r.startUs, r.durUs]);
  }
  const unknownHosts = [...selectByHost.keys()].filter((h) => !cerberusPods.includes(h));
  if (unknownHosts.length > 0) {
    error(`${unknownHosts.length} Select child host(s) are not cerberus pods (${unknownHosts.map((h) => JSON.stringify(h)).join(', ')}; cerberus pods: ${cerberusPods.join(',')}) — per-process attribution via query_log.client_hostname failed, so the per-pod ceiling cannot be trusted`);
    failures++;
  }
  for (const [host, hostIntervals] of selectByHost) {
    const hostPeak = maxConcurrent(hostIntervals);
    log(`  cerberus pod ${host}: ${hostIntervals.length} Select per-shard statements, peak concurrent=${hostPeak} (per-process cap ${perProcessCap})`);
    if (hostPeak > perProcessCap) {
      error(`cerberus pod ${host}: peak concurrent per-shard Select statement count ${hostPeak} exceeded its own per-process DataShardFanoutCap=${perProcessCap} — the gate did not hold inside one process`);
      failures++;
    }
  }
  // Cluster-wide contract: replicas x per-process cap, the budget the chart
  // apportioned (clickhouse.bundled.dataShards.fanoutCap).
  if (peakConcurrentSelectOnly > clusterCap) {
    error(`cluster-wide peak concurrent per-shard ClickHouse Select statement count ${peakConcurrentSelectOnly} exceeded replicas(${cerberusReplicas}) x per-process DataShardFanoutCap(${perProcessCap}) = ${clusterCap} — the admission-control ceiling did not hold under real load`);
    failures++;
  }
  if (peakConcurrentSelectOnly <= DATA_SHARD_COUNT) {
    error(`peak concurrent per-shard Select statement count ${peakConcurrentSelectOnly} never exceeded a single statement's own fan-out width (DataShardCount=${DATA_SHARD_COUNT}) — the burst never produced genuine CONCURRENT admitted requests, so DataShardFanoutGate's cross-request bound was never exercised`);
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
  notice(`e2e-datashard-verify: all assertions passed (dataShardCount=${DATA_SHARD_COUNT}, perProcessFanoutCap=${perProcessCap}, cerberusReplicas=${cerberusReplicas}, clusterFanoutCap=${clusterCap}, peakConcurrentSelectOnly=${peakConcurrentSelectOnly}, peakConcurrentAllQueryKinds=${peakConcurrentShardStatements}, maxKEffObserved=${maxKEffObserved})`);
}

main().catch((e) => {
  error(`unhandled error: ${e.stack || e}`);
  process.exit(1);
});
