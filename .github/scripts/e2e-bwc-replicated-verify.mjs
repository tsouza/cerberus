// e2e-bwc-replicated-verify.mjs — real-cluster proof that the Helm chart's
// SUPPORTED replication path, `clickhouse.bundled.replicas: 2` at the default
// `dataShards.count: 1`, actually replicates (cerberus issue #3566). Run by
// `just e2e-bwc-replicated-verify` after `just e2e-bwc-replicated-up`.
//
// Why this exists: every replication claim in the tree was render-only or
// single-node. `chart-render-assert.mjs` proves the chart EMITS a Keeper
// ensemble, a two-pod StatefulSet and the Replicated-database env wiring;
// the optcorpus `-tags=integration` tests prove a table REGISTERS as a
// replica on one node; nothing ever stood a second replica up and asked it
// whether the schema and the rows arrived. That is how the chart shipped a
// `zookeeperPath` default carrying `{shard}/{replica}` — a path the engine
// expands per node, so every replica registered its own unrelated
// single-replica database and nothing replicated — with every static check
// green. This script asks the pods, not the render.
//
// What it asserts, reading each ClickHouse pod directly via `kubectl exec`
// (never through the bundled Service, whose sessionAffinity would pin every
// query to ONE replica and hide exactly the divergence under test):
//
//   1. Topology. The StatefulSet's `.spec.replicas` is read back from the
//      live cluster and must be >= 2, and that many ClickHouse pods must
//      exist — a degenerate single-replica render cannot pass vacuously.
//   2. Schema on every replica. The set of tables in DB is read from
//      `system.tables` on EVERY pod and must be identical across pods and
//      non-empty. Polled to DDL_SYNC_SECONDS: cerberus ran its auto-create
//      DDL against whichever replica the Service handed it, and a Replicated
//      database applies that DDL on the other replicas asynchronously.
//   3. Engines and registration. Every MergeTree-family table is a
//      `Replicated*` engine (a plain `MergeTree` inside a Replicated database
//      is the silent per-replica partition docs/operations.md warns about),
//      and `system.replicas` on every pod reports every one of them with
//      `total_replicas` = `active_replicas` = the pod count. This is the
//      assertion the `{shard}/{replica}` path bug fails directly: each
//      replica's database rooted at its own node reports total_replicas 1.
//   4. Rows. For each pod, a marker row is inserted through that pod into
//      PROBE_TABLE and read back from every OTHER pod, after `SYSTEM SYNC
//      REPLICA` on the reader bounds the replication wait. Both directions,
//      so a one-way fetch path cannot pass.
//
// Env contract:
//   NAMESPACE             k8s namespace                                (default cerberus)
//   DB                    ClickHouse database cerberus auto-creates    (default otel)
//   CH_USER / CH_PASSWORD ClickHouse credentials                       (default cerberus/cerberus)
//   STATEFULSET           the bundled ClickHouse StatefulSet           (default cerberus-clickhouse)
//   PROBE_TABLE           table the marker rows are written to         (default otel_logs)
//   DDL_SYNC_SECONDS      bounded wait for the schema on every pod     (default 90)
//   ROW_SYNC_SECONDS      bounded wait for a marker row on a peer pod  (default 60)
//
// Exit 0 = every assertion passed; 1 = any failed (with ::error:: annotation).
//
// node: builtins only (via lib/gh.mjs / lib/k8s.mjs / lib/poll.mjs).

import process from 'node:process';
import { pathToFileURL } from 'node:url';
import { error, notice, log, capture } from './lib/gh.mjs';
import { makeKubectl, chQuery, chQueryRaw } from './lib/k8s.mjs';
import { pollUntil } from './lib/poll.mjs';

// A single-replica render has nothing to replicate to, so the topology
// assertion needs at least this many pods before any other check means
// anything.
export const MIN_REPLICAS = 2;

// The MergeTree family, as `system.tables.engine` spells it. Any of these
// WITHOUT the `Replicated` prefix inside a Replicated database is a table
// whose rows never leave the replica they were written to.
const MERGE_TREE_FAMILY_SUFFIX = 'MergeTree';
const REPLICATED_ENGINE_PREFIX = 'Replicated';

