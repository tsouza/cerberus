// sharded-json.mjs — shared storage helpers for a flat JSON ledger that is
// regenerated wholesale by a script but edited, conceptually, one entry at a
// time by many independently-authored PRs.
//
// Mirrors test/rejection-parity/catalogue.go's shard design (#2564, applied
// to the Node-owned ledgers by #2565): the checked-in artifact is a
// DIRECTORY of one small JSON file per entry rather than a single flat file,
// so two PRs adding/editing different entries never write the same path and
// can never conflict on unrelated content. A reader merges every shard back
// into one in-memory object, byte-for-byte the value a single-file artifact
// would have parsed to, so nothing downstream needs to know the artifact is
// sharded.
//
// Shard-file naming: a logical key is first split into SEGMENTS by its
// caller (a package path splits on "/", a "head:symbol" pair splits on ":"),
// then each segment is percent-escaped (see encodeSegment) and the segments
// are rejoined with "__". Escaping — rather than catalogue.go's "reject an
// ambiguous component" — is what keeps the mapping injective and reversible
// even when a segment's own content collides with the join separator or
// contains a literal "/" (the LogQL "op:/" division symbol is exactly this
// case: its symbol segment IS the single character "/").

import { mkdirSync, readdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import path from 'node:path';

export const SHARD_EXT = '.json';

// encodeSegment escapes the three characters that would otherwise be
// ambiguous once segments are joined with "__" into a flat filename:
// "%" (so the escaping itself round-trips), "_" (so no segment's own content
// can spell the "__" join separator), and "/" (so no segment can be mistaken
// for a path separator once written to disk).
export function encodeSegment(seg) {
  if (seg === '') throw new Error('shard key has an empty segment');
  return seg.replace(/%/g, '%25').replace(/_/g, '%5F').replace(/\//g, '%2F');
}

// decodeSegment reverses encodeSegment via a generic %XX decode.
export function decodeSegment(seg) {
  return seg.replace(/%([0-9A-Fa-f]{2})/g, (_, hex) => String.fromCharCode(parseInt(hex, 16)));
}

// shardFileName renders the segments of one logical key into its shard file
// name. The mapping is injective: two distinct segment lists always encode
// to two distinct file names, because encodeSegment removes every "_" and
// "/" from a segment's own content before the "__" join, so a decoder can
// always split back on "__" unambiguously.
export function shardFileName(segments) {
  if (segments.length === 0) throw new Error('shard key has no segments');
  return segments.map(encodeSegment).join('__') + SHARD_EXT;
}

// segmentsFromShardFileName reverses shardFileName.
export function segmentsFromShardFileName(name) {
  if (!name.endsWith(SHARD_EXT)) throw new Error(`${name} is not a ${SHARD_EXT} shard file`);
  const stem = name.slice(0, -SHARD_EXT.length);
  return stem.split('__').map(decodeSegment);
}

// listShardFiles returns the sorted *.json file names directly inside dir,
// or [] when dir does not exist yet (a fresh checkout bootstrapping its
// first regeneration).
export function listShardFiles(dir) {
  let names;
  try {
    names = readdirSync(dir);
  } catch (e) {
    if (e.code === 'ENOENT') return [];
    throw e;
  }
  return names.filter((n) => n.endsWith(SHARD_EXT)).sort();
}

// loadShardedMap reads every shard in dir — each holding exactly one
// {key: value} entry — and merges them into one flat object sorted by key.
// A key present in more than one shard is a corrupt tree (two entries
// claiming the same identity) and is refused rather than silently letting
// the later shard win.
export function loadShardedMap(dir) {
  const merged = {};
  for (const name of listShardFiles(dir)) {
    const shard = JSON.parse(readFileSync(path.join(dir, name), 'utf8'));
    for (const [key, value] of Object.entries(shard)) {
      if (Object.prototype.hasOwnProperty.call(merged, key)) {
        throw new Error(`key ${JSON.stringify(key)} present in more than one shard under ${dir}`);
      }
      merged[key] = value;
    }
  }
  const sorted = {};
  for (const key of Object.keys(merged).sort()) sorted[key] = merged[key];
  return sorted;
}

// loadShardedEntries reads every shard in dir — each holding a
// `{[field]: [...]}` array (see test/surface-parity/inventory_shard.go's
// inventoryShard, or test/rejection-parity/catalogue.go's catalogueShard) —
// and concatenates every shard's array, in shard-file-name order. Unlike
// loadShardedMap this does not sort or dedupe the result: callers that need
// a stable order (byte-for-byte comparison against a generator) sort it
// themselves the way the artifact's own generator does.
export function loadShardedEntries(dir, field = 'entries') {
  const out = [];
  for (const name of listShardFiles(dir)) {
    const shard = JSON.parse(readFileSync(path.join(dir, name), 'utf8'));
    if (!Array.isArray(shard[field])) {
      throw new Error(`shard ${path.join(dir, name)} has no ${JSON.stringify(field)} array`);
    }
    out.push(...shard[field]);
  }
  return out;
}

// writeShardedMap partitions a flat {key: value} object into one shard file
// per entry (2-space indent + trailing newline) and PRUNES shard files that
// no longer own an entry. Pruning is not housekeeping: a stale shard left on
// disk after its entry is deleted keeps feeding a value nothing regenerates
// any more into every future loadShardedMap call (mirrors
// catalogue.go's WriteCatalogue).
export function writeShardedMap(dir, map, keyToSegments) {
  mkdirSync(dir, { recursive: true });
  const wanted = new Set();
  for (const key of Object.keys(map).sort()) {
    const name = shardFileName(keyToSegments(key));
    if (wanted.has(name)) {
      throw new Error(`shard ${name} would be written by more than one key (last: ${JSON.stringify(key)})`);
    }
    wanted.add(name);
    writeFileSync(path.join(dir, name), `${JSON.stringify({ [key]: map[key] }, null, 2)}\n`);
  }
  for (const name of listShardFiles(dir)) {
    if (!wanted.has(name)) rmSync(path.join(dir, name));
  }
}
