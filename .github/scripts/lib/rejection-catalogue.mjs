// rejection-catalogue.mjs — reads test/rejection-parity/catalogue/, the
// rejection-parity catalogue stored as one JSON shard per lowering source file.
//
// Shared by rejection-parity-divergence-liveness.mjs (which needs the merged
// entry list) and rejection-parity-classification.mjs (which needs each
// entry's shard path, to name the file a contributor edits). No env inputs;
// it is a library, not a step.

import { readdirSync, readFileSync } from 'node:fs';
import path from 'node:path';

/** The catalogue's default location, relative to the repository root. */
export const DEFAULT_CATALOGUE_DIR = 'test/rejection-parity/catalogue';

// SHARD_EXT is the suffix of every catalogue shard; the directory holds
// nothing else.
const SHARD_EXT = '.json';

/**
 * Every shard in `dir` as `{ path, doc }`, in name order, `path` joined onto
 * `dir`. An empty (or shardless) directory is an error: a caller that reports
 * "nothing found" as a pass must never reach that conclusion by having read
 * nothing.
 */
export function readCatalogueShards(
  dir,
  { readDir = readdirSync, readFile = (p) => readFileSync(p, 'utf8') } = {},
) {
  const names = readDir(dir).filter((n) => n.endsWith(SHARD_EXT)).sort();
  if (names.length === 0) {
    throw new Error(`${dir} holds no ${SHARD_EXT} shard — the catalogue is a directory of per-source-file shards`);
  }
  return names.map((name) => {
    const shardPath = path.join(dir, name);
    return { path: shardPath, doc: JSON.parse(readFile(shardPath)) };
  });
}

/**
 * The entries of `shards`, concatenated in shard order. A shard with no
 * `entries` array throws rather than contributing nothing — an unreadable
 * shard must never be indistinguishable from a catalogue with nothing in it.
 */
export function mergeShards(shards) {
  const entries = [];
  for (const { path: shardPath, doc } of shards) {
    const shardEntries = doc && Array.isArray(doc.entries) ? doc.entries : null;
    if (shardEntries === null) {
      throw new Error(`${shardPath} has no "entries" array — malformed or wrong file`);
    }
    entries.push(...shardEntries);
  }
  return { entries };
}
