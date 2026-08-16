// chdb-roundtrip.test.mjs — node:test guard for the `roundtrip (<ql>)` leg
// runner's two load-bearing properties: the per-process `go test -timeout`
// exists and stays diagnosable, and the fan-out is a real partition.
//
// Runs on the cheap `check` lane (`node --test`), so a regression here fails in
// milliseconds rather than 20 minutes into a chDB job.
//
// The timeout ordering assertion reads chdb.yml itself rather than restating
// its number, because the property being pinned is a RELATION between two files:
// `go test`'s own alarm has to fire BEFORE the runner kills the job, or the
// goroutine dump that names the wedged frame is never printed. That dump is the
// whole reason #2096 was diagnosable at all.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { legCommands, warmupCommand, GO_TEST_TIMEOUT_MINUTES, HEADS } from './chdb-roundtrip.mjs';

const here = path.dirname(fileURLToPath(import.meta.url));
const workflow = readFileSync(path.join(here, '..', 'workflows', 'chdb.yml'), 'utf8');
const registry = JSON.parse(readFileSync(path.join(here, '..', 'ci-lanes.json'), 'utf8'));

const EXPECTED_ROUNDTRIP_TAGS = new Map([
  ['promql', 'chdb'],
  ['logql', 'chdb,agpl_oracle,chdb_agpl_oracle'],
  ['traceql', 'chdb,agpl_oracle,chdb_agpl_oracle'],
]);

function roundtripJob() {
  const start = workflow.indexOf('\n  roundtrip:');
  assert.ok(start >= 0, 'chdb.yml: missing roundtrip job');
  const remainder = workflow.slice(start + 1);
  const nextJob = /^  [a-zA-Z0-9_-]+:\s*$/m.exec(remainder.slice('  roundtrip:\n'.length));
  return nextJob
    ? remainder.slice(0, '  roundtrip:\n'.length + nextJob.index)
    : remainder;
}

/** Read the exact per-head matrix from the workflow rather than restating it. */
function roundtripMatrix() {
  const job = roundtripJob();
  const matrixStart = job.indexOf('\n      matrix:');
  assert.ok(matrixStart >= 0, 'chdb.yml: roundtrip job has no strategy.matrix');
  const header = '      matrix:';
  const matrixRemainder = job.slice(matrixStart + 1 + header.length);
  const matrixLines = [];
  for (const line of matrixRemainder.split('\n')) {
    const trimmed = line.trim();
    const indentation = line.length - line.trimStart().length;
    if (trimmed !== '' && !trimmed.startsWith('#') && indentation <= 6) break;
    matrixLines.push(line);
  }
  const entries = [];
  let pending = null;
  let sawInclude = false;
  for (const raw of matrixLines) {
    const trimmed = raw.trim();
    if (trimmed === '' || trimmed.startsWith('#')) continue;
    if (raw === '        include:') {
      assert.equal(sawInclude, false, 'chdb.yml: roundtrip matrix repeats include');
      assert.equal(pending, null, 'chdb.yml: incomplete roundtrip matrix entry before include');
      sawInclude = true;
      continue;
    }
    const ql = /^ {10}- ql:\s*(\S+)\s*$/.exec(raw);
    if (ql) {
      assert.ok(sawInclude, 'chdb.yml: roundtrip ql entry appears outside matrix.include');
      assert.equal(pending, null, 'chdb.yml: roundtrip matrix entry has no tags');
      pending = ql[1];
      continue;
    }
    const tags = /^ {12}tags:\s*(\S+)\s*$/.exec(raw);
    if (tags) {
      assert.notEqual(pending, null, 'chdb.yml: roundtrip tags have no ql');
      entries.push({ ql: pending, tags: tags[1] });
      pending = null;
      continue;
    }
    assert.fail(`chdb.yml: unsupported roundtrip matrix line ${JSON.stringify(raw)}`);
  }
  assert.ok(sawInclude, 'chdb.yml: roundtrip matrix has no include roster');
  assert.equal(pending, null, 'chdb.yml: roundtrip matrix entry has no tags');
  return entries;
}

function assertRoundtripEnrollment(matrix, lanes) {
  const byHead = new Map();
  for (const entry of matrix) {
    assert.equal(byHead.has(entry.ql), false, `${entry.ql}: duplicate roundtrip matrix entry`);
    byHead.set(entry.ql, entry.tags);
  }
  assert.deepEqual(
    [...byHead.keys()].sort(),
    [...EXPECTED_ROUNDTRIP_TAGS.keys()].sort(),
    'roundtrip matrix must contain each query head exactly once, with no extra head',
  );
  for (const [ql, expectedTags] of EXPECTED_ROUNDTRIP_TAGS) {
    const tags = byHead.get(ql);
    assert.equal(tags, expectedTags, `${ql}: workflow tag set`);
    const matches = lanes.filter(({ id }) => id === `chdb.roundtrip-${ql}`);
    assert.equal(matches.length, 1, `${ql}: expected exactly one roundtrip registry lane`);
    assert.deepEqual(
      [...matches[0].build_tags].sort(),
      tags.split(',').sort(),
      `${ql}: registry tags must describe the workflow execution`,
    );
    const commands = [...legCommands({ ql, tags })];
    const warmup = warmupCommand({ ql, tags });
    if (warmup) commands.push(warmup);
    assert.ok(commands.length > 0, `${ql}: runner emitted no command`);
    for (const command of commands) {
      const index = command.argv.indexOf('-tags');
      assert.equal(command.argv[index + 1], tags, `${command.name}: runner changed the matrix tags`);
    }
  }
}

