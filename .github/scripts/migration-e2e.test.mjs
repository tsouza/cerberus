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
  parsePassAssertions,
  hashPassAssertion,
  collectViolations,
  collectAttestations,
  outcomesFromReports,
  attestedCount,
  reportPathFor,
  buildMatrix,
  requestedTiers,
  nonRunnableTiers,
  tiersWithScenarios,
  storiesForTier,
  rollUp,
  KNOWN_TIERS,
  TIER_JOBS,
  BASELINE_SCHEMA_VERSION,
  PIN_SCHEMA_VERSION,
  REPORT_SUFFIX,
  TIER0_TIMEOUT_MIN,
} from './migration-e2e.mjs';

const DOC = readFileSync('docs/migration-testing.md', 'utf8');
const BASELINE = JSON.parse(readFileSync('test/e2e/migration/coverage-baseline.json', 'utf8'));
const PIN = JSON.parse(readFileSync('test/e2e/migration/pass-assertions.pin.json', 'utf8'));

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
    passAssertions: parsePassAssertions(DOC),
    pin: PIN,
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
  for (const tier of KNOWN_TIERS) {
    assert.deepEqual(requestedTiers(tier, scenarios), [tier]);
  }
  assert.equal(requestedTiers('nightly', scenarios), null);
});

// --- 3. the PASS-assertion pin ----------------------------------------------
//
// The ratchet derives its anchors LIVE from section 6, so narrowing a PASS
// cell used to be a valid route to "full coverage": weaken the doc, implement
// the weaker thing, stay green. The pin does not forbid narrowing — it forbids
// narrowing SILENTLY. These tests hold its detectors to firing, because a pin
// that rotted into a no-op looks exactly like a pin nobody has had to update.

test('live doc: the pin covers every section-6 row and matches it today', () => {
  const cells = parsePassAssertions(DOC);
  const stories = parseStories(DOC);
  assert.equal(cells.size, stories.length);
  assert.equal(Object.keys(PIN.assertions).length, stories.length);
  assert.equal(PIN.schema_version, PIN_SCHEMA_VERSION);
  assert.equal(PIN.algorithm, 'sha256');
  for (const story of stories) {
    const cell = cells.get(story);
    assert.ok(cell, `${story} has no section-6 row`);
    assert.equal(cell.columns, 5, `${story}'s row is not | ID | Tier(s) | CLI | Fixtures | PASS |`);
    assert.ok(cell.pass.length > 0, `${story}'s PASS cell is empty`);
    assert.equal(PIN.assertions[story].sha256, hashPassAssertion(cell.pass), `${story}'s pin is stale`);
    assert.equal(PIN.assertions[story].tiers, cell.tiers, `${story}'s pinned Tier(s) cell is stale`);
  }
});

test('the pin hash ignores a reflow but not a wording change', () => {
  // Markdownlint reflows these very wide tables, and a column realignment
  // shifts every cell's padding, so whitespace must not be a failure — while
  // a deleted clause must be.
  const cell = 'the boundary is observable;   the old backend is\n  kept read-only';
  assert.equal(
    hashPassAssertion(cell),
    hashPassAssertion(' the boundary is observable; the old backend is kept read-only '),
  );
  assert.notEqual(hashPassAssertion(cell), hashPassAssertion('the boundary is observable'));
});

test('V19 fires when a PASS cell is narrowed — the MIG-23 shape, verbatim', () => {
  // The observed narrowing: MIG-23's "the old backend is kept read-only as a
  // historical tier" clause deleted from the PASS cell, in the same commit
  // that implemented the narrower thing.
  const w = world();
  const cells = parsePassAssertions(DOC);
  const mig23 = cells.get('MIG-23');
  w.passAssertions = new Map(cells).set('MIG-23', {
    ...mig23,
    pass: mig23.pass.replace(' the old backend is kept read-only as a historical tier;', ''),
  });
  const v = collectViolations(w);
  assert.ok(
    v.some((x) => x.code === 'V19' && x.message.includes('MIG-23') && x.message.includes('IN THE SAME DIFF')),
    `expected V19 naming MIG-23, got ${JSON.stringify(v)}`,
  );
});

test('V20 fires when a Tier(s) cell is narrowed', () => {
  // The other observed shape: MIG-13 moved from "1, 2" to "2", dropping its
  // Tier-1 read-back half.
  const w = world();
  const cells = parsePassAssertions(DOC);
  w.passAssertions = new Map(cells).set('MIG-13', { ...cells.get('MIG-13'), tiers: '2' });
  assert.ok(codes(collectViolations(w)).includes('V20'));
});

