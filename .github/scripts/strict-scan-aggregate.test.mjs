// strict-scan-aggregate.test.mjs — node:test guard for the `strict-scan`
// roll-up. The verdict rule itself is pinned by lib/needs-verdict.test.mjs;
// this suite pins that the script applies it to the shard matrix and that its
// exit code follows the environment the workflow passes, in both directions.

import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import process from 'node:process';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';

import { aggregate, SHARD_JOB } from './strict-scan-aggregate.mjs';

const SCRIPT = fileURLToPath(new URL('./strict-scan-aggregate.mjs', import.meta.url));

function run(env) {
  const { CHANGES_RESULT, DOCS_ONLY, SHARDS_RESULT, ...rest } = process.env;
  return spawnSync(process.execPath, [SCRIPT], { env: { ...rest, ...env }, encoding: 'utf8' });
}

test('the shard job named is the workflow job the aggregate needs', () => {
  assert.equal(SHARD_JOB, 'strict-scan-shard');
});

test('a successful shard matrix passes; any other result fails naming the shard job', () => {
  assert.deepEqual(aggregate({ changes: 'success', docsOnly: 'false', shards: 'success' }), []);
  for (const bad of ['failure', 'cancelled', 'skipped', '']) {
    assert.deepEqual(
      aggregate({ changes: 'success', docsOnly: 'false', shards: bad }),
      [`strict-scan-shard did not succeed (${bad || 'no result'})`],
      `shards=${bad}`,
    );
  }
});

test('the script exits 0 on a green matrix and on a docs-only change', () => {
  assert.equal(run({ CHANGES_RESULT: 'success', DOCS_ONLY: 'false', SHARDS_RESULT: 'success' }).status, 0);
  assert.equal(run({ CHANGES_RESULT: 'success', DOCS_ONLY: 'true', SHARDS_RESULT: 'skipped' }).status, 0);
});

test('the script exits 1 on a failed, cancelled or skipped matrix, or with no environment', () => {
  for (const bad of ['failure', 'cancelled', 'skipped']) {
    const result = run({ CHANGES_RESULT: 'success', DOCS_ONLY: 'false', SHARDS_RESULT: bad });
    assert.equal(result.status, 1, `shards=${bad}`);
    assert.match(result.stdout, /strict-scan-shard did not succeed/);
  }
  assert.equal(run({}).status, 1);
});
