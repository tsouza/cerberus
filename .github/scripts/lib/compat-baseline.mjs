// compat-baseline.mjs — the one loader/writer for the sharded compatibility
// parity baseline.
//
// Every consumer gets the old semantic shape back:
//
//   { heads: { <head>: { passed, total, cases } } }
//
// but the committed representation is split into deterministic SHA-256
// buckets. The fixed bucket roster is deliberate: adding a case edits only the
// bucket its ID already owns and never a shared manifest or head-wide index.

import { createHash } from 'node:crypto';
import {
  existsSync,
  mkdirSync,
  readFileSync,
  readdirSync,
  rmSync,
  statSync,
  writeFileSync,
} from 'node:fs';
import path from 'node:path';

export const DEFAULT_BASELINE = 'compatibility/parity-baseline';
export const SCHEMA_VERSION = 1;
export const HASH_ALGORITHM = 'sha256-first-32-bits-modulo-v1';
export const BUCKET_COUNT = 64;
export const SUPPORTED_HEADS = ['prometheus', 'loki', 'tempo', 'tempo-grpc'];

const MANIFEST_NAME = 'manifest.json';
const BUCKET_SUFFIX = '.json';
const HEX_RADIX = 16;
const BUCKET_WIDTH = 2;
const MANIFEST_COMMENT =
  'Parity-regression ratchet baseline. Required compatibility checks reconstruct each head through ' +
  '.github/scripts/lib/compat-baseline.mjs and fail on any recorded case that diverges or vanishes, ' +
  'and on any new case that diverges or is not yet recorded. See .github/scripts/compat-ratchet.mjs ' +
  'and docs/compatibility.md.';
const MANIFEST_CONTRACT =
  "Every case in every deterministic bucket agreed with its reference backend. The loader derives " +
  "passed == total == cases.length and reconstructs one globally sorted roster per head; there is no " +
  "shape for recording an acceptable divergence. Sync a changed head from that run's compat-cases.json " +
  'artifact in the same PR rather than hand-editing buckets.';

function canonicalJson(value) {
  return `${JSON.stringify(value, null, 2)}\n`;
}

function manifestDocument() {
  return {
    schema_version: SCHEMA_VERSION,
    hash: HASH_ALGORITHM,
    bucket_count: BUCKET_COUNT,
    heads: [...SUPPORTED_HEADS],
    _comment: MANIFEST_COMMENT,
    _contract: MANIFEST_CONTRACT,
  };
}

export function bucketNames() {
  return Array.from({ length: BUCKET_COUNT }, (_, index) =>
    index.toString(HEX_RADIX).padStart(BUCKET_WIDTH, '0'),
  );
}

export function bucketForCaseId(id) {
  if (typeof id !== 'string' || id.trim() === '') {
    throw new Error('case ID must be a non-empty string');
  }
  const digest = createHash('sha256').update(id, 'utf8').digest();
  const index = digest.readUInt32BE(0) % BUCKET_COUNT;
  return index.toString(HEX_RADIX).padStart(BUCKET_WIDTH, '0');
}

function bucketDocument(head, bucket, cases) {
  return {
    schema_version: SCHEMA_VERSION,
    head,
    bucket,
    cases,
  };
}

function exactKeys(value, expected, label) {
  if (value == null || typeof value !== 'object' || Array.isArray(value)) {
    throw new Error(`${label}: expected an object`);
  }
  const got = Object.keys(value);
  if (got.length !== expected.length || got.some((key, index) => key !== expected[index])) {
    throw new Error(
      `${label}: expected fields ${expected.join(', ')} in canonical order; got ${got.join(', ') || '(none)'}`,
    );
  }
}

function readCanonicalJson(file, expectedBytes) {
  let text;
  try {
    text = readFileSync(file, 'utf8');
  } catch (error) {
    throw new Error(`${file}: ${error.message}`);
  }
  let value;
  try {
    value = JSON.parse(text);
  } catch (error) {
    throw new Error(`${file}: malformed JSON: ${error.message}`);
  }
  const canonical = expectedBytes?.(value) ?? canonicalJson(value);
  if (text !== canonical) {
    throw new Error(`${file}: bytes are not canonical deterministic JSON`);
  }
  return value;
}

function directoryEntries(dir) {
  try {
    return readdirSync(dir, { withFileTypes: true }).sort((a, b) =>
      a.name < b.name ? -1 : a.name > b.name ? 1 : 0,
    );
  } catch (error) {
    throw new Error(`${dir}: ${error.message}`);
  }
}

