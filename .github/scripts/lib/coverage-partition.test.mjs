import { test } from 'node:test';
import assert from 'node:assert/strict';
import { chmodSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import {
  COVERAGE_SHARDS, parseTestInventory, partitionTests, assertExecuted, testSelector,
  testKey, shardProfile, shardManifest, writePartitionReceipt, mergeCoveragePartitions, selectorTests,
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
    assert.equal(plan.selectors.length, 1);
    const selector = new RegExp(plan.selectors[0].pattern);
    for (const entry of tests) assert.equal(selector.test(entry.name), plan.selected.includes(entry));
    for (const entry of plan.selected) {
      assert.ok(!seen.has(testKey(entry)), 'no duplicate ownership');
      seen.add(testKey(entry));
    }
    assert.ok(mainSweepArgv('example.test/fixture', plan).includes(plan.selectors[0].pattern));
  }
  assert.deepEqual(seen, new Set(tests.map(testKey)));
});

test('a shard whose selector is past the argument budget runs as several selectors that together select it exactly once', () => {
  const budget = 64 * 1024;
  // 12000 names of 69 bytes with no shared prefix past `Test`: each shard's
  // single selector would be about 210 KiB.
  const big = Array.from({ length: 12000 }, (_, i) => ({
    package: `example.test/p${i % 7}`, name: `Test${createHash('sha256').update(String(i)).digest('hex')}${i % 2 ? 'A' : ''}`,
  }));
  big.push({ package: 'example.test/other', name: big[0].name });
  const covered = new Set();
  for (let index = 1; index <= COVERAGE_SHARDS; index++) {
    const plan = partitionTests(big, index);
    assert.ok(plan.selectors.length > 1, `shard ${index} needs more than one selector`);
    const owners = new Map();
    plan.selectors.forEach((selector, part) => {
      assert.ok(Buffer.byteLength(selector.pattern) <= budget, `shard ${index} selector ${part + 1} fits the budget`);
      const regex = new RegExp(selector.pattern);
      for (const entry of big) {
        if (!regex.test(entry.name)) continue;
        assert.ok(plan.selected.includes(entry), `${entry.name} is in shard ${index}`);
        assert.ok(!owners.has(testKey(entry)), `${entry.name} is selected once`);
        owners.set(testKey(entry), part);
      }
      assert.deepEqual(new Set(selectorTests(plan, selector).map(testKey)),
        new Set([...owners].filter(([, owner]) => owner === part).map(([key]) => key)));
      assert.ok(mainSweepArgv('example.test/fixture', plan, part).includes(selector.pattern));
    });
    assert.deepEqual(new Set(owners.keys()), new Set(plan.selected.map(testKey)));
    for (const key of owners.keys()) covered.add(key);
    const passed = plan.selected.map(testKey);
    assertExecuted(plan, passed);
    assert.throws(() => assertExecuted(plan, [...passed, passed[0]]), /repeated=/);
  }
  assert.deepEqual(covered, new Set(big.map(testKey)));
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

test('a shard past the argument budget runs one go test per selector, folds their profiles and proves each test passed once', async () => {
  const dir = mkdtempSync(join(tmpdir(), 'coverage-partition-runs-'));
  const run = (command, args) => {
    const result = spawnSync(command, args, { cwd: dir, encoding: 'utf8' });
    assert.equal(result.status, 0, result.stderr || result.stdout);
  };
  try {
    const packages = ['example.test/a', 'example.test/b', 'example.test/c'];
    const tests = Array.from({ length: 12000 }, (_, i) => ({
      package: packages[i % packages.length], name: `Test${createHash('sha256').update(String(i)).digest('hex')}`,
    }));
    writeFileSync(join(dir, 'tests.json'), JSON.stringify(tests));
    // The fake go lists the tests, and for each `-run` passes exactly the
    // tests its selector matches in the packages it targets, writing a
    // profile whose block is that run's own.
    const go = join(dir, 'fake-go.mjs');
    writeFileSync(go, `#!/usr/bin/env node
import { appendFileSync, readFileSync, writeFileSync } from 'node:fs';
const args = process.argv.slice(2);
const tests = JSON.parse(readFileSync(${JSON.stringify(join(dir, 'tests.json'))}, 'utf8'));
const event = (fields) => process.stdout.write(JSON.stringify(fields) + '\\n');
if (args[0] === 'list') { console.log(${JSON.stringify(packages.join('\n'))}); process.exit(0); }
if (args.includes('-list')) { for (const t of tests) event({ Action: 'output', Package: t.package, Output: t.name + '\\n' }); process.exit(0); }
const pattern = new RegExp(args[args.indexOf('-run') + 1]);
const targets = args.slice(args.indexOf('-run') + 2);
const profile = args[args.indexOf('-coverprofile') + 1];
appendFileSync(${JSON.stringify(join(dir, 'runs.log'))}, JSON.stringify(targets) + '\\n');
const run = readFileSync(${JSON.stringify(join(dir, 'runs.log'))}, 'utf8').split('\\n').filter(Boolean).length;
writeFileSync(profile, 'mode: set\\nexample.test/a/run.go:' + run + '.1,' + run + '.2 1 1\\n');
for (const t of tests) {
  if (pattern.test(t.name) && (targets.includes('./...') || targets.includes(t.package))) event({ Action: 'pass', Package: t.package, Test: t.name, Elapsed: 0 });
}
`);
    chmodSync(go, 0o755);
    const lib = join(dir, 'libchdb.so');
    writeFileSync(lib, 'fixture does not link chdb');
    run('git', ['init', '-q']);
    run('git', ['-c', 'user.name=Test', '-c', 'user.email=test@example.test', 'commit', '--allow-empty', '-qm', 'test: fixture']);
    const index = 4;
    const status = await main({ cwd: dir, go, stdout: { write() {} }, env: {
      ...process.env, CHDB_INSTALL_PATH: lib, CERBERUS_RAPID_SEED: '1', SKIP_RATCHET_FANOUT: '1',
      COVERAGE_SHARD_INDEX: String(index), COVERAGE_REQUIRE_LANES: 'default+chdb',
    } });
    assert.equal(status, 0);
    const runs = readFileSync(join(dir, 'runs.log'), 'utf8').split('\n').filter(Boolean).map((line) => JSON.parse(line));
    const receipt = JSON.parse(readFileSync(join(dir, shardManifest(index)), 'utf8'));
    assert.ok(runs.length > 1, 'the shard runs more than one go test');
    assert.equal(runs.length, receipt.selectors.length);
    // The first run runs every test binary, as the unsplit shard did; a
    // later one only the binaries that own its names.
    assert.deepEqual(runs[0], ['./...']);
    runs.slice(1).forEach((targets, i) => {
      const owners = [...new Set(receipt.selected.filter(({ name }) => receipt.selectors[i + 1].names.includes(name)).map((t) => t.package))].sort();
      assert.deepEqual(targets, owners);
    });
    assert.equal(receipt.passed.length, receipt.selected.length);
    const folded = readFileSync(join(dir, shardProfile(index)), 'utf8');
    for (let r = 1; r <= runs.length; r++) assert.match(folded, new RegExp(`run\\.go:${r}\\.1,${r}\\.2 1 1`));
  } finally { rmSync(dir, { recursive: true, force: true }); }
});
