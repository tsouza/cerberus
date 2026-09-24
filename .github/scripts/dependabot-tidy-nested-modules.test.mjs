// dependabot-tidy-nested-modules.test.mjs — pins for the scheduled nested-module
// tidy (issue #3659): which pull requests it touches, and what the tidy step
// does for each outcome of `go mod tidy`, `git status` and `git push`. The
// command runner is a recording fake, so the assertions are on the exact
// commands issued, not on a mock's verdict.

import assert from 'node:assert/strict';
import { test } from 'node:test';

import {
  DEPENDABOT_LOGIN,
  listTargets,
  selectTargets,
  sweep,
  tidyAndPush,
} from './dependabot-tidy-nested-modules.mjs';

const REPO = 'tsouza/cerberus';

const pr = (number, { login = DEPENDABOT_LOGIN, ref = `dependabot/go_modules/go-deps-${number}`, repo = REPO } = {}) => ({
  number,
  user: { login },
  head: { ref, repo: repo === null ? null : { full_name: repo } },
});

test('selectTargets keeps only Dependabot Go-module PRs from this repository', () => {
  const pulls = [
    pr(3645),
    pr(3601, { ref: 'dependabot/go_modules/github.com/apache/thrift-0.24.0' }),
    pr(3644, { ref: 'dependabot/npm_and_yarn/test/e2e/playwright/playwright-deps-febceb97ad' }),
    pr(3610, { ref: 'dependabot/github_actions/github-actions-abc' }),
    pr(3658, { login: 'tsouza', ref: 'dependabot/go_modules/lookalike' }),
    pr(3659, { repo: 'someone/fork' }),
    pr(3660, { repo: null }),
  ];
  assert.deepEqual(selectTargets(pulls, REPO), [
    { number: 3601, branch: 'dependabot/go_modules/github.com/apache/thrift-0.24.0' },
    { number: 3645, branch: 'dependabot/go_modules/go-deps-3645' },
  ]);
});

test('selectTargets of no pull requests is an empty list', () => {
  assert.deepEqual(selectTargets([], REPO), []);
});

test('listTargets reads open pull requests through the API and filters them', async () => {
  const urls = [];
  const fetchImpl = async (url) => {
    urls.push(url);
    return { ok: true, status: 200, statusText: 'OK', json: async () => [pr(3645), pr(3700, { login: 'tsouza' })] };
  };
  const targets = await listTargets({ repository: REPO, token: 't', apiUrl: 'https://api.example.test', fetchImpl });
  assert.deepEqual(targets, [{ number: 3645, branch: 'dependabot/go_modules/go-deps-3645' }]);
  assert.equal(urls.length, 1);
  assert.match(urls[0], /^https:\/\/api\.example\.test\/repos\/tsouza\/cerberus\/pulls\?state=open&per_page=100&page=1$/);
});

// fakeRun — records every command; `results` maps "cmd arg0 arg1" prefixes
// to the { status, stdout, stderr } that command returns.
function fakeRun(results = {}) {
  const calls = [];
  const run = (cmd, args, opts) => {
    calls.push({ line: [cmd, ...args].join(' '), cwd: opts.cwd });
    const key = Object.keys(results).find((prefix) => [cmd, ...args].join(' ').startsWith(prefix));
    return { status: 0, stdout: '', stderr: '', ...(key ? results[key] : {}) };
  };
  return { calls, run };
}

const BRANCH = 'dependabot/go_modules/go-deps-3645';
const tidy = (fake) => tidyAndPush({ targetDir: 'target', branch: BRANCH, run: fake.run });
const DIRTY = { 'git status': { stdout: ' M test/oracle/go.mod\n M test/oracle/go.sum\n' } };

test('an already-tidy module is neither committed nor pushed', () => {
  const fake = fakeRun();
  assert.equal(tidy(fake), 0);
  assert.deepEqual(
    fake.calls.map((c) => [c.line, c.cwd]),
    [
      ['go mod tidy', 'target/test/oracle'],
      ['git status --porcelain -- test/oracle', 'target'],
    ],
  );
});

test('a stale module is committed and pushed to the PR branch', () => {
  const fake = fakeRun(DIRTY);
  assert.equal(tidy(fake), 0);
  const lines = fake.calls.map((c) => c.line);
  assert.ok(lines.includes('git add -- test/oracle'));
  assert.ok(lines.some((l) => l.startsWith('git commit -m chore: go mod tidy nested modules')));
  assert.equal(lines.at(-1), `git push origin HEAD:refs/heads/${BRANCH}`);
  assert.ok(fake.calls.every((c) => c.cwd.startsWith('target')));
});

test('a push rejected because the branch moved is left for the next run', () => {
  const fake = fakeRun({
    ...DIRTY,
    'git push': { status: 1, stderr: ' ! [rejected]        HEAD -> x (fetch first)\n' },
  });
  assert.equal(tidy(fake), 0);
});

test('any other push failure fails the job', () => {
  const fake = fakeRun({
    ...DIRTY,
    'git push': { status: 128, stderr: 'remote: Permission to tsouza/cerberus.git denied\n' },
  });
  assert.equal(tidy(fake), 1);
});

