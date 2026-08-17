// perf-coverage-fanout.test.mjs — node:test guard for `just coverage`'s
// chDB-tagged TestCardinalityRatchet fan-out: an explicit per-process go
// test -timeout that stays below the coverage job's cap, the extra shards
// (2..RATCHET_FANOUT) sharded without dropping or duplicating a corpus
// slice, the Justfile's own PERF_SHARD_COUNT literal pinned to the same
// RATCHET_FANOUT this script uses, and the coverage recipe still carrying
// the main sweep WITHOUT `-skip` (so test/regression/tagged_test_enrollment_
// test.go's static evidence scanner keeps crediting it).
//
// Runs on the cheap `check` lane (`node --test`), so a regression here fails
// in milliseconds rather than 40+ minutes into a coverage job.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { RATCHET_TEST, RATCHET_RUN_PATTERN, RATCHET_FANOUT, RATCHET_TIMEOUT_MINUTES, legCommands } from './perf-coverage-fanout.mjs';

const here = path.dirname(fileURLToPath(import.meta.url));
const justfile = readFileSync(path.join(here, '..', '..', 'Justfile'), 'utf8');
const coverageWorkflow = readFileSync(path.join(here, '..', 'workflows', 'coverage.yml'), 'utf8');
const TAGS = 'chdb,agpl_oracle,chdb_agpl_oracle';
const COVERPKG = 'github.com/tsouza/cerberus/internal/promql,github.com/tsouza/cerberus/test/perf';

/** The coverage recipe body, isolated the same way coverage_recipe_fail_closed_test.go does. */
function coverageRecipeBody() {
  const start = justfile.indexOf('\ncoverage:\n');
  const end = justfile.indexOf('\nupdate-coverage-floor:');
  assert.ok(start >= 0 && end >= 0 && end > start, 'Justfile: cannot isolate the coverage recipe');
  return justfile.slice(start, end);
}

/** The `timeout-minutes:` of the coverage job, read out of coverage.yml. */
function coverageJobTimeoutMinutes() {
  const start = coverageWorkflow.indexOf('\n  coverage:');
  assert.ok(start >= 0, 'coverage.yml: missing coverage job');
  const job = coverageWorkflow.slice(start + 1);
  const m = /^\s{4}timeout-minutes:\s*(\d+)/m.exec(job);
  assert.ok(m, 'coverage.yml: the coverage job declares no timeout-minutes');
  return Number(m[1]);
}

test('legCommands returns RATCHET_FANOUT - 1 extra shards, indexed 2..RATCHET_FANOUT', () => {
  const legs = legCommands({ tags: TAGS, coverpkg: COVERPKG });
  assert.equal(legs.length, RATCHET_FANOUT - 1);
  for (let i = 0; i < legs.length; i++) {
    const shardIndex = i + 2;
    assert.equal(legs[i].name, `cardinality-ratchet ${shardIndex}/${RATCHET_FANOUT}`);
    assert.equal(legs[i].env.PERF_SHARD_INDEX, String(shardIndex));
    assert.equal(legs[i].env.PERF_SHARD_COUNT, String(RATCHET_FANOUT));
  }
});

test('every leg declares an explicit go test -timeout', () => {
  for (const leg of legCommands({ tags: TAGS, coverpkg: COVERPKG })) {
    assert.ok(
      leg.argv.some((a) => a.startsWith('-timeout=')),
      `${leg.name}: no -timeout — the leg would run under go test's invisible 10m default`,
    );
  }
});

test('the ratchet-shard timeout, plus the main sweep timeout it runs sequentially after, stays under the coverage job cap with the 3-minute abort-reporting margin', () => {
  const jobCap = coverageJobTimeoutMinutes();
  const abortReportingMarginMinutes = 3;
  assert.ok(
    RATCHET_TIMEOUT_MINUTES + abortReportingMarginMinutes <= jobCap,
    `ratchet-shard leg -timeout=${RATCHET_TIMEOUT_MINUTES}m must leave at least ${abortReportingMarginMinutes}m under ` +
      `the coverage job's timeout-minutes: ${jobCap}, so go test's own alarm — which dumps every goroutine stack — ` +
      'always fires before the runner takes the container away',
  );
});

test('RATCHET_RUN_PATTERN matches exactly RATCHET_TEST at the top level', () => {
  const re = new RegExp(RATCHET_RUN_PATTERN);
  assert.ok(re.test(RATCHET_TEST));
  assert.ok(!re.test(`${RATCHET_TEST}Extra`), 'pattern must be anchored, not a prefix match');
  assert.ok(!re.test('TestSomethingElse'), 'pattern must not match an unrelated test name');
});

