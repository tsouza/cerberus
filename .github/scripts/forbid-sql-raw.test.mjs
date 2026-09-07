// Unit + integration tests for forbid-sql-raw.mjs.
//
// The unit half pins RAW_WRITE_PATTERN and KNOWN_GOOD against positive and
// negative fixtures — the exact token shapes the gate exists to catch, and
// the neighbouring shapes (a typed Frag call, a Go `+` on a non-SQL string,
// a fmt.Sprintf) it must NOT catch, because the header says it does not and
// a gate whose header and regex disagree is the finding that produced this
// file. The integration half spins up a real temp git repo shaped like
// internal/chsql/ (a FLAT directory, no subpackages) and runs the actual
// script against it via subprocess — deliberately, not belt-and-braces: a
// bare `**` pathspec silently matches zero files against exactly this
// flat-directory shape without git's `glob` pathspec magic, which is the bug
// that let this script pass "clean" while scanning nothing (#2321). A unit
// test of the regex alone would never have caught that class of bug; only a
// real `git ls-files` invocation against a real flat directory does.

import assert from 'node:assert/strict';
import test from 'node:test';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, writeFileSync, mkdirSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

import { KNOWN_GOOD, RAW_WRITE_PATTERN, SCAN_PATHSPEC, scanFiles } from './forbid-sql-raw.mjs';

const SCRIPT = join(dirname(fileURLToPath(import.meta.url)), 'forbid-sql-raw.mjs');

// --- the pattern ---------------------------------------------------------------

test('RAW_WRITE_PATTERN matches every raw-write primitive shape', () => {
  for (const line of [
    'var b strings.Builder',
    '\te.b = strings.Builder{}',
    'func (b *Builder) writeSQL(s string) {',
    '\tb.writeSQL("SELECT ")',
    '\twriteSQL(b, " FROM ")',
    '\tb.sb.WriteString(sql)',
    '\tb.sb.WriteByte(\')\')',
    '\tx.writeSQL (fragment)',
  ]) {
    assert.ok(RAW_WRITE_PATTERN.test(line), `must match: ${line}`);
  }
});

test('RAW_WRITE_PATTERN leaves the typed API and non-SQL formatting alone', () => {
  // The header states fmt.Sprintf and `+`-concatenation are reviewer-caught,
  // not gate-caught: a fixture that matched them would make that statement
  // false, and a fixture that fails to match them pins it.
  for (const line of [
    '\treturn Call("toStartOfInterval", ts, InlineLit(step))',
    '\treturn And(Eq(col, lit), Gt(ts, start))',
    '\tmsg := fmt.Sprintf("unsupported op %q", op)',
    '\tname := prefix + "_" + suffix',
    '\tvar sb strings.Reader',
    '\tstrings.Join(parts, ", ")',
    '\t// writes go through the typed builder',
    '\tsb.WriteString(x) // a local named sb, not the Builder field',
  ]) {
    assert.ok(!RAW_WRITE_PATTERN.test(line), `must not match: ${line}`);
  }
});

// --- the inventory -------------------------------------------------------------

test('KNOWN_GOOD is exactly the two emitter files, and builder.go is excluded by pathspec', () => {
  assert.deepEqual([...KNOWN_GOOD].sort(), ['internal/chsql/emit.go', 'internal/chsql/emit_node.go']);
  // builder.go is where the primitives are DEFINED, so it is carved out of
  // the scan rather than listed as a consumer exemption.
  assert.ok(SCAN_PATHSPEC.some((p) => p.includes('!:internal/chsql/builder.go')));
  assert.ok(!KNOWN_GOOD.has('internal/chsql/builder.go'));
});

test('the pathspec is chsql-scoped and glob-magic', () => {
  // A chsql-scoped SUBSET of invariant 10 (the header's SCOPE section) —
  // widening is a separate design decision, so the scope is pinned.
  assert.ok(SCAN_PATHSPEC.every((p) => p.includes('internal/chsql/')));
  assert.ok(SCAN_PATHSPEC.some((p) => p.startsWith(':(glob)')), 'a bare ** matches zero files in a flat dir (#2321)');
});

