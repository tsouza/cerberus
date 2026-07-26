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
  tiersWithScenarios,
  storiesForTier,
  rollUp,
  KNOWN_TIERS,
  TIER_JOBS,
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

// The tier-1-only stories the live feature tree covers today: MIG-16/MIG-17
// (the mechanism-proving scenarios); MIG-02/MIG-06/MIG-07/MIG-08 (live
// cardinality inventory, ingest bridge, scrape parity, fault injection);
// MIG-11/MIG-12 (label mapping and metric-type/histogram fidelity); MIG-13
// (recording-rule read-back — its Tier-2 write-back half is a SEPARATE
// scenario in the same file, listed in TIER2_SPLIT_STORIES);
// MIG-15/MIG-20/MIG-21 (tenant isolation, tolerant downsample, trace/log
// correlation); and MIG-22/MIG-23/MIG-25 (cutover flip/revert,
// ingest-boundary, residual-reader decommission gate).
const TIER1_STORIES = [
  'MIG-02',
  'MIG-06',
  'MIG-07',
  'MIG-08',
  'MIG-11',
  'MIG-12',
  'MIG-13',
  'MIG-15',
  'MIG-16',
  'MIG-17',
  'MIG-20',
  'MIG-21',
  'MIG-22',
  'MIG-23',
  'MIG-25',
];

// Split-tier stories that carry a Tier-1 Scenario ALONGSIDE their Tier-0 one:
// MIG-10's live-schema-diff half, MIG-14's live-retention half and MIG-26's
// live-retention-vs-compliance-mandate half, each added in the same feature
// file as its Tier-0 half.
const TIER1_SPLIT_STORIES = ['MIG-10', 'MIG-14', 'MIG-26'];

// The tier-2-only stories the live feature tree covers today, all against the
// ruler substrate: MIG-09 (the shadow ruler evaluates against cerberus and its
// recording-rule output round-trips back through it), MIG-18 (the shadow
// ruler's own fire/resolve lifecycle at the dead-end receiver), MIG-19
// (recorded-sample parity against a live re-evaluation) and MIG-24 (the staged
// cutover gate).
const TIER2_STORIES = ['MIG-09', 'MIG-18', 'MIG-19', 'MIG-24'];

// Split-tier stories that carry a Tier-2 Scenario ALONGSIDE their Tier-1 one.
// MIG-13 is the only one: section 6 declares it at "1, 2" — the landing-zone
// read-back is provable on the dual-backend stack, but the ruler write-back
// that produces the recorded series needs the ruler tier. Both Scenarios live
// in MIG-13.feature, so this contributes no additional feature file.
const TIER2_SPLIT_STORIES = ['MIG-13'];

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

