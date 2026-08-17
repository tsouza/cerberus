// perf-guards-aggregate.test.mjs — node:test guard for the perf-guards shard
// roll-up.
//
// This aggregator is the ONLY thing that posts the `perf-guards` status check
// once the lane is a matrix, and two systems resolve that name by exact text:
// branch protection's required-contexts set and release.yml's
// RELEASE_REQUIRED_CHECKS preflight. An aggregator that drifts toward "always
// ok" would report green over a corpus nothing profiled, while every visible
// signal — the job ran, the check is green, the required context resolved —
// looks identical to a real pass.
//
// So these pin BOTH directions. The benign cases must pass, and every
// not-a-pass case must actually fail; a test file that only asserted the happy
// path would be satisfied by `() => ({ ok: true })`.

import { test } from 'node:test';
import assert from 'node:assert/strict';

import { classifyPerfGuards } from './perf-guards-aggregate.mjs';

const ran = (shardsResult) =>
  classifyPerfGuards({
    changesResult: 'success',
    docsOnly: 'false',
    shardsResult,
    shardCount: '8',
  });

test('every shard green passes', () => {
  const v = ran('success');
  assert.equal(v.ok, true);
  assert.match(v.message, /8 shard\(s\)/);
});

test('a rolled-up matrix that is not success fails, whatever the reason', () => {
  // A matrix reports ONE result to its dependents. `failure` is the obvious
  // case; `cancelled` is the one a `contains(…, 'failure')` test would let
  // through, and it is exactly how a `timeout-minutes` kill is recorded.
  for (const result of ['failure', 'cancelled', '']) {
    const v = ran(result);
    assert.equal(v.ok, false, `shardsResult=${result} must not pass`);
    assert.match(v.message, /did not succeed/);
  }
});

test('a docs-only change short-circuits green', () => {
  const v = classifyPerfGuards({
    changesResult: 'success',
    docsOnly: 'true',
    shardsResult: 'skipped',
    shardCount: '8',
  });
  assert.equal(v.ok, true);
  assert.match(v.message, /docs-only/);
});

test('an ordinary (non-release) PR short-circuits green via run_heavy=false (#2230)', () => {
  const v = classifyPerfGuards({
    changesResult: 'success',
    docsOnly: 'false',
    runHeavy: 'false',
    shardsResult: 'skipped',
    shardCount: '8',
  });
  assert.equal(v.ok, true);
  assert.match(v.message, /release-gate/);
});

test('a heavy event (run_heavy=true) that skips is still a failure, not a pass', () => {
  const v = classifyPerfGuards({
    changesResult: 'success',
    docsOnly: 'false',
    runHeavy: 'true',
    shardsResult: 'skipped',
    shardCount: '8',
  });
  assert.equal(v.ok, false);
  assert.match(v.message, /should have run/);
});

test('a skipped matrix is NOT green when the changes job failed to decide', () => {
  // The hollow green this aggregate exists to prevent: a crashed `changes` job
  // also skips the matrix, and reading the skip alone cannot tell the two
  // apart.
  for (const changesResult of ['failure', 'cancelled', 'skipped']) {
    const v = classifyPerfGuards({
      changesResult,
      docsOnly: '',
      shardsResult: 'skipped',
      shardCount: '8',
    });
    assert.equal(v.ok, false, `changesResult=${changesResult} must not pass`);
    assert.match(v.message, /never decided/);
  }
});

test('a skipped matrix is NOT green on a code change', () => {
  const v = classifyPerfGuards({
    changesResult: 'success',
    docsOnly: 'false',
    shardsResult: 'skipped',
    shardCount: '8',
  });
  assert.equal(v.ok, false);
  assert.match(v.message, /should have run/);
});

test('a successful matrix does not launder a failed changes job', () => {
  const v = classifyPerfGuards({
    changesResult: 'failure',
    docsOnly: 'false',
    shardsResult: 'success',
    shardCount: '8',
  });
  assert.equal(v.ok, false);
  assert.match(v.message, /changes.*did not succeed/);
});

test('the message names the child check a reader has to open', () => {
  assert.match(ran('failure').message, /perf-guards-shard \(n\)/);
});
