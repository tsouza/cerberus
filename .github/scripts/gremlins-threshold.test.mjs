// Tests for the mutation-efficacy gate.
//
// The point of each case is that the gate CAN FAIL, and fails for the right
// reason. A gate that only ever sees healthy reports is indistinguishable from
// one that reads nothing — and this script guards a number that gremlins
// computes over a subset of the mutants it ran.

import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';

import { countMutationStatuses, detectedEfficacy } from './gremlins-threshold.mjs';

const script = new URL('./gremlins-threshold.mjs', import.meta.url).pathname;

// mutations builds a files[] block with the given per-status counts.
function mutations({ killed = 0, lived = 0, timedOut = 0 }) {
  const out = [];
  for (let i = 0; i < killed; i++) out.push({ type: 'X', status: 'KILLED', line: i, column: 1 });
  for (let i = 0; i < lived; i++) out.push({ type: 'X', status: 'LIVED', line: i, column: 1 });
  for (let i = 0; i < timedOut; i++) out.push({ type: 'X', status: 'TIMED OUT', line: i, column: 1 });
  return [{ file_name: 'a.go', mutations: out }];
}

function run(report, threshold) {
  const dir = mkdtempSync(join(tmpdir(), 'gremlins-threshold-'));
  const path = join(dir, 'gremlins.json');
  writeFileSync(path, JSON.stringify(report));
  try {
    return { code: 0, out: execFileSync('node', [script], {
      env: { ...process.env, REPORT: path, THRESHOLD: String(threshold) },
      encoding: 'utf8',
    }) };
  } catch (e) {
    return { code: e.status, out: `${e.stdout ?? ''}${e.stderr ?? ''}` };
  }
}

test('a healthy leg passes', () => {
  // Real phase4-promql-g numbers.
  const r = run({ test_efficacy: 99.65, files: mutations({ killed: 286, lived: 1, timedOut: 8 }) }, 95);
  assert.equal(r.code, 0, r.out);
});

test('efficacy below the threshold still fails', () => {
  const r = run({ test_efficacy: 90, files: mutations({ killed: 90, lived: 10 }) }, 95);
  assert.equal(r.code, 1);
  // gh.mjs percent-encodes `%` into the workflow command, so match around it.
  assert.match(r.out, /efficacy 90\S* < threshold 95/);
});

// A mutant that exhausts a 15s budget against a 1-2.4s baseline broke
// termination — the suite caught it by hanging. Real phase2-builder numbers,
// which gremlins itself scored 97.50% over 40 of 399.
test('timed-out mutants count as detected', () => {
  const r = run({ test_efficacy: 97.5, files: mutations({ killed: 39, lived: 1, timedOut: 279 }) }, 95);
  assert.equal(r.code, 0, r.out);
  assert.match(r.out, /timed out 279\/319 mutants, counted as detected/);
});

// The case counting-as-detected must never wave through: nothing completed, so
// the timeouts describe the budget rather than the tests (#2692).
test('a leg where nothing completed fails, even though every mutant timed out', () => {
  const r = run({ test_efficacy: 0, files: mutations({ timedOut: 295 }) }, 0);
  assert.equal(r.code, 1, 'a total budget collapse must not score 295/295 = 100%');
  assert.match(r.out, /completed 0 mutant/);
});

test('the detected rate is never below the rate gremlins reported', () => {
  // Counting timeouts into numerator AND denominator moves the ratio toward
  // 100%, so there is no report on which a second threshold comparison could
  // fire. Pinned so nobody re-adds one as a "second opinion" — it would be
  // dead code, and dead code in a gate reads as coverage.
  for (const [killed, lived, timedOut] of [
    [39, 1, 279],
    [5, 40, 5],
    [1, 1, 0],
    [0, 7, 3],
  ]) {
    const reported = (killed / (killed + lived)) * 100;
    const detected = detectedEfficacy({ killed, lived, timedOut });
    assert.ok(
      detected >= reported - Number.EPSILON,
      `detected ${detected} < reported ${reported} for ${killed}/${lived}/${timedOut}`,
    );
  }
});

test('countMutationStatuses matches only the exact status strings', () => {
  const c = countMutationStatuses({
    files: [{ mutations: [{ status: 'TIMED OUT' }, { status: 'TIMEDOUT' }, { status: 'KILLED' }, { status: 'LIVED' }] }],
  });
  assert.deepEqual(c, { total: 4, timedOut: 1, killed: 1, lived: 1 });
});

test('detectedEfficacy reports null when no mutant reached a verdict', () => {
  assert.equal(detectedEfficacy({ killed: 0, lived: 0, timedOut: 0 }), null);
  assert.equal(detectedEfficacy({ killed: 1, lived: 1, timedOut: 2 }), 75);
});

test('a report with no per-mutation records falls back to the reported ratio', () => {
  const r = run({ test_efficacy: 99, files: [] }, 95);
  assert.equal(r.code, 0);
  assert.doesNotMatch(r.out, /timed out/);
});
