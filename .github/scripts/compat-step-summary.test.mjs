// compat-step-summary.test.mjs — node:test guard for the compatibility
// lanes' shared housekeeping step: the raw-report tally the prometheus lanes
// log (once an inline `jq` copied three times) and the one-row parity table
// every head appends to the step summary.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { reportTally, summaryTable } from './compat-step-summary.mjs';

const HERE = dirname(fileURLToPath(import.meta.url));
const CLI = join(HERE, 'compat-step-summary.mjs');

test('reportTally reads the tester\'s empty-string-means-clean encoding, and null the same way', () => {
  const report = {
    results: [
      { query: 'a', diff: '', unexpectedFailure: '' },
      { query: 'b', diff: null, unexpectedFailure: null },
      { query: 'c', diff: 'values differ', unexpectedFailure: '' },
      { query: 'd', diff: '', unexpectedFailure: 'context deadline exceeded' },
      { query: 'e', diff: 'x', unexpectedFailure: 'y' },
      { query: 'f' },
    ],
  };
  assert.deepEqual(reportTally(report), { total: 6, passed: 3, diffs: 2, unexpected_failures: 2 });
  assert.deepEqual(reportTally({}), { total: 0, passed: 0, diffs: 0, unexpected_failures: 0 });
  assert.deepEqual(reportTally(null), { total: 0, passed: 0, diffs: 0, unexpected_failures: 0 });
});

test('summaryTable renders the heading and the one row', () => {
  assert.equal(
    summaryTable({ title: 'compatibility/prometheus-floor (ClickHouse 24.8)', label: 'prometheus (floor)', passed: 9, total: 10, percent: 90 }),
    [
      '### compatibility/prometheus-floor (ClickHouse 24.8)',
      '',
      '| head | passed/total | percent |',
      '|------|--------------|---------|',
      '| prometheus (floor) | 9/10 | 90% |',
      '',
    ].join('\n'),
  );
});

test('the CLI appends the table with HEAD as the default title and label, and never exits non-zero', () => {
  const dir = mkdtempSync(join(tmpdir(), 'compat-summary-'));
  try {
    const score = join(dir, 'compat-score.json');
    writeFileSync(score, JSON.stringify({ percent: 100, passed: 4, total: 4 }));
    const report = join(dir, 'report.json');
    writeFileSync(report, JSON.stringify({ results: [{ diff: '', unexpectedFailure: '' }, { diff: 'd' }] }));
    const summary = join(dir, 'summary.md');
    const run = (env) =>
      spawnSync(process.execPath, [CLI], { encoding: 'utf8', env: { ...process.env, GITHUB_STEP_SUMMARY: summary, ...env } });

    let res = run({ HEAD: 'loki', SCORE: score });
    assert.equal(res.status, 0, res.stderr);
    assert.match(readFileSync(summary, 'utf8'), /### compatibility\/loki\n\n\| head \| passed\/total \| percent \|\n\|[-|]+\n\| loki \| 4\/4 \| 100% \|/);

    res = run({ HEAD: 'prometheus', SCORE: score, REPORT: report, TITLE: 'compatibility/prometheus-forced-route (CERBERUS_EVAL_ROUTE=sharded)', LABEL: 'prometheus (route B)' });
    assert.equal(res.status, 0, res.stderr);
    assert.match(res.stdout, /"total": 2/);
    assert.match(res.stdout, /"diffs": 1/);
    assert.match(readFileSync(summary, 'utf8'), /### compatibility\/prometheus-forced-route \(CERBERUS_EVAL_ROUTE=sharded\)[\s\S]*\| prometheus \(route B\) \| 4\/4 \| 100% \|/);

    res = run({ HEAD: 'tempo', SCORE: join(dir, 'missing.json'), REPORT: join(dir, 'missing-report.json') });
    assert.equal(res.status, 0);
    assert.match(res.stdout, /no report produced/);
    assert.match(res.stdout, /no compat-score\.json produced/);

    res = run({});
    assert.equal(res.status, 0, 'a wiring slip is logged, never a red housekeeping step');
    assert.match(res.stdout + res.stderr, /HEAD and SCORE env vars are required/);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
