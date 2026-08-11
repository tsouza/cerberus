/**
 * Self-test for crawl-surface-inventory-purity.mjs.
 *
 * A gate whose own failure path is untested is a gate nobody knows is
 * armed, so every check the script makes is paired here: the clean case
 * passes AND the dirty case is caught. The dirty fixtures for the main
 * assertion are the real rows the committed inventories carried (#1872)
 * rather than invented ones.
 *
 * Run: node --test .github/scripts/crawl-surface-inventory-purity.test.mjs
 */

import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';

import {
  checkInventory,
  declaredVocabulary,
  keyEmbeddedVocabulary,
  loadManifest,
  run,
  splitFragment,
} from './crawl-surface-inventory-purity.mjs';

const HERE = dirname(fileURLToPath(import.meta.url));
// Resolved from this file, never from the caller's cwd: a self-test that
// only passes when invoked from the repository root is one that silently
// stops running the day a job changes directory.
const CRAWL_DIR = join(HERE, '..', '..', 'test', 'e2e', 'playwright', 'crawl');

const manifest = () => {
  const loaded = loadManifest(CRAWL_DIR);
  assert.deepEqual(loaded.problems, [], 'the committed manifest is well-formed');
  return loaded;
};

/** A throwaway manifest directory, with fields overridden or removed. */
const manifestDir = (overrides) => {
  const dir = mkdtempSync(join(tmpdir(), 'purity-'));
  const merged = {
    representativePlaceholder: '{rep}',
    keyEmbeddedVocabularyPattern: '^(?:tabs|radio)\\[([\\s\\S]+)\\](?:#\\d+)?$',
    closedVocabularies: [],
    exhaustiveDataControls: [],
    ...overrides,
  };
  for (const [k, v] of Object.entries(merged)) {
    if (v === undefined) delete merged[k];
  }
  writeFileSync(join(dir, 'control-vocabularies.json'), JSON.stringify(merged));
  return dir;
};

const manifestProblems = (overrides) =>
  loadManifest(manifestDir(overrides)).problems;

const inventory = (...urls) => ({
  doc: 'd',
  stack: 's',
  surfaces: urls.map((url) => ({ url, lean: false })),
});

const check = (...urls) =>
  checkInventory('f.json', inventory(...urls), manifest());

// ---------------------------------------------------------------------------
// The manifest: every declaration is a claim about the app, so every
// malformed shape has to be rejected rather than silently half-applied.
// ---------------------------------------------------------------------------

test('the committed manifest declares a placeholder, a pattern and vocabularies', () => {
  const m = manifest();
  assert.equal(m.placeholder, '{rep}');
  assert.equal(typeof m.keyEmbeddedVocabularyPattern, 'string');
  assert.ok(m.vocabularies.length > 0);
});

test('a well-formed manifest reports nothing', () => {
  assert.deepEqual(manifestProblems({}), []);
});

test('a manifest missing either compiled string is rejected', () => {
  for (const field of [
    'representativePlaceholder',
    'keyEmbeddedVocabularyPattern',
  ]) {
    const problems = manifestProblems({ [field]: undefined });
    assert.equal(problems.length, 1, `expected one problem for ${field}`);
    assert.match(problems[0], new RegExp(`${field} must be a non-empty string`));
  }
});

test('a key-embedding pattern that is not a valid regex is rejected', () => {
  const problems = manifestProblems({ keyEmbeddedVocabularyPattern: '^(?:tabs' });
  assert.equal(problems.length, 1);
  assert.match(problems[0], /keyEmbeddedVocabularyPattern is not a valid regex/);
});

test('a manifest whose declaration lists are not arrays is rejected', () => {
  for (const field of ['closedVocabularies', 'exhaustiveDataControls']) {
    const problems = manifestProblems({ [field]: 'nope' });
    assert.equal(problems.length, 1, `expected one problem for ${field}`);
    assert.match(problems[0], new RegExp(`${field} must be an array`));
  }
});

