// dashboard-matrix.test.mjs — node:test guard for the dashboard (k3d) shard
// partition's coverage invariant.
//
// Runs on the CHEAP gate lane (`node --test .github/scripts/*.test.mjs`) — no
// setup-node, no deps, no k3d cluster — so a dropped/double-assigned spec
// fails on a much cheaper check than the heavy dashboard lane itself, and on
// every PR (the dashboard lane itself never runs on pull_request).
//
// Guards four things:
//   1. the live tree is a clean cover (the real invariant);
//   2. the UNASSIGNED detector actually fires (so it can't silently rot into
//      a no-op);
//   3. the double-assigned detector actually fires;
//   4. exactly one shard runs the Go e2e suite.

import { test } from 'node:test';
import assert from 'node:assert/strict';

import { discover, collectViolations, SHARDS } from './dashboard-matrix.mjs';

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

test('a doubly-counted discovered spec surfaces a dup-discovery flag', () => {
  const dup = discover();
  const violations = collectViolations([...dup, dup[0]]);
  assert.ok(
    violations.some((v) => v.includes('duplicate paths')),
    `expected a duplicate-discovery violation; got:\n${violations.join('\n')}`,
  );
});

test('exactly one shard runs the Go e2e suite', () => {
  const goE2E = SHARDS.filter((s) => s.runGoE2E);
  assert.equal(
    goE2E.length,
    1,
    `expected exactly one runGoE2E shard; got ${goE2E.length}: ${goE2E.map((s) => s.name).join(', ')}`,
  );
});
