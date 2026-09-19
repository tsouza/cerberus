// Unit tests for e2e-bwc-replicated-verify.mjs's pure verdict functions.
//
// The verifier itself needs a live k3d cluster with a two-replica bundled
// ClickHouse; the three functions below are plain set/row comparisons over
// what `system.tables` and `system.replicas` answer on each pod, and they
// decide every assertion the lane fails on — so they are exactly the part
// that must be provable without a cluster. The shapes are the ones the lane
// produces: the OTel-CH table set on two pods named by StatefulSet ordinal.

import assert from 'node:assert/strict';
import test from 'node:test';

import { tableSetDefects, engineDefects, replicaRegistryDefects, topologyDefects } from './e2e-bwc-replicated-verify.mjs';

const POD0 = 'cerberus-clickhouse-0';
const POD1 = 'cerberus-clickhouse-1';

const TABLES = [
  'otel_logs',
  'otel_metrics_exponential_histogram',
  'otel_metrics_gauge',
  'otel_metrics_histogram',
  'otel_metrics_sum',
  'otel_metrics_summary',
  'otel_traces',
  'otel_traces_trace_id_ts',
  'otel_traces_trace_id_ts_mv',
];

// A complete system.tables answer for one pod, as the verifier reads it.
const fullSchema = (pod) =>
  TABLES.map((table) => ({ pod, table, engine: table.endsWith('_mv') ? 'MaterializedView' : 'ReplicatedMergeTree' }));

const perPod = (rows) =>
  Object.fromEntries([POD0, POD1].map((pod) => [pod, rows.filter((r) => r.pod === pod).map((r) => r.table)]));

test('both replicas carrying the whole schema is a pass', () => {
  assert.deepEqual(tableSetDefects(perPod([...fullSchema(POD0), ...fullSchema(POD1)])), []);
});

test('one table missing on replica 1 is a defect naming the table and both sides', () => {
  const rows = [...fullSchema(POD0), ...fullSchema(POD1).filter((r) => r.table !== 'otel_logs')];
  const defects = tableSetDefects(perPod(rows));
  assert.equal(defects.length, 1);
  assert.match(defects[0], /^otel_logs: present on cerberus-clickhouse-0, missing on cerberus-clickhouse-1$/);
});

test('a replica with no database at all reports every table missing there', () => {
  // The shape the `{shard}/{replica}` Keeper-path bug produces: cerberus
  // created the database on the replica the Service handed it, and the
  // other replica never attached one.
  const defects = tableSetDefects(perPod(fullSchema(POD0)));
  assert.equal(defects.length, TABLES.length);
  for (const d of defects) assert.match(d, /missing on cerberus-clickhouse-1$/);
});

test('no table on any pod is a defect, never a vacuous pass', () => {
  const defects = tableSetDefects({ [POD0]: [], [POD1]: [] });
  assert.equal(defects.length, 1);
  assert.match(defects[0], /no table exists in the database on any pod/);
});

test('every Replicated* MergeTree-family engine passes; a materialized view is not a MergeTree', () => {
  assert.deepEqual(engineDefects([...fullSchema(POD0), ...fullSchema(POD1)]), []);
});

test('a plain MergeTree inside the Replicated database is a defect', () => {
  const rows = [...fullSchema(POD0), ...fullSchema(POD1)].map((r) =>
    r.table === 'otel_metrics_sum' && r.pod === POD1 ? { ...r, engine: 'MergeTree' } : r,
  );
  const defects = engineDefects(rows);
  assert.equal(defects.length, 1);
  assert.match(defects[0], /^cerberus-clickhouse-1: otel_metrics_sum is MergeTree, not a Replicated\* engine/);
});

const replicated = TABLES.filter((t) => !t.endsWith('_mv'));
const registryFor = (pod, total, active = total) =>
  replicated.map((table) => ({ pod, table, totalReplicas: total, activeReplicas: active }));

test('every Replicated* table registered at total=active=N on every pod is a pass', () => {
  const defects = replicaRegistryDefects({
    pods: [POD0, POD1],
    replicatedTables: replicated,
    registry: [...registryFor(POD0, 2), ...registryFor(POD1, 2)],
    expected: 2,
  });
  assert.deepEqual(defects, []);
});

test('each replica alone in its own database (total_replicas 1) is the Keeper-path defect', () => {
  const defects = replicaRegistryDefects({
    pods: [POD0, POD1],
    replicatedTables: replicated,
    registry: [...registryFor(POD0, 1), ...registryFor(POD1, 1)],
    expected: 2,
  });
  assert.equal(defects.length, replicated.length * 2);
  assert.match(defects[0], /total_replicas=1 active_replicas=1, want 2\/2/);
});

test('a peer that registered but is not active is a defect', () => {
  const defects = replicaRegistryDefects({
    pods: [POD0, POD1],
    replicatedTables: ['otel_logs'],
    registry: [
      { pod: POD0, table: 'otel_logs', totalReplicas: 2, activeReplicas: 1 },
      { pod: POD1, table: 'otel_logs', totalReplicas: 2, activeReplicas: 2 },
    ],
    expected: 2,
  });
  assert.equal(defects.length, 1);
  assert.match(defects[0], /^cerberus-clickhouse-0: otel_logs reports total_replicas=2 active_replicas=1/);
});

test('a Replicated* table absent from a pod\'s system.replicas is a defect', () => {
  const defects = replicaRegistryDefects({
    pods: [POD0, POD1],
    replicatedTables: ['otel_logs'],
    registry: [{ pod: POD0, table: 'otel_logs', totalReplicas: 2, activeReplicas: 2 }],
    expected: 2,
  });
  assert.deepEqual(defects, ['cerberus-clickhouse-1: otel_logs is absent from system.replicas — the engine never registered as a replica here']);
});

test('the topology floor is two replicas', () => {
  assert.match(topologyDefects(1, [POD0])[0], /needs >= 2/);
  assert.match(topologyDefects(0, [])[0], /needs >= 2/);
  assert.deepEqual(topologyDefects(2, [POD0, POD1]), []);
  assert.match(topologyDefects(2, [POD0])[0], /found 1 ClickHouse pod/);
});
