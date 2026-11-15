// perf-nightly-aggregate.test.mjs — node:test guard for the perf-nightly
// aggregate verdict, its wall-time report, and the wiring of a
// recalibration's combine. The combine's arithmetic (calibratedBound) is
// asserted by test/perf/nightly/calibration_test.go in the unit lane.

import assert from 'node:assert/strict';
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';

import { PERF_NIGHTLY_JOB, PERF_NIGHTLY_WORKFLOW, readCalibrationRunners } from './lib/perf-nightly-matrix.mjs';
import {
  calibrationFailures,
  COMBINE_PACKAGE,
  COMMITTED_BASELINE,
  combineArgs,
  formatDuration,
  roundsFiles,
  verdict,
  wallTimeTable,
} from './perf-nightly-aggregate.mjs';

test('verdict passes only when the sentinel legs and the replays both succeeded', () => {
  assert.deepEqual(verdict({ sentinels: 'success', replays: 'success' }), []);
  for (const bad of ['failure', 'cancelled', 'skipped', '', undefined]) {
    assert.equal(verdict({ sentinels: bad, replays: 'success' }).length, 1, `sentinels ${bad}`);
    assert.equal(verdict({ sentinels: 'success', replays: bad }).length, 1, `replays ${bad}`);
  }
  assert.equal(verdict({ sentinels: 'failure', replays: 'failure' }).length, 2);
});

test('a recalibration dispatch skips the replays, and only it may', () => {
  assert.deepEqual(verdict({ sentinels: 'success', replays: 'skipped', calibrate: true }), []);
  assert.equal(verdict({ sentinels: 'success', replays: 'skipped', calibrate: false }).length, 1);
  assert.equal(verdict({ sentinels: 'failure', replays: 'skipped', calibrate: true }).length, 1);
  assert.equal(verdict({ sentinels: 'success', replays: 'failure', calibrate: true }).length, 1);
});

test('formatDuration renders minutes and seconds', () => {
  assert.equal(formatDuration(0), '0m 0s');
  assert.equal(formatDuration(59.6), '1m 0s');
  assert.equal(formatDuration(754), '12m 34s');
});

test('wallTimeTable lists each finished perf-nightly job by name', () => {
  const jobs = [
    { name: 'perf-nightly-sentinels (26.6)', conclusion: 'success', started_at: '2026-10-22T05:00:00Z', completed_at: '2026-10-22T05:12:30Z' },
    { name: 'perf-nightly-sentinels (24.8)', conclusion: 'failure', started_at: '2026-10-22T05:00:00Z', completed_at: '2026-10-22T05:10:05Z' },
    { name: 'perf-nightly', conclusion: null, started_at: '2026-10-22T05:13:00Z', completed_at: null },
    { name: 'unrelated', conclusion: 'success', started_at: '2026-10-22T05:00:00Z', completed_at: '2026-10-22T05:01:00Z' },
  ];
  const table = wallTimeTable(jobs);
  const rows = table.split('\n').filter((l) => l.startsWith('| perf-nightly'));
  assert.deepEqual(rows, [
    '| perf-nightly-sentinels (24.8) | failure | 10m 5s |',
    '| perf-nightly-sentinels (26.6) | success | 12m 30s |',
  ]);
});

test('roundsFiles finds every leg\'s rounds file under the download directory, sorted', () => {
  const dir = mkdtempSync(join(tmpdir(), 'nightly-rounds-'));
  try {
    for (const leg of ['nightly-rounds-26.9-2', 'nightly-rounds-24.8-1', 'nightly-rounds-26.9-1']) {
      mkdirSync(join(dir, leg));
      writeFileSync(join(dir, leg, 'perf-nightly-rounds.json'), '{}');
    }
    writeFileSync(join(dir, 'nightly-rounds-26.9-1', 'notes.txt'), '');
    assert.deepEqual(roundsFiles(dir), [
      join(dir, 'nightly-rounds-24.8-1', 'perf-nightly-rounds.json'),
      join(dir, 'nightly-rounds-26.9-1', 'perf-nightly-rounds.json'),
      join(dir, 'nightly-rounds-26.9-2', 'perf-nightly-rounds.json'),
    ]);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
  assert.deepEqual(roundsFiles(join(tmpdir(), 'no-such-nightly-rounds-dir')), []);
  assert.deepEqual(roundsFiles(undefined), []);
});

test('combineArgs runs the combine over the committed file and every rounds file', () => {
  assert.deepEqual(combineArgs(['a.json', 'b.json'], '/tmp/out.json'), [
    'run', COMBINE_PACKAGE, '-baseline', COMMITTED_BASELINE, '-out', '/tmp/out.json', 'a.json', 'b.json',
  ]);
});

test('a recalibration fails on no rounds, on a refusal, and on a combine that wrote nothing', () => {
  assert.deepEqual(calibrationFailures({ files: ['a.json'], status: 0, written: true }), []);
  assert.equal(calibrationFailures({ files: [], status: 0, written: false }).length, 1);
  assert.match(calibrationFailures({ files: ['a.json'], status: 1, written: true })[0], /refused/);
  assert.match(calibrationFailures({ files: ['a.json'], status: 1, written: false })[0], /wrote no bound file/);
});

// The workflow's calibrate fan-out is the runner count the derivation
// requires: fewer legs and every bound is refused, more and the extra legs
// buy nothing the constant was justified by.
test('the calibrate matrix runs nightlyCalibrationRunners legs per server, each uploading its own rounds', () => {
  const workflow = readFileSync(PERF_NIGHTLY_WORKFLOW, 'utf8');
  const source = readFileSync('test/perf/nightly/calibration.go', 'utf8');
  const m = /\nconst nightlyCalibrationRunners = (\d+)\n/.exec(source);
  assert.ok(m, 'calibration.go declares no `const nightlyCalibrationRunners = <n>`');
  const runners = Number(m[1]);
  assert.ok(runners >= 2, `nightlyCalibrationRunners is ${runners}; a calibration across runners needs at least 2`);
  assert.deepEqual(
    readCalibrationRunners(workflow),
    Array.from({ length: runners }, (_, i) => i + 1),
    `${PERF_NIGHTLY_WORKFLOW}'s ${PERF_NIGHTLY_JOB} calibrate runner list is not 1..${runners}`,
  );
  assert.match(workflow, /name: nightly-rounds-\$\{\{ matrix\.clickhouse \}\}-\$\{\{ matrix\.runner \}\}\n/);
  assert.match(workflow, /pattern: nightly-rounds-\*\n/);
});

test('readCalibrationRunners reads only the gate-once, calibrate-many runner line', () => {
  const job = (line) => `jobs:\n  ${PERF_NIGHTLY_JOB}:\n    strategy:\n      matrix:\n${line}\n  perf-nightly:\n    steps: []\n`;
  assert.deepEqual(readCalibrationRunners(job("        runner: ${{ fromJSON(inputs.calibrate && '[1, 2, 3]' || '[1]') }}")), [1, 2, 3]);
  assert.equal(readCalibrationRunners(job("        runner: ${{ fromJSON(inputs.calibrate && '[1, 2]' || '[1, 2]') }}")), null);
  assert.equal(readCalibrationRunners(job('        runner: [1, 2]')), null);
  assert.equal(readCalibrationRunners('jobs:\n  other:\n    steps: []\n'), null);
});