// The marker rows are written to PROBE_TABLE's (Timestamp, ServiceName,
// Body) columns; every other column takes its default. These three are the
// upstream OTel-CH logs columns the default schema always carries.
const PROBE_SERVICE_NAME = 'e2e-bwc-replicated-verify';

// tableSetDefects compares the per-pod table sets. `perPod` maps a pod name
// to the array of table names `system.tables` reports for the database on
// that pod. Returns one line per defect: a table present on some pods and
// missing on others (naming both sides), or an empty union (no table on any
// pod — cerberus's auto-create never ran, and there is nothing to compare).
export function tableSetDefects(perPod) {
  const pods = Object.keys(perPod).sort();
  const union = new Set();
  for (const pod of pods) for (const t of perPod[pod]) union.add(t);
  if (union.size === 0) {
    return [`no table exists in the database on any pod (${pods.join(', ')}) — the auto-create DDL never ran`];
  }
  const defects = [];
  for (const table of [...union].sort()) {
    const present = pods.filter((p) => perPod[p].includes(table));
    const missing = pods.filter((p) => !perPod[p].includes(table));
    if (missing.length > 0) {
      defects.push(`${table}: present on ${present.join(', ')}, missing on ${missing.join(', ')}`);
    }
  }
  return defects;
}

// engineDefects flags every MergeTree-family table that is not a Replicated*
// engine. `rows` is [{ pod, table, engine }] as read from `system.tables` on
// each pod.
export function engineDefects(rows) {
  const defects = [];
  for (const { pod, table, engine } of rows) {
    if (engine.endsWith(MERGE_TREE_FAMILY_SUFFIX) && !engine.startsWith(REPLICATED_ENGINE_PREFIX)) {
      defects.push(`${pod}: ${table} is ${engine}, not a Replicated* engine — its rows never leave this replica`);
    }
  }
  return defects;
}

// replicaRegistryDefects checks `system.replicas` on every pod against the
// expected replica count. `replicatedTables` is the array of table names
// engineDefects found to be Replicated* engines (the same on every pod once
// tableSetDefects is empty); `registry` is [{ pod, table, totalReplicas,
// activeReplicas }] as read from `system.replicas` on each pod; `pods` the
// pod names. A Replicated* table absent from a pod's `system.replicas`, or
// present with a count other than `expected`, is a defect — the latter is
// exactly what a per-node Keeper root produces (each replica alone in its
// own database: total_replicas 1).
export function replicaRegistryDefects({ pods, replicatedTables, registry, expected }) {
  const defects = [];
  for (const pod of [...pods].sort()) {
    const byTable = new Map(registry.filter((r) => r.pod === pod).map((r) => [r.table, r]));
    for (const table of [...replicatedTables].sort()) {
      const row = byTable.get(table);
      if (!row) {
        defects.push(`${pod}: ${table} is absent from system.replicas — the engine never registered as a replica here`);
        continue;
      }
      if (row.totalReplicas !== expected || row.activeReplicas !== expected) {
        defects.push(
          `${pod}: ${table} reports total_replicas=${row.totalReplicas} active_replicas=${row.activeReplicas}, want ${expected}/${expected} — ` +
            `the replicas are not registered under one shared Keeper path`,
        );
      }
    }
  }
  return defects;
}

const NS = process.env.NAMESPACE || 'cerberus';
const DB = process.env.DB || 'otel';
const CH_USER = process.env.CH_USER || 'cerberus';
const CH_PASSWORD = process.env.CH_PASSWORD || 'cerberus';
const STATEFULSET = process.env.STATEFULSET || 'cerberus-clickhouse';
const PROBE_TABLE = process.env.PROBE_TABLE || 'otel_logs';
const DDL_SYNC_SECONDS = Number(process.env.DDL_SYNC_SECONDS || '90');
const ROW_SYNC_SECONDS = Number(process.env.ROW_SYNC_SECONDS || '60');