function assertExactDirectory(dir, expectedNames, label, { allowMissing = false } = {}) {
  const entries = existsSync(dir) ? directoryEntries(dir) : [];
  const byName = new Map(entries.map((entry) => [entry.name, entry]));
  const expected = new Set(expectedNames);
  const missing = expectedNames.filter((name) => !byName.has(name));
  const extra = entries.filter((entry) => !expected.has(entry.name)).map((entry) => entry.name);
  if ((!allowMissing && missing.length > 0) || extra.length > 0) {
    const parts = [];
    if (!allowMissing && missing.length > 0) parts.push(`missing: ${missing.join(', ')}`);
    if (extra.length > 0) parts.push(`extra: ${extra.join(', ')}`);
    throw new Error(`${label}: bucket roster mismatch (${parts.join('; ')})`);
  }
  return byName;
}

function validateManifest(root) {
  const file = path.join(root, MANIFEST_NAME);
  const manifest = readCanonicalJson(file, () => canonicalJson(manifestDocument()));
  exactKeys(manifest, ['schema_version', 'hash', 'bucket_count', 'heads', '_comment', '_contract'], file);
  const expected = manifestDocument();
  if (
    manifest.schema_version !== expected.schema_version ||
    manifest.hash !== expected.hash ||
    manifest.bucket_count !== expected.bucket_count ||
    manifest._comment !== expected._comment ||
    manifest._contract !== expected._contract ||
    !Array.isArray(manifest.heads) ||
    manifest.heads.length !== expected.heads.length ||
    manifest.heads.some((head, index) => head !== expected.heads[index])
  ) {
    throw new Error(
      `${file}: manifest contract differs from loader (schema/hash/bucket roster/head roster must match exactly)`,
    );
  }
  return manifest;
}

function validateRoot(root, repairHead) {
  const staleMonolith = `${root}.json`;
  if (existsSync(staleMonolith)) {
    throw new Error(
      `${staleMonolith}: stale monolithic parity baseline exists beside the shard tree; remove it`,
    );
  }
  if (!existsSync(root) || !statSync(root).isDirectory()) {
    throw new Error(`${root}: parity baseline directory is missing`);
  }
  const expected = [MANIFEST_NAME, ...SUPPORTED_HEADS];
  const entries = assertExactDirectory(root, expected, root, { allowMissing: repairHead != null });
  for (const name of expected) {
    if (name === repairHead && !entries.has(name)) continue;
    const entry = entries.get(name);
    if (!entry) throw new Error(`${root}: missing ${name}`);
    const wantDirectory = name !== MANIFEST_NAME;
    if (wantDirectory !== entry.isDirectory()) {
      throw new Error(`${path.join(root, name)}: expected ${wantDirectory ? 'a directory' : 'a file'}`);
    }
  }
  validateManifest(root);
}

function validateCases(cases, label) {
  if (!Array.isArray(cases)) throw new Error(`${label}: cases must be an array`);
  const seen = new Set();
  for (const [index, id] of cases.entries()) {
    if (typeof id !== 'string' || id.trim() === '') {
      throw new Error(`${label}: cases[${index}] must be a non-empty string`);
    }
    if (seen.has(id)) throw new Error(`${label}: case ID ${JSON.stringify(id)} appears twice`);
    seen.add(id);
  }
  const sorted = [...cases].sort();
  if (sorted.some((id, index) => id !== cases[index])) {
    throw new Error(`${label}: cases are not sorted deterministically`);
  }
  return cases;
}

function loadHead(root, head) {
  const headDir = path.join(root, head);
  const expectedBuckets = bucketNames();
  const expectedFiles = expectedBuckets.map((bucket) => `${bucket}${BUCKET_SUFFIX}`);
  const entries = assertExactDirectory(headDir, expectedFiles, headDir);
  for (const name of expectedFiles) {
    if (!entries.get(name)?.isFile()) throw new Error(`${path.join(headDir, name)}: expected a file`);
  }

  const owner = new Map();
  const allCases = [];
  for (const bucket of expectedBuckets) {
    const file = path.join(headDir, `${bucket}${BUCKET_SUFFIX}`);
    const document = readCanonicalJson(file, (value) => {
      const cases = Array.isArray(value?.cases) ? value.cases : value?.cases;
      return canonicalJson(bucketDocument(head, bucket, cases));
    });
    exactKeys(document, ['schema_version', 'head', 'bucket', 'cases'], file);
    if (document.schema_version !== SCHEMA_VERSION) {
      throw new Error(`${file}: schema_version must be ${SCHEMA_VERSION}`);
    }
    if (document.head !== head) throw new Error(`${file}: declares wrong head ${JSON.stringify(document.head)}`);
    if (document.bucket !== bucket) {
      throw new Error(`${file}: declares wrong bucket ${JSON.stringify(document.bucket)}`);
    }
    validateCases(document.cases, file);
    for (const id of document.cases) {
      const want = bucketForCaseId(id);
      if (want !== bucket) {
        throw new Error(`${file}: case ${JSON.stringify(id)} belongs in bucket ${want}.json`);
      }
      if (owner.has(id)) {
        throw new Error(
          `${headDir}: case ${JSON.stringify(id)} appears in both ${owner.get(id)}.json and ${bucket}.json`,
        );
      }
      owner.set(id, bucket);
      allCases.push(id);
    }
  }
  allCases.sort();
  if (allCases.length === 0) {
    throw new Error(`${headDir}: reconstructed an empty roster; a hollow head gates nothing`);
  }
  return { passed: allCases.length, total: allCases.length, cases: allCases };
}

