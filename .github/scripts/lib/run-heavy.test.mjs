// lib/run-heavy.test.mjs — node:test guard for createRunHeavyDecider(), the
// shared RUN_HEAVY scaffold behind chdb-run-heavy.mjs, coverage-run-heavy.mjs,
// property-run-heavy.mjs and perf-profile-run-heavy.mjs (finding #5: those
// four wrapper .test.mjs files each pin decide() through their OWN module,
// which is still the primary coverage for the decision itself — this file
// pins the scaffold's parameterization directly: that each lane's phrases
// actually land in the right place, and that ordinaryEventLabel's default
// and override both work).

import assert from 'node:assert/strict';
import test from 'node:test';

import { createRunHeavyDecider } from './run-heavy.mjs';

function testLane(overrides = {}) {
  return createRunHeavyDecider({
    scriptName: 'test-lane-run-heavy',
    heavyLanesPhrase: 'the heavy test-lane lanes',
    ordinaryPhrase: 'ordinary-phrase marker',
    redundantPhrase: 'the heavy test-lane lanes and posted their check-runs',
    firstRealPhrase: 'first real test-lane run for this tree',
    ...overrides,
  });
}

test('decide: a non-push heavy event names the lane via heavyLanesPhrase', () => {
  const { decide } = testLane();
  const v = decide({ eventName: 'schedule' });
  assert.equal(v.runHeavy, true);
  assert.match(v.reason, /runs the heavy test-lane lanes/);
});

test('decide: ordinaryEventLabel defaults to pull_request/merge_group', () => {
  const { decide } = testLane();
  const v = decide({ eventName: 'pull_request', headRef: 'fix/x' });
  assert.equal(v.runHeavy, false);
  assert.match(v.reason, /^ordinary pull_request\/merge_group — ordinary-phrase marker$/);
});

test('decide: ordinaryEventLabel can be overridden for a lane with no merge_group trigger', () => {
  const { decide } = testLane({ ordinaryEventLabel: 'pull_request' });
  const v = decide({ eventName: 'pull_request', headRef: 'fix/x' });
  assert.equal(v.runHeavy, false);
  assert.match(v.reason, /^ordinary pull_request — ordinary-phrase marker$/);
  assert.doesNotMatch(v.reason, /merge_group/);
});

test('decide: a redundant push names the source PR and redundantPhrase', () => {
  const { decide } = testLane();
  const v = decide({ eventName: 'push', sourcePR: { number: 99, headRef: 'release/1.0.x' } });
  assert.equal(v.runHeavy, false);
  assert.match(v.reason, /redundant with PR #99 \(release\/1\.0\.x\)/);
  assert.match(v.reason, /the heavy test-lane lanes and posted their check-runs/);
  assert.match(v.reason, /SOURCE-PR CREDIT/);
});

test('decide: an ordinary-PR push names firstRealPhrase', () => {
  const { decide } = testLane();
  const v = decide({ eventName: 'push', sourcePR: { number: 5, headRef: 'fix/x' } });
  assert.equal(v.runHeavy, true);
  assert.match(v.reason, /first real test-lane run for this tree/);
});

test('decide: an unresolved push source PR fails safe to heavy, independent of any lane phrase', () => {
  const { decide } = testLane();
  const v = decide({ eventName: 'push', sourcePR: null });
  assert.equal(v.runHeavy, true);
  assert.match(v.reason, /fail-safe default/);
});

test('two separately-constructed lanes do not leak state or phrasing into each other', () => {
  const laneA = testLane({ heavyLanesPhrase: 'lane A heavy lanes' });
  const laneB = testLane({ heavyLanesPhrase: 'lane B heavy lanes' });
  assert.match(laneA.decide({ eventName: 'schedule' }).reason, /lane A heavy lanes/);
  assert.match(laneB.decide({ eventName: 'schedule' }).reason, /lane B heavy lanes/);
});

test('resolvePushSourcePR fails safe to null when the env is incomplete, without calling the network', () => {
  const { resolvePushSourcePR } = testLane();
  return resolvePushSourcePR({ repo: '', sha: 'abc', token: '' }).then((result) => {
    assert.equal(result, null);
  });
});
