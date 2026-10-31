// needs-verdict.test.mjs — node:test guard for the aggregating-job verdict.
//
// Both directions are pinned: the passing worlds pass, and every not-a-pass
// result of any job fails, so a verdict drifting toward "always ok" (or toward
// testing `failure` alone) cannot stay green here.

import assert from 'node:assert/strict';
import { test } from 'node:test';

import { needsProblems } from './needs-verdict.mjs';

const NOT_A_PASS = ['failure', 'cancelled', 'skipped', '', undefined];

const world = (overrides = {}) => ({
  changes: 'success',
  docsOnly: 'false',
  producers: [
    ['setup', 'success'],
    ['shard', 'success'],
  ],
  ...overrides,
});

test('every job succeeded: no problems', () => {
  assert.deepEqual(needsProblems(world()), []);
});

test('any producer that did not succeed is one problem naming it', () => {
  for (const bad of NOT_A_PASS) {
    const problems = needsProblems(world({ producers: [['setup', 'success'], ['shard', bad]] }));
    assert.equal(problems.length, 1, `shard=${bad}`);
    assert.match(problems[0], /^shard did not succeed/);
  }
});

test('a changes job that did not succeed fails, even with every producer green', () => {
  for (const bad of NOT_A_PASS) {
    const problems = needsProblems(world({ changes: bad }));
    assert.deepEqual(problems, [`changes did not succeed (${bad || 'no result'})`], `changes=${bad}`);
  }
});

test('a docs-only change passes with its producers skipped by design', () => {
  const skipped = [['setup', 'skipped'], ['shard', 'skipped']];
  assert.deepEqual(needsProblems(world({ docsOnly: 'true', producers: skipped })), []);
});

test('a docs-only verdict from a changes job that failed does not excuse a skipped producer', () => {
  const problems = needsProblems(world({ changes: 'failure', docsOnly: 'true', producers: [['shard', 'skipped']] }));
  assert.deepEqual(problems, ['changes did not succeed (failure)', 'shard did not succeed (skipped)']);
});

test('a skipped producer on a code change fails', () => {
  const problems = needsProblems(world({ producers: [['shard', 'skipped']] }));
  assert.deepEqual(problems, ['shard did not succeed (skipped)']);
});
