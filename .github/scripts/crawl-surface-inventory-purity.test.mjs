/**
 * Self-test for crawl-surface-inventory-purity.mjs.
 *
 * A gate whose own failure path is untested is a gate nobody knows is
 * armed, so every assertion here is paired: the clean case passes AND
 * the dirty case is caught, with the real defect rows from the
 * committed inventories (#1872) as the dirty fixtures.
 *
 * Run: node --test .github/scripts/crawl-surface-inventory-purity.test.mjs
 */

import assert from 'node:assert/strict';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';

import {
  checkInventory,
  declaredVocabulary,
  keyEmbeddedVocabulary,
  loadManifest,
  run,
  splitFragment,
} from './crawl-surface-inventory-purity.mjs';

const CRAWL_DIR = join('test', 'e2e', 'playwright', 'crawl');

const manifest = () => {
  const loaded = loadManifest(CRAWL_DIR);
  assert.deepEqual(loaded.problems, [], 'the committed manifest is well-formed');
  return loaded;
};

const inventory = (...urls) => ({
  doc: 'd',
  stack: 's',
  surfaces: urls.map((url) => ({ url, lean: false })),
});

const check = (...urls) => checkInventory('f.json', inventory(...urls), manifest());

test('the committed manifest declares a placeholder, vocabularies and rationales', () => {
  const m = manifest();
  assert.equal(m.placeholder, '{rep}');
  assert.ok(m.vocabularies.length > 0);
});

test('a manifest entry with no closed set is rejected', () => {
  const dir = mkdtempSync(join(tmpdir(), 'purity-'));
  writeFileSync(
    join(dir, 'control-vocabularies.json'),
    JSON.stringify({
      representativePlaceholder: '{rep}',
      closedVocabularies: [
        { control: '^select\\[x\\]$', rationale: 'because' },
      ],
      exhaustiveDataControls: [],
    }),
  );
  const problems = loadManifest(dir).problems;
  assert.equal(problems.length, 1);
  assert.match(problems[0], /values must be a non-empty array/);
});

test('a manifest entry with no rationale is rejected', () => {
  const dir = mkdtempSync(join(tmpdir(), 'purity-'));
  writeFileSync(
    join(dir, 'control-vocabularies.json'),
    JSON.stringify({
      representativePlaceholder: '{rep}',
      closedVocabularies: [
        { control: '^select\\[x\\]$', rationale: '  ', values: ['a'] },
      ],
      exhaustiveDataControls: [],
    }),
  );
  const problems = loadManifest(dir).problems;
  assert.equal(problems.length, 1);
  assert.match(problems[0], /needs a rationale/);
});

test('a tab or radio key carries its own vocabulary', () => {
  assert.deepEqual(keyEmbeddedVocabulary('radio[Grid|Rows]'), ['Grid', 'Rows']);
  assert.deepEqual(keyEmbeddedVocabulary('tabs[A|B|C]#2'), ['A', 'B', 'C']);
  assert.equal(keyEmbeddedVocabulary('select[Tag filter]'), undefined);
  assert.equal(keyEmbeddedVocabulary('attribute-list'), undefined);
});

test('the fragment split survives a control key that contains =', () => {
  // `adhoc[+ label = value]` is a real committed key; splitting at the
  // first `=` would read the key as `adhoc[+ label ` and the value as
  // `value]={rep}`, quietly passing a row this gate must judge.
  const split = splitFragment('/explore#adhoc[+ label = value]={rep}');
  assert.equal(split.canonical, '/explore');
  assert.deepEqual(check('/explore#adhoc[+ label = value]={rep}'), []);
  assert.equal(
    check('/explore#adhoc[+ label = value]=namespace').length,
    1,
    'and the same key with a seeded value is still caught',
  );
});

test('a parameterized fragment passes', () => {
  assert.deepEqual(
    check(
      '/a/grafana-lokiexplore-app/explore#select[Filter by fields]={rep}',
      '/dashboards#select[Tag filter]={rep}',
      '/a/grafana-exploretraces-app/explore#attribute-list={rep}',
      '/d/x', // no fragment at all
    ),
    [],
  );
});

test('a declared closed vocabulary passes verbatim', () => {
  assert.deepEqual(
    check(
      '/x#select[SortBy direction]=Asc',
      '/x#select[Select match operator]=!~',
      '/x#select[{n} logs]=5000',
      '/x#radio[Grid|Rows]=Rows',
      '/x#tabs[Favorites|All|Resource|Span]=Span',
    ),
    [],
  );
});

test('an undeclared control pinning a literal is caught', () => {
  // The exact rows the committed compose inventory carried.
  for (const url of [
    '/dashboards#select[Tag filter]=cerberus (7)',
    '/a/grafana-lokiexplore-app/explore/service/{service}/logs#select[detected_level]=info',
    '/a/grafana-metricsdrilldown-app/drilldown?metric={metric}#select[group-by-selector-combobox]=cerberus_ql',
    '/a/grafana-exploretraces-app/explore?actionView=traceList#attribute-list=service.name',
    '/d/showcase-traceql#select[panel content]#4=Streaming Progress',
  ]) {
    const problems = check(url);
    assert.equal(problems.length, 1, `expected exactly one problem for ${url}`);
    assert.match(problems[0], /declares no closed vocabulary/);
  }
});

test('a declared control pinning a value outside its set is caught', () => {
  const problems = check('/x#select[SortBy direction]=Sideways');
  assert.equal(problems.length, 1);
  assert.match(problems[0], /outside that control's declared closed set/);
});

test('a tab or radio value outside its own key vocabulary is caught', () => {
  const problems = check('/x#radio[Grid|Rows]=Table');
  assert.equal(problems.length, 1);
  assert.match(problems[0], /outside that control's declared closed set/);
});

test('an unparseable fragment fails rather than being skipped', () => {
  const problems = check('/x#not a control fragment');
  assert.equal(problems.length, 1);
  assert.match(problems[0], /cannot parse/);
});

test('declaredVocabulary prefers a manifest entry over the key embedding', () => {
  const m = manifest();
  assert.deepEqual(declaredVocabulary('select[SortBy direction]', m.vocabularies), [
    'Asc',
    'Desc',
  ]);
  assert.equal(declaredVocabulary('select[Filter by fields]', m.vocabularies), undefined);
});

test('the runner refuses a directory holding no inventories', () => {
  const dir = mkdtempSync(join(tmpdir(), 'purity-'));
  writeFileSync(
    join(dir, 'control-vocabularies.json'),
    JSON.stringify({
      representativePlaceholder: '{rep}',
      closedVocabularies: [],
      exhaustiveDataControls: [],
    }),
  );
  const result = run(dir);
  const problems = Array.isArray(result) ? result : result.problems;
  assert.equal(problems.length, 1);
  assert.match(problems[0], /a gate that reads nothing checks nothing/);
});

test('the committed inventories are pure', () => {
  const result = run(CRAWL_DIR);
  const problems = Array.isArray(result) ? result : result.problems;
  assert.deepEqual(
    problems,
    [],
    `the committed surface inventories must pin no data-derived literal:\n${problems.join('\n')}`,
  );
});
