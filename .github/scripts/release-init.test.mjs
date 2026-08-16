import assert from 'node:assert/strict';
import { test } from 'node:test';

import {
  ReleaseInitError,
  createReleaseIdentity,
  releaseEntryPolicy,
} from './release-init.mjs';

const sha = 'a'.repeat(40);
const tree = 'b'.repeat(40);
const nonce = 'c'.repeat(64);

test('release identity binds source, run, fresh nonce, start, and dry-run policy', () => {
  assert.deepEqual(
    createReleaseIdentity({
      sha,
      tree,
      runID: '12',
      runAttempt: '2',
      dryRun: 'true',
      startedAtMs: 1234,
      correlationNonce: nonce,
    }),
    {
      source_sha: sha,
      source_tree: tree,
      started_at_ms: '1234',
      correlation_nonce: nonce,
      run_id: '12',
      run_attempt: '2',
      dry_run: 'true',
    },
  );
});

test('release identity rejects every ambiguous or stale-looking input', () => {
  const baseline = {
    sha,
    tree,
    runID: '12',
    runAttempt: '2',
    dryRun: 'false',
    startedAtMs: 1234,
    correlationNonce: nonce,
  };
  for (const mutate of [
    { sha: 'short' },
    { tree: 'short' },
    { runID: '0' },
    { runAttempt: '-1' },
    { dryRun: '' },
    { dryRun: 'FALSE' },
    { startedAtMs: 0 },
    { correlationNonce: '0'.repeat(63) },
  ]) {
    assert.throws(() => createReleaseIdentity({ ...baseline, ...mutate }), ReleaseInitError);
  }
});

test('manual qualification is a dry run bound to one exact main commit', () => {
  assert.deepEqual(
    releaseEntryPolicy({
      eventName: 'workflow_dispatch',
      ref: 'refs/heads/main',
      sha,
      requestedSHA: sha,
    }),
    { dryRun: 'true', requestedSHA: sha },
  );
  for (const invalid of [
    { ref: 'refs/heads/feature' },
    { requestedSHA: '' },
    { requestedSHA: 'c'.repeat(40) },
  ]) {
    assert.throws(
      () =>
        releaseEntryPolicy({
          eventName: 'workflow_dispatch',
          ref: 'refs/heads/main',
          sha,
          requestedSHA: sha,
          ...invalid,
        }),
      ReleaseInitError,
    );
  }
});

test('only configured push refs can carry a non-dry-run publication intent', () => {
  for (const ref of ['refs/heads/main', 'refs/heads/release/1.2.x']) {
    assert.deepEqual(
      releaseEntryPolicy({ eventName: 'push', ref, sha, requestedSHA: '' }),
      { dryRun: 'false', requestedSHA: null },
    );
  }
  for (const invalid of [
    { eventName: 'pull_request', ref: 'refs/pull/1/merge' },
    { eventName: 'push', ref: 'refs/heads/feature' },
    { eventName: 'push', ref: 'refs/heads/main', requestedSHA: sha },
  ]) {
    assert.throws(
      () => releaseEntryPolicy({ sha, requestedSHA: '', ...invalid }),
      ReleaseInitError,
    );
  }
});
