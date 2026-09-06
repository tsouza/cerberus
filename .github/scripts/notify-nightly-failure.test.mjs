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
  EXPERIMENTAL_LANES,
} from './notify-nightly-failure.mjs';

const allGreen = {
  'compose-smoke': 'success',
  'crawl-terminal': 'success',
  dashboard: 'success',
  'dashboard-crawl-terminal': 'success',
  'startup-bench': 'success',
  chaos: 'success',
  'bwc-minio': 'success',
  datashard: 'success',
  'datashard-replica-affinity': 'success',
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

// EXPERIMENTAL lanes (epic #3074's off-by-default Distributed-table path):
// advisory, reported, never decisive. Both directions pinned — a lane that
// only ever passed would be satisfied by dropping the lane from the map.
test('an EXPERIMENTAL lane failing alone is advisory: the night is still clean, and the lane is still named', () => {
  const v = classifyNightlyHealth({ ...allGreen, datashard: 'failure' }, { experimentalLanes: EXPERIMENTAL_LANES });
  assert.equal(v.ok, true);
  assert.deepEqual(v.failed, []);
  assert.deepEqual(v.experimentalFailed, ['datashard: failure']);
});

test('a supported-lane failure next to an experimental one is still caught, each named under its own list', () => {
  const v = classifyNightlyHealth(
    { ...allGreen, dashboard: 'failure', 'datashard-replica-affinity': 'cancelled' },
    { experimentalLanes: EXPERIMENTAL_LANES },
  );
  assert.equal(v.ok, false);
  assert.deepEqual(v.failed, ['dashboard: failure']);
  assert.deepEqual(v.experimentalFailed, ['datashard-replica-affinity: cancelled']);
});

test('without an experimental set every lane is decisive — the pre-existing contract is the default', () => {
  const v = classifyNightlyHealth({ ...allGreen, datashard: 'failure' });
  assert.equal(v.ok, false);
  assert.deepEqual(v.failed, ['datashard: failure']);
  assert.deepEqual(v.experimentalFailed, []);
});

test('buildFailureBody lists experimental lanes under their own advisory heading, apart from the decisive list', () => {
  const body = buildFailureBody({ failed: ['dashboard: failure'], experimentalFailed: ['datashard: failure'], runUrl: 'u', runId: '1' });
  assert.match(body, /EXPERIMENTAL lanes \(advisory only/);
  assert.match(body, /- `dashboard: failure`/);
  assert.match(body, /- `datashard: failure`/);
  assert.ok(body.indexOf('- `dashboard: failure`') < body.indexOf('EXPERIMENTAL lanes'), 'decisive list comes first');
});

test('a body with no experimental non-successes carries no advisory section at all', () => {
  const body = buildFailureBody({ failed: ['dashboard: failure'], runUrl: 'u', runId: '1' });
  assert.doesNotMatch(body, /EXPERIMENTAL/);
});

test('buildRecoveryBody still surfaces experimental non-successes on an otherwise clean night', () => {
  const body = buildRecoveryBody({ experimentalFailed: ['datashard: failure'], runUrl: 'u', runId: '1' });
  assert.match(body, /reached a clean pass/);
  assert.match(body, /EXPERIMENTAL lanes \(advisory only/);
  assert.match(body, /- `datashard: failure`/);
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
    /nightly-health-notify:\n    name: nightly-health-notify\n    needs:\n {6}\[compose-smoke, crawl-terminal, dashboard, dashboard-crawl-terminal, startup-bench, chaos, bwc-minio, datashard, datashard-replica-affinity\]\n {4}if: always\(\) && github\.event_name == 'schedule'/,
  );
});

test('nightly-health-notify has issues: write and invokes the script', () => {
  assert.match(e2eWorkflow, /nightly-health-notify[\s\S]{0,600}issues: write/);
  assert.match(e2eWorkflow, /run: node \.github\/scripts\/notify-nightly-failure\.mjs/);
});

test('the self-test runs on the PR path via ci.yml', () => {
  assert.match(ciWorkflow, /node --test \.github\/scripts\/notify-nightly-failure\.test\.mjs/);
});

test('every EXPERIMENTAL lane is a real terminal job the notify step still needs (advisory != dropped)', () => {
  const needs = e2eWorkflow.match(/nightly-health-notify:\n    name: nightly-health-notify\n    needs:\n {6}\[([^\]]+)\]/);
  assert.ok(needs, 'nightly-health-notify needs: list found');
  const jobs = needs[1].split(',').map((s) => s.trim());
  for (const lane of EXPERIMENTAL_LANES) {
    assert.ok(jobs.includes(lane), `experimental lane "${lane}" is still in nightly-health-notify's needs: — it must keep running and reporting, only its verdict is advisory`);
    assert.ok(allGreen[lane] !== undefined, `experimental lane "${lane}" is in the job map`);
  }
});
