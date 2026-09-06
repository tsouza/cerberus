// migration-tier-seed.test.mjs — node:test guard for migration-tier-seed.mjs's
// pure tier-parameterization, default-archetype-list, manifest-path, and
// seed-argv logic (issue #3099). The real `go run` seeder against a live
// stack is exercised by `just migration-tier1` / `migration-tier2` in the
// real migration-e2e.yml CI job.

import assert from 'node:assert/strict';
import test from 'node:test';

import { DEFAULT_ARCHETYPES, manifestPathFor, resolveArchetypes, seedArgsFor, tierLabel } from './migration-tier-seed.mjs';

test('tierLabel hyphenates the tier name', () => {
  assert.equal(tierLabel('tier1'), 'tier-1');
  assert.equal(tierLabel('tier2'), 'tier-2');
});

test('resolveArchetypes with no requested archetype returns tier1s default two-archetype list', () => {
  assert.deepEqual(resolveArchetypes('tier1', ''), ['three-signal', 'kube-prometheus-stack']);
});

test('resolveArchetypes with no requested archetype returns tier2s default single-archetype list', () => {
  assert.deepEqual(resolveArchetypes('tier2', ''), ['three-signal']);
});

test('resolveArchetypes with an explicit archetype seeds ONLY that one, for either tier', () => {
  assert.deepEqual(resolveArchetypes('tier1', 'kube-prometheus-stack'), ['kube-prometheus-stack']);
  assert.deepEqual(resolveArchetypes('tier2', 'three-signal'), ['three-signal']);
});

test('resolveArchetypes trims whitespace around an explicit archetype', () => {
  assert.deepEqual(resolveArchetypes('tier1', '  three-signal  '), ['three-signal']);
});

test('resolveArchetypes returns null for an unrecognised tier', () => {
  assert.equal(resolveArchetypes('tier3', ''), null);
});

test('resolveArchetypes never lets a caller mutate the shared defaults', () => {
  const list = resolveArchetypes('tier1', '');
  list.push('mutated');
  assert.deepEqual(DEFAULT_ARCHETYPES.tier1, ['three-signal', 'kube-prometheus-stack']);
});

test('manifestPathFor keeps three-signal at the historical unsuffixed path', () => {
  assert.equal(manifestPathFor('three-signal'), 'test/e2e/migration/.out/manifest.json');
});

test('manifestPathFor suffixes every other archetype', () => {
  assert.equal(manifestPathFor('kube-prometheus-stack'), 'test/e2e/migration/.out/manifest-kube-prometheus-stack.json');
});

test('seedArgsFor builds the go run argv for one archetype', () => {
  assert.deepEqual(seedArgsFor('three-signal', 'test/e2e/migration/.out/manifest.json'), [
    'run',
    './test/e2e/migration/cmd/seed',
    '--fixture',
    'test/e2e/migration/archetypes/three-signal/seed/fixture.json',
    '--manifest',
    'test/e2e/migration/.out/manifest.json',
  ]);
});