test('a failing go mod tidy fails the job before any git command', () => {
  const fake = fakeRun({ 'go mod tidy': { status: 1, stderr: 'go: module lookup disabled' } });
  assert.equal(tidy(fake), 1);
  assert.deepEqual(fake.calls.map((c) => c.line), ['go mod tidy']);
});

test('a failing commit fails the job without pushing', () => {
  const fake = fakeRun({ ...DIRTY, 'git commit': { status: 1, stderr: 'boom' } });
  assert.equal(tidy(fake), 1);
  assert.ok(!fake.calls.some((c) => c.line.startsWith('git push')));
});

test('every configured nested module is tidied', () => {
  const fake = fakeRun();
  assert.equal(tidyAndPush({ targetDir: 'target', branch: BRANCH, dirs: ['test/oracle', 'bench/x'], run: fake.run }), 0);
  assert.deepEqual(
    fake.calls.filter((c) => c.line === 'go mod tidy').map((c) => c.cwd),
    ['target/test/oracle', 'target/bench/x'],
  );
});

test('missing inputs fail without running anything', () => {
  const fake = fakeRun();
  assert.equal(tidyAndPush({ targetDir: '', branch: BRANCH, run: fake.run }), 1);
  assert.equal(tidyAndPush({ targetDir: 'target', branch: '', run: fake.run }), 1);
  assert.deepEqual(fake.calls, []);
});

// ---- sweep: one worktree per PR, run from the default-branch checkout ----

const TARGETS = [
  { number: 3645, branch: 'dependabot/go_modules/go-deps-3645' },
  { number: 3650, branch: 'dependabot/go_modules/go-deps-3650' },
];

test('sweep fetches, tidies in, and removes a worktree per PR', () => {
  const fake = fakeRun();
  assert.equal(sweep({ targets: TARGETS, repoDir: 'repo', worktreeRoot: '/tmp/w', run: fake.run }), 0);
  assert.deepEqual(
    fake.calls.map((c) => [c.line, c.cwd]),
    [
      ['git fetch --no-tags origin +refs/heads/dependabot/go_modules/go-deps-3645:refs/remotes/origin/dependabot/go_modules/go-deps-3645', 'repo'],
      ['git worktree add --detach /tmp/w/pr-3645 refs/remotes/origin/dependabot/go_modules/go-deps-3645', 'repo'],
      ['go mod tidy', '/tmp/w/pr-3645/test/oracle'],
      ['git status --porcelain -- test/oracle', '/tmp/w/pr-3645'],
      ['git worktree remove --force /tmp/w/pr-3645', 'repo'],
      ['git fetch --no-tags origin +refs/heads/dependabot/go_modules/go-deps-3650:refs/remotes/origin/dependabot/go_modules/go-deps-3650', 'repo'],
      ['git worktree add --detach /tmp/w/pr-3650 refs/remotes/origin/dependabot/go_modules/go-deps-3650', 'repo'],
      ['go mod tidy', '/tmp/w/pr-3650/test/oracle'],
      ['git status --porcelain -- test/oracle', '/tmp/w/pr-3650'],
      ['git worktree remove --force /tmp/w/pr-3650', 'repo'],
    ],
  );
});

test('sweep pushes each stale PR to its own branch from its own worktree', () => {
  const fake = fakeRun(DIRTY);
  assert.equal(sweep({ targets: TARGETS, repoDir: 'repo', worktreeRoot: '/tmp/w', run: fake.run }), 0);
  const pushes = fake.calls.filter((c) => c.line.startsWith('git push')).map((c) => [c.line, c.cwd]);
  assert.deepEqual(pushes, [
    ['git push origin HEAD:refs/heads/dependabot/go_modules/go-deps-3645', '/tmp/w/pr-3645'],
    ['git push origin HEAD:refs/heads/dependabot/go_modules/go-deps-3650', '/tmp/w/pr-3650'],
  ]);
});

test('one PR failing does not stop the others, but fails the sweep', () => {
  const fake = fakeRun({
    'git fetch --no-tags origin +refs/heads/dependabot/go_modules/go-deps-3645': { status: 128, stderr: "couldn't find remote ref" },
  });
  assert.equal(sweep({ targets: TARGETS, repoDir: 'repo', worktreeRoot: '/tmp/w', run: fake.run }), 1);
  assert.ok(fake.calls.some((c) => c.cwd === '/tmp/w/pr-3650/test/oracle'));
  assert.ok(!fake.calls.some((c) => c.line.includes('pr-3645')));
});

test('a failed tidy still removes its worktree and fails the sweep', () => {
  const fake = fakeRun({ 'go mod tidy': { status: 1, stderr: 'boom' } });
  assert.equal(sweep({ targets: TARGETS.slice(0, 1), repoDir: 'repo', worktreeRoot: '/tmp/w', run: fake.run }), 1);
  assert.equal(fake.calls.at(-1).line, 'git worktree remove --force /tmp/w/pr-3645');
});

test('a sweep over no PRs runs nothing and succeeds', () => {
  const fake = fakeRun();
  assert.equal(sweep({ targets: [], worktreeRoot: '/tmp/w', run: fake.run }), 0);
  assert.deepEqual(fake.calls, []);
});
