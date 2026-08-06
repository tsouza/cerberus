#!/usr/bin/env node
// PreToolUse hook for Bash. Guards `git commit` and `git push`.
//
// WHAT IT BLOCKS
//
//   1. A commit or push aimed at `main`. This repository is PR-per-change and
//      branch protection rejects the push server-side anyway; failing locally
//      turns a confusing remote rejection into a one-line message before any
//      work is done. Also catches the subtler form — an explicit
//      `git push origin HEAD:main` from a feature branch.
//
//   2. A commit or push while lefthook's git hooks are not installed.
//      `lefthook.yml` is the layer that actually owns local validation:
//      `pre-commit` formats staged files, `commit-msg` runs commitlint, and
//      `pre-push` mirrors the CI `check` + `lint` + `forbid-skip` jobs. When
//      those hooks are absent every one of those gates is silently off, and the
//      first signal is a red PR. `just hooks-install` is the fix.
//
// WHAT IT DELIBERATELY DOES NOT DO
//
// It does not run the test suite or golangci-lint. Two reasons, both load-
// bearing. First, lefthook's `pre-push` already runs that work once per push
// rather than once per commit, so wiring it here would duplicate a
// better-targeted layer at a cost of minutes on every commit. Second,
// golangci-lint invoked from a freshly created agent worktree has been observed
// reporting "No issues found" without having analysed the tree — a false green
// is worse than no local check, because it is trusted.
//
// THE FULL-CI VARIANT — one environment variable away
//
// Setting CERBERUS_PRECOMMIT_FULL_CI=1 makes this guard additionally run
// `just ci` (lint + test + build) before each commit and push, and block on
// failure. That is the literal "validate everything before committing"
// behaviour, kept off by default for the two reasons above. Turn it on for a
// session with:
//
//   export CERBERUS_PRECOMMIT_FULL_CI=1
//
// or permanently by adding it to the `env` block of .claude/settings.json. The
// hook's `timeout` in that file is already sized for a full `just ci` run; for
// the default fast path the guard returns in milliseconds regardless.
//
// Input: the PreToolUse JSON payload on stdin (`tool_name`, `tool_input`).
// Exit codes: 0 = allow; 2 = block, with the reason on stderr (Claude Code
// feeds a PreToolUse exit code 2 back to the model as a blocking error).

import { execFileSync } from 'node:child_process';
import { existsSync, readFileSync, statSync } from 'node:fs';
import { isAbsolute, join } from 'node:path';
import process from 'node:process';

const ALLOW = 0;
const BLOCK = 2;

const PROTECTED_BRANCH = 'main';
const GUARDED_SUBCOMMANDS = new Set(['commit', 'push']);
const FULL_CI_ENV = 'CERBERUS_PRECOMMIT_FULL_CI';

// Git's own global options that consume the following argument, so the
// subcommand scanner does not mistake their value for the subcommand.
const GIT_GLOBAL_OPTS_WITH_VALUE = new Set(['-C', '-c', '--git-dir', '--work-tree', '--namespace']);

// Command wrappers that may precede `git` on the line. `rtk` is this repo's
// token-reducing CLI proxy, invoked either as `rtk git ...` or `rtk proxy git ...`.
const COMMAND_WRAPPERS = new Set(['rtk', 'proxy', 'command', 'sudo', 'time', 'nice', 'env']);

function readPayload() {
  try {
    return JSON.parse(readFileSync(0, 'utf8'));
  } catch {
    return null;
  }
}

// splitSegments — break a shell line into independently-executed segments so a
// `git commit` buried in a `&&` chain is still seen. Quoting is not modelled;
// the cost of a rare false positive here is one explanatory message, while a
// false negative is the failure this guard exists to prevent.
function splitSegments(command) {
  return command.split(/&&|\|\||[;\n|]/g);
}

// effectiveCwd — the directory the guarded git command actually runs in.
//
// The payload's `cwd` is the session's project directory, which is not where
// the command runs when the line starts by changing directory: agents work in
// linked worktrees and reach them with `cd <worktree> && git commit ...`. The
// project directory and the worktree are different checkouts of the same
// repository on different branches, so reading the branch from the payload's
// `cwd` answers a question nobody asked — and blocks every commit made from a
// worktree whenever the main checkout happens to sit on `main`.
//
// `git -C <dir>` on the guarded segment itself takes precedence, since it binds
// tighter than any earlier `cd`.
function effectiveCwd(command, segment, payloadCwd) {
  const dirOpt = gitDirOption(segment);
  if (dirOpt) return resolveDir(dirOpt, payloadCwd);
  for (const seg of splitSegments(command)) {
    const words = seg.trim().split(/\s+/).filter(Boolean);
    if (words[0] === 'cd' && words[1] && !words[1].startsWith('-')) {
      const resolved = resolveDir(words[1], payloadCwd);
      if (resolved) return resolved;
    }
  }
  return payloadCwd;
}

// gitDirOption — the value of `-C <dir>` on a git invocation, or null.
function gitDirOption(segment) {
  const words = segment.trim().split(/\s+/).filter(Boolean);
  const at = words.indexOf('-C');
  return at >= 0 && words[at + 1] ? words[at + 1] : null;
}

function resolveDir(dir, base) {
  const abs = isAbsolute(dir) ? dir : join(base, dir);
  return existsSync(abs) ? abs : null;
}

