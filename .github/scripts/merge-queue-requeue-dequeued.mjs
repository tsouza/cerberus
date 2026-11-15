// merge-queue-requeue-dequeued.mjs — put a pull request that Mergify dequeued
// at an unchanged head back in the merge queue, when the dequeue was not a
// verdict on the pull request.
//
// Mergify keeps a dequeued entry out of the queue until its head changes or
// someone sends the `@mergifyio queue` command. Re-applying the `queue` label
// does not bring it back: Mergify answers with the `dequeued` label again and
// leaves the entry where it is. So both paths that should requeue an entry at
// the same head post the command:
//
//   - `dequeued` labelled (Mergify just dequeued the entry). The entry is
//     requeued only when the dequeue reason is CHECKS_INTERRUPTED: a required
//     check was cancelled before it reported, which says nothing about the
//     pull request. At most MAX_INTERRUPTED_REQUEUES_PER_HEAD times per head
//     commit, so a check that is cancelled on every run cannot cycle the queue.
//     Each attempt is recorded as a commit status (REQUEUE_STATUS_CONTEXT) on
//     the head commit before the command is posted, and the attempts are
//     counted from those statuses, so the bound is per head SHA.
//   - `queue` labelled (someone re-applied the label to a dequeued entry). The
//     entry is requeued when it left for an interruption or because it was
//     taken out of the queue (RELABEL_REQUEUE_REASONS); the bound does not
//     apply to a person's request.
//
// A dequeue for any other reason is left alone on both paths. CHECKS_FAILED
// and every other failure reason are a verdict, and an entry they removed
// waits for a push or for a person to post the command.
//
// The queue status is read from the payload block of Mergify's newest status
// comment on the pull request. Mergify writes that comment before it adds the
// `dequeued` label, but a status that does not read `dequeued` yet is re-read
// up to STATUS_READ_ATTEMPTS times before the run gives up.
//
// Env:
//   GITHUB_REPOSITORY  REQUIRED. `owner/repo`.
//   GITHUB_TOKEN       REQUIRED. Needs `pull-requests: write` and `statuses: write`.
//   PR_NUMBER          REQUIRED. The pull request that was labelled.
//   TRIGGER_LABEL      REQUIRED. The label that was added: DEQUEUED_LABEL or QUEUE_LABEL.
//   GITHUB_API_URL     API base (runner-provided; default https://api.github.com).

import process from 'node:process';
import { setTimeout as sleep } from 'node:timers/promises';
import { pathToFileURL } from 'node:url';

import { error, notice } from './lib/gh.mjs';
import { DEFAULT_API_BASE, NOT_FOUND_THROW, ghJSON, ghPaginate } from './lib/gh-api.mjs';
import { MERGE_QUEUE_AUTHOR } from './lib/merge-queue-draft.mjs';
import { QUEUE_LABEL } from './merge-queue-requeue.mjs';

// DEQUEUED_LABEL is the label Mergify adds when an entry leaves the queue
// (merge_queue.dequeued_label, left at its default in .github/mergify.yml).
export const DEQUEUED_LABEL = 'dequeued';

// REQUEUE_COMMAND puts the pull request back in the queue at its current head.
export const REQUEUE_COMMAND = '@mergifyio queue';

// REQUEUE_AUTHOR is the account GITHUB_TOKEN writes as. .github/mergify.yml's
// commands_restrictions lets it run REQUEUE_COMMAND.
export const REQUEUE_AUTHOR = 'github-actions[bot]';

// REQUEUE_STATUS_CONTEXT is the commit status each automatic requeue writes on
// the head commit; counting them is the per-head attempt count.
export const REQUEUE_STATUS_CONTEXT = 'merge-queue-requeue';

// CHECKS_INTERRUPTED is Mergify's dequeue code for a required check cancelled
// before it reported a result.
export const CHECKS_INTERRUPTED = 'CHECKS_INTERRUPTED';

