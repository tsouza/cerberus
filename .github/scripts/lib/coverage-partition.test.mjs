import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawnSync } from 'node:child_process';
import {
  COVERAGE_SHARDS, parseTestInventory, partitionTests, assertExecuted, testSelector,
  testKey, shardProfile, shardManifest, writePartitionReceipt, mergeCoveragePartitions,
} from './coverage-partition.mjs';
import { main, mainSweepArgv } from '../coverage-chdb.mjs';

const inventory = Array.from({ length: 80 }, (_, index) => ({ package: 'example.test/fixture', name: `TestCase${index}` }));
const profile = 'mode: set\nexample.test/fixture/fixture.go:2.20,2.32 1 1\n';

test('selectors factor prefixes without admitting extra names or losing prefix-named tests', () => {
  const names = ['TestLongSharedPrefix', 'TestLongSharedPrefix_A', 'TestLongSharedPrefix_AB', 'TestLongSharedPrefix_B', 'Example_value', 'FuzzValue', 'TestΩ'];
  const pattern = testSelector(names);
  const regex = new RegExp(pattern);
  for (const name of names) assert.ok(regex.test(name), name);
  for (const name of ['TestLongSharedPrefix_', 'TestLongSharedPrefix_ABC', 'Test', 'Example', 'FuzzValueSuffix']) assert.ok(!regex.test(name), name);
  assert.ok(pattern.length < `^(${names.join('|')})$`.length);
});

test('every discovered top-level test belongs to exactly one partition, including repeated names across packages', () => {
  const tests = [...inventory, { package: 'example.test/other', name: inventory[0].name },
    { package: 'example.test/fixture', name: 'ExampleValue' }, { package: 'example.test/fixture', name: 'FuzzValue' }];
  const seen = new Set();
  for (let index = 1; index <= COVERAGE_SHARDS; index++) {
    const plan = partitionTests(tests, index);
    const selector = new RegExp(plan.pattern);
    for (const entry of tests) assert.equal(selector.test(entry.name), plan.selected.includes(entry));
    for (const entry of plan.selected) {
      assert.ok(!seen.has(testKey(entry)), 'no duplicate ownership');
      seen.add(testKey(entry));
    }
    assert.ok(mainSweepArgv('example.test/fixture', plan).includes(plan.pattern));
  }
  assert.deepEqual(seen, new Set(tests.map(testKey)));
});

test('inventory comes from Go JSON list events and rejects empty or duplicate discovery', () => {
  const listed = inventory.map(({ package: pkg, name }) => JSON.stringify({ Action: 'output', Package: pkg, Output: `${name}\n` })).join('\n');
  assert.deepEqual(new Set(parseTestInventory(listed).map(testKey)), new Set(inventory.map(testKey)));
  assert.throws(() => parseTestInventory(''), /no tests/);
  assert.throws(() => parseTestInventory(`${listed}\n${listed}`), /duplicate/);
  assert.throws(() => parseTestInventory('not JSON'));
  assert.throws(() => partitionTests(inventory, 0), /invalid/);
  assert.throws(() => partitionTests(inventory, COVERAGE_SHARDS + 1), /invalid/);
});

test('execution proof rejects unrun tests and tests outside the selected group', () => {
  const plan = partitionTests(inventory, 1);
  assert.throws(() => assertExecuted(plan, new Set()), /missing=/);
  const passed = new Set(plan.selected.map(testKey));
  assertExecuted(plan, passed);
  passed.add('example.test/fixture/TestUnlisted');
  assert.throws(() => assertExecuted(plan, passed), /unexpected=/);
});

function writeReceipts(dir) {
  for (let index = 1; index <= COVERAGE_SHARDS; index++) {
    const plan = partitionTests(inventory, index);
    writeFileSync(join(dir, shardProfile(index)), profile);
    writePartitionReceipt(dir, plan, new Set(plan.selected.map(testKey)), 'same-revision');
  }
}

