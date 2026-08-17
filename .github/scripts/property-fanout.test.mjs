// property-fanout.test.mjs — node:test guard for the property lane's
// fan-out: an explicit per-process go test -timeout that stays below the
// job cap, a histogram sweep fan-out that partitions -rapid.checks without
// dropping or duplicating a check, the roster isolated in its own process,
// every OTHER test in test/property running exactly once across the legs,
// and the silent-truncation detector that turns rapid's own "stopped
// early, still OK" behavior into a hard failure instead of a hollow green.
//
// Runs on the cheap `check` lane (`node --test`), so a regression here fails
// in milliseconds rather than 20 minutes into a chDB job.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import {
  HISTOGRAM_SWEEP_TEST,
  HISTOGRAM_ROSTER_TEST,
  HISTOGRAM_TESTS,
  HISTOGRAM_RUN_PATTERN,
  HISTOGRAM_SWEEP_RUN_PATTERN,
  HISTOGRAM_ROSTER_RUN_PATTERN,
  HISTOGRAM_FANOUT,
  HISTOGRAM_TIMEOUT_MINUTES,
  ROSTER_TIMEOUT_MINUTES,
  REST_TIMEOUT_MINUTES,
  splitRapidChecks,
  legCommands,
  findTruncatedRapidRuns,
} from './property-fanout.mjs';

const here = path.dirname(fileURLToPath(import.meta.url));
const workflow = readFileSync(path.join(here, '..', 'workflows', 'property.yml'), 'utf8');
const TAGS = 'chdb,agpl_oracle,chdb_agpl_oracle';

/** The `timeout-minutes:` of the property job, read out of property.yml. */
function propertyJobTimeoutMinutes() {
  const start = workflow.indexOf('\n  property:');
  assert.ok(start >= 0, 'property.yml: missing property job');
  const job = workflow.slice(start + 1);
  const m = /^\s{4}timeout-minutes:\s*(\d+)/m.exec(job);
  assert.ok(m, 'property.yml: the property job declares no timeout-minutes');
  return Number(m[1]);
}

function sweepLegs(opts = {}) {
  return legCommands({ tags: TAGS, ...opts }).slice(0, HISTOGRAM_FANOUT);
}

function rosterLeg(opts = {}) {
  const legs = legCommands({ tags: TAGS, ...opts });
  return legs[HISTOGRAM_FANOUT];
}

function restLeg(opts = {}) {
  const legs = legCommands({ tags: TAGS, ...opts });
  return legs[legs.length - 1];
}

test('legCommands returns HISTOGRAM_FANOUT sweep shares, one roster leg, and one rest leg', () => {
  const legs = legCommands({ tags: TAGS });
  assert.equal(legs.length, HISTOGRAM_FANOUT + 2);
  assert.equal(legs[HISTOGRAM_FANOUT].name, 'histogram-roster');
  assert.equal(legs[legs.length - 1].name, 'rest');
});

test('every leg declares an explicit go test -timeout', () => {
  for (const leg of legCommands({ tags: TAGS })) {
    assert.ok(
      leg.argv.some((a) => a.startsWith('-timeout=')),
      `${leg.name}: no -timeout — the leg would run under go test's invisible 10m default`,
    );
  }
});

test('every per-process timeout stays strictly below the job cap', () => {
  const jobCap = propertyJobTimeoutMinutes();
  for (const [name, minutes] of [
    ['histogram sweep', HISTOGRAM_TIMEOUT_MINUTES],
    ['histogram roster', ROSTER_TIMEOUT_MINUTES],
    ['rest', REST_TIMEOUT_MINUTES],
  ]) {
    assert.ok(minutes < jobCap, `${name} leg -timeout=${minutes}m must be < the property job's timeout-minutes: ${jobCap}`);
  }
});

