// coverage-package-floor.test.mjs — unit and hermetic integration pins for the
// cheap PR coverage-enrollment gate.

import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';

import { compareEnrollment, coverBlockCount, mergePackageLanes, parsePackageRows } from './coverage-package-floor.mjs';
import { packageKeySegments } from './coverage-summary.mjs';
import { writeShardedMap } from './lib/sharded-json.mjs';

const SCRIPT = fileURLToPath(new URL('./coverage-package-floor.mjs', import.meta.url));
const FILE_SEPARATOR = '\u001f';

test('go-list rows retain active Go and cgo source files under module-relative package names', () => {
  const row = [
    'example.com/project/internal/a',
    'example.com/project',
    '/src/internal/a',
    `a.go${FILE_SEPARATOR}platform.go`,
    'cgo.go',
  ].join('\t');
  assert.deepEqual(parsePackageRows(`${row}\n`), {
    packages: [
      {
        name: 'internal/a',
        dir: '/src/internal/a',
        files: ['a.go', 'platform.go', 'cgo.go'],
      },
    ],
  });
});

test('cover output distinguishes declaration-only files from statement-carrying files', () => {
  assert.deepEqual(coverBlockCount('var GoCover = struct { NumStmt: [0]uint16 }{}'), { count: 0 });
  assert.deepEqual(coverBlockCount('var GoCover = struct { NumStmt: [17]uint16 }{}'), { count: 17 });
  assert.match(coverBlockCount('package p').err, /did not declare NumStmt/);
});

test('enrollment reports missing and non-positive floors but accepts a positive floor', () => {
  assert.deepEqual(compareEnrollment(['internal/ok', 'internal/missing', 'internal/zero'], {
    'internal/ok': 72.3,
    'internal/stale': 40,
    'internal/zero': 0,
  }), {
    missing: ['internal/missing'],
    nonPositive: ['internal/zero'],
    stale: ['internal/stale'],
  });
});

test('default and tagged lanes union their selected implementation files', () => {
  assert.deepEqual(mergePackageLanes([
    [{ name: 'internal/session', dir: '/src/internal/session', files: ['close_nochdb.go', 'doc.go'] }],
    [{ name: 'internal/session', dir: '/src/internal/session', files: ['close_chdb.go', 'doc.go'] }],
  ]), {
    packages: [{
      name: 'internal/session',
      dir: '/src/internal/session',
      files: ['close_chdb.go', 'close_nochdb.go', 'doc.go'],
    }],
  });
});

function fixture() {
  const dir = mkdtempSync(path.join(tmpdir(), 'coverage-package-floor-'));
  mkdirSync(path.join(dir, 'statement'));
  mkdirSync(path.join(dir, 'declarations'));
  writeFileSync(path.join(dir, 'go.mod'), 'module example.com/coveragefixture\n\ngo 1.25.0\n');
  writeFileSync(path.join(dir, 'statement', 'statement.go'), 'package statement\n\nfunc Answer() int { return 42 }\n');
  writeFileSync(path.join(dir, 'declarations', 'declarations.go'), 'package declarations\n\nconst Answer = 42\n');
  return dir;
}

function run(dir, floors) {
  const floorsDir = path.join(dir, 'floors');
  writeShardedMap(floorsDir, floors, packageKeySegments);
  const result = spawnSync(process.execPath, [SCRIPT], {
    cwd: dir,
    encoding: 'utf8',
    env: { ...process.env, COVERAGE_FLOORS: floorsDir, COVERAGE_PACKAGE_PATTERN: './...' },
  });
  return { status: result.status, output: `${result.stdout}${result.stderr}` };
}

test('end to end: statement packages require a positive floor while declaration-only packages do not', () => {
  const dir = fixture();
  try {
    const missing = run(dir, {});
    assert.equal(missing.status, 1, missing.output);
    assert.match(missing.output, /statement-carrying package/);
    assert.match(missing.output, /statement/);
    assert.doesNotMatch(missing.output, /declarations/);

    const zero = run(dir, { statement: 0 });
    assert.equal(zero.status, 1, zero.output);
    assert.match(zero.output, /non-positive/);

    const enrolled = run(dir, { statement: 50 });
    assert.equal(enrolled.status, 0, enrolled.output);
    assert.match(enrolled.output, /1 statement-carrying full-lane package\(s\) enrolled/);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
