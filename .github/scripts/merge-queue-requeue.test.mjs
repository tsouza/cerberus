// merge-queue-requeue.test.mjs — pins re-applying the queue label after a
// push.

import assert from 'node:assert/strict';
import { test } from 'node:test';

import { QUEUE_LABEL, requeue } from './merge-queue-requeue.mjs';

const API = 'https://api.github.test';
const REPO = 'tsouza/cerberus';
const ISSUE = `${API}/repos/${REPO}/issues/7`;

// fakeApi answers the label listing with `labels` and records every call as
// `METHOD url body`.
function fakeApi(labels) {
  const calls = [];
  const fetchImpl = async (url, init = {}) => {
    calls.push(`${init.method ?? 'GET'} ${url}${init.body ? ` ${init.body}` : ''}`);
    const json = url === `${ISSUE}/labels` && (init.method ?? 'GET') === 'GET' ? labels.map((name) => ({ name })) : [];
    return { ok: true, status: 200, statusText: 'OK', json: async () => json };
  };
  return { fetchImpl, calls };
}

const run = (api) => requeue({ repo: REPO, number: 7, token: 't', apiBase: API, fetchImpl: api.fetchImpl });

test('a labelled pull request has its queue label removed and added again, in that order', async () => {
  const api = fakeApi(['bug', QUEUE_LABEL]);
  assert.equal(await run(api), 'requeued');
  assert.deepEqual(api.calls, [
    `GET ${ISSUE}/labels`,
    `DELETE ${ISSUE}/labels/${QUEUE_LABEL}`,
    `POST ${ISSUE}/labels {"labels":["${QUEUE_LABEL}"]}`,
  ]);
});

test('a pull request without the queue label is left alone', async () => {
  const api = fakeApi(['bug', 'queued-elsewhere']);
  assert.equal(await run(api), 'unlabelled');
  assert.deepEqual(api.calls, [`GET ${ISSUE}/labels`]);
});

test('a failed label write fails the run instead of reporting a requeue', async () => {
  const fetchImpl = async (url, init = {}) =>
    (init.method ?? 'GET') === 'GET'
      ? { ok: true, status: 200, statusText: 'OK', json: async () => [{ name: QUEUE_LABEL }] }
      : { ok: false, status: 403, statusText: 'Forbidden', json: async () => ({}) };
  await assert.rejects(run({ fetchImpl }), /HTTP 403/);
});
