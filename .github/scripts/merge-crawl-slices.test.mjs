import assert from 'node:assert/strict';
import { test } from 'node:test';

import { mergeSlices, owner } from './merge-crawl-slices.mjs';

const stack = 'compose';
const shardCount = 2;
const otherOwnerURL = (url) => {
  const wanted = 1 - owner(url, shardCount);
  for (let suffix = 0; ; suffix++) {
    const candidate = `/surface-${suffix}`;
    if (owner(candidate, shardCount) === wanted) return candidate;
  }
};
const sameOwnerURL = (url) => {
  const wanted = owner(url, shardCount);
  for (let suffix = 0; ; suffix++) {
    const candidate = `/same-owner-${suffix}`;
    if (owner(candidate, shardCount) === wanted) return candidate;
  }
};
const shard = (index, visited, discovered = visited) => ({
  stack,
  depth: 'full',
  shard: { index, count: 2 },
  visited,
  discovered,
});

test('owner keeps URL-encoded interaction states with their source route', () => {
  const source = '/a/grafana-exploretraces-app/explore';
  const interactionURL = `${source}?actionView=traceList&var-primarySignal=kind=server`;
  const inPlaceState = `${interactionURL}#tabs[Favorites|All]=All`;
  assert.equal(owner(interactionURL, shardCount), owner(source, shardCount));
  assert.equal(owner(inPlaceState, shardCount), owner(source, shardCount));
});

test('mergeSlices accepts a total, owned shard cover', () => {
  const one = '/one';
  const two = `${otherOwnerURL(one)}#state=x`;
  const inventory = { stack, surfaces: [{ url: one, lean: true }, { url: two, lean: false }] };
  const slices = [shard(owner(one, shardCount), [one]), shard(owner(two, shardCount), [two])];
  assert.deepEqual(
    mergeSlices(slices, { stack, depth: 'full', inventory, exclusions: { exclusions: [] } }).violations,
    [],
  );
});

test('mergeSlices rejects missing owners and coverage growth', () => {
  assert.throws(
    () => mergeSlices([shard(0, [])], { stack, depth: 'full', inventory: { stack, surfaces: [] }, exclusions: { exclusions: [] } }),
    /cover 1 of 2/,
  );
  const first = '/one';
  const second = `${otherOwnerURL(first)}#state=x`;
  const inventory = { stack, surfaces: [{ url: first, lean: true }, { url: second, lean: false }] };
  const newURL = sameOwnerURL(first);
  const slices = [shard(owner(first, shardCount), [first, newURL]), shard(owner(second, shardCount), [second])];
  assert.match(
    mergeSlices(slices, { stack, depth: 'full', inventory, exclusions: { exclusions: [] } }).violations.join('\n'),
    /NEW surface/,
  );
});

test('mergeSlices ratchets audited interaction states, not only discovered URLs', () => {
  const first = '/one';
  const second = `${otherOwnerURL(first)}#state=x`;
  const inventory = { stack, surfaces: [{ url: first, lean: true }, { url: second, lean: false }] };
  const slices = [
    shard(owner(first, shardCount), [first, '/one#control=value'], [first]),
    shard(owner(second, shardCount), [second]),
  ];
  const result = mergeSlices(slices, {
    stack,
    depth: 'full',
    inventory,
    exclusions: { exclusions: [] },
  });
  assert.match(result.violations.join('\n'), /NEW surface "\/one#control=value" visited but not pinned/);
});

test('mergeSlices preserves every exclusion violation diffInventory reports', () => {
  const first = '/one';
  const second = `${otherOwnerURL(first)}#state=x`;
  const inventory = { stack, surfaces: [{ url: first, lean: true }, { url: second, lean: false }] };
  const slices = [shard(owner(first, shardCount), [first]), shard(owner(second, shardCount), [second])];
  const result = mergeSlices(slices, {
    stack,
    depth: 'full',
    inventory,
    exclusions: {
      exclusions: [
        { url: first, rationale: '' },
        { url: first, rationale: 'duplicate stale exclusion' },
      ],
    },
  });
  assert.match(result.violations.join('\n'), /has no rationale/);
  assert.match(result.violations.join('\n'), /is listed twice/);
  assert.match(result.violations.join('\n'), /is in BOTH the inventory and the exclusions file/);
});

test('mergeSlices reports cross-owner discovery as inventory growth', () => {
  const first = '/one';
  const second = `${otherOwnerURL(first)}#state=x`;
  const crossOwner = otherOwnerURL(first);
  const inventory = { stack, surfaces: [{ url: first, lean: true }, { url: second, lean: false }] };
  const result = mergeSlices(
    [
      shard(owner(first, shardCount), [first], [first, crossOwner]),
      shard(owner(second, shardCount), [second]),
    ],
    { stack, depth: 'full', inventory, exclusions: { exclusions: [] } },
  );
  assert.match(result.violations.join('\n'), new RegExp(`NEW surface ${JSON.stringify(crossOwner)}`));
});

test('mergeSlices validates inventory and exclusion files like crawl/lib.ts', () => {
  const slices = [shard(0, []), shard(1, [])];
  assert.throws(
    () => mergeSlices(slices, { stack, depth: 'full', inventory: { stack }, exclusions: { exclusions: [] } }),
    /has no surfaces\[\] array/,
  );
  assert.throws(
    () => mergeSlices(slices, { stack, depth: 'full', inventory: { stack: 'k3d', surfaces: [] }, exclusions: { exclusions: [] } }),
    /pins stack "k3d" but the active config is "compose"/,
  );
  assert.throws(
    () => mergeSlices(slices, { stack, depth: 'full', inventory: { stack, surfaces: [] }, exclusions: {} }),
    /has no exclusions\[\] array/,
  );
});
