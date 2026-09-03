// coverage-summary.mjs — render the per-package coverage table AND gate on it.
//
// The coverage lane used to compute a number nothing compared against: an
// inline awk aggregated the profile into a step summary and the job went green
// whatever it printed. A report that cannot fail is not a gate, so coverage
// could rot one package at a time with every run still green.
//
// This replaces the awk with the same table plus a floor comparison. Every
// package carrying statements has a committed floor in test/coverage-floor/
// (one shard file per package — see lib/sharded-json.mjs); a package below
// its floor, a package with no floor, a floor of ZERO, and a floor with no
// package are all failures. The two-directional check is deliberate — a
// one-directional one degrades to an allow-list the moment a package stops
// being measured.
//
// A floor of zero is rejected in both directions — the gate refuses to pass one
// and the updater refuses to write one — because it is a floor nothing can fall
// through: a package could have every statement deleted, or every test removed,
// and still clear it. It reads as "measured" while measuring nothing, which is
// exactly the hole a floor exists to close.
//
// The profile is produced with `-coverpkg` over the lane's whole package set
// (see the `coverage` recipe), so a package's coverage is the union of every
// test binary that executes it rather than only its own. That is also why the
// same block arrives many times, once per test binary that linked it:
// parseProfile folds duplicates by taking the widest count, which is what
// `mode: set` means.
//
// The floors are a ratchet: `just update-coverage-floor` raises them to what
// the tree actually achieves and REFUSES to lower one. Lowering a floor is a
// hand-edited, reviewable line in a pull request, never a tool's silent output.
//
// Usage:
//   node .github/scripts/coverage-summary.mjs
//
// Env contract:
//   COVERAGE_PROFILE        Go cover profile to read (default: cover-merged.out).
//   COVERAGE_FLOORS         floor ledger DIRECTORY to compare against, one
//                           shard file per package (default: test/coverage-floor).
//   COVERAGE_LANES          which test lanes produced the profile, as the
//                           Justfile recipe observed them: `default+chdb` or
//                           `default`. Floors are measured with both lanes, so
//                           enforcement is skipped (with a notice) on a
//                           `default`-only profile. Set on the COMPARE path,
//                           where it is also written into the lane record
//                           described below; the UPDATE path reads that record
//                           instead of trusting an environment variable it
//                           cannot check.
//   COVERAGE_REQUIRE_LANES  the lane set the caller guarantees. Set by CI to
//                           `default+chdb`; a mismatch fails rather than
//                           downgrading to the skip above, so a chdb install
//                           that silently no-ops cannot turn the gate off.
//   COVERAGE_UPDATE_FLOORS  `1` rewrites the ledger instead of comparing.
//   GITHUB_STEP_SUMMARY     appended to when present (set by Actions).
//
// The lane record (`<profile>.lanes.json`):
//
// Floors are only meaningful when they come from a profile carrying BOTH lanes,
// and the update path cannot establish that from the filesystem: an installed
// libchdb.so proves the chdb lane COULD have run, never that the profile in
// hand contains it, and a CI `coverage-profile` artifact is routinely enrolled
// from a machine that has no libchdb.so at all (docs/toolchain.md).
//
// So the compare path records the lane set beside the profile, bound to the
// SHA-256 of the profile's own bytes, and the update path reads that record and
// refuses anything it cannot prove is `default+chdb`. The digest is what makes
// the record a statement about THIS profile rather than about the directory it
// sits in: a record left behind by an earlier run, or shipped alongside a
// different profile, no longer matches and is refused.
//
// The property this buys is that every WIRED producer derives the lane set from
// the files it merged (`coverage-merge` sets COVERAGE_LANES from whether a
// non-empty cover-chdb.out was there to merge) and binds it to their bytes, so
// no ordinary run of the recipes can enroll a package from a narrow profile.
// It is not proof against a hand-written record or a hand-set COVERAGE_LANES —
// nothing short of re-deriving coverage would be — and it does not need to be:
// a forged floor still lands as a reviewable line in test/coverage-floor/,
// which is where a deliberate act belongs. The failure this closes is the
// accidental one, which left no line to review at all.
//
// Exit codes:
//   0  every package clears its floor (or the ledger was rewritten).
//   1  unreadable input, a COVERAGE_REQUIRE_LANES mismatch, a lane set the
//      update path cannot prove is `default+chdb`, or a floor violation.

import { createHash } from 'node:crypto';
import { readFileSync, writeFileSync } from 'node:fs';
import process from 'node:process';
import { pathToFileURL } from 'node:url';
import { appendStepSummary, error, log, notice } from './lib/gh.mjs';
import { loadShardedMap, writeShardedMap } from './lib/sharded-json.mjs';

