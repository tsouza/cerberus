// k3d-image-import.test.mjs — node:test guard for k3d-image-import.mjs's
// pure helpers (cerberus issue #3096): the ref-normalization `case`
// translation, the exclusion-filter glob, and the retry/verify loop against
// a FAKE `docker exec ctr images ls` + `k3d image import`. The real
// containerd verification and the real cluster this loop runs against are
// exercised live by `just e2e-up` in the real `e2e` CI job.

import assert from 'node:assert/strict';
import test from 'node:test';

import {
  normalizeRef,
  matchesAnyExcludePattern,
  filterImages,
  containerdHasImage,
  importAndVerifyOne,
} from './k3d-image-import.mjs';

test('normalizeRef leaves a ref with a registry host untouched', () => {
  assert.equal(normalizeRef('ghcr.io/open-telemetry/opentelemetry-collector-contrib/telemetrygen:v0.116.0'),
    'ghcr.io/open-telemetry/opentelemetry-collector-contrib/telemetrygen:v0.116.0');
  assert.equal(normalizeRef('localhost:5000/foo:bar'), 'localhost:5000/foo:bar');
});

test('normalizeRef prefixes a namespace/name ref with docker.io/', () => {
  assert.equal(normalizeRef('clickhouse/clickhouse-server:26.6-alpine'), 'docker.io/clickhouse/clickhouse-server:26.6-alpine');
});

test('normalizeRef prefixes a bare name with docker.io/library/', () => {
  assert.equal(normalizeRef('busybox:1.37'), 'docker.io/library/busybox:1.37');
});

test('matchesAnyExcludePattern matches a single-segment wildcard', () => {
  const patterns = ['clickhouse/clickhouse-server:*-alpine'];
  assert.equal(matchesAnyExcludePattern('clickhouse/clickhouse-server:26.6-alpine', patterns), true);
  assert.equal(matchesAnyExcludePattern('clickhouse/clickhouse-server:26.3', patterns), false);
  assert.equal(matchesAnyExcludePattern('busybox:1.37', patterns), false);
});

test('matchesAnyExcludePattern never lets * cross a / boundary', () => {
  // A pattern for one repository must never accidentally swallow another's
  // images just because both contain a common substring around a slash.
  assert.equal(matchesAnyExcludePattern('clickhouse/clickhouse-server-extra:26.6-alpine', ['clickhouse/*:26.6-alpine']), true);
  assert.equal(matchesAnyExcludePattern('other/clickhouse-server:26.6-alpine', ['clickhouse/*:26.6-alpine']), false);
});

test('filterImages drops only images matching an exclude pattern, preserving order', () => {
  const images = ['cerberus:e2e', 'clickhouse/clickhouse-server:26.6-alpine', 'busybox:1.37'];
  assert.deepEqual(
    filterImages(images, ['clickhouse/clickhouse-server:*-alpine']),
    ['cerberus:e2e', 'busybox:1.37'],
  );
});

test('filterImages with no patterns returns every image, unfiltered', () => {
  const images = ['a:1', 'b:2'];
  assert.deepEqual(filterImages(images, []), images);
});

test('containerdHasImage matches an exact ref line among many', () => {
  const fakeCapture = () => ({ status: 0, stdout: 'docker.io/library/busybox:1.37\ndocker.io/library/other:1\n', stderr: '' });
  assert.equal(containerdHasImage('k3d-x-server-0', 'docker.io/library/busybox:1.37', fakeCapture), true);
  assert.equal(containerdHasImage('k3d-x-server-0', 'docker.io/library/nope:1', fakeCapture), false);
});

test('containerdHasImage treats a non-zero status as not-landed, never throws', () => {
  const fakeCapture = () => ({ status: 1, stdout: '', stderr: 'Error: No such container' });
  assert.equal(containerdHasImage('k3d-x-server-0', 'docker.io/library/busybox:1.37', fakeCapture), false);
});

test('importAndVerifyOne returns true on the attempt where containerd first reports the ref', async () => {
  let calls = 0;
  const sleeps = [];
  const fakeCapture = (cmd) => {
    if (cmd === 'k3d') return { status: 0, stdout: '', stderr: '' };
    calls += 1;
    // Lands only from the 3rd containerd check onward.
    return { status: 0, stdout: calls >= 3 ? 'docker.io/library/busybox:1.37\n' : '', stderr: '' };
  };
  const ok = await importAndVerifyOne('busybox:1.37', {
    cluster: 'cerberus-e2e',
    serverContainer: 'k3d-cerberus-e2e-server-0',
    attempts: 5,
    backoffStepSeconds: 2,
    sleepFn: async (ms) => sleeps.push(ms),
    captureImpl: fakeCapture,
  });
  assert.equal(ok, true);
  assert.equal(calls, 3);
  // Backoff runs between attempts 1->2 and 2->3 only (landed on attempt 3).
  assert.deepEqual(sleeps, [2000, 4000]);
});

test('importAndVerifyOne exhausts its attempt budget and returns false when it never lands', async () => {
  const sleeps = [];
  const fakeCapture = () => ({ status: 0, stdout: '', stderr: '' });
  const ok = await importAndVerifyOne('busybox:1.37', {
    cluster: 'cerberus-e2e',
    serverContainer: 'k3d-cerberus-e2e-server-0',
    attempts: 3,
    backoffStepSeconds: 2,
    sleepFn: async (ms) => sleeps.push(ms),
    captureImpl: fakeCapture,
  });
  assert.equal(ok, false);
  // 3 attempts -> 2 backoff sleeps between them, none after the last.
  assert.deepEqual(sleeps, [2000, 4000]);
});
