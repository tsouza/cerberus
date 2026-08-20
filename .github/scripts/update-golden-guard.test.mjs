import assert from 'node:assert/strict';
import { test } from 'node:test';

import { runTargetsBranch, waitForBranchClear } from './update-golden-guard.mjs';

function run(overrides = {}) {
  return {
    display_title: 'update-golden[fix/example]',
    status: 'in_progress',
    html_url: 'https://github.com/tsouza/cerberus/actions/runs/1',
    ...overrides,
  };
}

test('runTargetsBranch matches the exact run-name shape update-golden.yml stamps', () => {
  assert.equal(runTargetsBranch('update-golden[fix/example]', 'fix/example'), true);
});

test('runTargetsBranch does not match a different branch, even a prefix of it', () => {
  assert.equal(runTargetsBranch('update-golden[fix/example-extra]', 'fix/example'), false);
  assert.equal(runTargetsBranch('update-golden[fix/example]', 'fix/example-extra'), false);
});

test('runTargetsBranch does not match an unrelated display title', () => {
  assert.equal(runTargetsBranch('update-golden', 'fix/example'), false);
  assert.equal(runTargetsBranch('some other workflow', 'fix/example'), false);
});

test('waitForBranchClear passes immediately when nothing targets the branch', async () => {
  let calls = 0;
  const result = await waitForBranchClear({
    listRuns: async () => {
      calls += 1;
      return [run({ display_title: 'update-golden[other-branch]' })];
    },
    branch: 'fix/example',
    sleep: async () => assert.fail('must not sleep when the branch is already clear'),
  });
  assert.deepEqual(result, { clear: true, waitedMs: 0, runs: [] });
  assert.equal(calls, 1);
});

test('waitForBranchClear polls until the matching run leaves the in-flight list', async () => {
  const responses = [
    [run()], // in_progress
    [run({ status: 'queued' })], // still there, now queued (serialised second dispatch)
    [], // finished — no longer in_progress or queued
  ];
  let sleeps = 0;
  const result = await waitForBranchClear({
    listRuns: async () => responses.shift(),
    branch: 'fix/example',
    pollIntervalMs: 5,
    sleep: async () => {
      sleeps += 1;
    },
    now: () => 0,
  });
  assert.equal(result.clear, true);
  assert.equal(sleeps, 2);
});

test('waitForBranchClear fails closed once the deadline passes with a run still in flight', async () => {
  let ticks = 0;
  const result = await waitForBranchClear({
    listRuns: async () => [run()],
    branch: 'fix/example',
    pollIntervalMs: 10,
    maxWaitMs: 25,
    sleep: async () => {
      ticks += 10;
    },
    now: () => ticks,
  });
  assert.equal(result.clear, false);
  assert.equal(result.timedOut, true);
  assert.equal(result.runs.length, 1);
});

test('waitForBranchClear ignores a run for a branch whose name is a substring of this one', async () => {
  const result = await waitForBranchClear({
    listRuns: async () => [run({ display_title: 'update-golden[fix/example]' })],
    branch: 'fix/example-longer',
    sleep: async () => assert.fail('must not sleep — the only run present targets a different branch'),
  });
  assert.equal(result.clear, true);
});

test('waitForBranchClear treats several in-flight runs (serialised dispatches) as one hazard', async () => {
  const responses = [
    [run({ html_url: 'https://…/1' }), run({ html_url: 'https://…/2', status: 'queued' })],
    [],
  ];
  let sleeps = 0;
  const result = await waitForBranchClear({
    listRuns: async () => responses.shift(),
    branch: 'fix/example',
    sleep: async () => {
      sleeps += 1;
    },
    now: () => 0,
  });
  assert.equal(result.clear, true);
  assert.equal(sleeps, 1);
});

test('waitForBranchClear calls onWaiting with the matching runs while polling', async () => {
  const responses = [[run()], []];
  const seen = [];
  await waitForBranchClear({
    listRuns: async () => responses.shift(),
    branch: 'fix/example',
    sleep: async () => {},
    now: () => 0,
    onWaiting: (runs) => seen.push(runs.length),
  });
  assert.deepEqual(seen, [1]);
});
