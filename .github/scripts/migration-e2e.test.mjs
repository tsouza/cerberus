// migration-e2e.test.mjs — node:test guard for the Layer-14 migration
// coverage ratchet.
//
// Runs on the CHEAP required `lint` lane (`node --test`) — no setup-node, no
// deps, no Docker — alongside the real `MODE=verify`. Two jobs:
//
//   1. pin the doc parsers against the LIVE docs/migration-testing.md, so a
//      story row, a Tier(s) cell or an archetype name cannot drift out from
//      under the anchors the ratchet derives;
//   2. prove every detector actually FIRES. A ratchet whose detectors have
//      rotted into no-ops reports zero violations forever and looks exactly
//      like a healthy one, which is the failure mode this file exists to
//      prevent (the same idiom compose-smoke-matrix.test.mjs uses).

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

import {
  parseStories,
  parseTierMap,
  parseArchetypes,
  collectViolations,
  buildMatrix,
  requestedTiers,
  nonRunnableTiers,
  KNOWN_TIERS,
  BASELINE_SCHEMA_VERSION,
  TIER0_TIMEOUT_MIN,
} from './migration-e2e.mjs';

const DOC = readFileSync('docs/migration-testing.md', 'utf8');
const BASELINE = JSON.parse(readFileSync('test/e2e/migration/coverage-baseline.json', 'utf8'));

// The eight archetypes and the seven stories Tier 0 covers today. Written out
// so a synthetic fixture stays readable; the live-tree assertions below check
// them against the doc rather than trusting these literals.
const ARCHETYPES = [
  'already-otel',
  'kube-prometheus-stack',
  'mimir-cortex',
  'prometheus-thanos',
  'regulated-airgapped',
  'saas-repatriation',
  'three-signal',
  'victoriametrics',
];
const TIER0_STORIES = ['MIG-01', 'MIG-03', 'MIG-04', 'MIG-05', 'MIG-10', 'MIG-14', 'MIG-26'];

// The tier-1 stories the live feature tree covers today (MIG-16, MIG-17 —
// this PR's dual-backend scenarios).
const TIER1_STORIES = ['MIG-16', 'MIG-17'];

// A synthetic scenario in the enumerator's emitted shape.
function scenario(story, overrides = {}) {
  return {
    feature: `test/e2e/migration/features/${story}.feature`,
    line: 7,
    keyword: 'Scenario',
    name: `${story} offline`,
    stories: [story],
    tiers: ['tier0'],
    archetypes: ['regulated-airgapped'],
    unknown_tags: [],
    steps: [
      { type: 'Context', text: 'the committed fixtures for each tagged archetype' },
      { type: 'Action', text: 'the operator runs the offline command' },
      { type: 'Outcome', text: 'the artifact matches its committed golden byte for byte' },
    ],
    ...overrides,
  };
}

// A clean synthetic world: the tier-0 scenarios plus the tier-1 scenarios
// (MIG-16, MIG-17) over the real story list — the same shape the live
// feature tree carries today, so it agrees with the committed baseline.
function world(overrides = {}) {
  const scenarios = [
    ...TIER0_STORIES.map((s) => scenario(s)),
    ...TIER1_STORIES.map((s) => scenario(s, { tiers: ['tier1'], archetypes: ['three-signal'] })),
  ];
  const stories = parseStories(DOC);
  return {
    scenarios,
    stories,
    tierMap: parseTierMap(DOC),
    archetypes: ARCHETYPES,
    archetypeDirNames: ARCHETYPES,
    featureFiles: [...TIER0_STORIES, ...TIER1_STORIES].map((s) => `${s}.feature`),
    baseline: BASELINE,
    ...overrides,
  };
}

const codes = (violations) => violations.map((v) => v.code);

// --- 1. the live doc parses clean -------------------------------------------

test('live doc: section 4 lists MIG-01..MIG-26 contiguously', () => {
  const stories = parseStories(DOC);
  assert.equal(stories.length, 26);
  assert.equal(stories[0], 'MIG-01');
  assert.equal(stories.at(-1), 'MIG-26');
  for (let i = 0; i < stories.length; i += 1) {
    assert.equal(stories[i], `MIG-${String(i + 1).padStart(2, '0')}`);
  }
});

