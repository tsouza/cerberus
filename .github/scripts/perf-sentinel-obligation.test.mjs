// perf-sentinel-obligation.test.mjs — node:test guard for #2370's
// same-PR obligation gate over the memory-bounding settings files.
//
// Pins both directions, matching forbid-deferral.test.mjs's own
// discipline: a change to a trigger file with no sentinel coverage and no
// waiver must FAIL, and every honest resolution (sentinel coverage, an
// open-issue waiver) must PASS — a test file that only asserted the
// rejection path would be satisfied by a gate that always fails, and one
// that only asserted the happy path would be satisfied by a gate that
// never fires at all.

import assert from 'node:assert/strict';
import test from 'node:test';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

import {
  CLASS_MEMORY_BOUNDING,
  CLASS_NEUTRAL,
  SENTINEL_FILES,
  SETTING_CONST_PATTERN,
  TRIGGER_FILES,
  WAIVER_PATTERN,
  changedCodeLines,
  classificationOf,
  memoryBoundingSurface,
  mechanismEdits,
  needsObligation,
  parseGoDecls,
  satisfiesViaSentinel,
  surfaceViolations,
  touchesTriggerFile,
  verdict,
  waiverRefs,
} from './perf-sentinel-obligation.mjs';

// The three real fixtures forbid-deferral.test.mjs already established for
// this repository — reused here rather than inventing new ones, since the
// citation-resolution shape (kind/state) is identical.
const RESOLVED = new Map([
  [1535, { kind: 'issue', state: 'open' }],
  [1486, { kind: 'issue', state: 'closed' }],
  [1143, { kind: 'pull-request', state: 'closed' }],
]);

// --- constants ---------------------------------------------------------------

test('the trigger and sentinel file lists are non-empty and disjoint', () => {
  assert.ok(TRIGGER_FILES.length > 0, 'an empty trigger list makes the gate vacuous');
  assert.ok(SENTINEL_FILES.length > 0, 'an empty sentinel list makes every obligation unsatisfiable');
  for (const f of TRIGGER_FILES) {
    assert.ok(!SENTINEL_FILES.includes(f), `${f} cannot be both a trigger and a satisfying sentinel file`);
  }
});

test('WAIVER_PATTERN compiles and is global (matchAll-safe)', () => {
  assert.ok(WAIVER_PATTERN.flags.includes('g'));
});

// --- waiverRefs ---------------------------------------------------------------

test('waiverRefs extracts the cited issue number', () => {
  assert.deepEqual(waiverRefs('PERF-SENTINEL-WAIVER: #1535'), [1535]);
});

test('waiverRefs is case- and spacing-tolerant is NOT required — exact label only', () => {
  // Deliberately strict: this is an opt-in marker a human writes on purpose,
  // unlike forbid-deferral's markers which must catch loose prose. Requiring
  // the exact label keeps the citation searchable and greppable.
  assert.deepEqual(waiverRefs('perf-sentinel-waiver: #1535'), []);
});

test('waiverRefs finds multiple distinct citations and de-duplicates', () => {
  assert.deepEqual(
    waiverRefs('PERF-SENTINEL-WAIVER: #1535\n\nAlso PERF-SENTINEL-WAIVER: #1535 and PERF-SENTINEL-WAIVER: #1486'),
    [1486, 1535],
  );
});

test('waiverRefs returns empty on prose with no label at all', () => {
  assert.deepEqual(waiverRefs('This change tunes spill.go for #1535, no new sentinel needed.'), []);
});

test('waiverRefs handles null/undefined input without throwing', () => {
  assert.deepEqual(waiverRefs(undefined), []);
  assert.deepEqual(waiverRefs(null), []);
});

test('waiverRefs ignores a citation quoted inside a fenced code block', () => {
  // A commit message quoting another commit (a revert, a copy-pasted
  // template) can legitimately contain the waiver string without the
  // author actually citing it. Matches forbid-deferral.mjs's own
  // stripFencedBlocks treatment of quoted material.
  const text = ['This reverts commit abc123.', '', '```', 'PERF-SENTINEL-WAIVER: #1535', '```'].join('\n');
  assert.deepEqual(waiverRefs(text), []);
});