test('HISTOGRAM_RUN_PATTERN matches exactly HISTOGRAM_TESTS at the top level', () => {
  const re = new RegExp(HISTOGRAM_RUN_PATTERN);
  for (const name of HISTOGRAM_TESTS) {
    assert.ok(re.test(name), `${name}: must match the rest leg's -skip pattern`);
  }
  assert.ok(!re.test(`${HISTOGRAM_SWEEP_TEST}Extra`), 'pattern must be anchored, not a prefix match');
  assert.ok(!re.test('TestSomethingElse'), 'pattern must not match an unrelated test name');
});

test('the sweep pattern matches only the randomized sweep, never the roster', () => {
  const re = new RegExp(HISTOGRAM_SWEEP_RUN_PATTERN);
  assert.ok(re.test(HISTOGRAM_SWEEP_TEST));
  assert.ok(!re.test(HISTOGRAM_ROSTER_TEST));
});

test('the roster pattern matches only the roster, never the sweep', () => {
  const re = new RegExp(HISTOGRAM_ROSTER_RUN_PATTERN);
  assert.ok(re.test(HISTOGRAM_ROSTER_TEST));
  assert.ok(!re.test(HISTOGRAM_SWEEP_TEST));
});

test('every sweep share selects only the sweep, never the roster', () => {
  for (const leg of sweepLegs()) {
    const pattern = leg.argv[leg.argv.indexOf('-run') + 1];
    assert.equal(pattern, HISTOGRAM_SWEEP_RUN_PATTERN, `${leg.name}: must not also run the roster`);
  }
});

test('the roster leg carries no -rapid.checks or -rapid.shrinktime — it is not a rapid sweep', () => {
  const leg = rosterLeg();
  assert.equal(leg.argv[leg.argv.indexOf('-run') + 1], HISTOGRAM_ROSTER_RUN_PATTERN);
  assert.ok(!leg.argv.some((a) => a.startsWith('-rapid.checks=')), 'roster leg must not set -rapid.checks');
  assert.ok(!leg.argv.some((a) => a.startsWith('-rapid.shrinktime=')), 'roster leg must not set -rapid.shrinktime');
});

test('every histogram-touching leg runs ONLY test/property, not its subpackages', () => {
  for (const leg of [...sweepLegs(), rosterLeg()]) {
    assert.ok(leg.argv.includes('./test/property'), `${leg.name}: must target the single package directly`);
    assert.ok(!leg.argv.includes('./test/property/...'), `${leg.name}: must not also sweep the subpackages`);
  }
});

test('the rest leg sweeps every property package and skips exactly the histogram tests', () => {
  const rest = restLeg();
  assert.ok(rest.argv.includes('./test/property/...'), 'must sweep every property package');
  const skipIndex = rest.argv.indexOf('-skip');
  assert.ok(skipIndex >= 0, 'must exclude the histogram tests with -skip');
  assert.equal(rest.argv[skipIndex + 1], HISTOGRAM_RUN_PATTERN);
});

test('splitRapidChecks sums back to the total and stays within +/-1 per share', () => {
  for (const total of [500, 100, 7, 1, 999]) {
    for (const n of [1, 2, 3, 4]) {
      const shares = splitRapidChecks(total, n);
      assert.equal(shares.length, n);
      assert.equal(shares.reduce((a, b) => a + b, 0), total, `splitRapidChecks(${total}, ${n}) must sum back to ${total}`);
      assert.ok(Math.max(...shares) - Math.min(...shares) <= 1, `splitRapidChecks(${total}, ${n}) shares must differ by at most 1`);
    }
  }
});

test('the sweep fan-out -rapid.checks shares sum to the requested total', () => {
  const total = 500;
  const legs = sweepLegs({ rapidChecks: String(total) });
  const sum = legs.reduce((acc, leg) => acc + Number(leg.argv.find((a) => a.startsWith('-rapid.checks=')).split('=')[1]), 0);
  assert.equal(sum, total, 'every requested check must be assigned to exactly one sweep share');
});

