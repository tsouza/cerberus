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
  TRIGGER_FILES,
  SENTINEL_FILES,
  WAIVER_PATTERN,
  waiverRefs,
  needsObligation,
  satisfiesViaSentinel,
  verdict,
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

// --- needsObligation / satisfiesViaSentinel -----------------------------------

test('needsObligation is true when a trigger file is in the changed set', () => {
  assert.equal(needsObligation(['internal/engine/spill.go', 'docs/README.md']), true);
});

test('needsObligation is false when no trigger file is touched', () => {
  assert.equal(needsObligation(['internal/engine/other.go', 'docs/README.md']), false);
});

test('needsObligation is false on an empty or missing file list', () => {
  assert.equal(needsObligation([]), false);
  assert.equal(needsObligation(undefined), false);
});

test('satisfiesViaSentinel is true when either sentinel file is touched', () => {
  assert.equal(satisfiesViaSentinel(['test/perf/smoke/sentinels.go']), true);
  assert.equal(satisfiesViaSentinel(['test/perf/nightly/sentinels.go']), true);
});

test('satisfiesViaSentinel is false when neither is touched', () => {
  assert.equal(satisfiesViaSentinel(['test/perf/smoke/seed.go']), false);
});

// --- verdict -------------------------------------------------------------------

test('verdict: no trigger file touched -> not obligated at all', () => {
  const v = verdict({ files: ['docs/README.md'], waiverNumbers: [], resolved: new Map() });
  assert.deepEqual(v, { obligated: false });
});

test('verdict: trigger file touched alongside smoke sentinel -> satisfied', () => {
  const v = verdict({
    files: ['internal/engine/spill.go', 'test/perf/smoke/sentinels.go'],
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
    waiverNumbers: [1535],
    resolved: RESOLVED,
  });
  assert.equal(v.obligated, true);
  assert.equal(v.satisfied, true);
  assert.equal(v.via, 'waiver');
  assert.equal(v.number, 1535);
});

test('verdict: trigger file touched, no sentinel, no waiver at all -> unsatisfied', () => {
  const v = verdict({ files: ['internal/engine/spill.go'], waiverNumbers: [], resolved: new Map() });
  assert.equal(v.obligated, true);
  assert.equal(v.satisfied, false);
  assert.match(v.reason, /no PERF-SENTINEL-WAIVER/);
});

test('verdict: waiver cites a CLOSED issue -> unsatisfied, named as such', () => {
  const v = verdict({ files: ['internal/engine/spill.go'], waiverNumbers: [1486], resolved: RESOLVED });
  assert.equal(v.satisfied, false);
  assert.match(v.reason, /#1486 is a closed issue/);
});

test('verdict: waiver cites a PULL REQUEST, not an issue -> unsatisfied, named as such', () => {
  const v = verdict({ files: ['internal/engine/spill.go'], waiverNumbers: [1143], resolved: RESOLVED });
  assert.equal(v.satisfied, false);
  assert.match(v.reason, /#1143 is a pull request/);
});

test('verdict: waiver cites a number that names nothing -> unsatisfied, named as such', () => {
  const v = verdict({ files: ['internal/engine/spill.go'], waiverNumbers: [999999], resolved: new Map() });
  assert.equal(v.satisfied, false);
  assert.match(v.reason, /#999999 names nothing/);
});

test('verdict: multiple waiver citations — the first VALID one satisfies, even if cited after an invalid one', () => {
  const v = verdict({
    files: ['internal/engine/spill.go'],
    waiverNumbers: [1486, 1535],
    resolved: RESOLVED,
  });
  assert.equal(v.satisfied, true);
  assert.equal(v.number, 1535);
});

test('verdict: sentinel coverage wins even when an invalid waiver is also present', () => {
  const v = verdict({
    files: ['internal/engine/spill.go', 'test/perf/smoke/sentinels.go'],
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
