// e2e-chaos-overlay.test.mjs — node:test guard for `parseEnvFile`, the
// exact blank/comment-skipping KEY=VALUE extraction the extracted bash's
// `while read` + `case ''|\#*` loop performed (cerberus issue #3096). The
// `kubectl set env` + rollout-wait around it is exercised live by
// `just e2e-chaos-overlay` in the real `e2e` CI job's `chaos` job.

import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import test from 'node:test';

import { parseEnvFile } from './e2e-chaos-overlay.mjs';

// Resolved relative to this test file's own location (never cwd), so the
// fixture read is independent of where `node --test` happens to be invoked
// from.
const chaosOverlayEnvPath = fileURLToPath(
  new URL('../../test/e2e/chaos/manifests/chaos-overlay.env', import.meta.url),
);

test('parseEnvFile skips comment and blank lines, keeps KEY=VALUE lines in order', () => {
  const text = [
    '# a leading comment',
    '',
    'CERBERUS_QUERY_TIMEOUT=5s',
    '  ', // whitespace-only line
    '# another comment',
    'CERBERUS_ADMIT_PROM=2',
    'CERBERUS_ADMIT_LOKI=2',
  ].join('\n');
  assert.deepEqual(parseEnvFile(text), [
    'CERBERUS_QUERY_TIMEOUT=5s',
    'CERBERUS_ADMIT_PROM=2',
    'CERBERUS_ADMIT_LOKI=2',
  ]);
});

test('parseEnvFile against the real chaos-overlay.env fixture', () => {
  const text = readFileSync(chaosOverlayEnvPath, 'utf8');
  const args = parseEnvFile(text);
  assert.ok(args.includes('CERBERUS_QUERY_TIMEOUT=5s'));
  assert.ok(args.includes('CERBERUS_SHARD_MIN_FANOUT=1000000'));
  // Every entry is a real KEY=VALUE token, never a comment fragment.
  for (const a of args) {
    assert.match(a, /^[A-Z0-9_]+=\S*$/);
  }
});

test('parseEnvFile returns empty for an all-comment file', () => {
  assert.deepEqual(parseEnvFile('# nothing here\n# still nothing\n'), []);
});
