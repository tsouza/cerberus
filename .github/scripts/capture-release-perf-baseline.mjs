// capture-release-perf-baseline.mjs — freezes the CURRENT rolling
// cardinality ratchet baseline (test/perf/cardinality-baseline/) as a
// permanent, version-labeled snapshot under
// test/perf/release-baseline/<version>/cardinality/, for
// TestReleasePerfRegression (test/perf/release_regression_test.go) to
// compare a future release candidate against.
//
// # Why a SEPARATE frozen tree, not the rolling one
//
// test/perf/cardinality-baseline/ is intentionally rolling: any PR that
// legitimately changes a fixture's structural cost re-baselines it via
// `just update-cardinality-baseline`, and the ratchet (TestCardinalityRatchet)
// only ever compares against whatever was last committed there. That is the
// right design for a merge-blocking per-PR gate — see that test's own doc —
// but it means the rolling baseline is NOT a stable "what did the last
// RELEASE look like" reference: between two releases it can move any number
// of times, each move individually reviewed and justified, with nothing
// checking whether the SUM of those moves regressed against what actually
// shipped last time. This script exists to give that sum a fixed point to
// be measured against.
//
// The frozen tree this script writes is NEVER touched by
// `update-cardinality-baseline` — only by this script, invoked either
// explicitly (backfilling a past release from its tag) or automatically as
// part of `just release-prep` (capturing the release about to ship).
//
// # Two capture modes
//
// --ref <git-ref>   Capture from a git ref (typically a tag, e.g. v1.19.0)
//                    via `git archive`, so a HISTORICAL release can be
//                    backfilled without checking it out.
// (no --ref)         Capture from the CURRENT WORKING TREE, so
//                    `release-prep` can freeze the version about to be
//                    tagged using its own already-staged state.
//
// Usage:
//   node capture-release-perf-baseline.mjs --version 1.19.0 --ref v1.19.0
//   node capture-release-perf-baseline.mjs --version 1.20.0
//
// Writes test/perf/release-baseline/<version>/cardinality/ (a straight
// copy of the source tree's test/perf/cardinality-baseline/, byte-for-byte
// — no re-profiling, since the source's own committed baseline IS the
// historical measurement) plus a meta.json recording provenance
// (source ref, source commit, capture date) so a reviewer can verify what a
// frozen snapshot was actually taken from without re-deriving it.
//
// Refuses to overwrite an existing frozen version — a release baseline is
// written once, at that version's own release, and never again; a second
// capture at the same version number is almost certainly a mistake (or a
// version being re-released, which needs a human decision, not a silent
// overwrite).

import { execFileSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, readdirSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import process from 'node:process';
import { pathToFileURL } from 'node:url';

export const SOURCE_DIR = 'test/perf/cardinality-baseline';
export const DEST_ROOT = 'test/perf/release-baseline';

function parseArgs(argv) {
  const out = { version: null, ref: null };
  for (let i = 0; i < argv.length; i++) {
    if (argv[i] === '--version') out.version = argv[++i];
    else if (argv[i] === '--ref') out.ref = argv[++i];
  }
  return out;
}

// git runs a git subcommand with argv (no shell), returning trimmed stdout.
function git(args, opts = {}) {
  return execFileSync('git', args, { encoding: 'utf8', ...opts }).trim();
}

// captureFromRef copies SOURCE_DIR as it existed at `ref` into `destDir`,
// via `git archive` piped straight into `tar -x` — no intermediate checkout,
// so it works from any worktree regardless of what is currently checked out.
function captureFromRef(ref, destDir) {
  const archive = execFileSync('git', ['archive', ref, '--', SOURCE_DIR], { maxBuffer: 1024 * 1024 * 512 });
  const tmp = mkdtempSync(join(tmpdir(), 'release-perf-baseline-'));
  try {
    execFileSync('tar', ['-x', '-C', tmp], { input: archive });
    const extracted = join(tmp, SOURCE_DIR);
    statSync(extracted); // throws if the ref never had this path — a clear failure, not a silent empty tree
    mkdirSync(destDir, { recursive: true });
    execFileSync('cp', ['-r', extracted + '/.', destDir]);
  } finally {
    rmSync(tmp, { recursive: true, force: true });
  }
}

// captureFromWorkingTree copies SOURCE_DIR as it currently sits on disk —
// used by `release-prep` to freeze the version about to ship, including any
// uncommitted staged changes that release-prep's own commit will carry.
function captureFromWorkingTree(destDir) {
  statSync(SOURCE_DIR);
  mkdirSync(destDir, { recursive: true });
  execFileSync('cp', ['-r', SOURCE_DIR + '/.', destDir]);
}

// countJSONFiles walks dir recursively without relying on Node 20.1+'s
// readdirSync({recursive}) option — some CI runners in this repo still pin
// Node 20.0, where that option throws.
function countJSONFiles(dir) {
  let n = 0;
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const p = join(dir, entry.name);
    if (entry.isDirectory()) n += countJSONFiles(p);
    else if (entry.name.endsWith('.json')) n += 1;
  }
  return n;
}

export function run(argv) {
  const { version, ref } = parseArgs(argv);
  if (!version) {
    console.error('capture-release-perf-baseline: --version <X.Y.Z> is required');
    process.exit(1);
  }
  const destDir = join(DEST_ROOT, version, 'cardinality');
  try {
    statSync(destDir);
    console.error(
      `capture-release-perf-baseline: ${destDir} already exists — a release baseline is captured once, at ` +
        `that version's own release, and never overwritten. Remove it by hand first if this version is ` +
        `genuinely being re-captured (e.g. a botched release re-cut), and say why in the commit that does it.`,
    );
    process.exit(1);
  } catch {
    // ENOENT is the expected, good path — fall through to capture.
  }

  if (ref) {
    captureFromRef(ref, destDir);
  } else {
    captureFromWorkingTree(destDir);
  }

  const n = countJSONFiles(destDir);
  if (n === 0) {
    console.error(`capture-release-perf-baseline: captured ${destDir} but it holds zero .json shards — refusing to commit an empty release baseline`);
    rmSync(join(DEST_ROOT, version), { recursive: true, force: true });
    process.exit(1);
  }

  // `^{commit}` peels an ANNOTATED tag (rev-parse on the bare ref name
  // otherwise returns the tag OBJECT's own sha, not the commit it points
  // at) to the commit it actually names; a no-op for a lightweight tag or
  // a plain commit-ish, which already resolve straight to a commit.
  const sourceCommit = ref ? git(['rev-parse', `${ref}^{commit}`]) : git(['rev-parse', 'HEAD']);
  const meta = {
    version,
    source_dir: SOURCE_DIR,
    source_ref: ref || '(working tree)',
    source_commit: sourceCommit,
    shard_count: n,
  };
  writeFileSync(join(DEST_ROOT, version, 'meta.json'), JSON.stringify(meta, null, 2) + '\n');

  console.log(`capture-release-perf-baseline: froze ${n} shard(s) from ${ref || 'the working tree'} (${sourceCommit}) into ${destDir}`);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  run(process.argv.slice(2));
}