test('waiverRefs still finds a real citation sitting outside a fenced block in the same text', () => {
  const text = ['PERF-SENTINEL-WAIVER: #1486', '', '```', 'PERF-SENTINEL-WAIVER: #1535', '```'].join('\n');
  assert.deepEqual(waiverRefs(text), [1486]);
});

// --- the memory-bounding surface (#2893) --------------------------------------

// A miniature stand-in for the two trigger files, carrying one setting of each
// class plus the derivation chain a real memory bound has: the stamp, the value
// arithmetic behind it, and a plan predicate gating it. Synthetic rather than a
// slice of the real file so the two DIRECTIONS below stay readable, and so a
// future edit to the real source cannot silently make a test vacuous — the
// "real source" pins further down cover that half.
const FAKE_SPILL = [
  '// settingCap makes the aggregator spill.',
  '//',
  '// perf-sentinel: memory-bounding — caps in-RAM state before it spills.',
  'const settingCap = "max_bytes_before_external_group_by"',
  '',
  '// capDenominator halves the cap.',
  'const capDenominator int64 = 2',
  '',
  '// threshold derives the byte threshold from the live cap.',
  'func threshold(maxMemory int64) int64 {',
  '\treturn maxMemory / capDenominator',
  '}',
  '',
  '// applyCap stamps the spill threshold.',
  'func applyCap(ctx context.Context, maxMemory int64) context.Context {',
  '\treturn chclient.WithQuerySetting(ctx, settingCap, threshold(maxMemory))',
  '}',
  '',
].join('\n');

const FAKE_RULES = [
  '// settingTag annotates a query.',
  '//',
  '// perf-sentinel: neutral — a free-form annotation, never a budget.',
  'const settingTag = "log_comment"',
  '',
  '// shapeID computes the annotation value.',
  'func shapeID(plan chplan.Node) string {',
  '\treturn plan.Kind()',
  '}',
  '',
  '// applyTag stamps the annotation.',
  'func applyTag(ctx context.Context, plan chplan.Node) context.Context {',
  '\treturn chclient.WithQuerySetting(ctx, settingTag, shapeID(plan))',
  '}',
  '',
].join('\n');

const FAKE = { [TRIGGER_FILES[0]]: FAKE_RULES, [TRIGGER_FILES[1]]: FAKE_SPILL };

function unifiedDiff(file, lines) {
  return [`diff --git a/${file} b/${file}`, `--- a/${file}`, `+++ b/${file}`, '@@ -1,1 +1,1 @@', ...lines].join('\n');
}

test('parseGoDecls finds every top-level declaration, grouped consts included', () => {
  const names = parseGoDecls(
    ['// perf-sentinel: neutral — group doc.', 'const (', '\talpha = "a"', '\tbeta  = "b"', ')', '',
      'func gamma() {', '\treturn', '}'].join('\n'),
  ).map((d) => d.name);
  assert.deepEqual(names, ['alpha', 'beta', 'gamma']);
});

test('a grouped const member inherits the group doc when it has none of its own', () => {
  const decls = parseGoDecls(
    ['// perf-sentinel: memory-bounding — shared paragraph.', 'const (', '\talpha = "a"', ')'].join('\n'),
  );
  assert.equal(classificationOf(decls[0]), CLASS_MEMORY_BOUNDING);
});

test('the surface is the bounding const AND everything deriving or stamping it', () => {
  const surface = memoryBoundingSurface(FAKE);
  for (const want of ['settingCap', 'applyCap', 'threshold', 'capDenominator']) {
    assert.ok(surface.has(want), `${want} is part of the memory-bounding mechanism`);
  }
});

test('the surface does NOT leak into the neutral rules sharing the same files', () => {
  const surface = memoryBoundingSurface(FAKE);
  for (const nope of ['settingTag', 'applyTag', 'shapeID']) {
    assert.ok(!surface.has(nope), `${nope} bounds nothing and must stay outside the surface`);
  }
});

test('an unclassified setting const FAILS the gate rather than being assumed harmless', () => {
  const problems = surfaceViolations({
    [TRIGGER_FILES[0]]: 'const settingMystery = "some_knob"\n',
    [TRIGGER_FILES[1]]: FAKE_SPILL,
  });
  assert.equal(problems.length, 1);
  assert.match(problems[0], /settingMystery carries no .*perf-sentinel/);
});

