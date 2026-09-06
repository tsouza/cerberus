// coverage-fold.test.mjs — node:test guard for the `mode: set` union fold
// (CLAUDE.md invariant 15, issue #3095, epic #3091): the widest-count
// dedup, the header-skip-by-position (not by content), the sorted output,
// and the CLI's in-place / multi-file-merge shapes.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

import { foldProfile, writeFoldedProfile } from './coverage-fold.mjs';

const here = fileURLToPath(new URL('.', import.meta.url));
const CLI = join(here, 'coverage-fold.mjs');

test('foldProfile keeps the widest count per block across every input', () => {
  const a = 'mode: set\nfoo.go:1.1,2.2 3 1\nfoo.go:3.1,4.2 1 5\n';
  const b = 'mode: set\nfoo.go:1.1,2.2 3 7\nfoo.go:3.1,4.2 1 2\n';
  const out = foldProfile([a, b]);
  assert.equal(out, 'mode: set\nfoo.go:1.1,2.2 3 7\nfoo.go:3.1,4.2 1 5\n');
});

test('foldProfile sorts rows lexically, matching the replaced `| sort`', () => {
  const text = 'mode: set\nzzz.go:1.1,2.2 1 1\naaa.go:1.1,2.2 1 1\n';
  const out = foldProfile([text]);
  assert.equal(out, 'mode: set\naaa.go:1.1,2.2 1 1\nzzz.go:1.1,2.2 1 1\n');
});

test('foldProfile skips the first line of EVERY input by position, whatever its content', () => {
  // Mirrors awk's `FNR==1{next}`: the first line of a file is dropped even
  // when — unlike a real profile — it happens to look like a data row.
  const text = 'foo.go:1.1,2.2 3 9\nfoo.go:3.1,4.2 1 5\n';
  const out = foldProfile([text]);
  assert.equal(out, 'mode: set\nfoo.go:3.1,4.2 1 5\n');
});

test('foldProfile on a single-line (header-only) input yields no rows', () => {
  assert.equal(foldProfile(['mode: set\n']), 'mode: set\n');
});

test('foldProfile de-duplicates a block repeated within the SAME input (the -coverpkg fan-out shape)', () => {
  const text = 'mode: set\nfoo.go:1.1,2.2 3 0\nfoo.go:1.1,2.2 3 4\nfoo.go:1.1,2.2 3 2\n';
  assert.equal(foldProfile([text]), 'mode: set\nfoo.go:1.1,2.2 3 4\n');
});

test('writeFoldedProfile folds two distinct files into a third', () => {
  const dir = mkdtempSync(join(tmpdir(), 'coverage-fold-test-'));
  try {
    const a = join(dir, 'a.out');
    const b = join(dir, 'b.out');
    const out = join(dir, 'merged.out');
    writeFileSync(a, 'mode: set\nfoo.go:1.1,2.2 1 1\n');
    writeFileSync(b, 'mode: set\nfoo.go:1.1,2.2 1 9\n');
    writeFoldedProfile(out, [a, b]);
    assert.equal(readFileSync(out, 'utf8'), 'mode: set\nfoo.go:1.1,2.2 1 9\n');
    // Inputs are untouched.
    assert.equal(readFileSync(a, 'utf8'), 'mode: set\nfoo.go:1.1,2.2 1 1\n');
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('writeFoldedProfile folds a file IN PLACE (out path repeated as the sole input)', () => {
  const dir = mkdtempSync(join(tmpdir(), 'coverage-fold-test-'));
  try {
    const file = join(dir, 'cover.out');
    writeFileSync(file, 'mode: set\nfoo.go:1.1,2.2 1 3\nfoo.go:1.1,2.2 1 1\n');
    writeFoldedProfile(file, [file]);
    assert.equal(readFileSync(file, 'utf8'), 'mode: set\nfoo.go:1.1,2.2 1 3\n');
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('CLI: node coverage-fold.mjs <out> <in...> folds and writes', () => {
  const dir = mkdtempSync(join(tmpdir(), 'coverage-fold-cli-test-'));
  try {
    const a = join(dir, 'a.out');
    const b = join(dir, 'b.out');
    const out = join(dir, 'merged.out');
    writeFileSync(a, 'mode: set\nfoo.go:1.1,2.2 1 2\n');
    writeFileSync(b, 'mode: set\nfoo.go:1.1,2.2 1 8\n');
    execFileSync(process.execPath, [CLI, out, a, b]);
    assert.equal(readFileSync(out, 'utf8'), 'mode: set\nfoo.go:1.1,2.2 1 8\n');
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('CLI: missing arguments exits non-zero', () => {
  assert.throws(() => execFileSync(process.execPath, [CLI], { stdio: 'pipe' }));
  assert.throws(() => execFileSync(process.execPath, [CLI, '/tmp/out.out'], { stdio: 'pipe' }));
});