// MAX_INTERRUPTED_REQUEUES_PER_HEAD bounds the automatic requeues of one head
// commit. A reclaimed runner or a cancelled run is absorbed twice; a check
// interrupted a third time on the same commit is cancelled by something
// systematic, which a person has to look at.
export const MAX_INTERRUPTED_REQUEUES_PER_HEAD = 2;

// RELABEL_REQUEUE_REASONS are the dequeue codes after which a re-applied
// `queue` label requeues the entry: an interruption, the entry leaving because
// its queue conditions stopped matching (the label was removed), and a manual
// dequeue.
export const RELABEL_REQUEUE_REASONS = new Set([CHECKS_INTERRUPTED, 'PR_DEQUEUED', 'PR_MANUALLY_DEQUEUED']);

// STATE_DEQUEUED is the payload state of an entry that left the queue.
export const STATE_DEQUEUED = 'dequeued';

// STATUS_READ_ATTEMPTS and STATUS_READ_INTERVAL_MS bound the wait for Mergify's
// status comment to reach the dequeue the `dequeued` label announced: one
// minute in all.
export const STATUS_READ_ATTEMPTS = 5;
export const STATUS_READ_INTERVAL_MS = 15_000;

export const TRIGGER_DEQUEUED = 'dequeued';
export const TRIGGER_RELABELLED = 'relabelled';

const TRIGGERS = new Map([
  [DEQUEUED_LABEL, TRIGGER_DEQUEUED],
  [QUEUE_LABEL, TRIGGER_RELABELLED],
]);

// triggerFor maps the label that was added to the path it takes.
export function triggerFor(label) {
  const trigger = TRIGGERS.get(label);
  if (!trigger) throw new Error(`TRIGGER_LABEL ${JSON.stringify(label)} is neither ${DEQUEUED_LABEL} nor ${QUEUE_LABEL}`);
  return trigger;
}

const PAYLOAD = /-\*- Mergify Payload -\*-\s*\n([\s\S]*?)\n\s*-\*- Mergify Payload End -\*-/;

// queueStatus returns the payload of Mergify's newest status comment, or null
// when Mergify has posted none. `comments` are in the API's order, oldest first.
export function queueStatus(comments) {
  for (let i = comments.length - 1; i >= 0; i--) {
    const c = comments[i];
    if (c?.user?.login !== MERGE_QUEUE_AUTHOR || c?.user?.type !== 'Bot') continue;
    const m = PAYLOAD.exec(c.body ?? '');
    if (!m) continue;
    try {
      return JSON.parse(m[1]);
    } catch (e) {
      throw new Error(`Mergify status comment ${c.id} carries an unreadable payload: ${e.message}`);
    }
  }
  return null;
}

// requeueAttempts counts the automatic requeues already made for a head
// commit, from that commit's statuses.
export function requeueAttempts(statuses) {
  return statuses.filter((s) => s?.context === REQUEUE_STATUS_CONTEXT && s?.creator?.login === REQUEUE_AUTHOR)
    .length;
}

// decide returns { requeue, counted, why }: whether to post REQUEUE_COMMAND,
// whether the requeue counts against MAX_INTERRUPTED_REQUEUES_PER_HEAD, and
// the reason, for the run's notice.
export function decide({ trigger, open, labelled, status, attempts }) {
  if (!open) return { requeue: false, counted: false, why: 'the pull request is not open' };
  if (!labelled) return { requeue: false, counted: false, why: `the pull request has no ${QUEUE_LABEL} label` };
  if (status?.state !== STATE_DEQUEUED) {
    return { requeue: false, counted: false, why: `the queue status is ${status?.state ?? 'absent'}, not ${STATE_DEQUEUED}` };
  }
  const reason = status.dequeue_reason?.code ?? 'unknown';
  if (trigger === TRIGGER_DEQUEUED) {
    if (reason !== CHECKS_INTERRUPTED) {
      return { requeue: false, counted: false, why: `it left the queue for ${reason}, which is not an interruption` };
    }
    if (attempts >= MAX_INTERRUPTED_REQUEUES_PER_HEAD) {
      return {
        requeue: false,
        counted: false,
        why: `this head was already requeued ${attempts} time(s) after an interruption, the most allowed`,
      };
    }
    return {
      requeue: true,
      counted: true,
      why: `a check was interrupted; requeue ${attempts + 1} of ${MAX_INTERRUPTED_REQUEUES_PER_HEAD} for this head`,
    };
  }
  if (trigger === TRIGGER_RELABELLED) {
    if (!RELABEL_REQUEUE_REASONS.has(reason)) {
      return {
        requeue: false,
        counted: false,
        why: `it left the queue for ${reason}; re-applying the label does not requeue a verdict`,
      };
    }
    return { requeue: true, counted: false, why: `the ${QUEUE_LABEL} label was re-applied after ${reason}` };
  }
  throw new Error(`unknown trigger ${JSON.stringify(trigger)}`);
}