// A clean synthetic world: the tier-0 scenarios, the tier-1-only scenarios,
// the tier-2-only scenarios and the extra-tier halves of both split-tier
// groups, over the real story list — the same shape the live feature tree
// carries today, so it agrees with the committed baseline.
//
// Keeping this in step with the live tree is the whole point: a synthetic
// world that knows only about the tiers that existed when it was written
// reports "clean" while the real tree has grown past the committed floor,
// which is precisely how a stale scenarios_covered ships.
function world(overrides = {}) {
  const scenarios = [
    ...TIER0_STORIES.map((s) => scenario(s)),
    ...TIER1_STORIES.map((s) => scenario(s, { tiers: ['tier1'], archetypes: ['three-signal'] })),
    ...TIER1_SPLIT_STORIES.map((s) =>
      scenario(s, { name: `${s} tier1`, tiers: ['tier1'], archetypes: ['three-signal'] }),
    ),
    ...TIER2_STORIES.map((s) => scenario(s, { tiers: ['tier2'], archetypes: ['three-signal'] })),
    ...TIER2_SPLIT_STORIES.map((s) =>
      scenario(s, { name: `${s} tier2`, tiers: ['tier2'], archetypes: ['three-signal'] }),
    ),
  ];
  const stories = parseStories(DOC);
  return {
    scenarios,
    stories,
    tierMap: parseTierMap(DOC),
    archetypes: ARCHETYPES,
    archetypeDirNames: ARCHETYPES,
    // Both SPLIT groups' scenarios live in the SAME feature file as the half
    // already counted (TIER0_STORIES for the tier-1 splits, TIER1_STORIES for
    // MIG-13's tier-2 split), so neither contributes an additional file here.
    featureFiles: [...TIER0_STORIES, ...TIER1_STORIES, ...TIER2_STORIES].map((s) => `${s}.feature`),
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

test('live doc: section 6 declares a tier for every story, split-tier exactly MIG-10/13/14/26', () => {
  const tierMap = parseTierMap(DOC);
  const stories = parseStories(DOC);
  assert.equal(tierMap.size, stories.length);
  for (const story of stories) {
    const tiers = tierMap.get(story);
    assert.ok(Array.isArray(tiers) && tiers.length > 0, `${story} declares no tier`);
    for (const t of tiers) assert.ok(KNOWN_TIERS.includes(t), `${story} declares unknown ${t}`);
  }
  const split = stories.filter((s) => tierMap.get(s).length > 1);
  assert.deepEqual(split, ['MIG-10', 'MIG-13', 'MIG-14', 'MIG-26']);
  const total = [...tierMap.values()].reduce((n, t) => n + t.length, 0);
  assert.equal(total, BASELINE.scenarios_total);
});

test('live doc: section 7 lists the eight archetypes the fixtures ship', () => {
  assert.deepEqual(parseArchetypes(DOC), [...ARCHETYPES].sort());
});

test('the committed baseline declares the schema version the ratchet reads', () => {
  assert.equal(BASELINE.schema_version, BASELINE_SCHEMA_VERSION);
  assert.equal(BASELINE.stories_total, 26);
  assert.equal(
    BASELINE.scenarios_covered,
    TIER0_STORIES.length +
      TIER1_STORIES.length +
      TIER1_SPLIT_STORIES.length +
      TIER2_STORIES.length +
      TIER2_SPLIT_STORIES.length,
  );
  // Every declared (story, tier) pair is covered: the enumerated floor has
  // caught up with what section 6 asks for, so the two numbers are equal
  // rather than merely ordered.
  assert.equal(BASELINE.scenarios_covered, BASELINE.scenarios_total);
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

  // Every one of the 26 stories now has a feature file that contributes a
  // Scenario, so the "contributes nothing" case needs a file no story claims.
  const orphan = 'MIG-99.feature';
  const empty = world();
  empty.featureFiles = [...empty.featureFiles, orphan];
  const v = collectViolations(empty);
  assert.ok(
    v.some((x) => x.code === 'V8' && x.message.includes(orphan)),
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
  // `all` resolves to the tiers the feature tree actually declares. Every
  // known tier is declared today, so drop one from the synthetic world and it
  // must drop out of the resolution rather than being emitted as a job with
  // nothing to run.
  assert.deepEqual(requestedTiers('all', world().scenarios), ['tier0', 'tier1', 'tier2']);
  const withoutTier2 = world().scenarios.filter((s) => !s.tiers.includes('tier2'));
  assert.deepEqual(requestedTiers('all', withoutTier2), ['tier0', 'tier1']);
});

test('a tier with no job is refused before a matrix entry can be built for it', () => {
  // A scenario tagged with a tier migration-e2e.yml has no job for would
  // silently never run. emit rejects it by name, and buildMatrix throws
  // rather than emitting an entry with no ceiling. All three KNOWN_TIERS have
  // a job today, so the guard is exercised against a tier ordinal beyond them
  // — which is exactly the state tier2 itself was in before its job landed.
  const undeclaredTier = 'tier3';
  assert.ok(!KNOWN_TIERS.includes(undeclaredTier), 'the guard needs a tier with no declared job');
  assert.deepEqual(nonRunnableTiers(KNOWN_TIERS), []);
  assert.deepEqual(nonRunnableTiers([...KNOWN_TIERS, undeclaredTier]), [undeclaredTier]);
  const scenarios = [scenario('MIG-09', { tiers: [undeclaredTier], archetypes: ['kube-prometheus-stack'] })];
  assert.throws(() => buildMatrix(scenarios, { tiers: [undeclaredTier] }), /no declared ceiling/);
});

test('a runnable tier whose job is not matrix-driven stays out of the matrix', () => {
  // tier1's and tier2's jobs each bring a compose stack up around the suite,
  // so they are fixed stanzas, not matrix shards. Emitting an entry for either
  // here would spawn a matrix shard running that tier's suite on a bare runner
  // with no stack behind it — green for the wrong reason, or red for a reason
  // that has nothing to do with the scenarios. It would also be named exactly
  // like the real fixed job, since both shapes render as `migration-<tier>`.
  assert.equal(TIER_JOBS.tier1.matrixDriven, false);
  assert.equal(TIER_JOBS.tier2.matrixDriven, false);
  const { include } = buildMatrix(world().scenarios, { tiers: KNOWN_TIERS });
  assert.deepEqual(include.map((e) => e.tier), ['tier0']);
  // ...but both are still RUNNABLE, and still selected, so the roll-up expects
  // a result from each.
  assert.deepEqual(nonRunnableTiers(['tier1', 'tier2']), []);
  assert.deepEqual(tiersWithScenarios(world().scenarios, KNOWN_TIERS), ['tier0', 'tier1', 'tier2']);
});

test('every tier2 story the live tree declares carries a scenario in the synthetic world', () => {
  // The synthetic world is only a guard if it mirrors the live tree. Section 6
  // is the anchor for which stories reach tier 2, so derive the expectation
  // from the doc rather than from the same literal list under test.
  const tierMap = parseTierMap(DOC);
  const declared = [...tierMap.entries()]
    .filter(([, tiers]) => tiers.includes('tier2'))
    .map(([story]) => story)
    .sort();
  assert.deepEqual(declared, [...TIER2_STORIES, ...TIER2_SPLIT_STORIES].sort());
  assert.deepEqual(storiesForTier(world().scenarios, 'tier2'), declared);
});

test('tiersWithScenarios drops a requested tier the tree has no scenario for', () => {
  const withoutTier2 = world().scenarios.filter((s) => !s.tiers.includes('tier2'));
  assert.deepEqual(storiesForTier(withoutTier2, 'tier2'), []);
  assert.deepEqual(tiersWithScenarios(withoutTier2, ['tier0', 'tier2']), ['tier0']);
});

test('the roll-up holds every selected tier to success and nothing else', () => {
  assert.deepEqual(rollUp({ expected: ['tier0', 'tier1'], results: { tier0: 'success', tier1: 'success' } }), []);
  // `cancelled` is the case a `contains(needs.*.result, 'failure')` fold would
  // wave through — the whole reason this is not that shape.
  assert.deepEqual(
    rollUp({ expected: ['tier0', 'tier1'], results: { tier0: 'success', tier1: 'cancelled' } }),
    ['tier1 did not succeed (cancelled)'],
  );
  // A selected tier that was skipped never ran its scenarios, so it is a
  // failure, not a pass.
  assert.deepEqual(
    rollUp({ expected: ['tier0', 'tier1'], results: { tier0: 'skipped', tier1: 'success' } }),
    ['tier0 did not succeed (skipped)'],
  );
});

test('the roll-up accepts a narrowed dispatch, but not a tier that ran unselected', () => {
  // TIER=tier0 dispatch: tier1's job is gated off, so `skipped` is correct.
  assert.deepEqual(rollUp({ expected: ['tier0'], results: { tier0: 'success', tier1: 'skipped' } }), []);
  // The gate and the roll-up read the same emit output, so a tier that ran
  // while unselected means they disagreed — a wiring bug, reported as one.
  assert.deepEqual(
    rollUp({ expected: ['tier0'], results: { tier0: 'success', tier1: 'success' } }),
    ['tier1 ran (success) but was not in the tier set this run selected'],
  );
  assert.deepEqual(
    rollUp({ expected: ['tier0', 'tier1'], results: { tier0: 'success' } }),
    ['tier1 was expected to run but reported no result at all'],
  );
});

test('requestedTiers resolves all to the tiers the tree declares, and rejects a bad name', () => {
  const scenarios = world().scenarios;
  assert.deepEqual(requestedTiers('all', scenarios), KNOWN_TIERS);
  assert.deepEqual(requestedTiers('tier0', scenarios), ['tier0']);
  assert.deepEqual(requestedTiers('tier1', scenarios), ['tier1']);
  assert.equal(requestedTiers('nightly', scenarios), null);
});

test('narrowing to a tier also selects the tiers its job needs', () => {
  // migration-tier2 `needs:` migration-tier1. Selecting tier2 alone gated
  // tier1 off, which skipped tier2 through the needs-cascade — a dispatch
  // option that could only ever run nothing and then report the very tier the
  // operator asked for as failed (observed: run 30210906040). Selecting a tier
  // must select whatever makes it runnable.
  assert.deepEqual(requestedTiers('tier2', world().scenarios), ['tier1', 'tier2']);
});

test('the roll-up holds tier2 to success alongside the other two', () => {
  // tier2's job is `needs: migration-tier1`, so a red tier1 makes tier2
  // `skipped` — which the fold must report as a failure, not wave through as
  // "nothing to see". Otherwise a broken tier1 would hide tier2 entirely.
  assert.deepEqual(rollUp({ expected: KNOWN_TIERS, results: { tier0: 'success', tier1: 'success', tier2: 'success' } }), []);
  assert.deepEqual(
    rollUp({ expected: KNOWN_TIERS, results: { tier0: 'success', tier1: 'failure', tier2: 'skipped' } }),
    ['tier1 did not succeed (failure)', 'tier2 did not succeed (skipped)'],
  );
});
