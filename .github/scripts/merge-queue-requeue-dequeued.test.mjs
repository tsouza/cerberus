// merge-queue-requeue-dequeued.test.mjs — pins which dequeued entries are put
// back in the merge queue, and how often.

import assert from 'node:assert/strict';
import { test } from 'node:test';

import {
  CHECKS_INTERRUPTED,
  MAX_INTERRUPTED_REQUEUES_PER_HEAD,
  REQUEUE_AUTHOR,
  REQUEUE_COMMAND,
  REQUEUE_STATUS_CONTEXT,
  STATUS_READ_ATTEMPTS,
  TRIGGER_DEQUEUED,
  TRIGGER_RELABELLED,
  decide,
  queueStatus,
  requeueAttempts,
  requeueDequeued,
  triggerFor,
} from './merge-queue-requeue-dequeued.mjs';
import { QUEUE_LABEL } from './merge-queue-requeue.mjs';

const dequeued = (code) => ({ state: 'dequeued', dequeue_reason: code ? { code } : undefined });
const labelledOpen = { open: true, labelled: true };

// Every dequeue reason Mergify reports that is a verdict on the pull request
// or its batch, from the configuration schema's dequeue-reason enum.
const VERDICTS = ['CHECKS_FAILED', 'CHECKS_TIMEOUT', 'BATCH_MAX_FAILURE_RESOLUTION_ATTEMPTS', 'CONFLICT_WITH_BASE_BRANCH'];

test('an entry dequeued for an interrupted check is requeued until the per-head bound', () => {
  for (let attempts = 0; attempts < MAX_INTERRUPTED_REQUEUES_PER_HEAD; attempts++) {
    const d = decide({ trigger: TRIGGER_DEQUEUED, ...labelledOpen, status: dequeued(CHECKS_INTERRUPTED), attempts });
    assert.equal(d.requeue, true, `attempt ${attempts + 1} must requeue: ${d.why}`);
    assert.equal(d.counted, true);
  }
  const spent = decide({
    trigger: TRIGGER_DEQUEUED,
    ...labelledOpen,
    status: dequeued(CHECKS_INTERRUPTED),
    attempts: MAX_INTERRUPTED_REQUEUES_PER_HEAD,
  });
  assert.equal(spent.requeue, false, 'the bound is reached; a person has to look');
});

test('an entry dequeued for a failed check is never requeued, whatever the attempt count', () => {
  for (const code of [...VERDICTS, 'PR_DEQUEUED', undefined]) {
    for (const attempts of [0, 1, MAX_INTERRUPTED_REQUEUES_PER_HEAD]) {
      const d = decide({ trigger: TRIGGER_DEQUEUED, ...labelledOpen, status: dequeued(code), attempts });
      assert.equal(d.requeue, false, `${code} at ${attempts} attempts must stay out: ${d.why}`);
    }
  }
});

test('a re-applied queue label requeues an entry taken out of the queue or interrupted', () => {
  for (const code of ['PR_DEQUEUED', 'PR_MANUALLY_DEQUEUED', CHECKS_INTERRUPTED]) {
    const d = decide({
      trigger: TRIGGER_RELABELLED,
      ...labelledOpen,
      status: dequeued(code),
      attempts: MAX_INTERRUPTED_REQUEUES_PER_HEAD,
    });
    assert.equal(d.requeue, true, `${code}: ${d.why}`);
    assert.equal(d.counted, false, 'a person asked; the automatic bound is not spent');
  }
});

test('a re-applied queue label does not requeue a failed check', () => {
  for (const code of VERDICTS) {
    const d = decide({ trigger: TRIGGER_RELABELLED, ...labelledOpen, status: dequeued(code), attempts: 0 });
    assert.equal(d.requeue, false, `${code}: ${d.why}`);
  }
});

