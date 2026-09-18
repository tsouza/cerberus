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
//   2. A commit or push while lefthook's git hooks are not installed — or
//      with them turned off for the one command (`--no-verify`, `commit -n`,
//      `-c core.hooksPath=…`). `lefthook.yml` is the layer that actually owns
//      local validation: `pre-commit` formats staged files, `commit-msg` runs
//      commitlint, and `pre-push` mirrors the CI `check` + `lint` +
//      `forbid-skip` jobs. When those hooks are absent or skipped every one of
//      those gates is silently off, and the first signal is a red PR. `just
//      hooks-install` is the fix; `LEFTHOOK=0 git push` is the documented,
//      visible escape hatch for a WIP push.
//
// WHAT "AIMED AT MAIN" MEANS. A push is judged by its REFSPECS, never by the
// branch the shell happens to be on: `git push origin feat/x:feat/x` from a
// checkout on `main` targets `feat/x`, and `git push origin --delete
// some-feature` targets nothing protected, while `git push origin HEAD:main`
// from any branch does. Only a push with no refspec (bare `git push`) is
// judged by the current branch, because that is what it pushes. A commit is
// judged by the current branch. The line is tokenised like a shell (see
// shell-words.mjs): a commit message that says "git push origin main is
// refused" is data, `bash -c "git push origin HEAD:main"` is a push, and the
// directory a git command runs in is the LAST `cd` before it (or its own
// `-C`), not the first `cd` on the line.
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

import { segments } from './shell-words.mjs';

const ALLOW = 0;
const BLOCK = 2;

const PROTECTED_BRANCH = 'main';
const GUARDED_SUBCOMMANDS = new Set(['commit', 'push']);
const FULL_CI_ENV = 'CERBERUS_PRECOMMIT_FULL_CI';

// Git's own global options that consume the following argument, so the
// subcommand scanner does not mistake their value for the subcommand.
const GIT_GLOBAL_OPTS_WITH_VALUE = new Set(['-C', '-c', '--git-dir', '--work-tree', '--namespace', '--exec-path']);

// `git push` options that consume the following argument, so a value is never
// read as the remote or a refspec.
const PUSH_OPTS_WITH_VALUE = new Set(['-o', '--push-option', '--receive-pack', '--exec', '--repo']);

function readPayload() {
  try {
    return JSON.parse(readFileSync(0, 'utf8'));
  } catch {
    return null;
  }
}

function resolveDir(dir, base) {
  const abs = isAbsolute(dir) ? dir : join(base, dir);
  return existsSync(abs) ? abs : null;
}

// parseGit — given one executed segment's words (wrappers already stripped by
// shell-words.mjs), return the git invocation it is, or null:
//   { sub, args, dir, hooksOff } — the subcommand, its own arguments, the
//   `-C <dir>` value if any, and whether a global `-c core.hooksPath=…` turned
//   the repository's hooks off for this one invocation.
function parseGit(w) {
  if (w.length === 0 || (w[0] !== 'git' && !w[0].endsWith('/git'))) return null;
  let i = 1;
  let dir = null;
  let hooksOff = false;
  while (i < w.length) {
    const a = w[i];
    if (!a.startsWith('-')) return { sub: a, args: w.slice(i + 1), dir, hooksOff };
    if (a === '-C') dir = w[i + 1] ?? dir;
    if (a === '-c' && /^core\.hooksPath=/i.test(w[i + 1] ?? '')) hooksOff = true;
    if (a.startsWith('-c') && a.length > 2 && /^-ccore\.hooksPath=/i.test(a)) hooksOff = true;
    i += GIT_GLOBAL_OPTS_WITH_VALUE.has(a) ? 2 : 1;
  }
  return null;
}

// hooksBypassed — the subcommand's own way of skipping the hooks. `commit -n`
// is `--no-verify`; `push -n` is `--dry-run`, which skips nothing and pushes
// nothing, so it is not a bypass.
function hooksBypassed(sub, args) {
  if (args.includes('--no-verify')) return true;
  if (sub === 'commit') return args.some((a) => /^-[a-zA-Z]*n[a-zA-Z]*$/.test(a) && !a.startsWith('--'));
  return false;
}

// pushDestinations — the branches a `git push` writes to, from its refspecs
// alone: `origin main`, `HEAD:main`, `+HEAD:refs/heads/main`, `:main` (a
// delete), `--delete main`. An `HEAD` destination is the current branch; no
// refspec at all (bare `git push`, `git push origin`) is the current branch too.
function pushDestinations(args, cwd) {
  const positional = [];
  for (let i = 0; i < args.length; i++) {
    const a = args[i];
    if (a === '--') { positional.push(...args.slice(i + 1)); break; }
    if (a.startsWith('-')) {
      if (PUSH_OPTS_WITH_VALUE.has(a)) i++;
      continue;
    }
    positional.push(a);
  }
  const refspecs = positional.slice(1); // the first positional is the remote
  if (refspecs.length === 0) return [currentBranch(cwd)];
  return refspecs.map((spec) => {
    const raw = spec.replace(/^\+/, '');
    const dst = raw.includes(':') ? raw.slice(raw.lastIndexOf(':') + 1) : raw;
    return dst === 'HEAD' ? currentBranch(cwd) : dst;
  });
}

