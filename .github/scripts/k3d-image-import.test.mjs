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
  parseImageStore,
  detectImageStore,
  importOnce,
  exhaustedMessage,
  imageStore,
} from './k3d-image-import.mjs';

// `docker info --format '{{json .DriverStatus}}'` as each store prints it:
// the classic graphdriver (overlay2 — what ubuntu-latest CI runners run)
// lists filesystem facts; the containerd image store lists exactly one
// `driver-type` row naming the snapshotter.
const overlay2DriverStatus =
  '[["Backing Filesystem","extfs"],["Supports d_type","true"],["Using metacopy","false"],["Native Overlay Diff","true"],["userxattr","false"]]\n';
const containerdDriverStatus = '[["driver-type","io.containerd.snapshotter.v1"]]\n';

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

test('importAndVerifyOne lands on the attempt where containerd first reports the ref', async () => {
  let calls = 0;
  const sleeps = [];
  const fakeCapture = (cmd) => {
    if (cmd === 'k3d') return { status: 0, stdout: '', stderr: '' };
    calls += 1;
    // Lands only from the 3rd containerd check onward.
    return { status: 0, stdout: calls >= 3 ? 'docker.io/library/busybox:1.37\n' : '', stderr: '' };
  };
  const { landed } = await importAndVerifyOne('busybox:1.37', {
    cluster: 'cerberus-e2e',
    serverContainer: 'k3d-cerberus-e2e-server-0',
    store: imageStore.graphdriver,
    attempts: 5,
    backoffStepSeconds: 2,
    sleepFn: async (ms) => sleeps.push(ms),
    captureImpl: fakeCapture,
  });
  assert.equal(landed, true);
  assert.equal(calls, 3);
  // Backoff runs between attempts 1->2 and 2->3 only (landed on attempt 3).
  assert.deepEqual(sleeps, [2000, 4000]);
});

test('importAndVerifyOne exhausts its attempt budget, keeping the last import stderr, when it never lands', async () => {
  const sleeps = [];
  let imports = 0;
  const fakeCapture = (cmd) => {
    if (cmd === 'sh') {
      imports += 1;
      return { status: 1, stdout: '', stderr: `ctr: content digest sha256:${imports}: not found\n` };
    }
    return { status: 0, stdout: '', stderr: '' };
  };
  const { landed, lastStderr } = await importAndVerifyOne('busybox:1.37', {
    cluster: 'cerberus-e2e',
    serverContainer: 'k3d-cerberus-e2e-server-0',
    store: imageStore.containerd,
    attempts: 3,
    backoffStepSeconds: 2,
    sleepFn: async (ms) => sleeps.push(ms),
    captureImpl: fakeCapture,
  });
  assert.equal(landed, false);
  assert.equal(lastStderr, 'ctr: content digest sha256:3: not found\n');
  // 3 attempts -> 2 backoff sleeps between them, none after the last.
  assert.deepEqual(sleeps, [2000, 4000]);
});

test('parseImageStore reads the classic graphdriver from overlay2 driver status', () => {
  assert.equal(parseImageStore(overlay2DriverStatus), imageStore.graphdriver);
});

test('parseImageStore reads the containerd image store from its driver-type row', () => {
  assert.equal(parseImageStore(containerdDriverStatus), imageStore.containerd);
});

test('detectImageStore asks docker info for DriverStatus and maps both stores', () => {
  const argvSeen = [];
  const fakeInfo = (out) => (cmd, args) => {
    argvSeen.push([cmd, ...args]);
    return { status: 0, stdout: out, stderr: '' };
  };
  assert.equal(detectImageStore(fakeInfo(overlay2DriverStatus)), imageStore.graphdriver);
  assert.equal(detectImageStore(fakeInfo(containerdDriverStatus)), imageStore.containerd);
  assert.deepEqual(argvSeen, [
    ['docker', 'info', '--format', '{{json .DriverStatus}}'],
    ['docker', 'info', '--format', '{{json .DriverStatus}}'],
  ]);
});

