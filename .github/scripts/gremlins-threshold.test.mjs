// Tests for the mutation-efficacy gate.
//
// The point of each case is that the gate CAN FAIL, and fails for the right
// reason. A gate that only ever sees healthy reports is indistinguishable from
// one that reads nothing — and this script guards a number that gremlins
// computes over a subset of the mutants it ran.
//
// The load-bearing case is the pair taken from runs 33542904091 /
// 33551271099: the same commit range, the same 486 kills, and the SLOWER
// runner scoring three points higher under the old reading (#2903). It is
// pinned here as a regression test because it is the shape a gate cannot be
// allowed to reward.

import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';

import { attemptedEfficacy, countMutationStatuses } from './gremlins-threshold.mjs';

const script = new URL('./gremlins-threshold.mjs', import.meta.url).pathname;

// mutations builds a files[] block with the given per-status counts.
function mutations({ killed = 0, lived = 0, timedOut = 0 }) {
  const out = [];
  for (let i = 0; i < killed; i++) out.push({ type: 'X', status: 'KILLED', line: i, column: 1 });
  for (let i = 0; i < lived; i++) out.push({ type: 'X', status: 'LIVED', line: i, column: 1 });
  for (let i = 0; i < timedOut; i++) out.push({ type: 'X', status: 'TIMED OUT', line: i, column: 1 });
  return [{ file_name: 'a.go', mutations: out }];
}

// gremlinsReported is gremlins' own ratio, killed/(killed+lived), which the
// report carries as `test_efficacy`. Computed rather than hand-written so a
// case cannot accidentally pair counts with an inconsistent report field.
function gremlinsReported({ killed, lived }) {
  return (killed / (killed + lived)) * 100;
}

function run(counts, threshold) {
  const dir = mkdtempSync(join(tmpdir(), 'gremlins-threshold-'));
  const path = join(dir, 'gremlins.json');
  writeFileSync(
    path,
    JSON.stringify({ test_efficacy: gremlinsReported(counts), files: mutations(counts) }),
  );
  try {
    return {
      code: 0,
      out: execFileSync('node', [script], {
        env: { ...process.env, REPORT: path, THRESHOLD: String(threshold) },
        encoding: 'utf8',
      }),
    };
  } catch (e) {
    return { code: e.status, out: `${e.stdout ?? ''}${e.stderr ?? ''}` };
  }
}

// The two runs the gate was rewritten for. Same leg, same 486 kills; the green
// run's machine was 44% slower (coverage 53.2s vs 36.8s) and 16 of the red
// run's survivors were booked as timeouts on it.
const promqlLowerRed = { killed: 486, lived: 31, timedOut: 4 };
const promqlLowerStarved = { killed: 486, lived: 15, timedOut: 36 };

test('a healthy leg passes', () => {
  // Real phase4-promql-g numbers.
  assert.equal(run({ killed: 286, lived: 1, timedOut: 8 }, 95).code, 0);
});

test('a leg that genuinely killed its mutants passes with no timeouts at all', () => {
  // The shape a correctly-budgeted leg produces: every mutant adjudicated.
  const r = run({ killed: 990, lived: 32, timedOut: 0 }, 95);
  assert.equal(r.code, 0, r.out);
  assert.doesNotMatch(r.out, /timed out/);
});

test('efficacy below the threshold still fails', () => {
  const r = run({ killed: 90, lived: 10 }, 95);
  assert.equal(r.code, 1);
  // gh.mjs percent-encodes `%` into the workflow command, so match around it.
  assert.match(r.out, /efficacy 90\S* < threshold 95/);
});

// #2903. The starved run of a leg must not outscore the honest one, and must
// not pass a bar the honest one failed.
test('the starved run of a leg scores BELOW the run it was compared against', () => {
  const red = attemptedEfficacy(promqlLowerRed);
  const starved = attemptedEfficacy(promqlLowerStarved);
  assert.ok(
    starved < red,
    `the slower runner scored ${starved} against ${red}: a timeout is still paying efficacy`,
  );
  // And the gate agrees with the arithmetic, in both directions.
  assert.equal(run(promqlLowerRed, 95).code, 1, 'the red run must stay red');
  const r = run(promqlLowerStarved, 95);
  assert.equal(r.code, 1, `the starved run must not pass where the red one failed: ${r.out}`);
  assert.match(r.out, /timed out 36/);
});

// The endpoint of the same bias, from run 33522074818's phase2-builder: a leg
// that adjudicated 22 of 345 mutants, found no survivor because it never ran
// long enough to see one, and reported a perfect score.
test('a leg that adjudicated 6% of its mutants cannot report 100%', () => {
  const counts = { killed: 22, lived: 0, timedOut: 323 };
  assert.equal(gremlinsReported(counts), 100);
  const r = run(counts, 95);
  assert.equal(r.code, 1, `100% over 22 of 345 mutants must not pass: ${r.out}`);
  assert.match(r.out, /killed 22 of 345 attempted/);
});

