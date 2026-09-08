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
//      DataShardFanoutGate's own real, unconditional ceiling at any instant
//      during the burst. The gate charges each dispatched statement
//      `scans × DataShardCount` — `scans` being the number of physical
//      (schema) table references the rendered statement contains, which
//      chsql.EmitCounted reports and the engine/solver stamp via
//      chclient.WithDataShardFanoutMultiplier — so the ceiling is
//      Σ (scans_i × kEff_i × DataShardCount) ≤ DataShardFanoutCap over every
//      concurrently admitted request (the same arithmetic
//      test/e2e/k3s/cerberus-values-datashard.yaml sizes the lane's cap
//      from). Asserted in BOTH of the gate's scopes (cerberus issue #3128):
//      per cerberus PROCESS (each child attributed to its pod via
//      query_log.client_hostname, bounded by the per-process cap read back
//      from the chart's env ConfigMap) and cluster-wide (bounded by
//      replicas x that cap, the chart's dataShards.fanoutCap budget), not
//      merely the trivially-safe N=2 case. Scoped to query_kind='Select'
//      because is_initial_query=0 also counts Insert/Alter children from
//      `just e2e-seed-rolling`'s rolling seeder, which writes directly to
//      ClickHouse over the native protocol — bypassing cerberus, and so
//      DataShardFanoutGate, entirely — throughout this script's own burst
//      window; the unfiltered total is still logged for visibility but is
//      informational only.
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
//      section), with `cap` read back from the same env ConfigMap — and no
//      MEMORY_LIMIT_EXCEEDED / OOM exception appears anywhere in the
//      cluster's query_log for the burst window.
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
//     `scans × DataShardCount` is_initial_query=0 children (one per data
//     shard per physical table reference) carrying that statement's
//     query_id as their own initial_query_id — exactly the rows
//     DataShardFanoutGate's own weight (scans × DataShardCount) bounds the
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
//   HEALTH_POLL_SECONDS     bounded wait for a clean errors_count (default 60)
//
// Exit 0 = every assertion passed; 1 = any failed (with ::error:: annotation).

import process from 'node:process';
import { pathToFileURL } from 'node:url';
import { setTimeout as sleep } from 'node:timers/promises';
import { error, notice, log, capture } from './lib/gh.mjs';
import { makeKubectl, clickhousePodName, chQuery, waitForClusterHealth } from './lib/k8s.mjs';

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
const HEALTH_POLL_SECONDS = Number(process.env.HEALTH_POLL_SECONDS || '60');

// requireDataShardCount is called from main, NOT at import time: this module
// is imported by its own test suite for the pure interval arithmetic below,
// and a module that exits the process just for being imported cannot be
// unit-tested at all — which is how peakOverlap shipped a false-failure with
// no coverage. Running the script still fails just as loudly and just as
// early, on the first line of main.
function requireDataShardCount() {
  if (!DATA_SHARD_COUNT || DATA_SHARD_COUNT < 2) {
    error(`DATA_SHARD_COUNT must be a real data-shard count (>= 2), got ${process.env.DATA_SHARD_COUNT}`);
    process.exit(1);
  }
}

const kubectl = makeKubectl(capture, NS);
const CH_OPTS = { database: DB, user: CH_USER, password: CH_PASSWORD };

// TSVRaw so multi-column rows split unambiguously without a JSON round trip
// (same convention e2e-datashard-replica-affinity-verify.mjs uses for the
// same reason). Every value with a possible embedded tab/newline is
// flattened in SQL BEFORE this split ever runs — otherwise an embedded
// newline would masquerade as a row break here.
function chQueryTSV(pod, sql) {
  const out = chQuery(kubectl, pod, { ...CH_OPTS, format: 'TSVRaw' }, sql);
  return out.split('\n').map((l) => l.trimEnd()).filter((l) => l.length > 0);
}

function chExec(pod, sql) {
  chQuery(kubectl, pod, CH_OPTS, sql);
}