test('a closed vocabulary missing its closed set, rationale or regex is rejected', () => {
  const entry = {
    control: '^select\\[x\\]$',
    rationale: 'because',
    values: ['a'],
  };
  for (const [override, pattern] of [
    [{ values: undefined }, /values must be a non-empty array/],
    [{ values: [] }, /values must be a non-empty array/],
    [{ values: [1] }, /values must be a non-empty array/],
    [{ rationale: '  ' }, /needs a rationale/],
    [{ rationale: undefined }, /needs a rationale/],
    [{ control: '' }, /control must be a non-empty regex source string/],
    [{ control: '^select\\[x(' }, /not a valid regex/],
  ]) {
    const problems = manifestProblems({
      closedVocabularies: [{ ...entry, ...override }],
    });
    assert.equal(
      problems.length,
      1,
      `expected one problem for ${JSON.stringify(override)}, got ${JSON.stringify(problems)}`,
    );
    assert.match(problems[0], pattern);
  }
});

test('an exhaustive-data control without a bound or a rationale is rejected', () => {
  // The bound is what stops an exhaustive sweep over a data-derived list
  // from growing with the seed until it trips the sweep cap and fails
  // the crawl, so a missing or nonsensical one is not a formatting nit.
  const entry = {
    control: '^attr-list$',
    rationale: 'because',
    maxOptions: 12,
    maxOptionsRationale: 'because',
  };
  for (const [override, pattern] of [
    [{ maxOptions: undefined }, /maxOptions must be a positive integer/],
    [{ maxOptions: 0 }, /maxOptions must be a positive integer/],
    [{ maxOptions: 2.5 }, /maxOptions must be a positive integer/],
    [{ maxOptionsRationale: '' }, /maxOptions needs its own rationale/],
    [{ maxOptionsRationale: undefined }, /maxOptions needs its own rationale/],
    [{ rationale: undefined }, /needs a rationale/],
    [{ control: '' }, /control must be a non-empty regex source string/],
  ]) {
    const problems = manifestProblems({
      exhaustiveDataControls: [{ ...entry, ...override }],
    });
    assert.equal(
      problems.length,
      1,
      `expected one problem for ${JSON.stringify(override)}, got ${JSON.stringify(problems)}`,
    );
    assert.match(problems[0], pattern);
  }
});

// ---------------------------------------------------------------------------
// Key identity
// ---------------------------------------------------------------------------

test('a tab or radio key carries its own vocabulary', () => {
  const pattern = manifest().keyEmbeddedVocabularyPattern;
  const embedded = (key) => keyEmbeddedVocabulary(key, pattern);
  assert.deepEqual(embedded('radio[Grid|Rows]'), ['Grid', 'Rows']);
  assert.deepEqual(embedded('tabs[A|B|C]#2'), ['A', 'B', 'C']);
  assert.equal(embedded('select[Tag filter]'), undefined);
  assert.equal(embedded('attribute-list'), undefined);
});

test('neither side hardcodes the key-embedding pattern', () => {
  // The tab/radio embedding is one of the two routes to a verbatim key,
  // so it decides what may be pinned — exactly what the closed
  // vocabularies decide. Both consumers must COMPILE it from the
  // manifest: a literal in either file is a second copy free to drift,
  // and a gate that drifts LOOSER certifies pins the crawler never
  // produced. Only a source scan catches that — two literals that
  // happen to agree today pass every behavioural test there is.
  const literal = /\/\^\(\?:tabs\|radio\)/;
  for (const file of [
    join(HERE, 'crawl-surface-inventory-purity.mjs'),
    join(CRAWL_DIR, 'interactions.ts'),
  ]) {
    assert.ok(
      !literal.test(readFileSync(file, 'utf8')),
      `${file} hardcodes the key-embedding pattern instead of compiling it from the manifest`,
    );
  }
});

test('declaredVocabulary prefers a manifest entry over the key embedding', () => {
  const m = manifest();
  assert.deepEqual(declaredVocabulary('select[SortBy direction]', m), [
    'Asc',
    'Desc',
  ]);
  assert.equal(declaredVocabulary('select[Filter by fields]', m), undefined);
});

