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
// thing that does — never opining on whether an in-flight run SUCCEEDED,
// only on whether it is still touching the branch. A run that finished
// (however it concluded) is no longer a race hazard; a golden that came out
// stale is a job for the ordinary golden-drift checks that run on the
// resulting push, not this one. It runs in two triggers, both required to
// close the race fully:
//
//   - `pull_request` (opened/synchronize/reopened/ready_for_review): the
//     original poll-and-block path. main() blocks for exactly as long as an
//     update-golden.yml run against the PR's own branch is queued or in
//     progress, polling from inside the one job run so the SAME check run
//     clears itself the moment the hazard is gone (see "why this polls"
//     below).
//   - `workflow_run` (requested/completed, for the `update-golden` workflow
//     itself): closes the gap the `pull_request` trigger alone leaves open.
//     A dispatch against an ALREADY-open PR's branch, made after that PR's
//     last push, never fires a new `pull_request` event — GitHub does not
//     re-poll an already-green required check on its own. This trigger
//     reacts to the dispatch directly: `requested` sets the guard context to
//     `pending` on every open PR whose head branch the dispatch targets
//     (found via the Pulls API, since a `workflow_run` job runs in the
//     default branch's context, not the PR's — the check has to be pushed
//     onto the PR's head SHA explicitly via the Statuses API), and
//     `completed` re-checks and flips it to `success` once no run remains
//     in flight against that branch (handling a second, serialised dispatch
//     via the same in-flight query the poll path uses).
//   - `merge_group`: the merge queue's own copy of the poll. See the section
//     below.
//
// # Why the merge queue needs its own poll rather than a free pass
//
// A merge queue moves the MERGE out of the pull request and onto a projected
// trunk: GitHub builds a `gh-readonly-queue/<base>/pr-<n>-<sha>` branch,
// dispatches `merge_group` against it, and merges (deleting the pull
// request's head branch, exactly as before) once every required context is
// green on that projected commit. Two consequences decide this file's
// behaviour there.
//
// First, a required context that never posts on `merge_group` never resolves,
// and the queue entry waits forever — so this check MUST report on the queue,
// whatever it reports.
//
// Second, reporting a free pass would not be conservative, it would be a
// REGRESSION of #2350. The queue widens the very window the guard exists to
// close: a pull request now sits between "last green on its own head" and
// "merged" for the whole duration of merge-group CI, and a dispatch started
// anywhere in that window strands its regenerated diff the same way #2350's
// did. The `workflow_run` path cannot cover it either — that path pushes a
// commit status onto the PULL REQUEST'S head SHA, which is no longer what
// branch protection is evaluating once the entry is in the queue. So the
// merge-group run re-runs the SAME poll, against the branch the queued pull
// request would delete.
//
// Resolving that branch is exact rather than heuristic: GitHub creates one
// `gh-readonly-queue` branch PER QUEUED PULL REQUEST, not one per batch, so a
// merge group's head ref names exactly the pull request that group would
// merge — and every pull request the queue merges gets a group of its own.
// parseQueuedPRNumber() reads the number back out of that ref (anchored on
// the group's own `base_ref`, so a base branch containing slashes cannot be
// mis-split), and the Pulls API turns it into the head branch name that
// `update-golden[<branch>]` run names are matched against. A head ref this
// script cannot parse FAILS the check rather than passing it: an unresolved
// merge group is precisely the state in which the guard has verified nothing.
//
// The residual window is the irreducible one the `pull_request` path already
// has: a dispatch created in the seconds between this poll's last clear read
// and the queue's merge. Nothing in-band can close that — the merge is
// GitHub's to make and there is no transactional handle on it — and it is the
// same exposure every branch-protection check carries.
//
// # How it finds "targets this branch" at all
//
// The Actions API's run-list endpoint never exposes workflow_dispatch INPUTS
// — only `head_branch`, which for a dispatch is the ref the workflow was
// TRIGGERED from (always `main` for update-golden.yml), never the target
// branch the dispatch names via `-f branch=`. update-golden.yml works around
// that with its own `run-name: update-golden[${{ inputs.branch }}]`, which
// the API surfaces as `display_title`. parseTargetBranch() / runTargetsBranch()
// are the one place that shape is parsed; keep them in sync with that
// `run-name:` line.
//
// # Why the pull_request path polls instead of failing once and asking for a re-run
//
// A required check only ever gates the PR's CURRENT head SHA. If it failed
// once while a run was in flight and never re-evaluated, the PR would stay
// red after the run finished until some unrelated event (a new push)
// happened to re-trigger it — exactly the friction a human would route
// around. Polling from inside the one job run instead means the SAME check
// run clears itself the moment the hazard is gone, with no second event
// needed. MAX_WAIT_MS bounds that loop so a stuck dispatch fails loudly
// rather than hanging the job (and this check) forever. The workflow_run
// path needs no such loop: GitHub itself re-invokes this script at
// `requested` and again at `completed`, so each invocation only has to take
// one snapshot of the in-flight list.
//
// Env contract (GITHUB_EVENT_NAME selects the branch — set by the runner):
//   pull_request:
//     GH_TOKEN        (required) a token with `actions: read` on this repo.
//     REPO             (required) `owner/repo`.
//     BRANCH           (required) the PR's head branch (github.head_ref).
//     API_URL          (optional) GitHub REST API base. Default public API.
//     POLL_INTERVAL_MS (optional) delay between polls. Default 30s.
//     MAX_WAIT_MS      (optional) total time budget before failing loudly.
//                       Default 60 minutes — update-golden.yml's own
//                       regenerate legs are capped at 45, plus plan/publish
//                       overhead.
//   merge_group:
//     GH_TOKEN              (required) a token with `actions: read` and
//                            `pull-requests: read` on this repo.
//     REPO                  (required) `owner/repo`.
//     MERGE_GROUP_HEAD_REF  (required) github.event.merge_group.head_ref.
//     MERGE_GROUP_BASE_REF  (required) github.event.merge_group.base_ref.
//     API_URL / POLL_INTERVAL_MS / MAX_WAIT_MS as for pull_request.
//   workflow_run:
//     GH_TOKEN                   (required) a token with `actions: read`,
//                                 `pull-requests: read` and `statuses: write`.
//     REPO                        (required) `owner/repo`.
//     WORKFLOW_RUN_DISPLAY_TITLE  (required) github.event.workflow_run.display_title.
//     WORKFLOW_RUN_HTML_URL       (optional) github.event.workflow_run.html_url,
//                                 used as the status's target_url.
//     API_URL                     (optional) GitHub REST API base.
//
// Exit codes:
//   0  no update-golden.yml run is queued or in_progress against the guarded
//      branch (pull_request, merge_group), or the workflow_run event was
//      handled (whatever state it resulted in — the Statuses API call failing
//      is the only workflow_run failure mode).
//   1  one still is after MAX_WAIT_MS, the merge group's head ref could not be
//      resolved to a pull request, or the API calls themselves failed.

