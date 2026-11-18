// Complete top-level Go test partitioning. Every test/example/fuzz seed
// discovered by go test -json -list belongs to exactly one shard. No
// source-file parser, exclusion list or timing file. Two consumers: the
// instrumented coverage lane (coverage-chdb.mjs, which also uses the profile
// receipt and merge helpers below) and go-test-fanout.mjs.
import { createHash } from 'node:crypto';
import { readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { writeFoldedProfile } from './coverage-fold.mjs';

export const COVERAGE_SHARDS = 4;
const maxRunPatternBytes = 64 * 1024;
const testName = /^(?:Test|Example|Fuzz)[\p{L}\p{N}_]*$/u;
const digest = (value) => createHash('sha256').update(value).digest('hex');
export const testKey = ({ package: pkg, name }) => `${pkg}/${name}`;
export const shardProfile = (index) => `cover-chdb-part-${index}.out`;
export const shardManifest = (index) => `cover-chdb-part-${index}.json`;
// One `go test` run's profile inside shard index, folded into shardProfile.
export const selectorProfile = (index, part) => `cover-chdb-run-${index}-${part + 1}.out`;

// Factor shared name prefixes instead of placing thousands of full names into
// one OS argument. All characters are Go identifier characters, not regex tokens.
export function testSelector(names) {
  const root = new Map();
  for (const name of names) {
    if (!testName.test(name)) throw new Error(`invalid test name ${name}`);
    let node = root;
    for (const char of name) {
      if (!node.has(char)) node.set(char, new Map());
      node = node.get(char);
    }
    node.set('', null);
  }
  const render = (node) => {
    const branches = [...node.entries()].sort(([a], [b]) => a.localeCompare(b, 'en'))
      .map(([char, child]) => char + (child ? render(child) : ''));
    return branches.length === 1 ? branches[0] : `(?:${branches.join('|')})`;
  };
  return `^${render(root)}$`;
}

// Splits sorted names into runs of consecutive names whose selector fits the
// argument budget. Adding a name never shortens a selector, so the longest
// run that fits is found by bisection.
export function selectorRuns(names, budget = maxRunPatternBytes) {
  const fits = (start, end) => Buffer.byteLength(testSelector(names.slice(start, end))) <= budget;
  const runs = [];
  for (let start = 0; start < names.length;) {
    if (!fits(start, start + 1)) throw new Error(`test name ${names[start]} alone exceeds the argument budget`);
    let lo = start + 1;
    let hi = names.length;
    while (lo < hi) {
      const mid = Math.ceil((lo + hi) / 2);
      if (fits(start, mid)) lo = mid; else hi = mid - 1;
    }
    const run = names.slice(start, lo);
    runs.push({ pattern: testSelector(run), names: run });
    start = lo;
  }
  return runs;
}

export function parseTestInventory(jsonLines) {
  const tests = new Map();
  for (const line of jsonLines.split('\n').filter(Boolean)) {
    const event = JSON.parse(line);
    if (event.Action !== 'output' || !event.Package) continue;
    const name = event.Output.trim();
    if (!testName.test(name)) continue;
    const test = { package: event.Package, name };
    if (tests.has(testKey(test))) throw new Error(`duplicate listed test: ${testKey(test)}`);
    tests.set(testKey(test), test);
  }
  if (!tests.size) throw new Error('test inventory contains no tests');
  return [...tests.values()].sort((a, b) => testKey(a).localeCompare(testKey(b), 'en'));
}

export function partitionTests(inventory, index, count = COVERAGE_SHARDS) {
  if (!Number.isInteger(index) || index < 1 || index > count) throw new Error(`invalid test shard ${index}/${count}`);
  const keys = new Set();
  for (const entry of inventory) {
    if (typeof entry.package !== 'string' || !entry.package || !testName.test(entry.name) || keys.has(testKey(entry))) {
      throw new Error('invalid or duplicate test inventory entry');
    }
    keys.add(testKey(entry));
  }
  const selected = inventory.filter(({ name }) => {
    const hash = createHash('sha256').update(name).digest().readUInt32BE();
    return hash % count + 1 === index;
  });
  if (!selected.length) throw new Error(`test shard ${index}/${count} is empty`);
  const names = [...new Set(selected.map(({ name }) => name))].sort();
  // Each selector is one `go test -run` argument; a shard whose names do not
  // fit one runs them as several.
  const selectors = selectorRuns(names);
  return { index, count, inventory, selected, selectors };
}

// The part of plan's selected tests that one selector of it runs.
export function selectorTests(plan, selector) {
  const names = new Set(selector.names);
  return plan.selected.filter(({ name }) => names.has(name));
}

// passed lists every top-level pass the runs reported, repeats included: a
// selected test must pass exactly once, and nothing else may run.
export function assertExecuted(plan, passed) {
  const expected = new Set(plan.selected.map(testKey));
  const seen = new Set();
  const repeated = [];
  for (const key of passed) {
    if (seen.has(key)) repeated.push(key);
    seen.add(key);
  }
  const missing = [...expected].filter((key) => !seen.has(key));
  const unexpected = [...seen].filter((key) => !expected.has(key));
  if (missing.length || unexpected.length || repeated.length) {
    throw new Error(`test execution mismatch: missing=${missing.join(',')} unexpected=${unexpected.join(',')} repeated=${repeated.join(',')}`);
  }
}

export function writePartitionReceipt(cwd, plan, passed, revision) {
  assertExecuted(plan, passed);
  const profile = readFileSync(join(cwd, shardProfile(plan.index)));
  if (!profile.toString().startsWith('mode: set\n') || profile.toString().trim() === 'mode: set') {
    throw new Error('coverage shard did not produce a set-mode profile with statement data');
  }
  writeFileSync(join(cwd, shardManifest(plan.index)), JSON.stringify({
    ...plan, revision, profileDigest: digest(profile), passed: [...passed].sort(),
  }) + '\n');
}

// All shards must prove the same inventory and revision, exact execution, and
// the digest of their own profile before any result can reach the floor gate.
export function mergeCoveragePartitions(cwd, count = COVERAGE_SHARDS, expectedRevision = null) {
  let inventoryDigest;
  let revision;
  const profiles = [];
  for (let index = 1; index <= count; index++) {
    const receipt = JSON.parse(readFileSync(join(cwd, shardManifest(index)), 'utf8'));
    const plan = partitionTests(receipt.inventory, index, count);
    if (receipt.index !== index || receipt.count !== count ||
        JSON.stringify(receipt.selected) !== JSON.stringify(plan.selected) ||
        JSON.stringify(receipt.selectors) !== JSON.stringify(plan.selectors)) {
      throw new Error(`coverage shard ${index} does not match its complete partition`);
    }
    const currentInventory = digest(JSON.stringify(receipt.inventory));
    if (index === 1) { inventoryDigest = currentInventory; revision = receipt.revision; }
    if (currentInventory !== inventoryDigest || !revision || receipt.revision !== revision ||
        (expectedRevision && receipt.revision !== expectedRevision)) {
      throw new Error('coverage shard inventories or revisions differ');
    }
    assertExecuted(plan, receipt.passed);
    const profile = join(cwd, shardProfile(index));
    if (digest(readFileSync(profile)) !== receipt.profileDigest) throw new Error(`coverage shard ${index} profile digest differs`);
    profiles.push(profile);
  }
  writeFoldedProfile(join(cwd, 'cover-chdb.out'), profiles);
}
