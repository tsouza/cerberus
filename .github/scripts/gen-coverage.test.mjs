// gen-coverage.test.mjs — node:test guard for the docs/coverage.md renderer.
//
// Pins the pieces whose output a reader sees: the MD060 cell width, the
// glance tally (through lib/surface-coverage.mjs, shared with
// doc-counts.mjs), the per-symbol status translation, the byte-for-byte
// backslash fidelity of a rendered probe (#3695), and the splice that
// replaces only the text between each pair of AUTOGEN markers.

import { test } from 'node:test';
import assert from 'node:assert/strict';

import { alignedTable, buildGlance, buildTables, mdWidth, render, splice } from './gen-coverage.mjs';

const entry = (head, kind, symbol, probe, cls) => ({ head, kind, symbol, probe, class: cls });

test('mdWidth is the raw code-point length, with no escape adjustment', () => {
  assert.equal(mdWidth('abc'), 3);
  assert.equal(mdWidth('a\\|b'), 4);
  assert.equal(mdWidth('a\\wb'), 4);
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

test('splice keeps a body\'s own backslashes byte-for-byte (#3695)', () => {
  const doc = 'before\n<!-- B -->\nstale\n<!-- E -->\nafter\n';
  assert.equal(
    splice(doc, '<!-- B -->', '<!-- E -->', 'a\\\\w b\\|c\n'),
    'before\n<!-- B -->\n\na\\\\w b\\|c\n\n<!-- E -->\nafter\n',
  );
});

test('buildTables renders a probe with \\\\ and a probe with \\| byte-for-byte, MD060-aligned', () => {
  const out = buildTables([
    entry('logql', 'parser-stage', 'parser:regexp', '{service_name="gateway"} | regexp "(?P<lvl>\\\\w+)"', 'parity-accept'),
    entry('logql', 'parser-stage', 'parser:pipe', 'literal \\| pipe', 'parity-accept'),
  ]);
  // The probe's own `\\` survives untouched — not collapsed to a single `\`.
  // Its literal `|` is separately escaped to `\|` for Markdown by renderHead.
  assert.match(out, /`\{service_name="gateway"\} \\\| regexp "\(\?P<lvl>\\\\w\+\)"`/);
  // A probe carrying its own `\|` escape gets renderHead's pipe-escaping
  // applied on top (it does not know the `|` is already escaped), and that
  // doubled escape survives the splice uncollapsed too.
  assert.match(out, /`literal \\\\\| pipe`/);
  // MD060-aligned: every row's Probe cell (the column with variable-width
  // escapes) has the SAME markdownlint-measured width as the separator row's
  // dash count for that column.
  const lines = out.split('\n').filter((l) => l.startsWith('| `'));
  const sepWidth = out.match(/\| -+ \| (-+) \| -+ \|/)[1].length;
  for (const line of lines) {
    const probeCell = line.split(' | ')[1];
    assert.equal(mdWidth(probeCell), sepWidth, `misaligned probe cell: ${JSON.stringify(probeCell)}`);
  }
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