import process from 'node:process';

import { error, log, notice } from './lib/gh.mjs';

const DEFAULT_API_URL = 'https://api.github.com';
const WORKFLOW_FILE = 'update-golden.yml';
const DEFAULT_POLL_INTERVAL_MS = 30_000;
const DEFAULT_MAX_WAIT_MS = 60 * 60 * 1000;

// The Actions API run states that mean "still touching the branch". A
// `completed` run — success, failure, or cancelled — is no longer a hazard,
// whatever its conclusion: see the file header on why this check does not
// read conclusion at all. `requested` is included alongside `in_progress`
// and `queued`: it is the transient status a workflow_dispatch run briefly
// reports between being created and being picked up by a runner, and a poll
// (or a workflow_run "requested" snapshot, see below) landing in that window
// must still see the run as in flight rather than reporting a false-clear.
const IN_FLIGHT_STATUSES = ['requested', 'in_progress', 'queued'];

// The context name this script publishes to when it sets a commit status
// directly (the workflow_run path — see the file header). Kept identical to
// the job name update-golden-guard.yml uses for its pull_request-triggered
// check-run so branch protection's single required-check entry is satisfied
// by either source.
const STATUS_CONTEXT = 'update-golden-guard';

// The two non-default `GITHUB_EVENT_NAME` values main() branches on. The
// default — anything else — is the original pull_request poll path.
const WORKFLOW_RUN_EVENT = 'workflow_run';
const MERGE_GROUP_EVENT = 'merge_group';

// The merge-queue branch shape GitHub stamps on a merge group's head ref:
//   refs/heads/gh-readonly-queue/<base branch>/pr-<number>-<base sha>
// Split into a prefix and a trailing segment pattern rather than one regex
// over the whole ref, because the <base branch> in the middle may itself
// contain slashes: the caller anchors on the group's own base_ref instead of
// guessing where the base name ends.
const QUEUE_REF_PREFIX = 'gh-readonly-queue/';
const REFS_HEADS_PREFIX = 'refs/heads/';
const QUEUE_PR_SEGMENT_RE = /^pr-(\d+)-[0-9a-fA-F]+$/;

/** A ref with the `refs/heads/` prefix removed, if it carried one. */
function branchName(ref) {
  return ref.startsWith(REFS_HEADS_PREFIX) ? ref.slice(REFS_HEADS_PREFIX.length) : ref;
}