test('the fragment split survives a control key that contains =', () => {
  // `adhoc[+ label = value]` is a real committed key; splitting at the
  // first `=` would read the key as `adhoc[+ label ` and the value as
  // `value]={rep}`, quietly passing a row this gate must judge.
  const split = splitFragment('/explore#adhoc[+ label = value]={rep}');
  assert.equal(split.canonical, '/explore');
  assert.equal(splitFragment('/d/abc'), undefined);
  assert.deepEqual(check('/explore#adhoc[+ label = value]={rep}'), []);
  assert.equal(
    check('/explore#adhoc[+ label = value]=namespace').length,
    1,
    'and the same key with a seeded value is still caught',
  );
});

// ---------------------------------------------------------------------------
// The gate proper
// ---------------------------------------------------------------------------

test('a parameterized fragment passes', () => {
  assert.deepEqual(
    check(
      '/a/grafana-lokiexplore-app/explore#select[Filter by fields]={rep}',
      '/dashboards#select[Tag filter]={rep}',
      '/a/grafana-exploretraces-app/explore#attribute-list={rep}',
      '/a/grafana-lokiexplore-app/explore/service/{service}/logs#select[detected_level]={rep}',
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
  // The exact rows the committed inventories carried before #1872.
  for (const url of [
    '/dashboards#select[Tag filter]=cerberus (7)',
    '/a/grafana-lokiexplore-app/explore/service/{service}/logs#select[detected_level]=info',
    '/a/grafana-lokiexplore-app/explore/service/{service}/logs#select[detected_level]=warn',
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

test('an unparseable fragment fails rather than being passed over', () => {
  const problems = check('/x#not a control fragment');
  assert.equal(problems.length, 1);
  assert.match(problems[0], /cannot parse/);
});

test('an inventory whose surfaces are not an array is rejected', () => {
  const problems = checkInventory('f.json', { surfaces: 'nope' }, manifest());
  assert.equal(problems.length, 1);
  assert.match(problems[0], /surfaces must be an array/);
});

// ---------------------------------------------------------------------------
// The runner
// ---------------------------------------------------------------------------

const problemsOf = (result) => (Array.isArray(result) ? result : result.problems);

test('an unparseable inventory fails rather than being passed over', () => {
  const dir = manifestDir({});
  writeFileSync(join(dir, 'grafana-surface-inventory.broken.json'), '{ not json');
  const problems = problemsOf(run(dir));
  assert.equal(problems.length, 1);
  assert.match(problems[0], /not parseable JSON/);
});

test('a malformed manifest is reported instead of the inventories', () => {
  const dir = manifestDir({ representativePlaceholder: undefined });
  writeFileSync(
    join(dir, 'grafana-surface-inventory.x.json'),
    JSON.stringify({ doc: 'd', stack: 'x', surfaces: [] }),
  );
  const problems = problemsOf(run(dir));
  assert.equal(problems.length, 1);
  assert.match(problems[0], /representativePlaceholder/);
});

test('the runner refuses a directory holding no inventories', () => {
  const problems = problemsOf(run(manifestDir({})));
  assert.equal(problems.length, 1);
  assert.match(problems[0], /a gate that reads nothing checks nothing/);
});

test('the runner discovers every committed inventory', () => {
  // Stack discovery is by filename pattern, so a third stack is covered
  // the day it registers — but only if discovery actually finds them.
  const result = run(CRAWL_DIR);
  assert.ok(!Array.isArray(result));
  assert.deepEqual(result.files, [
    'grafana-surface-inventory.compose.json',
    'grafana-surface-inventory.k3d.json',
  ]);
});

test('the committed inventories are pure', () => {
  const problems = problemsOf(run(CRAWL_DIR));
  assert.deepEqual(
    problems,
    [],
    `the committed surface inventories must pin no data-derived literal:\n${problems.join('\n')}`,
  );
});
