// e2e-seed-rolling.test.mjs — node:test guard for `initialSeedLanded`, the
// exact `grep -q '^.*seed: done'` marker check the extracted bash used to
// decide the rolling seeder's initial insert had landed (cerberus issue
// #3096). The port-forward-supervisor spawn, `go build`, and detached
// seeder launch around it are exercised live by `just e2e-seed-rolling` in
// the real `e2e` CI job, which is the only place a real ClickHouse Service
// exists to seed.

import assert from 'node:assert/strict';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';

import { initialSeedLanded } from './e2e-seed-rolling.mjs';

test('initialSeedLanded is false for a log file that does not exist yet', () => {
  assert.equal(initialSeedLanded('/tmp/does-not-exist-cerberus-e2e-seed-rolling.log'), false);
});

test('initialSeedLanded is false while the log has no "seed: done" line yet', () => {
  const dir = mkdtempSync(join(tmpdir(), 'cerberus-seed-rolling-test-'));
  const logPath = join(dir, 'rolling.log');
  writeFileSync(logPath, 'seed: applying DDL\nseed: inserting fixtures\n');
  try {
    assert.equal(initialSeedLanded(logPath), false);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('initialSeedLanded is true once the log carries the "seed: done" marker', () => {
  const dir = mkdtempSync(join(tmpdir(), 'cerberus-seed-rolling-test-'));
  const logPath = join(dir, 'rolling.log');
  writeFileSync(logPath, 'seed: applying DDL\nseed: inserting fixtures\n2026-09-06T00:00:00Z seed: done\n');
  try {
    assert.equal(initialSeedLanded(logPath), true);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
