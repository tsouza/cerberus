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
function mutations({ killed = 0, lived = 0, timedOut = 0, runTimedOut = 0, unknown = 0 }) {
  const out = [];
  for (let i = 0; i < killed; i++) out.push({ type: 'X', status: 'KILLED', line: i, column: 1 });
  for (let i = 0; i < lived; i++) out.push({ type: 'X', status: 'LIVED', line: i, column: 1 });
  for (let i = 0; i < timedOut; i++) out.push({ type: 'X', status: 'TIMED OUT', line: i, column: 1 });
  for (let i = 0; i < runTimedOut; i++) {
    out.push({ type: 'X', status: 'RUN TIMED OUT', line: i, column: 1 });
  }
  for (let i = 0; i < unknown; i++) out.push({ type: 'X', status: 'SOMETHING NEW', line: i, column: 1 });
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
  assert.match(r.out, /detected 22 of 345 attempted/);
});

// The rule the whole gate rests on, checked as a property rather than on one
// report: moving a mutant into the UNADJUDICATED status may never raise the
// score. Under the reading #2903 replaced — every timeout a detection — each of
// these pairs moved the wrong way, and that reading is what a starved runner
// exploits. It stays closed: the backstop status is the one a starved compile,
// a hung compile and a memory reap all land in.
test('moving any mutant into the backstop TIMED OUT never raises the score', () => {
  for (const [killed, lived, timedOut, runTimedOut] of [
    [486, 31, 4, 0],
    [39, 1, 279, 0],
    [5, 40, 5, 0],
    [1, 1, 0, 0],
    [0, 7, 3, 0],
    [990, 32, 0, 0],
    [361, 13, 11, 14],
    [280, 14, 9, 8],
  ]) {
    const base = attemptedEfficacy({ killed, lived, timedOut, runTimedOut });
    if (lived > 0) {
      const fromLived = attemptedEfficacy({ killed, lived: lived - 1, timedOut: timedOut + 1, runTimedOut });
      assert.ok(fromLived <= base, `LIVED -> TIMED OUT raised ${base} to ${fromLived}`);
    }
    if (killed > 0) {
      const fromKilled = attemptedEfficacy({ killed: killed - 1, lived, timedOut: timedOut + 1, runTimedOut });
      assert.ok(fromKilled <= base, `KILLED -> TIMED OUT raised ${base} to ${fromKilled}`);
    }
    if (runTimedOut > 0) {
      const fromRun = attemptedEfficacy({ killed, lived, timedOut: timedOut + 1, runTimedOut: runTimedOut - 1 });
      assert.ok(fromRun <= base, `RUN TIMED OUT -> TIMED OUT raised ${base} to ${fromRun}`);
    }
  }
});

// #2944. The one asymmetry the narrow reading introduces, pinned so it is a
// decision rather than an accident: a survivor that stops terminating stops
// being a survivor, so LIVED -> RUN TIMED OUT DOES raise the score. Nothing
// else does, which is why the two timeout kinds may not be collapsed.
test('LIVED -> RUN TIMED OUT raises the score, and KILLED -> RUN TIMED OUT leaves it alone', () => {
  for (const [killed, lived, timedOut, runTimedOut] of [
    [361, 13, 11, 14],
    [280, 14, 9, 8],
    [221, 6, 5, 3],
  ]) {
    const base = attemptedEfficacy({ killed, lived, timedOut, runTimedOut });
    const fromLived = attemptedEfficacy({ killed, lived: lived - 1, timedOut, runTimedOut: runTimedOut + 1 });
    assert.ok(fromLived > base, `LIVED -> RUN TIMED OUT did not raise ${base} (got ${fromLived})`);
    const fromKilled = attemptedEfficacy({
      killed: killed - 1,
      lived,
      timedOut,
      runTimedOut: runTimedOut + 1,
    });
    assert.equal(fromKilled, base, 'KILLED and RUN TIMED OUT are both detections, so the ratio is unchanged');
  }
});

// The distinction stated at the level the gate actually reads: two reports with
// the same total timeout count score differently only because of WHICH bound
// claimed them, and the backstop one is the lower.
test('a leg whose timeouts are all backstop scores below one whose timeouts all ran', () => {
  const backstop = { killed: 361, lived: 13, timedOut: 25, runTimedOut: 0 };
  const ranOut = { killed: 361, lived: 13, timedOut: 0, runTimedOut: 25 };
  assert.ok(attemptedEfficacy(backstop) < attemptedEfficacy(ranOut));
  assert.equal(run(backstop, 95).code, 1, 'backstop timeouts must not carry a leg over the floor');
  assert.equal(run(ranOut, 95).code, 0, 'run-phase timeouts are detections (#2944)');
});

