// poll.test.mjs — node:test guard for lib/poll.mjs's pollUntil (cerberus
// issue #3172). Every case uses a 0ms intervalMs so the suite runs
// instantly regardless of the real deadline being asserted against.

import assert from 'node:assert/strict';
import test from 'node:test';

import { pollUntil } from './poll.mjs';

test('pollUntil returns true the first time fn succeeds', async () => {
  let calls = 0;
  const ok = await pollUntil(
    async () => {
      calls++;
      return calls === 2;
    },
    { deadlineMs: 1000, intervalMs: 0 },
  );
  assert.equal(ok, true);
  assert.equal(calls, 2);
});

test('pollUntil returns false once deadlineMs elapses without success', async () => {
  let calls = 0;
  const ok = await pollUntil(
    async () => {
      calls++;
      return false;
    },
    { deadlineMs: 20, intervalMs: 5 },
  );
  assert.equal(ok, false);
  assert.ok(calls >= 1);
});

test('a thrown attempt counts as a failure, not an abort', async () => {
  let calls = 0;
  const ok = await pollUntil(
    async () => {
      calls++;
      if (calls === 1) throw new Error('transient');
      return true;
    },
    { deadlineMs: 1000, intervalMs: 0 },
  );
  assert.equal(ok, true);
  assert.equal(calls, 2);
});

test('fn receives a 1-based attempt counter', async () => {
  const seen = [];
  await pollUntil(
    async (attempt) => {
      seen.push(attempt);
      return seen.length === 3;
    },
    { deadlineMs: 1000, intervalMs: 0 },
  );
  assert.deepEqual(seen, [1, 2, 3]);
});

test('intervalMs defaults to DEFAULT_POLL_INTERVAL_MS when omitted', async () => {
  // A deadline shorter than the default 2s interval must still get exactly
  // one attempt before timing out — proves the default is actually wired,
  // not silently zero (which would spin instead of sleeping).
  let calls = 0;
  const ok = await pollUntil(
    async () => {
      calls++;
      return false;
    },
    { deadlineMs: 10 },
  );
  assert.equal(ok, false);
  assert.equal(calls, 1);
});
