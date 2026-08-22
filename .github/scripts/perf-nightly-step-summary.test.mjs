// perf-nightly-step-summary.test.mjs — the failure this script exists to
// prevent is a real regression getting buried in a wall of expected WARN
// log noise, and a correctly-rejected sentinel getting misread as a real
// failure. The load-bearing cases here are exactly those two
// discriminations, plus the escape hatches (missing/unparseable results
// file) a housekeeping step must never fail the job over.

import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';

import { formatBytes, formatPct, renderSummary, verdictCell } from './perf-nightly-step-summary.mjs';

const SCRIPT = fileURLToPath(new URL('./perf-nightly-step-summary.mjs', import.meta.url));

function sentinel(overrides = {}) {
  return {
    name: 'classic_histogram_quantile_by_route',
    family: 'histogram_quantile over a classic histogram',
    expected_status: 200,
    actual_status: 200,
    status_ok: true,
    max_of_n_bytes: 9_000_000,
    cap_ceiling_bytes: 912_680_550,
    cap_fraction_pct: 0.8,
    cap_ok: true,
    has_baseline: true,
    baseline_ceiling_bytes: 20_000_000,
    baseline_ok: true,
    pass: true,
    rejected: false,
    ...overrides,
  };
}

test('formatBytes / formatPct render human units and a placeholder for missing data', () => {
  assert.equal(formatBytes(2 * 1024 * 1024), '2.0 MiB');
  assert.equal(formatBytes(undefined), '—');
  assert.equal(formatPct(16.3), '16.3%');
  assert.equal(formatPct(null), '—');
});

test('verdictCell marks an expected-rejection PASS distinctly from a plain PASS', () => {
  const ok200 = sentinel();
  assert.equal(verdictCell(ok200), '✅ pass');

  const ok422 = sentinel({ expected_status: 422, actual_status: 422, rejected: true, pass: true });
  assert.equal(verdictCell(ok422), '✅ correctly rejected (expected)');
});

test('verdictCell names the SPECIFIC failure reason for a real regression', () => {
  const statusFail = sentinel({ status_ok: false, actual_status: 500, pass: false });
  assert.match(verdictCell(statusFail), /FAIL — status 500 \(want 200\)/);

  const capFail = sentinel({ cap_ok: false, pass: false });
  assert.match(verdictCell(capFail), /exceeds absolute cap-relative ceiling/);

  const baselineFail = sentinel({ baseline_ok: false, pass: false });
  assert.match(verdictCell(baselineFail), /exceeds committed baseline ceiling/);
});

test('renderSummary headlines a clean pass and a real regression differently', () => {
  const clean = renderSummary({ all_pass: true, sentinels: [sentinel(), sentinel({ name: 'gauge' })] });
  assert.match(clean, /all 2 sentinels passed/);

  const regressed = renderSummary({
    all_pass: false,
    sentinels: [sentinel(), sentinel({ name: 'gauge', pass: false, cap_ok: false })],
  });
  assert.match(regressed, /1 of 2 sentinel\(s\) regressed/);
  assert.match(regressed, /gauge.*exceeds absolute cap-relative ceiling/s);
});

test('renderSummary never re-derives pass/fail — a pass=true row always renders as passing', () => {
  // Even a row whose supporting fields look inconsistent (e.g. cap_ok false
  // but pass true, which Go itself would never produce) must still render
  // via the `pass` field alone — this script is a renderer, not a second
  // opinion.
  const summary = renderSummary({
    all_pass: true,
    sentinels: [sentinel({ cap_ok: false, pass: true })],
  });
  assert.match(summary, /✅ pass/);
  assert.doesNotMatch(summary, /❌/);
});

test('renderSummary labels a calibration-run sentinel (no committed baseline) explicitly', () => {
  const summary = renderSummary({
    all_pass: true,
    sentinels: [sentinel({ has_baseline: false, baseline_ceiling_bytes: undefined })],
  });
  assert.match(summary, /n\/a \(calibration run\)/);
});

// --- end-to-end script invocation (env contract + escape hatches) --------

function run(env = {}) {
  return spawnSync(process.execPath, [SCRIPT], {
    encoding: 'utf8',
    env: { ...process.env, GITHUB_STEP_SUMMARY: '', ...env },
  });
}

test('missing PERF_NIGHTLY_RESULTS_JSON is a no-op, not a failure', () => {
  const res = run({ PERF_NIGHTLY_RESULTS_JSON: '' });
  assert.equal(res.status, 0);
});

test('a missing results file prints a notice and still exits 0 — never fails the job', () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'perf-nightly-summary-'));
  const missing = path.join(dir, 'does-not-exist.json');
  const summaryFile = path.join(dir, 'step-summary.md');
  writeFileSync(summaryFile, '');
  try {
    const res = run({ PERF_NIGHTLY_RESULTS_JSON: missing, GITHUB_STEP_SUMMARY: summaryFile });
    assert.equal(res.status, 0);
    const summary = readFileSync(summaryFile, 'utf8');
    assert.match(summary, /no results were produced/);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('a real results file renders the verdict table into $GITHUB_STEP_SUMMARY', () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'perf-nightly-summary-'));
  const resultsFile = path.join(dir, 'results.json');
  const summaryFile = path.join(dir, 'step-summary.md');
  writeFileSync(
    resultsFile,
    JSON.stringify({
      all_pass: true,
      sentinels: [sentinel(), sentinel({ name: 'request_rate_by_method', expected_status: 422, actual_status: 422, rejected: true })],
    }),
  );
  writeFileSync(summaryFile, '');
  try {
    const res = run({ PERF_NIGHTLY_RESULTS_JSON: resultsFile, GITHUB_STEP_SUMMARY: summaryFile });
    assert.equal(res.status, 0);
    const summary = readFileSync(summaryFile, 'utf8');
    assert.match(summary, /all 2 sentinels passed/);
    assert.match(summary, /request_rate_by_method/);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
