// migration-artifact.test.mjs — node:test guard for the migration lane's
// artifact resolver.
//
// Runs on the CHEAP required `lint` lane (`node --test`) — no Docker, no
// network. migration-e2e.yml has no `pull_request:` trigger and release.yml
// has none either, so without this suite every edit to resolvePlan would be
// unverified until a release is cut.
//
// The headline case is a partially supplied candidate identity: it must fail,
// not quietly degrade to a from-source build under an artifact-shaped job name.
// Its negative control is the source path, where all candidate inputs are empty.

import { test } from 'node:test';
import assert from 'node:assert/strict';

import {
  resolvePlan,
  SOURCE_BUILD_VERSION,
  LOCAL_IMAGE_TAG,
  IMAGE_BINARY_PATH,
  loopbackRegistryEndpoint,
  PAIRED_INPUTS_MESSAGE,
  CANDIDATE_REGISTRY_ENV,
  stopLoopbackRegistry,
} from './migration-artifact.mjs';

const releaseVersion = '1.11.2';
const candidate = {
  candidateDir: 'build/release-candidate',
  candidateDigest: `sha256:${'a'.repeat(64)}`,
  expectVersion: releaseVersion,
  sourceSHA: 'b'.repeat(40),
  sourceTree: 'c'.repeat(40),
};

test('positive control: neither input supplied resolves the source plan', () => {
  const plan = resolvePlan({
    candidateDir: '',
    candidateDigest: '',
    expectVersion: '',
    sourceSHA: '',
    sourceTree: '',
  });

  assert.equal(plan.source, 'source');
  assert.equal(plan.image, LOCAL_IMAGE_TAG);
  // The CLI comes from `go build` with no ldflags, so it reports `dev` …
  assert.equal(plan.expectVersion, SOURCE_BUILD_VERSION);
  // … while the compose server comes from Dockerfile.local and reports `e2e`,
  // so there is no ONE stamp to export. An empty sharedVersion is what makes
  // the Go harness hold each side to its own default instead of to a wrong
  // shared value — the assertion is never skipped, only differently computed.
  assert.equal(plan.sharedVersion, '');
});

test('positive control: undefined inputs are the same as empty ones', () => {
  assert.deepEqual(resolvePlan({}), resolvePlan({
    candidateDir: '',
    candidateDigest: '',
    expectVersion: '',
    sourceSHA: '',
    sourceTree: '',
  }));
  assert.deepEqual(resolvePlan(), resolvePlan({}));
});

test('whitespace-only inputs count as absent, not as a half-supplied pair', () => {
  const plan = resolvePlan({ candidateDir: '   ', expectVersion: '\n' });
  assert.equal(plan.source, 'source');
});

test('every partial candidate identity is refused', () => {
  for (const key of Object.keys(candidate)) {
    const partial = { ...candidate, [key]: '' };
    assert.throws(
      () => resolvePlan(partial),
      (err) => {
        assert.equal(err.message, PAIRED_INPUTS_MESSAGE);
        return true;
      },
      key,
    );
  }
});

test('mutation control: a partial identity never coerces to the source default', () => {
  // The tempting "fix" for the half-supplied case is to fill the missing half
  // in from the source defaults. That would run a `go build` while the job name
  // and the notice both claim a candidate artifact — the exact hollow green this
  // module exists to prevent — so assert the refusal is a THROW and that no
  // plan carrying the source stamp escapes.
  let escaped = null;
  try {
    escaped = resolvePlan({ ...candidate, candidateDigest: '' });
  } catch {
    escaped = null;
  }
  assert.equal(escaped, null, 'resolvePlan returned a plan for a half-supplied pair');

  let escapedInverse = null;
  try {
    escapedInverse = resolvePlan({ expectVersion: releaseVersion });
  } catch {
    escapedInverse = null;
  }
  assert.equal(escapedInverse, null, 'resolvePlan invented an image for a version-only input');
});

test('a complete candidate identity carries every binding through verbatim', () => {
  const plan = resolvePlan(candidate);

  assert.equal(plan.source, 'candidate');
  assert.equal(plan.candidateDir, candidate.candidateDir);
  assert.equal(plan.candidateDigest, candidate.candidateDigest);
  assert.equal(plan.sourceSHA, candidate.sourceSHA);
  assert.equal(plan.sourceTree, candidate.sourceTree);
  assert.equal(plan.expectVersion, releaseVersion);
  // CLI and server are the same bytes on this path, so the stamp is shared and
  // BOTH the binary probe and the live /info probe assert against it.
  assert.equal(plan.sharedVersion, releaseVersion);
  assert.notEqual(plan.expectVersion, SOURCE_BUILD_VERSION);
});

test('the extraction path is the image path the release Dockerfiles write', () => {
  // Both Dockerfile and Dockerfile.local COPY the binary here and the compose
  // healthcheck execs it here; a drift would make `docker cp` fail opaquely.
  assert.equal(IMAGE_BINARY_PATH, '/usr/local/bin/cerberus');
});

test('the candidate registry uses one Docker-assigned loopback port', () => {
  assert.equal(loopbackRegistryEndpoint('127.0.0.1:49153\n'), '127.0.0.1:49153');
  for (const malformed of [
    '',
    '0.0.0.0:49153',
    '127.0.0.1:0',
    '127.0.0.1:65536',
    '127.0.0.1:not-a-port',
    '127.0.0.1:49153\n127.0.0.1:49154\n',
  ]) {
    assert.throws(() => loopbackRegistryEndpoint(malformed), /loopback registry/);
  }
});

test('the candidate registry survives resolution and has one constrained cleanup handle', () => {
  assert.equal(CANDIDATE_REGISTRY_ENV, 'CERBERUS_CANDIDATE_REGISTRY_CONTAINER');

  const calls = [];
  const runner = (command, args) => {
    calls.push([command, args]);
    return { status: 0, stdout: args.at(-1), stderr: '' };
  };
  assert.equal(
    stopLoopbackRegistry('cerberus-migration-candidate-registry-123', runner),
    true,
  );
  assert.deepEqual(calls, [
    [
      'docker',
      ['rm', '-f', 'cerberus-migration-candidate-registry-123'],
    ],
  ]);
});

test('candidate registry cleanup is safe, idempotent, and loud on real failure', () => {
  let invoked = false;
  const never = () => {
    invoked = true;
    throw new Error('runner must not be called');
  };
  assert.equal(stopLoopbackRegistry('', never), false);
  assert.equal(invoked, false);
  assert.throws(
    () => stopLoopbackRegistry('unrelated-container', never),
    /refusing to remove invalid candidate registry/,
  );
  assert.equal(invoked, false);

  assert.equal(
    stopLoopbackRegistry('cerberus-migration-candidate-registry-123', () => ({
      status: 1,
      stdout: '',
      stderr: 'No such container',
    })),
    false,
  );
  assert.throws(
    () =>
      stopLoopbackRegistry('cerberus-migration-candidate-registry-123', () => ({
        status: 1,
        stdout: '',
        stderr: 'permission denied',
      })),
    /could not be removed/,
  );
});