const kubectl = makeKubectl(capture, NS);
const CH_OPTS = { user: CH_USER, password: CH_PASSWORD };

// TSVRaw so multi-column rows split unambiguously without a JSON round trip
// (same convention the datashard verifiers use for the same reason).
function chQueryTSV(pod, sql) {
  const out = chQuery(kubectl, pod, { ...CH_OPTS, format: 'TSVRaw' }, sql);
  return out
    .split('\n')
    .map((l) => l.trimEnd())
    .filter((l) => l.length > 0)
    .map((l) => l.split('\t'));
}

function fail(lines) {
  for (const line of lines) error(line);
  process.exit(1);
}

// clickhousePods lists the bundled ClickHouse pods by the chart's immutable
// component label, sorted so ordinal 0 comes first. Keeper carries a
// distinct component label (`clickhouse-keeper`) and is not matched.
function clickhousePods() {
  const res = kubectl(['get', 'pod', '-l', 'app.kubernetes.io/component=clickhouse', '-o', 'jsonpath={.items[*].metadata.name}']);
  if (res.status !== 0) {
    fail([`could not list ClickHouse pods in namespace ${NS}: ${res.stderr.trim()}`]);
  }
  return res.stdout.trim().split(/\s+/).filter((n) => n.length > 0).sort();
}

function statefulSetReplicas() {
  const res = kubectl(['get', 'statefulset', STATEFULSET, '-o', 'jsonpath={.spec.replicas}']);
  const n = Number(res.stdout.trim());
  if (res.status !== 0 || !Number.isInteger(n)) {
    fail([`could not read statefulset/${STATEFULSET} .spec.replicas: ${res.stderr.trim()}`]);
  }
  return n;
}

// tablesOn reads the (name, engine) pairs of every table in DB on one pod.
// Uses chQueryRaw so a pod that cannot answer yet (a replica still attaching
// the database) reads as "no tables so far" inside the DDL-sync poll rather
// than aborting it.
function tablesOn(pod) {
  const res = chQueryRaw(kubectl, pod, { ...CH_OPTS, format: 'TSVRaw' },
    `SELECT name, engine FROM system.tables WHERE database = '${DB}' ORDER BY name`);
  if (res.status !== 0) {
    log(`    ${pod}: system.tables not readable yet: ${res.stderr.trim().split('\n')[0]}`);
    return [];
  }
  return res.stdout
    .split('\n')
    .map((l) => l.trimEnd())
    .filter((l) => l.length > 0)
    .map((l) => {
      const [table, engine] = l.split('\t');
      return { pod, table, engine };
    });
}

function replicaRegistryOn(pod) {
  return chQueryTSV(pod,
    `SELECT table, total_replicas, active_replicas FROM system.replicas WHERE database = '${DB}' ORDER BY table`,
  ).map(([table, total, active]) => ({ pod, table, totalReplicas: Number(total), activeReplicas: Number(active) }));
}

function insertMarker(pod, marker) {
  chQuery(kubectl, pod, { ...CH_OPTS, database: DB },
    `INSERT INTO ${PROBE_TABLE} (Timestamp, ServiceName, Body) VALUES (now64(9), '${PROBE_SERVICE_NAME}', '${marker}')`);
}

function markerCount(pod, marker) {
  return Number(chQuery(kubectl, pod, { ...CH_OPTS, database: DB },
    `SELECT count() FROM ${PROBE_TABLE} WHERE ServiceName = '${PROBE_SERVICE_NAME}' AND Body = '${marker}'`));
}

