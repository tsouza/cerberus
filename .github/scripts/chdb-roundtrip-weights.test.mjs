// chdb-roundtrip-weights.test.mjs — node:test guard for the promql roundtrip
// weight ledger generator: what a `go test -json` stream folds into, and what
// it refuses to fold.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { composeLedger, foldTestJson, laneTags, renderLedger } from './chdb-roundtrip-weights.mjs';

const here = path.dirname(fileURLToPath(import.meta.url));

const PKG = 'github.com/tsouza/cerberus/internal/promql';

/** One `go test -json` event line. */
function ev(fields) {
  return JSON.stringify(fields);
}

test('only top-level passing tests are weighed, keyed by package directory', () => {
  const stream = [
    ev({ Action: 'run', Package: PKG, Test: 'TestA' }),
    ev({ Action: 'pass', Package: PKG, Test: 'TestA/sub', Elapsed: 9 }),
    ev({ Action: 'pass', Package: PKG, Test: 'TestA', Elapsed: 12.345 }),
    ev({ Action: 'pass', Package: PKG, Test: 'TestB', Elapsed: 0.04 }),
    'not json: a build line',
    ev({ Action: 'pass', Package: PKG, Elapsed: 13 }),
  ].join('\n');
  assert.deepStrictEqual(foldTestJson(stream), { 'internal/promql': { TestA: 12.3, TestB: 0 } });
});

test('a failing test fails the fold, so a red run never becomes the ledger', () => {
  const stream = [
    ev({ Action: 'pass', Package: PKG, Test: 'TestA', Elapsed: 1 }),
    ev({ Action: 'fail', Package: PKG, Test: 'TestB', Elapsed: 2 }),
    ev({ Action: 'fail', Package: PKG, Elapsed: 3 }),
  ].join('\n');
  assert.throws(() => foldTestJson(stream), /internal\/promql TestB/);
});

test('a package that failed outside any test is reported with its output', () => {
  const stream = [
    ev({ Action: 'output', Package: PKG, Output: 'internal/promql/x_test.go:1:1: expected package\n' }),
    ev({ Action: 'fail', Package: PKG, Elapsed: 0 }),
  ].join('\n');
  assert.throws(() => foldTestJson(stream), /internal\/promql:\ninternal\/promql\/x_test\.go:1:1: expected package/);
});

test('a package that never finished fails the fold', () => {
  const stream = ev({ Action: 'pass', Package: PKG, Test: 'TestA', Elapsed: 1 });
  assert.throws(() => foldTestJson(stream), /internal\/promql reported tests but never finished/);
});

test('the rendered ledger sorts every key and keeps per-leg arrays in leg order', () => {
  const rendered = renderLedger({
    tests: { 'internal/promql': { TestZ: 1, TestA: 2 } },
    selfShardLegs: { 'internal/promql': { TestLower: [3, 1, 2] } },
    legs: 3,
  });
  assert.equal(
    rendered,
    '{\n  "legs": 3,\n  "selfShardLegs": {\n    "internal/promql": {\n      "TestLower": [\n        3,\n        1,\n        2\n' +
      '      ]\n    }\n  },\n  "tests": {\n    "internal/promql": {\n      "TestA": 2,\n      "TestZ": 1\n    }\n  }\n}\n',
  );
});

test('composeLedger records each self-sharding test once per leg, in its package', () => {
  const whole = { 'internal/promql': { TestA: 4 } };
  const perLeg = [
    { 'internal/promql': { TestLower: 7 }, 'test/spec/promql': { TestRoundTripChDB: 2 } },
    { 'internal/promql': { TestLower: 3 }, 'test/spec/promql': { TestRoundTripChDB: 5 } },
  ];
  assert.deepStrictEqual(composeLedger(whole, perLeg, ['TestLower', 'TestRoundTripChDB']), {
    legs: 2,
    selfShardLegs: { 'internal/promql': { TestLower: [7, 3] }, 'test/spec/promql': { TestRoundTripChDB: [2, 5] } },
    tests: whole,
  });
});

test('composeLedger refuses a leg missing a self-sharding test, or a self-sharding test in the whole run', () => {
  const self = ['TestLower', 'TestRoundTripChDB'];
  assert.throws(
    () => composeLedger({}, [{ 'internal/promql': { TestLower: 1 } }], self),
    /leg 1: no timing for TestRoundTripChDB/,
  );
  assert.throws(
    () => composeLedger({ 'internal/promql': { TestLower: 1 } }, [], self),
    /self-sharding test internal\/promql TestLower ran in the whole run/,
  );
  assert.throws(
    () => composeLedger({}, [{ 'internal/promql': { TestLower: 1, TestA: 1 }, 'test/spec/promql': { TestRoundTripChDB: 1 } }], self),
    /leg 1: internal\/promql TestA is not a self-sharding test/,
  );
});

test('the measured tags are the chdb.roundtrip-promql lane tags the registry declares', () => {
  const registry = JSON.parse(readFileSync(path.join(here, '..', 'ci-lanes.json'), 'utf8'));
  const lane = registry.lanes.find(({ id }) => id === 'chdb.roundtrip-promql');
  assert.equal(laneTags(registry, 'chdb.roundtrip-promql'), lane.build_tags.join(','));
  assert.throws(() => laneTags(registry, 'no.such-lane'), /expected exactly one no\.such-lane lane, found 0/);
});
