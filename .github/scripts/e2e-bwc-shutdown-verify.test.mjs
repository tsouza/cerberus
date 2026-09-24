// Unit tests for e2e-bwc-shutdown-verify.mjs's pure verdict functions.
//
// The verifier needs a live k3d cluster; the two functions below decide every
// verdict it reports, from numbers the cluster hands them, so they are the
// part that must be provable without one. The values are the chart defaults
// (grace 150s, wait 120s) and the lane's 20s bounded query.

import assert from 'node:assert/strict';
import test from 'node:test';

import { shutdownBudgetDefects, terminationDefects } from './e2e-bwc-shutdown-verify.mjs';

const BUDGET = { grace: 150, wait: 120, drain: 1, querySeconds: 20 };

test('the chart default budget passes', () => {
  assert.deepEqual(shutdownBudgetDefects(BUDGET), []);
});

test('a grace no longer than the wait is a defect', () => {
  for (const grace of [120, 30]) {
    const defects = shutdownBudgetDefects({ ...BUDGET, grace });
    assert.equal(defects.length, 1);
    assert.match(defects[0], /does not exceed shutdown_wait_unfinished=120/);
  }
});

test('a server that cancels queries on SIGTERM is a defect', () => {
  const defects = shutdownBudgetDefects({ ...BUDGET, drain: 0 });
  assert.equal(defects.length, 1);
  assert.match(defects[0], /shutdown_wait_unfinished_queries=0/);
});

test('a bounded query longer than the wait makes the drain check vacuous', () => {
  const defects = shutdownBudgetDefects({ ...BUDGET, querySeconds: 120 });
  assert.equal(defects.length, 1);
  assert.match(defects[0], /would prove nothing/);
});

test('an unreadable budget is reported, not compared', () => {
  const defects = shutdownBudgetDefects({ ...BUDGET, grace: Number('') + 0.5 });
  assert.equal(defects.length, 1);
  assert.match(defects[0], /could not read the budget/);
});

const DRAINED = { clientStatus: 0, clientOutput: '40\n', expectedRows: 40, goneAfterSeconds: 21.5, grace: 150 };

test('a completed query and an early exit pass', () => {
  assert.deepEqual(terminationDefects(DRAINED), []);
});

test('a query cut off by the termination is a defect', () => {
  const defects = terminationDefects({ ...DRAINED, clientStatus: 210, clientOutput: 'Code: 210. DB::NetException: Connection reset by peer' });
  assert.equal(defects.length, 1);
  assert.match(defects[0], /did not complete across the termination/);
});

test('a query that returned the wrong row count is a defect', () => {
  assert.equal(terminationDefects({ ...DRAINED, clientOutput: '12' }).length, 1);
});

test('a pod that lasted its whole grace was killed', () => {
  for (const goneAfterSeconds of [150, 151.2]) {
    const defects = terminationDefects({ ...DRAINED, goneAfterSeconds });
    assert.equal(defects.length, 1);
    assert.match(defects[0], /it was killed, not shut down/);
  }
});
