// notify-perf-nightly-selfcheck-failure.test.mjs — node:test guard for
// #2437's self-check tracking-issue roll-up, the THIRD consumer of
// lib/nightly-health-notify.mjs (the #1861 mechanism e2e.yml's own
// notify-nightly-failure.mjs established first, #2370's
// notify-perf-nightly-failure.mjs the second).
//
// The pure roll-up/dedup/decision functions are already exhaustively
// pinned by notify-nightly-failure.test.mjs against the SAME
// lib/nightly-health-notify.mjs implementation — re-asserting them here
// would just be testing the same function object twice. This file covers
// what's actually specific to this consumer: its own constants, its
// single-job result shape, and the workflow/ci wiring.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

import { classifyNightlyHealth } from './lib/nightly-health-notify.mjs';
import {
  PERF_NIGHTLY_SELFCHECK_TRACKING_LABELS,
  PERF_NIGHTLY_SELFCHECK_TRACKING_TITLE,
} from './notify-perf-nightly-selfcheck-failure.mjs';
import { buildFailureBody, buildRecoveryBody } from './lib/nightly-health-notify.mjs';

test('perf-nightly-selfcheck single-job success is a clean run', () => {
  const v = classifyNightlyHealth({ 'perf-nightly-selfcheck': 'success' });
  assert.equal(v.ok, true);
  assert.deepEqual(v.failed, []);
});

test('perf-nightly-selfcheck single-job failure is caught', () => {
  const v = classifyNightlyHealth({ 'perf-nightly-selfcheck': 'failure' });
  assert.equal(v.ok, false);
  assert.deepEqual(v.failed, ['perf-nightly-selfcheck: failure']);
});

test('a missing/empty result reads as a failure, not a silent pass', () => {
  const v = classifyNightlyHealth({ 'perf-nightly-selfcheck': '' });
  assert.equal(v.ok, false);
  assert.match(v.failed[0], /perf-nightly-selfcheck: \(missing\)/);
});

test('the tracking title is distinct from e2e\'s and perf-nightly\'s own', () => {
  assert.equal(
    PERF_NIGHTLY_SELFCHECK_TRACKING_TITLE,
    'perf-nightly self-check found an injected regression was not caught',
  );
  assert.notEqual(PERF_NIGHTLY_SELFCHECK_TRACKING_TITLE, 'nightly e2e run did not reach a clean pass');
  assert.notEqual(PERF_NIGHTLY_SELFCHECK_TRACKING_TITLE, 'nightly perf-nightly run did not reach a clean pass');
});

test('the tracking labels match the shared automated + area/ci pair', () => {
  assert.deepEqual(PERF_NIGHTLY_SELFCHECK_TRACKING_LABELS, ['automated', 'area/ci']);
});

test('buildFailureBody names the perf-nightly-selfcheck lane, the run, and #2437 (not #2370)', () => {
  const body = buildFailureBody({
    laneLabel: 'perf-nightly-selfcheck',
    failed: ['perf-nightly-selfcheck: failure'],
    runUrl: 'https://x/run/1',
    runId: '1',
    issueRef: 'tsouza/cerberus#2437',
  });
  assert.match(body, /`perf-nightly-selfcheck`/);
  assert.match(body, /perf-nightly-selfcheck: failure/);
  assert.match(body, /https:\/\/x\/run\/1/);
  assert.match(body, /#2437/);
});

test('buildRecoveryBody names the perf-nightly-selfcheck lane and the run', () => {
  const body = buildRecoveryBody({ laneLabel: 'perf-nightly-selfcheck', runUrl: 'https://x/run/2', runId: '2' });
  assert.match(body, /`perf-nightly-selfcheck`/);
  assert.match(body, /https:\/\/x\/run\/2/);
  assert.match(body, /clean pass/);
});

// Wiring pins, mirroring notify-perf-nightly-failure.test.mjs's regex-over-
// source approach: a job/needs edit that silently drops the trigger scope
// or widens it off `schedule` compiles fine and passes every unit test
// above, so the only thing that catches it is pinning the source text
// directly.

const workflow = readFileSync(resolve('.github/workflows/perf-nightly-selfcheck.yml'), 'utf8');
const ciWorkflow = readFileSync(resolve('.github/workflows/ci.yml'), 'utf8');

test('perf-nightly-selfcheck-health-notify needs perf-nightly-selfcheck and runs on schedule only', () => {
  assert.match(
    workflow,
    /perf-nightly-selfcheck-health-notify:\n {4}name: perf-nightly-selfcheck-health-notify\n {4}needs: \[perf-nightly-selfcheck\]\n {4}if: always\(\) && github\.event_name == 'schedule'/,
  );
});

test('perf-nightly-selfcheck-health-notify has issues: write and invokes the script', () => {
  assert.match(workflow, /perf-nightly-selfcheck-health-notify[\s\S]{0,400}issues: write/);
  assert.match(workflow, /run: node \.github\/scripts\/notify-perf-nightly-selfcheck-failure\.mjs/);
});

test('perf-nightly-selfcheck.yml carries no pull_request or push trigger', () => {
  assert.doesNotMatch(workflow, /^\s*pull_request:/m);
  assert.doesNotMatch(workflow, /^\s*push:/m);
  assert.match(workflow, /^\s*schedule:/m);
  assert.match(workflow, /^\s*workflow_dispatch:/m);
});

test('the self-test runs on the PR path via ci.yml', () => {
  assert.match(ciWorkflow, /node --test \.github\/scripts\/notify-perf-nightly-selfcheck-failure\.test\.mjs/);
  assert.match(ciWorkflow, /node --test \.github\/scripts\/perf-nightly-selfcheck\.test\.mjs/);
});
