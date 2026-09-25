// align-md-tables.mjs — align Markdown tables in place so MD060
// (table-column-style: aligned) passes.
//
// markdownlint's MD060 measures pipe positions by display width (the
// `string-width` package). Every character in this repository's tables is
// display-width 1 or whitespace, so a cell's width is its code-point count.
// A table that ever carries emoji or CJK text needs a display-width measure
// in cellWidth() instead.
//
// A table is a maximal run of lines that each start and end with `|` once
// surrounding whitespace is ignored. Every cell of a table is rewritten as
// `| <content padded to the column's widest cell> |`; the separator row (the
// row holding a `:?-+:?` cell) is rewritten as dashes of the column width.
// A run whose rows disagree on the column count is left untouched. Line
// endings are normalised to `\n`, and a file is rewritten only when its
// content changes.
//
// Usage:
//   node .github/scripts/align-md-tables.mjs FILE [FILE ...]
//
// Arguments not ending in `.md` are ignored. Exit codes: 0 = every file was
// aligned (changed or not — the lefthook pre-commit hook re-stages what this
// fixes); 1 = a file could not be read or written.

import { readFileSync, writeFileSync } from 'node:fs';
import { pathToFileURL } from 'node:url';
import process from 'node:process';

import { error } from './lib/gh.mjs';

// The whitespace stripped around a line and a cell. Spelled out rather than
// taken from `\s`, which also matches U+FEFF and misses U+001C..U+001F and
// U+0085.
const WS = '[\\t\\n\\v\\f\\r \\x1c-\\x1f\\x85\\xa0\\u1680\\u2000-\\u200a\\u2028\\u2029\\u202f\\u205f\\u3000]';
const LEADING_WS = new RegExp(`^${WS}+`, 'u');
const TRAILING_WS = new RegExp(`${WS}+$`, 'u');
const TRAILING_NEWLINES = /\n+$/;
const SEPARATOR_CELL = /^:?-+:?$/;

const lstrip = (s) => s.replace(LEADING_WS, '');
const rstrip = (s) => s.replace(TRAILING_WS, '');
const strip = (s) => lstrip(rstrip(s));

// cellWidth — a cell's width in code points (see the header).
const cellWidth = (s) => [...s].length;

function isTableLine(line) {
  return lstrip(line).startsWith('|') && rstrip(line).endsWith('|');
}

// alignTable — the aligned form of one run of table lines, or the run
// unchanged when it is not a well-formed table.
function alignTable(tableLines) {
  const rows = [];
  for (const line of tableLines) {
    const s = strip(line.replace(TRAILING_NEWLINES, ''));
    if (!s.startsWith('|') || !s.endsWith('|')) return tableLines;
    rows.push(s.slice(1, -1).split('|'));
  }
  if (rows.length === 0) return tableLines;
  const nCols = rows[0].length;
  const widths = new Array(nCols).fill(0);
  let sepIdx = null;
  for (let i = 0; i < rows.length; i++) {
    if (rows[i].length !== nCols) return tableLines;
    rows[i].forEach((cell, j) => {
      const stripped = strip(cell);
      if (SEPARATOR_CELL.test(stripped)) sepIdx = i;
      widths[j] = Math.max(widths[j], cellWidth(stripped));
    });
  }
  return rows.map((row, i) => {
    const cells = row.map((cell, j) => {
      if (i === sepIdx) return ` ${'-'.repeat(widths[j])} `;
      const stripped = strip(cell);
      return ` ${stripped}${' '.repeat(widths[j] - cellWidth(stripped))} `;
    });
    return `|${cells.join('|')}|\n`;
  });
}

// splitLines — the file's lines, each keeping its `\n`, after `\r\n` and a
// lone `\r` are normalised to `\n`.
function splitLines(text) {
  return text.replace(/\r\n?/g, '\n').match(/[^\n]*\n|[^\n]+$/g) ?? [];
}

// alignMarkdown — `text` with every table aligned. Pure; the CLI and
// lib/markdown-lintfix.mjs share it.
export function alignMarkdown(text) {
  const lines = splitLines(text);
  const out = [];
  let i = 0;
  while (i < lines.length) {
    if (!isTableLine(lines[i])) {
      out.push(lines[i]);
      i++;
      continue;
    }
    let j = i;
    while (j < lines.length && isTableLine(lines[j])) j++;
    out.push(...alignTable(lines.slice(i, j)));
    i = j;
  }
  return out.join('');
}

// alignFile — align one file in place; true when its content changed.
export function alignFile(path) {
  const text = readFileSync(path, 'utf8');
  const aligned = alignMarkdown(text);
  if (aligned === splitLines(text).join('')) return false;
  writeFileSync(path, aligned, 'utf8');
  return true;
}

function main() {
  for (const path of process.argv.slice(2)) {
    if (!path.endsWith('.md')) continue;
    try {
      alignFile(path);
    } catch (err) {
      error(`align-md-tables: ${path}: ${err.message}`);
      process.exit(1);
    }
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) main();
