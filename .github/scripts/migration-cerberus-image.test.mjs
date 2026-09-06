// migration-cerberus-image.test.mjs — node:test guard for
// migration-cerberus-image.mjs's pure build-vs-pull decision (issue #3099).
// The real Docker build/pull is exercised live by `just migration-tier1` /
// `migration-tier2` in the real migration-e2e.yml CI job.

import assert from 'node:assert/strict';
import test from 'node:test';

import { resolveCerberusImage } from './migration-cerberus-image.mjs';

test('an unset CERBERUS_IMAGE selects the build path against the local tag', () => {
  assert.deepEqual(resolveCerberusImage('', 'cerberus:migration-tier1'), {
    image: 'cerberus:migration-tier1',
    source: 'build',
  });
});

test('a CERBERUS_IMAGE set to a released tag selects the pull path', () => {
  assert.deepEqual(resolveCerberusImage('ghcr.io/tsouza/cerberus:v1.13.0', 'cerberus:migration-tier1'), {
    image: 'ghcr.io/tsouza/cerberus:v1.13.0',
    source: 'pull',
  });
});

test('CERBERUS_IMAGE set to exactly the local tag still selects build — matches the bash string-equality test, not an is-it-set check', () => {
  assert.deepEqual(resolveCerberusImage('cerberus:migration-tier1', 'cerberus:migration-tier1'), {
    image: 'cerberus:migration-tier1',
    source: 'build',
  });
});

test('a distinct local tag (COMPOSE_PROJECT_SUFFIX applied) still resolves against ITS OWN local tag', () => {
  assert.deepEqual(resolveCerberusImage('', 'cerberus:migration-tier1-abcd1234'), {
    image: 'cerberus:migration-tier1-abcd1234',
    source: 'build',
  });
});
