// release-tag.test.mjs — node:test guard for the release tag cut, driven
// against a real throwaway repository with a bare remote. release.yml has no
// pull_request trigger, so this is the only time the tag logic runs before a
// release is actually being cut.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync, spawnSync } from 'node:child_process';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { ensureReleaseTag, tagCommit } from './release-tag.mjs';

const HERE = dirname(fileURLToPath(import.meta.url));
const CLI = join(HERE, 'release-tag.mjs');

function git(cwd, ...args) {
  return execFileSync('git', args, { cwd, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] }).trim();
}

// newRepoWithRemote — a work tree with two commits and a bare `origin`.
function newRepoWithRemote() {
  const root = mkdtempSync(join(tmpdir(), 'release-tag-'));
  const remote = join(root, 'origin.git');
  const work = join(root, 'work');
  execFileSync('git', ['init', '--quiet', '--bare', remote]);
  execFileSync('git', ['init', '--quiet', '--initial-branch', 'main', work]);
  git(work, 'config', 'user.email', 'a@a');
  git(work, 'config', 'user.name', 'a');
  git(work, 'commit', '--quiet', '--allow-empty', '-m', 'one');
  const first = git(work, 'rev-parse', 'HEAD');
  git(work, 'commit', '--quiet', '--allow-empty', '-m', 'two');
  const head = git(work, 'rev-parse', 'HEAD');
  git(work, 'remote', 'add', 'origin', remote);
  git(work, 'push', '--quiet', 'origin', 'main');
  return { root, remote, work, first, head };
}

test('a missing tag is created at the merge commit and pushed; a re-run is a no-op', () => {
  const { root, remote, work, head } = newRepoWithRemote();
  try {
    assert.equal(tagCommit('v1.2.3', { cwd: work }), null);
    assert.equal(ensureReleaseTag({ tag: 'v1.2.3', sha: head, cwd: work }), 'created');
    assert.equal(tagCommit('v1.2.3', { cwd: work }), head);
    assert.equal(git(remote, 'rev-list', '-n1', 'v1.2.3'), head, 'the tag reached the remote');
    assert.equal(git(work, 'cat-file', '-t', 'v1.2.3'), 'tag', 'an annotated tag, which goreleaser reads');
    assert.equal(ensureReleaseTag({ tag: 'v1.2.3', sha: head, cwd: work }), 'present');
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test('a tag that exists at a different commit is refused, never moved', () => {
  const { root, work, first, head } = newRepoWithRemote();
  try {
    git(work, 'tag', 'v1.2.3', first);
    assert.throws(() => ensureReleaseTag({ tag: 'v1.2.3', sha: head, cwd: work }), /refusing to move a release tag/);
    assert.equal(tagCommit('v1.2.3', { cwd: work }), first);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test('the CLI fails closed on a missing input and on a moved tag', () => {
  const { root, work, first, head } = newRepoWithRemote();
  try {
    const run = (env) => spawnSync(process.execPath, [CLI], { cwd: work, encoding: 'utf8', env: { ...process.env, ...env } });
    let res = run({ TAG: '', GITHUB_SHA: head });
    assert.equal(res.status, 1);
    assert.match(res.stdout + res.stderr, /TAG and GITHUB_SHA are required/);
    git(work, 'tag', 'v9.9.9', first);
    res = run({ TAG: 'v9.9.9', GITHUB_SHA: head });
    assert.equal(res.status, 1);
    assert.match(res.stdout + res.stderr, /refusing to move a release tag/);
    res = run({ TAG: 'v2.0.0', GITHUB_SHA: head });
    assert.equal(res.status, 0, res.stderr);
    assert.match(res.stdout, /created \+ pushed v2\.0\.0/);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});
