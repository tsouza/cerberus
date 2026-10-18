// ch-server-safety-shards.test.mjs — node:test guard for the ch-server-safety
// shard plan: the shards cover every test of test/chserver, each cancellation
// shard selects exactly its build, and neither a shard that runs nothing nor a
// failed or skipped shard job can pass.

import { test } from 'node:test';
import process from 'node:process';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

import {
  CANCELLATION_TEST,
  KIND_CANCELLATION,
  KIND_REST,
  SHARD_ALL,
  SHARD_REST,
  aggregate,
  builtUnderIntegration,
  cancellationImages,
  cancellationShardId,
  exactNamePattern,
  plan,
  readPackage,
  selection,
  selectionArgs,
  shardImages,
  topLevelTests,
  verdict,
} from './ch-server-safety-shards.mjs';

const fixture = `
var cancellationBuilds = []struct {
	image        string
	foldBounded  bool
	regexBounded bool
}{
	{"clickhouse/clickhouse-server:26.6.1.1193-alpine", false, false},
	{"clickhouse/clickhouse-server:26.8.10.6-alpine", true, true},
}

func TestCancellation_CPUBoundEmittedShapesAcrossBuilds(t *testing.T) {
	for _, b := range cancellationBuilds {
		_ = struct{ x string }{"not a row"}
	}
}
`;
const a = 'clickhouse/clickhouse-server:26.6.1.1193-alpine';
const b = 'clickhouse/clickhouse-server:26.8.10.6-alpine';
const tests = ['TestConditionCache_X', CANCELLATION_TEST];
const pinned = ['clickhouse/clickhouse-server:25.3.14.14-alpine', a, b];
const pkg = { cancellation: [a, b], tests };

test('the cancellation table is read row by row, in order, and nothing past it', () => {
  assert.deepEqual(cancellationImages(fixture), [a, b]);
});

test('a missing or empty cancellation table is an error, never an empty plan', () => {
  assert.throws(() => cancellationImages('package chserver\n'), /no `var cancellationBuilds` table/);
  assert.throws(
    () => cancellationImages('var cancellationBuilds = []struct {\n\timage string\n}{\n}\n'),
    /lists no builds/,
  );
});

test('top-level tests are every Test function, deduplicated and sorted', () => {
  assert.deepEqual(topLevelTests([fixture, 'func TestB(t *testing.T) {}\nfunc helper(t *testing.T) {}\n']), [
    'TestB',
    CANCELLATION_TEST,
  ]);
});

test('an exact name pattern anchors every level, including the levels of an image name', () => {
  assert.equal(
    exactNamePattern(`${CANCELLATION_TEST}/${a}`),
    `^${CANCELLATION_TEST}$/^clickhouse$/^clickhouse-server:26\\.6\\.1\\.1193-alpine$`,
  );
});

test('the plan is one shard per cancellation build plus rest', () => {
  assert.deepEqual(plan([a, b]).include, [
    { id: 'cancellation-26.6.1.1193-alpine', name: 'cancellation 26.6.1.1193-alpine', kind: KIND_CANCELLATION },
    { id: 'cancellation-26.8.10.6-alpine', name: 'cancellation 26.8.10.6-alpine', kind: KIND_CANCELLATION },
    { id: SHARD_REST, name: SHARD_REST, kind: KIND_REST },
  ]);
});

test('a cancellation shard runs and owns exactly its build and pulls only its image', () => {
  const sel = selection(cancellationShardId(a), pkg);
  assert.equal(sel.run, exactNamePattern(`${CANCELLATION_TEST}/${a}`));
  assert.equal(sel.skip, '');
  assert.deepEqual(sel.expect, [`${CANCELLATION_TEST}/${a}`]);
  assert.deepEqual(shardImages(sel, pinned), [a]);
});

test('rest skips exactly the sharded cancellation builds and owns every top-level test', () => {
  const sel = selection(SHARD_REST, pkg);
  assert.equal(sel.run, '');
  // Each alternative is anchored on the cancellation test, so a condition-cache
  // subtest of the same build is never skipped.
  assert.deepEqual(sel.skip.split('|'), [
    exactNamePattern(`${CANCELLATION_TEST}/${a}`),
    exactNamePattern(`${CANCELLATION_TEST}/${b}`),
  ]);
  assert.deepEqual(sel.expect, tests);
  assert.deepEqual(shardImages(sel, pinned), pinned);
});

test('all runs everything and owns every top-level test', () => {
  const sel = selection(SHARD_ALL, pkg);
  assert.deepEqual(sel, { run: '', skip: '', expect: tests });
  assert.deepEqual(shardImages(sel, pinned), pinned);
});