test('live doc: section 6 declares a tier for every story, split-tier exactly MIG-10/14/26', () => {
  const tierMap = parseTierMap(DOC);
  const stories = parseStories(DOC);
  assert.equal(tierMap.size, stories.length);
  for (const story of stories) {
    const tiers = tierMap.get(story);
    assert.ok(Array.isArray(tiers) && tiers.length > 0, `${story} declares no tier`);
    for (const t of tiers) assert.ok(KNOWN_TIERS.includes(t), `${story} declares unknown ${t}`);
  }
  const split = stories.filter((s) => tierMap.get(s).length > 1);
  assert.deepEqual(split, ['MIG-10', 'MIG-14', 'MIG-26']);
  const total = [...tierMap.values()].reduce((n, t) => n + t.length, 0);
  assert.equal(total, BASELINE.scenarios_total);
});

test('live doc: section 7 lists the eight archetypes the fixtures ship', () => {
  assert.deepEqual(parseArchetypes(DOC), [...ARCHETYPES].sort());
});

test('the committed baseline declares the schema version the ratchet reads', () => {
  assert.equal(BASELINE.schema_version, BASELINE_SCHEMA_VERSION);
  assert.equal(BASELINE.stories_total, 26);
  assert.equal(BASELINE.scenarios_covered, TIER0_STORIES.length + TIER1_STORIES.length);
});

// --- 2. a clean set is clean -------------------------------------------------

test('a clean synthetic scenario set yields zero violations', () => {
  assert.deepEqual(collectViolations(world()), []);
});

// --- 3..15. every detector fires --------------------------------------------

test('V14 fires when a story loses its scenario', () => {
  const w = world();
  w.scenarios = w.scenarios.slice(1);
  w.featureFiles = w.featureFiles.slice(1);
  const v = collectViolations(w);
  assert.ok(codes(v).includes('V14'), `expected V14, got ${JSON.stringify(v)}`);
});

test('V15 fires when coverage grows without the baseline being raised', () => {
  const w = world();
  w.scenarios = [...w.scenarios, scenario('MIG-02', { tiers: ['tier1'] })];
  w.featureFiles = [...w.featureFiles, 'MIG-02.feature'];
  const v = collectViolations(w);
  assert.ok(codes(v).includes('V15'), `expected V15, got ${JSON.stringify(v)}`);
});

test('V16 fires when a scenario is deleted even if the baseline floor is lowered in lockstep', () => {
  // V14 is an AGGREGATE floor: decrementing scenarios_covered in the same
  // diff that deletes a scenario keeps it quiet. V16 is keyed per (story,
  // tier) straight from section 6, so it must still catch the deleted MIG-04
  // scenario even though the lockstep edit hides it from V14/V15.
  const w = world();
  w.scenarios = w.scenarios.filter((s) => !s.stories.includes('MIG-04'));
  w.featureFiles = w.featureFiles.filter((f) => f !== 'MIG-04.feature');
  w.baseline = { ...BASELINE, scenarios_covered: w.scenarios.length };
  const v = collectViolations(w);
  assert.ok(
    v.some((x) => x.code === 'V16' && x.message.includes('MIG-04')),
    `expected V16 naming MIG-04, got ${JSON.stringify(v)}`,
  );
  assert.ok(!codes(v).includes('V14'), 'V14 must not fire once the baseline is lowered in lockstep — V16 is the point of this test');
});

test('V1 fires when the story anchor loses a row from the middle of the run', () => {
  const stories = parseStories(DOC).filter((s) => s !== 'MIG-13');
  const v = collectViolations(world({ stories }));
  assert.ok(codes(v).includes('V1'), `expected V1, got ${JSON.stringify(v)}`);
});

test('V2 fires when the story anchor loses its last row', () => {
  // Truncating the tail leaves a contiguous run, so contiguity alone cannot
  // catch it — the baseline's stories_total is what pins the length.
  const v = collectViolations(world({ stories: parseStories(DOC).slice(0, -1) }));
  assert.ok(
    v.some((x) => x.code === 'V2' && x.message.includes('stories_total')),
    `expected the stories_total V2, got ${JSON.stringify(v)}`,
  );
});

