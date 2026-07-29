// compat-publish-score.mjs — publish one head's shields.io endpoint-badge
// JSON to the `compat-scores` orphan branch, extracted from the THREE
// identical "Publish score to compat-scores branch" steps in
// .github/workflows/compatibility.yml (prometheus / tempo / loki).
//
// The branch is an orphan branch in the same repo holding badge JSON at
// badges/<head>.json; the README badges hit the raw URL
// https://raw.githubusercontent.com/tsouza/cerberus/compat-scores/badges/<head>.json.
//
// Three things make this more than a `git push`, and are why it is a module
// rather than inline YAML:
//
//   Schema projection — the run's compat-score.json carries cerberus-only
//   bookkeeping (passed / total / percent) alongside the badge keys.
//   shields.io's endpoint schema is strict and renders an "invalid
//   properties: passed, total, percent" SVG title if it sees them, so the
//   file is projected down to the four schema keys before publishing.
//
//   Orphan bootstrap — on the very first run origin/compat-scores does not
//   exist, so the branch is created as an orphan and badges/ becomes its
//   entire initial state rather than a copy of main.
//
//   Concurrency — the three head jobs finish near-simultaneously and race to
//   push to the same branch. A non-fast-forward rejection is expected, not
//   exceptional: re-fetch the new tip, re-apply this head's badge over it,
//   and push again, capped so an unreachable remote cannot spin forever.
//
// All of that happens in a throwaway `git worktree` under $RUNNER_TEMP, never
// in the job's checkout. The job's checkout is a shared resource: later steps
// read it, and the runner resolves a local composite action's POST phase out
// of it after the last step. Switching it to an orphan branch whose entire
// content is badges/ therefore fails the job with "Can't find 'action.yml' …
// under .github/actions/<name>", long after this step reported success. A
// worktree gets the same shared object store and the same actions/checkout
// credentials without any of that blast radius, and is torn down on every
// exit path.
//
// Env contract:
//   HEAD  head name; names the badge file badges/<HEAD>.json (prometheus|tempo|loki)
//   SRC   path to that head's compat-score.json
//
// The workflow gates the step with `if: github.event_name == 'push' &&
// github.ref == 'refs/heads/main'`, so this module never runs on a pull
// request and needs no event check of its own.
//
// Exit codes: 0 published / nothing to publish / already up to date;
// 1 when every push attempt was rejected or a git step failed.

import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import process from 'node:process';
import { error, git, log, warning } from './lib/gh.mjs';

// The orphan branch and the directory within it that the README badge URLs
// resolve against — changing either breaks every published badge.
const SCORES_BRANCH = 'compat-scores';
const BADGES_DIR = 'badges';

// Identity for the badge commits, passed per-invocation with `git -c` rather
// than written with `git config`: worktrees share one .git/config, so a
// `git config` here would outlive the publish and mutate the job's checkout.
const BOT_NAME = 'cerberus-compat-bot';
const BOT_EMAIL = 'cerberus-compat-bot@users.noreply.github.com';

// The scratch worktree: a fresh directory under the runner's temp area (any
// tmpdir off-runner), named so a leaked one is attributable in a log.
const WORKTREE_PREFIX = 'compat-scores-publish-';
const WORKTREE_DIR = 'scores';

// One title for every annotation this module raises, so the three failure
// shapes group under a single heading in the job log.
const FAIL_TITLE = 'compat-scores publish failed';

// A rejected push means another head won the race, so retrying is the
// expected path, not error handling. The cap bounds the case where the
// remote is unreachable rather than merely contended; the backoff spaces
// attempts so three simultaneous heads deterministically de-collide.
const MAX_ATTEMPTS = 5;
const RETRY_BACKOFF_STEP_MS = 2000;

// The four keys shields.io's endpoint-badge schema accepts. Anything else
// in the score file is cerberus-side bookkeeping and must not be published.
const SHIELDS_KEYS = ['schemaVersion', 'label', 'message', 'color'];

// shieldsSubset projects a score object down to the shields.io endpoint
// schema, preserving key order and rendering a missing key as null — the
// same projection `jq '{schemaVersion, label, message, color}'` performs,
// down to the byte, so a run that publishes an unchanged score still
// produces an empty diff rather than a no-op commit.
export function shieldsSubset(score) {
  const out = {};
  for (const key of SHIELDS_KEYS) out[key] = score?.[key] ?? null;
  return `${JSON.stringify(out, null, 2)}\n`;
}

// retryDelayMs is the wait before re-attempting after `attempt` was rejected.
// It grows with the attempt number so three heads pushing at once spread out
// instead of re-colliding on a fixed cadence.
export function retryDelayMs(attempt) {
  return (attempt + 1) * RETRY_BACKOFF_STEP_MS;
}

// sleepSync blocks the loop between push attempts. The retry cadence has to
// hold across a git push, so a synchronous wait keeps the whole publish a
// single straight-line sequence rather than an async state machine.
function sleepSync(ms) {
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);
}

// PublishError is a failure this module has something to say about: it
// carries the ::error:: body. Throwing rather than exiting is what makes the
// worktree teardown reachable — process.exit() skips every finally block, so
// an exit hidden inside a helper would leak the worktree it was created under.
class PublishError extends Error {}

