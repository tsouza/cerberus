// Unit + integration tests for forbid-parity-duplicate-samples.mjs.
//
// The integration cases drive the REAL script as a subprocess over a temp
// fixture tree, because the gate's whole value is in what it does to a corpus
// on disk: it must go RED on the shape it exists to forbid and stay GREEN on
// the three neighbouring shapes that look similar and are legitimate — an
// identical-value duplicate (increase_duplicate_timestamp_dedup.txtar's real
// shape), a differing-value duplicate in an UNENROLLED fixture, and a gauge
// row sharing a (name, labels, timestamp) with a classic-histogram decoy
// (pinned_name_selector_no_histogram_fanout.txtar's real shape). A gate that
// fires on any of those would delete good coverage, so each is a pinned
// negative control rather than an afterthought.
//
// The vacuity case is pinned too: a scan that parses no metric-shaped seed at
// all must FAIL rather than report the green a satisfied scan reports.

import assert from 'node:assert/strict';
import test from 'node:test';
import { spawnSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import {
  duplicateSamples,
  isMetricTable,
  normalizeCell,
  opaqueMetricInserts,
  parseInserts,
  parseTables,
  seriesNamespace,
  splitSections,
  splitTopLevel,
} from './forbid-parity-duplicate-samples.mjs';

const SCRIPT = join(dirname(fileURLToPath(import.meta.url)), 'forbid-parity-duplicate-samples.mjs');

const GAUGE_DDL = `CREATE TABLE otel_metrics_gauge (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
`;

const HISTOGRAM_DDL = `CREATE TABLE otel_metrics_histogram (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Count UInt64,
    Sum Float64,
    ExplicitBounds Array(Float64),
    BucketCounts Array(UInt64)
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
`;

const PARITY = '-- parity --\noracle: prometheus\nendpoint: /api/v1/query\nscope: full\n';

function fixture({ parity = true, seed }) {
  return `# header comment\n${parity ? PARITY : ''}-- query.promql --\nup\n-- seed --\n${seed}`;
}

function row(name, labels, ts, value) {
  return `    ('${name}', map(${labels}), toDateTime64('${ts}', 9), ${value})`;
}

function gaugeSeed(rows) {
  return `${GAUGE_DDL}INSERT INTO otel_metrics_gauge VALUES\n${rows.join(',\n')};\n`;
}

// newCorpus() writes a throwaway spec tree of `name -> fixture text` under
// test/spec/promql and returns its root.
function newCorpus(fixtures) {
  const dir = mkdtempSync(join(tmpdir(), 'forbid-parity-dup-samples-'));
  mkdirSync(join(dir, 'test', 'spec', 'promql'), { recursive: true });
  for (const [name, text] of Object.entries(fixtures)) {
    writeFileSync(join(dir, 'test', 'spec', 'promql', name), text);
  }
  return dir;
}

function runGate(root) {
  return spawnSync('node', [SCRIPT], {
    cwd: root,
    encoding: 'utf8',
    env: { ...process.env, REPO_ROOT: root, SPEC_DIRS: 'test/spec/promql' },
  });
}

test('splitSections keys each TXTAR section and drops the leading comment', () => {
  const sections = splitSections('# note\n-- parity --\noracle: prometheus\n-- seed --\nSELECT 1;\n');
  assert.deepEqual([...sections.keys()], ['parity', 'seed']);
  assert.equal(sections.get('parity'), 'oracle: prometheus');
  assert.equal(sections.get('seed'), 'SELECT 1;\n'.trimEnd() + '\n');
});

test('splitTopLevel ignores separators nested in calls, arrays and quotes', () => {
  const parts = splitTopLevel("'a,b', map('x', 'y'), [1, 2], 3");
  assert.deepEqual(
    parts.map((p) => p.trim()),
    ["'a,b'", "map('x', 'y')", '[1, 2]', '3'],
  );
});

test('parseTables reads column names and trims DEFAULT tails off the type', () => {
  const tables = parseTables(
    'CREATE TABLE t (\n  MetricName String,\n  ResourceAttributes Map(String, String) DEFAULT map(),\n' +
      '  TimeUnix DateTime64(9),\n  Value Float64\n) ENGINE = Memory;\n',
  );
  assert.deepEqual(tables.get('t'), [
    { name: 'MetricName', type: 'String' },
    { name: 'ResourceAttributes', type: 'Map(String, String)' },
    { name: 'TimeUnix', type: 'DateTime64(9)' },
    { name: 'Value', type: 'Float64' },
  ]);
});

test('parseInserts aligns a positional VALUES list against the DDL column order', () => {
  const seed = gaugeSeed([row('m', "'job', 'api'", '2026-01-01 00:00:00', '1.0')]);
  const rows = parseInserts(seed, parseTables(seed));
  assert.equal(rows.length, 1);
  assert.deepEqual(rows[0].columns, ['MetricName', 'Attributes', 'TimeUnix', 'Value']);
  assert.equal(rows[0].cells[3], '1.0');
});

test('parseInserts honours an explicit column list', () => {
  const seed = `${GAUGE_DDL}INSERT INTO otel_metrics_gauge (MetricName, TimeUnix, Value) VALUES\n` +
    "    ('m', toDateTime64('2026-01-01 00:00:00', 9), 1.0);\n";
  const rows = parseInserts(seed, parseTables(seed));
  assert.deepEqual(rows[0].columns, ['MetricName', 'TimeUnix', 'Value']);
  assert.equal(rows[0].cells.length, 3);
});

test('normalizeCell compares numbers numerically and datetimes by instant', () => {
  assert.equal(normalizeCell('40'), normalizeCell('40.0'));
  assert.notEqual(normalizeCell('40'), normalizeCell('41'));
  assert.equal(
    normalizeCell("toDateTime64('2026-01-01 00:00:00', 9)", { timestamp: true }),
    normalizeCell("toDateTime64( '2026-01-01 00:00:00' ,9)", { timestamp: true }),
  );
  assert.equal(normalizeCell("map('job',  'api')"), "map('job', 'api')");
});

test('isMetricTable and seriesNamespace read the shape off the DDL', () => {
  const gauge = parseTables(GAUGE_DDL).get('otel_metrics_gauge');
  const histogram = parseTables(HISTOGRAM_DDL).get('otel_metrics_histogram');
  const logs = parseTables(
    'CREATE TABLE otel_logs (Timestamp DateTime64(9), Body String) ENGINE = Memory;',
  ).get('otel_logs');

  assert.equal(isMetricTable(gauge), true);
  assert.equal(isMetricTable(histogram), true);
  assert.equal(isMetricTable(logs), false);
  assert.equal(seriesNamespace(gauge), 'sample');
  assert.equal(seriesNamespace(histogram), 'classic-histogram');
});

test('duplicateSamples reports a differing-value duplicate and its two payloads', () => {
  const seed = gaugeSeed([
    row('m', "'job', 'api'", '2026-01-01 00:02:00', '2.0'),
    row('m', "'job', 'api'", '2026-01-01 00:02:00', '3.0'),
  ]);
  const { duplicates, metricRows } = duplicateSamples(seed);
  assert.equal(metricRows, 2);
  assert.equal(duplicates.length, 1);
  assert.equal(duplicates[0].timestamp, '2026-01-01 00:02:00');
  assert.deepEqual(duplicates[0].payloads, ['Value=#2', 'Value=#3']);
});

test('duplicateSamples ignores an identical-value duplicate however it is spelled', () => {
  const seed = gaugeSeed([
    row('m', "'job', 'api'", '2026-01-01 00:02:00', '40.0'),
    row('m', "'job',  'api'", '2026-01-01 00:02:00', '40'),
  ]);
  assert.deepEqual(duplicateSamples(seed).duplicates, []);
});

test('duplicateSamples separates a gauge row from a classic-histogram row at the same series', () => {
  const seed =
    `${GAUGE_DDL}INSERT INTO otel_metrics_gauge VALUES\n` +
    `${row('up', "'job', 'api'", '2026-01-01 00:00:00', '1.0')};\n` +
    `${HISTOGRAM_DDL}INSERT INTO otel_metrics_histogram VALUES\n` +
    "    ('up', map('job', 'api'), toDateTime64('2026-01-01 00:00:00', 9), 3, 6.0, [1.0], [1, 2]);\n";
  assert.deepEqual(duplicateSamples(seed).duplicates, []);
});

test('duplicateSamples groups rows ACROSS bare-named metric tables', () => {
  const sumDDL = GAUGE_DDL.replace(/otel_metrics_gauge/g, 'otel_metrics_sum');
  const seed =
    `${GAUGE_DDL}INSERT INTO otel_metrics_gauge VALUES\n` +
    `${row('m', "'job', 'api'", '2026-01-01 00:00:00', '1.0')};\n` +
    `${sumDDL}INSERT INTO otel_metrics_sum VALUES\n` +
    `${row('m', "'job', 'api'", '2026-01-01 00:00:00', '9.0')};\n`;
  assert.equal(duplicateSamples(seed).duplicates.length, 1);
});

test('opaqueMetricInserts counts generated INSERT ... SELECT seeds', () => {
  const seed = `${GAUGE_DDL}INSERT INTO otel_metrics_gauge\n    SELECT 'm', map(), now64(), 1.0 FROM numbers(10);\n`;
  const tables = parseTables(seed);
  assert.equal(opaqueMetricInserts(seed, tables), 1);
  assert.equal(duplicateSamples(seed).metricRows, 0);
});

test('the gate FAILS on a parity-enrolled differing-value duplicate', () => {
  const root = newCorpus({
    'clean.txtar': fixture({ seed: gaugeSeed([row('m', "'job', 'api'", '2026-01-01 00:00:00', '1.0')]) }),
    'bad.txtar': fixture({
      seed: gaugeSeed([
        row('m', "'job', 'api'", '2026-01-01 00:02:00', '2.0'),
        row('m', "'job', 'api'", '2026-01-01 00:02:00', '3.0'),
      ]),
    }),
  });
  const res = runGate(root);
  assert.equal(res.status, 1, res.stdout + res.stderr);
  assert.match(res.stdout, /bad\.txtar/);
  assert.doesNotMatch(res.stdout, /clean\.txtar/);
  assert.match(res.stdout, /1 violation\(s\)/);
});

test('the gate PASSES an identical-value duplicate, the dedup-contract fixture shape', () => {
  const root = newCorpus({
    'dedup.txtar': fixture({
      seed: gaugeSeed([
        row('m', "'job', 'api'", '1970-01-01 00:04:00', '40.0'),
        row('m', "'job', 'api'", '1970-01-01 00:04:00', '40.0'),
      ]),
    }),
  });
  const res = runGate(root);
  assert.equal(res.status, 0, res.stdout + res.stderr);
  assert.match(res.stdout, /::notice::/);
});

test('the gate PASSES a differing-value duplicate in an UNENROLLED fixture', () => {
  const root = newCorpus({
    'exempt.txtar': fixture({
      parity: false,
      seed: gaugeSeed([
        row('m', "'job', 'api'", '2026-01-01 00:02:00', '2.0'),
        row('m', "'job', 'api'", '2026-01-01 00:02:00', '3.0'),
      ]),
    }),
    'enrolled.txtar': fixture({ seed: gaugeSeed([row('m', "'job', 'api'", '2026-01-01 00:00:00', '1.0')]) }),
  });
  const res = runGate(root);
  assert.equal(res.status, 0, res.stdout + res.stderr);
});

test('the gate FAILS a scan that parsed no metric-shaped seed at all', () => {
  const root = newCorpus({
    'logs.txtar': fixture({
      seed:
        'CREATE TABLE otel_logs (Timestamp DateTime64(9), Body String) ENGINE = Memory;\n' +
        "INSERT INTO otel_logs VALUES (toDateTime64('2026-01-01 00:00:00', 9), 'a'),\n" +
        "    (toDateTime64('2026-01-01 00:00:00', 9), 'b');\n",
    }),
  });
  const res = runGate(root);
  assert.equal(res.status, 1, res.stdout + res.stderr);
  assert.match(res.stdout, /no metric-shaped seed at all/);
});
