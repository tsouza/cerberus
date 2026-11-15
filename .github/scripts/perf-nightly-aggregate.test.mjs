// perf-nightly-aggregate.test.mjs — node:test guard for the perf-nightly
// aggregate verdict, its wall-time report, and the wiring of a
// recalibration's combine. The combine's arithmetic (calibratedBound) is
// asserted by test/perf/nightly/calibration_test.go in the unit lane.

import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import process from 'node:process';
import test from 'node:test';

import {
  PERF_NIGHTLY_CALIBRATE_JOB,
  PERF_NIGHTLY_JOB,
  PERF_NIGHTLY_WORKFLOW,
  readCalibrationRunners,
  readClickHouseMatrix,
} from './lib/perf-nightly-matrix.mjs';
import {
  calibrationFailures,
  COMBINE_PACKAGE,
  combineArgs,
  combineRounds,
  formatDuration,
  missingLegs,
  roundsFiles,
  verdict,
  wallTimeTable,
} from './perf-nightly-aggregate.mjs';

test('verdict gates on the sentinel legs and the replays, and on nothing else', () => {
  assert.deepEqual(verdict({ sentinels: 'success', replays: 'success', calibration: 'skipped' }), []);
  for (const bad of ['failure', 'cancelled', 'skipped', '', undefined]) {
    assert.equal(verdict({ sentinels: bad, replays: 'success' }).length, 1, `sentinels ${bad}`);
    assert.equal(verdict({ sentinels: 'success', replays: bad }).length, 1, `replays ${bad}`);
  }
  assert.equal(verdict({ sentinels: 'failure', replays: 'failure' }).length, 2);
});