export const DEFAULT_PROFILE = 'cover-merged.out';
const DEFAULT_FLOORS = 'test/coverage-floor';

// The lane set the floors are measured with. A chdb-tagged run reaches code the
// default-tag run cannot compile, so the two profiles are not comparable.
export const FULL_LANES = 'default+chdb';

// Coverage is not bit-reproducible run to run — map iteration order and
// time-dependent branches move a package by a fraction of a point — so a floor
// sits this far below the measurement that produced it. Wide enough to absorb
// the jitter, narrow enough that a deleted test still trips it.
const FLOOR_SLACK_PCT = 1.0;

// Floors are recorded to this many decimals; the profile itself is integer
// statement counts, so more precision would be noise.
const FLOOR_DECIMALS = 1;

const MODULE_PREFIX = 'github.com/tsouza/cerberus/';

// The lane record sits beside the profile it describes, named after it, so a
// profile and its provenance travel together — through a CI artifact upload, a
// download into somebody else's checkout, or a plain `cp`.
const LANE_RECORD_SUFFIX = '.lanes.json';

class GateFailure extends Error {}

function fail(message) {
  error(message, { title: 'coverage floor' });
  // Do not call process.exit immediately after writing the annotation: stdout
  // may be piped by Actions or a test harness, and an immediate exit can drop
  // the only explanation for the red check.
  throw new GateFailure();
}

// packageOf maps a cover-profile file path to the package that owns it: strip
// the module prefix, then drop the file name.
export function packageOf(filePath) {
  const rel = filePath.startsWith(MODULE_PREFIX) ? filePath.slice(MODULE_PREFIX.length) : filePath;
  const cut = rel.lastIndexOf('/');
  return cut === -1 ? rel : rel.slice(0, cut);
}

// parseProfile aggregates a Go cover profile into per-package statement counts.
// Profile lines are `<file>:<startLine>.<col>,<endLine>.<col> <numStmts> <count>`;
// the leading `mode:` line and blank lines are skipped. A block counts as
// covered when its execution count is non-zero, which is what `go tool cover`
// reports and what the awk this replaces computed.
//
// A block may appear many times: `-coverpkg` instruments a package into every
// test binary that links it, and each binary emits its own row. Folding
// them by the widest count per block is the `mode: set` union — the alternative,
// counting each repeat, would inflate `total` by the number of test binaries and
// turn the percentage into an artefact of the suite's shape.
export function parseProfile(text) {
  const blocks = new Map();
  for (const raw of text.split('\n')) {
    const line = raw.trim();
    if (line === '' || line.startsWith('mode:')) continue;

    const fields = line.split(' ');
    if (fields.length < 3) {
      return { err: `malformed profile line: ${line}` };
    }
    const count = Number(fields[fields.length - 1]);
    const stmts = Number(fields[fields.length - 2]);
    const block = fields.slice(0, fields.length - 2).join(' ');
    const colon = block.lastIndexOf(':');
    if (colon === -1 || !Number.isInteger(stmts) || !Number.isFinite(count)) {
      return { err: `malformed profile line: ${line}` };
    }

    const seen = blocks.get(block);
    if (seen === undefined) {
      blocks.set(block, { file: block.slice(0, colon), stmts, count });
    } else {
      seen.stmts = Math.max(seen.stmts, stmts);
      seen.count = Math.max(seen.count, count);
    }
  }

  const packages = new Map();
  for (const { file, stmts, count } of blocks.values()) {
    const pkg = packageOf(file);
    const acc = packages.get(pkg) || { total: 0, covered: 0 };
    acc.total += stmts;
    if (count !== 0) acc.covered += stmts;
    packages.set(pkg, acc);
  }
  if (packages.size === 0) {
    return { err: 'profile contains no coverage blocks' };
  }
  return { packages };
}

export function pct(covered, total) {
  return total > 0 ? (100 * covered) / total : 0;
}

// floorFor rounds a measurement down to the floor it justifies. A measurement
// too thin to clear the slack justifies no floor at all, and says so with 0 —
// callers must treat that as "this package needs a test", never as a floor.
export function floorFor(measured) {
  const scale = 10 ** FLOOR_DECIMALS;
  return Math.max(0, Math.floor((measured - FLOOR_SLACK_PCT) * scale) / scale);
}

