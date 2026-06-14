// dashboard-matrix.test.mjs — node:test guard for the dashboard (k3d) shard
// partition's coverage invariant.
//
// Runs on the CHEAP gate lane (`node --test .github/scripts/*.test.mjs`) — no
// setup-node, no deps, no k3d cluster — so a dropped/double-assigned spec
// fails on a much cheaper check than the heavy dashboard lane itself, and on
// every PR (the dashboard lane itself never runs on pull_request).
//
// Guards four things:
//   1. the live tree is a clean cover (the real invariant);
//   2. the UNASSIGNED detector actually fires (so it can't silently rot into
//      a no-op);
//   3. the double-assigned detector actually fires;
//   4. exactly one shard runs the Go e2e suite.

import { test } from 'node:test';
import assert from 'node:assert/strict';

import {
  discover,
  collectViolations,
  SHARDS,
  CRAWL_SUB_SHARDS,
  CRAWL_TRIO,
} from './dashboard-matrix.mjs';

test('live tree: SHARDS ∪ EXCLUDED is a total, disjoint cover (no violations)', () => {
  const violations = collectViolations(discover());
  assert.deepEqual(violations, [], `unexpected coverage violations:\n${violations.join('\n')}`);
});

test('an unlisted discovered spec is flagged UNASSIGNED (the silent-gap guard)', () => {
  const synthetic = [...discover(), 'iterate-brand-new.spec.ts'];
  const violations = collectViolations(synthetic);
  assert.ok(
    violations.some((v) => v.includes('UNASSIGNED') && v.includes('iterate-brand-new.spec.ts')),
    `expected an UNASSIGNED violation for the synthetic spec; got:\n${violations.join('\n')}`,
  );
});

test('a doubly-counted discovered spec surfaces a dup-discovery flag', () => {
  const dup = discover();
  const violations = collectViolations([...dup, dup[0]]);
  assert.ok(
    violations.some((v) => v.includes('duplicate paths')),
    `expected a duplicate-discovery violation; got:\n${violations.join('\n')}`,
  );
});

test('exactly one shard runs the Go e2e suite', () => {
  const goE2E = SHARDS.filter((s) => s.runGoE2E);
  assert.equal(
    goE2E.length,
    1,
    `expected exactly one runGoE2E shard; got ${goE2E.length}: ${goE2E.map((s) => s.name).join(', ')}`,
  );
});

test('crawl trio is replicated across exactly CRAWL_SUB_SHARDS crawl sub-shards, each a distinct contiguous index', () => {
  const crawl = SHARDS.filter((s) => s.crawlStack === 'k3d');
  assert.equal(crawl.length, CRAWL_SUB_SHARDS, 'one crawl shard per sub-shard');
  const indices = crawl.map((s) => Number(s.crawlShardIndex)).sort((a, b) => a - b);
  assert.deepEqual(
    indices,
    Array.from({ length: CRAWL_SUB_SHARDS }, (_, i) => i),
    'crawl sub-shard indices are 0..CRAWL_SUB_SHARDS-1',
  );
  for (const s of crawl) {
    assert.equal(Number(s.crawlShardCount), CRAWL_SUB_SHARDS, `${s.name} count`);
    assert.deepEqual(s.specs, CRAWL_TRIO, `${s.name} runs the whole crawl trio`);
    assert.match(s.name, /^shard-crawl-\d+$/, `${s.name} naming`);
  }
});

test('crawl specs live exclusively on crawl sub-shards (replication never leaks onto a smoke shard)', () => {
  // A crawl spec on a non-crawl shard would run with CRAWL_STACK empty →
  // crawl/** ignored → the spec silently never runs there. The replication
  // exception in collectViolations explicitly guards against this; assert the
  // live partition honours it.
  for (const s of SHARDS) {
    for (const spec of s.specs) {
      if (!CRAWL_TRIO.includes(spec)) continue;
      assert.equal(
        s.crawlStack,
        'k3d',
        `crawl spec ${spec} found on non-crawl shard ${s.name}`,
      );
    }
  }
});
