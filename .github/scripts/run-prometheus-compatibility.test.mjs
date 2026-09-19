// run-prometheus-compatibility.test.mjs — node:test guard for the pure
// decisions inside the prometheus differential runner: which per-comparison
// deadline a server version gets, that the comparer patch is idempotent
// against a checkout an earlier run already patched, and that a run with no
// usable report never exits 0.

import { test } from 'node:test';
import assert from 'node:assert/strict';

import {
  COMPARER_TIMEOUT_SECONDS,
  DEFAULT_CH_IMAGE,
  testerParallelismArgs,
  FLOOR_COMPARER_TIMEOUT_SECONDS,
  chImageMinor,
  comparerTimeoutSeconds,
  hardFailureExit,
  patchComparerSource,
  reportIsUsable,
} from './run-prometheus-compatibility.mjs';

test('the floor widening applies only below the native-rate floor', () => {
  assert.ok(FLOOR_COMPARER_TIMEOUT_SECONDS > COMPARER_TIMEOUT_SECONDS);
  assert.equal(comparerTimeoutSeconds('clickhouse/clickhouse-server:24.8'), FLOOR_COMPARER_TIMEOUT_SECONDS);
  assert.equal(comparerTimeoutSeconds('clickhouse/clickhouse-server:25.8'), FLOOR_COMPARER_TIMEOUT_SECONDS);
  assert.equal(comparerTimeoutSeconds('clickhouse/clickhouse-server:25.9'), COMPARER_TIMEOUT_SECONDS);
  assert.equal(comparerTimeoutSeconds('clickhouse/clickhouse-server:26.5'), COMPARER_TIMEOUT_SECONDS);
  assert.equal(comparerTimeoutSeconds(DEFAULT_CH_IMAGE), COMPARER_TIMEOUT_SECONDS, 'the default image is a 26.5 lane');
  assert.equal(comparerTimeoutSeconds('clickhouse/clickhouse-server:latest'), COMPARER_TIMEOUT_SECONDS, 'an unreadable tag gets the tight bound');
  assert.equal(comparerTimeoutSeconds(undefined), COMPARER_TIMEOUT_SECONDS);
});

test('chImageMinor reads major.minor from the tag shapes compose uses', () => {
  assert.deepEqual(chImageMinor('clickhouse/clickhouse-server:24.8'), [24, 8]);
  assert.deepEqual(chImageMinor('clickhouse/clickhouse-server:26.5.1.1'), [26, 5]);
  assert.deepEqual(chImageMinor('clickhouse/clickhouse-server:26.6-alpine'), [26, 6]);
  assert.equal(chImageMinor('clickhouse/clickhouse-server:latest'), null);
  assert.equal(chImageMinor('clickhouse/clickhouse-server@sha256:abc'), null);
});

const UPSTREAM = [
  'func (c *Comparer) Compare() {',
  '\tctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)',
  '\tsort.Sort(testResult.(model.Matrix))',
  '\tdiff := cmp.Diff(refResult, testResult)',
  '}',
].join('\n');

test('patchComparerSource sorts both matrices and widens the deadline', () => {
  const patched = patchComparerSource(UPSTREAM, 45);
  assert.match(patched, /sort\.Sort\(testResult\.\(model\.Matrix\)\)\n\tsort\.Sort\(refResult\.\(model\.Matrix\)\)/);
  assert.match(patched, /45\*time\.Second/);
  assert.doesNotMatch(patched, /10\*time\.Second/);
});

test('patchComparerSource is idempotent across a constant change: a checkout patched at 45 re-patches to 90', () => {
  // `.gitmodules` marks the submodule `ignore = dirty`, so a comparer an
  // earlier run patched is what the next run reads. Matching `N*time.Second`
  // by shape rather than by the current constant is what keeps a re-run
  // from failing with "upstream layout changed?".
  const at45 = patchComparerSource(UPSTREAM, 45);
  const at90 = patchComparerSource(at45, 90);
  assert.match(at90, /90\*time\.Second/);
  assert.doesNotMatch(at90, /45\*time\.Second/);
  assert.equal(patchComparerSource(at90, 90), at90, 'a second pass at the same value changes nothing');
  assert.equal((at90.match(/sort\.Sort\(refResult/g) ?? []).length, 1, 'the sort line is inserted once');
});

test('patchComparerSource refuses a source that carries neither line', () => {
  assert.throws(() => patchComparerSource('package comparer\n', 45), /symmetric sort/);
  assert.throws(() => patchComparerSource('sort.Sort(testResult.(model.Matrix))\n', 45), /compare timeout/);
});

test('an unusable report is a failure even when the tester exited 0', () => {
  assert.equal(hardFailureExit(0), 1, 'tester rc 0 with no report must not exit 0');
  assert.equal(hardFailureExit(2), 2, 'a real tester failure code is kept');
  assert.equal(reportIsUsable(null), false);
  assert.equal(reportIsUsable({}), false);
  assert.equal(reportIsUsable({ results: [] }), false, 'zero comparisons measured nothing');
  assert.equal(reportIsUsable({ results: [{ diff: '' }] }), true);
});

test('the lanes default to the parallelism every comparer measurement was taken at', () => {
  assert.deepEqual(testerParallelismArgs({}), ['-query-parallelism', '2']);
  assert.deepEqual(testerParallelismArgs({ TESTER_QUERY_PARALLELISM: '' }), ['-query-parallelism', '2']);
  assert.deepEqual(testerParallelismArgs({ TESTER_QUERY_PARALLELISM: '4' }), ['-query-parallelism', '4']);
});