test('a recalibration dispatch gates on the calibration legs instead', () => {
  assert.deepEqual(verdict({ sentinels: 'skipped', replays: 'skipped', calibration: 'success', calibrate: true }), []);
  for (const bad of ['failure', 'cancelled', 'skipped', '', undefined]) {
    assert.equal(verdict({ sentinels: 'skipped', replays: 'skipped', calibration: bad, calibrate: true }).length, 1, `calibration ${bad}`);
  }
  assert.equal(verdict({ sentinels: 'success', replays: 'success', calibration: 'failure', calibrate: false }).length, 0);
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

test('combineArgs runs the combine over every rounds file into out', () => {
  assert.deepEqual(combineArgs(['a.json', 'b.json'], '/tmp/out.json'), [
    'run', COMBINE_PACKAGE, '-out', '/tmp/out.json', 'a.json', 'b.json',
  ]);
});

test('a recalibration fails on a missing leg, on no rounds, on a refusal, and on a combine that wrote nothing', () => {
  assert.deepEqual(calibrationFailures({ files: ['a.json'], missing: [], status: 0, written: true }), []);
  assert.equal(calibrationFailures({ files: [], missing: [], status: 0, written: false }).length, 1);
  assert.match(calibrationFailures({ files: ['a.json'], missing: [], status: 1, written: true })[0], /refused/);
  assert.match(calibrationFailures({ files: ['a.json'], missing: [], status: 1, written: false })[0], /wrote no bound file/);
  assert.deepEqual(calibrationFailures({ files: ['a.json'], missing: ['nightly-rounds-24.8-2'], status: 0, written: true }), [
    'calibration: no nightly-rounds-24.8-2 rounds were uploaded',
  ]);
});

test('missingLegs names every server-runner leg with no rounds file', () => {
  const files = ['/d/nightly-rounds-24.8-1/r.json', '/d/nightly-rounds-24.8-2/r.json', '/d/nightly-rounds-26.9-2/r.json'];
  assert.deepEqual(missingLegs(files, ['24.8', '26.9', '25.9'], [1, 2]), [
    'nightly-rounds-26.9-1',
    'nightly-rounds-25.9-1',
    'nightly-rounds-25.9-2',
  ]);
  assert.deepEqual(missingLegs(files.slice(0, 2), ['24.8'], [1, 2]), []);
});

// combineRounds over a two-server, two-runner matrix, with the combine
// replaced: what the job uploads (`written`) and fails on follows what the
// combine exited with and left on disk.
test('combineRounds sets written from the file the combine left, and fails on its exit and on missing legs', () => {
  const workflowText =
    `jobs:\n  ${PERF_NIGHTLY_CALIBRATE_JOB}:\n    strategy:\n      matrix:\n        clickhouse: ['24.8', '26.9']\n` +
    '        runner: [1, 2]\n  perf-nightly:\n    steps: []\n';
  const dir = mkdtempSync(join(tmpdir(), 'nightly-combine-'));
  try {
    const roundsDir = join(dir, 'rounds');
    for (const leg of ['nightly-rounds-24.8-1', 'nightly-rounds-24.8-2', 'nightly-rounds-26.9-1', 'nightly-rounds-26.9-2']) {
      mkdirSync(join(roundsDir, leg), { recursive: true });
      writeFileSync(join(roundsDir, leg, 'perf-nightly-rounds.json'), '{}');
    }
    const out = join(dir, 'nightly-baseline.json');
    const env = { ROUNDS_DIR: roundsDir, BASELINE_OUT: out };
    const outcome = (combine) => {
      const outputs = {};
      writeFileSync(out, 'stale'); // a file the combine did not write is never uploaded
      const failures = combineRounds(env, { workflowText, combine, output: (k, v) => (outputs[k] = v) });
      return { failures, written: outputs.written };
    };
    const writes = (status) => (files, path) => {
      assert.equal(files.length, 4);
      writeFileSync(path, '{}');
      return status;
    };
    assert.deepEqual(outcome(writes(0)), { failures: [], written: 'true' });
    const refused = outcome(writes(1));
    assert.equal(refused.written, 'true');
    assert.match(refused.failures.join('\n'), /refused/);
    const nothing = outcome(() => 1);
    assert.equal(nothing.written, 'false');
    assert.match(nothing.failures.join('\n'), /wrote no bound file/);

    rmSync(join(roundsDir, 'nightly-rounds-26.9-2'), { recursive: true });
    const short = outcome((files, path) => {
      writeFileSync(path, '{}');
      return 0;
    });
    assert.deepEqual(short, { failures: ['calibration: no nightly-rounds-26.9-2 rounds were uploaded'], written: 'true' });
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
  assert.match(combineRounds({ BASELINE_OUT: '/tmp/x.json' }, { workflowText: 'jobs: {}\n' })[0], /no calibrate matrix/);
});

// The workflow's calibrate fan-out is held to the runner count the
// derivation in calibration.go and docs/performance.background.md justifies:
// with fewer legs every bound is refused.
test('the calibration matrix runs nightlyCalibrationRunners legs per sentinel server, each uploading its own rounds', () => {
  const workflow = readFileSync(PERF_NIGHTLY_WORKFLOW, 'utf8');
  const source = readFileSync('test/perf/nightly/calibration.go', 'utf8');
  const m = /\nconst nightlyCalibrationRunners = (\d+)\n/.exec(source);
  assert.ok(m, 'calibration.go declares no `const nightlyCalibrationRunners = <n>`');
  const runners = Number(m[1]);
  assert.ok(runners >= 2, `nightlyCalibrationRunners is ${runners}; a calibration across runners needs at least 2`);
  assert.deepEqual(
    readCalibrationRunners(workflow),
    Array.from({ length: runners }, (_, i) => i + 1),
    `${PERF_NIGHTLY_WORKFLOW}'s ${PERF_NIGHTLY_CALIBRATE_JOB} runner list is not 1..${runners}`,
  );
  assert.deepEqual(readClickHouseMatrix(workflow, PERF_NIGHTLY_CALIBRATE_JOB), readClickHouseMatrix(workflow, PERF_NIGHTLY_JOB));
  assert.match(workflow, /name: nightly-rounds-\$\{\{ matrix\.clickhouse \}\}-\$\{\{ matrix\.runner \}\}\n/);
  assert.match(workflow, /pattern: nightly-rounds-\*\n/);
});

test('readCalibrationRunners reads the calibration job\'s runner list, and only its', () => {
  const job = (name, line) => `jobs:\n  ${name}:\n    strategy:\n      matrix:\n${line}\n  perf-nightly:\n    steps: []\n`;
  assert.deepEqual(readCalibrationRunners(job(PERF_NIGHTLY_CALIBRATE_JOB, '        runner: [1, 2, 3]')), [1, 2, 3]);
  assert.equal(readCalibrationRunners(job(PERF_NIGHTLY_JOB, '        runner: [1, 2, 3]')), null);
  assert.equal(readCalibrationRunners(job(PERF_NIGHTLY_CALIBRATE_JOB, "        clickhouse: ['24.8']")), null);
});

// The script itself: a recalibration whose combine cannot run fails the
// verdict even when every calibration leg succeeded.
test('a recalibration dispatch runs the combine into the verdict', () => {
  const run = (env) =>
    spawnSync(process.execPath, ['.github/scripts/perf-nightly-aggregate.mjs'], {
      encoding: 'utf8',
      env: {
        PATH: process.env.PATH,
        SENTINELS_RESULT: 'success',
        REPLAYS_RESULT: 'skipped',
        GITHUB_API_URL: 'http://127.0.0.1:1',
        REPO: 'o/r',
        RUN_ID: '1',
        RUN_ATTEMPT: '1',
        ...env,
      },
    });
  const calibrating = run({ CALIBRATE: 'true', SENTINELS_RESULT: 'skipped', CALIBRATE_RESULT: 'success' });
  assert.equal(calibrating.status, 1, calibrating.stdout + calibrating.stderr);
  assert.match(calibrating.stdout + calibrating.stderr, /BASELINE_OUT names no file/);
  assert.equal(run({ CALIBRATE: 'false', REPLAYS_RESULT: 'success' }).status, 0);
});
