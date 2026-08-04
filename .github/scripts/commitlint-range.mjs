#!/usr/bin/env node
// commitlint-range.mjs — lints a pull request's commit range, skipping
// commits that touched no files.
//
// WHY THIS EXISTS (issue #1643): GitHub Copilot's coding-agent workflow opens
// every PR it authors with an empty "Initial plan" placeholder commit — no
// changed files, empty subject, empty Conventional-Commits type. commitlint's
// own `--from`/`--to` range mode enumerates every commit SHA in the range via
// its internal traversal and lints ALL of them, including commits that
// changed nothing, so that placeholder trips `subject-empty` / `type-empty`
// and reds the whole `lint` required check on every Copilot PR.
//
// A first attempt added `--git-log-args="--diff-filter=ACMRT"` to the
// commitlint invocation, on the theory that narrowing `git log`'s traversal
// to commits with real file changes would narrow what commitlint lints.
// VERIFIED INEFFECTIVE: reproduced locally with commitlint@19 — byte-
// identical output with and without the flag, still failing on the empty
// commit. commitlint's range mode does not consult `--git-log-args` to decide
// which commits to lint.
//
// This module filters the commit LIST ourselves before commitlint ever sees
// it, on the one fact that actually describes "not a real change": zero
// files touched (`git diff-tree --no-commit-id --name-only -r <sha>` is
// empty). That is deliberately NOT keyed on Copilot's exact commit-message
// text ("Initial plan") — a commitlint `ignores` predicate matching that
// literal string was considered and confirmed to work today, but it is
// brittle two ways: it rots the moment the wording changes (a product
// surface this repo does not control), and it would also exempt any
// accidental non-empty commit that happened to share that exact subject line
// from real linting. Filtering by diff emptiness generalises to any
// future auto-generated empty commit, not just this one product's current
// wording, and cannot be fooled by message text alone.
//
// Every commit that DID touch a file is linted exactly as before, one at a
// time via commitlint's stdin ("--edit"-equivalent) mode — the same
// per-commit linting commitlint's own range mode performs internally, minus
// the empty ones. Merge commits are excluded via `git rev-list --no-merges`,
// matching commitlint's existing `defaultIgnores` behaviour for merge-commit
// subjects (enabled by default), so this is not a behaviour change for them.
//
// Env contract (only read when invoked as the program):
//   BASE_SHA           REQUIRED. Range start (exclusive) — a resolvable ref/SHA.
//   HEAD_SHA           REQUIRED. Range end (inclusive) — a resolvable ref/SHA.
//   COMMITLINT_CONFIG  Path to the commitlint config file.
//                      Default: .commitlintrc.json
//   GIT_CWD            Working directory for git + commitlint invocations.
//                      Default: process.cwd()
//
// Exit codes: 0 = every commit that touched a file passed commitlint (or the
// range contains no such commit), 1 = a commit failed linting, or the range
// itself could not be resolved (anti-vacuity: an unresolvable range is a
// failure, never a silent pass).
//
// The pure/injectable parts (selectLintableCommits, run) are exported and
// pinned by the `node --test` guard commitlint-range.test.mjs; the scan
// itself runs only when this file is invoked as the program.

import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { capture, error, log, notice } from './lib/gh.mjs';

// listRangeCommits — non-merge commit SHAs in (baseSha, headSha], oldest
// first, each paired with its full commit message.
export function listRangeCommits(baseSha, headSha, opts = {}) {
  const cwd = opts.cwd;
  // %x00-delimited records so a multi-line commit body can't be mistaken for
  // a record boundary; %H is the SHA, everything after the first NUL in a
  // record is the raw message (subject + blank line + body).
  const res = capture(
    'git',
    ['log', '--no-merges', '--reverse', '--format=%H%x00%B%x01', `${baseSha}..${headSha}`],
    { cwd },
  );
  if (res.status !== 0) {
    throw new Error(`git log ${baseSha}..${headSha} failed: ${res.stderr.trim() || res.stdout.trim()}`);
  }
  return res.stdout
    .split('\x01')
    .map((record) => record.replace(/^\n/, ''))
    .filter((record) => record.trim().length > 0)
    .map((record) => {
      const nul = record.indexOf('\x00');
      const sha = record.slice(0, nul);
      // Trim exactly one trailing newline `git log` leaves before the %x01
      // separator; preserve everything else (blank lines in the body matter
      // to commitlint's body-leading-blank rule).
      const message = record.slice(nul + 1).replace(/\n$/, '');
      return { sha, message };
    });
}

