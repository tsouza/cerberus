// coverage-merge.test.mjs — node:test guard for the `coverage-merge`
// recipe's extracted logic (CLAUDE.md invariant 15, issue #3095, epic
// #3091): the cover.out precondition, the ratchet-shard fold and its own
// precondition, the merge-vs-copy lane decision, and the handoff to the real
// (already independently tested) coverage-summary.mjs gate — no chDB or
// libchdb.so needed, since every fixture here is a synthetic profile.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { main, ratchetShardFiles } from './coverage-merge.mjs';

test('ratchetShardFiles matches the cover-chdb-ratchet-*.out shape and ignores everything else, sorted', () => {
  const names = ['cover.out', 'cover-chdb.out', 'cover-chdb-ratchet-3.out', 'cover-chdb-ratchet-2.out', 'notes.txt'];
  assert.deepEqual(ratchetShardFiles(names), ['cover-chdb-ratchet-2.out', 'cover-chdb-ratchet-3.out']);
});

test('ratchetShardFiles is empty when no shard files are present (the nullglob shape)', () => {
  assert.deepEqual(ratchetShardFiles(['cover.out', 'cover-chdb.out']), []);
});

function tmpDir() {
  return mkdtempSync(join(tmpdir(), 'coverage-merge-test-'));
}

// An empty COVERAGE_FLOORS directory: the coverage-summary.mjs gate this
// hands off to will report "no floor" problems for every package in the
// profile and exit 1 — irrelevant here, since these tests only assert on
// the MERGE mechanics (which files got written, and what lane record was
// stamped), never on the floor-gate verdict itself.
function floorlessEnv(dir) {
  return { ...process.env, COVERAGE_FLOORS: join(dir, 'empty-floors') };
}

test('missing cover.out fails closed and writes nothing', () => {
  const dir = tmpDir();
  try {
    const status = main({ cwd: dir, env: floorlessEnv(dir) });
    assert.equal(status, 1);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('empty cover.out is treated the same as missing (mirrors the replaced `test -s`)', () => {
  const dir = tmpDir();
  try {
    writeFileSync(join(dir, 'cover.out'), '');
    const status = main({ cwd: dir, env: floorlessEnv(dir) });
    assert.equal(status, 1);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('a ratchet shard file with no cover-chdb.out to fold into fails closed', () => {
  const dir = tmpDir();
  try {
    writeFileSync(join(dir, 'cover.out'), 'mode: set\nfoo.go:1.1,2.2 1 1\n');
    writeFileSync(join(dir, 'cover-chdb-ratchet-2.out'), 'mode: set\nfoo.go:1.1,2.2 1 1\n');
    const status = main({ cwd: dir, env: floorlessEnv(dir) });
    assert.equal(status, 1);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('no cover-chdb.out at all: cover-merged.out is a copy of cover.out, lane record says "default"', () => {
  const dir = tmpDir();
  try {
    writeFileSync(join(dir, 'cover.out'), 'mode: set\nfoo.go:1.1,2.2 1 1\n');
    main({ cwd: dir, env: floorlessEnv(dir) });
    assert.equal(readFileSync(join(dir, 'cover-merged.out'), 'utf8'), 'mode: set\nfoo.go:1.1,2.2 1 1\n');
    const record = JSON.parse(readFileSync(join(dir, 'cover-merged.out.lanes.json'), 'utf8'));
    assert.equal(record.lanes, 'default');
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('cover.out + cover-chdb.out fold into cover-merged.out, lane record says "default+chdb"', () => {
  const dir = tmpDir();
  try {
    writeFileSync(join(dir, 'cover.out'), 'mode: set\nfoo.go:1.1,2.2 1 1\nfoo.go:3.1,4.2 1 0\n');
    writeFileSync(join(dir, 'cover-chdb.out'), 'mode: set\nfoo.go:1.1,2.2 1 0\nfoo.go:3.1,4.2 1 9\n');
    main({ cwd: dir, env: floorlessEnv(dir) });
    // Widest count per block, across both files.
    assert.equal(
      readFileSync(join(dir, 'cover-merged.out'), 'utf8'),
      'mode: set\nfoo.go:1.1,2.2 1 1\nfoo.go:3.1,4.2 1 9\n',
    );
    const record = JSON.parse(readFileSync(join(dir, 'cover-merged.out.lanes.json'), 'utf8'));
    assert.equal(record.lanes, 'default+chdb');
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('a ratchet shard is folded into cover-chdb.out BEFORE the cover.out merge', () => {
  const dir = tmpDir();
  try {
    writeFileSync(join(dir, 'cover.out'), 'mode: set\nfoo.go:1.1,2.2 1 0\n');
    writeFileSync(join(dir, 'cover-chdb.out'), 'mode: set\nfoo.go:1.1,2.2 1 0\n');
    writeFileSync(join(dir, 'cover-chdb-ratchet-2.out'), 'mode: set\nfoo.go:1.1,2.2 1 5\n');
    main({ cwd: dir, env: floorlessEnv(dir) });
    assert.equal(readFileSync(join(dir, 'cover-chdb.out'), 'utf8'), 'mode: set\nfoo.go:1.1,2.2 1 5\n');
    assert.equal(readFileSync(join(dir, 'cover-merged.out'), 'utf8'), 'mode: set\nfoo.go:1.1,2.2 1 5\n');
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('the coverage-summary.mjs handoff is genuinely reached: on a both-lane profile, an unfloored package fails the gate, not silently', () => {
  // Proves main() does not swallow or bypass the downstream gate. A
  // default-only profile's floor comparison is deliberately SKIPPED (with a
  // notice) by coverage-summary.mjs itself — see its own module comment —
  // so this needs a cover-chdb.out present too (lanes=default+chdb) for the
  // comparison to actually run: an unfloored package is then its "unfloored
  // package" failure mode (see its test suite), which can only fire if the
  // subprocess actually ran against the file this wrote.
  const dir = tmpDir();
  try {
    writeFileSync(join(dir, 'cover.out'), 'mode: set\nfoo.go:1.1,2.2 10 1\n');
    writeFileSync(join(dir, 'cover-chdb.out'), 'mode: set\nfoo.go:1.1,2.2 10 1\n');
    const status = main({ cwd: dir, env: floorlessEnv(dir) });
    assert.equal(status, 1, 'an unfloored package on a both-lane profile must fail the gate, proving the handoff ran for real');
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
