// compose-pull-images.test.mjs — node:test guard for the model→image decision
// the compose pre-pull makes.
//
// Runs on the CHEAP lint/check lane — no compose stack, no daemon — because the
// decision is a pure function of the parsed compose config and its failure mode
// is expensive elsewhere: classifying a BUILT service as pullable sends the lane
// chasing an image ref that exists in no registry, five retries deep, and the
// only symptom is a manifest-not-found five minutes into a substrate job (run
// 30281594098).
//
// `test/regression/justfile_pull_retry_test.go` is the other half: it feeds the
// REAL compose files under `test/e2e/migration` through the module with a stub
// `docker` on PATH and pins the pulled set exactly. This file pins the shapes
// the live tree does not currently contain, so a model that grows one is
// already decided.

import { test } from 'node:test';
import assert from 'node:assert/strict';

import { pullableImages, splitArgs } from './compose-pull-images.mjs';

test('the argument list splits compose files from service names at `--`', () => {
  assert.deepEqual(splitArgs(['docker-compose.yml']), { files: ['docker-compose.yml'], services: [] });
  assert.deepEqual(splitArgs(['a.yml', 'b.yml', '--', 'clickhouse', 'cerberus']), {
    files: ['a.yml', 'b.yml'],
    services: ['clickhouse', 'cerberus'],
  });
  // A trailing separator narrows nothing, which is the whole model — the same
  // answer as omitting it, rather than an empty pull set.
  assert.deepEqual(splitArgs(['a.yml', '--']), { files: ['a.yml'], services: [] });
  // No compose file before the separator is the caller error `main` exits on;
  // the split still reports it faithfully rather than absorbing it.
  assert.deepEqual(splitArgs(['--', 'mimir']), { files: [], services: ['mimir'] });
});

test('a missing or serviceless model yields no images', () => {
  assert.deepEqual(pullableImages(undefined), []);
  assert.deepEqual(pullableImages({}), []);
  assert.deepEqual(pullableImages({ services: {} }), []);
});

test('plain image services are pullable', () => {
  assert.deepEqual(
    pullableImages({
      services: {
        clickhouse: { image: 'clickhouse/clickhouse-server:26.5' },
        prometheus: { image: 'prom/prometheus:v3.11.3' },
      },
    }),
    ['clickhouse/clickhouse-server:26.5', 'prom/prometheus:v3.11.3'],
  );
});

test('a service with a build section is never pulled', () => {
  // The shape that broke run 30281594098: an image ref that exists in no
  // registry, because compose builds it during `up`.
  assert.deepEqual(
    pullableImages({
      services: {
        'dead-end-receiver': {
          image: 'cerberus-migration-tier2:dead-end-receiver',
          build: { context: '.' },
        },
        clickhouse: { image: 'clickhouse/clickhouse-server:26.5' },
      },
    }),
    ['clickhouse/clickhouse-server:26.5'],
  );
});

test('pull_policy build/never are not fetched, missing is', () => {
  assert.deepEqual(
    pullableImages({
      services: {
        built: { image: 'cerberus:local', pull_policy: 'build' },
        held: { image: 'cerberus:cached', pull_policy: 'never' },
        fetched: { image: 'grafana/loki:3.7.0', pull_policy: 'missing' },
      },
    }),
    ['grafana/loki:3.7.0'],
  );
});

test('duplicate refs collapse and imageless services are skipped', () => {
  assert.deepEqual(
    pullableImages({
      services: {
        a: { image: 'clickhouse/clickhouse-server:26.5' },
        b: { image: 'clickhouse/clickhouse-server:26.5' },
        c: { image: '   ' },
        d: {},
      },
    }),
    ['clickhouse/clickhouse-server:26.5'],
  );
});

test('importing the module pulls nothing (the entrypoint guard holds)', () => {
  // The import above is the assertion: if the module ran `main()` on import it
  // would have exited non-zero on "no compose file given" before this file's
  // first test registered. Pinned explicitly so the guard cannot be dropped
  // while the other tests keep passing by accident.
  assert.equal(typeof pullableImages, 'function');
});
