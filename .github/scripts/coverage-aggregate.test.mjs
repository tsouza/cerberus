// coverage-aggregate.test.mjs — node:test guard for the coverage-default /
// coverage-chdb lane roll-up decision (tsouza/cerberus#2634).
//
// This aggregator is the ONLY thing that decides whether the `coverage` job
// (the name branch protection and release.yml's RELEASE_REQUIRED_CHECKS both
// resolve by exact text) proceeds to download and merge the two lane
// profiles or reports a no-op. A version that drifts toward "always ok"
// would let a merge run over a partial or failed lane sweep and publish it
// as if both lanes had actually measured coverage — so these pin both
// directions: every legitimate case must pass, and every drifted/failed one
// must actually fail.

import assert from 'node:assert/strict';
import { test } from 'node:test';

import { classifyCoverage } from './coverage-aggregate.mjs';

test('a heavy run where both lanes succeeded merges', () => {
  const v = classifyCoverage({ planResult: 'success', runHeavy: 'true', defaultResult: 'success', chdbResult: 'success' });
  assert.equal(v.ok, true);
  assert.equal(v.shouldMerge, true);
});

test('a heavy run where either lane did not succeed fails, whatever the reason', () => {
  // `cancelled` is exactly how a `timeout-minutes` kill on a lane is
  // recorded, and is the case a naive `!== 'failure'` check would miss.
  for (const badResult of ['failure', 'cancelled', '']) {
    const bothBad = classifyCoverage({ planResult: 'success', runHeavy: 'true', defaultResult: badResult, chdbResult: badResult });
    assert.equal(bothBad.ok, false, `defaultResult=chdbResult=${badResult} must not pass`);
    assert.equal(bothBad.shouldMerge, false);

    const onlyDefaultBad = classifyCoverage({ planResult: 'success', runHeavy: 'true', defaultResult: badResult, chdbResult: 'success' });
    assert.equal(onlyDefaultBad.ok, false, `defaultResult=${badResult} alone must not pass`);
    assert.equal(onlyDefaultBad.shouldMerge, false);

    const onlyChdbBad = classifyCoverage({ planResult: 'success', runHeavy: 'true', defaultResult: 'success', chdbResult: badResult });
    assert.equal(onlyChdbBad.ok, false, `chdbResult=${badResult} alone must not pass`);
    assert.equal(onlyChdbBad.shouldMerge, false);
  }
});

test('a heavy run whose lane(s) never ran (gate drift) fails', () => {
  const v = classifyCoverage({ planResult: 'success', runHeavy: 'true', defaultResult: 'skipped', chdbResult: 'success' });
  assert.equal(v.ok, false);
  assert.match(v.message, /never ran/);

  const both = classifyCoverage({ planResult: 'success', runHeavy: 'true', defaultResult: 'skipped', chdbResult: 'skipped' });
  assert.equal(both.ok, false);
  assert.match(both.message, /never ran/);
});

test('an ordinary PR with both lanes correctly skipped is a green no-op', () => {
  const v = classifyCoverage({ planResult: 'success', runHeavy: 'false', defaultResult: 'skipped', chdbResult: 'skipped' });
  assert.equal(v.ok, true);
  assert.equal(v.shouldMerge, false);
});

test('an ordinary PR whose lane(s) somehow ran anyway (gate drift) fails', () => {
  for (const defaultResult of ['success', 'failure', 'cancelled']) {
    const v = classifyCoverage({ planResult: 'success', runHeavy: 'false', defaultResult, chdbResult: 'skipped' });
    assert.equal(v.ok, false, `defaultResult=${defaultResult} on a non-heavy run must not pass`);
    assert.match(v.message, /drifted/);
  }
  for (const chdbResult of ['success', 'failure', 'cancelled']) {
    const v = classifyCoverage({ planResult: 'success', runHeavy: 'false', defaultResult: 'skipped', chdbResult });
    assert.equal(v.ok, false, `chdbResult=${chdbResult} on a non-heavy run must not pass`);
    assert.match(v.message, /drifted/);
  }
});

// coverage-plan is its own leading job (mirrors perf-profile.yml's
// decide-run-heavy). A hard failure there (not the gracefully-handled
// "source PR did not resolve" case, which coverage-run-heavy.mjs itself
// fails safe on — a genuine bug in the script or its test) must be caught
// explicitly, since coverage-default/coverage-chdb's `if:` reading an unset
// output would otherwise skip both lanes and read identically to an
// ordinary PR's legitimate no-op.
test('coverage-plan not succeeding is its own hard failure, regardless of the other facts', () => {
  for (const planResult of ['failure', 'cancelled', 'skipped', '']) {
    for (const runHeavy of ['true', 'false']) {
      for (const defaultResult of ['success', 'skipped', 'failure']) {
        for (const chdbResult of ['success', 'skipped', 'failure']) {
          const v = classifyCoverage({ planResult, runHeavy, defaultResult, chdbResult });
          assert.equal(v.ok, false, `planResult=${planResult} must not pass`);
          assert.equal(v.shouldMerge, false);
          assert.match(v.message, /coverage-plan/);
        }
      }
    }
  }
});