function targetsProtected(dst) {
  return dst === PROTECTED_BRANCH || dst === `refs/heads/${PROTECTED_BRANCH}`;
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

// hooksDir — where git will look for hook scripts in THIS working tree.
// `core.hooksPath` wins when set; otherwise hooks live in the common git dir,
// which is what makes the check work identically from a linked worktree.
// `--path-format=absolute` matters: in a non-linked checkout git prints the
// relative `.git`, which joined against the HOOK PROCESS's cwd is another
// repository's hooks directory whenever the command `cd`-ed somewhere else.
function hooksDir(cwd) {
  try {
    const configured = git(['config', '--get', 'core.hooksPath'], cwd);
    if (configured) return isAbsolute(configured) ? configured : join(git(['rev-parse', '--show-toplevel'], cwd), configured);
  } catch {
    // core.hooksPath unset: `git config --get` exits 1, which is the common case.
  }
  return join(git(['rev-parse', '--path-format=absolute', '--git-common-dir'], cwd), 'hooks');
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

  // Walk the executed segments in order, tracking the LAST `cd` before each
  // git invocation: agents reach a worktree with `cd <worktree> && git …`, and
  // `cd /tmp && cd <worktree> && git …` runs in the second directory, not the
  // first. `git -C <dir>` on the invocation itself binds tighter than any cd.
  const guarded = [];
  let cwd = payloadCwd;
  for (const w of segments(command)) {
    if (w[0] === 'cd') {
      const target = w[1] && !w[1].startsWith('-') ? w[1] : null;
      const resolved = target ? resolveDir(target, cwd) : null;
      if (resolved) cwd = resolved;
      continue;
    }
    const inv = parseGit(w);
    if (!inv || !GUARDED_SUBCOMMANDS.has(inv.sub)) continue;
    const segCwd = (inv.dir && resolveDir(inv.dir, cwd)) || cwd;
    guarded.push({ ...inv, cwd: segCwd });
  }
  if (guarded.length === 0) return ALLOW;

  for (const g of guarded) {
    const targets = g.sub === 'push' ? pushDestinations(g.args, g.cwd) : [currentBranch(g.cwd)];
    if (targets.some(targetsProtected)) {
      return block([
        `guard-git: refusing to ${g.sub} against \`${PROTECTED_BRANCH}\`.`,
        'This repository is PR-per-change and branch protection rejects direct pushes to main.',
        'Branch off the current origin/main, then push and `gh pr create` in the same step.',
      ]);
    }
    if (g.hooksOff || hooksBypassed(g.sub, g.args)) {
      return block([
        `guard-git: refusing to ${g.sub} with the git hooks turned off (--no-verify / -n / core.hooksPath).`,
        'lefthook owns local validation: the formatters, commitlint, and the pre-push mirror of',
        'the CI check / lint / forbid-skip gates. Bypassing it here is the "silently off" state',
        'this guard exists to prevent. For a WIP push use `LEFTHOOK=0 git push`, which says so.',
      ]);
    }
  }

  const cwdOfFirst = guarded[0].cwd;
  const hookNames = guarded.some((g) => g.sub === 'push') ? ['pre-commit', 'commit-msg', 'pre-push'] : ['pre-commit', 'commit-msg'];
  const hooks = lefthookInstalled(cwdOfFirst, hookNames);
  if (!hooks.ok) {
    return block([
      `guard-git: lefthook's git hooks are not installed — ${hooks.reason}.`,
      'Without them the formatters, commitlint, and the pre-push mirror of the CI',
      'check / lint / forbid-skip gates are all silently off.',
      'Fix with: just hooks-install',
    ]);
  }

  if (process.env[FULL_CI_ENV] === '1') {
    const ci = runFullCI(cwdOfFirst);
    if (!ci.ok) {
      return block([`guard-git: ${FULL_CI_ENV}=1 is set and ${ci.reason}.`, 'Fix the failure, or unset the variable to fall back to the lefthook + CI layers.']);
    }
  }

  return ALLOW;
}

// Only dispatch when run as the hook — a test may import the module.
if (process.argv[1] && import.meta.url === new URL(`file://${process.argv[1]}`).href) {
  process.exit(main());
}