// A memory reap reaches the gate as a BACKSTOP timeout, because the guard kills
// the child and then holds so that no exit status of its own can be read as a
// verdict, and gremlins' compile+run deadline is what finally claims it. #2921
// must not regress through the run-phase credit: the arithmetic below is the
// same leg with the same 25 unadjudicated mutants either way.
test('a memory-reaped mutant is still not a kill', () => {
  const reaped = { killed: 361, lived: 13, timedOut: 25, runTimedOut: 0 };
  const r = run(reaped, 95);
  assert.equal(r.code, 1, `a leg carried over the floor by reaped mutants: ${r.out}`);
  assert.match(r.out, /timed out 25/);
});

// The closed status set. A fork that adds or renames a status must fail loudly:
// an unrecognised status counted as nothing leaves those mutants out of BOTH
// sides of the ratio, which raises the leg's score for free. This is the exact
// failure an older gate would have hit against the fork that introduced RUN
// TIMED OUT.
test('an unrecognised status fails the gate instead of vanishing from the ratio', () => {
  const r = run({ killed: 100, lived: 0, unknown: 40 }, 95);
  assert.equal(r.code, 1, `a report with an unknown status must not pass: ${r.out}`);
  assert.match(r.out, /does not know/);
  assert.match(r.out, /SOMETHING NEW/);
});

test('the statuses that carry no verdict stay outside both sides of the ratio', () => {
  const c = countMutationStatuses({
    files: [
      {
        mutations: [
          { status: 'NOT COVERED' },
          { status: 'NOT VIABLE' },
          { status: 'SKIPPED' },
          { status: 'RUNNABLE' },
          { status: 'KILLED' },
        ],
      },
    ],
  });
  assert.equal(c.unknown.size, 0, 'the unscored statuses are known, just not counted');
  assert.equal(attemptedEfficacy(c), 100);
});

// A run bound that collapsed manufactures RUN TIMED OUT specifically, so the
// budget-collapse floor is computed over NORMAL verdicts only. Crediting run
// timeouts must not turn that signature into a perfect leg.
test('a leg whose every mutant run-timed-out is a collapsed budget, not a perfect suite', () => {
  const r = run({ killed: 0, lived: 0, runTimedOut: 295 }, 95);
  assert.equal(r.code, 1, `295 run timeouts and no normal verdict must not report 100%: ${r.out}`);
  assert.match(r.out, /budget collapsing/);
});

test('with no run-phase timeouts the gated rate is never above gremlins own', () => {
  // gremlins drops both timeout kinds from both sides; this gate keeps the
  // backstop ones in the denominator. Over a report with no RUN TIMED OUT
  // mutants this gate is therefore strictly the stricter of the two.
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

// …and the ordering does NOT hold once run-phase timeouts exist, which is why
// gremlins' number is reported and never compared against. A mutant gremlins
// discards from both sides is one this gate counts as a detection, so the two
// ratios are computed over different mutant sets and neither bounds the other.
// Pinned so the removed "always >= " claim cannot creep back as an assertion.
test('the two ratios are computed over different mutant sets, so neither bounds the other', () => {
  const runHeavy = { killed: 0, lived: 1, timedOut: 0, runTimedOut: 99 };
  assert.equal(gremlinsReported(runHeavy), 0);
  assert.equal(attemptedEfficacy(runHeavy), 99);

  const backstopHeavy = { killed: 90, lived: 10, timedOut: 100, runTimedOut: 0 };
  assert.equal(gremlinsReported(backstopHeavy), 90);
  assert.equal(attemptedEfficacy(backstopHeavy), 45);
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
          { status: 'RUN TIMED OUT' },
          { status: 'KILLED' },
          { status: 'LIVED' },
        ],
      },
    ],
  });
  assert.equal(c.total, 5);
  assert.equal(c.timedOut, 1);
  assert.equal(c.runTimedOut, 1);
  assert.equal(c.killed, 1);
  assert.equal(c.lived, 1);
  // A near-miss spelling is not silently folded into the status it resembles;
  // it is collected as unknown, and main() fails on it.
  assert.deepEqual([...c.unknown.entries()], [['TIMEDOUT', 1]]);
});

test('attemptedEfficacy reports null when the runner attempted nothing', () => {
  assert.equal(attemptedEfficacy({ killed: 0, lived: 0, timedOut: 0 }), null);
  assert.equal(attemptedEfficacy({ killed: 0, lived: 0, timedOut: 0, runTimedOut: 0 }), null);
  assert.equal(attemptedEfficacy({ killed: 1, lived: 1, timedOut: 2 }), 25);
  assert.equal(attemptedEfficacy({ killed: 1, lived: 1, timedOut: 1, runTimedOut: 1 }), 50);
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
