// go-test-fanout.test.mjs — node:test guard for go-test-fanout.mjs: the argv
// it is handed is run faithfully, and a fanned-out package's partitions cover
// its tests exactly once.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

import { FANOUT, parseInvocation, planCommands, tagArgs } from './go-test-fanout.mjs';

const PROM = 'github.com/tsouza/cerberus/internal/api/prom';
const OTHER = 'github.com/tsouza/cerberus/internal/chsql';
const FLAGS = ['-timeout', '10m', '-tags', 'chdb', '-count=1'];

function inventoryOf(pkg, names) {
  return names.map((name) => ({ package: pkg, name }));
}

const NAMES = Array.from({ length: 200 }, (_, i) => `TestCase${i}_ChDB`).concat([
  'TestQuery', 'TestQuery_Vector', 'TestQueryRange', 'ExampleHandler', 'FuzzParse',
]);

test('parseInvocation splits flags from packages and keeps both verbatim', () => {
  const inv = parseInvocation(['go', 'test', ...FLAGS, './internal/api/...', './internal/chsql/...']);
  assert.equal(inv.go, 'go');
  assert.deepEqual(inv.flags, FLAGS);
  assert.deepEqual(inv.packages, ['./internal/api/...', './internal/chsql/...']);
});

test('parseInvocation refuses what the script cannot run faithfully', () => {
  assert.throws(() => parseInvocation(['go', 'vet', './...']), /go test/);
  assert.throws(() => parseInvocation(['go', 'test', '-count=1']), /names no/);
  assert.throws(() => parseInvocation(['go', 'test', './a/...', '-v']), /follows the package list/);
  for (const owned of ['-run', '-skip', '-list', '-json', '--run=X', '-run=X']) {
    assert.throws(() => parseInvocation(['go', 'test', owned, 'X', './a/...']), /set by this script/);
  }
});

test('tagArgs forwards -tags in both spellings and nothing else', () => {
  assert.deepEqual(tagArgs(FLAGS), ['-tags', 'chdb']);
  assert.deepEqual(tagArgs(['-count=1', '-tags=chdb,x']), ['-tags=chdb,x']);
  assert.deepEqual(tagArgs(['-count=1']), []);
});

test('a fanned-out package runs every listed test in exactly one process', () => {
  const inventories = new Map([[PROM, inventoryOf(PROM, NAMES)]]);
  const commands = planCommands({ go: 'go', flags: FLAGS, importPaths: [OTHER, PROM], inventories });
  const legs = commands.filter((c) => c.argv.at(-1) === PROM);
  assert.equal(legs.length, FANOUT[PROM]);
  const selectors = legs.map((c) => new RegExp(c.argv[c.argv.indexOf('-run') + 1]));
  for (const name of NAMES) {
    const hits = selectors.filter((re) => re.test(name)).length;
    assert.equal(hits, 1, `${name} matched ${hits} partitions`);
  }
  // Anchored: a name that extends a listed name is not selected by accident.
  for (const re of selectors) assert.equal(re.test('TestQuery_VectorExtra'), false);
  for (const leg of legs) assert.ok(NAMES.some((n) => new RegExp(leg.argv[leg.argv.indexOf('-run') + 1]).test(n)));
});

test('every process keeps the invocation flags, including its -timeout', () => {
  const inventories = new Map([[PROM, inventoryOf(PROM, NAMES)]]);
  const commands = planCommands({ go: 'go', flags: FLAGS, importPaths: [OTHER, PROM], inventories });
  assert.equal(commands.length, 1 + FANOUT[PROM]);
  for (const c of commands) assert.deepEqual(c.argv.slice(0, 2 + FLAGS.length), ['go', 'test', ...FLAGS]);
  const whole = commands.find((c) => c.argv.at(-1) === OTHER);
  assert.ok(!whole.argv.includes(PROM), 'the fanned-out package also ran whole');
  assert.ok(!whole.argv.includes('-run'));
});

test('a plan without a fanned-out package is the plain invocation', () => {
  const commands = planCommands({ go: 'go', flags: FLAGS, importPaths: [OTHER], inventories: new Map() });
  assert.deepEqual(commands.map((c) => c.argv), [['go', 'test', ...FLAGS, OTHER]]);
});

test('a fanned-out package with no inventory is an error, not a silent skip', () => {
  assert.throws(
    () => planCommands({ go: 'go', flags: FLAGS, importPaths: [PROM], inventories: new Map() }),
    /no test inventory/,
  );
});

test('just test-chdb runs its go test through the fan-out, over every FANOUT package', () => {
  const recipes = readFileSync(new URL('../../just/test.just', import.meta.url), 'utf8');
  const body = recipes.split(/\n(?=\S)/).find((block) => /^test-chdb:/m.test(block));
  assert.ok(body, 'just/test.just has no test-chdb recipe');
  assert.match(body, /node \.github\/scripts\/go-test-fanout\.mjs go test /);
  for (const pkg of Object.keys(FANOUT)) {
    const rel = `./${pkg.replace(/^github\.com\/tsouza\/cerberus\//, '')}`;
    const parent = rel.split('/').slice(0, -1).join('/');
    assert.ok(body.includes(`${rel} `) || body.includes(`${rel}/...`) || body.includes(`${parent}/...`),
      `test-chdb does not select ${rel}`);
  }
});
