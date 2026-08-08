// Unit tests for lib/gh.mjs — the module every other script in here runs its
// subprocesses through.
//
// The decision pinned here is capture()'s option contract. capture() does not
// spread its `opts` into spawnSync; it names the keys it forwards. That is the
// right shape — it keeps the surface small — but it made an unsupported key a
// SILENT no-op, and the failure that produced was invisible: a caller passing
// `{ shell: true }` got its whole command string handed to spawnSync as argv[0],
// which never resolves, so the gate reading that status saw a plain non-zero
// exit and concluded the command had answered "no". The brew formula-to-cask
// migration check ran that way and carried zero signal until an audit read it.
// Rejecting unknown keys turns a permanently-wrong gate into a first-run crash.

import assert from 'node:assert/strict';
import test from 'node:test';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { assertSafeArg, capture, lsFiles } from './lib/gh.mjs';

test('capture forwards the options it documents', () => {
  const res = capture('sh', ['-c', 'printf "%s" "$MARKER"'], {
    env: { MARKER: 'forwarded' },
    timeout: 30_000,
  });
  assert.equal(res.status, 0);
  assert.equal(res.stdout, 'forwarded');
});

test('capture rejects an option it would otherwise drop', () => {
  // `shell` is the one that actually bit us; the assertion is about the class,
  // so an arbitrary key has to be rejected the same way.
  for (const opts of [{ shell: true }, { stdio: 'inherit' }, { cwd: '.', shell: false }]) {
    assert.throws(
      () => capture('true', [], opts),
      (err) => err instanceof TypeError && /not forwarded to spawnSync/.test(err.message),
      `capture() must reject ${JSON.stringify(opts)} rather than silently ignore it`,
    );
  }
});

test('capture still accepts an empty options object', () => {
  // The guard must not turn the common no-options call into a throw.
  const res = capture('sh', ['-c', 'exit 3']);
  assert.equal(res.status, 3);
});

test('capture reports an unspawnable command as a status, not a throw', () => {
  // The whole point of capture() is that the caller decides what failure means,
  // so a missing binary must come back as data. This is also what the `shell`
  // bug looked like from the outside — which is why the guard above exists.
  const res = capture('cerberus-no-such-binary-4f3a', []);
  assert.notEqual(res.status, 0);
  assert.match(res.stderr, /ENOENT|not found/i);
});

// assertSafeArg — the guard callers reading an env-var override (a package
// path, a git ref) put between process.env and a capture()/exec() argument
// (CodeQL js/indirect-command-line-injection). capture() never invokes a
// shell, so a leading `-` being parsed as a FLAG by the invoked binary is the
// risk this closes, not shell metacharacter injection.
test('assertSafeArg passes through an ordinary value unchanged', () => {
  assert.equal(assertSafeArg('./cmd/cerberus', 'PACKAGE'), './cmd/cerberus');
  assert.equal(assertSafeArg('a1b2c3d4', 'REV'), 'a1b2c3d4');
});

test('assertSafeArg rejects a value that could be parsed as a flag', () => {
  assert.throws(
    () => assertSafeArg('--upload-pack=/malicious', 'REV'),
    (err) => err instanceof TypeError && /starts with "-"/.test(err.message) && err.message.includes('REV'),
  );
  assert.throws(() => assertSafeArg('-x', 'PACKAGE'), TypeError);
});

test('assertSafeArg ignores non-string values (the common `undefined` no-override case)', () => {
  assert.equal(assertSafeArg(undefined, 'REV'), undefined);
});

// lsFiles() — issue #1938. A plain `git ls-files` reads the INDEX only, so a
// file a generator just wrote but nobody `git add`-ed yet was invisible to
// every forbid-skip.mjs scan (all of them route through this function): the
// scan reported clean on content it would reject the moment it was staged.
// `newRepo()` builds a throwaway git repository so the assertion is against
// the real CLI, not a mock.
function newRepo() {
  const dir = mkdtempSync(join(tmpdir(), 'gh-lsfiles-'));
  const run = (args) => {
    const res = spawnSync('git', args, { cwd: dir, encoding: 'utf8' });
    assert.equal(res.status, 0, `git ${args.join(' ')} failed: ${res.stderr}`);
  };
  run(['init', '--quiet']);
  writeFileSync(join(dir, 'tracked.go'), 'package main\n');
  run(['add', 'tracked.go']);
  run(['-c', 'user.email=a@a', '-c', 'user.name=a', 'commit', '--quiet', '-m', 'seed']);
  return { dir, run };
}

test('lsFiles includes an untracked-but-not-ignored file, not just the git index', () => {
  const { dir } = newRepo();
  // A generator wrote this file but it was never `git add`-ed.
  writeFileSync(join(dir, 'generated_test.go'), 'package main\n\nfunc TestFoo() {}\n');
  const files = lsFiles(['*.go'], { cwd: dir });
  assert.ok(
    files.includes('generated_test.go'),
    `expected the untracked file among ${JSON.stringify(files)}`,
  );
  assert.ok(files.includes('tracked.go'));
});

test('lsFiles still excludes a .gitignored file', () => {
  const { dir, run } = newRepo();
  writeFileSync(join(dir, '.gitignore'), 'ignored.go\n');
  run(['add', '.gitignore']);
  run(['-c', 'user.email=a@a', '-c', 'user.name=a', 'commit', '--quiet', '-m', 'gitignore']);
  writeFileSync(join(dir, 'ignored.go'), 'package main\n');
  const files = lsFiles(['*.go'], { cwd: dir });
  assert.ok(!files.includes('ignored.go'), `.gitignore-d file leaked into ${JSON.stringify(files)}`);
});

test('lsFiles honours an exclude pathspec against an untracked file, same as a tracked one', () => {
  const { dir } = newRepo();
  // Deliberately left untracked (never `git add`-ed) — the exclude pathspec
  // must still apply to it, not just to indexed paths.
  const upstream = join(dir, 'compatibility', 'promql', 'upstream');
  spawnSync('mkdir', ['-p', upstream]);
  writeFileSync(join(upstream, 'vendored_test.go'), 'package main\n');
  const files = lsFiles(['*_test.go', ':!:compatibility/*/upstream/**'], { cwd: dir });
  assert.ok(
    !files.some((f) => f.includes('upstream')),
    `exclude pathspec did not apply to an untracked path: ${JSON.stringify(files)}`,
  );
});
