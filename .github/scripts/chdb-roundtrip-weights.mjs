// chdb-roundtrip-weights.mjs — regenerates the per-test weight ledger that
// chdb-roundtrip.mjs's partitionTestNames balances the promql roundtrip legs
// with (`just update-chdb-roundtrip-weights`).
//
// Every run is `go test -json` over both promql roundtrip packages, one
// package at a time (`-p 1`), with the exact tag set the
// `chdb.roundtrip-promql` lane declares in .github/ci-lanes.json:
//
//   1. once with every SELF_SHARDING_TESTS entry skipped and no shard env,
//      timing each other top-level test whole (`tests`);
//   2. once per global leg i of N, with only the SELF_SHARDING_TESTS entries
//      selected and SPEC_SHARD_INDEX/COUNT = i/N, timing each one's slice on
//      that leg (`selfShardLegs`). N is chdb.yml's promql leg count.
//
// The runs are serial, so the ledger's seconds are one host's seconds with
// nothing else of the suite running beside them.
//
// A run with any failing test writes nothing: the weights of a red run
// measure the failure, not the suite.
//
// Inputs (env):
//   GO — the go binary (default `go`).
//
// Output: the ledger at WEIGHTS_LEDGER.promql, seconds rounded to
// WEIGHT_PRECISION_DIGITS decimals, keys sorted, so a regeneration diffs as a
// list of moved weights.

import process from 'node:process';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { readFileSync, writeFileSync } from 'node:fs';
import { error, notice } from './lib/gh.mjs';
import {
  SELF_SHARDING_TESTS,
  SPEC_SHARD_COUNT_ENV,
  SPEC_SHARD_INDEX_ENV,
  WEIGHTS_LEDGER,
  promqlLegCount,
  repoRoot,
  rosterPackages,
} from './chdb-roundtrip.mjs';

/** The head whose roundtrip legs fan out, and so the only one weighed. */
const QL = 'promql';

/** The registry lane whose build tags the measured run must match. */
const LANE_ID = 'chdb.roundtrip-promql';

/** The module path `go test -json` prefixes every package with. */
const MODULE_PREFIX = 'github.com/tsouza/cerberus/';

/** The trailing output lines of a failed package the error quotes. */
const FAILED_OUTPUT_LINES = 20;

/** Decimal places kept per weight: tenths of a second. */
const WEIGHT_PRECISION_DIGITS = 1;

/**
 * The watchdog for each measuring run. The first runs every partitioned test
 * of the suite in one process, many times one CI leg's share, so
 * GO_TEST_TIMEOUT_MINUTES does not apply.
 */
const MEASURE_TIMEOUT_MINUTES = 300;

/** The build tags of `laneId`, read out of the CI lane registry. */
export function laneTags(registry, laneId) {
  const lanes = registry.lanes.filter(({ id }) => id === laneId);
  if (lanes.length !== 1) throw new Error(`ci-lanes.json: expected exactly one ${laneId} lane, found ${lanes.length}`);
  return lanes[0].build_tags.join(',');
}

/**
 * Folds a `go test -json` stream into {packageDir: {TestName: seconds}}.
 *
 * Only top-level tests count (a subtest's time is inside its parent's). A
 * failing test, or a package with no terminal pass event, is an error; a
 * package that failed outside any test (a build error, a panic in TestMain)
 * is reported with the tail of its output.
 */
export function foldTestJson(stream) {
  const ledger = {};
  const failed = [];
  const passedPackages = new Set();
  const packageOutput = new Map();
  for (const line of stream.split('\n')) {
    if (!line.startsWith('{')) continue;
    const e = JSON.parse(line);
    if (!e.Package?.startsWith(MODULE_PREFIX)) continue;
    const pkg = e.Package.slice(MODULE_PREFIX.length);
    if (e.Action === 'output' && !e.Test) {
      if (!packageOutput.has(pkg)) packageOutput.set(pkg, []);
      packageOutput.get(pkg).push(e.Output.trimEnd());
    }
    if (e.Action === 'fail') {
      const tail = (packageOutput.get(pkg) ?? []).slice(-FAILED_OUTPUT_LINES).join('\n');
      failed.push(e.Test ? `${pkg} ${e.Test}` : `${pkg}${tail ? `:\n${tail}` : ''}`);
    }
    if (e.Action !== 'pass') continue;
    if (!e.Test) {
      passedPackages.add(pkg);
      continue;
    }
    if (e.Test.includes('/')) continue;
    ledger[pkg] ??= {};
    ledger[pkg][e.Test] = Number(e.Elapsed.toFixed(WEIGHT_PRECISION_DIGITS));
  }
  if (failed.length > 0) throw new Error(`the measured run failed: ${failed.join(', ')}`);
  for (const pkg of Object.keys(ledger)) {
    if (!passedPackages.has(pkg)) throw new Error(`${pkg} reported tests but never finished`);
  }
  return ledger;
}

