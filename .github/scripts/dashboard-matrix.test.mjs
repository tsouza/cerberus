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
  buildMatrix,
  SMOKE_MODES,
  MODE_MONOLITH,
  MODE_SPLIT,
  SPLIT_ONLY_SPECS,
  CRAWL_STACK_K3D,
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

// ---- emit-time cross-product (buildMatrix) -------------------------------

const SMOKE_SHARDS = SHARDS.filter((s) => s.crawlStack !== CRAWL_STACK_K3D);
const specsOf = (entry) => entry.specs.split(' ').filter(Boolean);
const hasSplitOnly = (entry) => specsOf(entry).some((spec) => SPLIT_ONLY_SPECS.has(spec));

// A smoke shard whose specs are ALL split-only (e.g. shard-split-isolation)
// has no monolith leg — buildMatrix filters it to empty in monolith and emits
// only its split leg. Mixed shards yield both legs.
const isSplitOnlyShard = (s) => s.specs.every((spec) => SPLIT_ONLY_SPECS.has(spec));

test('buildMatrix: with includeSplit, every smoke shard yields a split leg; mixed shards also a monolith leg', () => {
  const include = buildMatrix(false, true); // schedule/dispatch: both modes
  for (const s of SMOKE_SHARDS) {
    assert.ok(
      include.some((e) => e.name === `${s.name}-${MODE_SPLIT}` && e.mode === MODE_SPLIT),
      `expected split entry ${s.name}-split; got ${include.map((e) => e.name).join(', ')}`,
    );
    const hasMono = include.some((e) => e.name === `${s.name}-${MODE_MONOLITH}` && e.mode === MODE_MONOLITH);
    if (isSplitOnlyShard(s)) {
      assert.ok(!hasMono, `split-only shard ${s.name} must NOT yield a monolith leg`);
    } else {
      assert.ok(hasMono, `mixed shard ${s.name} must yield a monolith leg`);
    }
  }
});

test('buildMatrix: split_isolation runs ALONE in its own shard (never co-sharded with head specs)', () => {
  const include = buildMatrix(false, true);
  const owning = include.filter((e) => specsOf(e).includes('split_isolation.spec.ts'));
  assert.equal(owning.length, 1, `split_isolation must be in exactly one emitted shard; got ${owning.length}`);
  assert.deepEqual(
    specsOf(owning[0]),
    ['split_isolation.spec.ts'],
    `split_isolation must run alone (it scales a shared head to zero); shard ${owning[0].name} also has: ${owning[0].specs}`,
  );
});

test('buildMatrix: without includeSplit (PR/push), smoke shards are monolith-only — no split legs', () => {
  const include = buildMatrix(false, false); // PR/push cadence
  assert.equal(
    include.filter((e) => e.mode === MODE_SPLIT).length,
    0,
    `PR/push must emit no split legs; got ${include.filter((e) => e.mode === MODE_SPLIT).map((e) => e.name).join(', ')}`,
  );
  for (const s of SMOKE_SHARDS) {
    const name = `${s.name}-${MODE_MONOLITH}`;
    const hasMono = include.some((e) => e.name === name && e.mode === MODE_MONOLITH);
    if (isSplitOnlyShard(s)) {
      // A split-only shard emits nothing on PR/push (no monolith leg, no split leg).
      assert.ok(!hasMono, `split-only shard ${s.name} must emit nothing on PR/push`);
    } else {
      assert.ok(hasMono, `expected monolith entry ${name}; got ${include.map((e) => e.name).join(', ')}`);
    }
  }
});

test('buildMatrix: split-only specs run on split legs and NEVER on monolith legs', () => {
  const include = buildMatrix(true, true); // crawl + split: split legs must exist to assert against
  const splitOnlyOnSplit = include.filter((e) => e.mode === MODE_SPLIT && hasSplitOnly(e));
  const splitOnlyOnMono = include.filter((e) => e.mode === MODE_MONOLITH && hasSplitOnly(e));
  assert.ok(
    splitOnlyOnSplit.length >= 1,
    `expected at least one split-mode entry carrying a split-only spec; got none. ` +
      `split entries: ${include.filter((e) => e.mode === MODE_SPLIT).map((e) => e.name).join(', ')}`,
  );
  assert.equal(
    splitOnlyOnMono.length,
    0,
    `split-only specs leaked into monolith entries: ${splitOnlyOnMono.map((e) => e.name).join(', ')}`,
  );
});

test('buildMatrix: exactly one emitted entry runs the Go e2e suite (monolith leg)', () => {
  for (const includeCrawl of [false, true]) {
    const include = buildMatrix(includeCrawl);
    const goE2E = include.filter((e) => e.runGoE2E);
    assert.equal(
      goE2E.length,
      1,
      `includeCrawl=${includeCrawl}: expected exactly one runGoE2E entry; got ${goE2E.length}: ` +
        goE2E.map((e) => e.name).join(', '),
    );
    assert.equal(
      goE2E[0].mode,
      MODE_MONOLITH,
      `the runGoE2E entry must be the monolith leg; got ${goE2E[0].name} (mode=${goE2E[0].mode})`,
    );
  }
});

test('buildMatrix: the crawl shard runs once, monolith-only, and only when crawl is included', () => {
  const noCrawl = buildMatrix(false);
  assert.equal(
    noCrawl.filter((e) => e.crawlStack === CRAWL_STACK_K3D).length,
    0,
    'crawl must not be dispatched on PR/push (includeCrawl=false)',
  );
  const withCrawl = buildMatrix(true);
  const crawlEntries = withCrawl.filter((e) => e.crawlStack === CRAWL_STACK_K3D);
  assert.equal(crawlEntries.length, 1, 'crawl must be dispatched exactly once when included');
  assert.equal(crawlEntries[0].mode, MODE_MONOLITH, 'the crawl shard is monolith-only');
});

test('buildMatrix: every emitted entry has a non-empty spec list and a filename-safe name', () => {
  const include = buildMatrix(true, true); // widest matrix (crawl + both modes)
  for (const e of include) {
    assert.ok(specsOf(e).length > 0, `entry ${e.name} would boot a cluster to run nothing`);
    assert.match(e.name, /^[a-z0-9-]+$/, `entry name not filename-safe: ${e.name}`);
  }
});
