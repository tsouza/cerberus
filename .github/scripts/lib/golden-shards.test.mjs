// golden-shards.test.mjs — the changed-file set behind every golden-update
// coverage check is computed from the MERGE-BASE with the base ref, never
// from the ref's tip, and never silently from HEAD.
//
// The scenario that motivates the first pin is the ordinary life of a PR
// branch: it branches off main, commits its own change, and main then gains
// unrelated commits. Diffing the branch against main's tip lists main's own
// files as if the branch had changed them, so a dispatch that named exactly
// the shards the branch touched is refused for shards it never touched. The
// second pin closes the opposite hole: a base that cannot be resolved must
// be an error, because a base of HEAD turns the coverage check into a
// working-tree-only check that a committed fixture sails straight past.
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { test } from 'node:test';

import { changedFilesFrom } from './golden-shards.mjs';

function git(cwd, ...args) {
  const r = spawnSync('git', args, { cwd, encoding: 'utf8' });
  assert.equal(r.status, 0, `git ${args.join(' ')} failed:\n${r.stderr}`);
  return r.stdout.trim();
}

function commitFile(repo, name, message) {
  writeFileSync(path.join(repo, name), `${name}\n`);
  git(repo, 'add', name);
  git(repo, '-c', 'user.name=t', '-c', 'user.email=t@example.invalid', 'commit', '-q', '-m', message);
}

/**
 * A repository whose `main` has moved past the point `topic` branched from:
 * main = A, B(main-only); topic = A, C(topic-only).
 */
function repoWithDivergedMain() {
  const repo = mkdtempSync(path.join(tmpdir(), 'golden-shards-'));
  git(repo, 'init', '-q', '-b', 'main');
  commitFile(repo, 'a.txt', 'A');
  git(repo, 'checkout', '-q', '-b', 'topic');
  commitFile(repo, 'topic-only.txt', 'C');
  git(repo, 'checkout', '-q', 'main');
  commitFile(repo, 'main-only.txt', 'B');
  git(repo, 'checkout', '-q', 'topic');
  return repo;
}

test('changedFilesFrom diffs against the merge-base, so main-only commits are not the branch\'s changes', (t) => {
  const repo = repoWithDivergedMain();
  t.after(() => rmSync(repo, { recursive: true, force: true }));

  const viaExplicitRef = changedFilesFrom(repo, { baseRef: 'main' });
  assert.deepEqual(viaExplicitRef, ['topic-only.txt'], 'an explicit base ref must resolve to its merge-base, not its tip');

  const viaDefault = changedFilesFrom(repo, { defaultBranchRef: 'main' });
  assert.deepEqual(viaDefault, ['topic-only.txt'], 'the default base must resolve to the merge-base as well');

  // The working tree still counts: a new untracked fixture is part of the set.
  writeFileSync(path.join(repo, 'new-fixture.txtar'), '');
  assert.deepEqual(changedFilesFrom(repo, { baseRef: 'main' }).sort(), ['new-fixture.txtar', 'topic-only.txt']);
});

test('changedFilesFrom refuses a base whose merge-base cannot be resolved rather than falling back to HEAD', (t) => {
  const repo = repoWithDivergedMain();
  t.after(() => rmSync(repo, { recursive: true, force: true }));

  assert.throws(
    () => changedFilesFrom(repo, { baseRef: 'refs/remotes/origin/does-not-exist' }),
    /cannot resolve the merge-base of HEAD and refs\/remotes\/origin\/does-not-exist/,
  );
  assert.throws(() => changedFilesFrom(repo, { defaultBranchRef: 'origin/main' }), /merge-base/);
});
