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

/** The `timeout-minutes:` of the roundtrip job, read out of chdb.yml. */
function roundtripJobTimeoutMinutes() {
  const job = workflow.slice(workflow.indexOf('\n  roundtrip:'));
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