test('an unknown shard, an unpinned build, or a renamed cancellation test is an error', () => {
  assert.throws(() => selection('cancellation-1.2.3', pkg), /unknown shard/);
  assert.throws(() => shardImages(selection(cancellationShardId(a), pkg), [b]), /not among the pinned images/);
  assert.throws(() => selection(SHARD_REST, { ...pkg, tests: ['TestConditionCache_X'] }), /shard table is stale/);
});

test('the selection flags always ask for -json and carry -run / -skip only when set', () => {
  assert.deepEqual(selectionArgs({ run: '', skip: '' }), ['-json']);
  assert.deepEqual(selectionArgs({ run: 'R', skip: 'S' }), ['-json', '-run=R', '-skip=S']);
});

test('a shard whose owned test never ran fails, even with go test green', () => {
  const events = [{ Action: 'pass', Test: CANCELLATION_TEST }, { Action: 'pass' }];
  assert.deepEqual(verdict(events, [`${CANCELLATION_TEST}/${a}`]), [`${CANCELLATION_TEST}/${a} did not run`]);
});

test('a skipped owned test counts as not run', () => {
  const owned = `${CANCELLATION_TEST}/${a}`;
  assert.deepEqual(verdict([{ Action: 'skip', Test: owned }], [owned]), [`${owned} did not run`]);
});

test('every failed test is reported, owned or not', () => {
  const events = [
    { Action: 'fail', Test: `${CANCELLATION_TEST}/${a}/array_fold` },
    { Action: 'fail', Test: `${CANCELLATION_TEST}/${a}` },
  ];
  assert.deepEqual(verdict(events, [`${CANCELLATION_TEST}/${a}`]), [
    `${CANCELLATION_TEST}/${a} failed`,
    `${CANCELLATION_TEST}/${a}/array_fold failed`,
  ]);
});

test('a shard whose owned tests all passed has no problems', () => {
  const owned = `${CANCELLATION_TEST}/${a}`;
  assert.deepEqual(verdict([{ Action: 'pass', Test: owned }], [owned]), []);
});

test('the aggregate passes only when changes, setup and every shard succeeded', () => {
  const ok = { changes: 'success', docsOnly: 'false', setup: 'success', shards: 'success' };
  assert.deepEqual(aggregate(ok), []);
  for (const bad of ['failure', 'cancelled', 'skipped', '', undefined]) {
    assert.equal(aggregate({ ...ok, shards: bad }).length, 1, `shards=${bad}`);
    assert.equal(aggregate({ ...ok, setup: bad }).length, 1, `setup=${bad}`);
    assert.equal(aggregate({ ...ok, changes: bad }).length, 1, `changes=${bad}`);
  }
});

test('a docs-only change passes with its shards skipped, but never when changes failed', () => {
  assert.deepEqual(aggregate({ changes: 'success', docsOnly: 'true', setup: 'success', shards: 'skipped' }), []);
  assert.equal(aggregate({ changes: 'failure', docsOnly: 'true', setup: 'success', shards: 'skipped' }).length, 2);
});

// The live package: the table parses, the cancellation test exists, and every
// planned shard resolves against the images the recipe pins.
test('only sources -tags=integration compiles contribute tests', () => {
  assert.equal(builtUnderIntegration('package chserver\n'), true);
  assert.equal(builtUnderIntegration('//go:build integration\n\npackage chserver\n'), true);
  assert.equal(builtUnderIntegration('//go:build !integration\n\npackage chserver\n'), false);
  assert.equal(builtUnderIntegration('//go:build integration && chdb\n\npackage chserver\n'), false);
});

// The live package: the table parses, the cancellation test exists, and every
// planned shard resolves against the images the recipe pins.
test('live tree: every planned shard resolves against test/chserver and the pinned images', () => {
  const live = readPackage(process.cwd());
  assert.ok(live.cancellation.length > 0);
  const common = readFileSync('just/common.just', 'utf8');
  const pinned = [...common.matchAll(/^CH_(?:SERVER_SAFETY_IMAGES|HISTOGRAM_REPORT_IMAGE) := "([^"]+)"/gm)].flatMap(
    (m) => m[1].split(/\s+/),
  );
  for (const shard of plan(live.cancellation).include) {
    const sel = selection(shard.id, live);
    assert.ok(sel.expect.length > 0, `${shard.id} owns no test`);
    assert.ok(shardImages(sel, pinned).length > 0, `${shard.id} boots no image`);
  }
});