test('a memory bound stamped through a bare string literal FAILS the gate', () => {
  const problems = surfaceViolations({
    [TRIGGER_FILES[0]]: [
      '// perf-sentinel: neutral — classified, but the stamp below dodges it.',
      'const settingTag = "log_comment"',
      'func sneak(ctx context.Context) context.Context {',
      '\treturn chclient.WithQuerySetting(ctx, "max_memory_usage", 1)',
      '}',
    ].join('\n'),
    [TRIGGER_FILES[1]]: FAKE_SPILL,
  });
  assert.equal(problems.length, 1);
  assert.match(problems[0], /bare literal "max_memory_usage"/);
});

test('an unreadable trigger file FAILS the gate instead of narrowing the surface', () => {
  const problems = surfaceViolations({ [TRIGGER_FILES[0]]: FAKE_RULES, [TRIGGER_FILES[1]]: null });
  assert.equal(problems.length, 1);
  assert.match(problems[0], /could not be read at HEAD/);
});

test('changedCodeLines reads REMOVALS as well as additions — #2364 was a deletion', () => {
  const diff = unifiedDiff(TRIGGER_FILES[1], ['-\tctx = applyCap(ctx, maxMemory)', '+\treturn ctx']);
  const changed = changedCodeLines(diff, TRIGGER_FILES);
  assert.deepEqual(changed.map((c) => [c.added, c.code]), [
    [false, 'ctx = applyCap(ctx, maxMemory)'],
    [true, 'return ctx'],
  ]);
});

test('changedCodeLines ignores files outside the trigger set', () => {
  const diff = unifiedDiff('internal/engine/engine.go', ['+\tctx = applyCap(ctx, maxMemory)']);
  assert.deepEqual(changedCodeLines(diff, TRIGGER_FILES), []);
});

// --- needsObligation: the two directions #2893 is about -----------------------

test('FIRES: deleting the spill stamp still obligates a sentinel', () => {
  const diff = unifiedDiff(TRIGGER_FILES[1], ['-\treturn chclient.WithQuerySetting(ctx, settingCap, threshold(maxMemory))']);
  const edits = mechanismEdits(changedCodeLines(diff, TRIGGER_FILES), memoryBoundingSurface(FAKE));
  assert.equal(edits.length, 1);
  assert.equal(needsObligation([TRIGGER_FILES[1]], edits), true);
});

test('FIRES: re-deriving the threshold arithmetic still obligates a sentinel', () => {
  const diff = unifiedDiff(TRIGGER_FILES[1], ['-\treturn maxMemory / capDenominator', '+\treturn maxMemory']);
  const edits = mechanismEdits(changedCodeLines(diff, TRIGGER_FILES), memoryBoundingSurface(FAKE));
  assert.ok(edits.length > 0);
  assert.equal(needsObligation([TRIGGER_FILES[1]], edits), true);
});

test('DOES NOT FIRE: a comment-only edit naming the surface in prose owes nothing', () => {
  const diff = unifiedDiff(TRIGGER_FILES[1], [
    '-// threshold derives the byte threshold from the live cap.',
    '+// threshold derives the byte threshold from the live per-query cap.',
  ]);
  const edits = mechanismEdits(changedCodeLines(diff, TRIGGER_FILES), memoryBoundingSurface(FAKE));
  assert.deepEqual(edits, []);
  assert.equal(needsObligation([TRIGGER_FILES[1]], edits), false);
});

test('DOES NOT FIRE: adding a NEUTRAL setting to the same file owes nothing', () => {
  // The exact shape of #2832 / #2833 / #2849 — three issues whose entire
  // content was a waiver receipt. Under mechanism-class scoping there is
  // nothing left to receipt.
  const diff = unifiedDiff(TRIGGER_FILES[0], [
    '+// perf-sentinel: neutral — an index-selection threshold, never a budget.',
    '+const settingProjectionIndex = "min_table_rows_to_use_projection_index"',
    '+\tctx = chclient.WithQuerySetting(ctx, settingProjectionIndex, 0)',
  ]);
  const edits = mechanismEdits(changedCodeLines(diff, TRIGGER_FILES), memoryBoundingSurface(FAKE));
  assert.deepEqual(edits, []);
  assert.equal(needsObligation([TRIGGER_FILES[0]], edits), false);
});