test('an entry that is not dequeued, unlabelled, or closed is left alone', () => {
  const interrupted = dequeued(CHECKS_INTERRUPTED);
  for (const trigger of [TRIGGER_DEQUEUED, TRIGGER_RELABELLED]) {
    for (const [what, input] of [
      ['queued', { ...labelledOpen, status: { state: 'checking' } }],
      ['never queued', { ...labelledOpen, status: null }],
      ['unlabelled', { open: true, labelled: false, status: interrupted }],
      ['closed', { open: false, labelled: true, status: interrupted }],
    ]) {
      assert.equal(decide({ trigger, ...input, attempts: 0 }).requeue, false, `${trigger}, ${what}`);
    }
  }
});

test('the trigger comes from the label that was added', () => {
  assert.equal(triggerFor('dequeued'), TRIGGER_DEQUEUED);
  assert.equal(triggerFor(QUEUE_LABEL), TRIGGER_RELABELLED);
  assert.throws(() => triggerFor('queued'), /neither/);
});

const payloadComment = (id, payload, user = { login: 'mergify[bot]', type: 'Bot' }) => ({
  id,
  user,
  body: `<!---\nDO NOT EDIT\n-*- Mergify Payload -*-\n${JSON.stringify(payload)}\n-*- Mergify Payload End -*-\n-->\n\n### Left the queue`,
});

test("the queue status is the payload of Mergify's newest status comment", () => {
  const comments = [
    payloadComment(1, dequeued(CHECKS_INTERRUPTED)),
    { id: 2, user: { login: 'someone', type: 'User' }, body: '@mergifyio queue' },
    payloadComment(3, { state: 'checking' }),
    { id: 4, user: { login: 'mergify[bot]', type: 'Bot' }, body: 'no payload here' },
  ];
  assert.deepEqual(queueStatus(comments), { state: 'checking' });
  assert.equal(queueStatus([]), null);
});

test('a payload written by an account that is not the Mergify app is ignored', () => {
  const comments = [
    payloadComment(1, dequeued('CHECKS_FAILED')),
    payloadComment(2, dequeued(CHECKS_INTERRUPTED), { login: 'mergify[bot]', type: 'User' }),
    payloadComment(3, dequeued(CHECKS_INTERRUPTED), { login: 'mallory', type: 'User' }),
  ];
  assert.equal(queueStatus(comments).dequeue_reason.code, 'CHECKS_FAILED');
});

test('an unreadable payload fails the run', () => {
  const broken = { id: 9, user: { login: 'mergify[bot]', type: 'Bot' }, body: '-*- Mergify Payload -*-\n{oops\n-*- Mergify Payload End -*-' };
  assert.throws(() => queueStatus([broken]), /comment 9/);
});

test('attempts count only the requeue statuses this workflow wrote', () => {
  const statuses = [
    { context: REQUEUE_STATUS_CONTEXT, creator: { login: REQUEUE_AUTHOR } },
    { context: REQUEUE_STATUS_CONTEXT, creator: { login: 'mallory' } },
    { context: 'pr-body', creator: { login: REQUEUE_AUTHOR } },
    { context: REQUEUE_STATUS_CONTEXT, creator: { login: REQUEUE_AUTHOR } },
  ];
  assert.equal(requeueAttempts(statuses), 2);
});

// The end-to-end path, against a fake API.

const API = 'https://api.github.test';
const REPO = 'tsouza/cerberus';
const HEAD = 'abc123';

function fakeApi({ labels = [QUEUE_LABEL], state = 'open', statusSequence, priorAttempts = 0 }) {
  const calls = [];
  let commentReads = 0;
  const fetchImpl = async (url, init = {}) => {
    const method = init.method ?? 'GET';
    calls.push(`${method} ${url.replace(API, '').replace(/[?&]per_page=.*$/, '')}${init.body ? ` ${init.body}` : ''}`);
    let json = {};
    if (method === 'GET' && url === `${API}/repos/${REPO}/pulls/7`) {
      json = { state, head: { sha: HEAD }, labels: labels.map((name) => ({ name })) };
    } else if (method === 'GET' && url.startsWith(`${API}/repos/${REPO}/issues/7/comments`)) {
      const s = statusSequence[Math.min(commentReads, statusSequence.length - 1)];
      commentReads++;
      json = s ? [payloadComment(1, s)] : [];
    } else if (method === 'GET' && url.startsWith(`${API}/repos/${REPO}/commits/${HEAD}/statuses`)) {
      json = Array.from({ length: priorAttempts }, () => ({
        context: REQUEUE_STATUS_CONTEXT,
        creator: { login: REQUEUE_AUTHOR },
      }));
    }
    return { ok: true, status: 200, statusText: 'OK', json: async () => json };
  };
  return { fetchImpl, calls };
}

