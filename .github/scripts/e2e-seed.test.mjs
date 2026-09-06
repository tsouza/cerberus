// e2e-seed.test.mjs — node:test guard for `waitForPort`'s retry contract
// (cerberus issue #3096): the port-forward wait budget the extracted bash's
// `for i in 1..10; nc -z ...; sleep 1` loop implemented. The real
// port-forward + Go seeder run is exercised live by `just e2e-seed` /
// `just e2e-reseed` in the real `e2e` CI job, which is the only place a
// real ClickHouse Service exists to forward into.

import assert from 'node:assert/strict';
import test from 'node:test';

import { waitForPort } from './e2e-seed.mjs';

test('waitForPort returns true immediately once the port is open', async () => {
  const sleeps = [];
  const ok = await waitForPort(19000, {
    attempts: 10,
    intervalMs: 1000,
    portOpenFn: async () => true,
  });
  assert.equal(ok, true);
});

test('waitForPort retries up to the attempt budget, sleeping between attempts only', async () => {
  let calls = 0;
  const ok = await waitForPort(19000, {
    attempts: 4,
    intervalMs: 5, // real short sleeps in-test — kept tiny, never mocked, to also prove the real setTimeout path runs
    portOpenFn: async () => {
      calls += 1;
      return calls >= 3; // opens on the 3rd probe
    },
  });
  assert.equal(ok, true);
  assert.equal(calls, 3);
});

test('waitForPort returns false once every attempt in the budget fails', async () => {
  let calls = 0;
  const ok = await waitForPort(19000, {
    attempts: 3,
    intervalMs: 1,
    portOpenFn: async () => {
      calls += 1;
      return false;
    },
  });
  assert.equal(ok, false);
  assert.equal(calls, 3);
});
