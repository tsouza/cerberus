// coverage-fold.mjs — the `mode: set` union fold every coverage-lane profile
// merge in the Justfile used to repeat as its own three-line awk pipeline.
//
// -coverpkg (see just/test.just's own comment on COVERAGE_RAPID_SEED and the
// coverage lanes) makes every linked test binary emit a row for every block
// it links, so a raw profile carries the same block many times over. The
// fold keeps ONE row per block — the WIDEST execution count seen for it
// anywhere — which is exactly `mode: set`'s definition and loses nothing a
// floor gate cares about.
//
// Before this module, that fold was hand-copied three times in
// just/test.just:
//   - `_coverage-fold FILE` (a single-file, in-place fold `coverage-default`
//     runs on cover.out right after writing it);
//   - `coverage-merge`'s ratchet-shard fold (cover-chdb.out plus any
//     cover-chdb-ratchet-*.out sibling shards, folded back into
//     cover-chdb.out — tsouza/cerberus#2645);
//   - `coverage-merge`'s own closing fold (cover.out + cover-chdb.out into
//     cover-merged.out).
// All three ran the identical awk one-liner over a different file list.
// `foldProfile()` is that awk, written once (issue #3095, epic #3091);
// `writeFoldedProfile()` is the read-fold-write-atomically wrapper both the
// CLI below and coverage-merge.mjs call.
//
// CLI: node .github/scripts/lib/coverage-fold.mjs <out-file> <in-file...>
// (an in-place fold, matching the replaced `_coverage-fold FILE` recipe,
// simply repeats <out-file> as the sole <in-file>.)

import { readFileSync, renameSync, writeFileSync } from 'node:fs';
import process from 'node:process';
import { pathToFileURL } from 'node:url';

const PROFILE_HEADER = 'mode: set';

// parseProfileBody — yield [blockKey, count] pairs from one profile's raw
// text, skipping its first line unconditionally (the "mode: set" header),
// matching the replaced awk's `FNR==1{next}` — which skips a file's first
// line by POSITION, not by checking its content.
function parseProfileBody(text) {
  const lines = text.split('\n');
  const rows = [];
  for (let i = 1; i < lines.length; i++) {
    const line = lines[i];
    if (line === '') continue;
    const fields = line.trim().split(/\s+/);
    if (fields.length < 3) continue;
    const [block, numStmt, count] = fields;
    rows.push([`${block} ${numStmt}`, Number(count)]);
  }
  return rows;
}

// foldProfile — fold N raw profile texts (each carrying its own "mode: set"
// header) into one: one row per distinct block, the widest count kept, rows
// sorted (matching the replaced pipeline's trailing `| sort`). Pure — no I/O.
export function foldProfile(texts) {
  const widest = new Map();
  for (const text of texts) {
    for (const [key, count] of parseProfileBody(text)) {
      const current = widest.get(key);
      if (current === undefined || count > current) widest.set(key, count);
    }
  }
  const rows = [...widest.entries()].map(([key, count]) => `${key} ${count}`).sort();
  return [PROFILE_HEADER, ...rows].join('\n') + '\n';
}

// writeFoldedProfile — read every path in `inPaths` fully into memory, fold
// them, and write the result to `outPath`. `outPath` may be one of the
// inputs (the in-place case): every input is read before anything is
// written, and the write itself lands via a rename from a sibling temp file,
// so a reader never observes a partially-written profile — the same
// guarantee the replaced `... > FILE.folded && mv FILE.folded FILE` gave.
export function writeFoldedProfile(outPath, inPaths) {
  const texts = inPaths.map((p) => readFileSync(p, 'utf8'));
  const folded = foldProfile(texts);
  const tmpPath = `${outPath}.folding-${process.pid}`;
  writeFileSync(tmpPath, folded);
  renameSync(tmpPath, outPath);
}

function main() {
  const [outPath, ...inPaths] = process.argv.slice(2);
  if (!outPath || inPaths.length === 0) {
    console.error('coverage-fold: usage: node coverage-fold.mjs <out-file> <in-file...>');
    process.exit(1);
  }
  writeFoldedProfile(outPath, inPaths);
}

const invokedDirectly = process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href;
if (invokedDirectly) main();