test('V2 fires when the baseline disagrees with the doc', () => {
  const w = world({ baseline: { ...BASELINE, stories_total: 25, scenarios_total: 28 } });
  const v = collectViolations(w).filter((x) => x.code === 'V2');
  assert.equal(v.length, 2, `expected both baseline anchors to fail, got ${JSON.stringify(v)}`);
});

test('V3 fires when a Scenario carries no story tag', () => {
  const w = world();
  w.scenarios[0] = scenario('MIG-01', { stories: [] });
  assert.ok(codes(collectViolations(w)).includes('V3'));
});

test('V4 fires when a Scenario references a story the doc does not list', () => {
  const w = world();
  w.scenarios[0] = scenario('MIG-99');
  w.featureFiles = ['MIG-99.feature', ...w.featureFiles.slice(1)];
  const v = collectViolations(w);
  assert.ok(codes(v).includes('V4'), `expected V4, got ${JSON.stringify(v)}`);
});

test('V5 fires when one Scenario claims two tiers instead of one per tier', () => {
  const w = world();
  w.scenarios[0] = scenario('MIG-01', { tiers: ['tier0', 'tier1'] });
  assert.ok(codes(collectViolations(w)).includes('V5'));
});

test('V6 fires when a tier tag contradicts section 6', () => {
  const w = world();
  // Section 6 declares MIG-01 at tier 0 only.
  w.scenarios[0] = scenario('MIG-01', { tiers: ['tier1'] });
  const v = collectViolations(w);
  assert.ok(codes(v).includes('V6'), `expected V6, got ${JSON.stringify(v)}`);
});

test('V7 fires when two Scenarios cover the same story and tier', () => {
  const w = world();
  w.scenarios = [...w.scenarios, scenario('MIG-01')];
  w.baseline = { ...BASELINE, scenarios_covered: w.scenarios.length };
  const v = collectViolations(w);
  assert.ok(codes(v).includes('V7'), `expected V7, got ${JSON.stringify(v)}`);
});

test('V8 fires for a misnamed feature file and for one that contributes nothing', () => {
  const misnamed = world();
  misnamed.scenarios[0] = scenario('MIG-01', {
    feature: 'test/e2e/migration/features/assess.feature',
  });
  misnamed.featureFiles = ['assess.feature', ...misnamed.featureFiles.slice(1)];
  assert.ok(codes(collectViolations(misnamed)).includes('V8'));

  const empty = world();
  empty.featureFiles = [...empty.featureFiles, 'MIG-02.feature'];
  const v = collectViolations(empty);
  assert.ok(
    v.some((x) => x.code === 'V8' && x.message.includes('MIG-02.feature')),
    `expected the empty feature file to be flagged, got ${JSON.stringify(v)}`,
  );
});

test('V9 fires on a scenario-suppressing tag', () => {
  const w = world();
  w.scenarios[0] = scenario('MIG-01', { unknown_tags: ['@skip'] });
  const v = collectViolations(w);
  assert.ok(
    v.some((x) => x.code === 'V9' && x.message.includes('@skip')),
    `expected V9, got ${JSON.stringify(v)}`,
  );
});

test('V10 fires on an unknown archetype and on one with no fixture directory', () => {
  const unknown = world();
  unknown.scenarios[0] = scenario('MIG-01', { archetypes: ['not-a-real-one'] });
  assert.ok(codes(collectViolations(unknown)).includes('V10'));

  const undirected = world({ archetypeDirNames: ARCHETYPES.filter((a) => a !== 'three-signal') });
  undirected.scenarios[0] = scenario('MIG-01', { archetypes: ['three-signal'] });
  const v = collectViolations(undirected);
  assert.ok(
    v.some((x) => x.code === 'V10' && x.message.includes('no directory')),
    `expected the missing-fixture-directory V10, got ${JSON.stringify(v)}`,
  );
});

test('V11 fires when a Scenario has no Then step', () => {
  const w = world();
  w.scenarios[0] = scenario('MIG-01', {
    steps: [
      { type: 'Context', text: 'the committed fixtures' },
      { type: 'Action', text: 'the operator runs the offline command' },
    ],
  });
  const v = collectViolations(w);
  assert.ok(codes(v).includes('V11'), `expected V11, got ${JSON.stringify(v)}`);
});

