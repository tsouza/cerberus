// lib/sharded-json.test.mjs — node:test guard for the sharded-JSON-ledger
// storage helpers (#2564/#2565). Every existing exercise of this module is
// INDIRECT, through a consumer's own `.test.mjs` (coverage-summary.test.mjs,
// coverage-package-floor.test.mjs), and only ever over benign package-path
// keys — never the adversarial content the module's own header comment
// names as the reason the escaping scheme exists at all: a segment
// containing the join separator itself ("_"), the percent escape character
// ("%"), or a literal "/" (LogQL's `op:/` division symbol — its SYMBOL
// segment IS the single character "/"). This file drives encodeSegment /
// decodeSegment / shardFileName / segmentsFromShardFileName directly against
// that content, plus the duplicate-key (loadShardedMap) and duplicate-shard
// (writeShardedMap) error paths no consumer test exercises either.

import assert from 'node:assert/strict';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { test } from 'node:test';

import {
  decodeSegment,
  encodeSegment,
  listShardFiles,
  loadShardedEntries,
  loadShardedMap,
  segmentsFromShardFileName,
  shardFileName,
  writeShardedMap,
} from './sharded-json.mjs';

function withTempDir(fn) {
  const dir = mkdtempSync(path.join(tmpdir(), 'sharded-json-'));
  try {
    return fn(dir);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}

// --- encodeSegment / decodeSegment ------------------------------------------

test('encodeSegment leaves an ordinary segment untouched', () => {
  assert.equal(encodeSegment('internal'), 'internal');
});

test('encodeSegment escapes %, _ and / — the three characters ambiguous after a "__" join', () => {
  assert.equal(encodeSegment('%'), '%25');
  assert.equal(encodeSegment('_'), '%5F');
  assert.equal(encodeSegment('/'), '%2F');
  // The LogQL "op:/" case named in the module header: the symbol segment IS
  // the single character "/".
  assert.equal(encodeSegment('/'), '%2F');
});

test('encodeSegment escapes "%" FIRST so its own escaping round-trips', () => {
  // If "%" were escaped after "_"/"/"" the "%5F"/"%2F" this function itself
  // just produced would be mangled by a second pass over the "%" it contains.
  assert.equal(encodeSegment('%5F'), '%255F');
  assert.equal(decodeSegment(encodeSegment('%5F')), '%5F');
});

test('encodeSegment throws on an empty segment', () => {
  assert.throws(() => encodeSegment(''), /empty segment/);
});

test('decodeSegment reverses encodeSegment via a generic %XX decode', () => {
  for (const raw of ['internal', '%', '_', '/', '__', 'a/b_c%d', 'op:/']) {
    assert.equal(decodeSegment(encodeSegment(raw)), raw);
  }
});

test('decodeSegment is case-insensitive on the hex digits, even though encodeSegment only ever emits uppercase', () => {
  assert.equal(decodeSegment('%2f'), '/');
  assert.equal(decodeSegment('%2F'), '/');
});

// --- shardFileName / segmentsFromShardFileName ------------------------------

test('shardFileName / segmentsFromShardFileName round-trip ordinary segments', () => {
  const segments = ['internal', 'promql'];
  const name = shardFileName(segments);
  assert.equal(name, 'internal__promql.json');
  assert.deepEqual(segmentsFromShardFileName(name), segments);
});

test('shardFileName / segmentsFromShardFileName round-trip the adversarial op:/ case', () => {
  // A "head:symbol" key split on ":" for a division operator: the symbol
  // segment is literally "/".
  const segments = ['op', '/'];
  const name = shardFileName(segments);
  assert.equal(name, 'op__%2F.json');
  assert.deepEqual(segmentsFromShardFileName(name), segments);
});

test('shardFileName distinguishes a segment containing the join separator from an actual segment boundary', () => {
  // The injectivity property the module header claims: ["a__b"] (one
  // segment whose OWN content is "__") must never collide with ["a", "b"]
  // (two segments joined BY "__").
  const oneSegment = shardFileName(['a__b']);
  const twoSegments = shardFileName(['a', 'b']);
  assert.notEqual(oneSegment, twoSegments);
  assert.deepEqual(segmentsFromShardFileName(oneSegment), ['a__b']);
  assert.deepEqual(segmentsFromShardFileName(twoSegments), ['a', 'b']);
});

test('shardFileName round-trips a segment that is itself a literal percent-escape sequence', () => {
  const segments = ['%2F', 'plain'];
  const name = shardFileName(segments);
  assert.deepEqual(segmentsFromShardFileName(name), segments);
});

test('shardFileName throws on an empty segment list', () => {
  assert.throws(() => shardFileName([]), /no segments/);
});

test('segmentsFromShardFileName rejects a name with the wrong extension', () => {
  assert.throws(() => segmentsFromShardFileName('internal__promql.txt'), /not a \.json shard file/);
});

// --- listShardFiles ----------------------------------------------------------

test('listShardFiles returns [] for a directory that does not exist yet', () => {
  withTempDir((dir) => {
    assert.deepEqual(listShardFiles(path.join(dir, 'nope')), []);
  });
});

test('listShardFiles is sorted and ignores non-.json files', () => {
  withTempDir((dir) => {
    writeFileSync(path.join(dir, 'b.json'), '{}');
    writeFileSync(path.join(dir, 'a.json'), '{}');
    writeFileSync(path.join(dir, 'README.md'), 'not a shard');
    assert.deepEqual(listShardFiles(dir), ['a.json', 'b.json']);
  });
});

// --- loadShardedMap ------------------------------------------------------------

test('loadShardedMap merges every shard, sorted by key', () => {
  withTempDir((dir) => {
    writeFileSync(path.join(dir, 'b.json'), JSON.stringify({ b: 2 }));
    writeFileSync(path.join(dir, 'a.json'), JSON.stringify({ a: 1 }));
    assert.deepEqual(loadShardedMap(dir), { a: 1, b: 2 });
    assert.deepEqual(Object.keys(loadShardedMap(dir)), ['a', 'b']);
  });
});

test('loadShardedMap refuses a key present in more than one shard rather than letting the later one win', () => {
  withTempDir((dir) => {
    writeFileSync(path.join(dir, 'a.json'), JSON.stringify({ dup: 1 }));
    writeFileSync(path.join(dir, 'b.json'), JSON.stringify({ dup: 2 }));
    assert.throws(() => loadShardedMap(dir), /"dup".*more than one shard/);
  });
});

test('loadShardedMap on an empty/missing directory is an empty map', () => {
  withTempDir((dir) => {
    assert.deepEqual(loadShardedMap(path.join(dir, 'missing')), {});
  });
});

// --- loadShardedEntries --------------------------------------------------------

test('loadShardedEntries concatenates every shard\'s array in shard-file-name order', () => {
  withTempDir((dir) => {
    writeFileSync(path.join(dir, 'b.json'), JSON.stringify({ entries: [3, 4] }));
    writeFileSync(path.join(dir, 'a.json'), JSON.stringify({ entries: [1, 2] }));
    assert.deepEqual(loadShardedEntries(dir), [1, 2, 3, 4]);
  });
});

test('loadShardedEntries honours a custom field name', () => {
  withTempDir((dir) => {
    writeFileSync(path.join(dir, 'a.json'), JSON.stringify({ rows: ['x'] }));
    assert.deepEqual(loadShardedEntries(dir, 'rows'), ['x']);
  });
});

test('loadShardedEntries throws when a shard is missing the expected array field', () => {
  withTempDir((dir) => {
    writeFileSync(path.join(dir, 'a.json'), JSON.stringify({ entries: 'not an array' }));
    assert.throws(() => loadShardedEntries(dir), /has no "entries" array/);
  });
});

// --- writeShardedMap -----------------------------------------------------------

const splitOnColon = (key) => key.split(':');

test('writeShardedMap writes one shard per key and loadShardedMap reads the same map back', () => {
  withTempDir((dir) => {
    const shardDir = path.join(dir, 'shards');
    const map = { 'op:/': 'division', 'op:+': 'addition', 'a/b:c': 'nested-key' };
    writeShardedMap(shardDir, map, splitOnColon);
    assert.deepEqual(loadShardedMap(shardDir), map);
  });
});

test('writeShardedMap prunes a shard whose entry is no longer in the map', () => {
  withTempDir((dir) => {
    const shardDir = path.join(dir, 'shards');
    writeShardedMap(shardDir, { 'op:/': 'division', 'op:+': 'addition' }, splitOnColon);
    assert.deepEqual(listShardFiles(shardDir).length, 2);
    writeShardedMap(shardDir, { 'op:/': 'division' }, splitOnColon);
    assert.deepEqual(loadShardedMap(shardDir), { 'op:/': 'division' });
    assert.equal(listShardFiles(shardDir).length, 1);
  });
});

test('writeShardedMap refuses two distinct keys that collide on the same shard file name', () => {
  withTempDir((dir) => {
    const shardDir = path.join(dir, 'shards');
    // A deliberately lossy keyToSegments: both "A" and "a" collapse to the
    // same segment, so they would silently overwrite one another on disk —
    // refused instead, matching the same fail-closed posture as
    // loadShardedMap's duplicate-key check.
    const lossy = (key) => [key.toLowerCase()];
    assert.throws(() => writeShardedMap(shardDir, { A: 1, a: 2 }, lossy), /would be written by more than one key/);
  });
});

test('writeShardedMap round-trips the join-separator-ambiguity case end to end', () => {
  withTempDir((dir) => {
    const shardDir = path.join(dir, 'shards');
    // keyToSegments returning a single segment containing "__" must not be
    // confused, on disk, with two segments that happen to join to the same
    // characters.
    const map = { oneSegmentKey: 'x' };
    writeShardedMap(shardDir, map, () => ['a__b']);
    assert.deepEqual(listShardFiles(shardDir), ['a%5F%5Fb.json']);
    assert.deepEqual(loadShardedMap(shardDir), { oneSegmentKey: 'x' });
  });
});
