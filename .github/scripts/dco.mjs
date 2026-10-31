#!/usr/bin/env node
// dco.mjs — verifies that every commit a pull request contributes carries a
// Developer Certificate of Origin sign-off (https://developercertificate.org)
// from its author.
//
// A commit passes when its message has a `Signed-off-by: Name <email>` trailer
// whose email equals the commit author's email (compared case-insensitively).
// `git commit -s` writes exactly that trailer from the committer identity.
//
// The commits checked are the ones commitlint-range.mjs lints: the non-merge
// commits in (BASE_SHA, HEAD_SHA] that touched at least one file. A merge
// commit only records the integration of commits that are each checked in
// their own right, and a commit that changes no file contributes nothing to
// certify (GitHub Copilot's empty "Initial plan" placeholder is one).
//
// Env contract (only read when invoked as the program):
//   BASE_SHA  REQUIRED. Range start (exclusive) — a resolvable ref/SHA.
//   HEAD_SHA  REQUIRED. Range end (inclusive) — a resolvable ref/SHA.
//   GIT_CWD   Working directory for git invocations. Default: process.cwd()
//
// Exit codes: 0 = every contributed commit is signed off by its author or
// authored by an exempt service account (or the range contributes none),
// 1 = at least one commit is neither, or the range could not be resolved (an
// unresolvable range is a failure, never a silent pass).
//
// Commits authored by the GitHub service accounts in SERVICE_BOT_AUTHORS are
// exempt. They are automated dependency and maintenance updates with no person
// as author; Dependabot signs off as `support@github.com`, which never equals
// its author email. Any other bot account, including an AI coding agent's
// GitHub App, is checked like a person: its commits need the sign-off of the
// human who answers for them.
//
// signOffEmails, isServiceBotAuthor and checkCommit are pure; run and
// defaultAuthorOf read git. All are exported and pinned by the `node --test`
// guard dco.test.mjs.

import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { selectLintableCommits } from './commitlint-range.mjs';
import { capture, error, log, notice } from './lib/gh.mjs';

// SIGN_OFF_TRAILER matches one `Signed-off-by: Name <email>` line and captures
// the email. A trailer is a whole line of its own, so the pattern is anchored
// to line boundaries.
const SIGN_OFF_TRAILER = /^Signed-off-by:\s*.+?\s*<([^<>\s]+)>\s*$/gim;

// signOffEmails — the lower-cased emails of every sign-off trailer in message.
export function signOffEmails(message) {
  return [...message.matchAll(SIGN_OFF_TRAILER)].map((m) => m[1].toLowerCase());
}

// SERVICE_BOT_AUTHORS — the author emails of the exempt GitHub service
// accounts, lower-cased: Dependabot and the GitHub Actions bot.
const SERVICE_BOT_AUTHORS = new Set([
  '49699333+dependabot[bot]@users.noreply.github.com',
  '41898282+github-actions[bot]@users.noreply.github.com',
  'github-actions[bot]@users.noreply.github.com',
]);

// isServiceBotAuthor — whether authorEmail is an exempt service account's.
export function isServiceBotAuthor(authorEmail) {
  return SERVICE_BOT_AUTHORS.has(authorEmail.toLowerCase());
}

// checkCommit — null when the commit is signed off by its author or authored
// by an exempt service account, otherwise a one-line reason naming what is
// missing.
export function checkCommit({ authorName, authorEmail, message }) {
  if (isServiceBotAuthor(authorEmail)) {
    return null;
  }
  const emails = signOffEmails(message);
  if (emails.includes(authorEmail.toLowerCase())) {
    return null;
  }
  if (emails.length === 0) {
    return `no Signed-off-by trailer (author ${authorName} <${authorEmail}>)`;
  }
  return `signed off by ${emails.join(', ')}, but the author is ${authorName} <${authorEmail}>`;
}

// defaultAuthorOf — the author name and email of sha.
export function defaultAuthorOf(sha, opts = {}) {
  const res = capture('git', ['log', '-1', '--format=%an%x00%ae', sha], { cwd: opts.cwd });
  if (res.status !== 0) {
    throw new Error(`git log -1 ${sha} failed: ${res.stderr.trim() || res.stdout.trim()}`);
  }
  const [authorName, authorEmail] = res.stdout.replace(/\n$/, '').split('\x00');
  return { authorName, authorEmail };
}

// run — checks every contributed commit and reports each failure. Returns the
// process exit code.
export function run({
  baseSha,
  headSha,
  cwd,
  select = selectLintableCommits,
  authorOf = defaultAuthorOf,
}) {
  if (!baseSha || !headSha) {
    error('dco: BASE_SHA and HEAD_SHA are both required');
    return 1;
  }
  let commits;
  try {
    ({ lintable: commits } = select(baseSha, headSha, { cwd }));
  } catch (e) {
    error(`dco: ${e.message}`);
    return 1;
  }

  let failures = 0;
  for (const commit of commits) {
    const reason = checkCommit({ ...authorOf(commit.sha, { cwd }), message: commit.message });
    if (reason !== null) {
      failures++;
      const subject = commit.message.split('\n', 1)[0];
      error(`dco: ${commit.sha.slice(0, 12)} "${subject}": ${reason}`);
    }
  }

  if (failures > 0) {
    log(
      `dco: ${failures} of ${commits.length} commit(s) lack their author's sign-off. ` +
        'Sign off with `git commit -s`; to fix commits already on the branch, run ' +
        '`git rebase --signoff origin/main` and force-push the branch. ' +
        'See CONTRIBUTING.md, "Developer Certificate of Origin".',
    );
    return 1;
  }
  notice(`dco: all ${commits.length} contributed commit(s) are signed off by their author`);
  return 0;
}

const isMain = process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href;
if (isMain) {
  process.exit(
    run({
      baseSha: process.env.BASE_SHA,
      headSha: process.env.HEAD_SHA,
      cwd: process.env.GIT_CWD,
    }),
  );
}
