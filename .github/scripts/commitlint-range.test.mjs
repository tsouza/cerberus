// commitlint-range.test.mjs — in-process pins for the commitlint-range gate
// (issue #1643: GitHub Copilot's empty "Initial plan" placeholder commit
// trips commitlint's subject-empty/type-empty rules and reds the whole
// `lint` required check).
//
// Exercised against REAL git repos in a temp dir, same idiom as
// compat-publish-score.test.mjs: the empty-commit detection is a git
// behaviour (`git diff-tree` on a `--allow-empty` commit), and a mock that
// agreed with the module's own assumptions would pass while the real
// placeholder commit still broke CI.
//
// The orchestration (`run`) is pinned with an INJECTED `lintOne` rather than
// the real commitlint CLI, keeping this file dependency-light (node:
// builtins only, per .github/scripts/README.md) and offline. The injected
// fake reproduces exactly what real commitlint reports for a Conventional
// Commit vs. not — `defaultLintOne`'s wiring to the real CLI was verified
// separately against commitlint@19 (byte-for-byte the same pass/fail split
// this file pins), see the issue for the transcript.
//
// Negative controls matter most here: a genuinely bad NON-empty commit must
// still fail the gate even when an empty commit sits right next to it in the
// range (the empty-commit skip must not neuter real linting), and a bad
// commit whose SUBJECT happens to read "Initial plan" but which DID touch a
// file must still fail (proving the filter is diff-based, not text-based —
// the whole reason this module exists instead of a commitlint `ignores`
// predicate keyed on that literal string).

import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { test } from 'node:test';

import { hasFileChanges, listRangeCommits, run, selectLintableCommits } from './commitlint-range.mjs';

function git(cwd, args) {
  const res = spawnSync('git', args, { cwd, encoding: 'utf8' });
  assert.equal(res.status, 0, `git ${args.join(' ')} failed: ${res.stderr}`);
  return res.stdout;
}

// makeRepo builds: base (real commit) -> real commit -> empty "Initial plan"
// commit -> real commit. Returns the SHAs a test needs plus a helper to
// extend the history further (for the negative-control cases).
function makeRepo() {
  const cwd = mkdtempSync(path.join(tmpdir(), 'commitlint-range-'));
  git(cwd, ['init', '-q', '--initial-branch=main']);
  git(cwd, ['config', 'user.name', 'test']);
  git(cwd, ['config', 'user.email', 'test@example.com']);

  writeFileSync(path.join(cwd, 'a.txt'), 'a\n');
  git(cwd, ['add', 'a.txt']);
  git(cwd, ['commit', '-q', '-m', 'feat: add a']);
  const base = git(cwd, ['rev-parse', 'HEAD']).trim();

  writeFileSync(path.join(cwd, 'b.txt'), 'b\n');
  git(cwd, ['add', 'b.txt']);
  git(cwd, ['commit', '-q', '-m', 'fix: fix b']);

  git(cwd, ['commit', '-q', '--allow-empty', '-m', 'Initial plan']);

  writeFileSync(path.join(cwd, 'c.txt'), 'c\n');
  git(cwd, ['add', 'c.txt']);
  git(cwd, ['commit', '-q', '-m', 'feat: add c']);

  return { cwd, base, head: git(cwd, ['rev-parse', 'HEAD']).trim() };
}

function commit(cwd, file, contents, message, extraArgs = []) {
  writeFileSync(path.join(cwd, file), contents);
  git(cwd, ['add', file]);
  git(cwd, ['commit', '-q', '-m', message, ...extraArgs]);
  return git(cwd, ['rev-parse', 'HEAD']).trim();
}

// A fake `lintOne` standing in for real commitlint: Conventional-Commits
// header shape (`type: subject`) passes, anything else fails — the same
// split @commitlint/config-conventional's subject-empty/type-empty rules
// produce for these exact inputs (verified against the real CLI).
function fakeLintOne(message) {
  const subject = message.split('\n', 1)[0];
  const ok = /^[a-z]+(\([^)]+\))?: .+/.test(subject);
  return { status: ok ? 0 : 1, stdout: ok ? '' : `✖ ${subject}`, stderr: '' };
}

test('listRangeCommits returns every non-merge commit, oldest first, with full messages', () => {
  const { cwd, base, head } = makeRepo();
  try {
    const commits = listRangeCommits(base, head, { cwd });
    assert.deepEqual(
      commits.map((c) => c.message),
      ['fix: fix b', 'Initial plan', 'feat: add c'],
    );
  } finally {
    rmSync(cwd, { recursive: true, force: true });
  }
});

