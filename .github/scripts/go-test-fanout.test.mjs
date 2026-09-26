// go-test-fanout.test.mjs — node:test guard for go-test-fanout.mjs: the argv
// it is handed is run faithfully, and a fanned-out package's partitions cover
// its tests exactly once.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { chmodSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { FANOUT, judge, parseInvocation, planCommands, readTestEvents, tagArgs } from './go-test-fanout.mjs';

const SCRIPT = fileURLToPath(new URL('./go-test-fanout.mjs', import.meta.url));

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
  const refused = [
    'run', 'skip', 'list', 'json', 'bench', 'fuzz',
    'o', 'outputdir', 'coverprofile', 'cpuprofile', 'memprofile', 'blockprofile', 'mutexprofile', 'trace',
  ];
  for (const name of refused) {
    for (const spelling of [`-${name}`, `--${name}`, `-test.${name}`, `--test.${name}`]) {
      assert.throws(() => parseInvocation(['go', 'test', spelling, 'X', './a/...']), /cannot be passed/, spelling);
      assert.throws(() => parseInvocation(['go', 'test', `${spelling}=X`, './a/...']), /cannot be passed/, `${spelling}=X`);
    }
  }
  // A refused flag's NAME as another flag's value is not the flag.
  assert.doesNotThrow(() => parseInvocation(['go', 'test', '-tags', 'run', './a/...']));
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
  for (const leg of legs) assert.ok(leg.argv.includes('-json'), 'a partition runs without -json, so its execution is unverified');
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

test('the test-chdb* recipes run their go test through the fan-out, over every FANOUT package between them', () => {
  const recipes = readFileSync(new URL('../../just/test.just', import.meta.url), 'utf8');
  const blocks = recipes.split(/\n(?=\S)/).filter((block) => /^test-chdb[\w-]*:/m.test(block));
  assert.ok(blocks.length > 0, 'just/test.just has no test-chdb* recipe');
  for (const body of blocks) assert.match(body, /node \.github\/scripts\/go-test-fanout\.mjs go test /);
  const combined = blocks.join('\n');
  for (const pkg of Object.keys(FANOUT)) {
    const rel = `./${pkg.replace(/^github\.com\/tsouza\/cerberus\//, '')}`;
    const parent = rel.split('/').slice(0, -1).join('/');
    assert.ok(combined.includes(`${rel} `) || combined.includes(`${rel}/...`) || combined.includes(`${parent}/...`),
      `no test-chdb* recipe selects ${rel}`);
  }
});

function event(fields) {
  return JSON.stringify(fields);
}

test('a partition passes only when exactly its selected tests passed', () => {
  const inventories = new Map([[PROM, inventoryOf(PROM, NAMES)]]);
  const [leg] = planCommands({ go: 'go', flags: FLAGS, importPaths: [PROM], inventories });
  const selected = leg.plan.selected.map((t) => t.name);
  const passLines = selected.map((name) => event({ Action: 'pass', Package: PROM, Test: name }));
  const ok = [event({ Action: 'output', Package: PROM, Output: 'ok  \tprom\t1.0s\n' }), ...passLines].join('\n');
  assert.equal(judge(leg, 0, ok).ok, true);
  assert.match(judge(leg, 0, ok).text, /ok {2}\tprom/);

  // A -run that matched nothing exits 0 with "no tests to run".
  const none = event({ Action: 'output', Package: PROM, Output: 'testing: warning: no tests to run\n' });
  const empty = judge(leg, 0, none);
  assert.equal(empty.ok, false);
  assert.match(empty.text, /missing=/);

  // One selected test missing, or one test outside the partition, fails too.
  assert.equal(judge(leg, 0, passLines.slice(1).join('\n')).ok, false);
  const outsider = NAMES.find((n) => !selected.includes(n));
  assert.equal(judge(leg, 0, `${ok}\n${event({ Action: 'pass', Package: PROM, Test: outsider })}`).ok, false);

  // A failing go test fails the partition even when the passes line up.
  assert.equal(judge(leg, 1, ok).ok, false);
});

test('readTestEvents keeps subtests out of the pass set and non-JSON lines in the text', () => {
  const { text, passed } = readTestEvents([
    event({ Action: 'pass', Package: PROM, Test: 'TestA' }),
    event({ Action: 'pass', Package: PROM, Test: 'TestA/sub' }),
    '# github.com/x/y [build failed]',
  ].join('\n'));
  assert.deepEqual([...passed], [`${PROM}/TestA`]);
  assert.match(text, /build failed/);
});

test('the whole-package process is judged by its exit code alone', () => {
  const whole = { name: 'rest', argv: ['go', 'test', OTHER] };
  assert.equal(judge(whole, 0, 'ok').ok, true);
  assert.equal(judge(whole, 1, 'FAIL').ok, false);
});

// main(), end to end, against a stand-in `go` that lists two tests and whose
// partition runs pass nothing: every partition must fail, and so must the
// script.
test('main exits 1 when a partition runs none of its tests', () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'go-test-fanout-'));
  try {
    const fakeGo = path.join(dir, 'go');
    writeFileSync(fakeGo, `#!/usr/bin/env node
const args = process.argv.slice(2);
if (args[0] === 'list') { console.log('${PROM}'); process.exit(0); }
if (args.includes('-list')) {
  for (let i = 0; i < 40; i++) console.log(JSON.stringify({ Action: 'output', Package: '${PROM}', Output: 'TestFake' + i + '\\n' }));
  process.exit(0);
}
console.log(JSON.stringify({ Action: 'output', Package: '${PROM}', Output: 'testing: warning: no tests to run\\n' }));
process.exit(0);
`);
    chmodSync(fakeGo, 0o755);
    const res = spawnSync(process.execPath, [SCRIPT, fakeGo, 'test', '-count=1', './internal/api/prom'], { encoding: 'utf8' });
    assert.equal(res.status, 1, res.stdout + res.stderr);
    assert.match(res.stdout, /missing=/);
    assert.match(res.stdout, new RegExp(`${FANOUT[PROM]} of ${FANOUT[PROM]} process\\(es\\) failed`));
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('main exits 1 on an argv it cannot run faithfully', () => {
  const res = spawnSync(process.execPath, [SCRIPT, 'go', 'test', '-run', 'X', './a/...'], { encoding: 'utf8' });
  assert.equal(res.status, 1);
  assert.match(res.stdout + res.stderr, /cannot be passed/);
});
