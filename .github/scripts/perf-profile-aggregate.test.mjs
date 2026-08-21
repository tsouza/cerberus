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
  const v = classifyPerfProfile({ decideResult: 'success', runHeavy: 'true', shardsResult: 'success' });
  assert.equal(v.ok, true);
  assert.equal(v.shouldMerge, true);
});

test('a heavy run where the matrix rolled up to anything but success fails, whatever the reason', () => {
  // A matrix reports ONE result to its dependents. `failure` is the obvious
  // case; `cancelled` is the one a naive `contains(…, 'failure')` test would
  // let through, and it is exactly how a `timeout-minutes` kill on a leg is
  // recorded.
  for (const result of ['failure', 'cancelled', '']) {
    const v = classifyPerfProfile({ decideResult: 'success', runHeavy: 'true', shardsResult: result });
    assert.equal(v.ok, false, `shardsResult=${result} must not pass`);
    assert.equal(v.shouldMerge, false);
  }
});

test('a heavy run whose matrix never ran (gate drift) fails', () => {
  const v = classifyPerfProfile({ decideResult: 'success', runHeavy: 'true', shardsResult: 'skipped' });
  assert.equal(v.ok, false);
  assert.match(v.message, /never ran/);
});

test('an ordinary PR with the matrix correctly skipped is a green no-op', () => {
  const v = classifyPerfProfile({ decideResult: 'success', runHeavy: 'false', shardsResult: 'skipped' });
  assert.equal(v.ok, true);
  assert.equal(v.shouldMerge, false);
});

test('an ordinary PR whose matrix somehow ran anyway (gate drift) fails', () => {
  for (const result of ['success', 'failure', 'cancelled']) {
    const v = classifyPerfProfile({ decideResult: 'success', runHeavy: 'false', shardsResult: result });
    assert.equal(v.ok, false, `shardsResult=${result} on a non-heavy run must not pass`);
    assert.match(v.message, /drifted/);
  }
});

// tsouza/cerberus#2426: decide-run-heavy is now its own leading job. A hard
// failure there (not the gracefully-handled "source PR did not resolve"
// case, which decide() itself fails safe on — a genuine bug in the script or
// its test) must be caught explicitly, since profile-shard's `if:` reading
// an unset output would otherwise skip the matrix and read identically to an
// ordinary PR's legitimate no-op.
test('decide-run-heavy not succeeding is its own hard failure, regardless of the other two facts', () => {
  for (const decideResult of ['failure', 'cancelled', 'skipped', '']) {
    for (const runHeavy of ['true', 'false']) {
      for (const shardsResult of ['success', 'skipped', 'failure']) {
        const v = classifyPerfProfile({ decideResult, runHeavy, shardsResult });
        assert.equal(v.ok, false, `decideResult=${decideResult} must not pass`);
        assert.equal(v.shouldMerge, false);
        assert.match(v.message, /decide-run-heavy/);
      }
    }
  }
});
