// Unit tests for rejection-parity-divergence-liveness.mjs — run by
// `node --test` before the gate itself runs, mirroring the convention
// forbid-deferral.test.mjs establishes: only the pure halves (no live GitHub
// API call) are exercised, and the network half (resolveIssues) is left
// untested here because it is a thin loop around apiJson +
// issuesReadability — both already pinned by forbid-deferral.test.mjs
// against the same two-request capability-probe contract this script
// imports and reuses unchanged.

import assert from 'node:assert/strict';
import test from 'node:test';

import {
  divergenceEntries,
  livenessViolation,
  structuralViolation,
} from './rejection-parity-divergence-liveness.mjs';

// --- divergenceEntries -------------------------------------------------------

test('divergenceEntries keeps only class=divergence, in file order', () => {
  const catalogue = {
    entries: [
      { site: 'a', class: 'rejection' },
      { site: 'b', class: 'divergence', tracking_issue: 1759 },
      { site: 'c', class: 'internal' },
      { site: 'd', class: 'divergence', tracking_issue: 42 },
    ],
  };
  assert.deepEqual(
    divergenceEntries(catalogue).map((e) => e.site),
    ['b', 'd'],
  );
});

test('divergenceEntries is empty, not an error, when the catalogue has none', () => {
  assert.deepEqual(divergenceEntries({ entries: [{ site: 'a', class: 'rejection' }] }), []);
});

test('divergenceEntries rejects a catalogue with no entries array — a wrong file is not a clean pass', () => {
  assert.throws(() => divergenceEntries({}), /malformed or wrong file/);
  assert.throws(() => divergenceEntries(null), /malformed or wrong file/);
});

// --- structuralViolation -----------------------------------------------------

test('a positive integer tracking_issue is structurally fine', () => {
  assert.equal(structuralViolation({ tracking_issue: 1759 }), null);
});

for (const bad of [0, -1, 1.5, undefined, null, '1759', '#1759']) {
  test(`structuralViolation rejects tracking_issue=${JSON.stringify(bad)}`, () => {
    assert.match(structuralViolation({ tracking_issue: bad }), /must be a positive integer/);
  });
}

// --- livenessViolation -------------------------------------------------------

// The same three real fixtures forbid-deferral.test.mjs uses (RESOLVED map):
// #1535 open issue, #1486 closed issue, #1143 closed pull request. Reusing
// them keeps the two gates' notion of "what a resolved citation looks like"
// visibly in lock-step rather than independently invented.
test('an open issue satisfies the liveness claim', () => {
  assert.equal(livenessViolation(1535, { kind: 'issue', state: 'open' }), null);
});

test('a missing resolution (never looked up / not in the map) fails', () => {
  assert.match(livenessViolation(9999, undefined), /names nothing/);
});

test('a lookup that resolved to "missing" fails the same way', () => {
  assert.match(livenessViolation(9999, { kind: 'missing' }), /names nothing/);
});

test('a pull request is not an issue', () => {
  assert.match(livenessViolation(1143, { kind: 'pull-request', state: 'closed' }), /pull request, not an issue/);
});

test('a closed issue fails and explains the three remedies', () => {
  const detail = livenessViolation(1486, { kind: 'issue', state: 'closed' });
  assert.match(detail, /closed issue/);
  assert.match(detail, /re-filed/);
  assert.match(detail, /deleted/);
  assert.match(detail, /reclassified to "rejection"/);
});
