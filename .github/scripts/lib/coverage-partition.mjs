// Complete top-level Go test partitioning for instrumented coverage.
// Every test/example/fuzz seed discovered by go test -json -list . belongs
// to exactly one shard. No source-file parser, exclusion list or timing file.
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
  if (!tests.size) throw new Error('coverage inventory contains no tests');
  return [...tests.values()].sort((a, b) => testKey(a).localeCompare(testKey(b), 'en'));
}

export function partitionTests(inventory, index, count = COVERAGE_SHARDS) {
  if (!Number.isInteger(index) || index < 1 || index > count) throw new Error(`invalid coverage shard ${index}/${count}`);
  const keys = new Set();
  for (const entry of inventory) {
    if (typeof entry.package !== 'string' || !entry.package || !testName.test(entry.name) || keys.has(testKey(entry))) {
      throw new Error('invalid or duplicate coverage inventory entry');
    }
    keys.add(testKey(entry));
  }
  const selected = inventory.filter(({ name }) => {
    const hash = createHash('sha256').update(name).digest().readUInt32BE();
    return hash % count + 1 === index;
  });
  if (!selected.length) throw new Error(`coverage shard ${index}/${count} is empty`);
  const names = [...new Set(selected.map(({ name }) => name))].sort();
  const pattern = testSelector(names);
  if (Buffer.byteLength(pattern) > maxRunPatternBytes) throw new Error(`coverage test selector is ${Buffer.byteLength(pattern)} bytes for ${names.length} names, exceeding the argument budget; increase the complete shard count`);
  return { index, count, inventory, selected, pattern };
}

export function assertExecuted(plan, passed) {
  const expected = new Set(plan.selected.map(testKey));
  const missing = [...expected].filter((key) => !passed.has(key));
  const unexpected = [...passed].filter((key) => !expected.has(key));
  if (missing.length || unexpected.length) throw new Error(`coverage execution mismatch: missing=${missing.join(',')} unexpected=${unexpected.join(',')}`);
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
        JSON.stringify(receipt.selected) !== JSON.stringify(plan.selected) || receipt.pattern !== plan.pattern) {
      throw new Error(`coverage shard ${index} does not match its complete partition`);
    }
    const currentInventory = digest(JSON.stringify(receipt.inventory));
    if (index === 1) { inventoryDigest = currentInventory; revision = receipt.revision; }
    if (currentInventory !== inventoryDigest || !revision || receipt.revision !== revision ||
        (expectedRevision && receipt.revision !== expectedRevision)) {
      throw new Error('coverage shard inventories or revisions differ');
    }
    assertExecuted(plan, new Set(receipt.passed));
    const profile = join(cwd, shardProfile(index));
    if (digest(readFileSync(profile)) !== receipt.profileDigest) throw new Error(`coverage shard ${index} profile digest differs`);
    profiles.push(profile);
  }
  writeFoldedProfile(join(cwd, 'cover-chdb.out'), profiles);
}
