// coverage-schedule-overlap.test.mjs — node:test guard for the
// cerberus-heavy overlap check behind coverage.yml's nightly schedule
// (tsouza/cerberus#3708).

import assert from 'node:assert/strict';
import test from 'node:test';

import {
  checkOverlappingMainPushRun,
  findOverlappingPushRun,
} from './coverage-schedule-overlap.mjs';

test('findOverlappingPushRun: a queued or in_progress run overlaps', () => {
  assert.equal(findOverlappingPushRun([]), null);
  assert.equal(findOverlappingPushRun(null), null);
  assert.equal(
    findOverlappingPushRun([{ id: 1, status: 'completed', head_sha: 'a' }]),
    null,
  );

  const queued = findOverlappingPushRun([
    { id: 1, status: 'completed', head_sha: 'a' },
    { id: 2, status: 'queued', head_sha: 'b' },
  ]);
  assert.deepEqual(queued, { runId: 2, status: 'queued', headSha: 'b' });

  const inProgress = findOverlappingPushRun([{ id: 3, status: 'in_progress', head_sha: 'c' }]);
  assert.deepEqual(inProgress, { runId: 3, status: 'in_progress', headSha: 'c' });
});

test('findOverlappingPushRun: a completed run (any conclusion) never overlaps', () => {
  assert.equal(
    findOverlappingPushRun([
      { id: 1, status: 'completed', conclusion: 'success', head_sha: 'a' },
      { id: 2, status: 'completed', conclusion: 'failure', head_sha: 'b' },
      { id: 3, status: 'completed', conclusion: 'cancelled', head_sha: 'c' },
    ]),
    null,
  );
});

test('checkOverlappingMainPushRun: missing repo or token fails safe to no overlap', async () => {
  assert.equal(await checkOverlappingMainPushRun({ repo: null, token: 't' }), null);
  assert.equal(await checkOverlappingMainPushRun({ repo: 'o/r', token: null }), null);
});

test('checkOverlappingMainPushRun: reports the overlapping run from a live response', async () => {
  const fetchImpl = async (url) => {
    assert.match(url, /repos\/o\/r\/actions\/workflows\/coverage\.yml\/runs/);
    assert.match(url, /event=push/);
    assert.match(url, /branch=main/);
    return {
      ok: true,
      status: 200,
      json: async () => ({
        workflow_runs: [{ id: 99, status: 'in_progress', head_sha: 'deadbeef' }],
      }),
    };
  };
  const result = await checkOverlappingMainPushRun({ repo: 'o/r', token: 't', fetchImpl });
  assert.deepEqual(result, { runId: 99, status: 'in_progress', headSha: 'deadbeef' });
});

test('checkOverlappingMainPushRun: no overlapping run in a live response', async () => {
  const fetchImpl = async () => ({
    ok: true,
    status: 200,
    json: async () => ({ workflow_runs: [{ id: 1, status: 'completed', head_sha: 'a' }] }),
  });
  assert.equal(await checkOverlappingMainPushRun({ repo: 'o/r', token: 't', fetchImpl }), null);
});

test('checkOverlappingMainPushRun: a transport/API failure fails safe to no overlap', async () => {
  const fetchImpl = async () => ({ ok: false, status: 500, statusText: 'boom', json: async () => ({}) });
  assert.equal(await checkOverlappingMainPushRun({ repo: 'o/r', token: 't', fetchImpl }), null);

  const throwing = async () => {
    throw new Error('network down');
  };
  assert.equal(await checkOverlappingMainPushRun({ repo: 'o/r', token: 't', fetchImpl: throwing }), null);
});
