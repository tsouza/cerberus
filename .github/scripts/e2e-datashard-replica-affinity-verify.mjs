// e2e-datashard-replica-affinity-verify.mjs — real-cluster proof that
// ClickHouse's OWN remote-shard replica selection is deterministic across
// the several statements ONE cerberus multi-statement request fans out
// (cerberus issue #3086, epic #3074).
//
// Background (full decision record: internal/chclient/
// distributed_query_settings.go, docs/helm-clickhouse.md's "#3075
// compatibility" section): `sessionAffinity: ClientIP` (issue #3075) pins a
// cerberus pod's own connection to one replica of the ONE shard it dials
// directly, but once that connection issues a query against a `Distributed`
// wrapper table, ClickHouse's OWN internal replica-selection logic — not any
// k8s Service — picks which replica of EVERY OTHER shard answers,
// independently per statement. Two statements from the SAME cerberus
// multi-statement request (e.g. the sharded-pushdown solver's own
// time-range fan-out) could therefore land on two DIFFERENT replicas of the
// SAME remote shard, reopening the exact cross-replica divergence risk
// sessionAffinity exists to close, one level removed.
//
// Cerberus now stamps `load_balancing=first_or_random` +
// `load_balancing_first_offset=0` UNCONDITIONALLY on every data-plane query.
// This script proves what `system.query_log` — not cerberus's own HTTP
// responses — actually observed on a REAL cluster where every data shard has
// MORE THAN ONE replica (the one topology no other e2e lane in this
// repository stands up — `e2e-datashard-verify.mjs`'s own lane runs
// `replicas: 1`, so it has no replica diversity to select from at all):
//
//   1. The topology itself is genuinely multi-replica-per-shard — read back
//      from `system.clusters`, not merely asserted — so this leg cannot pass
//      vacuously against a degenerate single-replica render.
//   2. A genuine multi-statement trace (a solver-split request, which
//      dispatches more than one initiator statement under one trace id)
//      reached the `Distributed` target — otherwise there is no SECOND
//      statement to compare a first one against, and the whole leg would be
//      vacuous. Counted as statements per trace, NOT as kEff: kEff is the
//      solver's peak concurrent shard count, which this leg has no reason
//      to measure — two statements one after another are enough to observe
//      two independent replica selections.
//   3. For every such trace, every remote-shard child statement touching
//      the SAME data shard landed on the SAME physical replica
//      (`system.query_log.hostname`) — the central claim issue #3086 set out
//      to answer, observed directly rather than inferred. Asserted only
//      over (trace, shard) groups that hold at least TWO child statements;
//      a group of one has nothing to agree with, and a run in which no
//      group reached two is reported as vacuous rather than passed.
//   4. That shared replica is specifically each shard's OWN ordinal-0 pod —
//      confirming the MECHANISM (first_or_random + offset 0 always prefers
//      the config-order-first replica), not merely a coincidental agreement.
//
// Point 3 is what a regression would break: revert the load_balancing pin to
// ClickHouse's own default (`random`) and this assertion starts failing
// intermittently the moment a split trace's remote-shard children are
// dispatched to more than one replica — this is not a tautology.
//
// Env contract:
//   NAMESPACE               k8s namespace                       (default cerberus)
//   CERBERUS_URL            cerberus HTTP endpoint               (default http://localhost:8080)
//   DB                      ClickHouse database                    (default otel)
//   CH_USER / CH_PASSWORD   ClickHouse credentials                 (default cerberus/cerberus)
//   CH_CLUSTER              ClickHouse cluster name                 (default bwc_cluster)
//   DATA_SHARD_COUNT        expected DataShardCount                 (required)
//   REPLICAS                expected replicas PER shard             (required, must be >= 2)
//   REQUEST_COUNT           sequential solver-splitting requests fired (default 3)
//   FLUSH_WAIT_SECONDS      settle time before SYSTEM FLUSH LOGS    (default 10)
//   HEALTH_POLL_SECONDS     bounded wait for a clean errors_count   (default 60)
//
// Exit 0 = every assertion passed; 1 = any failed (with ::error:: annotation).
//
// THE RACE THIS SCRIPT MUST NOT LOSE (cerberus issue #3148, same class as
// #3109/PR #3125's mode-toggle readiness race): `just e2e-datashard-up`'s
// readiness gate is `kubectl rollout status`, which only proves every
// ClickHouse pod's own container passed its liveness/readiness probe — NOT
// that every node's inter-node connections to its PEER replicas are already
// healthy. A pod can report Ready to Kubernetes seconds before it can
// actually dial a sibling shard's replica; the FIRST time an initiator
// reaches a still-starting peer, that dial is refused, and ClickHouse
// increments that replica's `error_count` server-side
// (`PoolWithFailoverBase::Pool::error_count`, decaying only over
// `distributed_replica_error_half_life`, 60s default) — during that decay
// window `first_or_random` can legitimately prefer a NON-offset-0 replica,
// which is exactly the false positive this leg exists to rule out (observed
// live: run 34103918151, cerberus issue #3148). `system.clusters.errors_count`
// is that same error-tracking state, readable directly, so this script waits
// for a clean state (every row's `errors_count = 0`) via
// `lib/k8s.mjs`'s `waitForClusterHealth` — promoted there by cerberus issue
// #3172 so any OTHER datashard-lane script that fires load right after
// cluster-up can wait out the same race, not just this one — before firing
// the affinity-verify burst. That proves the fan-out load only starts once
// the cluster has actually settled, rather than inferring settlement from
// pod readiness alone.

