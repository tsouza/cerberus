// coverage-summary.mjs — render the per-package coverage table AND gate on it.
//
// The coverage lane used to compute a number nothing compared against: an
// inline awk aggregated the profile into a step summary and the job went green
// whatever it printed. A report that cannot fail is not a gate, so coverage
// could rot one package at a time with every run still green.
//
// This replaces the awk with the same table plus a floor comparison. Every
// package carrying statements has a committed floor in test/coverage-floor.json;
// a package below its floor, a package with no floor, and a floor with no
// package are all failures. The two-directional check is deliberate — a
// one-directional one degrades to an allow-list the moment a package stops
// being measured.
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
//   COVERAGE_FLOORS         floor ledger to compare against
//                           (default: test/coverage-floor.json).
//   COVERAGE_LANES          which test lanes produced the profile, as the
//                           Justfile recipe observed them: `default+chdb` or
//                           `default`. Floors are measured with both lanes, so
//                           enforcement is skipped (with a notice) on a
//                           `default`-only profile.
//   COVERAGE_REQUIRE_LANES  the lane set the caller guarantees. Set by CI to
//                           `default+chdb`; a mismatch fails rather than
//                           downgrading to the skip above, so a chdb install
//                           that silently no-ops cannot turn the gate off.
//   COVERAGE_UPDATE_FLOORS  `1` rewrites the ledger instead of comparing.
//   GITHUB_STEP_SUMMARY     appended to when present (set by Actions).
//
// Exit codes:
//   0  every package clears its floor (or the ledger was rewritten).
//   1  unreadable input, a lane mismatch, or a floor violation.

import { readFileSync, writeFileSync } from 'node:fs';
import process from 'node:process';
import { pathToFileURL } from 'node:url';
import { appendStepSummary, error, log, notice } from './lib/gh.mjs';

const DEFAULT_PROFILE = 'cover-merged.out';
const DEFAULT_FLOORS = 'test/coverage-floor.json';

// The lane set the floors are measured with. A chdb-tagged run reaches code the
// default-tag run cannot compile, so the two profiles are not comparable.
const FULL_LANES = 'default+chdb';

// Coverage is not bit-reproducible run to run — map iteration order and
// time-dependent branches move a package by a fraction of a point — so a floor
// sits this far below the measurement that produced it. Wide enough to absorb
// the jitter, narrow enough that a deleted test still trips it.
const FLOOR_SLACK_PCT = 1.0;

// Floors are recorded to this many decimals; the profile itself is integer
// statement counts, so more precision would be noise.
const FLOOR_DECIMALS = 1;

const MODULE_PREFIX = 'github.com/tsouza/cerberus/';

function fail(message) {
  error(message, { title: 'coverage floor' });
  process.exit(1);
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
export function parseProfile(text) {
  const packages = new Map();
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

    const pkg = packageOf(block.slice(0, colon));
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

// floorFor rounds a measurement down to the floor it justifies.
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
  const missing = [];

  for (const [pkg, { total, covered }] of packages) {
    if (total === 0) continue; // no statements: nothing to floor
    const floor = floors[pkg];
    if (floor === undefined) {
      unfloored.push(pkg);
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
    missing: missing.sort(),
  };
}

// nextFloors ratchets the ledger up to what the tree achieves. It never lowers
// one: a package that no longer clears its floor is returned as a regression so
// the caller can refuse, because a tool that rewrites the floor to match a drop
// launders the drop into a green run.
export function nextFloors(packages, floors) {
  const next = {};
  const regressions = [];

  for (const [pkg, { total, covered }] of [...packages.entries()].sort((a, b) => a[0].localeCompare(b[0]))) {
    if (total === 0) continue;
    const value = pct(covered, total);
    const existing = floors[pkg];
    if (existing !== undefined && value < existing) {
      regressions.push({ pkg, value, floor: existing });
    }
    next[pkg] = Math.max(existing ?? 0, floorFor(value));
  }
  return { next, regressions: regressions.sort((a, b) => a.pkg.localeCompare(b.pkg)) };
}

function readFloors(path) {
  let doc;
  try {
    doc = JSON.parse(readFileSync(path, 'utf8'));
  } catch (e) {
    if (e.code === 'ENOENT') return {};
    fail(`could not read ${path}: ${e.message}`);
  }
  if (doc === null || typeof doc !== 'object' || Array.isArray(doc)) {
    fail(`${path} must be a JSON object mapping package path to floor percentage`);
  }
  return doc;
}

function writeFloors(path, floors) {
  writeFileSync(path, `${JSON.stringify(floors, null, 2)}\n`);
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
  const floorsPath = process.env.COVERAGE_FLOORS || DEFAULT_FLOORS;

  let text;
  try {
    text = readFileSync(profilePath, 'utf8');
  } catch (e) {
    fail(`could not read ${profilePath}: ${e.message}`);
  }
  const { packages, err } = parseProfile(text);
  if (err) fail(`bad ${profilePath}: ${err}`);

  writeSummary(packages);

  const floors = readFloors(floorsPath);

  if (process.env.COVERAGE_UPDATE_FLOORS === '1') {
    const { next, regressions } = nextFloors(packages, floors);
    if (regressions.length) {
      fail(
        `${regressions.length} package(s) sit below a committed floor, and raising the ledger ` +
          `cannot fix that — the floor would have to come DOWN, which launders a coverage drop ` +
          `into a green run. Restore the tests, or lower the floor by hand so the drop is a ` +
          `reviewable line in the diff:\n` +
          regressions.map((r) => `  - ${r.pkg}: ${r.value.toFixed(2)}% < floor ${r.floor}%`).join('\n'),
      );
    }
    writeFloors(floorsPath, next);
    log(`coverage-summary: ${floorsPath} <- ${Object.keys(next).length} package floor(s)`);
    process.exit(0);
  }

  const lanes = resolveLanes(process.env.COVERAGE_LANES || '', process.env.COVERAGE_REQUIRE_LANES || '');
  if (lanes.err) fail(lanes.err);
  if (!lanes.enforce) {
    notice(
      `profile was produced without the chdb lane, so the floors in ${floorsPath} do not apply ` +
        `to it and were not compared. Install libchdb.so (\`just chdb-install\`) to run the gate.`,
      { title: 'coverage floor' },
    );
    process.exit(0);
  }

  const { below, unfloored, missing } = compare(packages, floors);
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
  process.exit(0);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main();
}