async function main() {
  // ---- 1. topology -------------------------------------------------------
  const expected = statefulSetReplicas();
  const pods = clickhousePods();
  log(`bwc-replicated verify: namespace=${NS} db=${DB} statefulset=${STATEFULSET} spec.replicas=${expected} pods=${pods.join(',')}`);
  if (expected < MIN_REPLICAS) {
    fail([`statefulset/${STATEFULSET} has .spec.replicas=${expected}; this lane needs >= ${MIN_REPLICAS} to observe replication at all`]);
  }
  if (pods.length !== expected) {
    fail([`found ${pods.length} ClickHouse pod(s) (${pods.join(', ')}) but statefulset/${STATEFULSET} declares ${expected}`]);
  }

  // ---- 2. schema on every replica ----------------------------------------
  let rows = [];
  let setDefects = [];
  const synced = await pollUntil(
    async () => {
      rows = pods.flatMap((pod) => tablesOn(pod));
      const perPod = Object.fromEntries(pods.map((pod) => [pod, rows.filter((r) => r.pod === pod).map((r) => r.table)]));
      setDefects = tableSetDefects(perPod);
      return setDefects.length === 0;
    },
    { deadlineMs: DDL_SYNC_SECONDS * 1000, label: 'ddl-sync' },
  );
  if (!synced) {
    fail([
      `after ${DDL_SYNC_SECONDS}s the database '${DB}' is not identical on every ClickHouse pod — the auto-created schema did not replicate:`,
      ...setDefects.map((d) => `  ${d}`),
    ]);
  }
  const tables = [...new Set(rows.map((r) => r.table))].sort();
  log(`  schema identical on ${pods.length} pods: ${tables.length} tables (${tables.join(', ')})`);

  // ---- 3. engines + system.replicas registration -------------------------
  const engDefects = engineDefects(rows);
  if (engDefects.length > 0) fail(['a table in the Replicated database does not replicate its rows:', ...engDefects.map((d) => `  ${d}`)]);
  const replicatedTables = [...new Set(rows.filter((r) => r.engine.startsWith(REPLICATED_ENGINE_PREFIX)).map((r) => r.table))].sort();
  if (replicatedTables.length === 0) {
    fail([`no Replicated* engine table in '${DB}' on any pod — nothing here replicates rows`]);
  }
  const registry = pods.flatMap((pod) => replicaRegistryOn(pod));
  const regDefects = replicaRegistryDefects({ pods, replicatedTables, registry, expected });
  if (regDefects.length > 0) fail(['system.replicas disagrees with the chart topology:', ...regDefects.map((d) => `  ${d}`)]);
  log(`  ${replicatedTables.length} Replicated* tables registered with total_replicas=active_replicas=${expected} on every pod`);

  if (!tables.includes(PROBE_TABLE)) {
    fail([`probe table '${PROBE_TABLE}' is not among the auto-created tables (${tables.join(', ')}); set PROBE_TABLE to one of them`]);
  }

  // ---- 4. rows written through each pod read from every other -----------
  for (const writer of pods) {
    const marker = `replication-probe-${writer}-${Date.now()}`;
    insertMarker(writer, marker);
    for (const reader of pods) {
      if (reader === writer) continue;
      const seen = await pollUntil(
        async () => {
          // SYSTEM SYNC REPLICA blocks until this replica has applied every
          // queued fetch, so the count below is the settled state rather
          // than a race against the replication queue. A failed sync
          // (session hiccup) simply retries on the next poll.
          const sync = chQueryRaw(kubectl, reader, { ...CH_OPTS, database: DB }, `SYSTEM SYNC REPLICA ${PROBE_TABLE}`);
          if (sync.status !== 0) {
            log(`    ${reader}: SYSTEM SYNC REPLICA ${PROBE_TABLE} failed: ${sync.stderr.trim().split('\n')[0]}`);
            return false;
          }
          return markerCount(reader, marker) === 1;
        },
        { deadlineMs: ROW_SYNC_SECONDS * 1000, label: `row-sync ${writer}->${reader}` },
      );
      if (!seen) {
        fail([`a row inserted into ${DB}.${PROBE_TABLE} through ${writer} was not readable from ${reader} within ${ROW_SYNC_SECONDS}s (marker ${marker})`]);
      }
      log(`  row written through ${writer} read back from ${reader}`);
    }
  }

  notice(
    `bwc-replicated verify PASSED: ${tables.length} tables identical on ${pods.length} replicas, ` +
      `${replicatedTables.length} Replicated* tables at total_replicas=${expected} on every pod, ` +
      `rows replicate in every direction (${pods.join(' <-> ')})`,
    { title: 'e2e-bwc-replicated-verify' },
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
