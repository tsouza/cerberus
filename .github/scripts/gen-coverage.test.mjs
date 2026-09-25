// gen-coverage.test.mjs — node:test guard for the docs/coverage.md renderer.
//
// Pins the pieces whose output a reader sees: the MD060 cell width, the
// replacement-template unescaping of a spliced block, the glance tally
// (through lib/surface-coverage.mjs, shared with doc-counts.mjs), the
// per-symbol status translation, and the splice that replaces only the text
// between each pair of AUTOGEN markers.

import { test } from 'node:test';
import assert from 'node:assert/strict';

import { alignedTable, buildGlance, buildTables, mdWidth, render, splice, unescapeTemplate } from './gen-coverage.mjs';

const entry = (head, kind, symbol, probe, cls) => ({ head, kind, symbol, probe, class: cls });

test('mdWidth drops one column per backslash escape except an escaped pipe', () => {
  assert.equal(mdWidth('abc'), 3);
  assert.equal(mdWidth('a\\|b'), 4);
  assert.equal(mdWidth('a\\wb'), 3);
  assert.equal(mdWidth('日本'), 2);
});

test('alignedTable pads to the widest cell with a three-column minimum', () => {
  assert.deepEqual(alignedTable(['A', 'Bee'], [['x', 'y'], ['long', 'z']]), [
    '| A    | Bee |',
    '| ---- | --- |',
    '| x    | y   |',
    '| long | z   |',
  ]);
});

test('unescapeTemplate collapses \\\\, keeps other non-letter escapes, and rejects letter escapes', () => {
  assert.equal(unescapeTemplate('a\\\\w b\\|c'), 'a\\w b\\|c');
  assert.equal(unescapeTemplate('x\\ny'), 'x\ny');
  assert.throws(() => unescapeTemplate('\\w'), /bad escape/);
  assert.throws(() => unescapeTemplate('\\1'), /bad escape/);
  assert.throws(() => unescapeTemplate('tail\\'), /bad escape/);
});

test('buildGlance tallies each head and the total through the shared fold', () => {
  const entries = [
    entry('promql', 'function', 'fn:a', 'a()', 'parity-accept'),
    entry('promql', 'function', 'fn:b', 'b()', 'wrong-accept'),
    entry('promql', 'function', 'fn:c', 'c()', 'parity-reject'),
    entry('logql', 'conv-fn', 'conv:d', 'd', 'wrong-reject'),
  ];
  const lines = buildGlance(entries).trimEnd().split('\n');
  assert.match(lines[0], /^\| Head +\| Symbols probed +\| Supported \(incl\. experimental\) +\|/);
  assert.match(lines[2], /^\| PromQL +\| 3 +\| 2 +\| 1 +\| 0 +\|$/);
  assert.match(lines[3], /^\| LogQL +\| 1 +\| 0 +\| 0 +\| 1 +\|$/);
  assert.match(lines[4], /^\| TraceQL +\| 0 +\| 0 +\| 0 +\| 0 +\|$/);
  assert.match(lines[5], /^\| \*\*Total\*\* \| \*\*4\*\* +\| \*\*2\*\* +\| \*\*1\*\* +\| \*\*1\*\* +\|$/);
});

test('buildGlance refuses a ledger class no column counts', () => {
  assert.throws(() => buildGlance([entry('promql', 'function', 'fn:a', 'a()', 'novel')]), /maps to no glance column/);
});

test('buildTables translates ledger classes into support states and escapes probe pipes', () => {
  const out = buildTables([
    entry('promql', 'function', 'fn:range', 'range()', 'wrong-accept'),
    entry('promql', 'function', 'fn:rate', 'rate(x[1m])', 'parity-accept'),
    entry('promql', 'function', 'fn:weird', 'weird()', 'wrong-accept'),
    entry('logql', 'parser-stage', 'parser:json', '{a="b"} | json', 'wrong-reject'),
  ]);
  assert.match(out, /^### PromQL \(3 symbols\)$/m);
  assert.match(out, /^#### Functions$/m);
  assert.match(out, /\| `range` +\| `range\(\)` +\| Supported \(experimental\) +\|/);
  assert.match(out, /\| `rate` +\| `rate\(x\[1m\]\)` +\| Supported +\|/);
  assert.match(out, /\| `weird` +\| `weird\(\)` +\| Supported \(cerberus extension\) \|/);
  assert.match(out, /\| `json` +\| `\{a="b"\} \\\| json` \| Not yet supported \|/);
  assert.match(out, /^### TraceQL \(0 symbols\)$/m);
});

test('splice replaces only the text between the markers', () => {
  const doc = 'before\n<!-- B -->\nstale\n<!-- E -->\nafter\n';
  assert.equal(splice(doc, '<!-- B -->', '<!-- E -->', 'fresh\n'), 'before\n<!-- B -->\n\nfresh\n\n<!-- E -->\nafter\n');
});

test('render rebuilds both generated blocks and is idempotent', () => {
  const doc = [
    'head',
    '<!-- BEGIN AUTOGEN: coverage-glance (.github/scripts/gen-coverage.mjs) -->',
    '<!-- END AUTOGEN: coverage-glance -->',
    'middle',
    '<!-- BEGIN AUTOGEN: coverage-tables (.github/scripts/gen-coverage.mjs) -->',
    'old table',
    '<!-- END AUTOGEN: coverage-tables -->',
    'tail',
    '',
  ].join('\n');
  const entries = [entry('traceql', 'intrinsic', 'intrinsic:duration', '{ duration > 1s }', 'parity-accept')];
  const once = render(doc, entries);
  assert.doesNotMatch(once, /old table/);
  assert.match(once, /\| TraceQL +\| 1 +\| 1 +\|/);
  assert.match(once, /^### TraceQL \(1 symbols\)$/m);
  assert.equal(render(once, entries), once);
});