// The rule the whole gate rests on, checked as a property rather than on one
// report: moving a mutant into TIMED OUT may never raise the score. Under the
// reading this replaced — timeouts as detections — every one of these pairs
// moved the wrong way.
test('moving any mutant into TIMED OUT never raises the score', () => {
  for (const [killed, lived, timedOut] of [
    [486, 31, 4],
    [39, 1, 279],
    [5, 40, 5],
    [1, 1, 0],
    [0, 7, 3],
    [990, 32, 0],
  ]) {
    const base = attemptedEfficacy({ killed, lived, timedOut });
    if (lived > 0) {
      const fromLived = attemptedEfficacy({ killed, lived: lived - 1, timedOut: timedOut + 1 });
      assert.ok(fromLived <= base, `LIVED -> TIMED OUT raised ${base} to ${fromLived}`);
    }
    if (killed > 0) {
      const fromKilled = attemptedEfficacy({ killed: killed - 1, lived, timedOut: timedOut + 1 });
      assert.ok(fromKilled <= base, `KILLED -> TIMED OUT raised ${base} to ${fromKilled}`);
    }
  }
});

test('the gated rate is never above the rate gremlins reported', () => {
  // gremlins drops timeouts from both sides; this gate keeps them in the
  // denominator. So this gate is strictly the stricter of the two, which is
  // why gremlins' own ratio is reported here but not compared against.
  for (const [killed, lived, timedOut] of [
    [486, 15, 36],
    [39, 1, 279],
    [5, 40, 5],
    [1, 1, 0],
    [0, 7, 3],
  ]) {
    const reported = gremlinsReported({ killed, lived });
    const gated = attemptedEfficacy({ killed, lived, timedOut });
    assert.ok(gated <= reported + Number.EPSILON, `gated ${gated} > reported ${reported}`);
  }
});

// The budget-collapse signature keeps its own message: 0/295 already fails the
// ratio, but "your tests are weak" is the wrong diagnosis for a broken budget.
test('a leg where nothing completed fails, and names the budget rather than the tests', () => {
  const r = run({ timedOut: 295 }, 0);
  assert.equal(r.code, 1, 'a total budget collapse must not pass even at a zero threshold');
  assert.match(r.out, /completed 0 mutant/);
  assert.match(r.out, /budget collapsing/);
});

test('countMutationStatuses matches only the exact status strings', () => {
  const c = countMutationStatuses({
    files: [
      {
        mutations: [
          { status: 'TIMED OUT' },
          { status: 'TIMEDOUT' },
          { status: 'KILLED' },
          { status: 'LIVED' },
        ],
      },
    ],
  });
  assert.deepEqual(c, { total: 4, timedOut: 1, killed: 1, lived: 1 });
});

test('attemptedEfficacy reports null when the runner attempted nothing', () => {
  assert.equal(attemptedEfficacy({ killed: 0, lived: 0, timedOut: 0 }), null);
  assert.equal(attemptedEfficacy({ killed: 1, lived: 1, timedOut: 2 }), 25);
});

test('a report with no per-mutation records falls back to the reported ratio', () => {
  const dir = mkdtempSync(join(tmpdir(), 'gremlins-threshold-'));
  const path = join(dir, 'gremlins.json');
  writeFileSync(path, JSON.stringify({ test_efficacy: 99, files: [] }));
  const out = execFileSync('node', [script], {
    env: { ...process.env, REPORT: path, THRESHOLD: '95' },
    encoding: 'utf8',
  });
  assert.doesNotMatch(out, /timed out/);
});

test('a report with no per-mutation records still fails below the threshold', () => {
  const dir = mkdtempSync(join(tmpdir(), 'gremlins-threshold-'));
  const path = join(dir, 'gremlins.json');
  writeFileSync(path, JSON.stringify({ test_efficacy: 90, files: [] }));
  assert.throws(
    () =>
      execFileSync('node', [script], {
        env: { ...process.env, REPORT: path, THRESHOLD: '95' },
        encoding: 'utf8',
        stdio: 'pipe',
      }),
    /./,
  );
});

test('a missing test_efficacy fails rather than being scored as zero timeouts', () => {
  const dir = mkdtempSync(join(tmpdir(), 'gremlins-threshold-'));
  const path = join(dir, 'gremlins.json');
  writeFileSync(path, JSON.stringify({ files: mutations({ killed: 100 }) }));
  let out = '';
  try {
    execFileSync('node', [script], {
      env: { ...process.env, REPORT: path, THRESHOLD: '95' },
      encoding: 'utf8',
    });
    assert.fail('a report with no test_efficacy must not pass');
  } catch (e) {
    out = `${e.stdout ?? ''}${e.stderr ?? ''}`;
  }
  assert.match(out, /test_efficacy missing or non-numeric/);
});
