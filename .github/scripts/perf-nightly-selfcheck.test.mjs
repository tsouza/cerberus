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
// runOneMutation()'s actual `just perf-nightly-integration` invocation and
// the "was it caught" verdict.

import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';

import { MUTATIONS, applyMutation } from './perf-nightly-selfcheck.mjs';

test('MUTATIONS is non-empty and every entry is well-formed', () => {
  assert.ok(MUTATIONS.length > 0, 'an empty mutation set makes the self-check vacuous');
  for (const m of MUTATIONS) {
    assert.equal(typeof m.id, 'string');
    assert.ok(m.id.length > 0);
    assert.equal(typeof m.description, 'string');
    assert.ok(m.description.length > 0);
    assert.equal(typeof m.file, 'string');
    assert.ok(m.file.length > 0);
    assert.notEqual(m.find, m.replace, `${m.id}: find and replace must differ, or the mutation is a no-op`);
  }
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
