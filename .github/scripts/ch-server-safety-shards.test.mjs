// ch-server-safety-shards.test.mjs — node:test guard for the ch-server-safety
// shard plan: the shards cover every test of test/chserver, each cancellation
// shard selects exactly its build, each family shard exactly its family, and
// neither a shard that runs nothing nor a failed or skipped shard job can pass.

import { test } from 'node:test';
import process from 'node:process';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync } from 'node:fs';
import { join } from 'node:path';

import {
  CANCELLATION_TEST,
  FAMILY_SHARDS,
  KIND_CANCELLATION,
  KIND_FAMILY,
  KIND_REST,
  PACKAGE_DIR,
  SHARD_ALL,
  SHARD_REST,
  aggregate,
  builtUnderIntegration,
  cancellationImages,
  cancellationShardId,
  exactNamePattern,
  familyImages,
  familyPattern,
  familyTests,
  namedImages,
  plan,
  readPackage,
  restImages,
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
const [family, ...otherFamilies] = FAMILY_SHARDS;
const familyA = `${family.prefix}_A`;
const familyB = `${family.prefix}_B`;
// One test of every other family, so the fixture package leaves no family of
// FAMILY_SHARDS without a test.
const otherFamilyTests = otherFamilies.map((f) => `${f.prefix}_A`);
const tests = [familyA, familyB, ...otherFamilyTests, 'TestConditionCache_X', CANCELLATION_TEST];
const floor = 'clickhouse/clickhouse-server:25.3.14.14-alpine';
const pinned = [floor, a, b];
const pkg = {
  cancellation: [a, b],
  tests,
  rest: [a, floor],
  families: Object.fromEntries(FAMILY_SHARDS.map((f) => [f.id, [b]])),
};

// A condition-cache source naming the floor and a build the cancellation table
// also names, plus a commented-out image that names nothing.
const conditionCacheFixture = `
var conditionCacheBuilds = []struct{ image string }{
	{"${floor}"},
	{"${a}"},
	// {"clickhouse/clickhouse-server:24.8.14.39-alpine"},
}
`;

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

test('the plan is one shard per cancellation build, one per family, plus rest', () => {
  assert.deepEqual(plan([a, b]).include, [
    { id: 'cancellation-26.6.1.1193-alpine', name: 'cancellation 26.6.1.1193-alpine', kind: KIND_CANCELLATION },
    { id: 'cancellation-26.8.10.6-alpine', name: 'cancellation 26.8.10.6-alpine', kind: KIND_CANCELLATION },
    ...FAMILY_SHARDS.map((f) => ({ id: f.id, name: f.id, kind: KIND_FAMILY })),
    { id: SHARD_REST, name: SHARD_REST, kind: KIND_REST },
  ]);
});

test('a family shard runs and owns exactly the tests its prefix names and pulls its sources\' images', () => {
  const sel = selection(family.id, pkg);
  assert.equal(sel.run, familyPattern(family));
  assert.equal(sel.skip, '');
  assert.deepEqual(sel.expect, [familyA, familyB]);
  assert.deepEqual(shardImages(sel, pinned), [b]);
  // The pattern anchors the prefix at the start of the top-level name only.
  const re = new RegExp(familyPattern(family));
  assert.ok(re.test(familyA));
  assert.ok(!re.test(`TestX${family.prefix}`));
  assert.deepEqual(familyTests(family, ['TestX', familyB, familyA]), [familyB, familyA]);
  assert.deepEqual(familyTests(family, [`TestX${family.prefix}`]), []);
});

// A family's membership is computed twice: familyTests decides which tests
// the shard owns, and familyPattern is what `go test -run` / `-skip` match.
// They must agree on every name, or a test is owned by one shard and run by
// another.
test("a family's -run pattern selects exactly the tests familyTests assigns it", () => {
  const names = [...tests, `TestX${family.prefix}`, ...readPackage(process.cwd()).tests];
  for (const f of [family, ...FAMILY_SHARDS]) {
    const re = new RegExp(familyPattern(f));
    for (const name of names) {
      assert.equal(re.test(name), familyTests(f, [name]).length > 0, `${f.id} disagrees on ${name}`);
    }
  }
});

test("a family's images are the ones its defining sources name, and no other source's", () => {
  const familySource = `func ${familyA}(t *testing.T) {\n\t_ = "${b}"\n}\n`;
  assert.deepEqual(familyImages([familySource, conditionCacheFixture])[family.id], [b]);
});

test("a family's images include an image constant its source uses and another source defines", () => {
  const familySource = `func ${familyA}(t *testing.T) {\n\tstart(t, sharedImage)\n}\n`;
  const definingSource = `const sharedImage = "${b}"\n\nconst unusedImage = "${a}"\n`;
  assert.deepEqual(familyImages([familySource, definingSource])[family.id], [b]);
});

test("a family's image constant named only in a comment, or inside a longer name, is not one it boots", () => {
  const familySource =
    `func ${familyA}(t *testing.T) {\n\t// was sharedImage\n\tstart(t, sharedImageOld)\n\t_ = "${a}"\n}\n`;
  const definingSource = `const sharedImage = "${b}"\n`;
  assert.deepEqual(familyImages([familySource, definingSource])[family.id], [a]);
});

test('only a single-line const declares an image constant, never a var or an assignment', () => {
  const familySource = `func ${familyA}(t *testing.T) {\n\tstart(t, varImage, assigned)\n\t_ = "${a}"\n}\n`;
  const definingSource = `var varImage = "${b}"\n\nfunc f() {\n\tassigned = "${b}"\n}\n`;
  assert.deepEqual(familyImages([familySource, definingSource])[family.id], [a]);
});

test('a family whose tests name no image is an error, never a shard that pulls nothing', () => {
  const familySource = `func ${familyA}(t *testing.T) {\n\tstart(t)\n}\n`;
  assert.throws(() => familyImages([familySource, conditionCacheFixture]), /names no image/);
});

test('a family no test belongs to is an error, never an empty shard', () => {
  assert.throws(
    () => selection(SHARD_REST, { ...pkg, tests: ['TestConditionCache_X', CANCELLATION_TEST] }),
    /FAMILY_SHARDS is stale/,
  );
});

test('a cancellation shard runs and owns exactly its build and pulls only its image', () => {
  const sel = selection(cancellationShardId(a), pkg);
  assert.equal(sel.run, exactNamePattern(`${CANCELLATION_TEST}/${a}`));
  assert.equal(sel.skip, '');
  assert.deepEqual(sel.expect, [`${CANCELLATION_TEST}/${a}`]);
  assert.deepEqual(shardImages(sel, pinned), [a]);
});

test('an image is named only by code, once, in order of first appearance', () => {
  assert.deepEqual(namedImages(conditionCacheFixture), [floor, a]);
  assert.deepEqual(namedImages(`x := "${b}"\ny := "${b}"\n`), [b]);
});

test('rest boots the images named outside the cancellation table, and only those', () => {
  // b appears only in the cancellation table, so rest never boots it; a is
  // named by the condition-cache source too, so rest keeps it.
  assert.deepEqual(restImages([fixture, conditionCacheFixture], fixture).sort(), [a, floor].sort());
  assert.throws(() => restImages([fixture], fixture), /rest would boot none/);
});

test('rest skips exactly the sharded builds and families and owns every other top-level test', () => {
  const sel = selection(SHARD_REST, pkg);
  assert.equal(sel.run, '');
  // Each cancellation alternative is anchored on the cancellation test, so a
  // condition-cache subtest of the same build is never skipped.
  assert.deepEqual(sel.skip.split('|'), [
    exactNamePattern(`${CANCELLATION_TEST}/${a}`),
    exactNamePattern(`${CANCELLATION_TEST}/${b}`),
    ...FAMILY_SHARDS.map(familyPattern),
  ]);
  assert.deepEqual(sel.expect, ['TestConditionCache_X', CANCELLATION_TEST]);
  assert.deepEqual(shardImages(sel, pinned), [floor, a]);
});

test('all runs everything and owns every top-level test', () => {
  const sel = selection(SHARD_ALL, pkg);
  assert.deepEqual(sel, { run: '', skip: '', expect: tests });
  assert.deepEqual(shardImages(sel, pinned), pinned);
});

test('an unknown shard, an unpinned build, or a renamed cancellation test is an error', () => {
  assert.throws(() => selection('cancellation-1.2.3', pkg), /unknown shard/);
  assert.throws(() => shardImages(selection(cancellationShardId(a), pkg), [b]), /not among the pinned images/);
  assert.throws(() => shardImages(selection(SHARD_REST, pkg), [a, b]), /not among the pinned images/);
  assert.throws(
    () => selection(SHARD_REST, { ...pkg, tests: [familyA, ...otherFamilyTests, 'TestConditionCache_X'] }),
    /shard table is stale/,
  );
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
  // Every top-level test is owned by exactly one shard other than the
  // cancellation builds' (whose parent test rest owns).
  const owners = plan(live.cancellation)
    .include.filter((shard) => shard.kind !== KIND_CANCELLATION)
    .flatMap((shard) => selection(shard.id, live).expect);
  assert.deepEqual([...owners].sort(), [...live.tests].sort());
  // Every pinned image is booted by some shard, and rest boots fewer than all.
  const booted = new Set(
    plan(live.cancellation).include.flatMap((shard) => shardImages(selection(shard.id, live), pinned)),
  );
  assert.deepEqual([...booted].sort(), [...pinned].sort());
  assert.ok(shardImages(selection(SHARD_REST, live), pinned).length < pinned.length, 'rest boots every image');
  // Every top-level test of a classic_bucket_* source runs in the
  // classic-histogram shard, never in rest.
  const classicTests = topLevelTests(
    readdirSync(PACKAGE_DIR)
      .filter((file) => file.startsWith('classic_bucket_') && file.endsWith('_test.go'))
      .map((file) => readFileSync(join(PACKAGE_DIR, file), 'utf8'))
      .filter(builtUnderIntegration),
  );
  assert.ok(classicTests.length > 0, 'test/chserver has no classic_bucket_* test');
  const classicShard = selection('classic-histogram', live).expect;
  assert.deepEqual(
    classicTests.filter((name) => !classicShard.includes(name)),
    [],
    'a classic_bucket_* test runs outside the classic-histogram shard',
  );
});