// Every number this script compares against is read back from the LIVE
// deployment, never from a literal it would have to keep in sync by hand
// with a values file: a value the chart did not render (unset key) is an
// error, not a default — the assertion would otherwise be comparing against
// a number nothing in the cluster enforces.
function liveConfigMapValue(key) {
  const res = kubectl(['get', 'configmap', CERBERUS_ENV_CONFIGMAP, '-o', `jsonpath={.data.${key}}`]);
  const raw = res.stdout.trim();
  if (res.status !== 0 || !raw) {
    error(`could not read ${key} from configmap/${CERBERUS_ENV_CONFIGMAP} in ${NS} (got ${JSON.stringify(raw)}): ${res.stderr.trim()}`);
    process.exit(1);
  }
  return raw;
}

// The admission-control contract has TWO halves (cerberus issue #3128):
// DataShardFanoutGate is a per-PROCESS semaphore, so each cerberus pod's own
// per-shard concurrency is bounded by the per-process cap, and the cluster
// as a whole by (replicas x per-process cap) — the chart's
// clickhouse.bundled.dataShards.fanoutCap budget apportioned across the
// replica count. The per-process cap comes from the env ConfigMap the chart
// rendered, the replica count from the Deployment.
function livePerProcessFanoutCap() {
  const raw = liveConfigMapValue('CERBERUS_SOLVER_DATA_SHARD_FANOUT_CAP');
  const cap = Number(raw);
  if (!Number.isInteger(cap) || cap < 1) {
    error(`CERBERUS_SOLVER_DATA_SHARD_FANOUT_CAP in configmap/${CERBERUS_ENV_CONFIGMAP} is not a positive integer: ${JSON.stringify(raw)}`);
    process.exit(1);
  }
  return cap;
}

// byteSizeMultipliers — the size suffixes the binary's own parser accepts
// (internal/config/byte_size.go: a bare integer of bytes, or a Kubernetes
// resource.Quantity such as 2Gi / 500Mi / 1G), which is also the set the
// chart passes through into the ConfigMap verbatim
// (deploy/helm/cerberus/templates/_helpers.tpl's chMaxMemory rendering).
const byteSizeMultipliers = {
  Ki: 1024, Mi: 1024 ** 2, Gi: 1024 ** 3, Ti: 1024 ** 4, Pi: 1024 ** 5,
  k: 1000, M: 1000 ** 2, G: 1000 ** 3, T: 1000 ** 4, P: 1000 ** 5,
};

// parseByteSize mirrors internal/config/byte_size.go's grammar for the one
// value this script needs it for: a non-negative whole number of bytes, or
// a humanized size that resolves to one. Returns null for anything else so
// the caller can name the key it came from.
function parseByteSize(raw) {
  const s = String(raw ?? '').trim();
  if (/^\d+$/.test(s)) return Number(s);
  const m = /^(\d+(?:\.\d+)?)(Ki|Mi|Gi|Ti|Pi|k|M|G|T|P)$/.exec(s);
  if (!m) return null;
  const bytes = Number(m[1]) * byteSizeMultipliers[m[2]];
  return Number.isInteger(bytes) ? bytes : null;
}

