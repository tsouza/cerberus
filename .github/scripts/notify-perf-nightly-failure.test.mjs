// notify-perf-nightly-failure.test.mjs — node:test guard for #2370's
// perf-nightly tracking-issue roll-up, the second consumer of
// lib/nightly-health-notify.mjs (the #1861 mechanism e2e.yml's own
// notify-nightly-failure.mjs established first).
//
// The pure roll-up/dedup/decision functions are already exhaustively
// pinned by notify-nightly-failure.test.mjs against the SAME
// lib/nightly-health-notify.mjs implementation — re-asserting them here
// would just be testing the same function object twice. This file covers
// what's actually specific to this consumer: its own constants, its
// single-job result shape, the generalised body builders taking a
// `laneLabel`, and the workflow/ci wiring.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

import { classifyNightlyHealth } from './lib/nightly-health-notify.mjs';
import { PERF_NIGHTLY_TRACKING_LABELS, PERF_NIGHTLY_TRACKING_TITLE } from './notify-perf-nightly-failure.mjs';
import { buildFailureBody, buildRecoveryBody } from './lib/nightly-health-notify.mjs';

test('perf-nightly single-job success is a clean night', () => {
  const v = classifyNightlyHealth({ 'perf-nightly': 'success' });
  assert.equal(v.ok, true);
  assert.deepEqual(v.failed, []);
});

test('perf-nightly single-job failure is caught', () => {
  const v = classifyNightlyHealth({ 'perf-nightly': 'failure' });
  assert.equal(v.ok, false);
  assert.deepEqual(v.failed, ['perf-nightly: failure']);
});

test('a missing/empty result reads as a failure, not a silent pass', () => {
  const v = classifyNightlyHealth({ 'perf-nightly': '' });
  assert.equal(v.ok, false);
  assert.match(v.failed[0], /perf-nightly: \(missing\)/);
});

test('the tracking title is distinct from e2e\'s own', () => {
  assert.equal(PERF_NIGHTLY_TRACKING_TITLE, 'nightly perf-nightly run did not reach a clean pass');
  assert.notEqual(PERF_NIGHTLY_TRACKING_TITLE, 'nightly e2e run did not reach a clean pass');
});

test('the tracking labels match the shared automated + area/ci pair', () => {
  assert.deepEqual(PERF_NIGHTLY_TRACKING_LABELS, ['automated', 'area/ci']);
});

test('buildFailureBody names the perf-nightly lane and the run', () => {
  const body = buildFailureBody({
    laneLabel: 'perf-nightly',
    failed: ['perf-nightly: failure'],
    runUrl: 'https://x/run/1',
    runId: '1',
    issueRef: 'tsouza/cerberus#2370',
  });
  assert.match(body, /`perf-nightly`/);
  assert.match(body, /perf-nightly: failure/);
  assert.match(body, /https:\/\/x\/run\/1/);
  assert.match(body, /#2370/);
});

test('buildRecoveryBody names the perf-nightly lane and the run', () => {
  const body = buildRecoveryBody({ laneLabel: 'perf-nightly', runUrl: 'https://x/run/2', runId: '2' });
  assert.match(body, /`perf-nightly`/);
  assert.match(body, /https:\/\/x\/run\/2/);
  assert.match(body, /clean pass/);
});

// Wiring pins, mirroring notify-nightly-failure.test.mjs's regex-over-source
// approach: a job/needs edit that silently drops the trigger scope or
// widens it off `schedule` compiles fine and passes every unit test above,
// so the only thing that catches it is pinning the source text directly.

const perfNightlyWorkflow = readFileSync(resolve('.github/workflows/perf-nightly.yml'), 'utf8');
const ciWorkflow = readFileSync(resolve('.github/workflows/ci.yml'), 'utf8');

test('perf-nightly-health-notify needs perf-nightly and runs on schedule only', () => {
  assert.match(
    perfNightlyWorkflow,
    /perf-nightly-health-notify:\n {4}name: perf-nightly-health-notify\n {4}needs: \[perf-nightly\]\n {4}if: always\(\) && github\.event_name == 'schedule'/,
  );
});

test('perf-nightly-health-notify has issues: write and invokes the script', () => {
  assert.match(perfNightlyWorkflow, /perf-nightly-health-notify[\s\S]{0,400}issues: write/);
  assert.match(perfNightlyWorkflow, /run: node \.github\/scripts\/notify-perf-nightly-failure\.mjs/);
});

test('the self-test runs on the PR path via ci.yml', () => {
  assert.match(ciWorkflow, /node --test \.github\/scripts\/notify-perf-nightly-failure\.test\.mjs/);
});
