// compose-grafana-smoke.test.mjs — node:test guard for
// compose-grafana-smoke.mjs's install-decision logic, in particular the
// npm-ci-failure regression this extraction exists to fix (CLAUDE.md
// invariant 15, issue #3098, epic #3091).
//
// The regression under test: the replaced Justfile idiom
// `( [ -f package-lock.json ] && npm ci || npm install ... )` silently fell
// back to `npm install` on ANY `npm ci` failure, not just a missing
// lockfile. `runNpmCiFailureStub` below simulates exactly that failure (a
// non-zero `npm ci` exit with a lockfile present) and asserts two things the
// old bash could not guarantee: (1) `installDeps` reports failure rather
// than swallowing it, and (2) the install step is invoked exactly once —
// proving no second, fallback command ever runs. A test written against the
// OLD masking behavior would see `ok: true` here; this one would fail
// against that behavior, which is the point.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { decideInstallCommand, installDeps, isRegularFile } from './compose-grafana-smoke.mjs';

test('a present lockfile decides npm ci', () => {
  assert.deepEqual(decideInstallCommand(true), { cmd: 'npm', args: ['ci'] });
});

test('a missing lockfile decides npm install --no-audit --no-fund', () => {
  assert.deepEqual(decideInstallCommand(false), { cmd: 'npm', args: ['install', '--no-audit', '--no-fund'] });
});

test('a failed npm ci is reported as a hard failure, never falls back to npm install', () => {
  const calls = [];
  const runFn = (cmd, args) => {
    calls.push({ cmd, args });
    // Simulate exactly the bug scenario: package-lock.json exists, but `npm
    // ci` itself fails (corrupted/out-of-sync lockfile, integrity error) —
    // NOT the ENOENT/missing-lockfile case the fallback was meant for.
    return { ok: false, status: 1 };
  };

  const result = installDeps('test/e2e/playwright', { lockfileExists: true, runFn });

  assert.equal(result.ok, false, 'a failed npm ci must be reported as a failure');
  assert.equal(result.status, 1);
  assert.equal(calls.length, 1, 'npm ci failing must never trigger a second, fallback install command');
  assert.deepEqual(calls[0], { cmd: 'npm', args: ['ci'] });
});

test('a successful npm ci is reported as success with no install fallback attempted', () => {
  const calls = [];
  const runFn = (cmd, args) => {
    calls.push({ cmd, args });
    return { ok: true, status: 0 };
  };

  const result = installDeps('test/e2e/playwright', { lockfileExists: true, runFn });

  assert.equal(result.ok, true);
  assert.equal(calls.length, 1);
  assert.deepEqual(calls[0], { cmd: 'npm', args: ['ci'] });
});

test('a missing lockfile runs npm install, never npm ci', () => {
  const calls = [];
  const runFn = (cmd, args) => {
    calls.push({ cmd, args });
    return { ok: true, status: 0 };
  };

  const result = installDeps('test/e2e/playwright', { lockfileExists: false, runFn });

  assert.equal(result.ok, true);
  assert.equal(calls.length, 1);
  assert.deepEqual(calls[0], { cmd: 'npm', args: ['install', '--no-audit', '--no-fund'] });
});

test('isRegularFile is false for a path that does not exist', () => {
  const dir = mkdtempSync(join(tmpdir(), 'compose-grafana-smoke-test-'));
  try {
    assert.equal(isRegularFile(join(dir, 'missing-lock.json')), false);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('isRegularFile is true for an actual regular file', () => {
  const dir = mkdtempSync(join(tmpdir(), 'compose-grafana-smoke-test-'));
  try {
    const file = join(dir, 'package-lock.json');
    writeFileSync(file, '{}');
    assert.equal(isRegularFile(file), true);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('isRegularFile is false for a directory — matches bash `[ -f ]`, not a bare existence check', () => {
  const dir = mkdtempSync(join(tmpdir(), 'compose-grafana-smoke-test-'));
  try {
    assert.equal(isRegularFile(dir), false);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
