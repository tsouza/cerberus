// compat-ratchet.test.mjs — in-process pins for the parity-regression
// ratchet.
//
// The script is a REQUIRED-check gate that runs only after a
// docker-compose compatibility harness has produced its artefacts, so a
// hole in it surfaces as a green check over a broken head — the exact
// failure mode a count-only ratchet had. Every case here drives the real
// script as a subprocess over real files in a temp dir, so the exit code
// under test is the one CI observes.
//
// The load-bearing case is `negative control` below: it builds the swap
// an aggregate ratchet cannot see (one case regresses, another starts
// passing) and asserts BOTH that the aggregate floors would still be
// satisfied AND that the gate fails anyway.

import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';

import { baselineRoster, runRoster, verdicts } from './compat-ratchet.mjs';

const SCRIPT = fileURLToPath(new URL('./compat-ratchet.mjs', import.meta.url));
const REPO = fileURLToPath(new URL('../..', import.meta.url));
const REAL_BASELINE = path.join(REPO, 'compatibility', 'parity-baseline.json');

const HEAD = 'prometheus';

// passingRoster builds a baseline entry from a list of case IDs, in the
// committed shape: full parity, sorted roster, counts agreeing with it.
function passingRoster(ids) {
  const cases = [...ids].sort();
  return { passed: cases.length, total: cases.length, cases };
}