test('each sweep leg records its own expected check depth for truncation detection', () => {
  const total = 500;
  const legs = sweepLegs({ rapidChecks: String(total) });
  const sum = legs.reduce((acc, leg) => acc + leg.expectRapidChecks, 0);
  assert.equal(sum, total);
  for (const leg of legs) {
    const flagValue = Number(leg.argv.find((a) => a.startsWith('-rapid.checks=')).split('=')[1]);
    assert.equal(leg.expectRapidChecks, flagValue, `${leg.name}: expectRapidChecks must match its own -rapid.checks`);
  }
});

test('the rest leg expects the full requested depth; the roster leg expects none (it has no sweep)', () => {
  assert.equal(restLeg({ rapidChecks: '500' }).expectRapidChecks, 500);
  assert.equal(rosterLeg().expectRapidChecks, null);
});

test('the build tags reach every go command verbatim', () => {
  for (const leg of legCommands({ tags: TAGS })) {
    const i = leg.argv.indexOf('-tags');
    assert.ok(i >= 0 && leg.argv[i + 1] === TAGS, `${leg.name}: build tags dropped`);
  }
});

test('-rapid.checks and -rapid.shrinktime appear after the package list', () => {
  for (const leg of [...sweepLegs({ rapidChecks: '500', rapidShrinktime: '5m' }), restLeg({ rapidChecks: '500', rapidShrinktime: '5m' })]) {
    const pkgIndex = leg.argv.findIndex((a) => a.startsWith('./test/property'));
    const checksIndex = leg.argv.findIndex((a) => a.startsWith('-rapid.checks='));
    const shrinkIndex = leg.argv.findIndex((a) => a.startsWith('-rapid.shrinktime='));
    assert.ok(pkgIndex >= 0, `${leg.name}: no package argument`);
    assert.ok(checksIndex > pkgIndex, `${leg.name}: -rapid.checks must follow the package list`);
    assert.ok(shrinkIndex > pkgIndex, `${leg.name}: -rapid.shrinktime must follow the package list`);
  }
});

test('the workflow invokes the fan-out script with the composite tag set', () => {
  assert.match(workflow, /run:\s*node \.github\/scripts\/property-fanout\.mjs/);
  assert.match(workflow, /TAGS:\s*chdb,agpl_oracle,chdb_agpl_oracle/);
});

test('findTruncatedRapidRuns: a full-depth pass reports nothing', () => {
  const out = '    file.go:1: [rapid] OK, passed 167 tests (10m0s)\n--- PASS: Test (600s)';
  assert.deepEqual(findTruncatedRapidRuns(out, 167), []);
});

test('findTruncatedRapidRuns: a short pass is flagged even though rapid called it OK', () => {
  const out = '    file.go:1: [rapid] OK, passed 139 tests (11m36s)\n--- PASS: Test (696s)';
  assert.deepEqual(findTruncatedRapidRuns(out, 167), [{ passed: 139, expected: 167 }]);
});

test('findTruncatedRapidRuns: a leg with no expectation (the roster) is never flagged', () => {
  const out = 'anything at all, even garbage that looks like [rapid] OK, passed 1 tests';
  assert.deepEqual(findTruncatedRapidRuns(out, null), []);
});

test('findTruncatedRapidRuns: multiple rapid tests in one leg are each checked independently', () => {
  const out = ['[rapid] OK, passed 500 tests (10s)', '[rapid] OK, passed 480 tests (12s)', '[rapid] OK, passed 500 tests (9s)'].join(
    '\n',
  );
  assert.deepEqual(findTruncatedRapidRuns(out, 500), [{ passed: 480, expected: 500 }]);
});

test('findTruncatedRapidRuns: passing MORE than expected (should not happen, but must not false-positive) is not flagged', () => {
  const out = '[rapid] OK, passed 501 tests (10s)';
  assert.deepEqual(findTruncatedRapidRuns(out, 500), []);
});