test('V11 does not accept an And chained after a Then as the assertion', () => {
  const w = world();
  w.scenarios[0] = scenario('MIG-01', {
    steps: [
      { type: 'Context', text: 'the committed fixtures' },
      { type: 'Action', text: 'the operator runs the offline command' },
      { type: 'Conjunction', text: 'the artifact matches its committed golden' },
    ],
  });
  assert.ok(codes(collectViolations(w)).includes('V11'));
});

test('V12 fires on a numeric literal in step text', () => {
  const w = world();
  w.scenarios[0] = scenario('MIG-01', {
    steps: [
      { type: 'Context', text: 'the committed fixtures' },
      { type: 'Action', text: 'the operator harvests the corpus' },
      { type: 'Outcome', text: 'the corpus lists 26 queries' },
    ],
  });
  const v = collectViolations(w);
  assert.ok(codes(v).includes('V12'), `expected V12, got ${JSON.stringify(v)}`);
});

test('V13 fires on an operator in step text', () => {
  const w = world();
  w.scenarios[0] = scenario('MIG-01', {
    steps: [
      { type: 'Context', text: 'the committed fixtures' },
      { type: 'Action', text: 'the operator computes the runway' },
      { type: 'Outcome', text: 'the ttl >= the lookback' },
    ],
  });
  const v = collectViolations(w);
  assert.ok(codes(v).includes('V13'), `expected V13, got ${JSON.stringify(v)}`);
});

test('a Scenario Outline placeholder is not read as an operator', () => {
  const w = world();
  w.scenarios[0] = scenario('MIG-01', {
    keyword: 'Scenario Outline',
    steps: [
      { type: 'Context', text: 'the <case> cerberus schema environment' },
      { type: 'Action', text: 'the operator renders the ClickHouse schema offline' },
      { type: 'Outcome', text: 'the rendered schema matches the committed golden for that environment' },
    ],
  });
  assert.deepEqual(collectViolations(w), []);
});

// --- the matrix --------------------------------------------------------------

test('buildMatrix emits one tier-0 entry carrying every tier-0 story and its ceiling', () => {
  const { include } = buildMatrix(world().scenarios, { tiers: ['tier0'] });
  assert.equal(include.length, 1);
  assert.equal(include[0].name, 'tier0');
  assert.equal(include[0].tier, 'tier0');
  assert.deepEqual(include[0].stories, TIER0_STORIES);
  assert.equal(include[0].timeoutMinutes, TIER0_TIMEOUT_MIN);
});

test('a tier no scenario declares never reaches the matrix', () => {
  // `all` resolves to the tiers the feature tree actually declares — tier0
  // and tier1 here — so tier2 (which nothing in the synthetic world
  // declares) is filtered before the matrix is built rather than emitted as
  // a job with nothing to run.
  assert.deepEqual(requestedTiers('all', world().scenarios), ['tier0', 'tier1']);
});

test('a tier with no job is refused before a matrix entry can be built for it', () => {
  // A scenario tagged with a tier migration-e2e.yml has no job for would
  // silently never run. emit rejects it by name, and buildMatrix throws
  // rather than emitting an entry with no ceiling.
  assert.deepEqual(nonRunnableTiers(['tier0']), []);
  assert.deepEqual(nonRunnableTiers(['tier0', 'tier1', 'tier2']), ['tier1', 'tier2']);
  const scenarios = [scenario('MIG-16', { tiers: ['tier1'], archetypes: ['victoriametrics'] })];
  assert.throws(() => buildMatrix(scenarios, { tiers: ['tier1'] }), /no declared ceiling/);
});

test('requestedTiers resolves all to the tiers the tree declares, and rejects a bad name', () => {
  const scenarios = world().scenarios;
  assert.deepEqual(requestedTiers('all', scenarios), ['tier0', 'tier1']);
  assert.deepEqual(requestedTiers('tier0', scenarios), ['tier0']);
  assert.deepEqual(requestedTiers('tier1', scenarios), ['tier1']);
  assert.equal(requestedTiers('nightly', scenarios), null);
});
