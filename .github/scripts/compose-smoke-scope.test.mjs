// Unit tests for the release-only event policy in compose-smoke-scope.mjs.
// The required quickstart context owns one-stack projected-trunk validation;
// this module keeps the exhaustive browser matrix off ordinary merges without
// weakening push, nightly, release-head, or explicit inventory qualification.

import assert from 'node:assert/strict';
import test from 'node:test';

import { NON_BOOTING_EVENTS, decide, regeneratesCompose } from './compose-smoke-scope.mjs';

test('every ordinary pull request omits the exhaustive Compose matrix', () => {
  for (const changed of [
    ['docker-compose.yml'],
    ['cmd/cerberus/main.go'],
    ['internal/api/health/health.go'],
    ['docs/engine.md'],
    null,
  ]) {
    const verdict = decide({ eventName: 'pull_request', headRef: 'feature/quickstart', changed });
    assert.equal(verdict.inScope, false, JSON.stringify(changed));
    assert.match(verdict.reason, /required one-stack quickstart/);
  }
});

test('release-head pull requests still run the full qualification matrix', () => {
  for (const headRef of ['release/1.14.x', 'release/2.0.x']) {
    const verdict = decide({ eventName: 'pull_request', headRef });
    assert.equal(verdict.inScope, true, headRef);
    assert.match(verdict.reason, /full release qualification/);
  }
});

test('push, schedule, and unfamiliar events run fail closed', () => {
  for (const eventName of ['push', 'schedule', 'future_event', '']) {
    assert.equal(decide({ eventName, headRef: '' }).inScope, true, eventName || '<blank>');
  }
});

test('merge queue does not duplicate the required projected-trunk canary', () => {
  const verdict = decide({ eventName: 'merge_group', headRef: '' });
  assert.equal(verdict.inScope, false);
  assert.match(verdict.reason, /projected-trunk/);
});

test('manual dispatch boots only for Compose inventory regeneration', () => {
  for (const selection of [undefined, '', 'none', 'k3d']) {
    assert.equal(
      decide({ eventName: 'workflow_dispatch', headRef: '', updateCrawlInventory: selection }).inScope,
      false,
      String(selection),
    );
  }
  for (const selection of ['compose', 'both']) {
    const verdict = decide({ eventName: 'workflow_dispatch', headRef: '', updateCrawlInventory: selection });
    assert.equal(verdict.inScope, true, selection);
    assert.match(verdict.reason, /inventory regen/);
  }
});

test('the explicit non-booting event set cannot drift into an allow-list', () => {
  assert.deepEqual([...NON_BOOTING_EVENTS], ['merge_group', 'workflow_dispatch']);
  assert.equal(regeneratesCompose('compose'), true);
  assert.equal(regeneratesCompose('both'), true);
  assert.equal(regeneratesCompose('none'), false);
  assert.equal(regeneratesCompose('k3d'), false);
});
