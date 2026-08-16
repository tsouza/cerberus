import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';

// The runner's process boundary is intentional: it must prove the exact env
// contract and two-invocation fallback, not a helper that bypasses main().
import { spawnSync } from 'node:child_process';

function fixture(t, totals) {
  const root = mkdtempSync(join(tmpdir(), 'mutation-run-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const calls = join(root, 'calls.jsonl');
  const fake = join(root, 'gremlins');
  writeFileSync(calls, '');
  writeFileSync(
    fake,
    `#!/usr/bin/env node
const fs = require('node:fs');
const args = process.argv.slice(2);
fs.appendFileSync(${JSON.stringify(calls)}, JSON.stringify(args) + '\\n');
const report = args[args.indexOf('--output') + 1];
const totals = ${JSON.stringify(totals)};
const call = fs.readFileSync(${JSON.stringify(calls)}, 'utf8').trim().split('\\n').length - 1;
fs.writeFileSync(report, JSON.stringify({ mutants_total: totals[call] }));
`,
    { mode: 0o755 },
  );
  return { root, calls, fake };
}

function run(root, env = {}) {
  return spawnSync(process.execPath, ['.github/scripts/mutation-run.mjs'], {
    cwd: process.cwd(),
    encoding: 'utf8',
    env: {
      ...process.env,
      PATH: `${root}:${process.env.PATH}`,
      SCOPE: './internal/chplan',
      REPORT: join(root, 'gremlins.json'),
      MUTANT_TIMEOUT_MAX: '15s',
      WORKERS: '0',
      EXCLUDE_FILES: '',
      ...env,
    },
  });
}

test('changed-line execution stays incremental when it executes mutants', (t) => {
  const { root, calls } = fixture(t, [3]);
  const result = run(root, { DIFF_REF: 'a'.repeat(40) });
  assert.equal(result.status, 0, result.stderr);
  const invocations = readFileSync(calls, 'utf8').trim().split('\n').map(JSON.parse);
  assert.equal(invocations.length, 1);
  assert.deepEqual(invocations[0].slice(-3), ['--diff', 'a'.repeat(40), './internal/chplan']);
});

test('zero changed-line mutants trigger one full fallback', (t) => {
  const { root, calls } = fixture(t, [0, 7]);
  const result = run(root, { DIFF_REF: 'a'.repeat(40) });
  assert.equal(result.status, 0, result.stderr);
  const invocations = readFileSync(calls, 'utf8').trim().split('\n').map(JSON.parse);
  assert.equal(invocations.length, 2);
  assert.equal(invocations[0].includes('--diff'), true);
  assert.equal(invocations[1].includes('--diff'), false);
});

test('zero mutants after full fallback fail closed', (t) => {
  const { root } = fixture(t, [0, 0]);
  const result = run(root, { DIFF_REF: 'a'.repeat(40) });
  assert.equal(result.status, 1);
  assert.match(`${result.stdout}\n${result.stderr}`, /executed zero mutants after full fallback/);
});

test('invalid diff refs and stale reports fail before gremlins runs', (t) => {
  const { root, calls } = fixture(t, [3]);
  let result = run(root, { DIFF_REF: 'main' });
  assert.equal(result.status, 1);
  assert.equal(readFileSync(calls, 'utf8'), '');

  writeFileSync(join(root, 'gremlins.json'), '{}');
  result = run(root, { DIFF_REF: '' });
  assert.equal(result.status, 1);
  assert.match(`${result.stdout}\n${result.stderr}`, /refusing stale gremlins report/);
});
