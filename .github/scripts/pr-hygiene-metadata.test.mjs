import assert from 'node:assert/strict';
import { test } from 'node:test';

import { fetchPullRequest } from './lib/pull-request.mjs';

test('metadata gates read the live pull request object', async () => {
  let requested;
  const metadata = await fetchPullRequest({
    apiUrl: 'https://api.github.test',
    repository: 'tsouza/cerberus',
    number: 3596,
    token: 'token',
    fetchImpl: async (url, init) => {
      requested = { url, init };
      return { ok: true, status: 200, json: async () => ({ title: 'live title', body: 'live body' }) };
    },
  });

  assert.deepEqual(metadata, { title: 'live title', body: 'live body' });
  assert.equal(requested.url, 'https://api.github.test/repos/tsouza/cerberus/pulls/3596');
  assert.equal(requested.init.headers.authorization, 'Bearer token');
});

test('metadata failures fail closed', async () => {
  await assert.rejects(
    fetchPullRequest({
      apiUrl: 'https://api.github.test',
      repository: 'tsouza/cerberus',
      number: 3596,
      token: 'token',
      fetchImpl: async () => ({ ok: false, status: 500 }),
    }),
    /HTTP 500/,
  );
});