/** The `timeout-minutes:` of the roundtrip job, read out of chdb.yml. */
function roundtripJobTimeoutMinutes() {
  const job = roundtripJob();
  const m = /^\s{4}timeout-minutes:\s*(\d+)/m.exec(job);
  assert.ok(m, 'chdb.yml: the roundtrip job declares no timeout-minutes');
  return Number(m[1]);
}

test('every leg declares an explicit go test -timeout (the #2096/#2094 regression)', () => {
  for (const ql of HEADS) {
    for (const leg of legCommands({ ql, tags: 'chdb' })) {
      assert.ok(
        leg.argv.some((a) => a.startsWith('-timeout=')),
        `${leg.name}: no -timeout — the leg would run under go test's invisible 10m default`,
      );
    }
  }
});

test('the per-process timeout stays strictly below the job cap, so the dump is printed', () => {
  const jobCap = roundtripJobTimeoutMinutes();
  assert.ok(
    GO_TEST_TIMEOUT_MINUTES < jobCap,
    `go test -timeout=${GO_TEST_TIMEOUT_MINUTES}m must be < the roundtrip job's ` +
      `timeout-minutes: ${jobCap}; at or above it the runner kills the job first and the ` +
      'goroutine dump that names the wedged frame is lost',
  );
});

test('a fanned-out head partitions 1..N and declares both env variables', () => {
  const legs = legCommands({ ql: 'promql', tags: 'chdb' });
  assert.ok(legs.length > 1, 'promql is the head the fan-out exists for');
  const indices = legs.map((l) => Number(l.env.SPEC_SHARD_INDEX));
  assert.deepEqual(
    indices,
    legs.map((_, i) => i + 1),
    'indices must be contiguous and 1-based: an index outside [1, N] names a slice ' +
      'that does not exist, so that leg walks nothing and still exits 0',
  );
  for (const leg of legs) {
    assert.equal(leg.env.SPEC_SHARD_COUNT, String(legs.length));
  }
});

test('an unfanned head declares NO shard env rather than 1/1', () => {
  for (const ql of HEADS.filter((h) => legCommands({ ql: h, tags: 'chdb' }).length === 1)) {
    const [leg] = legCommands({ ql, tags: 'chdb' });
    assert.deepEqual(
      leg.env,
      {},
      `${ql}: a whole-corpus leg must reach spec.WalkShard through the unset-means-everything ` +
        'path, not through a half-declared partition',
    );
  }
});

test('each leg runs both packages of its head, and only its own head', () => {
  for (const ql of HEADS) {
    for (const leg of legCommands({ ql, tags: 'chdb' })) {
      assert.ok(leg.argv.includes(`./test/spec/${ql}/...`), `${leg.name}: missing the pre-optimizer walk`);
      assert.ok(leg.argv.includes(`./internal/${ql}/...`), `${leg.name}: missing the post-optimizer walk`);
      for (const other of HEADS.filter((h) => h !== ql)) {
        assert.ok(
          !leg.argv.some((a) => a.includes(`/${other}/`)),
          `${leg.name}: walks ${other}, which its own matrix leg already covers`,
        );
      }
    }
  }
});

test('legs run one package at a time, so N legs mean N test binaries', () => {
  const legs = legCommands({ ql: 'promql', tags: 'chdb' });
  for (const leg of legs) {
    const p = leg.argv.indexOf('-p');
    assert.ok(p >= 0 && leg.argv[p + 1] === '1', `${leg.name}: without -p 1 the real concurrency is 2N`);
  }
});

test('a fanned-out head warms the build cache, and the warm-up runs NO test', () => {
  const warmup = warmupCommand({ ql: 'promql', tags: 'chdb' });
  assert.ok(warmup, 'a fan-out without a warm-up compiles the same binaries once per leg');
  assert.ok(
    warmup.argv.includes('-run=^$'),
    'the warm-up must match no test — it exists to compile, not to walk a slice of the corpus',
  );
  assert.deepEqual(warmup.env, {}, 'the warm-up owns no corpus slice');
});

test('an unfanned head has no warm-up: its single leg IS the compile', () => {
  for (const ql of HEADS.filter((h) => legCommands({ ql: h, tags: 'chdb' }).length === 1)) {
    assert.equal(warmupCommand({ ql, tags: 'chdb' }), null);
  }
});

test('the build tags reach the go command verbatim', () => {
  const tags = 'chdb,agpl_oracle,chdb_agpl_oracle';
  for (const leg of legCommands({ ql: 'logql', tags })) {
    const i = leg.argv.indexOf('-tags');
    assert.ok(i >= 0 && leg.argv[i + 1] === tags, `${leg.name}: build tags dropped`);
  }
});

test('workflow, registry, and runner pin the exact per-head oracle tag matrix', () => {
  assertRoundtripEnrollment(roundtripMatrix(), registry.lanes);
});

test('every head rejects a tag set that compiles out its live reference path', () => {
  const withoutLiveReference = new Map([
    ['promql', 'agpl_oracle'],
    ['logql', 'chdb'],
    ['traceql', 'chdb'],
  ]);
  for (const [ql, tags] of withoutLiveReference) {
    const downgraded = roundtripMatrix().map((entry) =>
      entry.ql === ql ? { ...entry, tags } : entry,
    );
    assert.throws(
      () => assertRoundtripEnrollment(downgraded, registry.lanes),
      /workflow tag set/,
      `${ql}: removing the reference tags must break the contract`,
    );
  }
});
