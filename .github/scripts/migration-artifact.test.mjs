// migration-artifact.test.mjs — node:test guard for the migration lane's
// artifact resolver.
//
// Runs on the CHEAP required `lint` lane (`node --test`) — no Docker, no
// network. migration-e2e.yml has no `pull_request:` trigger and release.yml
// has none either, so without this suite every edit to resolvePlan would be
// unverified until a release is cut.
//
// The headline case is the half-supplied input pair: a release lane wired with
// `cerberus_image` but no `expect_version` must FAIL, not quietly degrade to a
// from-source build under an artifact-shaped job name. Its negative control is
// the positive path — both inputs empty is the PR/dispatch shape that keeps the
// currently-green lane green.

import { test } from 'node:test';
import assert from 'node:assert/strict';

import {
  resolvePlan,
  SOURCE_BUILD_VERSION,
  LOCAL_IMAGE_TAG,
  IMAGE_BINARY_PATH,
  PAIRED_INPUTS_MESSAGE,
} from './migration-artifact.mjs';

const releaseImage = 'ghcr.io/tsouza/cerberus:1.11.2';
const releaseVersion = '1.11.2';

test('positive control: neither input supplied resolves the source plan', () => {
  const plan = resolvePlan({ image: '', expectVersion: '' });

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
  assert.deepEqual(resolvePlan({}), resolvePlan({ image: '', expectVersion: '' }));
  assert.deepEqual(resolvePlan(), resolvePlan({ image: '', expectVersion: '' }));
});

test('whitespace-only inputs count as absent, not as a half-supplied pair', () => {
  const plan = resolvePlan({ image: '   ', expectVersion: '\n' });
  assert.equal(plan.source, 'source');
});

test('an image without an expected version is refused', () => {
  assert.throws(
    () => resolvePlan({ image: releaseImage, expectVersion: '' }),
    (err) => {
      assert.equal(err.message, PAIRED_INPUTS_MESSAGE);
      return true;
    },
  );
});

test('an expected version without an image is refused', () => {
  assert.throws(
    () => resolvePlan({ image: '', expectVersion: releaseVersion }),
    (err) => {
      assert.equal(err.message, PAIRED_INPUTS_MESSAGE);
      return true;
    },
  );
});

test('mutation control: a half pair never coerces to the source default', () => {
  // The tempting "fix" for the half-supplied case is to fill the missing half
  // in from the source defaults. That would run a `go build` while the job name
  // and the notice both claim a released artifact — the exact hollow green this
  // module exists to prevent — so assert the refusal is a THROW and that no
  // plan carrying the source stamp escapes.
  let escaped = null;
  try {
    escaped = resolvePlan({ image: releaseImage, expectVersion: '' });
  } catch {
    escaped = null;
  }
  assert.equal(escaped, null, 'resolvePlan returned a plan for a half-supplied pair');

  let escapedInverse = null;
  try {
    escapedInverse = resolvePlan({ image: '', expectVersion: releaseVersion });
  } catch {
    escapedInverse = null;
  }
  assert.equal(escapedInverse, null, 'resolvePlan invented an image for a version-only input');
});

test('a full pair carries the ref and the version through verbatim', () => {
  const plan = resolvePlan({ image: releaseImage, expectVersion: releaseVersion });

  assert.equal(plan.source, 'image');
  assert.equal(plan.image, releaseImage);
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
