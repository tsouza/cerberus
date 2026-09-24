#!/usr/bin/env node
// dependabot-tidy-nested-modules.mjs — .github/workflows/dependabot-tidy-nested-modules.yml.
//
// test/oracle is a nested Go module (its own go.mod) that carries
// `replace github.com/tsouza/cerberus => ../..` so its AGPL-quarantined
// oracle tests can import cerberus's internal/** packages (see the module
// doc-comment in test/oracle/go.mod and docs re: the agpl-clean gate). That
// local-path replace entangles its module graph with the root's: almost
// every dependency Dependabot bumps in the root go.mod is also an indirect
// require in test/oracle/go.mod, so a root-only bump leaves the nested
// module's go.mod/go.sum stale. `.github/dependabot.yml` only watches
// `directory: /` (by design — a second Dependabot directory would open a
// SEPARATE, unsynced PR, not fix this), so nothing ever tidies test/oracle
// on its own. Left alone, `ci.yml`'s "test oracle module" and "go.mod is
// tidy" steps fail on nearly every Dependabot Go PR (see PR #1211).
//
// This script closes that loop. It lists the open Dependabot Go-module PRs of
// this repository and, for each one, checks its branch out into a git
// worktree of the default-branch checkout it runs from, runs `go mod tidy` in
// each nested module there and, only if that produced a diff, commits and
// pushes a fixup to the branch. One PR's failure does not stop the others;
// the run fails if any PR failed.
//
// WHO PUSHES (issue #3659). The fixup has to start the PR's checks the way a
// human push does. A push made with GITHUB_TOKEN starts runs that wait in
// `action_required` for a manual approval (#3645: every run on the fixup
// commit 671ca35162 sat there until re-run by hand), and a run triggered by
// Dependabot sees only Dependabot secrets, of which this repository has none,
// so RELEASE_PAT is empty inside it. The workflow therefore runs on a
// schedule — never Dependabot-triggered, so Actions secrets are present — and
// its checkout persists RELEASE_PAT, whose push triggers CI normally. Every
// fetch and push here goes through that checkout's `origin`.
//
// A push rejected because the branch moved (Dependabot rebased or updated the
// PR since the checkout) is not a failure: the next scheduled run tidies the
// new head.
//
// Env contract:
//   GITHUB_REPOSITORY    owner/name. Required.
//   GITHUB_TOKEN         reads the open pull requests. Required.
//   GITHUB_API_URL       default https://api.github.com.
//   PUSH_TOKEN_SET       "true" when the checkout persisted RELEASE_PAT.
//                        Required: without it a push would need approval.
//   WORKTREE_ROOT        where the per-PR worktrees go (default: RUNNER_TEMP,
//                        else the OS temp dir).
//   NESTED_MODULE_DIRS   space-separated dirs, each containing a go.mod to
//                        re-tidy (default: "test/oracle")
//   GIT_USER_NAME        commit author name (default: "github-actions[bot]")
//   GIT_USER_EMAIL       commit author email
//                        (default: "github-actions[bot]@users.noreply.github.com")
//
// Exit codes: 0 = every PR was already tidy, got its fixup pushed, or had
// moved since it was fetched; 1 = a missing input, an API failure, or a PR
// whose fetch, `go mod tidy`, commit or push failed.

import { tmpdir } from 'node:os';
import { join } from 'node:path';
import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { capture, error, notice } from './lib/gh.mjs';
import { DEFAULT_API_BASE, ghPaginate } from './lib/gh-api.mjs';

export const DEPENDABOT_LOGIN = 'dependabot[bot]';
export const GO_MODULES_BRANCH_PREFIX = 'dependabot/go_modules/';
export const DEFAULT_NESTED_MODULE_DIRS = ['test/oracle'];

// A run over more open pull requests than this is a runaway listing, not a
// real backlog; ghPaginate throws rather than return a silent prefix.
const MAX_PULL_REQUEST_PAGES = 10;

// git's wording when a push is refused because the remote branch moved.
const MOVED_BRANCH = /\[rejected\]|non-fast-forward|fetch first|stale info/;

// selectTargets — the open pull requests this workflow tidies: authored by
// Dependabot, updating Go modules, from a branch of this repository.
export function selectTargets(pulls, repository) {
  return pulls
    .filter(
      (pr) =>
        pr.user?.login === DEPENDABOT_LOGIN &&
        pr.head?.repo?.full_name === repository &&
        typeof pr.head?.ref === 'string' &&
        pr.head.ref.startsWith(GO_MODULES_BRANCH_PREFIX),
    )
    .map((pr) => ({ number: pr.number, branch: pr.head.ref }))
    .sort((a, b) => a.number - b.number);
}

export async function listTargets({ repository, token, apiUrl = DEFAULT_API_BASE, fetchImpl }) {
  const pulls = await ghPaginate({
    url: `${apiUrl}/repos/${repository}/pulls?state=open`,
    token,
    what: 'list open pull requests',
    maxPages: MAX_PULL_REQUEST_PAGES,
    fetchImpl,
  });
  return selectTargets(pulls, repository);
}

const COMMIT_MESSAGE =
  'chore: go mod tidy nested modules\n\n' +
  "Dependabot bumped a dependency shared with test/oracle's module\n" +
  'graph (entangled via its local-path replace on the root module);\n' +
  're-tidying keeps its go.mod/go.sum consistent.';