// --- scanFiles -----------------------------------------------------------------

test('scanFiles reports each offending line in a file outside KNOWN_GOOD', () => {
  const fixtures = {
    'internal/chsql/new_emitter.go': ['package chsql', '', 'var b strings.Builder', '', 'func f() { b.writeSQL("x") }'].join('\n'),
    'internal/chsql/clean.go': ['package chsql', '', 'func g() Frag { return Call("now") }'].join('\n'),
  };
  const found = scanFiles(Object.keys(fixtures), (f) => fixtures[f]);
  assert.deepEqual(
    found.map((v) => [v.file, v.line]),
    [
      ['internal/chsql/new_emitter.go', 3],
      ['internal/chsql/new_emitter.go', 5],
    ],
  );
});

test('scanFiles skips KNOWN_GOOD files but never a file merely NEAR them', () => {
  const fixtures = {
    'internal/chsql/emit.go': 'var b strings.Builder',
    'internal/chsql/emit_node.go': 'b.sb.WriteString(sql)',
    'internal/chsql/emit_extra.go': 'var b strings.Builder',
  };
  const found = scanFiles(Object.keys(fixtures), (f) => fixtures[f]);
  assert.deepEqual(found.map((v) => v.file), ['internal/chsql/emit_extra.go']);
});

test('an unreadable file is a violation, not a silent skip', () => {
  const found = scanFiles(['internal/chsql/ghost.go'], () => {
    throw new Error('ENOENT');
  });
  assert.equal(found.length, 1);
  assert.equal(found[0].line, 0);
  assert.match(found[0].reason, /cannot read/);
});

// --- integration: the real script against a real flat git tree ----------------

// newChsqlRepo() builds a throwaway git repo with an internal/chsql/
// directory containing the given files (path -> content), commits them,
// and returns the repo dir. Mirrors internal/chsql/'s real shape: flat,
// no subpackages.
function newChsqlRepo(files) {
  const dir = mkdtempSync(join(tmpdir(), 'forbid-sql-raw-test-'));
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
  return dir;
}

function runScript(cwd) {
  return spawnSync(process.execPath, [SCRIPT], { cwd, encoding: 'utf8' });
}

test('a clean flat chsql tree passes and reports a non-zero file count', () => {
  const dir = newChsqlRepo({
    'internal/chsql/builder.go': 'package chsql\n\nvar b strings.Builder // the primitive layer, excluded by pathspec\n',
    'internal/chsql/emit.go': 'package chsql\n\ntype emitter struct{ b strings.Builder }\n',
    'internal/chsql/ok.go': 'package chsql\n\nfunc f() Frag { return Call("now") }\n',
    'internal/chsql/ok_test.go': 'package chsql\n\nvar sb strings.Builder // tests are out of scope\n',
  });
  const res = runScript(dir);
  assert.equal(res.status, 0, res.stdout + res.stderr);
  assert.match(res.stdout, /scanning 2 file\(s\)/, 'ok.go + emit.go; builder.go and the test file are excluded');
  assert.match(res.stdout, /no raw SQL token writes/);
});

test('a raw write in a new chsql file fails the scan with its file:line', () => {
  const dir = newChsqlRepo({
    'internal/chsql/ok.go': 'package chsql\n',
    'internal/chsql/rogue.go': 'package chsql\n\nfunc f() { b.writeSQL("SELECT 1") }\n',
  });
  const res = runScript(dir);
  assert.equal(res.status, 1);
  assert.match(res.stdout, /internal\/chsql\/rogue\.go:3: raw SQL token write/);
  assert.match(res.stdout, /1 raw-write violation\(s\)/);
});

test('a tree the pathspec cannot see fails loudly instead of passing vacuously', () => {
  // No internal/chsql/ at all: zero files must be a hard failure (#2321).
  const dir = newChsqlRepo({ 'README.md': 'nothing to scan\n' });
  const res = runScript(dir);
  assert.equal(res.status, 1);
  assert.match(res.stdout, /matched zero files/);
});
