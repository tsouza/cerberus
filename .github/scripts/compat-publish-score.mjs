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
//   exist, so the branch is created with --orphan and its tree cleared, and
//   badges/ becomes the branch's initial state.
//
//   Concurrency — the three head jobs finish near-simultaneously and race to
//   push to the same branch. A non-fast-forward rejection is expected, not
//   exceptional: re-fetch the new tip, re-apply this head's badge over it,
//   and push again, capped so an unreachable remote cannot spin forever.
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

import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { error, git, log } from './lib/gh.mjs';

// The orphan branch and the directory within it that the README badge URLs
// resolve against — changing either breaks every published badge.
const SCORES_BRANCH = 'compat-scores';
const BADGES_DIR = 'badges';

// Identity for the badge commits. A distinct bot name keeps the branch's
// history legible next to release commits from the same token.
const BOT_NAME = 'cerberus-compat-bot';
const BOT_EMAIL = 'cerberus-compat-bot@users.noreply.github.com';

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

// mustGit runs a git command that has no meaningful failure mode other than
// "the runner is broken", and exits 1 with the command's own stderr when it
// does fail — mirroring `set -e` in the bash this replaces.
function mustGit(args) {
  const res = git(args);
  if (res.status !== 0) {
    error(`git ${args.join(' ')} failed: ${res.stderr.trim() || res.stdout.trim()}`, {
      title: 'compat-scores publish failed',
    });
    process.exit(1);
  }
  return res.stdout;
}

function main() {
  const head = process.env.HEAD || '';
  const src = process.env.SRC || '';

  if (!head || !src) {
    error('compat-publish-score.mjs: HEAD and SRC env vars are required', {
      title: 'compat-scores publish failed',
    });
    process.exit(1);
  }

  // A missing score file means the harness failed before the scorer ran. That
  // failure is already raised by the job's own "Fail job" step; publishing has
  // simply got nothing to do, so it must not add a second red herring.
  if (!existsSync(src)) {
    log(`no score file at ${src}; nothing to publish`);
    process.exit(0);
  }

  // Read and project BEFORE touching the branch: the orphan-bootstrap path
  // clears the working tree, which would take the score file with it.
  let badge;
  try {
    badge = shieldsSubset(JSON.parse(readFileSync(src, 'utf8')));
  } catch (e) {
    error(`could not read ${src}: ${e.message}`, { title: 'compat-scores publish failed' });
    process.exit(1);
  }

  mustGit(['config', 'user.name', BOT_NAME]);
  mustGit(['config', 'user.email', BOT_EMAIL]);

  for (let attempt = 1; attempt <= MAX_ATTEMPTS; attempt++) {
    log(`==> publish attempt ${attempt} / ${MAX_ATTEMPTS}`);

    // A fetch failure is not fatal on its own: the branch may simply not exist
    // yet, which the rev-parse below distinguishes.
    git(['fetch', 'origin', SCORES_BRANCH]);

    if (git(['rev-parse', '--verify', `origin/${SCORES_BRANCH}`]).status === 0) {
      mustGit(['checkout', '-B', SCORES_BRANCH, `origin/${SCORES_BRANCH}`]);
    } else {
      // First run: detach onto an orphan, then clear index and working tree so
      // badges/ is the branch's entire initial state, not a copy of main.
      mustGit(['checkout', '--orphan', SCORES_BRANCH]);
      git(['rm', '-rf', '.']); // an empty index on a fresh orphan is not a failure
      mustGit(['clean', '-fdx']);
    }

    mkdirSync(BADGES_DIR, { recursive: true });
    writeFileSync(path.join(BADGES_DIR, `${head}.json`), badge);
    mustGit(['add', `${BADGES_DIR}/`]);

    if (git(['diff', '--staged', '--quiet']).status === 0) {
      log('score unchanged; no commit needed');
      process.exit(0);
    }

    mustGit(['commit', '-m', `${SCORES_BRANCH}: update ${head} score`]);

    if (git(['push', 'origin', SCORES_BRANCH]).status === 0) {
      log(`==> published ${head} score on attempt ${attempt}`);
      process.exit(0);
    }

    log('==> push rejected (likely concurrent update); retrying');
    // Drop back to a clean tree so the next attempt's checkout starts from a
    // state git will fast-forward rather than refuse.
    mustGit(['reset', '--hard']);
    // No backoff after the final attempt — there is nothing left to wait for,
    // and the bash this replaces spent its longest sleep on the way to exit 1.
    if (attempt < MAX_ATTEMPTS) sleepSync(retryDelayMs(attempt));
  }

  error(`could not push ${head} score after ${MAX_ATTEMPTS} attempts`, {
    title: 'compat-scores publish failed',
  });
  process.exit(1);
}

if (import.meta.url === `file://${process.argv[1]}`) {
  main();
}
