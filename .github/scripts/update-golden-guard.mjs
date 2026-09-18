// update-golden-guard.mjs — the PR check that closes issue #2350 (Info-only
// today, not yet in `main`'s required_status_checks ruleset — see
// docs/test-strategy.md's CI-gate inventory).
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
// resulting push, not this one. It runs in three triggers:
//
//   - `pull_request` (opened/synchronize/reopened/ready_for_review): takes
//     ONE snapshot of the update-golden.yml runs currently requested,
//     in_progress or queued against the PR's own branch. Clear → the check
//     passes. A match → the check FAILS immediately rather than waiting
//     (see "why this fails fast instead of blocking" below); it relies on
//     the `workflow_run` trigger to flip the SAME check back to green once
//     that dispatch finishes, with no new push needed.
//   - `workflow_run` (requested/completed, for the `update-golden` workflow
//     itself): closes the gap the `pull_request` trigger alone leaves open,
//     in both directions. A dispatch against an ALREADY-open PR's branch,
//     made after that PR's last push, never fires a new `pull_request`
//     event — GitHub does not re-poll an already-green required check on
//     its own, and a `pull_request` run that already failed fast has no
//     later event of its own to re-evaluate on. This trigger reacts to the
//     dispatch directly instead: `requested` sets the guard context to
//     `pending` on every open PR whose head branch the dispatch targets
//     (found via the Pulls API, since a `workflow_run` job runs in the
//     default branch's context, not the PR's — the check has to be pushed
//     onto the PR's head SHA explicitly via the Statuses API), and
//     `completed` re-checks and flips it to `success` once no run remains
//     in flight against that branch (handling a second, serialised dispatch
//     via the same in-flight query the snapshot path uses). Because this
//     status is pushed under the exact same `STATUS_CONTEXT` the
//     `pull_request`-triggered check-run uses, branch protection's combined
//     status for that SHA takes whichever of the two reported LAST — the
//     same mechanism the "dispatch starts after check went green" case
//     already depended on, now doing double duty for "check failed fast,
//     dispatch later completes" too.
//   - `merge_group`: the merge queue's own copy of the snapshot, with one
//     asymmetry from the `pull_request` path. See the section below.
//
// # Why the merge queue needs its own check rather than a free pass
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
// did. So the merge-group run takes the SAME single snapshot the
// `pull_request` path does, against the branch the queued pull request would
// delete.
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
// Unlike the `pull_request` path, a fast-failed `merge_group` snapshot does
// NOT self-heal in place: `workflow_run`'s `completed` handler pushes its
// status onto the pull request's own head SHA (see above), never onto the
// queue's own ephemeral `gh-readonly-queue/…` commit — GitHub tears that
// branch down once the group resolves, and by `completed` time there is no
// stable API handle from a branch name back to "the projected commit some
// now-possibly-gone queue attempt built for it." So a `merge_group` snapshot
// that finds an in-flight dispatch reports a REAL failure (not the spurious,
// cancellation-triggered kind `cancel-in-progress: false` below already
// guards against), and GitHub dequeues that entry the same way it would for
// any other genuinely failing required check. The pull request itself is not
// left red, though: `workflow_run`'s `completed` handler still flips the
// SAME context back to `success` on the PR's own head SHA the moment the
// dispatch clears, exactly as in the `pull_request` case — what does not
// happen automatically is the PR re-entering the merge queue, which needs a
// fresh "add to merge queue" the same as any other dequeue. This is judged an
// acceptable trade rather than a gap needing its own mechanism: the
// concurrency block below already tolerates a real-failure dequeue as
// non-spurious, the overlap it requires (a `merge_group` snapshot landing
// while a dispatch against that exact branch is in flight) is far rarer than
// the `pull_request` path's own trigger frequency (every push, and every
// update-golden.yml dispatch made during active iteration on an open PR —
// the actual source of the sustained-poll cost this snapshot replaces), and
// building a second status-push target for an ephemeral queue commit would
// be new machinery for a narrow, self-recovering window, not a closure of
// #2350's own race. (As of this writing this check is Info-only — not in
// `main`'s required_status_checks ruleset — so a merge_group failure has no
// effect on the queue in practice today; the reasoning above targets the
// check's intended eventual role once it is required. See the CI-gate
// inventory in docs/test-strategy.md for the current status.)
//
// The residual window is the irreducible one the `pull_request` path already
// has: a dispatch created in the seconds between this snapshot's read and the
// queue's merge. Nothing in-band can close that — the merge is GitHub's to
// make and there is no transactional handle on it — and it is the same
// exposure every branch-protection check carries.
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
// # Why the pull_request (and merge_group) path fails fast instead of blocking
//
// This check used to poll from inside the one job run, sleeping in a loop
// for up to an hour so the SAME check run could clear itself the moment the
// hazard was gone, with no second event needed. That held a runner busy
// doing nothing but re-polling for the whole wait — and in this repo, an
// update-golden.yml dispatch against an open PR's own branch is a routine
// part of active iteration (a regen after every fix during a multi-round PR),
// so the busy-poll recurred often and each occurrence could burn up to the
// full hour. A required check only ever gates the PR's CURRENT head SHA, so
// the one thing a non-blocking check genuinely needs is some OTHER mechanism
// to re-evaluate that same SHA once the hazard clears — and one already
// existed: the `workflow_run` trigger's `completed` handler, which was
// already relied on to flip this same check from green to pending on a
// dispatch that starts AFTER the PR's last push (see above). That handler
// does not care which event last reported to `STATUS_CONTEXT`, only what the
// CURRENT in-flight state is — so it is equally able to flip a check that
// failed fast back to `success` once the dispatch it flagged finishes,
// closing the loop this file used to close by blocking. The `pull_request`
// (and `merge_group`) step therefore takes exactly ONE snapshot of the
// in-flight list and reports on it immediately: clear now → pass now; not
// clear now → fail now, and rely on `workflow_run`'s `completed` event (which
// GitHub raises independently, without this job's help) to reflect the branch
// clearing later. The `workflow_run` path itself needs no such change: it was
// already snapshot-based, since GitHub re-invokes this script at `requested`
// and again at `completed`.
//
// Env contract (GITHUB_EVENT_NAME selects the branch — set by the runner):
//   pull_request:
//     GH_TOKEN        (required) a token with `actions: read` on this repo.
//     REPO             (required) `owner/repo`.
//     BRANCH           (required) the PR's head branch (github.head_ref).
//     API_URL          (optional) GitHub REST API base. Default public API.
//   merge_group:
//     GH_TOKEN              (required) a token with `actions: read` and
//                            `pull-requests: read` on this repo.
//     REPO                  (required) `owner/repo`.
//     MERGE_GROUP_HEAD_REF  (required) github.event.merge_group.head_ref.
//     MERGE_GROUP_BASE_REF  (required) github.event.merge_group.base_ref.
//     API_URL as for pull_request.
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
//   0  no update-golden.yml run is queued, requested or in_progress against
//      the guarded branch (pull_request, merge_group) at the moment of this
//      check's one snapshot, or the workflow_run event was handled (whatever
//      state it resulted in — the Statuses API call failing is the only
//      workflow_run failure mode).
//   1  one was in flight at snapshot time, the merge group's head ref could
//      not be resolved to a pull request, or the API calls themselves
//      failed. On `pull_request`, this check will flip back to success on
//      its own once `workflow_run`'s `completed` handler sees the dispatch
//      finish — no new push needed (see the file header). On `merge_group`
//      it dequeues the entry; the pull request itself still flips back to a
//      green required check the same way, but re-entering the merge queue
//      is not automatic (see "Why the merge queue needs its own check").