const run = (api, trigger) =>
  requeueDequeued({
    repo: REPO,
    number: 7,
    token: 't',
    trigger,
    apiBase: API,
    fetchImpl: api.fetchImpl,
    waitImpl: async () => {},
  });

const writes = (api) => api.calls.filter((c) => c.startsWith('POST'));

test('an interrupted entry gets a requeue status on its head, then the queue command', async () => {
  const api = fakeApi({ statusSequence: [dequeued(CHECKS_INTERRUPTED)], priorAttempts: 1 });
  assert.equal((await run(api, TRIGGER_DEQUEUED)).requeue, true);
  const w = writes(api);
  assert.equal(w.length, 2);
  assert.match(w[0], new RegExp(`^POST /repos/${REPO}/statuses/${HEAD} .*"context":"${REQUEUE_STATUS_CONTEXT}"`));
  assert.equal(w[1], `POST /repos/${REPO}/issues/7/comments ${JSON.stringify({ body: REQUEUE_COMMAND })}`);
});

test('an interrupted entry whose head spent its bound is not requeued', async () => {
  const api = fakeApi({
    statusSequence: [dequeued(CHECKS_INTERRUPTED)],
    priorAttempts: MAX_INTERRUPTED_REQUEUES_PER_HEAD,
  });
  assert.equal((await run(api, TRIGGER_DEQUEUED)).requeue, false);
  assert.deepEqual(writes(api), []);
});

test('a failed entry produces no write at all', async () => {
  const api = fakeApi({ statusSequence: [dequeued('CHECKS_FAILED')] });
  assert.equal((await run(api, TRIGGER_DEQUEUED)).requeue, false);
  assert.deepEqual(writes(api), []);
});

test('a status that has not caught up with the dequeued label is read again', async () => {
  const api = fakeApi({ statusSequence: [{ state: 'checking' }, { state: 'checking' }, dequeued(CHECKS_INTERRUPTED)] });
  assert.equal((await run(api, TRIGGER_DEQUEUED)).requeue, true);
  assert.equal(api.calls.filter((c) => c.includes('/issues/7/comments') && c.startsWith('GET')).length, 3);
});

test('a status that never catches up is read a bounded number of times and left alone', async () => {
  const api = fakeApi({ statusSequence: [{ state: 'checking' }] });
  assert.equal((await run(api, TRIGGER_DEQUEUED)).requeue, false);
  assert.equal(api.calls.filter((c) => c.includes('/issues/7/comments') && c.startsWith('GET')).length, STATUS_READ_ATTEMPTS);
  assert.deepEqual(writes(api), []);
});

test('a re-applied label on an entry taken out of the queue posts the command without a status', async () => {
  const api = fakeApi({ statusSequence: [dequeued('PR_DEQUEUED')] });
  assert.equal((await run(api, TRIGGER_RELABELLED)).requeue, true);
  assert.deepEqual(writes(api), [`POST /repos/${REPO}/issues/7/comments ${JSON.stringify({ body: REQUEUE_COMMAND })}`]);
});

test('a failed write fails the run', async () => {
  const fetchImpl = async (url, init = {}) => {
    if ((init.method ?? 'GET') === 'POST') return { ok: false, status: 403, statusText: 'Forbidden', json: async () => ({}) };
    return fakeApi({ statusSequence: [dequeued(CHECKS_INTERRUPTED)] }).fetchImpl(url, init);
  };
  await assert.rejects(run({ fetchImpl }, TRIGGER_DEQUEUED), /HTTP 403/);
});
