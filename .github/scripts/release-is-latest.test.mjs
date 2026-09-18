// release-is-latest.test.mjs — node:test guard for the "is this the newest
// stable line?" signal. The shape it exists to get right: a stable backport
// cut AFTER a newer minor is not latest, and a prerelease is never latest.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync, spawnSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { highestStable, isLatest, parseStable } from './release-is-latest.mjs';

const HERE = dirname(fileURLToPath(import.meta.url));
const CLI = join(HERE, 'release-is-latest.mjs');

test('parseStable accepts only a bare vX.Y.Z', () => {
  assert.deepEqual(parseStable('v1.20.3'), [1, 20, 3]);
  assert.equal(parseStable('v1.20.3-rc.1'), null);
  assert.equal(parseStable('chart-v0.6.4'), null);
  assert.equal(parseStable('1.20.3'), null);
});

test('highestStable orders numerically, not lexically, and ignores prereleases', () => {
  assert.equal(highestStable(['v1.9.0', 'v1.10.0', 'v1.10.0-rc.2', 'v2.0.0-rc.1']), 'v1.10.0');
  assert.equal(highestStable(['v1.2.1', 'v1.2.10', 'v1.2.9']), 'v1.2.10');
  assert.equal(highestStable(['v1.0.0-rc.1']), null);
  assert.equal(highestStable([]), null);
});

test('isLatest: a stable backport cut after a newer minor is not latest; a prerelease never is', () => {
  const tags = ['v1.11.3', 'v1.12.1', 'v1.13.0'];
  assert.equal(isLatest('v1.13.0', tags), true);
  assert.equal(isLatest('v1.12.2', tags), false, 'a backport on an older line');
  assert.equal(isLatest('v1.14.0', tags), true, 'a new minor not yet in the list is latest once cut');
  assert.equal(isLatest('v1.14.0-rc.1', tags), false, 'a prerelease is never the stable latest');
  assert.equal(isLatest('v1.0.0', []), true, 'the first stable release is latest');
});

test('the CLI writes RELEASE_IS_LATEST to $GITHUB_ENV and is_latest to $GITHUB_OUTPUT from the checkout\'s tags', () => {
  const root = mkdtempSync(join(tmpdir(), 'release-is-latest-'));
  try {
    execFileSync('git', ['init', '--quiet', '--initial-branch', 'main', root]);
    const git = (...args) => execFileSync('git', args, { cwd: root, encoding: 'utf8' }).trim();
    git('config', 'user.email', 'a@a');
    git('config', 'user.name', 'a');
    git('commit', '--quiet', '--allow-empty', '-m', 'root');
    for (const tag of ['v1.12.1', 'v1.13.0', 'v1.13.1-rc.1']) git('tag', tag);
    const envFile = join(root, 'env');
    const outFile = join(root, 'out');
    const run = (tag) =>
      spawnSync(process.execPath, [CLI], { cwd: root, encoding: 'utf8', env: { ...process.env, TAG: tag, GITHUB_ENV: envFile, GITHUB_OUTPUT: outFile } });

    let res = run('v1.13.0');
    assert.equal(res.status, 0, res.stderr);
    assert.match(readFileSync(envFile, 'utf8'), /^RELEASE_IS_LATEST=true$/m);
    assert.match(readFileSync(outFile, 'utf8'), /^is_latest=true$/m);

    res = run('v1.12.1');
    assert.equal(res.status, 0, res.stderr);
    assert.match(readFileSync(envFile, 'utf8'), /RELEASE_IS_LATEST=false$/m);
    assert.match(res.stdout, /highest_stable=v1\.13\.0/);

    res = run('');
    assert.equal(res.status, 1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});