// tidyAndPush — re-tidy each nested module in `targetDir` and push a fixup
// commit to `branch` when that changed anything. `run` is capture()-shaped
// ({ status, stdout, stderr }) and injectable for tests.
export function tidyAndPush({
  targetDir,
  branch,
  dirs = DEFAULT_NESTED_MODULE_DIRS,
  userName = 'github-actions[bot]',
  userEmail = 'github-actions[bot]@users.noreply.github.com',
  run = capture,
}) {
  if (!targetDir || !branch) {
    error('dependabot-tidy-nested-modules: tidyAndPush needs a targetDir and a branch');
    return 1;
  }
  const step = (cmd, args, cwd = targetDir) => {
    const res = run(cmd, args, { cwd });
    if (res.status !== 0) {
      error(`dependabot-tidy-nested-modules: ${cmd} ${args.join(' ')} failed in ${cwd}: ${res.stderr.trim()}`);
    }
    return res;
  };

  for (const dir of dirs) {
    notice(`go mod tidy in ${dir}`);
    if (step('go', ['mod', 'tidy'], join(targetDir, dir)).status !== 0) return 1;
  }

  const status = step('git', ['status', '--porcelain', '--', ...dirs]);
  if (status.status !== 0) return 1;
  if (status.stdout.trim() === '') {
    notice(`nested module(s) already tidy on ${branch}: ${dirs.join(', ')}`);
    return 0;
  }

  for (const args of [
    ['config', 'user.name', userName],
    ['config', 'user.email', userEmail],
    ['add', '--', ...dirs],
    ['commit', '-m', COMMIT_MESSAGE],
  ]) {
    if (step('git', args).status !== 0) return 1;
  }

  const push = run('git', ['push', 'origin', `HEAD:refs/heads/${branch}`], { cwd: targetDir });
  if (push.status !== 0) {
    if (MOVED_BRANCH.test(push.stderr)) {
      notice(`${branch} moved since it was fetched; the next scheduled run tidies its new head`);
      return 0;
    }
    error(`dependabot-tidy-nested-modules: push to ${branch} failed: ${push.stderr.trim()}`);
    return 1;
  }
  notice(`pushed go mod tidy fixup for ${dirs.join(', ')} to ${branch}`);
  return 0;
}

// sweep — tidy every target in its own worktree under `worktreeRoot`, run
// from the default-branch checkout (`repoDir`) whose `origin` carries the
// push credential. Returns 1 if any target failed, after trying them all.
export function sweep({ targets, repoDir = '.', worktreeRoot, run = capture, ...tidyOptions }) {
  let failed = 0;
  for (const { number, branch } of targets) {
    const remoteRef = `refs/remotes/origin/${branch}`;
    const worktree = join(worktreeRoot, `pr-${number}`);
    const fetch = run('git', ['fetch', '--no-tags', 'origin', `+refs/heads/${branch}:${remoteRef}`], { cwd: repoDir });
    if (fetch.status !== 0) {
      error(`dependabot-tidy-nested-modules: fetching #${number} (${branch}) failed: ${fetch.stderr.trim()}`);
      failed++;
      continue;
    }
    const add = run('git', ['worktree', 'add', '--detach', worktree, remoteRef], { cwd: repoDir });
    if (add.status !== 0) {
      error(`dependabot-tidy-nested-modules: worktree for #${number} failed: ${add.stderr.trim()}`);
      failed++;
      continue;
    }
    if (tidyAndPush({ targetDir: worktree, branch, run, ...tidyOptions }) !== 0) failed++;
    run('git', ['worktree', 'remove', '--force', worktree], { cwd: repoDir });
  }
  return failed === 0 ? 0 : 1;
}

const isMain = process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href;
if (isMain) {
  let status;
  try {
    const { GITHUB_REPOSITORY: repository, GITHUB_TOKEN: token } = process.env;
    if (!repository || !token) {
      error('dependabot-tidy-nested-modules: GITHUB_REPOSITORY and GITHUB_TOKEN are required');
      status = 1;
    } else if (process.env.PUSH_TOKEN_SET !== 'true') {
      error(
        'dependabot-tidy-nested-modules: RELEASE_PAT is empty. A fixup pushed with GITHUB_TOKEN starts ' +
          'runs that wait for a manual approval, which is the failure this workflow exists to avoid.',
      );
      status = 1;
    } else {
      const targets = await listTargets({
        repository,
        token,
        apiUrl: process.env.GITHUB_API_URL || DEFAULT_API_BASE,
      });
      notice(
        `${targets.length} open Dependabot Go-module pull request(s)` +
          (targets.length ? `: ${targets.map((t) => `#${t.number}`).join(', ')}` : ''),
      );
      status = sweep({
        targets,
        worktreeRoot: process.env.WORKTREE_ROOT || process.env.RUNNER_TEMP || tmpdir(),
        dirs: process.env.NESTED_MODULE_DIRS
          ? process.env.NESTED_MODULE_DIRS.split(/\s+/).filter(Boolean)
          : DEFAULT_NESTED_MODULE_DIRS,
        userName: process.env.GIT_USER_NAME || undefined,
        userEmail: process.env.GIT_USER_EMAIL || undefined,
      });
    }
  } catch (err) {
    error(`dependabot-tidy-nested-modules: ${err.message}`);
    status = 1;
  }
  process.exit(status);
}
