import assert from 'node:assert/strict';
import {
  cpSync,
  existsSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  rmSync,
  unlinkSync,
  writeFileSync,
} from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { test } from 'node:test';

import {
  SCHEMA_VERSION,
  SUPPORTED_HEADS,
  bucketForCaseId,
  bucketNames,
  loadParityBaseline,
  materializeParityBaseline,
  syncParityBaselineHead,
} from './compat-baseline.mjs';

function baseline(casesByHead = {}) {
  const heads = {};
  for (const head of SUPPORTED_HEADS) {
    const cases = [...(casesByHead[head] ?? [`${head} | seed`])].sort();
    heads[head] = { passed: cases.length, total: cases.length, cases };
  }
  return { heads };
}

function fixture(casesByHead = {}) {
  const dir = mkdtempSync(path.join(tmpdir(), 'compat-baseline-'));
  const root = path.join(dir, 'parity-baseline');
  materializeParityBaseline(root, baseline(casesByHead));
  return { dir, root };
}

function canonicalWrite(file, value) {
  writeFileSync(file, `${JSON.stringify(value, null, 2)}\n`);
}

function bucketPath(root, head, bucket) {
  return path.join(root, head, `${bucket}.json`);
}

function snapshotTree(root) {
  const out = new Map();
  const walk = (dir, prefix = '') => {
    for (const entry of readdirSync(dir, { withFileTypes: true })) {
      const rel = prefix ? `${prefix}/${entry.name}` : entry.name;
      if (entry.isDirectory()) walk(path.join(dir, entry.name), rel);
      else out.set(rel, readFileSync(path.join(dir, entry.name), 'utf8'));
    }
  };
  walk(root);
  return out;
}

function idsInSameBucket(prefix) {
  const byBucket = new Map();
  for (let index = 0; ; index++) {
    const id = `${prefix}-${index}`;
    const bucket = bucketForCaseId(id);
    const prior = byBucket.get(bucket);
    if (prior) return [prior, id];
    byBucket.set(bucket, id);
  }
}

