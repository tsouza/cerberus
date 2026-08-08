// forbid-skip.test.mjs — node:test guard for the GA test-discipline gate's
// CLI, run end-to-end against a real throwaway git repository.
//
// forbid-skip.mjs has no importable pure functions (unlike repo-hygiene.mjs):
// every scan is a closure over `process.env.CHECK` and the module runs its
// dispatch as a side effect of being loaded, so the only way to exercise it
// is to spawn the real CLI as a subprocess — exactly as
// repo-hygiene.test.mjs already does for its own end-to-end assertions.
//
// The case this file exists to pin (#1938): every scan derives its file set
// from lib/gh.mjs's lsFiles(), which used to run a bare `git ls-files` — the
// git INDEX only. A file a generator just wrote but never `git add`-ed was
// therefore invisible: the gate reported clean on content it would reject
// the instant the file was staged. lsFiles() now also reads
// `--others --exclude-standard`, so an untracked-but-not-ignored violation
// is caught too, while a `.gitignore`d path stays out of scope exactly as
// before. gh.test.mjs pins lsFiles() itself directly; this file pins the
// same gap from the CLI's own vantage point, against the actual scan a
// contributor runs locally after CI reds (CLAUDE.md's own narrowed-local-
// repro rule).

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const CLI = join(HERE, 'forbid-skip.mjs');

// newFixtureRepo — a throwaway git repo with one committed, clean Go file so
// `git log` / `git status` behave normally. Returns a `write(relPath,
// content)` helper that writes a file WITHOUT staging it — the point of
// every "untracked" case below is that the file is never `git add`-ed.
function newFixtureRepo() {
  const dir = mkdtempSync(join(tmpdir(), 'forbid-skip-'));
  const run = (args) => {
    const res = spawnSync('git', args, { cwd: dir, encoding: 'utf8' });
    assert.equal(res.status, 0, `git ${args.join(' ')} failed: ${res.stderr}`);
  };
  run(['init', '--quiet']);
  writeFileSync(join(dir, 'ok_test.go'), 'package main\n\nfunc TestOK(t *testing.T) {}\n');
  run(['add', 'ok_test.go']);
  run(['-c', 'user.email=a@a', '-c', 'user.name=a', 'commit', '--quiet', '-m', 'seed']);
  return {
    dir,
    write(relPath, content) {
      mkdirSync(join(dir, dirname(relPath)), { recursive: true });
      writeFileSync(join(dir, relPath), content);
    },
  };
}

function runGate(check, cwd) {
  const res = spawnSync(process.execPath, [CLI], {
    cwd,
    encoding: 'utf8',
    env: { ...process.env, CHECK: check },
  });
  return { status: res.status, out: `${res.stdout}${res.stderr}` };
}

test('the CLI passes CHECK=t-skip on a clean fixture tree', () => {
  const { dir } = newFixtureRepo();
  const { status, out } = runGate('t-skip', dir);
  assert.equal(status, 0, `t-skip must pass on a clean tree; got:\n${out}`);
});

test('the CLI FAILS CHECK=t-skip on an UNTRACKED violating file, naming it (#1938)', () => {
  const { dir, write } = newFixtureRepo();
  // Deliberately not `git add`-ed — this is the exact gap #1938 describes: a
  // generator wrote the file but nobody staged it yet.
  write('generated_test.go', 'package main\n\nfunc TestBad(t *testing.T) { t.Skip("flaky") }\n');
  const { status, out } = runGate('t-skip', dir);
  assert.notEqual(
    status,
    0,
    `t-skip must fail on an untracked t.Skip file; a bare \`git ls-files\` regression would exit 0 here:\n${out}`,
  );
  assert.match(out, /::error::/);
  assert.match(out, /generated_test\.go/);
});

test('the CLI still ignores an untracked violating file under the upstream exclude', () => {
  const { dir, write } = newFixtureRepo();
  write(
    'compatibility/promql/upstream/vendored_test.go',
    'package main\n\nfunc TestVendored(t *testing.T) { t.Skip("upstream") }\n',
  );
  const { status, out } = runGate('t-skip', dir);
  assert.equal(status, 0, `the upstream exclude pathspec must still apply to an untracked path; got:\n${out}`);
});

test('the CLI FAILS CHECK=t-skip on a TRACKED violating file, same as before', () => {
  const { dir, write } = newFixtureRepo();
  write('tracked_test.go', 'package main\n\nfunc TestBad(t *testing.T) { t.Skip("flaky") }\n');
  const add = spawnSync('git', ['add', 'tracked_test.go'], { cwd: dir, encoding: 'utf8' });
  assert.equal(add.status, 0, add.stderr);
  const { status, out } = runGate('t-skip', dir);
  assert.notEqual(status, 0, `t-skip must still fail on a tracked t.Skip file; got:\n${out}`);
  assert.match(out, /tracked_test\.go/);
});

test('the CLI rejects an unknown CHECK rather than passing silently', () => {
  const { dir } = newFixtureRepo();
  const { status, out } = runGate('', dir);
  assert.notEqual(status, 0);
  assert.match(out, /unknown CHECK/);
});
