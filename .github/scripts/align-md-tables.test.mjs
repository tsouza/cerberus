// align-md-tables.test.mjs — node:test guard for the MD060 table aligner.
//
// Pins the alignment rule on small fixtures (column padding, separator
// rewrite, code-point width, ragged runs left alone, line-ending
// normalisation) and the CLI's in-place contract: `.md` files are rewritten
// only when their content changes, other arguments are ignored.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, readFileSync, statSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { alignFile, alignMarkdown } from './align-md-tables.mjs';

const CLI = join(dirname(fileURLToPath(import.meta.url)), 'align-md-tables.mjs');

test('pads every cell to its column width and rewrites the separator row', () => {
  const src = ['intro', '| a | long header |', '|:-|--:|', '| wider cell | x |', 'outro', ''].join('\n');
  const want = [
    'intro',
    '| a          | long header |',
    '| ---------- | ----------- |',
    '| wider cell | x           |',
    'outro',
    '',
  ].join('\n');
  assert.equal(alignMarkdown(src), want);
});

test('measures width in code points, not UTF-16 units or bytes', () => {
  const src = '| é日𝔸 | b |\n|-|-|\n| c | d |\n';
  assert.equal(alignMarkdown(src), '| é日𝔸 | b |\n| --- | - |\n| c   | d |\n');
});

test('leaves a run whose rows disagree on the column count untouched', () => {
  const src = '| a | b |\n| c |\n';
  assert.equal(alignMarkdown(src), src);
});

test('an indented table is re-emitted at column zero', () => {
  assert.equal(alignMarkdown('  | a | bb |\n  |-|-|\n'), '| a | bb |\n| - | -- |\n');
});

test('normalises CRLF to LF and terminates a final table line', () => {
  assert.equal(alignMarkdown('x\r\n| a | bb |\r\n|-|-|'), 'x\n| a | bb |\n| - | -- |\n');
});

test('text with no table passes through byte for byte', () => {
  const src = 'plain | text\nno leading pipe |\n\n';
  assert.equal(alignMarkdown(src), src);
});

test('alignFile rewrites a misaligned file and leaves an aligned one alone', () => {
  const dir = mkdtempSync(join(tmpdir(), 'align-md-'));
  const misaligned = join(dir, 'a.md');
  const aligned = join(dir, 'b.md');
  writeFileSync(misaligned, '| a | bb |\n|-|-|\n');
  writeFileSync(aligned, '| a | bb |\n| - | -- |\n');
  assert.equal(alignFile(misaligned), true);
  assert.equal(readFileSync(misaligned, 'utf8'), '| a | bb |\n| - | -- |\n');
  assert.equal(alignFile(aligned), false);
});

test('the CLI aligns .md arguments in place and ignores the rest', () => {
  const dir = mkdtempSync(join(tmpdir(), 'align-md-cli-'));
  const md = join(dir, 'doc.md');
  const txt = join(dir, 'doc.txt');
  const table = '| a | bb |\n|-|-|\n';
  writeFileSync(md, table);
  writeFileSync(txt, table);
  const res = spawnSync('node', [CLI, md, txt], { encoding: 'utf8' });
  assert.equal(res.status, 0, res.stdout + res.stderr);
  assert.equal(readFileSync(md, 'utf8'), '| a | bb |\n| - | -- |\n');
  assert.equal(readFileSync(txt, 'utf8'), table);
});

test('the CLI fails with an ::error:: on an unreadable .md argument', () => {
  const missing = join(mkdtempSync(join(tmpdir(), 'align-md-missing-')), 'gone.md');
  assert.throws(() => statSync(missing));
  const res = spawnSync('node', [CLI, missing], { encoding: 'utf8' });
  assert.equal(res.status, 1);
  assert.match(res.stdout, /::error::align-md-tables: .*gone\.md/);
});