test('hasFileChanges is false for an --allow-empty commit and true for a real one', () => {
  const { cwd, base, head } = makeRepo();
  try {
    const commits = listRangeCommits(base, head, { cwd });
    const bySubject = Object.fromEntries(commits.map((c) => [c.message, c.sha]));
    assert.equal(hasFileChanges(bySubject['Initial plan'], { cwd }), false);
    assert.equal(hasFileChanges(bySubject['fix: fix b'], { cwd }), true);
    assert.equal(hasFileChanges(bySubject['feat: add c'], { cwd }), true);
  } finally {
    rmSync(cwd, { recursive: true, force: true });
  }
});

test('selectLintableCommits filters out only the empty commit', () => {
  const { cwd, base, head } = makeRepo();
  try {
    const { lintable, skipped } = selectLintableCommits(base, head, { cwd });
    assert.deepEqual(
      lintable.map((c) => c.message),
      ['fix: fix b', 'feat: add c'],
    );
    assert.deepEqual(
      skipped.map((c) => c.message),
      ['Initial plan'],
    );
  } finally {
    rmSync(cwd, { recursive: true, force: true });
  }
});

test('run() passes a range containing only the placeholder commit and valid commits', () => {
  const { cwd, base, head } = makeRepo();
  try {
    const status = run({ baseSha: base, headSha: head, cwd, lintOne: fakeLintOne });
    assert.equal(status, 0);
  } finally {
    rmSync(cwd, { recursive: true, force: true });
  }
});

test('run() still fails when a genuinely bad NON-empty commit sits beside the empty one', () => {
  const { cwd, base, head } = makeRepo();
  try {
    const badHead = commit(cwd, 'd.txt', 'd\n', 'this is not conventional');
    const status = run({ baseSha: base, headSha: badHead, cwd, lintOne: fakeLintOne });
    assert.equal(status, 1, 'a real bad commit must still fail the gate');
    void head; // the good head is unused in this scenario; kept for clarity
  } finally {
    rmSync(cwd, { recursive: true, force: true });
  }
});

test('run() fails on a non-empty commit whose subject happens to read "Initial plan"', () => {
  // Proves the filter is diff-based, not text-based: an empty-diff commit
  // and a real commit that merely SHARES the placeholder's subject line must
  // be treated differently.
  const cwd = mkdtempSync(path.join(tmpdir(), 'commitlint-range-'));
  try {
    git(cwd, ['init', '-q', '--initial-branch=main']);
    git(cwd, ['config', 'user.name', 'test']);
    git(cwd, ['config', 'user.email', 'test@example.com']);
    writeFileSync(path.join(cwd, 'a.txt'), 'a\n');
    git(cwd, ['add', 'a.txt']);
    git(cwd, ['commit', '-q', '-m', 'feat: add a']);
    const base = git(cwd, ['rev-parse', 'HEAD']).trim();

    const head = commit(cwd, 'b.txt', 'b\n', 'Initial plan');

    const status = run({ baseSha: base, headSha: head, cwd, lintOne: fakeLintOne });
    assert.equal(status, 1, 'a real commit must be linted even if its subject reads "Initial plan"');
  } finally {
    rmSync(cwd, { recursive: true, force: true });
  }
});

test('run() passes a range with no lintable commits (every commit is empty)', () => {
  const cwd = mkdtempSync(path.join(tmpdir(), 'commitlint-range-'));
  try {
    git(cwd, ['init', '-q', '--initial-branch=main']);
    git(cwd, ['config', 'user.name', 'test']);
    git(cwd, ['config', 'user.email', 'test@example.com']);
    writeFileSync(path.join(cwd, 'a.txt'), 'a\n');
    git(cwd, ['add', 'a.txt']);
    git(cwd, ['commit', '-q', '-m', 'feat: add a']);
    const base = git(cwd, ['rev-parse', 'HEAD']).trim();
    git(cwd, ['commit', '-q', '--allow-empty', '-m', 'Initial plan']);
    const head = git(cwd, ['rev-parse', 'HEAD']).trim();

    const status = run({ baseSha: base, headSha: head, cwd, lintOne: fakeLintOne });
    assert.equal(status, 0);
  } finally {
    rmSync(cwd, { recursive: true, force: true });
  }
});

test('run() fails, not vacuously passes, on an unresolvable range', () => {
  const { cwd, base } = makeRepo();
  try {
    const status = run({ baseSha: base, headSha: 'not-a-real-ref', cwd, lintOne: fakeLintOne });
    assert.equal(status, 1);
  } finally {
    rmSync(cwd, { recursive: true, force: true });
  }
});

test('run() fails when BASE_SHA or HEAD_SHA is missing rather than defaulting silently', () => {
  const status = run({ baseSha: '', headSha: 'HEAD', cwd: process.cwd(), lintOne: fakeLintOne });
  assert.equal(status, 1);
});