// runRatchet writes a baseline / score / cases triple to a temp dir and
// runs the real script against them.
function runRatchet({ baseline, cases, score, head = HEAD }) {
  const dir = mkdtempSync(path.join(tmpdir(), 'compat-ratchet-'));
  try {
    const baselinePath = path.join(dir, 'parity-baseline.json');
    const casesPath = path.join(dir, 'compat-cases.json');
    const scorePath = path.join(dir, 'compat-score.json');
    writeFileSync(baselinePath, JSON.stringify(baseline, null, 2));
    writeFileSync(casesPath, JSON.stringify({ head, cases }, null, 2));
    const derived = score ?? {
      passed: cases.filter((c) => c.passed).length,
      total: cases.length,
    };
    writeFileSync(scorePath, JSON.stringify(derived, null, 2));
    const res = spawnSync(process.execPath, [SCRIPT], {
      encoding: 'utf8',
      env: {
        ...process.env,
        HEAD: head,
        SCORE: scorePath,
        CASES: casesPath,
        BASELINE: baselinePath,
      },
    });
    return { status: res.status, out: decode(`${res.stdout}${res.stderr}`) };
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}

// decode undoes the %0A escaping a ::error:: workflow command applies to
// its multi-line body, so an assertion can match the lines a reader sees
// in the GitHub log rather than one long escaped string.
function decode(out) {
  return out.replaceAll('%0A', '\n');
}

// aggregateFloorsHold replays the count-only comparison an aggregate
// ratchet performs, so a test can assert that the counts alone would
// have let a run through.
function aggregateFloorsHold(baselineEntry, cases) {
  const runPassed = cases.filter((c) => c.passed).length;
  return runPassed >= baselineEntry.passed && cases.length >= baselineEntry.total;
}

test('negative control: a regressed case fails the gate even though the counts do not drop', () => {
  // The scenario the gate exists for: case `a` breaks, unrelated case
  // `d` starts passing. Passing cases stay at 3 and the corpus only
  // grew, so every aggregate floor is satisfied — and parity is worse.
  const baseline = { heads: { [HEAD]: passingRoster(['a', 'b', 'c']) } };
  const cases = [
    { id: 'a', passed: false },
    { id: 'b', passed: true },
    { id: 'c', passed: true },
    { id: 'd', passed: true },
  ];

  assert.equal(
    aggregateFloorsHold(baseline.heads[HEAD], cases),
    true,
    'precondition: the count-only floors must be satisfied, or this proves nothing',
  );

  const { status, out } = runRatchet({ baseline, cases });
  assert.equal(status, 1, `expected the gate to FAIL; output was:\n${out}`);
  assert.match(out, /REGRESSED/);
  assert.match(out, /- a$/m);
  assert.doesNotMatch(out, /- b$/m, 'only the regressed case should be named as REGRESSED');
});

test('a baseline case that vanished from the corpus fails the gate', () => {
  const baseline = { heads: { [HEAD]: passingRoster(['a', 'b', 'c']) } };
  const cases = [
    { id: 'b', passed: true },
    { id: 'c', passed: true },
  ];
  const { status, out } = runRatchet({ baseline, cases });
  assert.equal(status, 1, `expected the gate to FAIL; output was:\n${out}`);
  assert.match(out, /VANISHED/);
  assert.match(out, /- a$/m);
});

test('a new case that diverges on arrival fails the gate', () => {
  const baseline = { heads: { [HEAD]: passingRoster(['a', 'b']) } };
  const cases = [
    { id: 'a', passed: true },
    { id: 'b', passed: true },
    { id: 'c', passed: false },
  ];
  const { status, out } = runRatchet({ baseline, cases });
  assert.equal(status, 1, `expected the gate to FAIL; output was:\n${out}`);
  assert.match(out, /ARRIVED-FAILING/);
  assert.match(out, /- c$/m);
});

test('a new case that passes fails the gate until the baseline records it', () => {
  const baseline = { heads: { [HEAD]: passingRoster(['a', 'b']) } };
  const cases = [
    { id: 'a', passed: true },
    { id: 'b', passed: true },
    { id: 'c', passed: true },
  ];
  const { status, out } = runRatchet({ baseline, cases });
  assert.equal(status, 1, `expected the gate to FAIL; output was:\n${out}`);
  assert.match(out, /UNRECORDED/);
  assert.match(out, /- c$/m);
});

test('a run that matches the baseline exactly passes', () => {
  const baseline = { heads: { [HEAD]: passingRoster(['a', 'b', 'c']) } };
  const cases = [
    { id: 'a', passed: true },
    { id: 'b', passed: true },
    { id: 'c', passed: true },
  ];
  const { status, out } = runRatchet({ baseline, cases });
  assert.equal(status, 0, `expected the gate to PASS; output was:\n${out}`);
  assert.match(out, /matches the committed baseline/);
});

test('a cases file that disagrees with the score file fails the gate', () => {
  const baseline = { heads: { [HEAD]: passingRoster(['a', 'b']) } };
  const cases = [
    { id: 'a', passed: true },
    { id: 'b', passed: true },
  ];
  // The score claims a third case the roster never mentions: one of the
  // two artefacts is stale, so neither can be trusted to gate.
  const { status, out } = runRatchet({ baseline, cases, score: { passed: 3, total: 3 } });
  assert.equal(status, 1, `expected the gate to FAIL; output was:\n${out}`);
  assert.match(out, /disagree about this run/);
});

test('a cases file from another head fails the gate', () => {
  const baseline = { heads: { [HEAD]: passingRoster(['a']) } };
  const dir = mkdtempSync(path.join(tmpdir(), 'compat-ratchet-head-'));
  try {
    const baselinePath = path.join(dir, 'parity-baseline.json');
    const casesPath = path.join(dir, 'compat-cases.json');
    const scorePath = path.join(dir, 'compat-score.json');
    writeFileSync(baselinePath, JSON.stringify(baseline));
    writeFileSync(casesPath, JSON.stringify({ head: 'loki', cases: [{ id: 'a', passed: true }] }));
    writeFileSync(scorePath, JSON.stringify({ passed: 1, total: 1 }));
    const res = spawnSync(process.execPath, [SCRIPT], {
      encoding: 'utf8',
      env: { ...process.env, HEAD, SCORE: scorePath, CASES: casesPath, BASELINE: baselinePath },
    });
    assert.equal(res.status, 1);
    assert.match(decode(`${res.stdout}${res.stderr}`), /crossed over between heads/);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('a missing cases file fails the gate rather than passing on absent data', () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'compat-ratchet-missing-'));
  try {
    const baselinePath = path.join(dir, 'parity-baseline.json');
    const scorePath = path.join(dir, 'compat-score.json');
    writeFileSync(baselinePath, JSON.stringify({ heads: { [HEAD]: passingRoster(['a']) } }));
    writeFileSync(scorePath, JSON.stringify({ passed: 1, total: 1 }));
    const res = spawnSync(process.execPath, [SCRIPT], {
      encoding: 'utf8',
      env: {
        ...process.env,
        HEAD,
        SCORE: scorePath,
        CASES: path.join(dir, 'nope.json'),
        BASELINE: baselinePath,
      },
    });
    assert.equal(res.status, 1);
    assert.match(decode(`${res.stdout}${res.stderr}`), /not found/);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('baselineRoster rejects an entry that cannot be trusted to gate', () => {
  assert.match(baselineRoster(undefined, HEAD).err, /passed,total/);
  assert.match(baselineRoster({ passed: 1, total: 1 }, HEAD).err, /cases as an array/);
  assert.match(
    baselineRoster({ passed: 2, total: 2, cases: ['a'] }, HEAD).err,
    /lists 1 case\(s\)/,
    'a count that disagrees with the roster would silently shrink what is gated',
  );
  assert.match(
    baselineRoster({ passed: 1, total: 2, cases: ['a'] }, HEAD).err,
    /FULL parity/,
    'there is no shape for recording a divergence as acceptable',
  );
  assert.match(baselineRoster({ passed: 2, total: 2, cases: ['b', 'a'] }, HEAD).err, /not sorted/);
  assert.match(baselineRoster({ passed: 2, total: 2, cases: ['a', 'a'] }, HEAD).err, /twice/);
  assert.equal(baselineRoster({ passed: 1, total: 1, cases: ['a'] }, HEAD).err, undefined);
});

test('runRoster rejects a malformed run roster', () => {
  assert.match(runRoster({ head: HEAD, cases: [{ id: 'a' }] }, HEAD).err, /passed: boolean/);
  assert.match(runRoster({ head: HEAD, cases: [{ id: '', passed: true }] }, HEAD).err, /non-empty/);
  assert.match(
    runRoster({ head: HEAD, cases: [{ id: 'a', passed: true }, { id: 'a', passed: false }] }, HEAD)
      .err,
    /appears twice/,
  );
});

test('verdicts partitions every movement into exactly one bucket', () => {
  const baselineIds = new Set(['keep', 'break', 'gone']);
  const runCases = new Map([
    ['keep', true],
    ['break', false],
    ['new-ok', true],
    ['new-bad', false],
  ]);
  assert.deepEqual(verdicts(baselineIds, runCases), {
    regressed: ['break'],
    vanished: ['gone'],
    arrivedFailing: ['new-bad'],
    unrecorded: ['new-ok'],
  });
});

test('the committed baseline is a roster the ratchet can gate on', () => {
  const baseline = JSON.parse(readFileSync(REAL_BASELINE, 'utf8'));
  const heads = Object.keys(baseline.heads ?? {});
  assert.deepEqual(heads.sort(), ['loki', 'prometheus', 'tempo']);
  for (const head of heads) {
    const { err, ids } = baselineRoster(baseline.heads[head], head);
    assert.equal(err, undefined, `heads.${head} is not gateable: ${err}`);
    assert.ok(ids.size > 0, `heads.${head} has an empty roster — nothing would be gated`);
  }
});
