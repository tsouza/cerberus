// Unit tests for e2e-datashard-verify.mjs's pure interval arithmetic.
//
// The verifier itself needs a live k3d cluster, but peakOverlap is plain
// arithmetic over query_log rows and decides TWO assertions the datashard
// lane fails on — so it is exactly the part that must be provable without a
// cluster. It shipped with no coverage at all and a real false-failure in
// it; these cases are written from the shapes that lane actually produces.

import assert from 'node:assert/strict';
import test from 'node:test';

import { peakOverlap } from './e2e-datashard-verify.mjs';

// iv builds one child-statement interval the way the verifier does: a start
// microsecond, a duration, and the initial_query_id naming the dispatch that
// issued it.
const iv = (startUs, durUs, qid) => ({ startUs, durUs, qid });

test('peakOverlap counts simultaneous children', () => {
  const { peak } = peakOverlap([iv(0, 10, 'a'), iv(1, 10, 'a'), iv(100, 10, 'a')]);
  assert.equal(peak, 2);
});

test('a single dispatch, however wide, is one concurrent dispatch', () => {
  // One rate() dispatch at N=2 is 3 physical scans x 2 shards = 6 children,
  // all overlapping. Six concurrent statements, but no cross-request
  // evidence whatsoever — the distinction the gate assertion rests on.
  const children = Array.from({ length: 6 }, (_, i) => iv(i, 100, 'solo'));
  const { peak, maxDistinctQidsLive } = peakOverlap(children);
  assert.equal(peak, 6);
  assert.equal(maxDistinctQidsLive, 1);
});

test('two overlapping dispatches are seen even when a wider one peaks first', () => {
  // The regression this file exists for. The peak (6) is reached FIRST by a
  // single wide dispatch; two dispatches overlap later at a lower
  // concurrency. Reporting the count at the first peak instant answers 1 and
  // fails the lane; the honest answer is 2 — two admitted requests really
  // were in flight together.
  const solo = Array.from({ length: 6 }, (_, i) => iv(i, 50, 'wide'));
  const pair = [iv(1000, 50, 'x'), iv(1010, 50, 'y')];
  const { peak, maxDistinctQidsLive } = peakOverlap([...solo, ...pair]);
  assert.equal(peak, 6, 'the wide dispatch still sets the peak');
  assert.equal(maxDistinctQidsLive, 2, 'two dispatches overlapped, at a later and lower-concurrency instant');
});

test('dispatches that never overlap are not counted as concurrent', () => {
  const { maxDistinctQidsLive } = peakOverlap([iv(0, 10, 'a'), iv(100, 10, 'b'), iv(200, 10, 'c')]);
  assert.equal(maxDistinctQidsLive, 1);
});

test('an interval ending exactly where the next begins does not overlap it', () => {
  // Close-before-open at an equal timestamp: back-to-back statements are
  // sequential, and counting them as concurrent would manufacture the
  // cross-request evidence the assertion is supposed to demand.
  const { peak, maxDistinctQidsLive } = peakOverlap([iv(0, 10, 'a'), iv(10, 10, 'b')]);
  assert.equal(peak, 1);
  assert.equal(maxDistinctQidsLive, 1);
});

test('a zero-duration statement still occupies an instant', () => {
  // query_duration_ms can round to 0 for a sub-millisecond statement; the
  // verifier floors the width at 1us so such a row cannot vanish from the
  // overlap arithmetic entirely.
  const { peak } = peakOverlap([iv(5, 0, 'a'), iv(5, 0, 'b')]);
  assert.equal(peak, 2);
});

test('no intervals is a clean zero rather than a throw', () => {
  assert.deepEqual(peakOverlap([]), { peak: 0, maxDistinctQidsLive: 0 });
});
