// migration-tier-logs.test.mjs — node:test guard for migration-tier-logs.mjs's
// pure tier-label, list-parsing, and docker-argv-construction logic (issue
// #3099). The real `docker compose ps` / `logs` against a live stack is
// exercised by `just migration-tier1` / `migration-tier2` in the real
// migration-e2e.yml CI job.

import assert from 'node:assert/strict';
import test from 'node:test';

import { composeFileArgs, logsArgsFor, parseList, psArgs, tierLabel } from './migration-tier-logs.mjs';

test('tierLabel hyphenates the tier name', () => {
  assert.equal(tierLabel('tier1'), 'tier-1');
  assert.equal(tierLabel('tier2'), 'tier-2');
});

test('parseList splits on whitespace and drops empty tokens', () => {
  assert.deepEqual(parseList('clickhouse   otel-collector\tprometheus'), ['clickhouse', 'otel-collector', 'prometheus']);
});

test('parseList on an empty/undefined value is an empty array', () => {
  assert.deepEqual(parseList(''), []);
  assert.deepEqual(parseList(undefined), []);
});

test('composeFileArgs emits one -f pair per file, in order', () => {
  assert.deepEqual(composeFileArgs(['a.yml']), ['-f', 'a.yml']);
  assert.deepEqual(composeFileArgs(['a.yml', 'b.yml']), ['-f', 'a.yml', '-f', 'b.yml']);
});

test('psArgs builds the Tier-1-shaped single-file ps command', () => {
  assert.deepEqual(psArgs(['test/e2e/migration/tiers/tier1-dual/docker-compose.dual.yml']), [
    'compose',
    '-f',
    'test/e2e/migration/tiers/tier1-dual/docker-compose.dual.yml',
    'ps',
  ]);
});

test('psArgs builds the Tier-2-shaped two-file ps command, files in order', () => {
  assert.deepEqual(
    psArgs([
      'test/e2e/migration/tiers/tier1-dual/docker-compose.dual.yml',
      'test/e2e/migration/tiers/tier2-ruler/docker-compose.ruler.yml',
    ]),
    [
      'compose',
      '-f',
      'test/e2e/migration/tiers/tier1-dual/docker-compose.dual.yml',
      '-f',
      'test/e2e/migration/tiers/tier2-ruler/docker-compose.ruler.yml',
      'ps',
    ],
  );
});

test('logsArgsFor builds a --tail=N logs command for one service', () => {
  assert.deepEqual(logsArgsFor(['a.yml'], 'clickhouse', '200'), ['compose', '-f', 'a.yml', 'logs', '--tail=200', 'clickhouse']);
});
