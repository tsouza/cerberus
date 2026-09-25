// gen-coverage.mjs — render the generated tables in docs/coverage.md from the
// surface-parity conformance ledger.
//
// The ledger (test/surface-parity/inventory/, one JSON shard per (head,
// symbol) pair — see test/surface-parity/inventory_shard.go) pairs every
// grammar symbol the three upstream parsers expose with cerberus's
// accept/reject verdict and the reference backend's, classified four ways
// (parity-accept / parity-reject / wrong-reject / wrong-accept). This script
// translates that test vocabulary into user-facing support states and writes
// two blocks into docs/coverage.md:
//
//   coverage-glance  the per-head summary counts, tallied by
//                    lib/surface-coverage.mjs (which doc-counts.mjs re-derives
//                    the same cells with).
//   coverage-tables  one table per head and symbol kind: symbol, probe query,
//                    support status.
//
// Each block sits between its BEGIN/END AUTOGEN markers and is replaced
// wholesale. Tables are padded to markdownlint's MD060 aligned style.
//
// Usage:
//   node .github/scripts/gen-coverage.mjs           rewrite docs/coverage.md
//   node .github/scripts/gen-coverage.mjs --check   exit 1 if it is stale
//
// Exit codes: 0 = written / fresh; 1 = stale under --check, or the ledger
// carries a head or class the glance table has no place for.

import { readFileSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import process from 'node:process';

import { error } from './lib/gh.mjs';
import { loadShardedEntries } from './lib/sharded-json.mjs';
import { GLANCE_COLUMNS, SURFACE_HEADS, SURFACE_TOTAL_ROW, surfaceParityTotals } from './lib/surface-coverage.mjs';

const REPO = join(dirname(fileURLToPath(import.meta.url)), '..', '..');
const INVENTORY_DIR = join(REPO, 'test', 'surface-parity', 'inventory');
const DOC = join(REPO, 'docs', 'coverage.md');

const TABLES_BEGIN = '<!-- BEGIN AUTOGEN: coverage-tables (.github/scripts/gen-coverage.mjs) -->';
const TABLES_END = '<!-- END AUTOGEN: coverage-tables -->';
const GLANCE_BEGIN = '<!-- BEGIN AUTOGEN: coverage-glance (.github/scripts/gen-coverage.mjs) -->';
const GLANCE_END = '<!-- END AUTOGEN: coverage-glance -->';

// markdownlint's MD060 needs at least three dashes in a separator cell.
const MIN_COLUMN_WIDTH = 3;

// PromQL functions the upstream parser marks Experimental (gated behind
// --enable-feature=promql-experimental-functions, which cerberus enables in
// its prod parser config; internal/api/prom/handler.go). Kept in sync with the
// upstream parser.Functions table.
const PROMQL_EXPERIMENTAL = new Set([
  'double_exponential_smoothing', 'first_over_time', 'histogram_quantiles',
  'info', 'mad_over_time', 'sort_by_label', 'sort_by_label_desc',
  'ts_of_first_over_time', 'ts_of_last_over_time', 'ts_of_max_over_time',
  'ts_of_min_over_time', 'range', 'step', 'start', 'end',
]);

// Per head: the symbol kinds, in table order, and each kind's heading.
const KIND_TITLES = {
  promql: {
    aggregator: 'Aggregations', function: 'Functions',
    'binary-op': 'Binary operators', modifier: 'Modifiers',
  },
  logql: {
    'vector-agg': 'Vector aggregations', 'range-agg': 'Range aggregations',
    'parser-stage': 'Parser stages', 'label-fn': 'Label / format stages',
    'label-filter': 'Label filters', 'line-filter': 'Line filters',
    'conv-fn': 'Conversion functions', 'binary-op': 'Binary operators',
  },
  traceql: {
    aggregate: 'Aggregates', intrinsic: 'Intrinsics',
    'metrics-op': 'Metrics operators',
  },
};

// symbolName — the symbol with its `<kind>:` prefix removed.
const symbolName = (entry) => entry.symbol.slice(entry.symbol.indexOf(':') + 1);

// status — the user-facing support state of one ledger entry.
function status(entry) {
  const isExperimental = entry.head === 'promql' && PROMQL_EXPERIMENTAL.has(symbolName(entry));
  switch (entry.class) {
    case 'parity-accept':
      return isExperimental ? 'Supported (experimental)' : 'Supported';
    case 'parity-reject':
      // Both cerberus and the reference reject. For the experimental
      // query-context fns (start/end) this is a real intentional gate.
      return 'Rejected (parity with reference)';
    case 'wrong-accept':
      // Cerberus accepts, the probe's reference rejects. For range()/step()
      // this is a faithful experimental implementation the bare-call probe
      // cannot see.
      return isExperimental ? 'Supported (experimental)' : 'Supported (cerberus extension)';
    case 'wrong-reject':
      return 'Not yet supported';
    default:
      return entry.class;
  }
}

// codePoints — a string's length in code points.
const codePoints = (s) => [...s].length;

// mdWidth — a cell's width as markdownlint's MD060 measures it: the source
// length, less one for every backslash escape of a character other than `|`
// (`\w` renders as one glyph; an escaped pipe `\|` keeps both source bytes).
export function mdWidth(cell) {
  const backslashEscapes = (cell.match(/\\./g) ?? []).length;
  const escapedPipes = cell.split('\\|').length - 1;
  return codePoints(cell) - (backslashEscapes - escapedPipes);
}

// alignedTable — the lines of a Markdown table whose pipes land where MD060's
// aligned style expects them.
export function alignedTable(header, rows) {
  const all = [header, ...rows];
  const widths = header.map((_, i) => Math.max(MIN_COLUMN_WIDTH, ...all.map((r) => mdWidth(r[i]))));
  const pad = (cell, w) => cell + ' '.repeat(Math.max(0, w - mdWidth(cell)));
  const fmt = (cells) => `| ${cells.map((c, i) => pad(c, widths[i])).join(' | ')} |`;
  const sep = `| ${widths.map((w) => '-'.repeat(w)).join(' | ')} |`;
  return [fmt(header), sep, ...rows.map(fmt)];
}

const bySymbol = (a, b) => (a.symbol < b.symbol ? -1 : a.symbol > b.symbol ? 1 : 0);

function renderHead(head, entries) {
  const titles = KIND_TITLES[head];
  const byKind = new Map();
  for (const x of entries) {
    if (!byKind.has(x.kind)) byKind.set(x.kind, []);
    byKind.get(x.kind).push(x);
  }
  const out = [];
  for (const kind of Object.keys(titles).filter((k) => byKind.has(k))) {
    out.push(`#### ${titles[kind]}\n`);
    const rows = [...byKind.get(kind)]
      .sort(bySymbol)
      .map((x) => [`\`${symbolName(x)}\``, `\`${x.probe.replaceAll('|', '\\|')}\``, status(x)]);
    out.push(...alignedTable(['Symbol', 'Probe', 'Status'], rows));
    out.push('');
  }
  return out.join('\n');
}

// buildGlance — the per-head summary table. Every cell is a tally over the
// ledger's `class` field, so the headline figures are derived, not typed.
export function buildGlance(entries) {
  const totals = surfaceParityTotals({ entries });
  const row = (label, t, fmt) => [label, ...GLANCE_COLUMNS.map(({ key }) => fmt(t[key]))];
  const rows = SURFACE_HEADS.map(({ head, label }) => row(label, totals[head], String));
  rows.push(row('**Total**', totals[SURFACE_TOTAL_ROW], (n) => `**${n}**`));
  const header = ['Head', ...GLANCE_COLUMNS.map(({ title }) => title)];
  return `${alignedTable(header, rows).join('\n')}\n`;
}

// buildTables — every head's per-kind symbol tables.
export function buildTables(entries) {
  const blocks = [];
  for (const { head, label } of SURFACE_HEADS) {
    const he = entries.filter((x) => x.head === head);
    blocks.push(`### ${label} (${he.length} symbols)\n`);
    blocks.push(renderHead(head, he));
  }
  return `${blocks.join('\n').trimEnd()}\n`;
}

const escapeRegExp = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');

// The backslash escapes unescapeTemplate() resolves to one character.
const TEMPLATE_ESCAPES = { '\\': '\\', a: '\x07', b: '\b', f: '\f', n: '\n', r: '\r', t: '\t', v: '\v' };

// unescapeTemplate — the body as docs/coverage.md carries it: backslash
// escapes resolved the way a regular-expression replacement template resolves
// them (`\\` -> `\`, `\n` -> newline, and so on; any other escape of a
// non-letter is kept as written). An escape of a letter, a digit, or a
// trailing lone backslash has no meaning there and throws. The Probe column
// therefore shows a ledger probe's `\\` as `\` (#3695).
export function unescapeTemplate(body) {
  return body.replace(/\\([\s\S]?)/g, (whole, c) => {
    if (Object.hasOwn(TEMPLATE_ESCAPES, c)) return TEMPLATE_ESCAPES[c];
    if (c === '' || /[A-Za-z0-9]/.test(c)) throw new Error(`bad escape ${JSON.stringify(whole)} in a rendered block`);
    return whole;
  });
}

// splice — `doc` with everything between `begin` and `end` replaced by
// `body` (after unescapeTemplate).
export function splice(doc, begin, end, body) {
  const re = new RegExp(`${escapeRegExp(begin)}[\\s\\S]*?${escapeRegExp(end)}`, 'g');
  const block = `${begin}\n\n${unescapeTemplate(body)}\n${end}`;
  return doc.replace(re, () => block);
}

// render — docs/coverage.md with both generated blocks rebuilt from `entries`.
export function render(doc, entries) {
  const withGlance = splice(doc, GLANCE_BEGIN, GLANCE_END, buildGlance(entries));
  return splice(withGlance, TABLES_BEGIN, TABLES_END, buildTables(entries));
}

function main() {
  const doc = readFileSync(DOC, 'utf8');
  let next;
  try {
    next = render(doc, loadShardedEntries(INVENTORY_DIR));
  } catch (err) {
    error(`gen-coverage: ${err.message}`);
    process.exit(1);
  }
  if (process.argv.includes('--check')) {
    if (next !== doc) {
      error('gen-coverage: docs/coverage.md is stale; run `node .github/scripts/gen-coverage.mjs`', {
        file: 'docs/coverage.md',
      });
      process.exit(1);
    }
    return;
  }
  if (next !== doc) writeFileSync(DOC, next);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) main();
