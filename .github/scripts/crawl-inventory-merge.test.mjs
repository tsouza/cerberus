// crawl-inventory-merge.test.mjs — node:test guard for the crawl shard
// slice union-merge's coverage invariant (the partition guarantee).
//
// Runs on the cheap gate lane (`node --test .github/scripts/*.test.mjs`)
// — no setup-node, no k3d, no deps. Proves the merge is an EXACT,
// DISJOINT, TOTAL cover and that its assertions actually fire.

import { test } from 'node:test';
import assert from 'node:assert/strict';

import {
  fnv1a32,
  baseSurfaceOf,
  ownsSurface,
  collectViolations,
  mergeShardSlices,
} from './crawl-inventory-merge.mjs';

// The same key cross-section the spec's sharding pins use — bare
// surfaces, dashboards, structural-param surfaces, parameterized
// families, in-place states.
const SAMPLE_KEYS = [
  '/',
  '/dashboards',
  '/explore',
  '/d/cerberus-self',
  '/d/clickhouse',
  '/a/grafana-exploretraces-app/explore',
  '/a/grafana-exploretraces-app/explore?var-groupBy=kind',
  '/a/grafana-exploretraces-app/explore?actionView=comparison&var-groupBy=kind',
  '/a/grafana-metricsdrilldown-app/drilldown?metric={metric}',
  '/a/grafana-lokiexplore-app/explore/service/{service}/logs',
  '/a/grafana-lokiexplore-app/explore/service/{service}/logs?visualizationType="table"',
  '/a/grafana-exploretraces-app/explore#var-metric={rep}',
  '/d/cerberus-self#tab=overview',
];

const sliceFor = (index, count, keys = SAMPLE_KEYS) => ({
  stack: 'k3d',
  shardIndex: index,
  shardCount: count,
  surfaces: keys
    .filter((k) => ownsSurface(k, index, count))
    .map((url) => ({ url, lean: false })),
});

test('fnv1a32 is deterministic and unsigned (lockstep anchor with sharding.ts)', () => {
  // A fixed vector — if this changes, the .mjs and .ts hashes diverged.
  assert.equal(fnv1a32(''), 0x811c9dc5);
  assert.equal(fnv1a32('a'), 0xe40c292c);
  assert.equal(fnv1a32('/'), fnv1a32('/'));
  assert.ok(fnv1a32('/dashboards') >= 0);
});

test('baseSurfaceOf strips both the query and the state suffix to the bare path', () => {
  assert.equal(baseSurfaceOf('/a/x/explore?var-groupBy=kind'), '/a/x/explore');
  assert.equal(baseSurfaceOf('/a/x/explore#var-metric={rep}'), '/a/x/explore');
  assert.equal(
    baseSurfaceOf('/a/x/explore?actionView=comparison#k=v'),
    '/a/x/explore',
  );
  assert.equal(baseSurfaceOf('/d/abc'), '/d/abc');
});

test('a surface + its structural-param children + in-place states co-locate on one shard', () => {
  const path = '/a/grafana-exploretraces-app/explore';
  const family = [
    path,
    `${path}?var-groupBy=kind`,
    `${path}?actionView=comparison&var-groupBy=kind`,
    `${path}#var-metric={rep}`,
  ];
  for (const count of [2, 3, 4]) {
    for (let index = 0; index < count; index++) {
      const owned = family.map((k) => ownsSurface(k, index, count));
      assert.ok(
        owned.every((v) => v === owned[0]),
        `family not co-located at ${count}/${index}`,
      );
    }
  }
});

test('the partition is total, disjoint, deterministic over the sample set', () => {
  for (const count of [1, 2, 3, 4, 5]) {
    for (const k of SAMPLE_KEYS) {
      const owners = [];
      for (let index = 0; index < count; index++) {
        if (ownsSurface(k, index, count)) owners.push(index);
      }
      assert.equal(owners.length, 1, `${k} owned by ${owners.length} shards at count ${count}`);
    }
  }
});

test('mergeShardSlices: the union of owned slices is the EXACT full set, disjoint', () => {
  const count = 3;
  const slices = Array.from({ length: count }, (_, i) => sliceFor(i, count));
  const merged = mergeShardSlices(slices, 'doc', 'k3d');
  assert.deepEqual(
    new Set(merged.surfaces.map((s) => s.url)),
    new Set(SAMPLE_KEYS),
  );
  assert.equal(merged.surfaces.length, SAMPLE_KEYS.length, 'no duplicates');
  assert.equal(merged.stack, 'k3d');
  assert.equal(merged.doc, 'doc');
});

test('collectViolations flags a missing shard slice (coverage gap)', () => {
  const v = collectViolations([sliceFor(0, 2)]);
  assert.ok(v.some((m) => m.includes('missing slice for shard index 1')), v.join('\n'));
});

test('collectViolations flags a double-owned surface (not disjoint)', () => {
  const count = 2;
  const a = sliceFor(0, count);
  const b = sliceFor(1, count);
  const dupUrl = a.surfaces[0].url;
  b.surfaces.push({ url: dupUrl, lean: false });
  const v = collectViolations([a, b]);
  assert.ok(
    v.some((m) => m.includes('owned by more than one shard') || m.includes('does NOT map to it')),
    v.join('\n'),
  );
});

test('collectViolations flags a smuggled surface (assignment mismatch)', () => {
  const count = 2;
  const notMine = SAMPLE_KEYS.find((k) => !ownsSurface(k, 0, count));
  const a = { stack: 'k3d', shardIndex: 0, shardCount: count, surfaces: [{ url: notMine, lean: false }] };
  const b = sliceFor(1, count);
  const v = collectViolations([a, b]);
  assert.ok(v.some((m) => m.includes('does NOT map to it')), v.join('\n'));
});

test('mergeShardSlices rejects a stack mismatch', () => {
  const slices = [sliceFor(0, 1)];
  assert.throws(() => mergeShardSlices(slices, 'doc', 'compose'), /merge target stack/);
});
