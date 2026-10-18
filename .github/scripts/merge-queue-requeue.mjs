// merge-queue-requeue.mjs — put a pull request back in the Mergify merge
// queue after a push.
//
// A push to a pull request in the queue (.github/mergify.yml) removes it from
// the queue, and Mergify does not re-add it: the `queue` label stays on, the
// queue check reads "Dequeued — pull request manually updated", and the pull
// request waits there until someone re-applies the label. Removing the label
// and adding it again is the same re-application, done here on every push to a
// labelled pull request. Mergify then queues it once the new head meets the
// queue conditions, as it does for a newly labelled pull request.
//
// A pull request without the label is left alone. The labels are read from the
// live pull request rather than the event payload, so a label removed after
// the push is not put back.
//
// Env:
//   GITHUB_REPOSITORY  REQUIRED. `owner/repo`.
//   GITHUB_TOKEN       REQUIRED. Needs `pull-requests: write`.
//   PR_NUMBER          REQUIRED. The pull request that was pushed to.
//   GITHUB_API_URL     API base (runner-provided; default https://api.github.com).

import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { error, notice } from './lib/gh.mjs';
import { DEFAULT_API_BASE, NOT_FOUND_THROW, ghJSON } from './lib/gh-api.mjs';

export const QUEUE_LABEL = 'queue';

// requeue re-applies the queue label when the pull request carries it, and
// returns what it did: 'requeued' or 'unlabelled'.
export async function requeue({ repo, number, token, apiBase = DEFAULT_API_BASE, fetchImpl }) {
  const issue = `${apiBase}/repos/${repo}/issues/${number}`;
  const labels = await ghJSON(`${issue}/labels`, {
    token,
    what: `read the labels of #${number}`,
    notFound: NOT_FOUND_THROW,
    fetchImpl,
  });
  if (!labels.some((l) => l?.name === QUEUE_LABEL)) return 'unlabelled';
  await ghJSON(`${issue}/labels/${QUEUE_LABEL}`, {
    token,
    what: `remove the ${QUEUE_LABEL} label from #${number}`,
    notFound: NOT_FOUND_THROW,
    init: { method: 'DELETE' },
    fetchImpl,
  });
  await ghJSON(`${issue}/labels`, {
    token,
    what: `add the ${QUEUE_LABEL} label to #${number}`,
    notFound: NOT_FOUND_THROW,
    init: { method: 'POST', body: JSON.stringify({ labels: [QUEUE_LABEL] }) },
    fetchImpl,
  });
  return 'requeued';
}

function requireEnv(name) {
  const v = process.env[name];
  if (!v) throw new Error(`${name} is unset`);
  return v;
}

async function main() {
  const number = requireEnv('PR_NUMBER');
  const outcome = await requeue({
    repo: requireEnv('GITHUB_REPOSITORY'),
    number,
    token: requireEnv('GITHUB_TOKEN'),
    apiBase: process.env.GITHUB_API_URL || DEFAULT_API_BASE,
  });
  notice(
    outcome === 'requeued'
      ? `merge-queue-requeue: re-applied the ${QUEUE_LABEL} label to #${number}; Mergify queues it once the new head meets the queue conditions.`
      : `merge-queue-requeue: #${number} has no ${QUEUE_LABEL} label; nothing to do.`,
  );
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch((e) => {
    error(`merge-queue-requeue: ${e.message}`);
    process.exit(1);
  });
}
