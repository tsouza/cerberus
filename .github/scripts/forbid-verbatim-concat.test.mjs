// Unit + integration tests for forbid-verbatim-concat.mjs.
//
// The integration tests spin up a real temp git repo shaped like
// internal/chsql/ (a FLAT directory, no subpackages) and run the actual
// script against it via subprocess — this is deliberate, not just belt-and-
// braces: a bare `**` pathspec silently matches zero files against exactly
// this flat-directory shape without git's `glob` pathspec magic, which is
// exactly the bug that let both this script and forbid-sql-raw.mjs pass
// "clean" while scanning nothing (#2321's discovery). A unit test of the
// concat-detection logic alone would never have caught that class of bug;
// only a real `git ls-files` invocation against a real flat directory does.

import assert from 'node:assert/strict';
import test from 'node:test';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, writeFileSync, mkdirSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const SCRIPT = join(dirname(fileURLToPath(import.meta.url)), 'forbid-verbatim-concat.mjs');

// newChsqlRepo() builds a throwaway git repo with an internal/chsql/
// directory containing the given files (path -> content), commits them,
// and returns the repo dir. Mirrors internal/chsql/'s real shape: flat,
// no subpackages.
function newChsqlRepo(files) {
  const dir = mkdtempSync(join(tmpdir(), 'forbid-verbatim-concat-test-'));
  const run = (args) => spawnSync('git', args, { cwd: dir, encoding: 'utf8' });
  run(['init', '-q']);
  run(['config', 'user.email', 'a@a']);
  run(['config', 'user.name', 'a']);
  mkdirSync(join(dir, 'internal', 'chsql'), { recursive: true });
  for (const [rel, content] of Object.entries(files)) {
    const full = join(dir, rel);
    mkdirSync(dirname(full), { recursive: true });
    writeFileSync(full, content);
  }
  run(['add', '-A']);
  run(['commit', '-q', '-m', 'seed']);
  return { dir, run };
}

function runGate(dir) {
  return spawnSync('node', [SCRIPT], { cwd: dir, encoding: 'utf8' });
}

test('exits non-zero and reports the real file count against a flat internal/chsql/ directory', () => {
  const { dir } = newChsqlRepo({
    'internal/chsql/builder.go': 'package chsql\n\nfunc verbatim(sql string) Frag { return nil }\n',
    'internal/chsql/emit.go': 'package chsql\n\nfunc alias() Frag {\n\treturn verbatim("anchor_ts")\n}\n',
  });
  const res = runGate(dir);
  // A flat internal/chsql/ (no subpackages) is exactly the shape that a
  // bare, non-glob-magic `**` pathspec silently matches ZERO files
  // against. The zero-file guard in the script itself would fail loudly
  // in that case; here we assert the POSITIVE case — it actually finds
  // and scans the one non-builder.go file.
  assert.match(res.stdout, /scanning 1 file\(s\)/, `expected to scan emit.go, got: ${res.stdout}`);
  assert.equal(res.status, 0, `expected clean exit, got status=${res.status}: ${res.stdout}`);
});

test('flags a verbatim() call built via string concatenation', () => {
  const { dir } = newChsqlRepo({
    'internal/chsql/builder.go': 'package chsql\n\nfunc verbatim(sql string) Frag { return nil }\n',
    'internal/chsql/shape.go':
      'package chsql\n\nfunc bad(col string) Frag {\n\treturn verbatim("sum(" + col + ") OVER (PARTITION BY x)")\n}\n',
  });
  const res = runGate(dir);
  assert.equal(res.status, 1, `expected a violation, got status=${res.status}: ${res.stdout}`);
  assert.match(res.stdout, /shape\.go:4/, `expected the violation at shape.go:4, got: ${res.stdout}`);
});

test('does not flag a `+` that appears only inside a string literal', () => {
  const { dir } = newChsqlRepo({
    'internal/chsql/builder.go': 'package chsql\n\nfunc verbatim(sql string) Frag { return nil }\n',
    'internal/chsql/literal.go':
      'package chsql\n\nfunc literal() Frag {\n\treturn verbatim("1 + 1")\n}\n',
  });
  const res = runGate(dir);
  assert.equal(res.status, 0, `a "+" inside a string literal must not trigger, got: ${res.stdout}`);
});

test('ignores a concatenating verbatim() call inside a _test.go file', () => {
  const { dir } = newChsqlRepo({
    'internal/chsql/builder.go': 'package chsql\n\nfunc verbatim(sql string) Frag { return nil }\n',
    'internal/chsql/emit.go': 'package chsql\n\nfunc alias() Frag {\n\treturn verbatim("anchor_ts")\n}\n',
    'internal/chsql/shape_test.go':
      'package chsql\n\nfunc TestBad(t *T) {\n\t_ = verbatim("a" + "b")\n}\n',
  });
  const res = runGate(dir);
  assert.equal(res.status, 0, `test files are out of scope, got: ${res.stdout}`);
});

test('ignores builder.go itself even though it defines verbatim', () => {
  const { dir } = newChsqlRepo({
    'internal/chsql/builder.go':
      'package chsql\n\nfunc verbatim(sql string) Frag { return nil }\n\nfunc self() Frag {\n\treturn verbatim("a" + "b")\n}\n',
    'internal/chsql/emit.go': 'package chsql\n\nfunc alias() Frag {\n\treturn verbatim("anchor_ts")\n}\n',
  });
  const res = runGate(dir);
  assert.equal(res.status, 0, `builder.go is excluded by design, got: ${res.stdout}`);
});

test('fails loudly rather than passing vacuously when the pathspec matches nothing', () => {
  const dir = mkdtempSync(join(tmpdir(), 'forbid-verbatim-concat-test-empty-'));
  const run = (args) => spawnSync('git', args, { cwd: dir, encoding: 'utf8' });
  run(['init', '-q']);
  run(['config', 'user.email', 'a@a']);
  run(['config', 'user.name', 'a']);
  writeFileSync(join(dir, 'README.md'), 'no chsql here\n');
  run(['add', '-A']);
  run(['commit', '-q', '-m', 'seed']);

  const res = runGate(dir);
  assert.equal(res.status, 1, `a zero-file scan must fail loudly, got: ${res.stdout}`);
  assert.match(res.stdout, /matched zero files/, `expected the zero-file guard message, got: ${res.stdout}`);
});
