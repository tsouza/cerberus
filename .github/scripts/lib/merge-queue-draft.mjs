// merge-queue-draft.mjs — resolve a Mergify merge-queue draft pull request to
// the pull requests it checks.
//
// When the merge queue checks a batch, Mergify opens a draft pull request of
// its own on a `mergify/merge-queue/<id>` branch holding `main` plus the
// batch, and every pull_request workflow runs on that draft. A gate that judges
// a pull request by its description would otherwise judge the text Mergify
// wrote for the draft, which is long and names no work, so it passes without
// having looked at any change. The gates call resolveMergeQueueBatch and judge
// the batch's own pull requests instead.
//
// Recognition needs all four of: the `mergify[bot]` login, a `Bot` account
// type, a head branch in this repository, and the merge-queue branch prefix. A
// branch name alone is not enough: anyone who can push a branch can name it.
//
// The batch is read from the metadata block Mergify writes into the draft for
// its own CI tooling (a fenced `yaml` block with a `pull_requests:` list).
// Pull requests queued AHEAD of the batch are not in that list: each was
// judged on its own draft. Every listed pull request must target the draft's
// base branch, and its head commit must be contained in the draft's head, so
// the gates judge only pull requests whose code the draft actually carries.
//
// The event payload is enough to recognise a draft, but the draft is read
// again from the API before its batch is trusted, and must still be one.
// Any failure to read the batch throws: a draft whose batch cannot be
// resolved fails the gate rather than falling back to the draft's own text.
//
// Exports:
//   MERGE_QUEUE_AUTHOR, MERGE_QUEUE_BRANCH_PREFIX
//   isMergeQueueDraft(pr, repository)
//   batchNumbers(body)
//   resolveMergeQueueBatch({ number, repository, apiBase, token, fetchImpl })
//     → { draft, batch: [{ number, title, body, author }] }

import { ghJSON, NOT_FOUND_THROW } from './gh-api.mjs';

export const MERGE_QUEUE_AUTHOR = 'mergify[bot]';
export const MERGE_QUEUE_BRANCH_PREFIX = 'mergify/merge-queue/';

export function isMergeQueueDraft(pr, repository) {
  return (
    pr?.user?.login === MERGE_QUEUE_AUTHOR &&
    pr?.user?.type === 'Bot' &&
    typeof repository === 'string' &&
    pr?.head?.repo?.full_name === repository &&
    typeof pr?.head?.ref === 'string' &&
    pr.head.ref.startsWith(MERGE_QUEUE_BRANCH_PREFIX)
  );
}

const METADATA_BLOCK = /^```yaml[ \t]*\n([\s\S]*?)^```/gm;
const BATCH_KEY = /^pull_requests:\s*$/;
const BATCH_ENTRY = /^\s+-\s+number:\s*(\d+)\s*$/;
const TOP_LEVEL_KEY = /^\S/;

// batchNumbers — the pull request numbers in the metadata block's
// `pull_requests:` list, in order. Throws when no block carries one, or when
// the list is empty.
export function batchNumbers(body) {
  for (const [, block] of String(body ?? '').matchAll(METADATA_BLOCK)) {
    const lines = block.split('\n');
    const start = lines.findIndex((l) => BATCH_KEY.test(l));
    if (start === -1) continue;
    const numbers = [];
    for (const line of lines.slice(start + 1)) {
      if (TOP_LEVEL_KEY.test(line)) break;
      const entry = BATCH_ENTRY.exec(line);
      if (entry) numbers.push(Number(entry[1]));
    }
    if (numbers.length === 0) {
      throw new Error('the merge-queue draft metadata lists no pull requests under pull_requests:');
    }
    return numbers;
  }
  throw new Error('the merge-queue draft carries no metadata block with a pull_requests: list');
}

// A head commit is in the draft when comparing it to the draft's head shows
// the draft at or ahead of it.
const CONTAINED = new Set(['ahead', 'identical']);

export async function resolveMergeQueueBatch({ number: draftNumber, repository, apiBase, token, fetchImpl }) {
  const draft = await ghJSON(`${apiBase}/repos/${repository}/pulls/${draftNumber}`, {
    token,
    what: `read merge-queue draft #${draftNumber}`,
    notFound: NOT_FOUND_THROW,
    fetchImpl,
  });
  if (!isMergeQueueDraft(draft, repository)) {
    throw new Error(`#${draftNumber} is not a merge-queue draft of ${repository}`);
  }
  const numbers = batchNumbers(draft.body);
  const batch = [];
  for (const number of numbers) {
    const pr = await ghJSON(`${apiBase}/repos/${repository}/pulls/${number}`, {
      token,
      what: `read merge-queue batch member #${number}`,
      notFound: NOT_FOUND_THROW,
      fetchImpl,
    });
    if (pr?.base?.ref !== draft.base.ref) {
      throw new Error(
        `merge-queue batch member #${number} targets ${pr?.base?.ref}, not the draft's base ${draft.base.ref}`,
      );
    }
    const comparison = await ghJSON(
      `${apiBase}/repos/${repository}/compare/${pr.head.sha}...${draft.head.sha}`,
      { token, what: `compare #${number}'s head with the draft's head`, notFound: NOT_FOUND_THROW, fetchImpl },
    );
    if (!CONTAINED.has(comparison?.status)) {
      throw new Error(
        `merge-queue batch member #${number}'s head ${pr.head.sha.slice(0, 12)} is not in the draft's head ` +
          `${draft.head.sha.slice(0, 12)} (compare status: ${comparison?.status})`,
      );
    }
    batch.push({
      number,
      title: typeof pr.title === 'string' ? pr.title : '',
      body: typeof pr.body === 'string' ? pr.body : '',
      author: typeof pr?.user?.login === 'string' && pr.user.login.trim() !== '' ? pr.user.login.trim() : null,
    });
  }
  return { draft: draftNumber, batch };
}
