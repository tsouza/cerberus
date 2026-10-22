// perf-nightly-aggregate.test.mjs — node:test guard for the perf-nightly
// aggregate verdict and its wall-time report.

import assert from 'node:assert/strict';
import test from 'node:test';

import { formatDuration, verdict, wallTimeTable } from './perf-nightly-aggregate.mjs';

test('verdict passes only when the sentinel legs and the replays both succeeded', () => {
  assert.deepEqual(verdict({ sentinels: 'success', replays: 'success' }), []);
  for (const bad of ['failure', 'cancelled', 'skipped', '', undefined]) {
    assert.equal(verdict({ sentinels: bad, replays: 'success' }).length, 1, `sentinels ${bad}`);
    assert.equal(verdict({ sentinels: 'success', replays: bad }).length, 1, `replays ${bad}`);
  }
  assert.equal(verdict({ sentinels: 'failure', replays: 'failure' }).length, 2);
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
