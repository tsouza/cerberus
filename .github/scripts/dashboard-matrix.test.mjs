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

test('buildMatrix: with includeSplit, every smoke shard yields both a monolith and a split entry', () => {
  const include = buildMatrix(false, true); // schedule/dispatch: both modes
  for (const s of SMOKE_SHARDS) {
    for (const mode of SMOKE_MODES) {
      const name = `${s.name}-${mode}`;
      assert.ok(
        include.some((e) => e.name === name && e.mode === mode),
        `expected a matrix entry ${name} (mode=${mode}); got ${include.map((e) => e.name).join(', ')}`,
      );
    }
  }
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
    assert.ok(
      include.some((e) => e.name === name && e.mode === MODE_MONOLITH),
      `expected monolith entry ${name}; got ${include.map((e) => e.name).join(', ')}`,
    );
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