// rows renders the per-package table, widest coverage first — the same shape,
// column widths and ordering the inline awk produced, so a run's summary stays
// diffable against the runs that came before this script.
export function rows(packages) {
  return [...packages.entries()]
    .map(([pkg, { total, covered }]) => ({ pkg, total, covered, value: pct(covered, total) }))
    .sort((a, b) => b.value - a.value || a.pkg.localeCompare(b.pkg))
    .map(
      (r) =>
        `${r.value.toFixed(2).padStart(6)}%  ${String(r.covered).padStart(5)} / ${String(r.total).padEnd(5)}  ${r.pkg}`,
    );
}

// compare checks the profile against the ledger in both directions.
export function compare(packages, floors) {
  const below = [];
  const unfloored = [];
  const unfailable = [];
  const missing = [];

  for (const [pkg, { total, covered }] of packages) {
    if (total === 0) continue; // no statements: nothing to floor
    const floor = floors[pkg];
    if (floor === undefined) {
      unfloored.push(pkg);
      continue;
    }
    // A non-positive floor is a recorded value that no measurement can fall
    // through, so it belongs with "no floor at all" rather than with the
    // comparisons below.
    if (!(floor > 0)) {
      unfailable.push(pkg);
      continue;
    }
    const value = pct(covered, total);
    if (value < floor) below.push({ pkg, value, floor });
  }
  for (const pkg of Object.keys(floors)) {
    const measured = packages.get(pkg);
    if (!measured || measured.total === 0) missing.push(pkg);
  }

  return {
    below: below.sort((a, b) => a.pkg.localeCompare(b.pkg)),
    unfloored: unfloored.sort(),
    unfailable: unfailable.sort(),
    missing: missing.sort(),
  };
}

// nextFloors ratchets the ledger up to what the tree achieves. It never lowers
// one: a package that no longer clears its floor is returned as a regression so
// the caller can refuse, because a tool that rewrites the floor to match a drop
// launders the drop into a green run.
//
// Nor does it record a zero. A package whose measurement cannot justify a floor
// above 0 is returned as unfloorable rather than written out, so the ledger
// never gains an entry that no future run can fail.
export function nextFloors(packages, floors) {
  const next = {};
  const regressions = [];
  const unfloorable = [];

  for (const [pkg, { total, covered }] of [...packages.entries()].sort((a, b) => a[0].localeCompare(b[0]))) {
    if (total === 0) continue;
    const value = pct(covered, total);
    const existing = floors[pkg];
    if (existing !== undefined && value < existing) {
      regressions.push({ pkg, value, floor: existing });
    }
    const floor = Math.max(existing ?? 0, floorFor(value));
    if (!(floor > 0)) {
      unfloorable.push({ pkg, value, total });
      continue;
    }
    next[pkg] = floor;
  }
  return {
    next,
    regressions: regressions.sort((a, b) => a.pkg.localeCompare(b.pkg)),
    unfloorable: unfloorable.sort((a, b) => a.pkg.localeCompare(b.pkg)),
  };
}

// packageKeySegments splits a package path into shard-key segments on "/",
// refusing an empty component the way test/rejection-parity/catalogue.go's
// shardName refuses one — an empty segment could only come from a leading,
// trailing or doubled "/", none of which is a real Go import path.
export function packageKeySegments(pkg) {
  const segs = pkg.split('/');
  if (segs.some((s) => s === '')) {
    throw new Error(`package path ${JSON.stringify(pkg)} has an empty "/"-separated component`);
  }
  return segs;
}

function readFloors(dir) {
  try {
    return loadShardedMap(dir);
  } catch (e) {
    return fail(`could not read ${dir}: ${e.message}`);
  }
}

function writeFloors(dir, floors) {
  writeShardedMap(dir, floors, packageKeySegments);
}

function writeSummary(packages) {
  let total = 0;
  let covered = 0;
  for (const p of packages.values()) {
    total += p.total;
    covered += p.covered;
  }
  const table = rows(packages).join('\n');
  appendStepSummary(
    [
      '## Coverage baseline',
      '',
      'Generated by `just coverage`.',
      '',
      '### Total',
      '',
      '```',
      `total: (statements) ${pct(covered, total).toFixed(1)}% — ${covered} / ${total}`,
      '```',
      '',
      '### Per-package (sorted by coverage)',
      '',
      '```',
      table,
      '```',
      '',
    ].join('\n'),
  );
  log(table);
  log(`total: ${pct(covered, total).toFixed(1)}% (${covered} / ${total} statements)`);
}

export function laneRecordPath(profilePath) {
  return `${profilePath}${LANE_RECORD_SUFFIX}`;
}