test('complete profiles merge; missing, mismatched or stale evidence fails closed', () => {
  const dir = mkdtempSync(join(tmpdir(), 'coverage-partition-'));
  try {
    assert.throws(() => mergeCoveragePartitions(dir), /ENOENT/);
    writeReceipts(dir);
    mergeCoveragePartitions(dir);
    assert.equal(readFileSync(join(dir, 'cover-chdb.out'), 'utf8'), profile);
    assert.throws(() => mergeCoveragePartitions(dir, COVERAGE_SHARDS, 'wrong-checkout'), /revisions differ/);
    writeFileSync(join(dir, shardProfile(1)), `${profile}example.test/fixture/extra.go:1.1,1.2 1 0\n`);
    assert.throws(() => mergeCoveragePartitions(dir), /digest/);
    writeReceipts(dir);
    const receipt = JSON.parse(readFileSync(join(dir, shardManifest(2)), 'utf8'));
    receipt.revision = 'another-revision';
    writeFileSync(join(dir, shardManifest(2)), JSON.stringify(receipt));
    assert.throws(() => mergeCoveragePartitions(dir), /revisions differ/);
    writeReceipts(dir);
    receipt.revision = 'same-revision';
    receipt.passed = [];
    writeFileSync(join(dir, shardManifest(2)), JSON.stringify(receipt));
    assert.throws(() => mergeCoveragePartitions(dir), /missing=/);
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

test('workflow launches every shard, retains both proof and profile, and requires complete merge', () => {
  const workflow = readFileSync(new URL('../../workflows/coverage.yml', import.meta.url), 'utf8');
  const job = workflow.slice(workflow.indexOf('\n  coverage-chdb:'), workflow.indexOf('\n  coverage-chdb-ratchet:'));
  assert.match(job, new RegExp(`shard: \\[${Array.from({ length: COVERAGE_SHARDS }, (_, i) => i + 1).join(', ')}\\]`));
  assert.match(job, /fail-fast: false/);
  assert.match(job, /COVERAGE_SHARD_INDEX: \$\{\{ matrix.shard \}\}/);
  assert.match(job, /cover-chdb-part-\$\{\{ matrix.shard \}\}\.out/);
  assert.match(job, /cover-chdb-part-\$\{\{ matrix.shard \}\}\.json/);
  assert.match(workflow, /COVERAGE_REQUIRE_PARTITIONS: '1'/);
});

test('real Go execution covers every partition, including examples and fuzz seeds, then merges profiles', async () => {
  const dir = mkdtempSync(join(tmpdir(), 'coverage-partition-go-'));
  const run = (command, args) => {
    const result = spawnSync(command, args, { cwd: dir, encoding: 'utf8' });
    assert.equal(result.status, 0, result.stderr || result.stdout);
  };
  try {
    writeFileSync(join(dir, 'go.mod'), 'module example.test/fixture\n\ngo 1.26\n');
    writeFileSync(join(dir, 'fixture.go'), 'package fixture\nfunc value() int { return 1 }\n');
    const tests = inventory.map(({ name }) => `func ${name}(t *testing.T) { if value() != 1 { t.Fatal("value") } }`);
    writeFileSync(join(dir, 'fixture_test.go'), 'package fixture\nimport ("testing"; "fmt")\n' + tests.join('\n') +
      '\nfunc Example_value() { fmt.Println(value())\n// Output: 1\n}\n' +
      '\nfunc FuzzValue(f *testing.F) { f.Add(1); f.Fuzz(func(t *testing.T, n int) { if value() != 1 { t.Fatal(n) } }) }\n');
    const lib = join(dir, 'libchdb.so');
    writeFileSync(lib, 'fixture does not link chdb');
    run('git', ['init', '-q']);
    run('git', ['-c', 'user.name=Test', '-c', 'user.email=test@example.test', 'commit', '--allow-empty', '-qm', 'test: fixture']);
    for (let index = 1; index <= COVERAGE_SHARDS; index++) {
      const status = await main({ cwd: dir, stdout: { write() {} }, env: {
        ...process.env, CHDB_INSTALL_PATH: lib, CERBERUS_RAPID_SEED: '1', SKIP_RATCHET_FANOUT: '1',
        COVERAGE_SHARD_INDEX: String(index), COVERAGE_REQUIRE_LANES: 'default+chdb',
      } });
      assert.equal(status, 0);
    }
    mergeCoveragePartitions(dir);
    assert.match(readFileSync(join(dir, 'cover-chdb.out'), 'utf8'), /fixture.go:.* 1 1/);
  } finally { rmSync(dir, { recursive: true, force: true }); }
});
