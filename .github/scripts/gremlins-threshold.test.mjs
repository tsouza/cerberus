// Tests for the mutation-efficacy gate.
//
// The point of each case is that the gate CAN FAIL. A gate that only ever sees
// healthy reports is indistinguishable from one that reads nothing, and this
// script guards a number (efficacy) that is structurally blind to the failure
// mode it now also bounds.

import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';

import { countMutationStatuses } from './gremlins-threshold.mjs';

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
    const stdout = execFileSync('node', [script], {
      env: { ...process.env, REPORT: path, THRESHOLD: String(threshold) },
      encoding: 'utf8',
    });
    return { code: 0, out: stdout };
  } catch (e) {
    return { code: e.status, out: `${e.stdout ?? ''}${e.stderr ?? ''}` };
  }
}

test('a healthy leg passes', () => {
  const r = run(
    { test_efficacy: 99.65, files: mutations({ killed: 286, lived: 1, timedOut: 8 }) },
    95,
  );
  assert.equal(r.code, 0, r.out);
});

test('efficacy below the threshold still fails', () => {
  const r = run({ test_efficacy: 90, files: mutations({ killed: 90, lived: 10 }) }, 95);
  assert.equal(r.code, 1);
  // gh.mjs percent-encodes `%` into the workflow command, so match around it.
  assert.match(r.out, /efficacy 90\S* < threshold 95/);
});

// The regression this gate was extended for. These are the REAL numbers from
// run 33126692323's phase2-other leg, which reported GREEN.
test('a leg that timed out most of its mutants fails despite a high efficacy', () => {
  const r = run(
    { test_efficacy: 98.72, files: mutations({ killed: 77, lived: 1, timedOut: 262 }) },
    95,
  );
  assert.equal(r.code, 1, 'efficacy 98.72% over 78 of 340 mutants must not pass');
  assert.match(r.out, /timed out 262\/340/);
});

test('a few pathological mutants are tolerated', () => {
  // An inverted loop-advance genuinely does not terminate; the ceiling exists
  // to bound it, so a handful of timeouts is the healthy shape, not a failure.
  const r = run({ test_efficacy: 99, files: mutations({ killed: 80, timedOut: 20 }) }, 95);
  assert.equal(r.code, 0, r.out);
});

test('countMutationStatuses matches only the exact status string', () => {
  // A rename upstream must not silently count zero timeouts forever.
  const { total, timedOut } = countMutationStatuses({
    files: [{ mutations: [{ status: 'TIMED OUT' }, { status: 'TIMEDOUT' }, { status: 'KILLED' }] }],
  });
  assert.equal(total, 3);
  assert.equal(timedOut, 1);
});

test('a report with no per-mutation records does not fabricate a share', () => {
  // Older reports, or a leg that produced none: the share is unknowable, and
  // inventing 0/0 = 0 would read as a clean bill of health.
  const r = run({ test_efficacy: 99, files: [] }, 95);
  assert.equal(r.code, 0);
  assert.doesNotMatch(r.out, /timed out/);
});