// profileDigest binds a lane record to the exact bytes it describes.
export function profileDigest(profileText) {
  return createHash('sha256').update(profileText).digest('hex');
}

// writeLaneRecord stamps the lane set the caller observed onto the profile it
// observed it from. Written on the COMPARE path only: the update path is the
// consumer, and a path that wrote its own evidence would be asserting nothing.
function writeLaneRecord(profilePath, profileText, lanes) {
  const record = { lanes, profileSha256: profileDigest(profileText) };
  const recordPath = laneRecordPath(profilePath);
  try {
    writeFileSync(recordPath, `${JSON.stringify(record, null, 2)}\n`);
  } catch (e) {
    // This runs inside the required `coverage` context, so an unwritable
    // directory must arrive as the gate's own annotated failure rather than as
    // a bare stack trace indistinguishable from a coverage regression.
    fail(
      `could not write ${recordPath}: ${e.message}. Without it the profile carries no lane ` +
        `provenance, so \`just update-coverage-floor\` would refuse to derive floors from it.`,
    );
  }
}

// resolveUpdateLanes decides whether a profile may be turned into floors.
//
// Every answer but "this record describes this profile and names both lanes" is
// a refusal, because the alternative to refusing is recording a floor measured
// without the chdb lane. That floor is not merely low: it passes the enrollment
// scan and passes the compare gate, and the ratchet — which refuses to LOWER a
// floor — has no mechanism that ever corrects it upward. Refusing to write
// costs a re-run; writing costs a package its gate, silently and indefinitely.
//
// `recordText` is null when the record could not be read at all.
export function resolveUpdateLanes(recordText, profileText, recordPath) {
  const remedy =
    `Produce the profile with a full \`just coverage\` (after \`just chdb-install\`), or ` +
    `download the \`coverage-profile\` artifact of a heavy CI run whose lane jobs all ` +
    `succeeded — it carries this record alongside the profile. See docs/toolchain.md.`;

  if (recordText === null) {
    return {
      err:
        `${recordPath} does not exist, so the lane set of the profile the floors would be ` +
        `recorded from cannot be established. Floors are measured with both lanes, and one ` +
        `recorded from a default-tag-only profile under-records every package the chdb lane ` +
        `reaches — it still passes enrollment and still passes the gate, so nothing ever ` +
        `corrects it. ${remedy}`,
    };
  }

  let record;
  try {
    record = JSON.parse(recordText);
  } catch (e) {
    return { err: `${recordPath} is not readable as JSON (${e.message}), so it proves nothing. ${remedy}` };
  }
  const shaped =
    record !== null &&
    typeof record === 'object' &&
    typeof record.lanes === 'string' &&
    typeof record.profileSha256 === 'string';
  if (!shaped) {
    return {
      err:
        `${recordPath} is not a lane record: it must be a JSON object carrying a string ` +
        `\`lanes\` and a string \`profileSha256\`. ${remedy}`,
    };
  }

  const digest = profileDigest(profileText);
  if (record.profileSha256 !== digest) {
    return {
      err:
        `${recordPath} describes a different profile (it records sha256 ` +
        `${record.profileSha256}, the profile on disk hashes to ${digest}), so its lane set is ` +
        `not evidence about the profile the floors would come from. A record left over from an ` +
        `earlier run, or shipped next to a profile it did not come from, is exactly the case ` +
        `this digest exists to catch. ${remedy}`,
    };
  }
  if (record.lanes !== FULL_LANES) {
    return {
      err:
        `the profile was produced by the '${record.lanes}' lane set, not '${FULL_LANES}'. Floors ` +
        `recorded from it would under-record every package the chdb lane reaches, and the ` +
        `ratchet never lowers a floor, so nothing would ever correct one written too low. ` +
        `${remedy}`,
    };
  }
  return { lanes: record.lanes };
}

// resolveLanes decides whether the profile can be held to the ledger.
export function resolveLanes(lanes, required) {
  if (required && lanes !== required) {
    return {
      err:
        `the caller requires the '${required}' lane set but the profile was produced by ` +
        `'${lanes || '(unset)'}'. Floors are measured with both lanes, so comparing a narrower ` +
        `profile against them would report drops that are not real — and skipping the ` +
        `comparison would silently disarm the gate. Fix the missing lane (usually a failed ` +
        `\`just chdb-install\`) rather than relaxing this.`,
    };
  }
  return { enforce: lanes === FULL_LANES };
}