// gitInvocation — given one segment, return the git subcommand it runs, or null.
// Skips leading `VAR=value` assignments and known wrappers, then walks git's
// global options to find the first bare word, which is the subcommand.
function gitInvocation(segment) {
  const words = segment.trim().split(/\s+/).filter(Boolean);
  let i = 0;
  while (i < words.length && (/^[A-Za-z_][A-Za-z0-9_]*=/.test(words[i]) || COMMAND_WRAPPERS.has(words[i]))) i += 1;
  if (i >= words.length || (words[i] !== 'git' && !words[i].endsWith('/git'))) return null;
  i += 1;
  while (i < words.length) {
    const w = words[i];
    if (!w.startsWith('-')) return w;
    if (GIT_GLOBAL_OPTS_WITH_VALUE.has(w)) i += 2;
    else i += 1;
  }
  return null;
}

function git(args, cwd) {
  return execFileSync('git', args, { cwd, encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] }).trim();
}

function currentBranch(cwd) {
  try {
    return git(['rev-parse', '--abbrev-ref', 'HEAD'], cwd);
  } catch {
    return null;
  }
}

// pushesToProtectedBranch — a push from a feature branch can still target
// `main` via an explicit refspec (`main`, `HEAD:main`, `+HEAD:refs/heads/main`).
function pushesToProtectedBranch(segment) {
  const refspecs = segment.trim().split(/\s+/).slice(1).filter((w) => !w.startsWith('-'));
  return refspecs.some((spec) => {
    const dst = spec.includes(':') ? spec.slice(spec.lastIndexOf(':') + 1) : spec;
    return dst === PROTECTED_BRANCH || dst === `refs/heads/${PROTECTED_BRANCH}`;
  });
}

// hooksDir — where git will look for hook scripts in THIS working tree.
// `core.hooksPath` wins when set; otherwise hooks live in the common git dir,
// which is what makes the check work identically from a linked worktree.
function hooksDir(cwd) {
  try {
    const configured = git(['config', '--get', 'core.hooksPath'], cwd);
    if (configured) return isAbsolute(configured) ? configured : join(git(['rev-parse', '--show-toplevel'], cwd), configured);
  } catch {
    // core.hooksPath unset: `git config --get` exits 1, which is the common case.
  }
  return join(git(['rev-parse', '--git-common-dir'], cwd), 'hooks');
}

// lefthookInstalled — a hook file exists AND delegates to lefthook. The name
// alone is not enough: an unrelated hook script at that path would satisfy a
// bare existence check while running none of the repo's gates.
function lefthookInstalled(cwd, hookNames) {
  let dir;
  try {
    dir = hooksDir(cwd);
  } catch {
    return { ok: false, reason: 'not inside a git working tree' };
  }
  const missing = hookNames.filter((name) => {
    const p = join(dir, name);
    if (!existsSync(p) || !statSync(p).isFile()) return true;
    return !readFileSync(p, 'utf8').includes('lefthook');
  });
  if (missing.length > 0) return { ok: false, reason: `${dir} has no lefthook ${missing.join(' / ')} hook` };
  return { ok: true };
}

function runFullCI(cwd) {
  try {
    execFileSync('just', ['ci'], { cwd, stdio: ['ignore', 'inherit', 'inherit'] });
    return { ok: true };
  } catch (e) {
    const detail = e.code === 'ENOENT' ? '`just` is not on PATH' : '`just ci` failed';
    return { ok: false, reason: detail };
  }
}

function block(lines) {
  process.stderr.write(`${lines.join('\n')}\n`);
  return BLOCK;
}

function main() {
  const payload = readPayload();
  if (!payload || payload.tool_name !== 'Bash') return ALLOW;

  const command = payload?.tool_input?.command;
  if (typeof command !== 'string' || command.length === 0) return ALLOW;

  const payloadCwd = payload.cwd && existsSync(payload.cwd) ? payload.cwd : process.cwd();

  const guarded = [];
  for (const segment of splitSegments(command)) {
    const sub = gitInvocation(segment);
    if (sub && GUARDED_SUBCOMMANDS.has(sub)) {
      guarded.push({ sub, segment, cwd: effectiveCwd(command, segment, payloadCwd) });
    }
  }
  if (guarded.length === 0) return ALLOW;

  const cwd = guarded[0].cwd;
  for (const { sub, segment, cwd: segCwd } of guarded) {
    const targetsMain = currentBranch(segCwd) === PROTECTED_BRANCH || (sub === 'push' && pushesToProtectedBranch(segment));
    if (targetsMain) {
      return block([
        `guard-git: refusing to ${sub} against \`${PROTECTED_BRANCH}\`.`,
        'This repository is PR-per-change and branch protection rejects direct pushes to main.',
        'Branch off the current origin/main, then push and `gh pr create` in the same step.',
      ]);
    }
  }

  const hookNames = guarded.some((g) => g.sub === 'push') ? ['pre-commit', 'commit-msg', 'pre-push'] : ['pre-commit', 'commit-msg'];
  const hooks = lefthookInstalled(cwd, hookNames);
  if (!hooks.ok) {
    return block([
      `guard-git: lefthook's git hooks are not installed — ${hooks.reason}.`,
      'Without them the formatters, commitlint, and the pre-push mirror of the CI',
      'check / lint / forbid-skip gates are all silently off.',
      'Fix with: just hooks-install',
    ]);
  }

  if (process.env[FULL_CI_ENV] === '1') {
    const ci = runFullCI(cwd);
    if (!ci.ok) {
      return block([`guard-git: ${FULL_CI_ENV}=1 is set and ${ci.reason}.`, 'Fix the failure, or unset the variable to fall back to the lefthook + CI layers.']);
    }
  }

  return ALLOW;
}

process.exit(main());
