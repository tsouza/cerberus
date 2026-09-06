// coverage-merge.mjs — merges cover.out (default-tag lane, required) with
// cover-chdb.out (chdb-tagged lane, optional) into cover-merged.out and runs
// the floor gate. Extracted from the `coverage-merge` Justfile recipe body
// (CLAUDE.md invariant 15, issue #3095, epic #3091) — the recipe now reads
// `coverage-merge:\n    @node .github/scripts/coverage-merge.mjs`, and every
// awk `mode: set` fold this used to hand-copy now goes through the single
// `foldProfile()`/`writeFoldedProfile()` implementation in
// `lib/coverage-fold.mjs`.
//
// Reads whatever cover.out / cover-chdb.out already sit in `cwd`, so it
// works equally after running `coverage-default` + `coverage-chdb` locally
// in the same tree, or after a CI job downloads each lane's profile
// artifact into one directory (tsouza/cerberus#2634) — either way the merge
// is a fact about the files, not about how they got there.
//
// Two folds happen before the coverage-summary.mjs gate runs:
//   1. cover-chdb-ratchet-*.out sibling shard profiles (tsouza/cerberus#2645
//      moved TestCardinalityRatchet's extra shards out of `coverage-chdb`
//      itself into their own CI matrix job) are folded INTO cover-chdb.out
//      first, if any are present. A ratchet shard with no cover-chdb.out to
//      fold into is an error, not a silent no-op — that combination can only
//      mean the main sweep never ran at all.
//   2. cover.out and cover-chdb.out (now including any folded-in ratchet
//      shards) are folded together into cover-merged.out. Absent
//      cover-chdb.out, cover-merged.out is just a copy of cover.out and the
//      lane record says so (`default`, not `default+chdb`).
//
// The lane set decided here is handed to coverage-summary.mjs via
// COVERAGE_LANES, which records it beside the profile as
// cover-merged.out.lanes.json, bound to the profile's own SHA-256 — the ONLY
// thing `update-coverage-floor` accepts as proof a profile carries both
// lanes. This script is the one step that legitimately knows, because it is
// the step that decided.
//
// Deliberately UNCHANGED by this extraction: `coverage-chdb`'s own
// conditional `go test`/COVERAGE_REQUIRE_LANES gate stays literal Justfile
// bash — test/regression/tagged_test_enrollment_test.go statically parses
// that exact recipe body as execution evidence for the chdb-tagged test
// tail, and test/regression/coverage_recipe_fail_closed_test.go counts its
// `go test -timeout` invocations via `just --dump`. Moving that recipe's
// control flow into a script would require rewriting both scanners — real,
// separate work tracked as tsouza/cerberus#3113.
//
// Usage: node .github/scripts/coverage-merge.mjs
// Exit: 1 if cover.out is missing/empty, or if ratchet shard files exist
// with no cover-chdb.out to fold them into; otherwise the exit status of the
// coverage-summary.mjs floor gate it hands off to.

import { copyFileSync, existsSync, readdirSync, statSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import { join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import process from 'node:process';

import { error, log } from './lib/gh.mjs';
import { writeFoldedProfile } from './lib/coverage-fold.mjs';

const COVERAGE_SUMMARY_MJS = new URL('./coverage-summary.mjs', import.meta.url);
const RATCHET_SHARD_PATTERN = /^cover-chdb-ratchet-.*\.out$/;

function isNonEmptyFile(path) {
  try {
    return existsSync(path) && statSync(path).size > 0;
  } catch {
    return false;
  }
}

// ratchetShardFiles — the cover-chdb-ratchet-*.out sibling shard profiles
// among `names` (a directory listing), sorted. Mirrors the replaced bash's
// `shopt -s nullglob; RATCHET_FILES=(cover-chdb-ratchet-*.out)`: an empty
// result when none are present, never the literal unmatched pattern.
export function ratchetShardFiles(names) {
  return names.filter((name) => RATCHET_SHARD_PATTERN.test(name)).sort();
}

// main — the recipe body. Every path is resolved against `cwd` (default
// process.cwd()); `env` seeds the coverage-summary.mjs subprocess's
// environment on top of process.env (a test-only hook — the CLI entry point
// below always uses process.env). Returns the exit status the CLI should
// use; never throws.
export function main({ cwd = process.cwd(), env = process.env } = {}) {
  const p = (name) => join(cwd, name);

  if (!isNonEmptyFile(p('cover.out'))) {
    error("cover.out not found — run 'just coverage-default' first");
    return 1;
  }

  const ratchetFiles = ratchetShardFiles(readdirSync(cwd));
  if (ratchetFiles.length > 0) {
    if (!isNonEmptyFile(p('cover-chdb.out'))) {
      error(`found ratchet shard profile(s) (${ratchetFiles.join(' ')}) but no cover-chdb.out to fold them into`);
      return 1;
    }
    log(`==> folding cover-chdb.out with the ratchet shard profile(s): ${ratchetFiles.join(' ')}`);
    writeFoldedProfile(p('cover-chdb.out'), [p('cover-chdb.out'), ...ratchetFiles.map(p)]);
  }

  let lanes;
  if (isNonEmptyFile(p('cover-chdb.out'))) {
    log('==> merging profiles');
    writeFoldedProfile(p('cover-merged.out'), [p('cover.out'), p('cover-chdb.out')]);
    lanes = 'default+chdb';
  } else {
    log('==> no chdb-tagged profile found, merged profile is default-tag only');
    copyFileSync(p('cover.out'), p('cover-merged.out'));
    lanes = 'default';
  }

  log('');
  const res = spawnSync(process.execPath, [fileURLToPath(COVERAGE_SUMMARY_MJS)], {
    stdio: 'inherit',
    cwd,
    env: { ...env, COVERAGE_LANES: lanes },
  });
  return res.status ?? 1;
}

const invokedDirectly = process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href;
if (invokedDirectly) process.exit(main());