test('V18 fires in both directions: a new story with no pin, a pin with no story', () => {
  const added = world();
  added.passAssertions = new Map(parsePassAssertions(DOC)).set('MIG-27', {
    tiers: '0',
    pass: 'something new',
    columns: 5,
  });
  assert.ok(
    collectViolations(added).some((x) => x.code === 'V18' && x.message.includes('pins no PASS assertion')),
    'expected V18 for a section-6 row with no pin entry',
  );

  const orphaned = world();
  orphaned.pin = { ...PIN, assertions: { ...PIN.assertions, 'MIG-99': { tiers: '0', sha256: 'deadbeef' } } };
  assert.ok(
    collectViolations(orphaned).some((x) => x.code === 'V18' && x.message.includes('MIG-99')),
    'expected V18 for a pin entry with no section-6 row',
  );
});

test('V17 fires when the pin declares a schema or algorithm the reader does not implement', () => {
  const bumped = world();
  bumped.pin = { ...PIN, schema_version: PIN_SCHEMA_VERSION + 1 };
  assert.ok(codes(collectViolations(bumped)).includes('V17'));

  const rehashed = world();
  rehashed.pin = { ...PIN, algorithm: 'md5' };
  assert.ok(codes(collectViolations(rehashed)).includes('V17'));
});

test('V21 fires on a malformed section-6 row rather than hashing the wrong cell', () => {
  const w = world();
  w.passAssertions = new Map(parsePassAssertions(DOC)).set('MIG-01', {
    tiers: null,
    pass: null,
    columns: 4,
  });
  assert.ok(codes(collectViolations(w)).includes('V21'));
});

// --- 4. attestation: coverage means EXECUTED, not enumerated ----------------
//
// MODE=verify walks feature FILES. On 2026-07-26 it reported "scenarios 30/30
// across 26/26 stories; 0 violations" on a branch whose five tier-2 scenarios
// had never executed once, because their job had been skipped by a `needs:`
// cascade after tier 1 failed. These tests hold the detector that closes that
// to firing.

// element — one cucumber-JSON scenario in godog's own emitted shape. godog
// prepends the FEATURE's tags to every element, which is where the migration
// features declare @MIG-nn / @tierN, so the tags alone carry the (story, tier)
// key the coverage ratchet counts on.
function element(story, tier, { statuses = ['passed'], name = `${story} scenario`, line = 9 } = {}) {
  return {
    id: `${story};${name}`,
    keyword: 'Scenario',
    name,
    line,
    type: 'scenario',
    tags: [
      { name: `@${story}`, line: 1 },
      { name: `@${tier}`, line: 1 },
    ],
    steps: statuses.map((status, i) => ({
      keyword: 'Then ',
      name: `step ${i}`,
      line: line + i + 1,
      result: { status },
    })),
  };
}

function report(path, elements) {
  return {
    path,
    features: elements.map((el) => ({
      uri: `../../features/${el.tags[0].name.slice(1)}.feature`,
      elements: [el],
    })),
  };
}

// A world whose enumeration is two tier-0 scenarios and one tier-1 scenario,
// with a report that (by default) covers all three.
function attestWorld({ tiers = ['tier0', 'tier1'], story = '', elements = null } = {}) {
  const scenarios = [
    scenario('MIG-01'),
    scenario('MIG-03'),
    scenario('MIG-02', { tiers: ['tier1'], archetypes: ['three-signal'] }),
  ];
  const els = elements || [element('MIG-01', 'tier0'), element('MIG-03', 'tier0'), element('MIG-02', 'tier1')];
  const reports = [report('build/migration-reports/all.cucumber.json', els)];
  const { outcomes, unkeyable } = outcomesFromReports(reports);
  return { scenarios, tiers, story, outcomes, unkeyable, reportCount: reports.length };
}

test('a fully-executed run attests clean', () => {
  assert.deepEqual(collectAttestations(attestWorld()), []);
});

test('A1 fires for a counted scenario that never ran — the skipped-tier hole', () => {
  // The exact 2026-07-26 shape: tier1 is SELECTED, its job never ran, so no
  // report mentions its scenarios — while the enumeration still counts them.
  const w = attestWorld({ elements: [element('MIG-01', 'tier0'), element('MIG-03', 'tier0')] });
  const a = collectAttestations(w);
  assert.deepEqual(
    a.map((x) => x.code),
    ['A1'],
  );
  assert.ok(a[0].message.includes('MIG-02'), a[0].message);
  assert.ok(a[0].message.includes('tier1'), a[0].message);
  assert.ok(a[0].message.includes('NEVER RAN'), a[0].message);
});

test('A0 fires when no run report exists at all', () => {
  const w = attestWorld();
  w.outcomes = new Map();
  w.reportCount = 0;
  const a = collectAttestations(w);
  assert.ok(
    a.some((x) => x.code === 'A0'),
    `expected A0, got ${JSON.stringify(a)}`,
  );
});