import process from 'node:process';

import { error, log, notice } from './lib/gh.mjs';
import { GITHUB_PER_PAGE, NOT_FOUND_THROW, ghJSON as ghRequest, ghPaginate } from './lib/gh-api.mjs';

const DEFAULT_API_URL = 'https://api.github.com';
const WORKFLOW_FILE = 'update-golden.yml';

// The Actions API run states that mean "still touching the branch". A
// `completed` run — success, failure, or cancelled — is no longer a hazard,
// whatever its conclusion: see the file header on why this check does not
// read conclusion at all. `requested` is included alongside `in_progress`
// and `queued`: it is the transient status a workflow_dispatch run briefly
// reports between being created and being picked up by a runner, and a
// snapshot (whether from the pull_request/merge_group path or a workflow_run
// one) landing in that window must still see the run as in flight rather
// than reporting a false-clear.
const IN_FLIGHT_STATUSES = ['requested', 'in_progress', 'queued'];

// The context name this script publishes to when it sets a commit status
// directly (the workflow_run path — see the file header). Kept identical to
// the job name update-golden-guard.yml uses for its pull_request-triggered
// check-run so branch protection's single required-check entry is satisfied
// by either source.
const STATUS_CONTEXT = 'update-golden-guard';

// The two non-default `GITHUB_EVENT_NAME` values main() branches on. The
// default — anything else — is the pull_request snapshot path.
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