test('every extra shard selects only the ratchet and targets test/perf', () => {
  for (const leg of legCommands({ tags: TAGS, coverpkg: COVERPKG })) {
    assert.equal(leg.argv[leg.argv.indexOf('-run') + 1], RATCHET_RUN_PATTERN);
    assert.ok(leg.argv.includes('./test/perf/...'), `${leg.name}: must target test/perf`);
    assert.ok(!leg.argv.includes('-skip'), `${leg.name}: must not carry -skip`);
  }
});

test('every leg carries the identical -coverpkg and its own distinct -coverprofile, matching leg.profile', () => {
  const legs = legCommands({ tags: TAGS, coverpkg: COVERPKG });
  const profiles = new Set();
  for (const leg of legs) {
    const i = leg.argv.indexOf('-coverpkg');
    assert.ok(i >= 0 && leg.argv[i + 1] === COVERPKG, `${leg.name}: -coverpkg dropped or altered`);
    const p = leg.argv[leg.argv.indexOf('-coverprofile') + 1];
    assert.ok(p, `${leg.name}: no -coverprofile`);
    assert.ok(!profiles.has(p), `${leg.name}: -coverprofile ${p} reused by another leg`);
    profiles.add(p);
    assert.equal(leg.profile, p, `${leg.name}: leg.profile must match its own -coverprofile`);
  }
});

test('the build tags reach every go command verbatim', () => {
  for (const leg of legCommands({ tags: TAGS, coverpkg: COVERPKG })) {
    const i = leg.argv.indexOf('-tags');
    assert.ok(i >= 0 && leg.argv[i + 1] === TAGS, `${leg.name}: build tags dropped`);
  }
});

test('the coverage recipe still invokes the fan-out script with the composite tag set', () => {
  const body = coverageRecipeBody();
  assert.match(body, /node \.github\/scripts\/perf-coverage-fanout\.mjs/);
  assert.match(body, /TAGS=chdb,agpl_oracle,chdb_agpl_oracle/);
});

test('the coverage recipe main sweep carries PERF_SHARD_INDEX=1 and a PERF_SHARD_COUNT matching RATCHET_FANOUT — the two constants must not drift apart', () => {
  const body = coverageRecipeBody();
  const sweepLine = body.split('\n').find((l) => l.includes('PERF_SHARD_INDEX=1') && l.includes('go test'));
  assert.ok(sweepLine, 'Justfile: could not find the main sweep line');
  assert.ok(sweepLine.includes('-tags chdb,agpl_oracle,chdb_agpl_oracle'), 'main sweep: missing composite tag set');
  assert.ok(sweepLine.includes('-coverprofile=cover-chdb.out'), 'main sweep: missing -coverprofile=cover-chdb.out');
  assert.ok(sweepLine.includes('./...'), 'main sweep: must target the whole tree');
  const m = /PERF_SHARD_COUNT=(\d+)/.exec(sweepLine);
  assert.ok(m, 'Justfile: main sweep declares no PERF_SHARD_COUNT');
  assert.equal(
    Number(m[1]),
    RATCHET_FANOUT,
    `Justfile's PERF_SHARD_COUNT=${m[1]} on the main sweep must equal perf-coverage-fanout.mjs's own RATCHET_FANOUT=${RATCHET_FANOUT}`,
  );
});

test('the coverage recipe main sweep never carries -skip — it is the sole CI evidence for other chdb-tagged packages', () => {
  const body = coverageRecipeBody();
  const sweepLine = body
    .split('\n')
    .find((l) => l.includes('PERF_SHARD_INDEX=1') && l.includes('go test'));
  assert.ok(sweepLine, 'Justfile: could not find the main sweep line');
  assert.ok(
    !sweepLine.includes('-skip'),
    'the main sweep must not use -skip: test/regression/tagged_test_enrollment_test.go discards a go test ' +
      'invocation outright the moment it sees -skip, dropping this sweep\'s enrollment evidence for every ' +
      'other chdb-tagged package it is the sole CI evidence for',
  );
});

test('the merge step folds cover-chdb.out together with every extra shard profile', () => {
  const body = coverageRecipeBody();
  assert.match(body, /cover-chdb\.out cover-chdb-ratchet-\*\.out/);
});
