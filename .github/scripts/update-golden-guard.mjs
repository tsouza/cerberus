// update-golden-guard.mjs — the required PR check that closes issue #2350.
//
// # The race it closes
//
// PR #2347 dispatched update-golden.yml against its own branch while the PR
// was still open. The PR merged — deleting its head branch, per this repo's
// normal `--delete-branch` convention — while the dispatch was still
// regenerating. Every regenerate leg finished; the publish job's own
// assertTargetUnmoved refused, correctly, to push into a branch that no
// longer existed. The already-computed, correct regenerated diff was simply
// lost, and the PR's code (which needed it) landed on `main` with stale
// goldens.
//
// Nothing in the merge path saw that dispatch coming. This script is the
// thing that does: run as a required status check on every pull_request
// event, it blocks merge for exactly as long as an update-golden.yml run
// against the PR's own branch is queued or in progress, and clears the
// moment none is — never opining on whether that run SUCCEEDED, only on
// whether it is still touching the branch. A run that finished (however it
// concluded) is no longer a race hazard; a golden that came out stale is a
// job for the ordinary golden-drift checks that run on the resulting push,
// not this one.
//
// # How it finds "targets this branch" at all
//
// The Actions API's run-list endpoint never exposes workflow_dispatch INPUTS
// — only `head_branch`, which for a dispatch is the ref the workflow was
// TRIGGERED from (always `main` for update-golden.yml), never the target
// branch the dispatch names via `-f branch=`. update-golden.yml works around
// that with its own `run-name: update-golden[${{ inputs.branch }}]`, which
// the API surfaces as `display_title`. runTargetsBranch() is the one place
// that shape is parsed; keep it in sync with that `run-name:` line.
//
// # Why this polls instead of failing once and asking for a re-run
//
// A required check only ever gates the PR's CURRENT head SHA. If it failed
// once while a run was in flight and never re-evaluated, the PR would stay
// red after the run finished until some unrelated event (a new push)
// happened to re-trigger it — exactly the friction a human would route
// around. Polling from inside the one job run instead means the SAME check
// run clears itself the moment the hazard is gone, with no second event
// needed. MAX_WAIT_MS bounds that loop so a stuck dispatch fails loudly
// rather than hanging the job (and this check) forever.
//
// Env contract:
//   GH_TOKEN        (required) a token with `actions: read` on this repo.
//   REPO             (required) `owner/repo`.
//   BRANCH           (required) the PR's head branch (github.head_ref).
//   API_URL          (optional) GitHub REST API base. Default the public API.
//   POLL_INTERVAL_MS (optional) delay between polls. Default 30s.
//   MAX_WAIT_MS      (optional) total time budget before failing loudly.
//                     Default 60 minutes — update-golden.yml's own regenerate
//                     legs are capped at 45, plus plan/publish overhead.
//
// Exit codes:
//   0  no update-golden.yml run is queued or in_progress against BRANCH.
//   1  one still is after MAX_WAIT_MS, or the API calls themselves failed.

import process from 'node:process';

import { error, log, notice } from './lib/gh.mjs';

const DEFAULT_API_URL = 'https://api.github.com';
const WORKFLOW_FILE = 'update-golden.yml';
const DEFAULT_POLL_INTERVAL_MS = 30_000;
const DEFAULT_MAX_WAIT_MS = 60 * 60 * 1000;

// The two Actions API run states that mean "still touching the branch". A
// `completed` run — success, failure, or cancelled — is no longer a hazard,
// whatever its conclusion: see the file header on why this check does not
// read conclusion at all.
const IN_FLIGHT_STATUSES = ['in_progress', 'queued'];

function apiHeaders(token) {
  return {
    accept: 'application/vnd.github+json',
    authorization: `Bearer ${token}`,
    'x-github-api-version': '2022-11-28',
    'user-agent': 'cerberus-update-golden-guard',
  };
}

async function ghJSON(url, token, init = {}) {
  const res = await fetch(url, { ...init, headers: { ...apiHeaders(token), ...(init.headers ?? {}) } });
  if (!res.ok) {
    throw new Error(`${init.method ?? 'GET'} ${url} -> ${res.status} ${res.statusText}: ${await res.text()}`);
  }
  return res.status === 204 ? null : res.json();
}

/**
 * The exact run-name shape update-golden.yml stamps on every dispatch. Kept
 * as one function so a shape change only needs to move in one place, on
 * either side.
 */
export function runTargetsBranch(displayTitle, branch) {
  return displayTitle === `update-golden[${branch}]`;
}