test('detectImageStore fails loudly when docker info fails or answers nonsense', () => {
  const failing = () => ({ status: 1, stdout: '', stderr: 'Cannot connect to the Docker daemon' });
  assert.throws(() => detectImageStore(failing), /docker info.*failed.*Cannot connect to the Docker daemon/s);
  const nonsense = () => ({ status: 0, stdout: 'not json', stderr: '' });
  assert.throws(() => detectImageStore(nonsense), /docker info.*DriverStatus.*not json/s);
});

test('importOnce on the graphdriver store runs exactly the k3d image import CI uses today', () => {
  const argvSeen = [];
  const fakeCapture = (cmd, args) => {
    argvSeen.push([cmd, ...args]);
    return { status: 0, stdout: '', stderr: '' };
  };
  importOnce('busybox:1.37', imageStore.graphdriver, {
    cluster: 'cerberus-e2e',
    serverContainer: 'k3d-cerberus-e2e-server-0',
    captureImpl: fakeCapture,
  });
  assert.deepEqual(argvSeen, [['k3d', 'image', 'import', 'busybox:1.37', '-c', 'cerberus-e2e']]);
});

test('importOnce on the containerd store streams docker save into the node ctr, never k3d', () => {
  const argvSeen = [];
  const fakeCapture = (cmd, args) => {
    argvSeen.push([cmd, ...args]);
    return { status: 0, stdout: '', stderr: '' };
  };
  importOnce('clickhouse/clickhouse-server:26.6', imageStore.containerd, {
    cluster: 'cerberus-e2e',
    serverContainer: 'k3d-cerberus-e2e-server-0',
    captureImpl: fakeCapture,
  });
  assert.equal(argvSeen.length, 1);
  const [cmd, dashC, pipeline, argv0, ...positional] = argvSeen[0];
  assert.equal(cmd, 'sh');
  assert.equal(dashC, '-c');
  assert.equal(argv0, 'sh');
  // The image ref and the node container travel as positional parameters,
  // never interpolated into the pipeline text.
  assert.deepEqual(positional, ['clickhouse/clickhouse-server:26.6', 'k3d-cerberus-e2e-server-0']);
  assert.match(pipeline, /docker save "\$1" \| docker exec -i "\$2" ctr -n k8s\.io images import -/);
  // Never `--all-platforms`: that is the flag k3d's own import passes and
  // the reason the containerd-store tarball (an index naming every platform,
  // blobs for one) is refused.
  assert.doesNotMatch(pipeline, /all-platforms/);
  assert.equal(argvSeen.some((a) => a[0] === 'k3d'), false);
});

test('importAndVerifyOne threads the detected store through to the import path', async () => {
  const argvSeen = [];
  const fakeCapture = (cmd, args) => {
    argvSeen.push([cmd, ...args]);
    return { status: 0, stdout: 'docker.io/library/busybox:1.37\n', stderr: '' };
  };
  const { landed } = await importAndVerifyOne('busybox:1.37', {
    cluster: 'cerberus-e2e',
    serverContainer: 'k3d-cerberus-e2e-server-0',
    store: imageStore.containerd,
    attempts: 1,
    backoffStepSeconds: 2,
    sleepFn: async () => {},
    captureImpl: fakeCapture,
  });
  assert.equal(landed, true);
  assert.equal(argvSeen[0][0], 'sh');
  assert.equal(argvSeen.some((a) => a[0] === 'k3d'), false);
});

test('exhaustedMessage names the store, the path taken and the last import error', () => {
  const msg = exhaustedMessage('docker.io/library/busybox:1.37', 5, imageStore.containerd, 'ctr: content digest sha256:abc: not found');
  assert.match(msg, /docker\.io\/library\/busybox:1\.37 missing from k3d node containerd after 5 import attempts/);
  assert.match(msg, /containerd image store/);
  assert.match(msg, /docker save \| docker exec ctr images import/);
  assert.match(msg, /ctr: content digest sha256:abc: not found/);
  const k3dMsg = exhaustedMessage('docker.io/library/busybox:1.37', 5, imageStore.graphdriver, '');
  assert.match(k3dMsg, /overlay2 graphdriver/);
  assert.match(k3dMsg, /k3d image import/);
});
