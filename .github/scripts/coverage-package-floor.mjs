// coverage-package-floor.mjs — catch new unfloored Go packages on every PR.
//
// The full coverage profile needs chDB and is intentionally kept on the
// push/nightly/release paths. That used to make an ordinary PR's required
// `coverage` check a green no-op: a PR could add a statement-carrying package
// without enrolling it in test/coverage-floor/, merge, and leave the first
// real coverage run red on main.
//
// This is the cheap structural half of that gate. `go list` supplies the files
// active in the default and full chdb+oracle builds, and `go tool cover` tells
// us whether each file actually carries statements. Listing and rewriting do
// not load libchdb, start containers, or run tests. Every resulting package
// must already have a positive floor. Declaration-only packages need no floor,
// and there is no exclusion list to drift.
//
// Usage:
//   node .github/scripts/coverage-package-floor.mjs
//
// Env contract:
//   COVERAGE_FLOORS           floor ledger DIRECTORY to compare against, one
//                             shard file per package (default: test/coverage-floor).
//   COVERAGE_PACKAGE_PATTERN  go-list pattern (default: ./...). Intended for
//                             the hermetic unit fixture; CI leaves it unset.
//
// Exit codes:
//   0  every full-lane package carrying statements has a positive floor and
//      every floor still names a statement-carrying package.
//   1  go-list/cover failed, the ledger is invalid, or enrollment is missing.

import path from 'node:path';
import process from 'node:process';
import { pathToFileURL } from 'node:url';
import { capture, error, log, notice } from './lib/gh.mjs';
import { loadShardedMap } from './lib/sharded-json.mjs';

const DEFAULT_FLOORS = 'test/coverage-floor';
const DEFAULT_PATTERN = './...';
const FILE_SEPARATOR = '\u001f';
const FULL_TAGS = 'chdb,agpl_oracle,chdb_agpl_oracle';

class GateFailure extends Error {}

function failure(message) {
  error(message, { title: 'coverage package floor' });
  // Let Node drain the workflow-command write before exiting. A direct
  // process.exit(1) can discard stdout when the script is run under a pipe,
  // leaving the failed check with no diagnostic at all.
  throw new GateFailure();
}

// parsePackageRows decodes the deliberately small go-list template emitted by
// listPackages. Tabs separate fields and an ASCII unit separator separates
// file basenames; Go package import paths, module paths and source basenames
// cannot contain either delimiter in a valid command-line package.
export function parsePackageRows(text) {
  const packages = [];
  for (const raw of text.split('\n')) {
    if (raw === '') continue;
    const fields = raw.split('\t');
    if (fields.length !== 5) {
      return { err: `malformed go list row: ${JSON.stringify(raw)}` };
    }
    const [importPath, modulePath, dir, goFiles, cgoFiles] = fields;
    if (!modulePath || (importPath !== modulePath && !importPath.startsWith(`${modulePath}/`))) {
      return { err: `package ${importPath} is not inside module ${modulePath || '(missing)'}` };
    }
    packages.push({
      name: importPath === modulePath ? importPath : importPath.slice(modulePath.length + 1),
      dir,
      files: [goFiles, cgoFiles].flatMap((field) => (field === '' ? [] : field.split(FILE_SEPARATOR))),
    });
  }
  if (packages.length === 0) return { err: 'go list returned no packages' };
  return { packages };
}

// coverBlockCount reads the array width Go's cover rewriter assigns to the
// file's statement counts. We need only zero versus non-zero here: the full
// coverage lane remains responsible for the measured percentage.
export function coverBlockCount(instrumented) {
  const match = instrumented.match(/NumStmt:\s*\[(\d+)\]uint16/);
  if (!match) return { err: 'go tool cover output did not declare NumStmt' };
  return { count: Number(match[1]) };
}

// compareEnrollment checks structural enrollment in both directions. The scan
// enumerates the same default + full-tag package union as measured coverage, so
// a floor whose package vanished is as actionable on a PR as a new package
// whose floor is absent. Only the percentage comparison needs the heavy lane.
export function compareEnrollment(statementPackages, floors) {
  const missing = [];
  const nonPositive = [];
  const stale = [];
  const active = new Set(statementPackages);
  for (const pkg of statementPackages) {
    const floor = floors[pkg];
    if (floor === undefined) missing.push(pkg);
    else if (!(typeof floor === 'number' && Number.isFinite(floor) && floor > 0)) nonPositive.push(pkg);
  }
  for (const pkg of Object.keys(floors)) {
    if (!active.has(pkg)) stale.push(pkg);
  }
  return { missing: missing.sort(), nonPositive: nonPositive.sort(), stale: stale.sort() };
}

