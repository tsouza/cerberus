// go-mod-tidy-check.test.mjs — node:test guard for the tidy gate: module
// list parsing, and the CLI against a throwaway repository with a stub `go`
// on PATH so both the clean and the "tidy changed go.mod" outcomes run for
// real.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync, spawnSync } from 'node:child_process';
import { chmodSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { DEFAULT_MODULES, parseModules } from './go-mod-tidy-check.mjs';

const HERE = dirname(fileURLToPath(import.meta.url));
const CLI = join(HERE, 'go-mod-tidy-check.mjs');

test('parseModules defaults to the root and the nested oracle module', () => {
  assert.deepEqual(parseModules(undefined), DEFAULT_MODULES);
  assert.deepEqual(parseModules('  '), DEFAULT_MODULES);
  assert.deepEqual(parseModules('. a/b'), ['.', 'a/b']);
  assert.ok(DEFAULT_MODULES.includes('test/oracle'), 'the nested module drifts the same way and must be tidied');
});

// repoWithStubGo — a committed go.mod and a `go` whose `mod tidy` appends a
// line when MUTATE is set.
function repoWithStubGo() {
  const root = mkdtempSync(join(tmpdir(), 'tidy-check-'));
  const git = (...args) => execFileSync('git', args, { cwd: root, encoding: 'utf8' }).trim();
  git('init', '--quiet', '--initial-branch', 'main');
  git('config', 'user.email', 'a@a');
  git('config', 'user.name', 'a');
  writeFileSync(join(root, 'go.mod'), 'module example.com/x\n\ngo 1.23\n');
  git('add', 'go.mod');
  git('commit', '--quiet', '-m', 'seed');
  const bin = join(root, 'bin');
  execFileSync('mkdir', ['-p', bin]);
  writeFileSync(join(bin, 'go'), '#!/bin/sh\n[ "$1 $2" = "mod tidy" ] || exit 2\n[ -n "$MUTATE" ] && echo "require example.com/y v1.0.0" >> go.mod\nexit 0\n');
  chmodSync(join(bin, 'go'), 0o755);
  return { root, env: { PATH: `${bin}:${process.env.PATH}` } };
}

test('the CLI passes when tidy changes nothing and fails with the diff when it does', () => {
  const { root, env } = repoWithStubGo();
  try {
    const run = (extra) => spawnSync(process.execPath, [CLI], { cwd: root, encoding: 'utf8', env: { ...process.env, ...env, MODULES: '.', ...extra } });
    let res = run({ MUTATE: '' });
    assert.equal(res.status, 0, res.stderr);
    assert.match(res.stdout, /are tidy/);
    res = run({ MUTATE: '1' });
    assert.equal(res.status, 1);
    assert.match(res.stdout, /\+require example\.com\/y v1\.0\.0/);
    assert.match(res.stdout + res.stderr, /go mod tidy changed go\.mod\/go\.sum/);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});
