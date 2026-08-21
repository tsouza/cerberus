// perf-profile-aggregate.test.mjs — node:test guard for the profile-shard
// roll-up decision.
//
// This aggregator is the ONLY thing that decides whether the `profile` job
// (which release.yml's RELEASE_REQUIRED_CHECKS resolves by exact text)
// proceeds to merge the profile-shard matrix's outputs or reports a no-op.
// A version that drifts toward "always ok" would let a merge run over a
// partial or failed shard sweep and publish it as if it were the whole
// corpus — so these pin both directions: every legitimate case must pass,
// and every drifted/failed one must actually fail.

import assert from 'node:assert/strict';
import { test } from 'node:test';

import { classifyPerfProfile } from './perf-profile-aggregate.mjs';

test('a heavy run where every shard succeeded merges', () => {
  const v = classifyPerfProfile({ runHeavy: 'true', shardsResult: 'success' });
  assert.equal(v.ok, true);
  assert.equal(v.shouldMerge, true);
});

test('a heavy run where the matrix rolled up to anything but success fails, whatever the reason', () => {
  // A matrix reports ONE result to its dependents. `failure` is the obvious
  // case; `cancelled` is the one a naive `contains(…, 'failure')` test would
  // let through, and it is exactly how a `timeout-minutes` kill on a leg is
  // recorded.
  for (const result of ['failure', 'cancelled', '']) {
    const v = classifyPerfProfile({ runHeavy: 'true', shardsResult: result });
    assert.equal(v.ok, false, `shardsResult=${result} must not pass`);
    assert.equal(v.shouldMerge, false);
  }
});

test('a heavy run whose matrix never ran (gate drift) fails', () => {
  const v = classifyPerfProfile({ runHeavy: 'true', shardsResult: 'skipped' });
  assert.equal(v.ok, false);
  assert.match(v.message, /never ran/);
});

test('an ordinary PR with the matrix correctly skipped is a green no-op', () => {
  const v = classifyPerfProfile({ runHeavy: 'false', shardsResult: 'skipped' });
  assert.equal(v.ok, true);
  assert.equal(v.shouldMerge, false);
});

test('an ordinary PR whose matrix somehow ran anyway (gate drift) fails', () => {
  for (const result of ['success', 'failure', 'cancelled']) {
    const v = classifyPerfProfile({ runHeavy: 'false', shardsResult: result });
    assert.equal(v.ok, false, `shardsResult=${result} on a non-heavy run must not pass`);
    assert.match(v.message, /drifted/);
  }
});