/**
 * The pull-request number a merge group is validating, read back out of the
 * `gh-readonly-queue/<base>/pr-<number>-<sha>` branch GitHub builds for it, or
 * null if the head ref does not have that shape under this group's own base
 * ref. GitHub creates one such branch per QUEUED PULL REQUEST rather than one
 * per batch, so this resolves the whole of what the group would merge — see
 * the file header.
 *
 * Both refs are accepted with or without a `refs/heads/` prefix: the webhook
 * payload carries `base_ref` fully qualified and `head_ref` has been observed
 * both ways.
 */
export function parseQueuedPRNumber(headRef, baseRef) {
  if (typeof headRef !== 'string' || typeof baseRef !== 'string') return null;
  const base = branchName(baseRef);
  if (base === '') return null;
  const head = branchName(headRef);
  const prefix = `${QUEUE_REF_PREFIX}${base}/`;
  if (!head.startsWith(prefix)) return null;
  const match = QUEUE_PR_SEGMENT_RE.exec(head.slice(prefix.length));
  return match === null ? null : Number(match[1]);
}

/**
 * Raised when a `merge_group` run cannot tell which pull request its projected
 * trunk would merge. It is a hard failure rather than a pass: an unresolved
 * merge group is exactly the state in which this check has verified nothing,
 * and certifying it would re-open #2350 on the queue path.
 */
export class UnresolvableMergeGroupError extends Error {
  constructor(headRef, baseRef) {
    super(
      `update-golden-guard: merge_group head ref ${JSON.stringify(headRef)} does not have the ` +
        `${QUEUE_REF_PREFIX}<base>/pr-<number>-<sha> shape GitHub stamps on a queue branch under ` +
        `base ref ${JSON.stringify(baseRef)}, so the pull request this group would merge — and ` +
        'therefore the branch an update-golden.yml dispatch might still be regenerating — cannot ' +
        'be resolved. Failing rather than certifying a merge this check did not verify.',
    );
    this.name = 'UnresolvableMergeGroupError';
  }
}

/**
 * The head BRANCH of one pull request by number. The merge-group path needs
 * it because `update-golden[<branch>]` run names carry a branch name, while a
 * queue branch carries only the pull request's number.
 */
export async function findPRHeadBranch({ api, repo, token, number, fetchJSON = ghJSON }) {
  const pr = await fetchJSON(`${api}/repos/${repo}/pulls/${number}`, token);
  const ref = pr?.head?.ref;
  if (typeof ref !== 'string' || ref.trim() === '') {
    throw new Error(`unexpected response reading PR #${number}: no head.ref in ${JSON.stringify(pr)}`);
  }
  return ref;
}

/**
 * The branch whose in-flight update-golden.yml dispatches this run must gate
 * on. On `pull_request` that is the PR's own head branch, handed over by the
 * runner. On `merge_group` it is the head branch of the pull request the queue
 * would merge, resolved from the group's own refs.
 */
export async function resolveGuardedBranch({
  eventName,
  env,
  token,
  repo,
  api,
  findHeadBranch = findPRHeadBranch,
}) {
  if (eventName !== MERGE_GROUP_EVENT) return required(env, 'BRANCH');

  const headRef = required(env, 'MERGE_GROUP_HEAD_REF');
  const baseRef = required(env, 'MERGE_GROUP_BASE_REF');
  const number = parseQueuedPRNumber(headRef, baseRef);
  if (number === null) throw new UnresolvableMergeGroupError(headRef, baseRef);

  const branch = await findHeadBranch({ api, repo, token, number });
  notice(
    `update-golden-guard: merge group ${headRef} would merge PR #${number} (head branch ` +
      `${branch}); guarding that branch, not the queue's own projected ref.`,
  );
  return branch;
}

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

const RUN_NAME_PREFIX = 'update-golden[';
const RUN_NAME_SUFFIX = ']';

/**
 * The inverse of the `update-golden[${branch}]` run-name shape
 * update-golden.yml stamps on every dispatch: extracts the branch back out
 * of a display_title, or null if the title does not have that shape at all
 * (an unrelated workflow_run event, or a malformed one). Kept as one
 * function, alongside runTargetsBranch() below, so a shape change only
 * needs to move in one place, on either side.
 */
export function parseTargetBranch(displayTitle) {
  if (
    typeof displayTitle !== 'string' ||
    !displayTitle.startsWith(RUN_NAME_PREFIX) ||
    !displayTitle.endsWith(RUN_NAME_SUFFIX) ||
    displayTitle.length < RUN_NAME_PREFIX.length + RUN_NAME_SUFFIX.length
  ) {
    return null;
  }
  return displayTitle.slice(RUN_NAME_PREFIX.length, displayTitle.length - RUN_NAME_SUFFIX.length);
}

export function runTargetsBranch(displayTitle, branch) {
  return parseTargetBranch(displayTitle) === branch;
}