// mustGit runs a git command that has no meaningful failure mode other than
// "the runner is broken", and reports the command's own stderr when it does
// fail — mirroring `set -e` in the bash this replaces.
function mustGit(args, opts) {
  const res = git(args, opts);
  if (res.status !== 0) {
    const why = res.stderr.trim() || res.stdout.trim();
    throw new PublishError(`git ${args.join(' ')} failed: ${why}`);
  }
  return res.stdout;
}

// removeWorktree tears down the scratch worktree and its temp parent. A
// failure here cannot fail the job: the badge is already published or already
// reported as unpublishable, and the runner discards its temp area anyway.
function removeWorktree(worktree, parent) {
  // Absent when `git worktree add` is what failed; there is then nothing
  // registered to remove and only the empty temp parent to drop.
  if (existsSync(worktree)) {
    const res = git(['worktree', 'remove', '--force', worktree]);
    if (res.status !== 0) {
      warning(`could not remove scratch worktree ${worktree}: ${res.stderr.trim()}`);
      git(['worktree', 'prune']);
    }
  }
  rmSync(parent, { recursive: true, force: true });
}

// publishFrom does the actual publish inside `worktree`, and returns the
// process exit code. Every git call is scoped to the worktree, so the job's
// checkout is never read, written, or switched.
function publishFrom(worktree, head, badge) {
  const wtGit = (args) => git(args, { cwd: worktree });
  const mustWtGit = (args) => mustGit(args, { cwd: worktree });

  for (let attempt = 1; attempt <= MAX_ATTEMPTS; attempt++) {
    log(`==> publish attempt ${attempt} / ${MAX_ATTEMPTS}`);

    // A fetch failure is not fatal on its own: the branch may simply not exist
    // yet, which the rev-parse below distinguishes.
    wtGit(['fetch', 'origin', SCORES_BRANCH]);

    if (wtGit(['rev-parse', '--verify', `origin/${SCORES_BRANCH}`]).status === 0) {
      mustWtGit(['checkout', '-B', SCORES_BRANCH, `origin/${SCORES_BRANCH}`]);
    } else {
      // First run: `switch --orphan` starts the branch with an empty index and
      // an empty working tree, so badges/ is its entire initial state without
      // main's tree ever being materialised here.
      mustWtGit(['switch', '--orphan', SCORES_BRANCH]);
    }

    mkdirSync(path.join(worktree, BADGES_DIR), { recursive: true });
    writeFileSync(path.join(worktree, BADGES_DIR, `${head}.json`), badge);
    mustWtGit(['add', `${BADGES_DIR}/`]);

    if (wtGit(['diff', '--staged', '--quiet']).status === 0) {
      log('score unchanged; no commit needed');
      return 0;
    }

    mustWtGit([
      '-c',
      `user.name=${BOT_NAME}`,
      '-c',
      `user.email=${BOT_EMAIL}`,
      'commit',
      '-m',
      `${SCORES_BRANCH}: update ${head} score`,
    ]);

    if (wtGit(['push', 'origin', SCORES_BRANCH]).status === 0) {
      log(`==> published ${head} score on attempt ${attempt}`);
      return 0;
    }

    log('==> push rejected (likely concurrent update); retrying');
    // Drop back to a clean tree so the next attempt's checkout starts from a
    // state git will fast-forward rather than refuse.
    mustWtGit(['reset', '--hard']);
    // No backoff after the final attempt — there is nothing left to wait for,
    // and the bash this replaces spent its longest sleep on the way to exit 1.
    if (attempt < MAX_ATTEMPTS) sleepSync(retryDelayMs(attempt));
  }

  throw new PublishError(`could not push ${head} score after ${MAX_ATTEMPTS} attempts`);
}

// run returns the process exit code, or throws PublishError.
function run() {
  const head = process.env.HEAD || '';
  const src = process.env.SRC || '';

  if (!head || !src) {
    throw new PublishError('compat-publish-score.mjs: HEAD and SRC env vars are required');
  }

  // A missing score file means the harness failed before the scorer ran. That
  // failure is already raised by the job's own "Fail job" step; publishing has
  // simply got nothing to do, so it must not add a second red herring.
  if (!existsSync(src)) {
    log(`no score file at ${src}; nothing to publish`);
    return 0;
  }

  let badge;
  try {
    badge = shieldsSubset(JSON.parse(readFileSync(src, 'utf8')));
  } catch (e) {
    throw new PublishError(`could not read ${src}: ${e.message}`);
  }

  // mkdtemp gives the parent; the worktree itself must not exist yet, since
  // `git worktree add` refuses a path that is already there.
  const parent = mkdtempSync(path.join(process.env.RUNNER_TEMP || tmpdir(), WORKTREE_PREFIX));
  const worktree = path.join(parent, WORKTREE_DIR);

  // --no-checkout: nothing of HEAD is written here. The branch checkout below
  // populates only badges/, so the scratch tree stays a few kB on a runner
  // that has already had to reclaim disk to fit the compatibility stack.
  try {
    mustGit(['worktree', 'add', '--no-checkout', '--detach', worktree, 'HEAD']);
    return publishFrom(worktree, head, badge);
  } finally {
    removeWorktree(worktree, parent);
  }
}

// main funnels every outcome through one return so the teardown in run() is
// the last thing that happens before the process exits.
function main() {
  try {
    return run();
  } catch (e) {
    if (!(e instanceof PublishError)) throw e;
    error(e.message, { title: FAIL_TITLE });
    return 1;
  }
}

if (import.meta.url === `file://${process.argv[1]}`) {
  process.exit(main());
}