// The per-query ClickHouse memory cap the solver apportions —
// perShardMemoryBytes = cap/(kEff*DataShardCount) — read back from the same
// ConfigMap the process itself boots from, so point 4 below compares
// against the cap the cluster is actually running with.
function liveQueryMaxMemoryBytes() {
  const raw = liveConfigMapValue('CERBERUS_CH_QUERY_MAX_MEMORY');
  const bytes = parseByteSize(raw);
  if (bytes === null || bytes < 1) {
    error(`CERBERUS_CH_QUERY_MAX_MEMORY in configmap/${CERBERUS_ENV_CONFIGMAP} is not a positive byte size (bytes, or 2Gi / 500Mi / 1G): ${JSON.stringify(raw)}`);
    process.exit(1);
  }
  return bytes;
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
// (lib/proto/query.go), and a pod's hostname is its own name. Read once
// before the burst and once after, then unioned (see main): a pod that
// rolls mid-burst (a restart, a rescheduling) issues real gate-bound
// dispatches under a name only one of the two snapshots knows, and a
// single snapshot would misclassify those as foreign-host statements —
// dropping them from the very population the ceiling is asserted over.
function cerberusPodNames() {
  const res = kubectl(['get', 'pod', '-l', 'app.kubernetes.io/name=cerberus', '-o', 'jsonpath={.items[*].metadata.name}']);
  const names = res.stdout.trim().split(/\s+/).filter(Boolean);
  if (res.status !== 0 || names.length === 0) {
    error(`could not list cerberus pods in namespace ${NS}: ${res.stderr.trim()}`);
    process.exit(1);
  }
  return names;
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

// ---- OOM/restart baseline (captured BEFORE the burst) ----
function restartCounts(pods) {
  const out = {};
  for (const p of pods) {
    const res = kubectl(['get', 'pod', p, '-o', 'jsonpath={.status.containerStatuses[0].restartCount}']);
    out[p] = Number(res.stdout.trim() || '0');
  }
  return out;
}

// oomKilledPods — every pod whose LAST terminated container state is an
// OOMKill. `lastState` is sticky for the pod's whole life, so a kill that
// predates this script (the lane's own bring-up, an earlier verify run on
// the same cluster) reads identically to one the burst caused; the baseline
// captured before the burst is what tells them apart (see main).
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
// Each interval is { startUs, durUs, qid? }. Returns the peak overlap and
// `maxDistinctQidsLive`: the MOST distinct `qid`s (initial_query_ids — i.e.
// distinct dispatches) that were ever live simultaneously. That second
// number is the cross-request evidence point 2's "exercised" check needs,
// since one dispatch alone already contributes scans × DataShardCount
// overlapping children and an instant made of a single dispatch says
// nothing about the gate's bound ACROSS admitted requests.
//
// Deliberately the max over EVERY instant rather than the count at the
// instant the peak happens to be first reached: those are different
// questions, and answering the second one gives a false negative whenever a
// single wide dispatch reaches the peak first and two dispatches only
// overlap later (at N=2 a rate() over the 3-member metrics merge union is
// already 6 concurrent children from ONE dispatch, so that ordering is the
// common case, not a corner). "Were two admitted requests ever in flight
// together" is what the gate's cross-request bound needs, and it does not
// depend on the peak instant at all.
export function peakOverlap(intervals) {
  const events = [];
  for (const { startUs, durUs, qid } of intervals) {
    events.push([startUs, 1, qid]);
    events.push([startUs + Math.max(durUs, 1), -1, qid]);
  }
  events.sort((a, b) => (a[0] - b[0]) || (a[1] - b[1]));
  const live = new Map();
  let cur = 0;
  let peak = 0;
  let maxDistinctQidsLive = 0;
  for (const [, delta, qid] of events) {
    cur += delta;
    const n = (live.get(qid) || 0) + delta;
    if (n > 0) live.set(qid, n);
    else live.delete(qid);
    if (cur > peak) peak = cur;
    if (live.size > maxDistinctQidsLive) maxDistinctQidsLive = live.size;
  }
  return { peak, maxDistinctQidsLive };
}

function maxConcurrent(intervals) {
  return peakOverlap(intervals).peak;
}

// durationUsExpr — a statement's wall-clock duration in microseconds, as
// SQL. `event_time_microseconds - query_start_time_microseconds` on a
// QueryFinish row is the microsecond-precise span; `query_duration_ms`
// (millisecond-truncated, so a sub-millisecond statement reads as 0) is
// the fallback for a row whose two timestamps disagree (a non-positive
// span — clock skew between the columns' sources).
function durationUsExpr(alias) {
  const start = `toUnixTimestamp64Micro(${alias}query_start_time_microseconds)`;
  const finish = `toUnixTimestamp64Micro(${alias}event_time_microseconds)`;
  return `if(${finish} - ${start} > 0, ${finish} - ${start}, ${alias}query_duration_ms * 1000)`;
}

async function main() {
  requireDataShardCount();
  const initiatorPod = clickhousePodName(kubectl, NS);
  const allPods = allClickhousePodNames();
  const perProcessCap = livePerProcessFanoutCap();
  const queryMaxMemoryBytes = liveQueryMaxMemoryBytes();
  const cerberusReplicas = liveCerberusReplicas();
  const cerberusPodsBefore = cerberusPodNames();
  const clusterCap = perProcessCap * cerberusReplicas;
  log(`datashard verify: namespace=${NS} db=${DB} cluster=${CH_CLUSTER} dataShardCount=${DATA_SHARD_COUNT} perProcessFanoutCap=${perProcessCap} cerberusReplicas=${cerberusReplicas} clusterFanoutCap=${clusterCap} queryMaxMemoryBytes=${queryMaxMemoryBytes} chPods=${allPods.join(',')} cerberusPods=${cerberusPodsBefore.join(',')}`);
  let failures = 0;
  if (cerberusPodsBefore.length > cerberusReplicas) {
    error(`${cerberusPodsBefore.length} cerberus pods present but deployment/${CERBERUS_DEPLOYMENT} declares ${cerberusReplicas} replicas — the cluster-wide ceiling (replicas x per-process cap) would be measured against a smaller process count than actually ran`);
    failures++;
  }

  const restartsBefore = restartCounts(allPods);
  const oomedBefore = new Set(oomKilledPods(allPods));
  if (oomedBefore.size > 0) {
    log(`pre-burst baseline: ${oomedBefore.size} ClickHouse pod(s) already carry an OOMKilled lastState from before this script ran (${[...oomedBefore].join(', ')}) — only a NEW OOMKill is attributed to the burst`);
  }

  // ---- settle window: wait for a genuinely healthy inter-node state ----
  // `just e2e-datashard-up`'s pod-readiness gate (`kubectl rollout status`)
  // proves nothing about whether every data shard can already dial every
  // OTHER shard — the same startup race e2e-datashard-replica-affinity-
  // verify.mjs exists to rule out (cerberus issue #3148), and this script
  // fires load against the exact same Distributed cross-shard fan-out right
  // after cluster-up, so it is exposed to it too (cerberus issue #3172:
  // waitForClusterHealth was previously private to that one script).
  try {
    const settleSeconds = await waitForClusterHealth(kubectl, initiatorPod, CH_OPTS, {
      cluster: CH_CLUSTER,
      deadlineMs: HEALTH_POLL_SECONDS * 1000,
    });
    log(`cluster health confirmed clean (errors_count=0 for every replica) after ${settleSeconds.toFixed(1)}s`);
  } catch (e) {
    error(e.message);
    process.exit(1);
  }

  const burstStartMs = Date.now();
  const statuses = await runBurst();
  const burstEndMs = Date.now();

  // See cerberusPodNames' own doc: union the pre- and post-burst snapshots
  // so a pod that rolled mid-burst keeps its dispatches attributed.
  const cerberusPods = [...new Set([...cerberusPodsBefore, ...cerberusPodNames()])];
  if (cerberusPods.length > cerberusPodsBefore.length) {
    log(`cerberus pod set changed during the burst: ${cerberusPods.join(',')} (was ${cerberusPodsBefore.join(',')})`);
  }

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
  // the pod clickhousePodName() happened to resolve to. is_initial_query=1
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
            ${durationUsExpr('')} AS dur_us
     FROM clusterAllReplicas('${CH_CLUSTER}', system.query_log)
     WHERE is_initial_query = 1 AND type = 'QueryFinish'
       AND event_time >= toDateTime(${windowStart}) AND event_time <= toDateTime(${windowEnd})`,
  );
  const traceIntervals = new Map();
  for (const row of traceIntervalRows) {
    const [trace, startUs, durUs] = row.split('\t');
    if (!traceIntervals.has(trace)) traceIntervals.set(trace, []);
    traceIntervals.get(trace).push({ startUs: Number(startUs), durUs: Number(durUs) });
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
  // ONE query over every is_initial_query=0 row in the window feeds both
  // point 2 (the overlap) and point 4 (the max_memory_usage setting each
  // child ran with) — the two assertions are about the SAME population, so
  // they read it once rather than each scoping it independently and
  // drifting apart. Columns beyond the interval itself:
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
  //     Select does, and a query with no query_kind filter counts them
  //     identically — inflating measured overlap with statements the gate
  //     was never designed to bound. CONFIRMED against real dispatch run
  //     34043234494 to fully explain at least one N=2 failure on its own
  //     (unfiltered peak 9 > cap 8; Select-only peak 7 <= cap 8).
  //   - initial_query_id (the coordinator's own globally-unique
  //     "<traceID>-<spanID>-<counter>" query_id — mintQueryID's process-wide
  //     atomic counter makes two distinct dispatches sharing one id
  //     structurally impossible) groups every child back to the ONE
  //     dispatch that produced it, independent of timing — which is what
  //     lets the "exercised" check below count DISTINCT dispatches at the
  //     peak instant, and lets a dispatch's own child count be read back as
  //     its physical-scan multiplier (children / DataShardCount).
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
  //   - mem is the child's own recorded max_memory_usage setting (a Map
  //     lookup, so '' when none was stamped), for point 4.
  //   - query_snippet is a short, whitespace-flattened prefix of the child's
  //     SQL so a surprising per-dispatch width can be matched against a
  //     known query SHAPE from the log alone.
  const shardStmtRows = chQueryTSV(
    initiatorPod,
    `SELECT toUnixTimestamp64Micro(c.query_start_time_microseconds) AS start_us, ${durationUsExpr('c.')} AS dur_us,
            c.query_kind, c.initial_query_id,
            if(i.client_hostname != '', i.client_hostname, c.client_hostname) AS cerberus_host,
            c.Settings['max_memory_usage'] AS mem,
            c.memory_usage AS mem_used,
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
    const [s, d, kind, qid, host, mem, memUsed, snippet] = r.split('\t');
    return { startUs: Number(s), durUs: Number(d), kind, qid, host, mem, memUsed: Number(memUsed), snippet };
  });
  const peakConcurrentShardStatements = maxConcurrent(shardStmts);
  const kindCounts = {};
  for (const r of shardStmts) kindCounts[r.kind] = (kindCounts[r.kind] || 0) + 1;
  log(`real per-shard statements observed: ${shardStmts.length}; peak concurrent (ALL query_kind, informational only)=${peakConcurrentShardStatements}; by query_kind: ${JSON.stringify(kindCounts)}`);

  // Select-only overlap is the ceiling DataShardFanoutGate actually bounds
  // (see the point-2 query's own doc above for the seeder's Insert/Alter
  // children, which were never subject to the gate). The real assertion is
  // scoped to query_kind='Select' — the only kind chclient's queryOpen /
  // queryCursorColumnar (this gate's one seam) ever dispatches.
  //
  // The same seeder also issues Selects: its stale-row pruning resolves a
  // cutoff with a `SELECT max(<time>)` over the Distributed tables
  // (test/e2e/seed/cmd/seed/sharded_mutation.go's resolveStaleCutoff) from
  // the runner host, and Distributed fans those out to per-shard Select
  // children too. query_kind cannot tell them apart from cerberus's own;
  // the initiator's client_hostname can (real run 34055887965: 31 such
  // children, host = the GitHub runner VM, peak 1). So the population the
  // gate assertions run over is: query_kind='Select' AND issued by a
  // cerberus pod — `cerberusPods` being the union of the pre- and
  // post-burst pod snapshots, so a pod that rolled mid-burst is still
  // "a cerberus pod" and not misread as a foreign host. Foreign-host
  // Selects are reported, never counted; an EMPTY host means the
  // attribution itself failed and is an error — that is the one shape that
  // would let a real over-cap dispatch hide.
  const allSelectRows = shardStmts.filter((r) => r.kind === 'Select');
  const unattributedSelectRows = allSelectRows.filter((r) => !r.host);
  const foreignSelectRows = allSelectRows.filter((r) => r.host && !cerberusPods.includes(r.host));
  const selectRows = allSelectRows.filter((r) => cerberusPods.includes(r.host));
  const selectPeak = peakOverlap(selectRows);
  const peakConcurrentSelectOnly = selectPeak.peak;
  log(`Select-only per-shard statements observed: ${allSelectRows.length} total; issued by cerberus pods: ${selectRows.length} (peak concurrent=${peakConcurrentSelectOnly}, most dispatches ever in flight together=${selectPeak.maxDistinctQidsLive}); issued by other hosts (direct native-protocol clients such as the rolling seeder — never gate-bound, informational only): ${foreignSelectRows.length}${foreignSelectRows.length ? ` from ${[...new Set(foreignSelectRows.map((r) => r.host))].join(',')} (peak concurrent=${maxConcurrent(foreignSelectRows)})` : ''}`);
  if (unattributedSelectRows.length > 0) {
    error(`${unattributedSelectRows.length} Select per-shard statement(s) carry no client hostname at all (neither on their initiator row nor on themselves) — per-process attribution via query_log.client_hostname failed for them, so the per-pod ceiling cannot be trusted; sample initial_query_id: ${unattributedSelectRows[0].qid}`);
    failures++;
  }
  if (selectRows.length === 0) {
    error('no per-shard Select statement was attributed to any cerberus pod during the burst — the assertions below would be vacuous');
    failures++;
  }

  // Per-dispatch width — grouped by initial_query_id (see the point-2
  // query's own doc above for why this is a safe, exact join key). A
  // dispatch's child count is `scans × DataShardCount` (one child per data
  // shard per physical table reference), so children / DataShardCount reads
  // back the physical-scan multiplier the gate charged for it
  // (chsql.EmitCounted -> chclient.WithDataShardFanoutMultiplier). Logged as
  // a regression signal on that multiplier: a dispatch whose children are
  // not a whole multiple of DataShardCount, or whose implied scan count is
  // wider than anything the gate was told, is the shape a future
  // under-charge would first show up as. The per-process/cluster ceilings
  // below are what FAIL on an under-charge; this is the log line that says
  // which dispatch shape did it.
  const childrenByQid = new Map();
  for (const r of selectRows) {
    if (!childrenByQid.has(r.qid)) childrenByQid.set(r.qid, []);
    childrenByQid.get(r.qid).push(r);
  }
  const widthHistogram = {};
  let maxScansObserved = 0;
  const raggedDispatches = [];
  for (const [qid, rows] of childrenByQid) {
    widthHistogram[rows.length] = (widthHistogram[rows.length] || 0) + 1;
    const scans = Math.ceil(rows.length / DATA_SHARD_COUNT);
    if (scans > maxScansObserved) maxScansObserved = scans;
    if (rows.length % DATA_SHARD_COUNT !== 0) raggedDispatches.push([qid, rows]);
  }
  // Cap how many ragged dispatches get their own log line — a
  // representative sample for manual correlation, not an exhaustive dump.
  const raggedSampleLimit = 10;
  log(`children per dispatch (by initial_query_id; each physical scan fans out to DataShardCount=${DATA_SHARD_COUNT} children): ${JSON.stringify(widthHistogram)} over ${childrenByQid.size} dispatch(es); max implied physical scans per dispatch=${maxScansObserved}`);
  if (raggedDispatches.length > 0) {
    log(`${raggedDispatches.length} dispatch(es) whose child count is not a whole multiple of DataShardCount=${DATA_SHARD_COUNT} (a child outside the window, or a fan-out the physicalScans multiplier did not predict):`);
    for (const [qid, rows] of raggedDispatches.slice(0, raggedSampleLimit)) {
      log(`  dispatch ${qid}: ${rows.length} children, own internal peak concurrency=${maxConcurrent(rows)}; sample query: ${rows[0].snippet}`);
    }
  }

  // Per-process contract: every cerberus-issued Select child attributed to
  // its pod (selectRows is already scoped to cerberus pods — see its own
  // doc above), each pod's own peak overlap within the per-process cap the
  // chart rendered.
  const selectByHost = new Map();
  for (const r of selectRows) {
    if (!selectByHost.has(r.host)) selectByHost.set(r.host, []);
    selectByHost.get(r.host).push(r);
  }
  for (const [host, hostRows] of selectByHost) {
    const hostPeak = maxConcurrent(hostRows);
    log(`  cerberus pod ${host}: ${hostRows.length} Select per-shard statements, peak concurrent=${hostPeak} (per-process cap ${perProcessCap})`);
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
  // Exercised at all: the ceiling is a CROSS-request bound, so the peak
  // instant must hold children of at least two distinct dispatches. A
  // single dispatch on its own already contributes scans × DataShardCount
  // overlapping children (a rate() over the metrics merge() union is 3
  // scans, so 6 at N=2), which is why "peak > DataShardCount" proves
  // nothing — one admitted request clears it alone.
  const minConcurrentDispatches = 2;
  if (selectRows.length > 0 && selectPeak.maxDistinctQidsLive < minConcurrentDispatches) {
    error(`at no instant were children of more than ${selectPeak.maxDistinctQidsLive} distinct dispatch(es) in flight together (peak was ${peakConcurrentSelectOnly} concurrent per-shard Select statements) — the burst never produced genuine CONCURRENT admitted requests, so DataShardFanoutGate's cross-request bound was never exercised`);
    failures++;
  }

  // ---- point 4: perShardMemoryBytes prediction + no OOM anywhere ----
  // Each child (is_initial_query=0) row's own initial_query_id names the
  // EXACT parent statement that dispatched it, so its trace prefix keys
  // straight into kEffByTrace above — this is what lets the ceiling below
  // be the real per-QUERY formula cap/(kEff*DataShardCount) rather than the
  // group-wide worst case, catching a regression that silently drops the
  // kEff term (which a flat cap/DataShardCount bound could never catch,
  // since every real kEff>=1 observation would still satisfy it). The
  // population is the SAME gate-bound one point 2 asserts over — Select
  // children issued by a cerberus pod — so the seeder's own Selects (never
  // stamped by cerberus at all) cannot fail, or vacuously pass, a check
  // about cerberus's own stamping.
  const memByTrace = new Map();
  for (const r of selectRows) {
    if (r.mem === '') continue;
    const trace = r.qid.slice(0, 32);
    if (!memByTrace.has(trace)) memByTrace.set(trace, new Set());
    memByTrace.get(trace).add(r.mem);
  }
  const memObservations = [...memByTrace].flatMap(([trace, mems]) => [...mems].map((mem) => [trace, mem]));
  log(`observed per-shard max_memory_usage settings (cerberus-issued Select children): ${[...new Set(memObservations.map(([, v]) => v))].join(', ') || '(none)'}`);
  if (selectRows.length > 0 && memObservations.length === 0) {
    error(`no cerberus-issued per-shard Select statement recorded a max_memory_usage setting — cannot confirm perShardMemoryBytes was ever stamped`);
    failures++;
  }
  for (const [trace, v] of memObservations) {
    const n = Number(v);
    // A child row whose parent trace never showed up in kEffByTrace (e.g. it
    // fell just outside the initiator-side window) is treated as kEff=1 —
    // the loosest, most conservative ceiling — rather than silently
    // skipped, so a real formula regression is never masked by a windowing
    // edge case.
    const kEff = kEffByTrace.get(trace) || 1;
    // Integer floor, mirroring chclient.ApportionMemoryBytes (cap / divisor
    // in int64 arithmetic, never below 1).
    const perQueryCeiling = Math.max(1, Math.floor(queryMaxMemoryBytes / (kEff * DATA_SHARD_COUNT)));
    if (!(n > 0) || n > perQueryCeiling) {
      error(`observed max_memory_usage=${v} (trace=${trace}, kEff=${kEff}) is not within (0, ${perQueryCeiling}] predicted by perShardMemoryBytes = cap/(kEff*DataShardCount) with cap=${queryMaxMemoryBytes}, kEff=${kEff}, DataShardCount=${DATA_SHARD_COUNT}`);
      failures++;
    }
  }

  // Real-usage headroom diagnostic, logged on EVERY run, pass or fail, so a
  // MEMORY_LIMIT_EXCEEDED failure is never the first time this lane learns
  // how close the tightest apportioned tier (highest observed kEff x
  // DataShardCount) actually runs to a real query's measured memory_usage.
  // Grouped by kEff, not by trace, since the question this answers is "does
  // headroom shrink as kEff grows" — exactly the axis perShardMemoryBytes
  // divides on.
  const usageByKEff = new Map();
  for (const r of selectRows) {
    if (!(r.memUsed > 0)) continue;
    const kEff = kEffByTrace.get(r.qid.slice(0, 32)) || 1;
    const ceiling = Math.max(1, Math.floor(queryMaxMemoryBytes / (kEff * DATA_SHARD_COUNT)));
    if (!usageByKEff.has(kEff)) usageByKEff.set(kEff, { max: 0, ceiling });
    const bucket = usageByKEff.get(kEff);
    if (r.memUsed > bucket.max) bucket.max = r.memUsed;
  }
  for (const [kEff, { max, ceiling }] of [...usageByKEff].sort((a, b) => a[0] - b[0])) {
    log(`kEff=${kEff}: peak real memory_usage observed=${max} against configured ceiling=${ceiling} (headroom=${(((ceiling - max) / ceiling) * 100).toFixed(1)}%)`);
  }

  const exceptionRows = chQueryTSV(
    initiatorPod,
    `SELECT c.initial_query_id, c.Settings['max_memory_usage'] AS mem,
            replaceRegexpAll(substring(c.query, 1, 300), '[\\t\\n\\r]+', ' ') AS query_snippet
     FROM clusterAllReplicas('${CH_CLUSTER}', system.query_log) AS c
     WHERE c.type = 'ExceptionWhileProcessing'
       AND (c.exception_code = 241 OR c.exception ILIKE '%Memory limit%')
       AND c.event_time >= toDateTime(${windowStart}) AND c.event_time <= toDateTime(${windowEnd})`,
  );
  const exceptionCount = exceptionRows.length;
  if (exceptionCount > 0) {
    error(`${exceptionCount} MEMORY_LIMIT_EXCEEDED exception(s) recorded in system.query_log during the burst — perShardMemoryBytes did not bound memory pressure as predicted`);
    for (const row of exceptionRows) {
      const [qid, mem, snippet] = row.split('\t');
      const kEff = kEffByTrace.get((qid || '').slice(0, 32)) || 1;
      log(`  exception: initial_query_id=${qid}, kEff=${kEff}, configured max_memory_usage=${mem}, query=${snippet}`);
    }
    failures++;
  }

  const restartsAfter = restartCounts(allPods);
  // Set difference against the pre-burst baseline (see oomKilledPods' own
  // doc): only a pod that was NOT already OOMKilled before the burst is
  // evidence the burst caused one.
  const oomedDuringBurst = oomKilledPods(allPods).filter((p) => !oomedBefore.has(p));
  for (const p of allPods) {
    if (restartsAfter[p] > restartsBefore[p]) {
      error(`ClickHouse pod ${p} restarted during the burst (restartCount ${restartsBefore[p]} -> ${restartsAfter[p]})`);
      failures++;
    }
  }
  if (oomedDuringBurst.length > 0) {
    error(`ClickHouse pod(s) newly OOMKilled during/after the burst: ${oomedDuringBurst.join(', ')}`);
    failures++;
  }

  if (failures > 0) {
    error(`e2e-datashard-verify: ${failures} assertion(s) failed`);
    process.exit(1);
  }
  notice(`e2e-datashard-verify: all assertions passed (dataShardCount=${DATA_SHARD_COUNT}, perProcessFanoutCap=${perProcessCap}, cerberusReplicas=${cerberusReplicas}, clusterFanoutCap=${clusterCap}, queryMaxMemoryBytes=${queryMaxMemoryBytes}, peakConcurrentSelectOnly=${peakConcurrentSelectOnly}, maxConcurrentDispatches=${selectPeak.maxDistinctQidsLive}, maxScansObserved=${maxScansObserved}, peakConcurrentAllQueryKinds=${peakConcurrentShardStatements}, maxKEffObserved=${maxKEffObserved})`);
}

// Run only as a script, never on import: the test suite imports this module
// for its pure helpers and must not start a kubectl-driven verification run.
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch((e) => {
    error(`unhandled error: ${e.stack || e}`);
    process.exit(1);
  });
}
