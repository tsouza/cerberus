// regen-diff.test.mjs — node:test guard for the shared "diff of regenerated
// X" idiom (CLAUDE.md invariant 15, issue #3095, epic #3091). Runs the real
// `git` binary against a throwaway repo so the assertions cover the actual
// `git --no-pager diff --stat` behaviour the replaced Justfile lines relied
// on, not a stubbed approximation of it.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = fileURLToPath(new URL('.', import.meta.url));
const CLI = join(here, 'regen-diff.mjs');

// gitRepoFixture — a throwaway repo with one committed file, so a caller can
// modify it and get a real, non-empty `git diff --stat`.
function gitRepoFixture() {
  const dir = mkdtempSync(join(tmpdir(), 'regen-diff-test-'));
  execFileSync('git', ['init', '-q'], { cwd: dir });
  execFileSync('git', ['config', 'user.email', 'test@example.com'], { cwd: dir });
  execFileSync('git', ['config', 'user.name', 'test'], { cwd: dir });
  writeFileSync(join(dir, 'baseline.json'), 'one\ntwo\nthree\n');
  execFileSync('git', ['add', 'baseline.json'], { cwd: dir });
  execFileSync('git', ['commit', '-q', '-m', 'baseline'], { cwd: dir });
  return dir;
}

function runCli(dir, args) {
  return execFileSync(process.execPath, [CLI, ...args], { cwd: dir, encoding: 'utf8' });
}

test('with a label: prints a blank line, a "Diff of regenerated <label>:" banner, then the stat', () => {
  const dir = gitRepoFixture();
  try {
    writeFileSync(join(dir, 'baseline.json'), 'one\ntwo\nthree\nfour\n');
    const out = runCli(dir, ['--label', 'baseline', 'baseline.json']);
    const lines = out.split('\n');
    assert.equal(lines[0], '');
    assert.equal(lines[1], 'Diff of regenerated baseline:');
    assert.match(out, /baseline\.json\s*\|\s*1 \+/);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('a multi-word label is printed verbatim', () => {
  const dir = gitRepoFixture();
  try {
    writeFileSync(join(dir, 'baseline.json'), 'one\ntwo\nthree\nfour\n');
    const out = runCli(dir, ['--label', 'parity ledgers', 'baseline.json']);
    assert.match(out, /^Diff of regenerated parity ledgers:$/m);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('without --label: prints the bare diff-stat, no banner at all (the update-parity-enrolment-baseline shape)', () => {
  const dir = gitRepoFixture();
  try {
    writeFileSync(join(dir, 'baseline.json'), 'one\ntwo\nthree\nfour\n');
    const out = runCli(dir, ['baseline.json']);
    assert.doesNotMatch(out, /Diff of regenerated/);
    assert.match(out, /baseline\.json\s*\|\s*1 \+/);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('a no-op regeneration (no diff) prints nothing beyond the banner', () => {
  const dir = gitRepoFixture();
  try {
    const out = runCli(dir, ['--label', 'baseline', 'baseline.json']);
    assert.equal(out, '\nDiff of regenerated baseline:\n');
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('multiple paths are diffed together, matching update-parity-ledgers\' two-directory call', () => {
  const dir = gitRepoFixture();
  try {
    writeFileSync(join(dir, 'a.json'), 'x\n');
    writeFileSync(join(dir, 'b.json'), 'y\n');
    execFileSync('git', ['add', 'a.json', 'b.json'], { cwd: dir });
    execFileSync('git', ['commit', '-q', '-m', 'seed'], { cwd: dir });
    writeFileSync(join(dir, 'a.json'), 'x\nx2\n');
    writeFileSync(join(dir, 'b.json'), 'y\ny2\n');
    const out = runCli(dir, ['--label', 'parity ledgers', 'a.json', 'b.json']);
    assert.match(out, /a\.json/);
    assert.match(out, /b\.json/);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('exits 0 even though `git diff --stat` on an unmodified path prints nothing — matches `|| true`', () => {
  const dir = gitRepoFixture();
  try {
    // No modification at all: exercises the never-throws, always-0-exit path.
    execFileSync(process.execPath, [CLI, '--label', 'baseline', 'baseline.json'], { cwd: dir });
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('CLI: no path arguments exits non-zero', () => {
  const dir = gitRepoFixture();
  try {
    assert.throws(() => execFileSync(process.execPath, [CLI], { cwd: dir, stdio: 'pipe' }));
    assert.throws(() => execFileSync(process.execPath, [CLI, '--label', 'x'], { cwd: dir, stdio: 'pipe' }));
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
