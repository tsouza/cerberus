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

// The per-mutant budget must be a DECLARED value, not an accident of build-cache
// warmth. gremlins computes `min(coefficient * max(coverage_elapsed, 1s),
// timeout-max)`, so unless the coefficient alone carries the whole ceiling
// across gremlins' own 1s floor, the hot-cache fallback invocation silently gets
// a smaller budget than the cold-cache first one.
test('both invocations pin the per-mutant budget to the declared ceiling', (t) => {
  const { root, calls } = fixture(t, [0, 7]);
  const result = run(root, { DIFF_REF: 'a'.repeat(40), MUTANT_TIMEOUT_MAX: '15s' });
  assert.equal(result.status, 0, result.stderr);
  const invocations = readFileSync(calls, 'utf8').trim().split('\n').map(JSON.parse);
  assert.equal(invocations.length, 2);
  for (const args of invocations) {
    assert.deepEqual(args.slice(args.indexOf('--timeout-max'), args.indexOf('--timeout-max') + 2), [
      '--timeout-max',
      '15s',
    ]);
    const at = args.indexOf('--timeout-coefficient');
    assert.notEqual(at, -1, `--timeout-coefficient missing from ${JSON.stringify(args)}`);
    // gremlins clamps its measured coverage time up to a 1s floor, so
    // `coefficient * 1s` is the smallest budget the coefficient can yield; it
    // must already reach the ceiling for the ceiling to be the sole budget.
    assert.ok(Number(args[at + 1]) * 1 >= 15, `coefficient ${args[at + 1]} is below the ceiling`);
  }
});

test('the declared coefficient rounds a compound ceiling up, and rejects a bad one', (t) => {
  const { root, calls } = fixture(t, [4]);
  let result = run(root, { DIFF_REF: '', MUTANT_TIMEOUT_MAX: '1m30s' });
  assert.equal(result.status, 0, result.stderr);
  const args = readFileSync(calls, 'utf8').trim().split('\n').map(JSON.parse)[0];
  assert.equal(args[args.indexOf('--timeout-coefficient') + 1], '90');

  const bad = fixture(t, [4]);
  result = run(bad.root, { DIFF_REF: '', MUTANT_TIMEOUT_MAX: '15 seconds' });
  assert.equal(result.status, 1);
  assert.equal(readFileSync(bad.calls, 'utf8'), '');
  assert.match(`${result.stdout}\n${result.stderr}`, /MUTANT_TIMEOUT_MAX is not a Go duration/);
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