// Every resource this guard reads must exist — a PR by number, this
// workflow's runs, a commit to post a status on — so a 404 is a failure,
// never an empty answer.
async function ghJSON(url, token, init = {}) {
  return ghRequest(url, { token, init, notFound: NOT_FOUND_THROW });
}

// The paged form of the same policy, for the two list endpoints below.
async function ghPages(url, token, pick) {
  return ghPaginate({ url, token, pick });
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
export async function listInFlightRuns({ api, repo, token, fetchPages = ghPages }) {
  const runs = [];
  for (const status of IN_FLIGHT_STATUSES) {
    const url = `${api}/repos/${repo}/actions/workflows/${WORKFLOW_FILE}/runs?status=${status}`;
    const page = await fetchPages(url, token, (body) => {
      if (!Array.isArray(body?.workflow_runs)) {
        throw new Error(`unexpected response listing ${status} runs of ${WORKFLOW_FILE}: ${JSON.stringify(body)}`);
      }
      return body.workflow_runs;
    });
    runs.push(...page);
  }
  return runs;
}

/**
 * A single snapshot: is any update-golden.yml run currently requested,
 * in_progress or queued against `branch`? Does not wait or retry — see the
 * file header ("why this fails fast instead of blocking") for why one
 * snapshot per check run is now enough.
 *
 * `listRuns` is injected so tests drive it from a scripted response instead
 * of the network.
 */
export async function checkBranchClear({ listRuns, branch }) {
  const runs = await listRuns();
  const matching = runs.filter((r) => runTargetsBranch(r.display_title, branch));
  return matching.length === 0 ? { clear: true, runs: [] } : { clear: false, runs: matching };
}

/**
 * Every open PR (in this repo) whose head branch is exactly `branch`. Used
 * only from the workflow_run path: that job runs in the default branch's
 * context, with no PR of its own, so it has to look the PR up by branch name
 * to know which head SHA to push a commit status onto.
 */
export async function findOpenPRsForBranch({ api, repo, token, branch, fetchPages = ghPages }) {
  const owner = repo.split('/')[0];
  const url = `${api}/repos/${repo}/pulls?state=open&head=${encodeURIComponent(`${owner}:${branch}`)}`;
  return fetchPages(url, token, (prs) => {
    if (!Array.isArray(prs)) {
      throw new Error(`unexpected response listing open PRs for branch ${branch}: ${JSON.stringify(prs)}`);
    }
    return prs;
  });
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

  const result = await checkBranchClear({
    listRuns: () => listInFlightRuns({ api, repo, token }),
    branch,
  });

  if (result.clear) {
    notice(`update-golden-guard: no update-golden.yml dispatch is in flight against ${branch}.`);
    return;
  }

  for (const r of result.runs) log(`  in flight: ${r.html_url} (${r.status})`);
  const urls = result.runs.map((r) => r.html_url).join(', ');
  const recovery =
    eventName === MERGE_GROUP_EVENT
      ? 'This dequeues the entry; the pull request itself will flip back to a green ' +
        `${STATUS_CONTEXT} check automatically once the dispatch finishes (the workflow_run trigger ` +
        're-checks and reports on its head SHA), but it will need to be re-added to the merge queue.'
      : 'This check will flip back to success on its own, with no new push needed, once the ' +
        `workflow_run trigger sees that dispatch finish (see the file header's "why this fails fast ` +
        'instead of blocking").';
  error(
    `update-golden-guard: an update-golden.yml dispatch against ${branch} is in flight: ${urls}. ` +
      recovery,
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

if (import.meta.url === `file://${process.argv[1]}`) {
  main().catch((e) => {
    error(e?.stack ?? String(e), { title: 'update-golden-guard' });
    process.exitCode = 1;
  });
}