function main() {
  const profilePath = process.env.COVERAGE_PROFILE || DEFAULT_PROFILE;
  const floorsDir = process.env.COVERAGE_FLOORS || DEFAULT_FLOORS;

  let text;
  try {
    text = readFileSync(profilePath, 'utf8');
  } catch (e) {
    fail(`could not read ${profilePath}: ${e.message}`);
  }
  const { packages, err } = parseProfile(text);
  if (err) fail(`bad ${profilePath}: ${err}`);

  writeSummary(packages);

  const updating = process.env.COVERAGE_UPDATE_FLOORS === '1';
  const lanesEnv = process.env.COVERAGE_LANES || '';

  // Stamp the provenance BEFORE the comparison below can fail: a red floor gate
  // is precisely when somebody needs to enroll a package from this profile, and
  // the CI artifact that carries it is uploaded whatever the gate said.
  if (!updating && lanesEnv) writeLaneRecord(profilePath, text, lanesEnv);

  const floors = readFloors(floorsDir);

  if (updating) {
    const recordPath = laneRecordPath(profilePath);
    let recordText = null;
    try {
      recordText = readFileSync(recordPath, 'utf8');
    } catch {
      recordText = null;
    }
    const update = resolveUpdateLanes(recordText, text, recordPath);
    if (update.err) fail(update.err);

    const { next, regressions, unfloorable } = nextFloors(packages, floors);
    if (regressions.length) {
      fail(
        `${regressions.length} package(s) sit below a committed floor, and raising the ledger ` +
          `cannot fix that — the floor would have to come DOWN, which launders a coverage drop ` +
          `into a green run. Restore the tests, or lower the floor by hand so the drop is a ` +
          `reviewable line in the diff:\n` +
          regressions.map((r) => `  - ${r.pkg}: ${r.value.toFixed(2)}% < floor ${r.floor}%`).join('\n'),
      );
    }
    if (unfloorable.length) {
      fail(
        `${unfloorable.length} package(s) carry statements but are not exercised enough to justify ` +
          `any floor above 0, and 0 is not a floor — nothing can fall through it, so recording one ` +
          `would leave the package unmeasured while looking measured. The profile is built with ` +
          `\`-coverpkg\` over the whole module, so a test in ANY package counts: give each one a ` +
          `test that reaches it, or delete the code that nothing reaches:\n` +
          unfloorable.map((r) => `  - ${r.pkg}: ${r.value.toFixed(2)}% of ${r.total} statements`).join('\n'),
      );
    }
    writeFloors(floorsDir, next);
    log(`coverage-summary: ${floorsDir} <- ${Object.keys(next).length} package floor(s)`);
    return;
  }

  const lanes = resolveLanes(lanesEnv, process.env.COVERAGE_REQUIRE_LANES || '');
  if (lanes.err) fail(lanes.err);
  if (!lanes.enforce) {
    notice(
      `profile was produced without the chdb lane, so the floors in ${floorsDir} do not apply ` +
        `to it and were not compared. Install libchdb.so (\`just chdb-install\`) to run the gate.`,
      { title: 'coverage floor' },
    );
    return;
  }

  const { below, unfloored, unfailable, missing } = compare(packages, floors);
  const problems = [];
  if (below.length) {
    problems.push(
      `${below.length} package(s) below their committed floor:\n` +
        below.map((r) => `  - ${r.pkg}: ${r.value.toFixed(2)}% < floor ${r.floor}%`).join('\n'),
    );
  }
  if (unfloored.length) {
    problems.push(
      `${unfloored.length} package(s) carry statements but no floor:\n` +
        unfloored.map((p) => `  - ${p}`).join('\n'),
    );
  }
  if (unfailable.length) {
    problems.push(
      `${unfailable.length} package(s) carry statements but a floor of 0, which no measurement can ` +
        `fall through — every test could be deleted and the gate would still pass. A package is ` +
        `either exercised enough to hold a floor above 0 or it is untested; there is no third ` +
        `state to record:\n` +
        unfailable.map((p) => `  - ${p}`).join('\n'),
    );
  }
  if (missing.length) {
    problems.push(
      `${missing.length} floored package(s) contributed no statements to the profile — renamed, ` +
        `deleted, or no longer built by the test lanes:\n` +
        missing.map((p) => `  - ${p}`).join('\n'),
    );
  }
  if (problems.length) {
    fail(`${problems.join('\n\n')}\n\nRun \`just update-coverage-floor\` once the tree is right.`);
  }

  log(`coverage-summary: ${Object.keys(floors).length} package floor(s) clear`);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    main();
  } catch (e) {
    if (!(e instanceof GateFailure)) throw e;
    process.exitCode = 1;
  }
}
