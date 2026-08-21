// notify-nightly-failure.test.mjs — node:test guard for the nightly-health
// tracking-issue roll-up (#1861 acceptance criterion #2: a future
// cancellation, or any other non-success nightly, must be VISIBLE, not just
// a red job nobody watches).
//
// Pins both directions: the benign cells (clean night, no stale issue left
// open) must stay quiet, and every not-clean cell must actually notify —
// a test file that only asserted the happy path would be satisfied by
// `() => ({ ok: true })`.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

import {
  classifyNightlyHealth,
  findTrackingIssue,
  decideNotifyAction,
  buildFailureBody,
  buildRecoveryBody,
  NIGHTLY_TRACKING_TITLE,
} from './notify-nightly-failure.mjs';

const allGreen = {
  'compose-smoke': 'success',
  'crawl-terminal': 'success',
  dashboard: 'success',
  'dashboard-crawl-terminal': 'success',
  'startup-bench': 'success',
  chaos: 'success',
  'bwc-minio': 'success',
};

test('every job success is a clean night', () => {
  const v = classifyNightlyHealth(allGreen);
  assert.equal(v.ok, true);
  assert.deepEqual(v.failed, []);
});

test('a real failure is caught', () => {
  const v = classifyNightlyHealth({ ...allGreen, dashboard: 'failure' });
  assert.equal(v.ok, false);
  assert.deepEqual(v.failed, ['dashboard: failure']);
});

test('a cancellation is caught exactly like a failure — the #1861 regression shape', () => {
  const v = classifyNightlyHealth({ ...allGreen, 'crawl-terminal': 'cancelled' });
  assert.equal(v.ok, false);
  assert.deepEqual(v.failed, ['crawl-terminal: cancelled']);
});

test('an unexpected skip on a schedule run is caught, not laundered as benign', () => {
  const v = classifyNightlyHealth({ ...allGreen, chaos: 'skipped' });
  assert.equal(v.ok, false);
  assert.deepEqual(v.failed, ['chaos: skipped']);
});

test('a missing/empty result reads as a failure, not a silent pass', () => {
  const v = classifyNightlyHealth({ ...allGreen, 'bwc-minio': '' });
  assert.equal(v.ok, false);
  assert.match(v.failed[0], /bwc-minio: \(missing\)/);
});

test('multiple simultaneous non-successes are all named', () => {
  const v = classifyNightlyHealth({ ...allGreen, dashboard: 'failure', chaos: 'cancelled' });
  assert.equal(v.ok, false);
  assert.equal(v.failed.length, 2);
});

test('findTrackingIssue matches the exact stable title only', () => {
  const issues = [
    { number: 42, title: 'some unrelated issue' },
    { number: 99, title: NIGHTLY_TRACKING_TITLE },
  ];
  assert.equal(findTrackingIssue(issues, NIGHTLY_TRACKING_TITLE), 99);
});

test('findTrackingIssue returns null when nothing matches', () => {
  assert.equal(findTrackingIssue([{ number: 1, title: 'unrelated' }], NIGHTLY_TRACKING_TITLE), null);
  assert.equal(findTrackingIssue([], NIGHTLY_TRACKING_TITLE), null);
  assert.equal(findTrackingIssue(undefined, NIGHTLY_TRACKING_TITLE), null);
});

test('decideNotifyAction: not-ok + no existing issue -> create', () => {
  const d = decideNotifyAction({ ok: false, existingIssueNumber: null });
  assert.deepEqual(d, { action: 'create' });
});

test('decideNotifyAction: not-ok + an existing issue -> comment on it, never a duplicate', () => {
  const d = decideNotifyAction({ ok: false, existingIssueNumber: 99 });
  assert.deepEqual(d, { action: 'comment', number: 99 });
});

test('decideNotifyAction: ok + an existing (now-stale) issue -> close it', () => {
  const d = decideNotifyAction({ ok: true, existingIssueNumber: 99 });
  assert.deepEqual(d, { action: 'close', number: 99 });
});

test('decideNotifyAction: ok + nothing open -> noop', () => {
  const d = decideNotifyAction({ ok: true, existingIssueNumber: null });
  assert.deepEqual(d, { action: 'noop' });
});

test('decideNotifyAction: existingIssueNumber 0 would be falsy but is never a valid issue number', () => {
  // gh issue numbers are 1-based; this only pins that the check is an
  // explicit null/undefined test, not a truthiness test that would treat a
  // (hypothetical, never-real) 0 as "no issue".
  const d = decideNotifyAction({ ok: false, existingIssueNumber: 0 });
  assert.deepEqual(d, { action: 'comment', number: 0 });
});

test('buildFailureBody names every failed job and the run', () => {
  const body = buildFailureBody({ failed: ['dashboard: failure', 'chaos: cancelled'], runUrl: 'https://x/run/1', runId: '1' });
  assert.match(body, /dashboard: failure/);
  assert.match(body, /chaos: cancelled/);
  assert.match(body, /https:\/\/x\/run\/1/);
  assert.match(body, /#1861/);
});

test('buildRecoveryBody names the run and says it closed automatically', () => {
  const body = buildRecoveryBody({ runUrl: 'https://x/run/2', runId: '2' });
  assert.match(body, /https:\/\/x\/run\/2/);
  assert.match(body, /clean pass/);
});

// Wiring pins, mirroring crawl-frontier-workflow.test.mjs's regex-over-source
// approach: a job/needs edit that silently drops a leg or widens the trigger
// off `schedule` compiles fine and passes every unit test above, so the only
// thing that catches it is pinning the source text directly.

const e2eWorkflow = readFileSync(resolve('.github/workflows/e2e.yml'), 'utf8');
const ciWorkflow = readFileSync(resolve('.github/workflows/ci.yml'), 'utf8');

test('nightly-health-notify needs every terminal job and runs on schedule only', () => {
  assert.match(
    e2eWorkflow,
    /nightly-health-notify:\n    name: nightly-health-notify\n    needs:\n {6}\[compose-smoke, crawl-terminal, dashboard, dashboard-crawl-terminal, startup-bench, chaos, bwc-minio\]\n {4}if: always\(\) && github\.event_name == 'schedule'/,
  );
});

test('nightly-health-notify has issues: write and invokes the script', () => {
  assert.match(e2eWorkflow, /nightly-health-notify[\s\S]{0,400}issues: write/);
  assert.match(e2eWorkflow, /run: node \.github\/scripts\/notify-nightly-failure\.mjs/);
});

test('the self-test runs on the PR path via ci.yml', () => {
  assert.match(ciWorkflow, /node --test \.github\/scripts\/notify-nightly-failure\.test\.mjs/);
});
