// coverage-run-heavy.test.mjs — node:test guard for the RUN_HEAVY decision
// behind coverage.yml's push lane (tsouza/cerberus#2416, part 2 of #2394).
//
// What it pins:
//   - pull_request / merge_group / schedule / workflow_dispatch behave
//     EXACTLY as the old inline GHA expression did (delegated to
//     scope-gate.mjs's runsFullLane, shared with mutation.yml and
//     e2e.yml's compose-smoke-scope);
//   - a push produced by a release/*-headed source PR is the ONE case that
//     newly skips — everything else (an ordinary-PR push, an unresolved
//     source PR, a maintenance-line hotfix push with no PR at all) still
//     runs heavy, so the fail-safe default is "run it", never "skip it".
//   - tsouza/cerberus#3708: a schedule event ALSO skips when told an
//     overlapping push-to-main run already holds or is about to hold
//     cerberus-heavy — the one exception to "schedule is unconditional" the
//     other lib/run-heavy.mjs consumers keep. No overlap (including the
//     null fail-safe default) leaves schedule heavy, unchanged.

import assert from 'node:assert/strict';
import test from 'node:test';

import { decide } from './coverage-run-heavy.mjs';

test('pull_request and merge_group are unchanged: release/*-headed runs heavy, everything else does not', () => {
  assert.equal(decide({ eventName: 'pull_request', headRef: 'release/1.14.x' }).runHeavy, true);
  assert.equal(decide({ eventName: 'pull_request', headRef: 'fix/some-bug' }).runHeavy, false);
  assert.equal(decide({ eventName: 'pull_request', headRef: '' }).runHeavy, false);
  // A merge_group event never actually carries a head_ref in production (GitHub
  // does not populate one for a batch), so this is always false in practice —
  // runsFullLane applies the identical rule regardless, exactly as the old
  // inline GHA expression did.
  assert.equal(decide({ eventName: 'merge_group', headRef: '' }).runHeavy, false);
});

test('schedule and workflow_dispatch run heavy by default, sourcePR or not', () => {
  for (const eventName of ['schedule', 'workflow_dispatch']) {
    assert.equal(decide({ eventName }).runHeavy, true, eventName);
    assert.equal(decide({ eventName, sourcePR: { number: 1, headRef: 'release/1.14.x' } }).runHeavy, true, eventName);
  }
});

test('tsouza/cerberus#3708: schedule yields ONLY when an overlapping push run is reported', () => {
  const overlap = { runId: 4242, status: 'in_progress', headSha: 'deadbeef' };

  const v = decide({ eventName: 'schedule', scheduleOverlap: overlap });
  assert.equal(v.runHeavy, false);
  assert.match(v.reason, /main-push run #4242/);
  assert.match(v.reason, /in_progress/);
  assert.match(v.reason, /cerberus-heavy/);

  // No overlap reported (null, the fail-safe default) — unchanged, heavy.
  assert.equal(decide({ eventName: 'schedule', scheduleOverlap: null }).runHeavy, true);
  assert.equal(decide({ eventName: 'schedule' }).runHeavy, true);

  // The override is schedule-only: a push or workflow_dispatch event must
  // never consult scheduleOverlap, even if a caller passed one by mistake.
  assert.equal(decide({ eventName: 'push', scheduleOverlap: overlap, sourcePR: null }).runHeavy, true);
  assert.equal(decide({ eventName: 'workflow_dispatch', scheduleOverlap: overlap }).runHeavy, true);
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
  assert.match(v.reason, /first real coverage run/);
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