test('DOES NOT FIRE: renaming a neutral helper owes nothing', () => {
  const diff = unifiedDiff(TRIGGER_FILES[0], ['-\treturn chclient.WithQuerySetting(ctx, settingTag, shapeID(plan))', '+\treturn chclient.WithQuerySetting(ctx, settingTag, planShapeID(plan))']);
  const edits = mechanismEdits(changedCodeLines(diff, TRIGGER_FILES), memoryBoundingSurface(FAKE));
  assert.deepEqual(edits, []);
});

// --- the REAL trigger files, so the synthetic fixtures cannot drift away ------

const REAL = Object.fromEntries(TRIGGER_FILES.map((p) => [p, readFileSync(resolve(p), 'utf8')]));

test('every setting const in the real trigger files carries a classification', () => {
  assert.deepEqual(surfaceViolations(REAL), []);
});

test('the real surface is non-empty — an empty one would pass every change', () => {
  assert.ok(memoryBoundingSurface(REAL).size > 0);
});

test('the real surface holds every spill/thread bound and none of the neutral knobs', () => {
  const surface = memoryBoundingSurface(REAL);
  for (const want of [
    'settingMaxBytesBeforeExternalGroupBy',
    'settingMaxBytesBeforeExternalSort',
    'settingMaxBytesBeforeExternalJoin',
    'settingMaxThreads',
    'spillThreshold',
    'spillThresholdBytes',
    'spillCapDenominator',
    'applySpillSettings',
    'applyJoinSpillSettings',
    'applyCompareMemoryBound',
  ]) {
    assert.ok(surface.has(want), `${want} must stay inside the memory-bounding surface`);
  }
  for (const nope of [
    'settingOptimizeAggregationInOrder',
    'settingUseQueryConditionCache',
    'settingEnableAnalyzer',
    'settingLogComment',
    'settingQueryPlanOptimizeLazyMaterialization',
    'settingMinTableRowsToUseProjectionIndex',
    'apply',
    'eligibleForResultCache',
  ]) {
    assert.ok(!surface.has(nope), `${nope} bounds no memory and must stay outside the surface`);
  }
});

test('every real setting const is classified exactly one of the two classes', () => {
  for (const path of TRIGGER_FILES) {
    for (const decl of parseGoDecls(REAL[path])) {
      if (!SETTING_CONST_PATTERN.test(decl.name)) continue;
      assert.ok(
        [CLASS_MEMORY_BOUNDING, CLASS_NEUTRAL].includes(classificationOf(decl)),
        `${path}: ${decl.name} is unclassified`,
      );
    }
  }
});

// --- touchesTriggerFile / satisfiesViaSentinel --------------------------------

test('touchesTriggerFile is true when a trigger file is in the changed set', () => {
  assert.equal(touchesTriggerFile(['internal/engine/spill.go', 'docs/README.md']), true);
});

test('touchesTriggerFile is false when no trigger file is touched', () => {
  assert.equal(touchesTriggerFile(['internal/engine/other.go', 'docs/README.md']), false);
});

test('needsObligation is false on an empty or missing file list', () => {
  assert.equal(needsObligation([], []), false);
  assert.equal(needsObligation(undefined, undefined), false);
});

test('needsObligation is false when a trigger file moved but the surface did not', () => {
  assert.equal(needsObligation(['internal/engine/spill.go'], []), false);
});

test('satisfiesViaSentinel is true when either sentinel file is touched', () => {
  assert.equal(satisfiesViaSentinel(['test/perf/smoke/sentinels.go']), true);
  assert.equal(satisfiesViaSentinel(['test/perf/nightly/sentinels.go']), true);
});

test('satisfiesViaSentinel is false when neither is touched', () => {
  assert.equal(satisfiesViaSentinel(['test/perf/smoke/seed.go']), false);
});

// --- verdict -------------------------------------------------------------------

// The surface-naming changed line every obligated verdict below stands on:
// mechanism-class scoping means `verdict` is handed the EVIDENCE that a memory
// bound moved, not merely the filename it moved in.
const BOUND_EDIT = [{ file: 'internal/engine/spill.go', code: 'ctx = applySpillSettings(ctx, maxMemory)', added: true }];