/** Sorts an object's keys, recursively through plain objects, leaving arrays in order. */
function sortKeys(value) {
  if (Array.isArray(value) || typeof value !== 'object' || value === null) return value;
  return Object.fromEntries(
    Object.keys(value)
      .sort()
      .map((k) => [k, sortKeys(value[k])]),
  );
}

/**
 * Composes the ledger from the whole run's fold and one fold per leg.
 *
 * Every self-sharding test must report on every leg, in one package only, and
 * never in the whole run, or the per-leg loads the partition starts from
 * would be missing a slice.
 */
export function composeLedger(whole, perLeg, selfSharding) {
  const self = new Set(selfSharding);
  for (const [pkg, byName] of Object.entries(whole)) {
    for (const name of Object.keys(byName)) {
      if (self.has(name)) throw new Error(`self-sharding test ${pkg} ${name} ran in the whole run`);
    }
  }
  const legs = perLeg.length;
  const selfShardLegs = {};
  for (const [i, fold] of perLeg.entries()) {
    const seen = new Set();
    for (const [pkg, byName] of Object.entries(fold)) {
      for (const [name, seconds] of Object.entries(byName)) {
        if (!self.has(name)) throw new Error(`leg ${i + 1}: ${pkg} ${name} is not a self-sharding test`);
        if (seen.has(name)) throw new Error(`leg ${i + 1}: ${name} ran in more than one package`);
        seen.add(name);
        selfShardLegs[pkg] ??= {};
        selfShardLegs[pkg][name] ??= Array(legs).fill(null);
        selfShardLegs[pkg][name][i] = seconds;
      }
    }
    const missing = selfSharding.filter((n) => !seen.has(n));
    if (missing.length > 0) throw new Error(`leg ${i + 1}: no timing for ${missing.join(', ')}`);
  }
  return { legs, selfShardLegs, tests: whole };
}

/** The ledger as committed: object keys sorted, per-leg arrays in leg order. */
export function renderLedger(ledger) {
  return `${JSON.stringify(sortKeys(ledger), null, 2)}\n`;
}

/** The anchored `-run`/`-skip` alternation over `names`. */
function anchored(names) {
  return `^(${names.join('|')})$`;
}

function measure(go, tags, selection, shardEnv) {
  const env = { ...process.env };
  delete env[SPEC_SHARD_INDEX_ENV];
  delete env[SPEC_SHARD_COUNT_ENV];
  Object.assign(env, shardEnv);
  const res = spawnSync(
    go,
    [
      'test',
      '-json',
      '-tags',
      tags,
      '-count=1',
      '-p',
      '1',
      `-timeout=${MEASURE_TIMEOUT_MINUTES}m`,
      ...selection,
      ...rosterPackages(QL).map((dir) => `./${dir}/...`),
    ],
    { cwd: repoRoot, env, encoding: 'utf8', maxBuffer: Number.MAX_SAFE_INTEGER, stdio: ['ignore', 'pipe', 'inherit'] },
  );
  if (res.error) throw res.error;
  return foldTestJson(res.stdout);
}

function main() {
  const registry = JSON.parse(readFileSync(path.join(repoRoot, '.github', 'ci-lanes.json'), 'utf8'));
  const tags = laneTags(registry, LANE_ID);
  const legs = promqlLegCount(readFileSync(path.join(repoRoot, '.github', 'workflows', 'chdb.yml'), 'utf8'));
  const self = SELF_SHARDING_TESTS[QL];
  const go = process.env.GO ?? 'go';
  let ledger;
  try {
    notice(`chdb-roundtrip-weights: timing every partitioned test whole`);
    const whole = measure(go, tags, [`-skip=${anchored(self)}`], {});
    const missing = rosterPackages(QL).filter((dir) => !whole[dir]);
    if (missing.length > 0) throw new Error(`no test ran in ${missing.join(', ')}`);
    const perLeg = [];
    for (let i = 1; i <= legs; i++) {
      notice(`chdb-roundtrip-weights: timing the self-sharding tests on leg ${i} of ${legs}`);
      perLeg.push(
        measure(go, tags, [`-run=${anchored(self)}`], {
          [SPEC_SHARD_INDEX_ENV]: String(i),
          [SPEC_SHARD_COUNT_ENV]: String(legs),
        }),
      );
    }
    ledger = composeLedger(whole, perLeg, self);
  } catch (e) {
    error(`chdb-roundtrip-weights: ${e.message}`);
    process.exit(1);
  }
  writeFileSync(path.join(repoRoot, WEIGHTS_LEDGER[QL]), renderLedger(ledger));
  const count = Object.values(ledger.tests).reduce((n, tests) => n + Object.keys(tests).length, 0);
  notice(`chdb-roundtrip-weights: wrote ${count} test weights and ${self.length} self-sharding tests over ${legs} legs`);
}

if (process.argv[1] && process.argv[1].endsWith('chdb-roundtrip-weights.mjs')) {
  main();
}
