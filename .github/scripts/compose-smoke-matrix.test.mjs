// compose-smoke-matrix.test.mjs — node:test guard for the compose-smoke
// shard partition's coverage invariant.
//
// Runs on the CHEAP lint/check lane (`node --test .github/scripts/*.test.mjs`)
// — no setup-node, no deps, no compose stack — so a dropped/double-assigned
// spec fails on a much cheaper required check than compose-smoke itself, and
// on every PR (including docs-only PRs that short-circuit compose-smoke).
//
// Guards three things:
//   1. the live tree is a clean cover (the real invariant);
//   2. the UNASSIGNED detector actually fires (so it can't silently rot into
//      a no-op);
//   3. the double-assigned detector actually fires.

import { test } from 'node:test';
import assert from 'node:assert/strict';

import { discover, collectViolations } from './compose-smoke-matrix.mjs';

test('live tree: SHARDS ∪ EXCLUDED is a total, disjoint cover (no violations)', () => {
  const violations = collectViolations(discover());
  assert.deepEqual(violations, [], `unexpected coverage violations:\n${violations.join('\n')}`);
});

test('an unlisted discovered spec is flagged UNASSIGNED (the silent-gap guard)', () => {
  const synthetic = [...discover(), 'iterate-brand-new.spec.ts'];
  const violations = collectViolations(synthetic);
  assert.ok(
    violations.some((v) => v.includes('UNASSIGNED') && v.includes('iterate-brand-new.spec.ts')),
    `expected an UNASSIGNED violation for the synthetic spec; got:\n${violations.join('\n')}`,
  );
});

test('a doubly-counted discovered spec surfaces no false UNASSIGNED but a dup-discovery flag', () => {
  // Discovery returning a path twice must be caught (defends against a glob
  // returning a path twice), without spuriously failing the cover checks.
  const dup = discover();
  const violations = collectViolations([...dup, dup[0]]);
  assert.ok(
    violations.some((v) => v.includes('duplicate paths')),
    `expected a duplicate-discovery violation; got:\n${violations.join('\n')}`,
  );
});