/**
 * Load and fully validate the committed shard tree.
 *
 * `repairHead` exists only for sync: the selected head is authoritative from
 * compat-cases.json and may be absent or malformed because it is about to be
 * replaced. The manifest, root roster, stale-monolith check, and every other
 * head remain fail-closed.
 */
export function loadParityBaseline(root = DEFAULT_BASELINE, { repairHead = null } = {}) {
  if (repairHead != null && !SUPPORTED_HEADS.includes(repairHead)) {
    throw new Error(`unknown parity head ${JSON.stringify(repairHead)}`);
  }
  validateRoot(root, repairHead);
  const heads = {};
  for (const head of SUPPORTED_HEADS) {
    if (head === repairHead) continue;
    heads[head] = loadHead(root, head);
  }
  return { heads };
}

function normalizedIds(ids, label) {
  const cases = [...ids];
  validateCases(cases, label);
  return cases;
}

function desiredHeadFiles(head, ids) {
  const groups = new Map(bucketNames().map((bucket) => [bucket, []]));
  for (const id of ids) groups.get(bucketForCaseId(id)).push(id);
  return new Map(
    [...groups].map(([bucket, cases]) => [
      `${bucket}${BUCKET_SUFFIX}`,
      canonicalJson(bucketDocument(head, bucket, cases)),
    ]),
  );
}

function writeIfChanged(file, bytes) {
  let current = null;
  try {
    current = readFileSync(file, 'utf8');
  } catch (error) {
    if (error?.code !== 'ENOENT') throw error;
  }
  if (current === bytes) return false;
  writeFileSync(file, bytes);
  return true;
}

/** Create a complete canonical tree from reconstructed full-parity entries. */
export function materializeParityBaseline(root, baseline) {
  if (baseline == null || typeof baseline !== 'object' || Array.isArray(baseline)) {
    throw new Error('baseline: expected an object');
  }
  exactKeys(baseline.heads, SUPPORTED_HEADS, 'baseline.heads');
  mkdirSync(root, { recursive: true });
  writeFileSync(path.join(root, MANIFEST_NAME), canonicalJson(manifestDocument()));
  for (const head of SUPPORTED_HEADS) {
    const entry = baseline.heads[head];
    exactKeys(entry, ['passed', 'total', 'cases'], `baseline.heads.${head}`);
    const ids = normalizedIds(entry.cases, `baseline.heads.${head}.cases`);
    if (entry.passed !== ids.length || entry.total !== ids.length || ids.length === 0) {
      throw new Error(`baseline.heads.${head}: expected full non-empty parity matching cases.length`);
    }
    const headDir = path.join(root, head);
    mkdirSync(headDir, { recursive: true });
    for (const [name, bytes] of desiredHeadFiles(head, ids)) {
      writeFileSync(path.join(headDir, name), bytes);
    }
  }
}

/**
 * Replace exactly one head from an authoritative, all-passing roster.
 * Unexpected files in that selected directory are pruned; no other head is
 * opened for writing.
 */
export function syncParityBaselineHead(root, head, ids) {
  const sorted = [...ids].sort();
  normalizedIds(sorted, `heads.${head}.cases`);
  if (sorted.length === 0) throw new Error(`heads.${head}: refusing to write a hollow roster`);
  loadParityBaseline(root, { repairHead: head });

  const headDir = path.join(root, head);
  mkdirSync(headDir, { recursive: true });
  const desired = desiredHeadFiles(head, sorted);
  const changed = [];
  const removed = [];

  for (const entry of directoryEntries(headDir)) {
    if (!entry.isFile()) {
      throw new Error(`${path.join(headDir, entry.name)}: unexpected non-file entry; refusing to prune it`);
    }
    if (!desired.has(entry.name)) {
      rmSync(path.join(headDir, entry.name));
      removed.push(path.join(headDir, entry.name));
    }
  }
  for (const [name, bytes] of desired) {
    const file = path.join(headDir, name);
    if (writeIfChanged(file, bytes)) changed.push(file);
  }

  const loaded = loadParityBaseline(root);
  const got = loaded.heads[head].cases;
  if (got.length !== sorted.length || got.some((id, index) => id !== sorted[index])) {
    throw new Error(`heads.${head}: synced roster did not round-trip through the baseline loader`);
  }
  return { changed, removed, baseline: loaded };
}
