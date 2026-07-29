// compat-publish-score.test.mjs — in-process pins for the compat-scores
// badge publisher.
//
// The publish path is exercised against a REAL local git remote rather than a
// mock: the orphan bootstrap, the badge commit, and the unchanged-score
// short-circuit are all git behaviours, and a mock that agreed with the
// script's assumptions would pass while the branch stayed empty in CI. The
// remote is a bare repo in a temp dir, so the whole file runs offline.
//
// Every publish also asserts that the job's checkout came out untouched --
// see assertCheckoutUndisturbed, which publish() applies to each scenario so
// a new exit path cannot be added without inheriting the check.

import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import {
  chmodSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';

import { retryDelayMs, shieldsSubset } from './compat-publish-score.mjs';

const SCRIPT = fileURLToPath(new URL('./compat-publish-score.mjs', import.meta.url));

// The runner resolves a local composite action's POST phase out of the
// checkout after the job's last step. compatibility/{loki,tempo} failed with
// "Can't find 'action.yml' ... under .github/actions/setup-buildx" because a
// publish had switched the checkout to the orphan branch, where this path
// does not exist. Stand the real file up in the fixture so the regression is
// asserted against the artefact that actually broke.
const COMPOSITE_ACTION = '.github/actions/setup-buildx/action.yml';

function git(cwd, args) {
  const res = spawnSync('git', args, { cwd, encoding: 'utf8' });
  assert.equal(res.status, 0, `git ${args.join(' ')} failed: ${res.stderr}`);
  return res.stdout;
}

// makeRemote builds an origin/work pair: a bare repo standing in for GitHub
// and a clone-shaped work repo with one commit on main, matching the state
// actions/checkout leaves the runner in.
function makeRemote() {
  const root = mkdtempSync(path.join(tmpdir(), 'compat-scores-'));
  const origin = path.join(root, 'origin.git');
  const work = path.join(root, 'work');
  git(root, ['init', '--bare', '--initial-branch=main', origin]);
  git(root, ['init', '--initial-branch=main', work]);
  git(work, ['config', 'user.name', 'test']);
  git(work, ['config', 'user.email', 'test@example.com']);
  writeFileSync(path.join(work, 'README.md'), '# work\n');
  mkdirSync(path.join(work, path.dirname(COMPOSITE_ACTION)), { recursive: true });
  writeFileSync(
    path.join(work, COMPOSITE_ACTION),
    'name: setup-buildx\nruns:\n  using: composite\n',
  );
  git(work, ['add', '.']);
  git(work, ['commit', '-m', 'init']);
  git(work, ['remote', 'add', 'origin', origin]);
  git(work, ['push', '-u', 'origin', 'main']);
  return { root, origin, work };
}

// checkoutState captures everything a later step or a composite action's POST
// phase can observe about the checkout.
function checkoutState(work) {
  return {
    branch: git(work, ['rev-parse', '--abbrev-ref', 'HEAD']).trim(),
    head: git(work, ['rev-parse', 'HEAD']).trim(),
    tracked: git(work, ['ls-files']),
    status: git(work, ['status', '--porcelain']),
    composite: existsSync(path.join(work, COMPOSITE_ACTION)),
    worktrees: git(work, ['worktree', 'list']).trim().split('\n').length,
  };
}

// assertCheckoutUndisturbed is the regression gate. Publishing is a side
// errand of the job; the checkout it runs in belongs to every other step.
function assertCheckoutUndisturbed(before, after) {
  assert.equal(after.branch, before.branch, 'publish must leave the checkout on its own branch');
  assert.equal(after.head, before.head, 'publish must not move the checkout HEAD');
  assert.equal(
    after.composite,
    true,
    `publish must leave ${COMPOSITE_ACTION} in place -- the runner reads it in the POST phase`,
  );
  assert.equal(after.tracked, before.tracked, 'publish must not change the checked-out tree');
  assert.equal(after.status, before.status, 'publish must not add or delete working-tree files');
  assert.equal(after.worktrees, before.worktrees, 'publish must not leak its scratch worktree');
}

// writeScore drops a score file where the workflow's SRC points — INSIDE the
// repo, as in production, so the orphan bootstrap's `git clean -fdx` really
// does get the chance to delete it out from under the publisher.
function writeScore(work, score) {
  const dir = path.join(work, 'compatibility', 'prometheus');
  mkdirSync(dir, { recursive: true });
  const file = path.join(dir, 'compat-score.json');
  writeFileSync(file, `${JSON.stringify(score, null, 2)}\n`);
  return file;
}

// publish runs the module the way the workflow step does, and asserts the
// checkout invariant on every call so each scenario below covers it.
function publish(work, { head = 'prometheus', src }) {
  const before = checkoutState(work);
  const res = spawnSync(process.execPath, [SCRIPT], {
    cwd: work,
    encoding: 'utf8',
    env: { ...process.env, HEAD: head, SRC: src },
  });
  assertCheckoutUndisturbed(before, checkoutState(work));
  return res;
}

// badgeOn reads the published badge straight out of the remote, so the
// assertion is about what a shields.io fetch would see, not about local state.
function badgeOn(origin, head) {
  return git(origin, ['show', `compat-scores:badges/${head}.json`]);
}

const richScore = {
  schemaVersion: 1,
  label: 'promql compat',
  message: '737/737',
  color: 'brightgreen',
  passed: 737,
  total: 737,
  percent: 100,
};

test('shieldsSubset keeps only the endpoint-schema keys, in order', () => {
  const out = shieldsSubset(richScore);
  assert.deepEqual(Object.keys(JSON.parse(out)), [
    'schemaVersion',
    'label',
    'message',
    'color',
  ]);
  // The cerberus-side bookkeeping is what makes shields.io render an
  // "invalid properties" title instead of the badge.
  for (const key of ['passed', 'total', 'percent']) {
    assert.equal(out.includes(key), false, `${key} must not be published`);
  }
});

test('shieldsSubset renders jq-identical bytes so an unchanged score is an empty diff', () => {
  const jq = spawnSync('jq', ['{schemaVersion, label, message, color}'], {
    input: JSON.stringify(richScore),
    encoding: 'utf8',
  });
  // jq is present on ubuntu-latest and on the dev box; if it is genuinely
  // absent the byte-for-byte claim cannot be checked here, and the publish
  // integration test below still pins the behaviour that depends on it.
  assert.equal(jq.status, 0, 'jq must be available to pin the byte-identical claim');
  assert.equal(shieldsSubset(richScore), jq.stdout);
});

test('shieldsSubset nulls absent keys and preserves falsy ones', () => {
  assert.deepEqual(JSON.parse(shieldsSubset({ schemaVersion: 1 })), {
    schemaVersion: 1,
    label: null,
    message: null,
    color: null,
  });
  // 0 and '' are legitimate badge values; only null/undefined become null.
  assert.deepEqual(JSON.parse(shieldsSubset({ schemaVersion: 0, label: '' })), {
    schemaVersion: 0,
    label: '',
    message: null,
    color: null,
  });
});

test('retryDelayMs grows with the attempt so racing heads de-collide', () => {
  const delays = [1, 2, 3, 4, 5].map(retryDelayMs);
  assert.deepEqual(delays, [4000, 6000, 8000, 10000, 12000]);
});

test('publish bootstraps the orphan branch and lands the badge on the remote', () => {
  const { root, origin, work } = makeRemote();
  try {
    const src = writeScore(work, richScore);
    const res = publish(work, { src });
    assert.equal(res.status, 0, res.stdout + res.stderr);

    assert.equal(badgeOn(origin, 'prometheus'), shieldsSubset(richScore));

    // Orphan, not a branch off main: the badge tree must not carry main's
    // files, or every badge push would drag the whole repo with it.
    const tree = git(origin, ['ls-tree', '-r', '--name-only', 'compat-scores']);
    assert.deepEqual(tree.trim().split('\n'), ['badges/prometheus.json']);
    assert.equal(
      git(origin, ['rev-list', '--count', 'compat-scores']).trim(),
      '1',
      'orphan branch must start its own history',
    );
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test('publish is idempotent: an unchanged score adds no commit', () => {
  const { root, origin, work } = makeRemote();
  try {
    const src = writeScore(work, richScore);
    assert.equal(publish(work, { src }).status, 0);
    const first = git(origin, ['rev-parse', 'compat-scores']).trim();

    const res = publish(work, { src });
    assert.equal(res.status, 0, res.stdout + res.stderr);
    assert.match(res.stdout, /score unchanged; no commit needed/);
    assert.equal(git(origin, ['rev-parse', 'compat-scores']).trim(), first);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test('publish commits over the existing branch when the score moves', () => {
  const { root, origin, work } = makeRemote();
  try {
    assert.equal(publish(work, { src: writeScore(work, richScore) }).status, 0);

    const moved = { ...richScore, message: '738/738', passed: 738, total: 738 };
    assert.equal(publish(work, { src: writeScore(work, moved) }).status, 0);

    assert.equal(badgeOn(origin, 'prometheus'), shieldsSubset(moved));
    assert.equal(
      git(origin, ['rev-list', '--count', 'compat-scores']).trim(),
      '2',
      'a moved score must fast-forward the branch, not restart it',
    );
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test('a second head publishes alongside the first rather than replacing it', () => {
  const { root, origin, work } = makeRemote();
  try {
    assert.equal(publish(work, { src: writeScore(work, richScore) }).status, 0);

    const loki = { ...richScore, label: 'logql compat', message: '512/512' };
    const res = publish(work, { head: 'loki', src: writeScore(work, loki) });
    assert.equal(res.status, 0, res.stdout + res.stderr);

    // Both badges must survive: the three heads share one branch, and a
    // publisher that reset the tree would silently unpublish its siblings.
    assert.deepEqual(
      git(origin, ['ls-tree', '-r', '--name-only', 'compat-scores']).trim().split('\n').sort(),
      ['badges/loki.json', 'badges/prometheus.json'],
    );
    assert.equal(badgeOn(origin, 'prometheus'), shieldsSubset(richScore));
    assert.equal(badgeOn(origin, 'loki'), shieldsSubset(loki));
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test('a missing score file is a no-op, not a failure', () => {
  const { root, origin, work } = makeRemote();
  try {
    const res = publish(work, { src: path.join(work, 'nope', 'compat-score.json') });
    // The harness failure is raised by the job's own "Fail job" step; this
    // step must not add a second red herring on top of it.
    assert.equal(res.status, 0, res.stdout + res.stderr);
    assert.match(res.stdout, /nothing to publish/);
    assert.equal(
      spawnSync('git', ['rev-parse', '--verify', 'compat-scores'], { cwd: origin }).status !== 0,
      true,
      'nothing may be published when there is no score',
    );
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test('a wiring slip with no HEAD/SRC fails loudly', () => {
  const { root, work } = makeRemote();
  try {
    const res = publish(work, { head: '', src: '' });
    assert.equal(res.status, 1);
    assert.match(res.stdout, /HEAD and SRC env vars are required/);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

// The regression this file's checkout invariant exists for: a publish that
// switched the checkout onto the orphan branch passed its own step and then
// failed the job minutes later, in the POST phase of a local composite
// action, with "Can't find 'action.yml'". Only push-to-main reaches the
// publish step, so no pull_request run could ever have caught it.
test('every publish exit path leaves the checkout and the score file intact', () => {
  const { root, work } = makeRemote();
  try {
    const src = writeScore(work, richScore);

    // Exit path 1: orphan bootstrap, badge published.
    assert.equal(publish(work, { src }).status, 0);
    // SRC lives inside the checkout, so a publisher that cleared the tree
    // would also have deleted the score it was told to publish.
    assert.equal(existsSync(src), true, 'the score file must survive the bootstrap');

    // Exit path 2: score unchanged, short-circuit before any commit.
    const unchanged = publish(work, { src });
    assert.equal(unchanged.status, 0);
    assert.match(unchanged.stdout, /score unchanged; no commit needed/);

    // Exit path 3: score moved, commit pushed over the existing branch.
    const moved = publish(work, { src: writeScore(work, { ...richScore, message: '9/9' }) });
    assert.equal(moved.status, 0, moved.stdout + moved.stderr);

    // Exit path 4: nothing to publish at all.
    const absent = publish(work, { src: path.join(work, 'nope', 'compat-score.json') });
    assert.equal(absent.status, 0);

    // Belt and braces on top of the invariant publish() already asserted:
    // name the file whose absence produced the CI error.
    const action = readFileSync(path.join(work, COMPOSITE_ACTION), 'utf8');
    assert.match(action, /using: composite/, 'the composite action must still be readable');
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

// rejectFirstPush installs a pre-receive hook that rejects exactly one push
// and then deletes itself, reproducing the lost race between two head jobs
// without needing two of them.
function rejectFirstPush(origin) {
  const hook = path.join(origin, 'hooks', 'pre-receive');
  mkdirSync(path.dirname(hook), { recursive: true });
  writeFileSync(hook, '#!/bin/sh\nrm -f "$0"\necho "concurrent update" >&2\nexit 1\n');
  chmodSync(hook, 0o755);
}

test('a rejected push retries to success without disturbing the checkout', () => {
  const { root, origin, work } = makeRemote();
  try {
    assert.equal(publish(work, { src: writeScore(work, richScore) }).status, 0);

    rejectFirstPush(origin);
    const moved = { ...richScore, message: '740/740' };
    // One real backoff is spent here; that is the cost of covering the retry
    // arm against a genuine push rejection rather than a stubbed one.
    const res = publish(work, { src: writeScore(work, moved) });
    assert.equal(res.status, 0, res.stdout + res.stderr);
    assert.match(res.stdout, /push rejected \(likely concurrent update\); retrying/);
    assert.match(res.stdout, /published prometheus score on attempt 2/);
    assert.equal(badgeOn(origin, 'prometheus'), shieldsSubset(moved));
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test('a git failure mid-publish exits 1, annotates, and still tears the worktree down', () => {
  const { root, origin, work } = makeRemote();
  try {
    assert.equal(publish(work, { src: writeScore(work, richScore) }).status, 0);

    // A rejecting pre-commit hook fails the publisher's commit after its
    // scratch worktree already exists — the exit path where teardown is
    // easiest to forget. Hooks live in the shared common dir, so the scratch
    // worktree inherits it exactly as the runner's checkout would.
    const hook = path.join(work, '.git', 'hooks', 'pre-commit');
    mkdirSync(path.dirname(hook), { recursive: true });
    writeFileSync(hook, '#!/bin/sh\necho "hook says no" >&2\nexit 1\n');
    chmodSync(hook, 0o755);

    const res = publish(work, { src: writeScore(work, { ...richScore, message: '1/1' }) });
    assert.equal(res.status, 1, res.stdout + res.stderr);
    assert.match(res.stdout, /::error title=compat-scores publish failed::/);
    assert.equal(
      git(origin, ['rev-list', '--count', 'compat-scores']).trim(),
      '1',
      'a failed publish must not have pushed anything',
    );
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});
