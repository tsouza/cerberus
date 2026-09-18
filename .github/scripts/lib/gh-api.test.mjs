// gh-api.test.mjs — node:test guard for the shared GitHub REST client.
//
// The behaviour that matters is the one the eleven per-script copies used to
// disagree on: what a 404 means. Every caller now states it, and an
// unstated policy is a programming error rather than a silent default.

import { test } from 'node:test';
import assert from 'node:assert/strict';

import {
  GITHUB_API_VERSION,
  GITHUB_PER_PAGE,
  NOT_FOUND_NULL,
  NOT_FOUND_THROW,
  ghHeaders,
  ghJSON,
  ghPaginate,
  withPage,
} from './gh-api.mjs';

function response({ status = 200, statusText = 'OK', body = {} } = {}) {
  return { ok: status >= 200 && status < 300, status, statusText, json: async () => body };
}

function fakeFetch(handler) {
  const calls = [];
  const impl = async (url, opts) => {
    calls.push({ url, opts });
    return response(handler(url, opts));
  };
  impl.calls = calls;
  return impl;
}

test('ghHeaders pins the API version and carries the bearer token', () => {
  const headers = ghHeaders('t0k');
  assert.equal(headers.Authorization, 'Bearer t0k');
  assert.equal(headers['X-GitHub-Api-Version'], GITHUB_API_VERSION);
  assert.equal(headers.Accept, 'application/vnd.github+json');
  assert.equal('User-Agent' in headers, false);

  const raw = ghHeaders('t0k', { accept: 'application/vnd.github.raw+json', userAgent: 'cerberus-x' });
  assert.equal(raw.Accept, 'application/vnd.github.raw+json');
  assert.equal(raw['User-Agent'], 'cerberus-x');
});

test('ghJSON refuses an unstated 404 policy', async () => {
  await assert.rejects(
    () => ghJSON('https://api.invalid/x', { token: 't', fetchImpl: fakeFetch(() => ({})) }),
    /notFound must be null or throw/,
  );
});

test('ghJSON: NOT_FOUND_NULL returns null on 404 and still throws on any other non-2xx', async () => {
  const missing = await ghJSON('https://api.invalid/x', {
    token: 't',
    notFound: NOT_FOUND_NULL,
    fetchImpl: fakeFetch(() => ({ status: 404, statusText: 'Not Found' })),
  });
  assert.equal(missing, null);

  await assert.rejects(
    () =>
      ghJSON('https://api.invalid/x', {
        token: 't',
        what: 'read thing',
        notFound: NOT_FOUND_NULL,
        fetchImpl: fakeFetch(() => ({ status: 503, statusText: 'Service Unavailable' })),
      }),
    /^Error: read thing: HTTP 503 Service Unavailable for https:\/\/api\.invalid\/x$/,
  );
});

test('ghJSON: NOT_FOUND_THROW treats 404 like any other failure, naming the method when `what` is absent', async () => {
  await assert.rejects(
    () =>
      ghJSON('https://api.invalid/x', {
        token: 't',
        notFound: NOT_FOUND_THROW,
        fetchImpl: fakeFetch(() => ({ status: 404, statusText: 'Not Found' })),
      }),
    /^Error: GET: HTTP 404 Not Found for https:\/\/api\.invalid\/x$/,
  );
});

test('ghJSON parses a 2xx body, resolves 204 to null, and merges init headers over the token headers', async () => {
  const fetchImpl = fakeFetch((url, opts) =>
    opts.method === 'POST' ? { status: 204, statusText: 'No Content' } : { body: { hello: 'world' } },
  );
  const body = await ghJSON('https://api.invalid/x', { token: 't', notFound: NOT_FOUND_THROW, fetchImpl });
  assert.deepEqual(body, { hello: 'world' });

  const posted = await ghJSON('https://api.invalid/x', {
    token: 't',
    notFound: NOT_FOUND_THROW,
    init: { method: 'POST', body: '{}', headers: { 'Content-Type': 'application/json' } },
    fetchImpl,
  });
  assert.equal(posted, null);
  const { opts } = fetchImpl.calls[1];
  assert.equal(opts.method, 'POST');
  assert.equal(opts.headers.Authorization, 'Bearer t');
  assert.equal(opts.headers['Content-Type'], 'application/json');
});

test('withPage appends per_page and page with the right separator', () => {
  assert.equal(withPage('https://a/b', 2), `https://a/b?per_page=${GITHUB_PER_PAGE}&page=2`);
  assert.equal(withPage('https://a/b?x=1', 3, 50), 'https://a/b?x=1&per_page=50&page=3');
});

test('ghPaginate walks every page, stops on the first short one, and picks the wrapped array', async () => {
  const pageOf = (n, offset) => Array.from({ length: n }, (_, i) => ({ id: offset + i }));
  const pages = [{ check_runs: pageOf(GITHUB_PER_PAGE, 0) }, { check_runs: pageOf(3, GITHUB_PER_PAGE) }];
  const fetchImpl = fakeFetch((url) => ({ body: pages[Number(new URL(url).searchParams.get('page')) - 1] }));
  const items = await ghPaginate({
    url: 'https://api.invalid/runs?x=1',
    token: 't',
    pick: (body) => body.check_runs,
    fetchImpl,
  });
  assert.equal(items.length, GITHUB_PER_PAGE + 3);
  assert.equal(fetchImpl.calls.length, 2);
  assert.match(fetchImpl.calls[0].url, /\?x=1&per_page=100&page=1$/);
});

test('ghPaginate with maxPages throws on a walk that never shortens instead of returning a prefix', async () => {
  const full = Array.from({ length: GITHUB_PER_PAGE }, (_, i) => ({ id: i }));
  await assert.rejects(
    () =>
      ghPaginate({
        url: 'https://api.invalid/runs',
        token: 't',
        what: 'read runs',
        maxPages: 2,
        fetchImpl: fakeFetch(() => ({ body: full })),
      }),
    /read runs: still returning full pages after 2 of them \(200 items\)/,
  );
});

test('ghPaginate surfaces a non-2xx page as an error', async () => {
  await assert.rejects(
    () =>
      ghPaginate({
        url: 'https://api.invalid/runs',
        token: 't',
        what: 'read runs',
        fetchImpl: fakeFetch(() => ({ status: 403, statusText: 'Forbidden' })),
      }),
    /read runs: HTTP 403 Forbidden/,
  );
});
