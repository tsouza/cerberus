// compose-smoke-matrix.test.mjs — node:test guard for the compose-smoke
// shard partition's coverage invariant.
//
// Runs on the CHEAP lint/check lane (`node --test .github/scripts/*.test.mjs`)
// — no setup-node, no deps, no compose stack — so a dropped/double-assigned
// spec fails on a much cheaper required check than compose-smoke itself, and
// on every PR (including docs-only PRs that short-circuit compose-smoke).
//
// Guards three things:
//   1. the live tree is a clean cover (the real invariant);
//   2. the UNASSIGNED detector actually fires (so it can't silently rot into
//      a no-op);
//   3. the double-assigned detector actually fires.

import { test } from 'node:test';
import assert from 'node:assert/strict';

import {
  discover,
  collectViolations,
  shardSweepDepth,
  shardTimeoutMinutes,
} from './compose-smoke-matrix.mjs';
import {
  CRAWL_SHARD_TIMEOUT_FULL_MIN,
  CRAWL_SHARD_TIMEOUT_LEAN_MIN,
} from './lib/crawl-budget.mjs';

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

test('a doubly-counted discovered spec surfaces no false UNASSIGNED but a dup-discovery flag', () => {
  // Discovery returning a path twice must be caught (defends against a glob
  // returning a path twice), without spuriously failing the cover checks.
  const dup = discover();
  const violations = collectViolations([...dup, dup[0]]);
  assert.ok(
    violations.some((v) => v.includes('duplicate paths')),
    `expected a duplicate-discovery violation; got:\n${violations.join('\n')}`,
  );
});

// The crawl shard is the one the de-gate splits out, and the one that writes
// the compose surface inventory.
const CRAWL_SHARD = 'shard-crawl';
const OTHER_SHARD = 'shard-kiosk';

test('an inventory-regen dispatch sweeps the crawl shard full AND pays for it', () => {
  // The regression this pins cost a whole regen cycle to find, and it was
  // invisible as a failure: the shard's SWEEP_DEPTH expression knew that a
  // compose-inventory dispatch must sweep full (crawl.spec.ts refuses to write
  // the inventory at lean depth), while the timeout came from `isSchedule`
  // alone and stayed lean. GitHub cancelled the job at 29m27s against the lean
  // 29-minute ceiling, before the spec's own 75-minute budget could report —
  // so the regen path could not complete, and reported only a cancellation.
  // Depth and ceiling now come from one function; these assert they agree.
  for (const update of ['compose', 'both']) {
    const opts = { isSchedule: false, regeneratesComposeInventory: true };
    assert.equal(
      shardSweepDepth(CRAWL_SHARD, opts),
      'full',
      `${update} regen must sweep the crawl shard full`,
    );
    assert.equal(
      shardTimeoutMinutes(CRAWL_SHARD, opts),
      CRAWL_SHARD_TIMEOUT_FULL_MIN,
      `${update} regen must give the crawl shard the FULL ceiling`,
    );
  }
});

test('a regen dispatch does not make the required shards pay a full sweep', () => {
  const opts = { isSchedule: false, regeneratesComposeInventory: true };
  assert.equal(shardSweepDepth(OTHER_SHARD, opts), 'lean');
});

test('ordinary PR/push and nightly depths are unchanged', () => {
  const pr = { isSchedule: false, regeneratesComposeInventory: false };
  assert.equal(shardSweepDepth(CRAWL_SHARD, pr), 'lean');
  assert.equal(shardTimeoutMinutes(CRAWL_SHARD, pr), CRAWL_SHARD_TIMEOUT_LEAN_MIN);
  assert.equal(shardSweepDepth(OTHER_SHARD, pr), 'lean');

  const nightly = { isSchedule: true, regeneratesComposeInventory: false };
  assert.equal(shardSweepDepth(CRAWL_SHARD, nightly), 'full');
  assert.equal(shardTimeoutMinutes(CRAWL_SHARD, nightly), CRAWL_SHARD_TIMEOUT_FULL_MIN);
  assert.equal(shardSweepDepth(OTHER_SHARD, nightly), 'full');
});

test('every shard ceiling outlives the spec budget at its own depth', () => {
  // The invariant behind the bug: the SPEC must time out first, so a crawl
  // failure reports a verdict instead of a cancellation. Asserting it across
  // both depths is what makes a future depth/ceiling split fail here rather
  // than 29 minutes into a regen.
  for (const opts of [
    { isSchedule: false, regeneratesComposeInventory: false },
    { isSchedule: false, regeneratesComposeInventory: true },
    { isSchedule: true, regeneratesComposeInventory: false },
  ]) {
    const depth = shardSweepDepth(CRAWL_SHARD, opts);
    const ceiling = shardTimeoutMinutes(CRAWL_SHARD, opts);
    const expected =
      depth === 'full' ? CRAWL_SHARD_TIMEOUT_FULL_MIN : CRAWL_SHARD_TIMEOUT_LEAN_MIN;
    assert.equal(
      ceiling,
      expected,
      `crawl ceiling must follow its own sweep depth (${depth})`,
    );
  }
});