import process from 'node:process';
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
const REPLICAS = Number(process.env.REPLICAS || '0');
const REQUEST_COUNT = Number(process.env.REQUEST_COUNT || '3');
const FLUSH_WAIT_SECONDS = Number(process.env.FLUSH_WAIT_SECONDS || '10');
const HEALTH_POLL_SECONDS = Number(process.env.HEALTH_POLL_SECONDS || '60');

if (!DATA_SHARD_COUNT || DATA_SHARD_COUNT < 2) {
  error(`DATA_SHARD_COUNT must be a real data-shard count (>= 2), got ${process.env.DATA_SHARD_COUNT}`);
  process.exit(1);
}
if (!REPLICAS || REPLICAS < 2) {
  error(`REPLICAS must be >= 2 — this leg exists to observe cross-REPLICA selection, got ${process.env.REPLICAS}`);
  process.exit(1);
}

const kubectl = makeKubectl(capture, NS);
const CH_OPTS = { database: DB, user: CH_USER, password: CH_PASSWORD };

// TSVRaw so multi-column rows split unambiguously without a JSON round trip
// (same convention e2e-datashard-verify.mjs uses for the same reason).
function chQueryTSV(pod, sql) {
  const out = chQuery(kubectl, pod, { ...CH_OPTS, format: 'TSVRaw' }, sql);
  return out.split('\n').map((l) => l.trimEnd()).filter((l) => l.length > 0);
}

function chExec(pod, sql) {
  chQuery(kubectl, pod, CH_OPTS, sql);
}

// A wide range (3h) at a fine step (5s) yields ~2160 anchors — combined
// with cerberus-values-datashard.yaml's collapsed MinFanout/MinAnchorPairs/
// MinAnchorsPerSlice (reused unchanged as this lane's base overlay), this
// reliably produces a multi-statement (solver-split) trace without needing
// concurrent load — see that file's own doc for why.
function wideRangeURL() {
  const now = Math.floor(Date.now() / 1000);
  const start = now - 3 * 60 * 60;
  const promQ = encodeURIComponent('rate(http_server_request_duration_count[5m])');
  return `${CERBERUS_URL}/api/v1/query_range?query=${promQ}&start=${start}&end=${now}&step=5`;
}

async function fireOne(url) {
  try {
    const resp = await fetch(url, { signal: AbortSignal.timeout(20000) });
    return resp.status;
  } catch (e) {
    return `error:${e.message}`;
  }
}

// hostnameShardReplica parses this chart's own pod-naming convention
// (deploy/helm/cerberus/templates/clickhouse/_helpers.tpl's
// fullnameForDataShard: `<fullname>-datashard-<shard>`, StatefulSet pod
// hostname `<sts-name>-<ordinal>`) into { shard, replica }, or null for a
// hostname that does not match (never expected on this lane's own cluster,
// but never silently misclassified as shard "0" either).
//
// `system.query_log.hostname` reports the pod's FULLY QUALIFIED domain name
// on this cluster (e.g.
// `cerberus-clickhouse-datashard-1-0.cerberus-clickhouse-headless-datashard-1
// .cerberus.svc.cluster.local`), so the `-datashard-<shard>-<ordinal>`
// segment is followed by the FQDN's own `.` domain separator, not
// necessarily end-of-string — issue #3131.
function hostnameShardReplica(hostname) {
  const m = /-datashard-(\d+)-(\d+)(?:\.|$)/.exec(hostname);
  if (!m) return null;
  return { shard: Number(m[1]), replica: Number(m[2]) };
}