/**
 * Every currently in_progress or queued update-golden.yml run, across the
 * whole repository — not just this branch's. Filtering by branch happens in
 * the caller, over runTargetsBranch(); the API itself cannot filter on an
 * input value.
 *
 * Two requests rather than one: the `status` query param accepts only a
 * single value, and `queued` matters as much as `in_progress` — a second
 * dispatch against a branch already mid-regeneration serialises behind the
 * first via update-golden.yml's own concurrency group instead of running
 * beside it, so the hazard window spans both.
 */
export async function listInFlightRuns({ api, repo, token, fetchJSON = ghJSON }) {
  const runs = [];
  for (const status of IN_FLIGHT_STATUSES) {
    const url = `${api}/repos/${repo}/actions/workflows/${WORKFLOW_FILE}/runs?status=${status}&per_page=100`;
    const page = await fetchJSON(url, token);
    if (!Array.isArray(page?.workflow_runs)) {
      throw new Error(`unexpected response listing ${status} runs of ${WORKFLOW_FILE}: ${JSON.stringify(page)}`);
    }
    runs.push(...page.workflow_runs);
  }
  return runs;
}

/** node:timers/promises' setTimeout, imported lazily so tests can stub it. */
function defaultSleep(ms) {
  return new Promise((resolve) => {
    setTimeout(resolve, ms);
  });
}

/**
 * The core poll loop: block until no update-golden.yml run is in_progress or
 * queued against `branch`, or until `maxWaitMs` has elapsed.
 *
 * `listRuns` is injected so tests drive it from a scripted sequence instead
 * of the network; `sleep` and `now` are injected so a test never actually
 * waits.
 */
export async function waitForBranchClear({
  listRuns,
  branch,
  pollIntervalMs = DEFAULT_POLL_INTERVAL_MS,
  maxWaitMs = DEFAULT_MAX_WAIT_MS,
  sleep = defaultSleep,
  now = Date.now,
  onWaiting,
}) {
  const deadline = now() + maxWaitMs;
  for (;;) {
    const runs = await listRuns();
    const matching = runs.filter((r) => runTargetsBranch(r.display_title, branch));
    if (matching.length === 0) {
      return { clear: true, waitedMs: 0, runs: [] };
    }
    if (now() >= deadline) {
      return { clear: false, timedOut: true, runs: matching };
    }
    onWaiting?.(matching);
    await sleep(Math.min(pollIntervalMs, Math.max(deadline - now(), 0)));
  }
}

export async function main(env = process.env) {
  const token = required(env, 'GH_TOKEN');
  const repo = required(env, 'REPO');
  const branch = required(env, 'BRANCH');
  const api = env.API_URL || DEFAULT_API_URL;
  const pollIntervalMs = numberEnv(env, 'POLL_INTERVAL_MS', DEFAULT_POLL_INTERVAL_MS);
  const maxWaitMs = numberEnv(env, 'MAX_WAIT_MS', DEFAULT_MAX_WAIT_MS);

  const result = await waitForBranchClear({
    listRuns: () => listInFlightRuns({ api, repo, token }),
    branch,
    pollIntervalMs,
    maxWaitMs,
    onWaiting: (runs) => {
      for (const r of runs) log(`  in flight: ${r.html_url} (${r.status})`);
      notice(
        `update-golden-guard: an update-golden.yml dispatch against ${branch} is still running; ` +
          'waiting for it to finish before this check can pass.',
      );
    },
  });

  if (result.clear) {
    notice(`update-golden-guard: no update-golden.yml dispatch is in flight against ${branch}.`);
    return;
  }

  const urls = result.runs.map((r) => r.html_url).join(', ');
  error(
    `update-golden-guard: timed out after ${maxWaitMs}ms waiting for an update-golden.yml ` +
      `dispatch against ${branch} to finish: ${urls}. Wait for it, or cancel it, then re-run ` +
      'this check (or push a new commit) once it is no longer queued or in progress.',
    { title: 'update-golden-guard' },
  );
  process.exitCode = 1;
}

function required(env, name) {
  const value = String(env[name] ?? '').trim();
  if (value === '') {
    error(`${name} is required`, { title: 'update-golden-guard' });
    process.exit(1);
  }
  return value;
}

function numberEnv(env, name, fallback) {
  const raw = env[name];
  if (raw === undefined || raw === '') return fallback;
  const parsed = Number(raw);
  if (!Number.isFinite(parsed) || parsed <= 0) {
    error(`${name} must be a positive number, got ${JSON.stringify(raw)}`, { title: 'update-golden-guard' });
    process.exit(1);
  }
  return parsed;
}

if (import.meta.url === `file://${process.argv[1]}`) {
  main().catch((e) => {
    error(e?.stack ?? String(e), { title: 'update-golden-guard' });
    process.exitCode = 1;
  });
}