// hasFileChanges — true if `sha` touched at least one file. A commit with an
// empty tree diff (GitHub Copilot's "Initial plan" placeholder is created via
// `git commit --allow-empty`) prints nothing here regardless of its message.
export function hasFileChanges(sha, opts = {}) {
  const res = capture('git', ['diff-tree', '--no-commit-id', '--name-only', '-r', sha], {
    cwd: opts.cwd,
  });
  if (res.status !== 0) {
    throw new Error(`git diff-tree ${sha} failed: ${res.stderr.trim() || res.stdout.trim()}`);
  }
  return res.stdout.trim().length > 0;
}

// selectLintableCommits — the commits in (baseSha, headSha] commitlint should
// actually see, plus the ones filtered out (for reporting). Throws if the
// range itself does not resolve — an unresolvable range must never read as
// "no commits to lint".
export function selectLintableCommits(baseSha, headSha, opts = {}) {
  const commits = listRangeCommits(baseSha, headSha, opts);
  const lintable = [];
  const skipped = [];
  for (const commit of commits) {
    if (hasFileChanges(commit.sha, opts)) {
      lintable.push(commit);
    } else {
      skipped.push(commit);
    }
  }
  return { lintable, skipped };
}

// defaultLintOne — lints a single commit message via the real commitlint
// CLI, stdin mode (equivalent to `commitlint --edit` reading from a file: no
// `--from`/`--to`, so commitlint's own range enumeration never runs — this
// module IS the range enumeration now). `npx --no` refuses to fetch from the
// registry, so this only succeeds when commitlint is already installed
// (the workflow step installs it immediately before invoking this module).
export function defaultLintOne(message, opts = {}) {
  const config = opts.config ?? '.commitlintrc.json';
  return capture('npx', ['--no', '--', 'commitlint', '--config', config, '--verbose'], {
    cwd: opts.cwd,
    input: message,
  });
}

// run — the orchestration seam. `lintOne` defaults to defaultLintOne (real
// commitlint) but is injectable so the orchestration logic can be pinned
// offline without installing commitlint — see commitlint-range.test.mjs.
export function run({ baseSha, headSha, cwd, config = '.commitlintrc.json', lintOne = defaultLintOne }) {
  if (!baseSha || !headSha) {
    error('commitlint-range: BASE_SHA and HEAD_SHA are both required.');
    return 1;
  }

  let selection;
  try {
    selection = selectLintableCommits(baseSha, headSha, { cwd });
  } catch (err) {
    error(`commitlint-range: could not resolve commit range ${baseSha}..${headSha}: ${err.message}`);
    return 1;
  }

  const { lintable, skipped } = selection;

  for (const commit of skipped) {
    const subject = commit.message.split('\n', 1)[0];
    notice(`skipping ${commit.sha.slice(0, 12)} "${subject}" — no file changes`);
  }

  const failures = [];
  for (const commit of lintable) {
    const subject = commit.message.split('\n', 1)[0];
    const result = lintOne(commit.message, { cwd, config });
    log(`⧗   ${commit.sha.slice(0, 12)}   ${subject}`);
    if (result.stdout) log(result.stdout.trimEnd());
    if (result.stderr) log(result.stderr.trimEnd());
    if (result.status !== 0) {
      failures.push(commit);
    }
  }

  if (failures.length > 0) {
    error(
      `commitlint-range: ${failures.length} of ${lintable.length} commit(s) failed — ` +
        failures.map((c) => c.sha.slice(0, 12)).join(', '),
    );
    return 1;
  }

  notice(
    `commitlint-range: ${lintable.length} commit(s) linted, ${skipped.length} skipped (no file changes).`,
  );
  return 0;
}

const isMain = process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href;
if (isMain) {
  const status = run({
    baseSha: process.env.BASE_SHA,
    headSha: process.env.HEAD_SHA,
    cwd: process.env.GIT_CWD,
    config: process.env.COMMITLINT_CONFIG ?? '.commitlintrc.json',
  });
  process.exit(status);
}
