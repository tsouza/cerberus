// perf-nightly-selfcheck.test.mjs — node:test guard for #2437's mutation
// self-check.
//
// What it pins:
//   - MUTATIONS is well-formed: non-empty, every entry names a real repo
//     file with distinct find/replace text, and its `find` text currently
//     matches the checked-in source EXACTLY ONCE — the same drift check
//     applyMutation() enforces at run time, run here so a source edit that
//     silently invalidates a mutation fails a fast unit test instead of the
//     next (expensive, real-ClickHouse) self-check run;
//   - applyMutation()'s drift detection: a target found once mutates
//     cleanly, zero or multiple occurrences throw rather than silently
//     mutating nothing or something ambiguous.
//
// Deliberately NOT covered here (needs Docker + real ClickHouse, exercised
// by the self-check workflow itself, not this fast unit suite):
// runOneMutation()'s actual `just <recipe>` invocation and the "was it
// caught" verdict.

import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';

import { MUTATIONS, applyMutation, failedSentinels } from './perf-nightly-selfcheck.mjs';

// KNOWN_RECIPES are the two #2370 sentinel gate recipes a mutation may name.
// Pinned as a closed set so a typo'd recipe fails here rather than as a
// `just` "no such recipe" the run would count as the gate catching it.
const KNOWN_RECIPES = new Set(['perf-nightly-integration', 'perf-smoke-integration']);

test('MUTATIONS is non-empty and every entry is well-formed', () => {
  assert.ok(MUTATIONS.length > 0, 'an empty mutation set makes the self-check vacuous');
  for (const m of MUTATIONS) {
    assert.equal(typeof m.id, 'string');
    assert.ok(m.id.length > 0);
    assert.equal(typeof m.description, 'string');
    assert.ok(m.description.length > 0);
    assert.equal(typeof m.file, 'string');
    assert.ok(m.file.length > 0);
    // recipe names the `just` gate that must catch this mutation. Without
    // it runOneMutation would invoke `just undefined`, which fails for the
    // wrong reason and would read as a spurious "caught".
    assert.equal(typeof m.recipe, 'string', `${m.id}: missing recipe`);
    assert.ok(KNOWN_RECIPES.has(m.recipe), `${m.id}: unknown gate recipe ${m.recipe}`);
    assert.notEqual(m.find, m.replace, `${m.id}: find and replace must differ, or the mutation is a no-op`);
  }
});

// SENTINEL_SOURCES are the corpora whose sentinel names a mutation may cite,
// keyed by the recipe that runs them.
const SENTINEL_SOURCES = {
  'perf-nightly-integration': 'test/perf/nightly/sentinels.go',
  'perf-smoke-integration': 'test/perf/smoke/sentinels.go',
};

test('every mutation names sentinels its own recipe runs', () => {
  for (const m of MUTATIONS) {
    assert.ok(Array.isArray(m.sentinels) && m.sentinels.length > 0, `${m.id}: names no sentinel to catch it`);
    const src = readFileSync(SENTINEL_SOURCES[m.recipe], 'utf8');
    for (const name of m.sentinels) {
      assert.match(src, new RegExp(`Name:\\s+"${name}"`), `${m.id}: ${name} is no sentinel of ${m.recipe}`);
    }
  }
});

test('failedSentinels counts only the named subtests that failed', () => {
  const output = [
    '    --- PASS: TestPerfNightlyRealCH/label_values_matcher_stock_key (1.12s)',
    '    --- FAIL: TestPerfNightlyRealCH/label_values_matcher_stock_key_renamed (1.60s)',
    '    --- FAIL: TestPerfNightlyRealCH/classic_native_histogram_derived (18.23s)',
    '        --- FAIL: TestPerfSmokeRealCH/join-spill/join_spill_vector_join (3.01s)',
    'FAIL',
  ].join('\n');
  assert.deepEqual(
    failedSentinels(output, ['label_values_matcher_stock_key', 'label_values_matcher_stock_key_renamed']),
    ['label_values_matcher_stock_key_renamed'],
  );
  assert.deepEqual(failedSentinels(output, ['instant_selector_in_order']), []);
  assert.deepEqual(failedSentinels(output, ['join_spill_vector_join']), ['join_spill_vector_join']);
});

test('MUTATIONS ids are unique', () => {
  const ids = MUTATIONS.map((m) => m.id);
  assert.equal(new Set(ids).size, ids.length);
});

test("every mutation's target text matches the checked-in source exactly once", () => {
  for (const m of MUTATIONS) {
    const src = readFileSync(m.file, 'utf8');
    const occurrences = src.split(m.find).length - 1;
    assert.equal(
      occurrences,
      1,
      `${m.id}: find text found ${occurrences} time(s) in ${m.file} (expected 1) — the source has drifted, ` +
        'update MUTATIONS in perf-nightly-selfcheck.mjs',
    );
  }
});

test('applyMutation replaces a target found exactly once', () => {
  const dir = mkdtempSync(join(tmpdir(), 'perf-nightly-selfcheck-'));
  try {
    const file = join(dir, 'fixture.go');
    writeFileSync(file, 'const limit = 100\nother line\n', 'utf8');
    applyMutation({ id: 'x', file, find: 'const limit = 100\n', replace: 'const limit = 100000\n' });
    assert.equal(readFileSync(file, 'utf8'), 'const limit = 100000\nother line\n');
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('applyMutation throws when the target text is absent (zero occurrences)', () => {
  const dir = mkdtempSync(join(tmpdir(), 'perf-nightly-selfcheck-'));
  try {
    const file = join(dir, 'fixture.go');
    writeFileSync(file, 'const limit = 100\n', 'utf8');
    assert.throws(
      () => applyMutation({ id: 'x', file, find: 'const limit = 999\n', replace: 'const limit = 1\n' }),
      /found 0 time/,
    );
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('applyMutation throws when the target text matches more than once (ambiguous)', () => {
  const dir = mkdtempSync(join(tmpdir(), 'perf-nightly-selfcheck-'));
  try {
    const file = join(dir, 'fixture.go');
    writeFileSync(file, 'const limit = 100\nconst limit = 100\n', 'utf8');
    assert.throws(
      () => applyMutation({ id: 'x', file, find: 'const limit = 100\n', replace: 'const limit = 1\n' }),
      /found 2 time/,
    );
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