test('A2 fires for a scenario that ran but did not pass, on every non-passed status', () => {
  for (const status of ['failed', 'skipped', 'undefined', 'pending', 'ambiguous']) {
    const w = attestWorld({
      elements: [
        element('MIG-01', 'tier0', { statuses: ['passed', status] }),
        element('MIG-03', 'tier0'),
        element('MIG-02', 'tier1'),
      ],
    });
    const a = collectAttestations(w);
    assert.ok(
      a.some((x) => x.code === 'A2' && x.message.includes('MIG-01')),
      `expected A2 for step status ${status}, got ${JSON.stringify(a)}`,
    );
  }
});

test('A2 fires for a scenario whose report records no steps at all', () => {
  // An element with an empty step list has nothing that could have run, so it
  // is not evidence of a pass.
  const w = attestWorld({
    elements: [element('MIG-01', 'tier0', { statuses: [] }), element('MIG-03', 'tier0'), element('MIG-02', 'tier1')],
  });
  assert.ok(collectAttestations(w).some((x) => x.code === 'A2' && x.message.includes('MIG-01')));
});

test('A1 fires when fewer nodes ran than the enumeration declares', () => {
  const w = attestWorld({ tiers: ['tier0'] });
  w.scenarios = [scenario('MIG-01'), scenario('MIG-01', { name: 'MIG-01 second' }), scenario('MIG-03')];
  const a = collectAttestations(w);
  assert.ok(
    a.some((x) => x.code === 'A1' && x.message.includes('never executed')),
    `expected an under-count A1, got ${JSON.stringify(a)}`,
  );
});

test('a Scenario Outline expanding to more elements than nodes is not a violation', () => {
  const w = attestWorld({
    tiers: ['tier0'],
    elements: [
      element('MIG-01', 'tier0', { name: 'outline row one', line: 9 }),
      element('MIG-01', 'tier0', { name: 'outline row two', line: 10 }),
      element('MIG-03', 'tier0'),
    ],
  });
  w.scenarios = [scenario('MIG-01'), scenario('MIG-03')];
  assert.deepEqual(collectAttestations(w), []);
});

test('A3 fires when a report attests a scenario the enumeration does not list', () => {
  const w = attestWorld({
    elements: [
      element('MIG-01', 'tier0'),
      element('MIG-03', 'tier0'),
      element('MIG-02', 'tier1'),
      element('MIG-99', 'tier0'),
    ],
  });
  assert.ok(collectAttestations(w).some((x) => x.code === 'A3' && x.message.includes('MIG-99')));
});

test('A4 fires when a report element cannot be attributed to one story and one tier', () => {
  const bad = element('MIG-01', 'tier0');
  bad.tags = [
    { name: '@MIG-01', line: 1 },
    { name: '@MIG-05', line: 1 },
    { name: '@tier0', line: 1 },
  ];
  const w = attestWorld({ elements: [bad, element('MIG-03', 'tier0'), element('MIG-02', 'tier1')] });
  assert.ok(
    collectAttestations(w).some((x) => x.code === 'A4'),
    'expected A4 for an element carrying two story tags',
  );
});

test('attestation respects the tier set the run SELECTED', () => {
  // A dispatch narrowed to tier0 must not fail because tier1's scenarios did
  // not run — the whole reason attest reads emit's `tiers` output rather than
  // attesting everything the feature tree declares.
  const w = attestWorld({ tiers: ['tier0'], elements: [element('MIG-01', 'tier0'), element('MIG-03', 'tier0')] });
  assert.deepEqual(collectAttestations(w), []);
});

test('attestation respects a single-story dispatch without calling the rest drift', () => {
  // STORY=MIG-01 narrows what must be attested, but a report that still
  // mentions the other tier-0 scenarios is not corrupt — A3 is judged against
  // the enumeration, not against the narrowed selection.
  const w = attestWorld({ tiers: ['tier0'], story: 'MIG-01' });
  assert.deepEqual(collectAttestations(w), []);
});

test('attestedCount counts only scenarios a report proves executed AND passed', () => {
  const { outcomes } = outcomesFromReports([
    report('r.json', [element('MIG-01', 'tier0'), element('MIG-03', 'tier0', { statuses: ['failed'] })]),
  ]);
  const scenarios = [scenario('MIG-01'), scenario('MIG-03'), scenario('MIG-02', { tiers: ['tier1'] })];
  assert.equal(attestedCount(scenarios, outcomes), 1);
  // With no report at all — the `lint` job's case — nothing is attested, and
  // MODE=verify's notice reports that number rather than implying otherwise.
  assert.equal(attestedCount(scenarios, new Map()), 0);
});

test('the run report path is owned by this module, not by the workflow', () => {
  // MODE=run derives it from REPORT_DIR + the tier, so a tier job cannot ship
  // without a report by forgetting a step, and lib.SuiteFormat receives an
  // absolute path (`go test` runs a test binary with the PACKAGE directory as
  // its working directory, so a relative one would land in the wrong place).
  const p = reportPathFor('tier2', 'build/migration-reports');
  assert.ok(p.startsWith('/'), `expected an absolute path, got ${p}`);
  assert.ok(p.endsWith(`tier2${REPORT_SUFFIX}`), p);
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