test('verdict: no trigger file touched -> not obligated at all', () => {
  const v = verdict({ files: ['docs/README.md'], edits: [], waiverNumbers: [], resolved: new Map() });
  assert.deepEqual(v, { obligated: false });
});

test('verdict: trigger file touched alongside smoke sentinel -> satisfied', () => {
  const v = verdict({
    files: ['internal/engine/spill.go', 'test/perf/smoke/sentinels.go'],
    edits: BOUND_EDIT,
    waiverNumbers: [],
    resolved: new Map(),
  });
  assert.equal(v.obligated, true);
  assert.equal(v.satisfied, true);
  assert.equal(v.via, 'sentinel');
});

test('verdict: trigger file touched alongside nightly sentinel -> satisfied', () => {
  const v = verdict({
    files: ['internal/engine/query_settings_rules.go', 'test/perf/nightly/sentinels.go'],
    edits: BOUND_EDIT,
    waiverNumbers: [],
    resolved: new Map(),
  });
  assert.equal(v.obligated, true);
  assert.equal(v.satisfied, true);
  assert.equal(v.via, 'sentinel');
});

test('verdict: trigger file touched, no sentinel, an OPEN waiver -> satisfied', () => {
  const v = verdict({
    files: ['internal/engine/spill.go'],
    edits: BOUND_EDIT,
    waiverNumbers: [1535],
    resolved: RESOLVED,
  });
  assert.equal(v.obligated, true);
  assert.equal(v.satisfied, true);
  assert.equal(v.via, 'waiver');
  assert.equal(v.number, 1535);
});

test('verdict: trigger file touched, no sentinel, no waiver at all -> unsatisfied', () => {
  const v = verdict({ files: ['internal/engine/spill.go'], edits: BOUND_EDIT, waiverNumbers: [], resolved: new Map() });
  assert.equal(v.obligated, true);
  assert.equal(v.satisfied, false);
  assert.match(v.reason, /no PERF-SENTINEL-WAIVER/);
});

test('verdict: waiver cites a CLOSED issue -> unsatisfied, named as such', () => {
  const v = verdict({ files: ['internal/engine/spill.go'], edits: BOUND_EDIT, waiverNumbers: [1486], resolved: RESOLVED });
  assert.equal(v.satisfied, false);
  assert.match(v.reason, /#1486 is a closed issue/);
});

test('verdict: waiver cites a PULL REQUEST, not an issue -> unsatisfied, named as such', () => {
  const v = verdict({ files: ['internal/engine/spill.go'], edits: BOUND_EDIT, waiverNumbers: [1143], resolved: RESOLVED });
  assert.equal(v.satisfied, false);
  assert.match(v.reason, /#1143 is a pull request/);
});

test('verdict: waiver cites a number that names nothing -> unsatisfied, named as such', () => {
  const v = verdict({ files: ['internal/engine/spill.go'], edits: BOUND_EDIT, waiverNumbers: [999999], resolved: new Map() });
  assert.equal(v.satisfied, false);
  assert.match(v.reason, /#999999 names nothing/);
});

test('verdict: multiple waiver citations — the first VALID one satisfies, even if cited after an invalid one', () => {
  const v = verdict({
    files: ['internal/engine/spill.go'],
    edits: BOUND_EDIT,
    waiverNumbers: [1486, 1535],
    resolved: RESOLVED,
  });
  assert.equal(v.satisfied, true);
  assert.equal(v.number, 1535);
});

test('verdict: sentinel coverage wins even when an invalid waiver is also present', () => {
  const v = verdict({
    files: ['internal/engine/spill.go', 'test/perf/smoke/sentinels.go'],
    edits: BOUND_EDIT,
    waiverNumbers: [999999],
    resolved: new Map(),
  });
  assert.equal(v.satisfied, true);
  assert.equal(v.via, 'sentinel');
});

// --- wiring pin ----------------------------------------------------------------

const forbidDeferralWorkflow = readFileSync(resolve('.github/workflows/forbid-deferral.yml'), 'utf8');

test('the obligation gate rides forbid-deferral.yml and invokes the script', () => {
  assert.match(forbidDeferralWorkflow, /run: node \.github\/scripts\/perf-sentinel-obligation\.mjs/);
  assert.match(forbidDeferralWorkflow, /run: node --test \.github\/scripts\/perf-sentinel-obligation\.test\.mjs/);
});