// requeueDequeued reads the pull request, decides, and acts. It returns the
// decision.
export async function requeueDequeued({
  repo,
  number,
  token,
  trigger,
  apiBase = DEFAULT_API_BASE,
  fetchImpl,
  waitImpl = sleep,
}) {
  const api = `${apiBase}/repos/${repo}`;
  const read = (url, what) => ghJSON(url, { token, what, notFound: NOT_FOUND_THROW, fetchImpl });
  const write = (url, what, body) =>
    ghJSON(url, { token, what, notFound: NOT_FOUND_THROW, init: { method: 'POST', body: JSON.stringify(body) }, fetchImpl });

  const pr = await read(`${api}/pulls/${number}`, `read #${number}`);
  const head = pr?.head?.sha;
  if (!head) throw new Error(`#${number} has no head commit in the API response`);
  const open = pr.state === 'open';
  const labelled = (pr.labels ?? []).some((l) => l?.name === QUEUE_LABEL);

  let status = null;
  for (let attempt = 1; attempt <= STATUS_READ_ATTEMPTS; attempt++) {
    const comments = await ghPaginate({
      url: `${api}/issues/${number}/comments`,
      token,
      what: `read the comments of #${number}`,
      fetchImpl,
    });
    status = queueStatus(comments);
    if (!open || !labelled || status?.state === STATE_DEQUEUED || trigger !== TRIGGER_DEQUEUED) break;
    if (attempt < STATUS_READ_ATTEMPTS) await waitImpl(STATUS_READ_INTERVAL_MS);
  }

  const statuses = await ghPaginate({
    url: `${api}/commits/${head}/statuses`,
    token,
    what: `read the statuses of ${head}`,
    fetchImpl,
  });
  const decision = decide({ trigger, open, labelled, status, attempts: requeueAttempts(statuses) });
  if (!decision.requeue) return decision;

  if (decision.counted) {
    await write(`${api}/statuses/${head}`, `record the requeue on ${head}`, {
      state: 'success',
      context: REQUEUE_STATUS_CONTEXT,
      description: decision.why,
    });
  }
  await write(`${api}/issues/${number}/comments`, `post ${REQUEUE_COMMAND} on #${number}`, { body: REQUEUE_COMMAND });
  return decision;
}

function requireEnv(name) {
  const v = process.env[name];
  if (!v) throw new Error(`${name} is unset`);
  return v;
}

async function main() {
  const number = requireEnv('PR_NUMBER');
  const decision = await requeueDequeued({
    repo: requireEnv('GITHUB_REPOSITORY'),
    number,
    token: requireEnv('GITHUB_TOKEN'),
    trigger: triggerFor(requireEnv('TRIGGER_LABEL')),
    apiBase: process.env.GITHUB_API_URL || DEFAULT_API_BASE,
  });
  notice(
    decision.requeue
      ? `merge-queue-requeue-dequeued: posted \`${REQUEUE_COMMAND}\` on #${number}: ${decision.why}.`
      : `merge-queue-requeue-dequeued: left #${number} alone: ${decision.why}.`,
  );
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch((e) => {
    error(`merge-queue-requeue-dequeued: ${e.message}`);
    process.exit(1);
  });
}
