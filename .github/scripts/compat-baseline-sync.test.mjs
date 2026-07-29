// compat-baseline-sync.test.mjs — pins for the baseline updater.
//
// This tool writes the roster the parity ratchet gates on, so a bug in
// it either burns a CI cycle (an unsorted or short roster fails closed)
// or, worse, quietly records a divergence. The round-trip case below is
// the load-bearing one: what the tool writes must satisfy the ratchet.

import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';

const SYNC = fileURLToPath(new URL('./compat-baseline-sync.mjs', import.meta.url));
const RATCHET = fileURLToPath(new URL('./compat-ratchet.mjs', import.meta.url));

const HEAD = 'tempo';

// fixture lays down a baseline with a placeholder entry for HEAD plus a
// cases file, and returns the paths so a test can drive either script.
function fixture(cases) {
  const dir = mkdtempSync(path.join(tmpdir(), 'compat-sync-'));
  const baselinePath = path.join(dir, 'parity-baseline.json');
  const casesPath = path.join(dir, 'compat-cases.json');
  const scorePath = path.join(dir, 'compat-score.json');
  writeFileSync(
    baselinePath,
    JSON.stringify({ heads: { [HEAD]: { passed: 0, total: 0, cases: [] } } }, null, 2),
  );
  writeFileSync(casesPath, JSON.stringify({ head: HEAD, cases }, null, 2));
  writeFileSync(
    scorePath,
    JSON.stringify({ passed: cases.filter((c) => c.passed).length, total: cases.length }),
  );
  return { dir, baselinePath, casesPath, scorePath };
}

function runSync(baselinePath, casesPath) {
  const res = spawnSync(process.execPath, [SYNC, casesPath], {
    encoding: 'utf8',
    env: { ...process.env, BASELINE: baselinePath },
  });
  return { status: res.status, out: `${res.stdout}${res.stderr}`.replaceAll('%0A', '\n') };
}

test('sync writes a sorted roster with counts derived from it', () => {
  const { dir, baselinePath, casesPath } = fixture([
    { id: 'zz | last', passed: true },
    { id: 'aa | first', passed: true },
    { id: 'mm | middle', passed: true },
  ]);
  try {
    const { status, out } = runSync(baselinePath, casesPath);
    assert.equal(status, 0, out);
    const entry = JSON.parse(readFileSync(baselinePath, 'utf8')).heads[HEAD];
    assert.deepEqual(entry.cases, ['aa | first', 'mm | middle', 'zz | last']);
    assert.equal(entry.passed, 3);
    assert.equal(entry.total, 3);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('sync refuses to record a roster that omits a failing case', () => {
  // Dropping the failing case would leave a green baseline over a known
  // divergence — an allow-list written by a tool instead of by hand.
  const { dir, baselinePath, casesPath } = fixture([
    { id: 'ok | a', passed: true },
    { id: 'broken | b', passed: false },
  ]);
  try {
    const { status, out } = runSync(baselinePath, casesPath);
    assert.equal(status, 1, `expected a refusal; output was:\n${out}`);
    assert.match(out, /allow-list/);
    assert.match(out, /broken \| b/);
    const entry = JSON.parse(readFileSync(baselinePath, 'utf8')).heads[HEAD];
    assert.deepEqual(entry.cases, [], 'the baseline must be left untouched on refusal');
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('a synced baseline satisfies the ratchet on the same run', () => {
  // The round trip: after moving the baseline from a run's artefact, that
  // same run must gate green. If this ever fails, every corpus change
  // costs an extra red CI cycle.
  const { dir, baselinePath, casesPath, scorePath } = fixture([
    { id: 'b | two', passed: true },
    { id: 'a | one', passed: true },
  ]);
  try {
    assert.equal(runSync(baselinePath, casesPath).status, 0);
    const res = spawnSync(process.execPath, [RATCHET], {
      encoding: 'utf8',
      env: {
        ...process.env,
        HEAD,
        SCORE: scorePath,
        CASES: casesPath,
        BASELINE: baselinePath,
      },
    });
    assert.equal(res.status, 0, `ratchet rejected a freshly synced baseline:\n${res.stdout}${res.stderr}`);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('sync rejects a cases file with no head', () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'compat-sync-nohead-'));
  try {
    const casesPath = path.join(dir, 'compat-cases.json');
    const baselinePath = path.join(dir, 'parity-baseline.json');
    writeFileSync(casesPath, JSON.stringify({ cases: [{ id: 'a', passed: true }] }));
    writeFileSync(baselinePath, JSON.stringify({ heads: {} }));
    const { status, out } = runSync(baselinePath, casesPath);
    assert.equal(status, 1);
    assert.match(out, /no 'head' field/);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
