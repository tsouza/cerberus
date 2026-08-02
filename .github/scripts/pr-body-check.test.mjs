// pr-body-check.test.mjs — pins for the PR-description gate off the
// pull_request path.
//
// `pr-body` became a required status check, which means it now runs on three
// surfaces instead of one, and only the first carries a body in the event
// payload. The load-bearing pin here is the agreement between this script and
// forbid-deferral.mjs on what an UNASSOCIATED head commit looks like: the
// exemption is matched on the origin label that resolver produces, so a wording
// change there would silently turn the exemption into either a gate that fails
// every maintenance push or — far worse — a `startsWith` that matches an
// ordinary pull request and waves every stub through. The test drives the real
// resolver rather than restating the label.

import assert from 'node:assert/strict';
import { test } from 'node:test';

import { descriptionSurface } from './forbid-deferral.mjs';
import { classify, inspect, MIN_CHARS } from './pr-body-check.mjs';

const HEAD = '0123456789abcdef0123456789abcdef01234567';
const API = 'https://api.github.test';
const REPO = 'tsouza/cerberus';

// withPulls runs fn with `fetch` answering the associated-pull-requests call
// with the given list.
async function withPulls(pulls, fn) {
  const real = globalThis.fetch;
  globalThis.fetch = async (url) => {
    assert.equal(url, `${API}/repos/${REPO}/commits/${HEAD}/pulls`);
    return { ok: true, status: 200, statusText: 'OK', json: async () => pulls };
  };
  try {
    return await fn();
  } finally {
    globalThis.fetch = real;
  }
}

function resolve() {
  return descriptionSurface({ eventName: 'push', repo: REPO, head: HEAD, token: 't', apiBase: API });
}

test('a head commit with no associated pull request is exempt, not a stub', async () => {
  // A commit that never had a pull request has no description to be a stub. The
  // exemption has to key off the resolver's own label, so this drives it.
  const description = await withPulls([], resolve);
  const verdict = inspect(description);
  assert.equal(verdict.unassociated, true);
  assert.equal(verdict.stub, false);
});

test('an associated pull request with an empty body is still a stub', async () => {
  // The dangerous near-miss: if the exemption matched on emptiness instead of
  // on the origin label, every stub would pass off the pull_request path.
  const description = await withPulls([{ number: 7, body: '' }], resolve);
  const verdict = inspect(description);
  assert.notEqual(verdict.unassociated, true);
  assert.equal(verdict.stub, true);
});

test('an associated pull request with a real body passes and names its origin', async () => {
  const body = 'Chains the parity ledgers into update-golden so a surface change cannot go stale.';
  const description = await withPulls([{ number: 7, body }], resolve);
  assert.match(description.origin, /#7/);
  assert.equal(inspect(description).stub, false);
});

test('two associated pull requests are inspected together', async () => {
  // A commit can be associated with more than one pull request (a backport and
  // its source). The resolver joins them; a stub in one is not rescued by the
  // other being long, and a real description in either is enough.
  const description = await withPulls(
    [
      { number: 7, body: 'cat' },
      { number: 8, body: 'Restores the epoch alignment the inner sample grid needs.' },
    ],
    resolve,
  );
  assert.match(description.origin, /#7, #8/);
  assert.equal(inspect(description).stub, false);
});

test('classify still rejects the stubs the inline self-test names', () => {
  // The gate's original subject, kept under `node --test` so a regression here
  // reports as a failing test rather than as a thrown self-test. The
  // source-comment work markers in PLACEHOLDER are spelled only by the module's
  // own `--self-test`, which the same job runs one step earlier: writing them
  // here would put those tokens in this change's own added lines, where
  // forbid-deferral reads them as real work markers rather than as fixtures.
  for (const stub of ['', '   ', 'cat', 'tbd', 'wip', '<!-- describe your change -->']) {
    assert.equal(classify(stub).stub, true, `expected ${JSON.stringify(stub)} to be a stub`);
  }
  assert.equal(classify('x'.repeat(MIN_CHARS - 1)).stub, true, 'below the minimum length');
  assert.equal(classify('Fixes the Loki tail overflow by advancing the cursor.').stub, false);
});
