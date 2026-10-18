// merge-queue-draft.test.mjs — pins recognising a merge-queue draft and
// resolving it to the batch it checks.

import assert from 'node:assert/strict';
import { test } from 'node:test';

import {
  AHEAD,
  API,
  DRAFT,
  REPO,
  draftBody,
  draftPullRequest,
  fakeApi,
} from './merge-queue-draft.fixture.mjs';
import { batchNumbers, isMergeQueueDraft, resolveMergeQueueBatch } from './merge-queue-draft.mjs';

test('a pull request Mergify opened on a merge-queue branch of this repository is a draft', () => {
  assert.equal(isMergeQueueDraft(draftPullRequest({ numbers: [7] }), REPO), true);
});

test('each recognition condition alone is not enough', () => {
  const draft = draftPullRequest({ numbers: [7] });
  const variants = {
    'another login': { ...draft, user: { login: 'someone', type: 'User' } },
    'a user account with the bot login': { ...draft, user: { login: 'mergify[bot]', type: 'User' } },
    'a head branch in a fork': { ...draft, head: { ...draft.head, repo: { full_name: 'fork/cerberus' } } },
    'another branch name': { ...draft, head: { ...draft.head, ref: 'fix/merge-queue' } },
    'a human branch named like the queue': {
      ...draft,
      user: { login: 'someone', type: 'User' },
      head: { ...draft.head, ref: 'mergify/merge-queue/0a1b2c3d4e' },
    },
  };
  for (const [name, pr] of Object.entries(variants)) {
    assert.equal(isMergeQueueDraft(pr, REPO), false, name);
  }
  assert.equal(isMergeQueueDraft(null, REPO), false, 'no pull request');
});

test('the batch is read from the metadata block, not from the pull requests queued ahead', () => {
  assert.deepEqual(batchNumbers(draftBody([7])), [7]);
  assert.deepEqual(batchNumbers(draftBody([7, 8, 9])), [7, 8, 9]);
  assert.ok(!batchNumbers(draftBody([7])).includes(AHEAD));
});

test('a yaml block without a pull_requests list is skipped for the one that has it', () => {
  const body = '```yaml\nscopes: []\n```\n\n' + draftBody([7]);
  assert.deepEqual(batchNumbers(body), [7]);
});

test('a body with no batch fails instead of yielding an empty batch', () => {
  assert.throws(() => batchNumbers('Mergify opened this draft.'), /no metadata block/);
  assert.throws(() => batchNumbers('```yaml\npull_requests: []\n```'), /no metadata block/);
  assert.throws(() => batchNumbers('```yaml\npull_requests:\nscopes: []\n```'), /lists no pull requests/);
});

test('the batch resolves to each member with its own description and author', async () => {
  const draft = draftPullRequest({ numbers: [7, 8] });
  const { fetchImpl } = fakeApi({
    draft,
    members: {
      7: { title: 'fix: first', body: 'First change.', author: 'a' },
      8: { title: 'feat: second', body: 'Second change.', author: 'b' },
    },
  });
  const { draft: number, batch } = await resolveMergeQueueBatch({ number: DRAFT, repository: REPO, apiBase: API, token: 't', fetchImpl });
  assert.equal(number, DRAFT);
  assert.deepEqual(batch, [
    { number: 7, title: 'fix: first', body: 'First change.', author: 'a' },
    { number: 8, title: 'feat: second', body: 'Second change.', author: 'b' },
  ]);
});

test('the draft is read again from the API and must still be a draft', async () => {
  const draft = draftPullRequest({ numbers: [7], overrides: { user: { login: 'someone', type: 'User' } } });
  const { fetchImpl } = fakeApi({ draft, members: { 7: { body: 'x' } } });
  await assert.rejects(
    resolveMergeQueueBatch({ number: DRAFT, repository: REPO, apiBase: API, token: 't', fetchImpl }),
    /is not a merge-queue draft/,
  );
});

test('a member that targets another base fails the resolution', async () => {
  const draft = draftPullRequest({ numbers: [7] });
  const { fetchImpl } = fakeApi({ draft, members: { 7: { body: 'x', base: 'release/1.21.x' } } });
  await assert.rejects(
    resolveMergeQueueBatch({ number: DRAFT, repository: REPO, apiBase: API, token: 't', fetchImpl }),
    /targets release\/1\.21\.x/,
  );
});

test('a member whose head the draft does not carry fails the resolution', async () => {
  const draft = draftPullRequest({ numbers: [7, 8] });
  const { fetchImpl } = fakeApi({ draft, members: { 7: { body: 'x' }, 8: { body: 'y', compare: 'diverged' } } });
  await assert.rejects(
    resolveMergeQueueBatch({ number: DRAFT, repository: REPO, apiBase: API, token: 't', fetchImpl }),
    /#8's head .* is not in the draft's head/,
  );
});