function readFloors(dir) {
  try {
    return loadShardedMap(dir);
  } catch (e) {
    return failure(`could not read ${dir}: ${e.message}`);
  }
}

function listPackages(pattern, tags = '') {
  const template = [
    '{{.ImportPath}}',
    '{{.Module.Path}}',
    '{{.Dir}}',
    `{{join .GoFiles "${FILE_SEPARATOR}"}}`,
    `{{join .CgoFiles "${FILE_SEPARATOR}"}}`,
  ].join('\t');
  const args = ['list'];
  if (tags !== '') args.push('-tags', tags);
  args.push('-f', template, pattern);
  const listed = capture('go', args);
  if (listed.status !== 0) {
    const tagLabel = tags === '' ? '' : ` -tags ${tags}`;
    failure(`go list${tagLabel} ${pattern} failed:\n${listed.stderr.trim() || listed.stdout.trim()}`);
  }
  const parsed = parsePackageRows(listed.stdout);
  if (parsed.err) failure(parsed.err);
  return parsed.packages;
}

// mergePackageLanes unions active source basenames across the default and
// tagged package universes. A package may select different implementation
// files in each lane (chdbsession is the canonical example), and both must be
// considered without instrumenting the shared files twice.
export function mergePackageLanes(lanes) {
  const merged = new Map();
  for (const lane of lanes) {
    for (const pkg of lane) {
      const found = merged.get(pkg.name);
      if (found && found.dir !== pkg.dir) {
        return { err: `package ${pkg.name} resolved to both ${found.dir} and ${pkg.dir}` };
      }
      const entry = found || { name: pkg.name, dir: pkg.dir, files: new Set() };
      for (const file of pkg.files) entry.files.add(file);
      merged.set(pkg.name, entry);
    }
  }
  return {
    packages: [...merged.values()].map((pkg) => ({ ...pkg, files: [...pkg.files].sort() })),
  };
}

function statementPackages(packages) {
  const carrying = [];
  for (const pkg of packages) {
    for (const file of pkg.files) {
      const covered = capture('go', ['tool', 'cover', '-mode=set', '-var=GoCover', file], { cwd: pkg.dir });
      if (covered.status !== 0) {
        failure(`go tool cover failed for ${path.join(pkg.dir, file)}:\n${covered.stderr.trim()}`);
      }
      const blocks = coverBlockCount(covered.stdout);
      if (blocks.err) failure(`${path.join(pkg.dir, file)}: ${blocks.err}`);
      if (blocks.count > 0) {
        carrying.push(pkg.name);
        break;
      }
    }
  }
  return carrying;
}

function main() {
  const floorDir = process.env.COVERAGE_FLOORS || DEFAULT_FLOORS;
  const pattern = process.env.COVERAGE_PACKAGE_PATTERN || DEFAULT_PATTERN;
  const floors = readFloors(floorDir);
  const merged = mergePackageLanes([listPackages(pattern), listPackages(pattern, FULL_TAGS)]);
  if (merged.err) failure(merged.err);
  const carrying = statementPackages(merged.packages);
  const { missing, nonPositive, stale } = compareEnrollment(carrying, floors);

  const failures = [];
  if (missing.length > 0) {
    failures.push(
      `${missing.length} statement-carrying package(s) have no committed floor:\n  - ${missing.join('\n  - ')}`,
    );
  }
  if (nonPositive.length > 0) {
    failures.push(
      `${nonPositive.length} statement-carrying package(s) have a non-positive or invalid floor:\n` +
        `  - ${nonPositive.join('\n  - ')}`,
    );
  }
  if (stale.length > 0) {
    failures.push(
      `${stale.length} committed floor(s) name no statement-carrying full-lane package:\n  - ` + stale.join('\n  - '),
    );
  }
  if (failures.length > 0) {
    failure(
      `${failures.join('\n\n')}\n\nRun the full 'just coverage' lane, restore any coverage regressions, then ` +
        "run 'just update-coverage-floor'. A zero floor or an exclusion is not enrollment.",
    );
  }

  log(`coverage package floor: ${carrying.length} statement-carrying full-lane package(s) enrolled`);
  notice('Every statement-carrying full-lane package has a positive committed coverage floor.');
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    main();
  } catch (e) {
    if (!(e instanceof GateFailure)) throw e;
    process.exitCode = 1;
  }
}
