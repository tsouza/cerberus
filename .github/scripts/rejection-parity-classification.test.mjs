// Tests for rejection-parity-classification.mjs. Run with:
//   node --test .github/scripts/rejection-parity-classification.test.mjs

import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import process from 'node:process';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';

import {
  CLASSES,
  CLASSIFICATION_GATE,
  main,
  reportLines,
  summaryMarkdown,
  unclassifiedEntries,
} from './rejection-parity-classification.mjs';

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');

function catalogue(shards) {
  const dir = mkdtempSync(path.join(tmpdir(), 'rp-class-'));
  for (const [name, entries] of Object.entries(shards)) {
    writeFileSync(path.join(dir, name), JSON.stringify({ entries }, null, 2));
  }
  return dir;
}

const classified = {
  head: 'promql',
  site: 'internal/promql/a.go:f#00000001',
  message: 'promql: classified',
  class: 'internal',
  rationale: 'parser-enforced',
};
const fresh = {
  head: 'promql',
  site: 'internal/promql/a.go:f#00000002',
  message: 'promql: new site',
  class: '',
};
const demoted = {
  head: 'logql',
  site: 'internal/logql/b.go:g#00000003',
  message: 'logql: demoted site',
};

test('the class vocabulary equals the Class constants in catalogue.go', () => {
  const src = readFileSync(path.join(repoRoot, 'test/rejection-parity/catalogue.go'), 'utf8');
  const block = src.match(/const \(\s*\n((?:\s*Class\w+\s*=\s*"[^"]*"\s*\n)+)\s*\)/);
  assert.ok(block, 'catalogue.go no longer declares its Class constants in one const block');
  const goClasses = [...block[1].matchAll(/=\s*"([^"]*)"/g)].map((m) => m[1]).sort();
  assert.deepEqual(Object.keys(CLASSES).sort(), goClasses);
});

test('unclassifiedEntries names exactly the entries without a valid class', () => {
  const dir = catalogue({
    'internal__promql__a.go__f.json': [classified, fresh],
    'internal__logql__b.go__g.json': [demoted],
    'internal__traceql__c.go__h.json': [{ ...classified, class: 'bogus' }],
  });
  const got = unclassifiedEntries(dir);
  assert.deepEqual(
    got.map((e) => [path.basename(e.shard), e.site, e.class]),
    [
      ['internal__logql__b.go__g.json', demoted.site, ''],
      ['internal__promql__a.go__f.json', fresh.site, ''],
      ['internal__traceql__c.go__h.json', classified.site, 'bogus'],
    ],
  );
});

test('a fully classified catalogue reports nothing', () => {
  const dir = catalogue({ 'x.json': [classified] });
  assert.deepEqual(unclassifiedEntries(dir), []);
  assert.deepEqual(reportLines([]), []);
  assert.equal(summaryMarkdown([]), '');
});

test('the report names the entry, its file, every class and the gate', () => {
  const dir = catalogue({ 'internal__promql__a.go__f.json': [fresh] });
  const text = reportLines(unclassifiedEntries(dir)).join('\n');
  assert.match(text, new RegExp(fresh.site.replace(/[.#]/g, '\\$&')));
  assert.match(text, /promql: new site/);
  assert.match(text, /internal__promql__a\.go__f\.json/);
  assert.match(text, new RegExp(CLASSIFICATION_GATE));
  for (const cls of Object.keys(CLASSES)) assert.match(text, new RegExp(`\\b${cls}\\b`));
});

test('main appends the report to the job summary and exits 0', () => {
  const dir = catalogue({ 'internal__promql__a.go__f.json': [fresh] });
  const summary = path.join(mkdtempSync(path.join(tmpdir(), 'rp-sum-')), 'summary.md');
  writeFileSync(summary, '');
  const saved = process.env.GITHUB_STEP_SUMMARY;
  process.env.GITHUB_STEP_SUMMARY = summary;
  try {
    assert.equal(main({ CATALOGUE_DIR: dir }), 0);
  } finally {
    if (saved === undefined) delete process.env.GITHUB_STEP_SUMMARY;
    else process.env.GITHUB_STEP_SUMMARY = saved;
  }
  const md = readFileSync(summary, 'utf8');
  assert.match(md, /need classification/);
  assert.match(md, /internal\/promql\/a\.go:f#00000002/);
});

test('main exits 1 on a shard that is not a catalogue document', () => {
  const dir = catalogue({});
  writeFileSync(path.join(dir, 'broken.json'), '{"not_entries": []}');
  assert.equal(main({ CATALOGUE_DIR: dir }), 1);
  writeFileSync(path.join(dir, 'broken.json'), '{');
  assert.equal(main({ CATALOGUE_DIR: dir }), 1);
});

test('main exits 1 when the catalogue directory is missing or holds no shard', () => {
  assert.equal(main({ CATALOGUE_DIR: path.join(tmpdir(), 'rp-class-does-not-exist') }), 1);
  assert.equal(main({ CATALOGUE_DIR: catalogue({}) }), 1);
});
