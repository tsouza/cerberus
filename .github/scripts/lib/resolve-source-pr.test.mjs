// resolve-source-pr.test.mjs — node:test guard for the source-PR resolver
// behind release-preflight.mjs's dedup fix (tsouza/cerberus#2394).
//
// Why it exists separately from `resolve-source-pr.mjs --self-test`: that
// self-test only runs where release.yml or ci.yml's `lint` job invokes it.
// This suite runs on every PR via `node --test`, same rationale as
// release-preflight.test.mjs sitting next to release-preflight.mjs.
//
// What it pins:
//   - the exact-match contract (`selectSourcePR`) that keeps PR resolution
//     FAIL CLOSED: ambiguous or partial matches must resolve to null, never
//     a best guess:
//   - `resolveSourcePR`'s network wrapper: pagination, header shape, and
//     that a non-2xx response throws rather than silently resolving null
//     (a caller must be able to tell "no PR" apart from "the API call
//     itself failed" — this suite's mocked fetch stands in for both).

import { test } from 'node:test';
import assert from 'node:assert/strict';

import { selectSourcePR, resolveSourcePR } from './resolve-source-pr.mjs';

const mergedPR = (overrides = {}) => ({
  number: 2393,
  merged_at: '2026-08-19T12:00:00Z',
  merge_commit_sha: 'merged0000000000000000000000000000000000',
  head: { sha: 'head000000000000000000000000000000000000', ref: 'release/v1.15.3' },
  base: { ref: 'main' },
  ...overrides,
});

const TARGET_SHA = 'merged0000000000000000000000000000000000';

test('selectSourcePR resolves the one exact merge_commit_sha match', () => {
  const r = selectSourcePR([mergedPR()], TARGET_SHA);
  assert.deepEqual(r, {
    number: 2393,
    headSha: 'head000000000000000000000000000000000000',
    headRef: 'release/v1.15.3',
    baseRef: 'main',
  });
});

test('selectSourcePR: no candidates -> null', () => {
  assert.equal(selectSourcePR([], TARGET_SHA), null);
  assert.equal(selectSourcePR(undefined, TARGET_SHA), null);
});

test('selectSourcePR: an open (unmerged) PR must not resolve', () => {
  const r = selectSourcePR([mergedPR({ merged_at: null, merge_commit_sha: null })], TARGET_SHA);
  assert.equal(r, null);
});

test('selectSourcePR: a merged PR with a different merge_commit_sha must not resolve', () => {
  // This is the exact shape `commits/{sha}/pulls` returns for a PR that
  // merely CONTAINS the target commit in its ancestry (e.g. it branched
  // from main after the target landed) — not the PR that produced it.
  const r = selectSourcePR([mergedPR({ merge_commit_sha: 'some-other-sha-entirely' })], TARGET_SHA);
  assert.equal(r, null);
});

test('selectSourcePR: two exact matches is ambiguous, refuses rather than guessing', () => {
  const r = selectSourcePR([mergedPR({ number: 1 }), mergedPR({ number: 2 })], TARGET_SHA);
  assert.equal(r, null);
});

test('selectSourcePR: a match missing head.sha cannot resolve (nothing to read check-runs from)', () => {
  const r = selectSourcePR([mergedPR({ head: { sha: null, ref: 'x' } })], TARGET_SHA);
  assert.equal(r, null);
});

test('selectSourcePR: the one exact match resolves despite unrelated noise', () => {
  const r = selectSourcePR(
    [
      mergedPR({ number: 10, merged_at: null, merge_commit_sha: null }),
      mergedPR({ number: 11, merge_commit_sha: 'unrelated' }),
      mergedPR({ number: 12 }),
    ],
    TARGET_SHA,
  );
  assert.equal(r?.number, 12);
});

test('selectSourcePR: a merge-commit-strategy or rebase-strategy PR resolves the same way as squash', () => {
  // merge_commit_sha carries the same meaning across all three GitHub merge
  // strategies — the fixture shape does not change per-strategy, only the
  // repo's OWN convention (squash, per invariant 1) determines which shape
  // shows up in production. Pinning this keeps the contract from silently
  // narrowing to "squash only".
  const r = selectSourcePR([mergedPR({ merge_commit_sha: TARGET_SHA })], TARGET_SHA);
  assert.equal(r?.number, 2393);
});

// --- resolveSourcePR: the network wrapper ----------------------------------

function fakeFetch(pagesByUrl) {
  const calls = [];
  const impl = async (url, opts) => {
    calls.push({ url, opts });
    const page = pagesByUrl(url);
    if (page.status && page.status !== 200) {
      return { ok: false, status: page.status, statusText: page.statusText ?? 'error' };
    }
    return { ok: true, status: 200, json: async () => page.body };
  };
  impl.calls = calls;
  return impl;
}

test('resolveSourcePR sends bearer auth + the pinned API version header', async () => {
  const fetchImpl = fakeFetch(() => ({ body: [mergedPR()] }));
  const r = await resolveSourcePR({
    repo: 'tsouza/cerberus',
    sha: TARGET_SHA,
    token: 'tok123',
    fetchImpl,
  });
  assert.equal(r?.number, 2393);
  assert.equal(fetchImpl.calls.length, 1);
  const { url, opts } = fetchImpl.calls[0];
  assert.match(url, /\/repos\/tsouza\/cerberus\/commits\/merged0+\/pulls\?per_page=100&page=1$/);
  assert.equal(opts.headers.Authorization, 'Bearer tok123');
  assert.equal(opts.headers['X-GitHub-Api-Version'], '2022-11-28');
});

test('resolveSourcePR paginates until a short page', async () => {
  const fullPage = Array.from({ length: 100 }, (_, i) => mergedPR({ number: i, merge_commit_sha: 'noise' }));
  const fetchImpl = fakeFetch((url) => {
    // Exact `page=` match, not `.includes()` — `page=10` also CONTAINS the
    // substring `page=1`, so a naive `.includes('page=1')` matches every page
    // from 10 onward too and the mock never returns the short page that ends
    // the loop. new URL(...).searchParams is the precise way to read it.
    const page = new URL(url).searchParams.get('page');
    if (page === '1') return { body: fullPage };
    if (page === '2') return { body: [mergedPR({ number: 999 })] };
    throw new Error(`unexpected page in ${url}`);
  });
  const r = await resolveSourcePR({ repo: 'tsouza/cerberus', sha: TARGET_SHA, token: 't', fetchImpl });
  assert.equal(r?.number, 999, 'the match on the second page still resolves');
  assert.equal(fetchImpl.calls.length, 2, 'stopped after the short (< 100) page');
});

test('resolveSourcePR throws on a non-2xx response — a real API failure is never mistaken for "no PR"', async () => {
  const fetchImpl = fakeFetch(() => ({ status: 503, statusText: 'Service Unavailable', body: [] }));
  await assert.rejects(
    () => resolveSourcePR({ repo: 'tsouza/cerberus', sha: TARGET_SHA, token: 't', fetchImpl }),
    /503/,
  );
});

test('resolveSourcePR resolves null (not a throw) when the API succeeds but names no matching PR', async () => {
  const fetchImpl = fakeFetch(() => ({ body: [] }));
  const r = await resolveSourcePR({ repo: 'tsouza/cerberus', sha: TARGET_SHA, token: 't', fetchImpl });
  assert.equal(r, null);
});