/**
 * Every currently requested, in_progress or queued update-golden.yml run,
 * across the whole repository — not just this branch's. Filtering by branch
 * happens in the caller, over runTargetsBranch(); the API itself cannot
 * filter on an input value.
 *
 * One request per status rather than one: the `status` query param accepts
 * only a single value, and each of the three matters — `queued` because a
 * second dispatch against a branch already mid-regeneration serialises
 * behind the first via update-golden.yml's own concurrency group instead of
 * running beside it, and `requested` because it is the transient status a
 * fresh dispatch briefly reports before a runner picks it up (see
 * IN_FLIGHT_STATUSES above) — so the hazard window spans all three.
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

/**
 * Every open PR (in this repo) whose head branch is exactly `branch`. Used
 * only from the workflow_run path: that job runs in the default branch's
 * context, with no PR of its own, so it has to look the PR up by branch name
 * to know which head SHA to push a commit status onto.
 */
export async function findOpenPRsForBranch({ api, repo, token, branch, fetchJSON = ghJSON }) {
  const owner = repo.split('/')[0];
  const url = `${api}/repos/${repo}/pulls?state=open&head=${encodeURIComponent(`${owner}:${branch}`)}&per_page=100`;
  const prs = await fetchJSON(url, token);
  if (!Array.isArray(prs)) {
    throw new Error(`unexpected response listing open PRs for branch ${branch}: ${JSON.stringify(prs)}`);
  }
  return prs;
}

/**
 * Push a commit status onto `sha` under the STATUS_CONTEXT context. This is
 * what lets a workflow_run-triggered job — which has no check-run of its own
 * on the PR, since it did not run FROM the PR — gate that PR's merge anyway,
 * the same way the pull_request-triggered job's own check-run does.
 */
export async function setCommitStatus({ api, repo, token, sha, state, description, targetUrl, postJSON = ghJSON }) {
  const url = `${api}/repos/${repo}/statuses/${sha}`;
  await postJSON(url, token, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({
      state,
      // The Statuses API caps description at 140 characters and rejects a
      // longer one outright.
      description: description.slice(0, 140),
      context: STATUS_CONTEXT,
      target_url: targetUrl || undefined,
    }),
  });
}

/**
 * The workflow_run path: given the display_title of an update-golden.yml
 * run that just transitioned (requested or completed), find every open PR
 * that run targets and push a commit status reflecting the CURRENT in-flight
 * state for that branch — not merely this one run's own state, since a
 * second, serialised dispatch can still be in flight after the first
 * completes. A display_title that doesn't have the `update-golden[branch]`
 * shape, or that names a branch with no open PR, is a no-op: there is
 * nothing to gate.
 */
export async function runForWorkflowRunEvent({
  env,
  token,
  repo,
  api,
  listRuns = listInFlightRuns,
  findPRs = findOpenPRsForBranch,
  pushStatus = setCommitStatus,
}) {
  const displayTitle = required(env, 'WORKFLOW_RUN_DISPLAY_TITLE');
  const targetUrl = env.WORKFLOW_RUN_HTML_URL || undefined;
  const branch = parseTargetBranch(displayTitle);
  if (branch === null) {
    notice(
      `update-golden-guard: workflow_run display_title ${JSON.stringify(displayTitle)} does not match the ` +
        'update-golden[<branch>] shape; nothing to guard.',
    );
    return;
  }

  const prs = await findPRs({ api, repo, token, branch });
  if (prs.length === 0) {
    notice(`update-golden-guard: no open PR has head branch ${branch}; nothing to guard.`);
    return;
  }

  const runs = await listRuns({ api, repo, token });
  const inFlight = runs.filter((r) => runTargetsBranch(r.display_title, branch));
  const state = inFlight.length > 0 ? 'pending' : 'success';
  const description =
    inFlight.length > 0
      ? `An update-golden.yml dispatch against ${branch} is in flight.`
      : `No update-golden.yml dispatch is in flight against ${branch}.`;

  for (const pr of prs) {
    log(`  setting ${STATUS_CONTEXT}=${state} on PR #${pr.number} (${pr.head.sha})`);
    await pushStatus({ api, repo, token, sha: pr.head.sha, state, description, targetUrl });
  }
  notice(`update-golden-guard: ${description} (${prs.length} open PR(s) updated).`);
}

export async function main(env = process.env) {
  const token = required(env, 'GH_TOKEN');
  const repo = required(env, 'REPO');
  const api = env.API_URL || DEFAULT_API_URL;
  const eventName = env.GITHUB_EVENT_NAME || 'pull_request';

  if (eventName === WORKFLOW_RUN_EVENT) {
    await runForWorkflowRunEvent({ env, token, repo, api });
    return;
  }

  let branch;
  try {
    branch = await resolveGuardedBranch({ eventName, env, token, repo, api });
  } catch (e) {
    if (e instanceof UnresolvableMergeGroupError) {
      error(e.message, { title: 'update-golden-guard' });
      process.exitCode = 1;
      return;
    }
    throw e;
  }

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