async function main() {
  const pod = clickhousePodName(kubectl, NS);
  log(`replica-affinity verify: namespace=${NS} db=${DB} cluster=${CH_CLUSTER} dataShardCount=${DATA_SHARD_COUNT} replicas=${REPLICAS} pod=${pod}`);
  let failures = 0;

  // ---- point 1: the topology is genuinely multi-replica-per-shard ----
  const clusterRows = chQueryTSV(
    pod,
    `SELECT shard_num, count(DISTINCT replica_num) AS n
     FROM system.clusters WHERE cluster = '${CH_CLUSTER}'
     GROUP BY shard_num ORDER BY shard_num`,
  );
  if (clusterRows.length !== DATA_SHARD_COUNT) {
    error(`system.clusters reports ${clusterRows.length} shard(s) for cluster '${CH_CLUSTER}'; want DATA_SHARD_COUNT=${DATA_SHARD_COUNT}`);
    failures++;
  }
  for (const row of clusterRows) {
    const [shardNum, n] = row.split('\t');
    if (Number(n) !== REPLICAS) {
      error(`system.clusters shard_num=${shardNum} reports ${n} distinct replica(s); want REPLICAS=${REPLICAS} — this leg's own topology is not what it claims, every assertion below would be vacuous`);
      failures++;
    }
  }
  if (failures > 0) {
    error(`replica-affinity verify: ${failures} topology assertion(s) failed before firing any load — aborting rather than running a vacuous check`);
    process.exit(1);
  }
  log(`topology confirmed: ${clusterRows.length} shard(s), ${REPLICAS} replica(s) each`);

  // ---- settle window: wait for a genuinely healthy inter-node state ----
  // See the module doc's "THE RACE THIS SCRIPT MUST NOT LOSE" section —
  // `just e2e-datashard-up`'s pod-readiness gate proves nothing about
  // whether every node can already reach every peer replica.
  try {
    const settleSeconds = await waitForClusterHealth(kubectl, pod, CH_OPTS, {
      cluster: CH_CLUSTER,
      deadlineMs: HEALTH_POLL_SECONDS * 1000,
    });
    log(`cluster health confirmed clean (errors_count=0 for every replica) after ${settleSeconds.toFixed(1)}s`);
  } catch (e) {
    error(e.message);
    process.exit(1);
  }

  // ---- fire REQUEST_COUNT sequential solver-splitting requests ----
  const burstStartMs = Date.now();
  const statuses = [];
  for (let i = 0; i < REQUEST_COUNT; i++) {
    statuses.push(await fireOne(wideRangeURL()));
  }
  const burstEndMs = Date.now();
  const serverErrors = statuses.filter((s) => typeof s === 'number' && s >= 500);
  log(`fired ${statuses.length} request(s): statuses=${statuses.join(',')}`);
  if (serverErrors.length > 0) {
    error(`cerberus returned ${serverErrors.length} 5xx response(s) while firing the replica-affinity load`);
    failures++;
  }

  await sleep(FLUSH_WAIT_SECONDS * 1000);
  chExec(pod, `SYSTEM FLUSH LOGS ON CLUSTER ${CH_CLUSTER}`);

  const windowStart = Math.floor(burstStartMs / 1000) - 5;
  const windowEnd = Math.floor(burstEndMs / 1000) + FLUSH_WAIT_SECONDS + 10;

  // ---- point 2: a genuine multi-statement trace happened ----
  // statementsPerTrace is a flat count of a trace's initiator statements
  // over the window — deliberately not the peak-overlap kEff
  // e2e-datashard-verify.mjs derives, because sequential statements are
  // exactly as good as concurrent ones for observing replica selection.
  const traceStatementRows = chQueryTSV(
    pod,
    `SELECT substring(query_id, 1, 32) AS trace, count() AS statements
     FROM clusterAllReplicas('${CH_CLUSTER}', system.query_log)
     WHERE is_initial_query = 1 AND type = 'QueryFinish'
       AND event_time >= toDateTime(${windowStart}) AND event_time <= toDateTime(${windowEnd})
     GROUP BY trace`,
  );
  const multiStatementTraces = traceStatementRows
    .map((r) => r.split('\t'))
    .filter(([, statementsPerTrace]) => Number(statementsPerTrace) > 1)
    .map(([trace]) => trace);
  log(`observed ${traceStatementRows.length} initiator trace(s); ${multiStatementTraces.length} multi-statement (solver-split) trace(s)`);
  if (multiStatementTraces.length === 0) {
    error('no multi-statement (solver-split) trace was observed in system.query_log — this leg is vacuous without one to compare statements within');
    process.exit(1);
  }

  // ---- points 3 + 4: same-shard child statements within one trace share
  // the SAME replica, and it is each shard's own ordinal-0 pod ----
  const childRows = chQueryTSV(
    pod,
    `SELECT substring(initial_query_id, 1, 32) AS trace, hostname
     FROM clusterAllReplicas('${CH_CLUSTER}', system.query_log)
     WHERE is_initial_query = 0 AND type = 'QueryFinish'
       AND event_time >= toDateTime(${windowStart}) AND event_time <= toDateTime(${windowEnd})`,
  );
  const multiStatementTraceSet = new Set(multiStatementTraces);
  // (trace, shard) -> { hosts: Set<hostname>, n: child statement count }.
  // `n` is what makes the assertion non-vacuous: a group holding ONE child
  // statement trivially "agrees with itself", so only groups with n >= 2
  // are evidence, and a run where no group reaches 2 has observed nothing.
  const byTraceShard = new Map();
  for (const row of childRows) {
    const [trace, hostname] = row.split('\t');
    if (!multiStatementTraceSet.has(trace)) continue;
    const sr = hostnameShardReplica(hostname);
    if (!sr) {
      error(`child statement hostname ${JSON.stringify(hostname)} does not match this chart's <fullname>-datashard-<shard>-<ordinal> convention — cannot classify`);
      failures++;
      continue;
    }
    const key = `${trace}\t${sr.shard}`;
    if (!byTraceShard.has(key)) byTraceShard.set(key, { hosts: new Set(), n: 0 });
    const group = byTraceShard.get(key);
    group.hosts.add(hostname);
    group.n++;
  }
  if (byTraceShard.size === 0) {
    error('no remote-shard child statement (is_initial_query=0) was observed for any multi-statement trace — cannot assert cross-statement replica affinity');
    process.exit(1);
  }
  // The smallest group size that can show two statements agreeing.
  const minGroupSizeForAffinity = 2;
  let groupsChecked = 0;
  let maxGroupSize = 0;
  for (const [key, { hosts, n }] of byTraceShard) {
    const [trace, shard] = key.split('\t');
    if (n > maxGroupSize) maxGroupSize = n;
    if (n < minGroupSizeForAffinity) continue;
    groupsChecked++;
    if (hosts.size !== 1) {
      error(`trace ${trace} data-shard ${shard}: ${hosts.size} DIFFERENT replicas served this ONE trace's ${n} statements (${[...hosts].join(', ')}) — cross-statement replica divergence reopened; the load_balancing pin did not hold`);
      failures++;
      continue;
    }
    const [hostname] = hosts;
    const sr = hostnameShardReplica(hostname);
    if (sr.replica !== 0) {
      error(`trace ${trace} data-shard ${shard}: all ${n} statements consistently served by replica ordinal ${sr.replica} (${hostname}), not the expected ordinal-0 — the OBSERVED behavior no longer matches the first_or_random+offset=0 mechanism this pin relies on`);
      failures++;
    }
  }
  log(`checked ${groupsChecked} (trace, data-shard) group(s) holding >= ${minGroupSizeForAffinity} statements (of ${byTraceShard.size} groups total, largest ${maxGroupSize} statements) across ${multiStatementTraces.length} multi-statement trace(s)`);
  if (groupsChecked === 0) {
    error(`every (trace, data-shard) group held a single child statement (largest group ${maxGroupSize}) — no group had two statements whose replicas could be compared, so the affinity assertion is vacuous on this run`);
    failures++;
  }

  if (failures > 0) {
    error(`e2e-datashard-replica-affinity-verify: ${failures} assertion(s) failed`);
    process.exit(1);
  }
  notice(`e2e-datashard-replica-affinity-verify: all assertions passed (dataShardCount=${DATA_SHARD_COUNT}, replicas=${REPLICAS}, multiStatementTraces=${multiStatementTraces.length}, groupsChecked=${groupsChecked}, maxGroupSize=${maxGroupSize})`);
}

main().catch((e) => {
  error(`unhandled error: ${e.stack || e}`);
  process.exit(1);
});