test('loader reconstructs the monolith semantic shape with global ordering', () => {
  const source = baseline({ prometheus: ['z-last', 'a-first', 'm-middle'] });
  const { dir, root } = fixture({ prometheus: source.heads.prometheus.cases });
  try {
    assert.deepEqual(loadParityBaseline(root), source);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('loader fails closed on missing and extra bucket files', async (t) => {
  await t.test('missing bucket', () => {
    const { dir, root } = fixture();
    try {
      unlinkSync(bucketPath(root, 'prometheus', '00'));
      assert.throws(() => loadParityBaseline(root), /bucket roster mismatch.*missing: 00\.json/);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
  await t.test('extra bucket', () => {
    const { dir, root } = fixture();
    try {
      canonicalWrite(bucketPath(root, 'prometheus', 'ff'), {});
      assert.throws(() => loadParityBaseline(root), /bucket roster mismatch.*extra: ff\.json/);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});

test('loader rejects malformed, duplicate, wrongly placed, and nondeterministic entries', async (t) => {
  await t.test('malformed entry', () => {
    const { dir, root } = fixture();
    try {
      const id = 'prometheus | seed';
      const file = bucketPath(root, 'prometheus', bucketForCaseId(id));
      canonicalWrite(file, { schema_version: SCHEMA_VERSION, head: 'prometheus', bucket: bucketForCaseId(id) });
      assert.throws(() => loadParityBaseline(root), /expected fields .*cases/);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
  await t.test('duplicate entry', () => {
    const { dir, root } = fixture();
    try {
      const id = 'prometheus | seed';
      const bucket = bucketForCaseId(id);
      const file = bucketPath(root, 'prometheus', bucket);
      canonicalWrite(file, { schema_version: SCHEMA_VERSION, head: 'prometheus', bucket, cases: [id, id] });
      assert.throws(() => loadParityBaseline(root), /appears twice/);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
  await t.test('wrong bucket placement', () => {
    const { dir, root } = fixture();
    try {
      const id = 'prometheus | seed';
      const owner = bucketForCaseId(id);
      const wrong = bucketNames().find((bucket) => bucket !== owner);
      canonicalWrite(bucketPath(root, 'prometheus', owner), {
        schema_version: SCHEMA_VERSION,
        head: 'prometheus',
        bucket: owner,
        cases: [],
      });
      canonicalWrite(bucketPath(root, 'prometheus', wrong), {
        schema_version: SCHEMA_VERSION,
        head: 'prometheus',
        bucket: wrong,
        cases: [id],
      });
      assert.throws(() => loadParityBaseline(root), /belongs in bucket/);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
  await t.test('unsorted entries', () => {
    const [a, b] = idsInSameBucket('unsorted');
    const sorted = [a, b].sort();
    const { dir, root } = fixture({ prometheus: sorted });
    try {
      const bucket = bucketForCaseId(a);
      canonicalWrite(bucketPath(root, 'prometheus', bucket), {
        schema_version: SCHEMA_VERSION,
        head: 'prometheus',
        bucket,
        cases: [...sorted].reverse(),
      });
      assert.throws(() => loadParityBaseline(root), /not sorted deterministically/);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
  await t.test('noncanonical bytes', () => {
    const { dir, root } = fixture();
    try {
      const id = 'prometheus | seed';
      const file = bucketPath(root, 'prometheus', bucketForCaseId(id));
      writeFileSync(file, `${readFileSync(file, 'utf8')}\n`);
      assert.throws(() => loadParityBaseline(root), /not canonical deterministic JSON/);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});

test('loader rejects hollow heads and the retired monolith', async (t) => {
  await t.test('hollow head', () => {
    const { dir, root } = fixture();
    try {
      for (const bucket of bucketNames()) {
        canonicalWrite(bucketPath(root, 'prometheus', bucket), {
          schema_version: SCHEMA_VERSION,
          head: 'prometheus',
          bucket,
          cases: [],
        });
      }
      assert.throws(() => loadParityBaseline(root), /hollow head gates nothing/);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
  await t.test('stale monolith', () => {
    const { dir, root } = fixture();
    try {
      canonicalWrite(`${root}.json`, baseline());
      assert.throws(() => loadParityBaseline(root), /stale monolithic parity baseline/);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});

test('two unrelated same-head additions change disjoint bucket files', () => {
  const original = fixture({ prometheus: ['existing-prometheus-case'] });
  const branchA = path.join(original.dir, 'branch-a');
  const branchB = path.join(original.dir, 'branch-b');
  cpSync(original.root, branchA, { recursive: true });
  cpSync(original.root, branchB, { recursive: true });
  try {
    let first = 'unrelated-case-a';
    let second = 'unrelated-case-b';
    for (let index = 0; bucketForCaseId(first) === bucketForCaseId(second); index++) {
      second = `unrelated-case-b-${index}`;
    }
    const resultA = syncParityBaselineHead(branchA, 'prometheus', ['existing-prometheus-case', first].sort());
    const resultB = syncParityBaselineHead(branchB, 'prometheus', ['existing-prometheus-case', second].sort());
    const changedA = resultA.changed.map((file) => path.basename(file));
    const changedB = resultB.changed.map((file) => path.basename(file));
    assert.deepEqual(changedA, [`${bucketForCaseId(first)}.json`]);
    assert.deepEqual(changedB, [`${bucketForCaseId(second)}.json`]);
    assert.equal(changedA.some((file) => changedB.includes(file)), false);
    assert.equal(readFileSync(path.join(branchA, 'manifest.json'), 'utf8'), readFileSync(path.join(branchB, 'manifest.json'), 'utf8'));
  } finally {
    rmSync(original.dir, { recursive: true, force: true });
  }
});

test('selected-head sync leaves every other head byte-identical', () => {
  const { dir, root } = fixture({ prometheus: ['old-case'] });
  try {
    const before = snapshotTree(root);
    const missing = bucketPath(root, 'prometheus', '00');
    unlinkSync(missing);
    const stale = bucketPath(root, 'prometheus', 'ff');
    canonicalWrite(stale, { stale: true });
    const result = syncParityBaselineHead(root, 'prometheus', ['new-case', 'old-case']);
    assert.ok(result.changed.length > 0);
    assert.ok(result.removed.includes(stale), 'selected-head sync must prune undeclared files');
    assert.equal(existsSync(stale), false);
    assert.equal(existsSync(missing), true, 'selected-head sync must restore every declared bucket');
    const after = snapshotTree(root);
    for (const [file, bytes] of before) {
      if (file.startsWith('prometheus/')) continue;
      assert.equal(after.get(file), bytes, `${file} changed while syncing prometheus`);
    }
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
