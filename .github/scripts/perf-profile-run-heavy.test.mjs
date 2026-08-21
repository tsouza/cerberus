// perf-profile-run-heavy.test.mjs — node:test guard for the RUN_HEAVY
// decision behind perf-profile.yml's push lane (tsouza/cerberus#2426, a port
// of coverage-run-heavy.test.mjs for tsouza/cerberus#2416).
//
// What it pins:
//   - pull_request is unchanged: release/*-headed runs heavy, everything
//     else does not (delegated to scope-gate.mjs's runsFullLane);
//   - schedule/workflow_dispatch always run heavy;
//   - a push produced by a release/*-headed source PR is the ONE case that
//     newly skips — everything else (an ordinary-PR push, an unresolved
//     source PR, a maintenance-line hotfix push with no PR at all) still
//     runs heavy, so the fail-safe default is "run it", never "skip it".

import assert from 'node:assert/strict';
import test from 'node:test';

import { decide } from './perf-profile-run-heavy.mjs';

test('pull_request is unchanged: release/*-headed runs heavy, everything else does not', () => {
  assert.equal(decide({ eventName: 'pull_request', headRef: 'release/1.14.x' }).runHeavy, true);
  assert.equal(decide({ eventName: 'pull_request', headRef: 'fix/some-bug' }).runHeavy, false);
  assert.equal(decide({ eventName: 'pull_request', headRef: '' }).runHeavy, false);
});

test('schedule and workflow_dispatch always run heavy, sourcePR or not', () => {
  for (const eventName of ['schedule', 'workflow_dispatch']) {
    assert.equal(decide({ eventName }).runHeavy, true, eventName);
    assert.equal(decide({ eventName, sourcePR: { number: 1, headRef: 'release/1.14.x' } }).runHeavy, true, eventName);
  }
});

test('push produced by a release/*-headed source PR is redundant — skips', () => {
  const v = decide({ eventName: 'push', sourcePR: { number: 2400, headRef: 'release/1.14.x' } });
  assert.equal(v.runHeavy, false);
  assert.match(v.reason, /redundant with PR #2400/);
  assert.match(v.reason, /SOURCE-PR CREDIT/);
});

test('push produced by an ordinary (non-release) source PR is the first real run — runs heavy', () => {
  const v = decide({ eventName: 'push', sourcePR: { number: 41, headRef: 'fix/something' } });
  assert.equal(v.runHeavy, true);
  assert.match(v.reason, /first real profile run/);
});

test('push with no resolved source PR (maintenance hotfix, or resolution failure) fails safe to heavy', () => {
  const v = decide({ eventName: 'push', sourcePR: null });
  assert.equal(v.runHeavy, true);
  assert.match(v.reason, /fail-safe default/);
});

test('a source PR match with no head ref (malformed) is treated as not release-headed — runs heavy', () => {
  const v = decide({ eventName: 'push', sourcePR: { number: 7, headRef: null } });
  assert.equal(v.runHeavy, true);
});

test('an unfamiliar event name fails open, same as scope-gate.runsFullLane', () => {
  assert.equal(decide({ eventName: 'some_future_event' }).runHeavy, true);
});
