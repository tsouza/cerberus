// Unit tests for forbid-deferral.mjs — run by `node --test` in the
// `forbid-deferral` workflow, before the gate itself runs.
//
// The failure mode these pin is silence in BOTH directions. A gate that stops
// matching reports a clean green on a change that parks work in prose; a gate
// that matches too much fires on ordinary architecture sentences, and an
// unusable gate gets routed around, which is worse than no gate. So every
// assertion below is a pair: this shape must match, that neighbouring shape
// must not.
//
// The calibration cases come from a sweep of all 1326 commits on `main`: 217
// carry deferral text, 178 of them ONLY in intra-branch commit messages, and
// the bulk of the RAW regex hits were false positives of exactly three shapes —
// Go's `defer` statement, the phrase used to RECORD completed work, and changes
// whose whole purpose was DELETING a marker line. All three are pinned here.
//
// Marker fixtures are spelled through lit() rather than written plainly: the
// gate scans the added lines of the very pull request that adds this file, so a
// literal fixture would make the file unmergeable under its own rule. The
// runtime strings are exact — lit('TO', 'DO') IS the four-character token — so
// nothing is weakened, only the file's own source text is kept clean. Same move
// forbid-skip.mjs makes when it writes its t.Skip pattern as an escaped regex.

import assert from 'node:assert/strict';
import test from 'node:test';

import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import {
  CITATION_WINDOW_LINES,
  DEFERRAL_MARKERS,
  candidatesFor,
  citationVerdict,
  descriptionSurface,
  evaluate,
  findMarkers,
  headingLevel,
  issueRefs,
  issuesReadability,
  paragraphs,
  parseDiff,
  scanDiff,
  eventPayloadAuthor,
  eventPayloadNumber,
  scanProse,
  scansDescription,
  sectionAt,
  stripFencedBlocks,
} from './forbid-deferral.mjs';

const lit = (...parts) => parts.join('');

const TODO_WORD = lit('TO', 'DO');
const FIXME_WORD = lit('FIX', 'ME');
const XXX_WORD = lit('XX', 'X');
const DEFERRED_WORD = lit('defer', 'red');
const DEFERRED_COLON = lit('DEFER', 'RED:');
const REVISIT_WORD = lit('re', 'visit');
const FOLLOWUP_WORD = lit('Follow', '-up');
const OUT_OF_SCOPE_PR = lit('out of scope ', 'for this PR');
const OUTSIDE_PR_SCOPE = lit("outside this PR", "'s scope");
const NOT_FIXED_HERE = lit('not fixed ', 'here');
const NOT_YET_HERE = lit('not yet implemented ', 'here');
const LATER_PR = lit('in a later ', 'PR');
const LEFT_FOR_LATER = lit('left for ', 'later');
const PUNTED_ON = lit('punted ', 'on');

const REPO = 'tsouza/cerberus';

const ids = (text) => findMarkers(text).map((m) => m.id);

// The three real fixtures this repository supplies: an open issue, an issue
// closed as a duplicate, and a number that resolves to a merged pull request
// rather than an issue at all.
const RESOLVED = new Map([
  [1535, { kind: 'issue', state: 'open' }],
  [1486, { kind: 'issue', state: 'closed' }],
  [1143, { kind: 'pull-request', state: 'closed' }],
]);

// --- the marker table itself -------------------------------------------------

test('the marker table is non-empty, uniquely keyed, and compiles', () => {
  assert.ok(DEFERRAL_MARKERS.length > 0, 'an empty table makes every scan vacuous');
  const keys = DEFERRAL_MARKERS.map((m) => m.id);
  assert.equal(new Set(keys).size, keys.length, 'ids must be unique so a report is addressable');
  for (const m of DEFERRAL_MARKERS) {
    assert.ok(m.description, `${m.id} has no description to put in a failure message`);
    assert.doesNotThrow(() => new RegExp(m.pattern, 'gi'), `${m.id} does not compile`);
  }
});

test('no pattern matches the table it lives in', () => {
  // Structural, not cosmetic: this module is itself added by a pull request the
  // gate scans, so a table that matched its own source would be unmergeable.
  for (const m of DEFERRAL_MARKERS) {
    for (const other of DEFERRAL_MARKERS) {
      for (const [what, text] of [
        ['source', m.pattern],
        ['id', m.id],
        ['description', m.description],
      ]) {
        assert.ok(!new RegExp(other.pattern, 'gi').test(text), `${other.id} matches the ${what} of ${m.id}`);
      }
    }
  }
});

test('every table row is reachable — each id fires on a real example', () => {
  // A row whose pattern nothing can trigger is a row that silently gates
  // nothing. Every id must appear at least once across the corpus below.
  const corpus = [
    `// ${TODO_WORD}: rewire the cursor`,
    `// ${FIXME_WORD} the off-by-one`,
    `// ${XXX_WORD} suspicious`,
    `The bucket bug is ${NOT_FIXED_HERE}.`,
    `Exponential buckets are ${NOT_YET_HERE}.`,
    `## ${FOLLOWUP_WORD}`,
    `${FOLLOWUP_WORD}: widen the ratchet`,
    `The label hoist is ${DEFERRED_WORD}.`,
    `The label hoist is ${lit('deferring ', 'to a later')} change.`,
    `The rewrite is ${LEFT_FOR_LATER}.`,
    `The rewrite lands ${LATER_PR}.`,
    `We ${PUNTED_ON} the retry budget.`,
    `Worth a ${REVISIT_WORD} once the solver lands.`,
    `That is ${OUT_OF_SCOPE_PR}.`,
    `That is ${OUTSIDE_PR_SCOPE}.`,
  ].join('\n\n');
  const fired = new Set(ids(corpus));
  for (const m of DEFERRAL_MARKERS) {
    assert.ok(fired.has(m.id), `${m.id} fired on nothing — the row gates nothing`);
  }
});

// --- calibration: the three false-positive shapes the sweep found ------------

test("Go's defer statement is not a deferral", () => {
  // The single largest source of raw hits in this repository. An unanchored
  // `defer(red|ring)?` fires on every resource cleanup in the tree.
  for (const line of [
    'defer rows.Close()',
    'defer func() { _ = tx.Rollback() }()',
    'the caller defers the close',
    'deferring is what defer does',
    'defers the close until the scan ends',
  ]) {
    assert.deepEqual(ids(line), [], `matched on: ${line}`);
  }
  // …while the English participle still does match.
  assert.deepEqual(ids(`the hoist is ${DEFERRED_WORD}`), ['deferral-to-later']);
});

test('attributive "deferred NOUN" describing already-shipped work is not a deferral', () => {
  // The exact false-positive class found in a real, in-flight pull request
  // (#1955): the participle modifying a noun to NAME an existing pipeline
  // stage, not a verb complement saying the work was put off. The bare
  // `\bdeferred\b` alternative used to fire on all three of these; only the
  // PREDICATE form — right after a linking or passive-voice verb — should.
  for (const line of [
    'the deferred label-shaping (-State/-Merge) shape only exists on top of ' +
      'a native rate grid, so it is read only where one is being built.',
    'recollapseTower is the deferred-label-shaping expression the lowering assembles.',
    'Deferred label shaping on the native grid keeps the rate node byte-identical.',
    'the deferred close runs after the scan tears down its cursor.',
  ]) {
    assert.deepEqual(ids(line), [], `matched on: ${line}`);
  }
  // The predicate form the row exists to catch is the regression guard: it
  // must still fire after the narrowing.
  assert.deepEqual(ids(`the hoist is ${DEFERRED_WORD}`), ['deferral-to-later']);
});

test('a colon-suffixed label still matches, mirroring the follow-up heading form', () => {
  // A real pre-gate example: a test case excluded with a leading label
  // naming genuinely postponed work, rather than a predicate sentence.
  assert.deepEqual(
    ids(`${DEFERRED_COLON} the broadcast semantics divergence, a real gap`),
    ['deferral-to-later'],
  );
});

test('recording completed work is not deferring it', () => {
  // "Follow-up <merged PR>" was the commonest prose false positive: it names
  // work that ALREADY happened. Only the label forms — a heading, or the colon
  // introducing a list nobody has taken — are deferrals.
  for (const line of [
    `${FOLLOWUP_WORD} to #1143 fixed the lexer`,
    `this is the ${FOLLOWUP_WORD} that finishes #1535`,
    `${FOLLOWUP_WORD} tracked in #1535`,
  ]) {
    assert.deepEqual(ids(line), [], `matched on: ${line}`);
  }
  assert.deepEqual(ids(`${FOLLOWUP_WORD}: widen the ratchet`), ['followup-label']);
  assert.deepEqual(ids(`### ${FOLLOWUP_WORD}s`), ['followup-label']);
});

test('a heading that reports completed work in the past tense is clean', () => {
  // The heading form of the same false positive, and the one that slipped past
  // the row above: a section headed with the label but describing work this
  // change DID. The gate cannot read intent, so the remedy names retitling as
  // one of its three branches — and the past-tense titles it suggests have to
  // actually be clean, or the advice sends the author in a circle.
  for (const line of [
    '## Resolved in this change: the assertion was corrected',
    '## Fixed here: the wrong bound in the scanner',
    '## Done in this change: three axes of assertion added',
    '## What this change already fixes',
  ]) {
    assert.deepEqual(ids(line), [], `matched on: ${line}`);
  }
  // Recall is not traded away to get that precision: the mislabelled heading
  // still fires, because "the section below might describe finished work" is
  // not something a scanner can decide, and a heading reading as a deferral IS
  // a deferral until its author says otherwise. Retitling is the remedy.
  assert.deepEqual(ids(`## ${FOLLOWUP_WORD}: CI red on the new test`), ['followup-label']);
});

test('deleting a marker line is not adding one', () => {
  // A change whose whole purpose is REMOVING stale prose reads as a diff full
  // of markers on `-` lines. Removed lines have no position in the new file and
  // must contribute nothing.
  const diff = [
    'diff --git a/internal/promql/lower.go b/internal/promql/lower.go',
    '--- a/internal/promql/lower.go',
    '+++ b/internal/promql/lower.go',
    '@@ -10,3 +10,2 @@',
    ' func lower() {',
    `-\t// ${TODO_WORD}: support the offset modifier`,
    '\treturn nil',
  ].join('\n');
  assert.deepEqual(scanDiff(diff, REPO), []);
});

// --- calibration: the qualifier rows ----------------------------------------

test('bare architecture prose does not match; the PR-scoped variant does', () => {
  // `main` carries this phrase in the hundreds as a description of a system
  // boundary. Only the form that names this pull request is a deferral.
  for (const line of [
    'out of scope for the unknown-name fan-out',
    'label shaping is out of scope for the optimizer',
    'this is out of scope',
  ]) {
    assert.deepEqual(ids(line), [], `matched on: ${line}`);
  }
  assert.deepEqual(ids(`that is ${OUT_OF_SCOPE_PR}`), ['pr-scoped-exclusion']);
  assert.deepEqual(ids(`that is ${OUTSIDE_PR_SCOPE}`), ['pr-scoped-exclusion']);
});

test('bare "not yet" is state, not a deferral', () => {
  for (const line of [
    'the solver has not yet run',
    'not yet, and possibly never',
    'the column is not yet populated by the exporter',
  ]) {
    assert.deepEqual(ids(line), [], `matched on: ${line}`);
  }
  assert.deepEqual(ids(`exponential buckets are ${NOT_YET_HERE}`), ['not-yet-here']);
});

test('ordinary engineering prose stays clean', () => {
  for (const line of [
    'The RangeWindow emitter now checks commensurability before slicing.',
    'Later steps reuse the same grid, so the anchor is computed once.',
    'A separate PR shipped the chart bump; this one only touches the emitter.',
    'The punt list is empty.',
  ]) {
    assert.deepEqual(ids(line), [], `matched on: ${line}`);
  }
});

// --- citation extraction -----------------------------------------------------

test('issue references are read as numbers and as this repository’s URLs', () => {
  assert.deepEqual(issueRefs('tracked in #1535', REPO), [1535]);
  assert.deepEqual(issueRefs('see https://github.com/tsouza/cerberus/issues/1535', REPO), [1535]);
  assert.deepEqual(issueRefs('(#1535) and #1486', REPO), [1486, 1535]);
});

test('a reference this gate cannot resolve is not a citation', () => {
  // Another repository's issue cannot be resolved against this one, so it must
  // not silently satisfy the requirement.
  assert.deepEqual(issueRefs('see https://github.com/grafana/loki/issues/42', REPO), []);
  // A colour literal is digits behind a hash, not an issue.
  assert.deepEqual(issueRefs('background #1a2b3c', REPO), []);
});

// --- prose surface -----------------------------------------------------------

test('a fenced block is quoted material, not authored commitment', () => {
  const body = ['intro', '', '```', `// ${TODO_WORD}: quoted from a log`, '```', '', 'outro'].join('\n');
  assert.deepEqual(scanProse({ text: body, surface: 'pr-body', locate: (l) => `line ${l}`, repoSlug: REPO }), []);
  // The line count survives, so locations outside the fence stay addressable.
  assert.equal(stripFencedBlocks(body).split('\n').length, body.split('\n').length);
});

test('an unfenced marker in the same body still reports, with its line', () => {
  const body = ['intro', '', '```', 'quoted', '```', '', `${FOLLOWUP_WORD}: widen the ratchet`].join('\n');
  const found = scanProse({ text: body, surface: 'pr-body', locate: (l) => `line ${l}`, repoSlug: REPO });
  assert.equal(found.length, 1);
  assert.equal(found[0].markerId, 'followup-label');
  assert.equal(found[0].location, 'line 7');
});

test('the citation scope for prose is the paragraph, not the whole body', () => {
  const sameParagraph = `${FOLLOWUP_WORD}: widen the ratchet\ntracked in #1535`;
  const otherParagraph = `${FOLLOWUP_WORD}: widen the ratchet\n\ntracked in #1535`;
  const scan = (text) => scanProse({ text, surface: 'pr-body', locate: (l) => `line ${l}`, repoSlug: REPO });
  assert.deepEqual(scan(sameParagraph)[0].refs, [1535]);
  assert.deepEqual(scan(otherParagraph)[0].refs, [], 'a citation a paragraph away tracks something else');
});

// --- section scope: the shape paragraph scope got wrong ----------------------

test('a heading introduces a section, bounded by the next heading of its level', () => {
  const lines = ['# top', 'a', '## mid', 'b', '### deep', 'c', '## other', 'd'];
  assert.deepEqual([1, 0, 2, 0, 3, 0, 2, 0], lines.map(headingLevel));
  // A deeper heading is INSIDE the section; a same-level one ends it.
  assert.equal(sectionAt(lines, 2), ['## mid', 'b', '### deep', 'c'].join('\n'));
  // A higher-level section swallows every subsection under it.
  assert.equal(sectionAt(lines, 0), lines.join('\n'));
  // The last section runs to the end of the surface.
  assert.equal(sectionAt(lines, 6), ['## other', 'd'].join('\n'));
  // A line that is not a heading has no section.
  assert.equal(sectionAt(lines, 1), null);
});

test('a heading is satisfied by an issue cited anywhere in its section', () => {
  // The exact body shape the first live run rejected, and the one this gate
  // should most want to see: a heading naming the category, then a list whose
  // every entry leads with its issue. A markdown heading is its own paragraph,
  // so paragraph scope reported "cites no issue at all" while the author was
  // looking straight at the number — the most damaging thing a gate can say.
  const body = [
    `## Filed, ${NOT_FIXED_HERE}`,
    '',
    '- #1535 — the roundtrip harness cannot expand a star projection whose only',
    '  column-name source is a raw table.',
    '',
    '## Something else entirely',
    '',
    'No citation here.',
  ].join('\n');
  const found = scanProse({ text: body, surface: 'pr-body', locate: (l) => `line ${l}`, repoSlug: REPO });
  assert.equal(found.length, 1);
  assert.equal(found[0].scope, 'the section it introduces');
  assert.deepEqual(found[0].refs, [1535]);
  assert.deepEqual(evaluate(found, RESOLVED), [], 'a filed and cited finding is not a deferral');
});

test('a heading whose section cites nothing is still a violation', () => {
  // The widening buys precision, not tolerance. Same body, citation removed.
  const body = [
    `## Filed, ${NOT_FIXED_HERE}`,
    '',
    '- the roundtrip harness cannot expand a star projection.',
    '',
    '## Something else entirely',
    '',
    'Tracked in #1535.',
  ].join('\n');
  const found = scanProse({ text: body, surface: 'pr-body', locate: (l) => `line ${l}`, repoSlug: REPO });
  assert.equal(found.length, 1);
  assert.deepEqual(found[0].refs, [], 'a citation under a different heading tracks something else');
  const violations = evaluate(found, RESOLVED);
  assert.equal(violations.length, 1);
  assert.match(violations[0].detail, /cites no issue/);
});

test('a section citation only rescues a closed or wrong-kind reference honestly', () => {
  const withClosed = [`## ${FOLLOWUP_WORD}`, '', '- widen the ratchet, tracked in #1486'].join('\n');
  const withPr = [`## ${FOLLOWUP_WORD}`, '', '- widen the ratchet, tracked in #1143'].join('\n');
  const scan = (text) => scanProse({ text, surface: 'pr-body', locate: (l) => `line ${l}`, repoSlug: REPO });
  assert.match(evaluate(scan(withClosed), RESOLVED)[0].detail, /closed issue/);
  assert.match(evaluate(scan(withPr), RESOLVED)[0].detail, /pull request, not an issue/);
});

test('only headings widen — a mid-section sentence keeps paragraph scope', () => {
  // A heading is a label for the block beneath it. A sentence buried in prose
  // is not, so an issue named two paragraphs down must not adopt it.
  const body = [
    '## Notes',
    '',
    `The bucket bug is ${NOT_FIXED_HERE}.`,
    '',
    'Unrelated paragraph that happens to mention #1535.',
  ].join('\n');
  const found = scanProse({ text: body, surface: 'pr-body', locate: (l) => `line ${l}`, repoSlug: REPO });
  assert.equal(found.length, 1);
  assert.equal(found[0].scope, 'the same paragraph');
  assert.deepEqual(found[0].refs, []);
});

test('a heading marker is reported on the heading line, not the line above it', () => {
  // The heading arm starts at a zero-width line boundary. Consuming the
  // preceding newline instead would report the line above and, worse, would not
  // be recognised as a heading when its citation scope is computed — silently
  // reverting this whole fix for any body without a blank line before the
  // heading.
  const body = ['intro text', `## ${FOLLOWUP_WORD}`, '', 'tracked in #1535'].join('\n');
  const found = scanProse({ text: body, surface: 'pr-body', locate: (l) => `line ${l}`, repoSlug: REPO });
  assert.equal(found.length, 1);
  assert.equal(found[0].location, 'line 2');
  assert.equal(found[0].scope, 'the section it introduces');
  assert.deepEqual(found[0].refs, [1535]);
});

test('paragraphs carry their starting line', () => {
  const p = paragraphs('a\n\n\nb\nc\n');
  assert.deepEqual(
    p.map((x) => [x.startLine, x.text]),
    [
      [1, 'a'],
      [4, 'b\nc'],
    ],
  );
});

// --- diff surface ------------------------------------------------------------

const diffWith = (body) =>
  [
    'diff --git a/internal/chsql/emit.go b/internal/chsql/emit.go',
    '--- a/internal/chsql/emit.go',
    '+++ b/internal/chsql/emit.go',
    '@@ -20,4 +20,6 @@ func emit() {',
    ...body,
  ].join('\n');

test('added lines are addressed by their new-file line number', () => {
  const found = scanDiff(diffWith([' func emit() {', `+\t// ${TODO_WORD}: fold the grid`, ' }']), REPO);
  assert.equal(found.length, 1);
  assert.equal(found[0].location, 'internal/chsql/emit.go:21');
  assert.equal(found[0].markerId, 'code-work-marker');
});

test('a citation inside the comment block counts, one far away does not', () => {
  const near = scanDiff(
    diffWith([' func emit() {', `+\t// ${TODO_WORD}: fold the grid`, '+\t// Tracked in #1535.', ' }']),
    REPO,
  );
  assert.deepEqual(near[0].refs, [1535]);

  const far = scanDiff(
    diffWith([
      ' func emit() {',
      `+\t// ${TODO_WORD}: fold the grid`,
      ...Array.from({ length: CITATION_WINDOW_LINES + 1 }, (_, i) => `+\tline${i}()`),
      '+\t// Tracked in #1535.',
      ' }',
    ]),
    REPO,
  );
  assert.deepEqual(far[0].refs, [], 'a citation beyond the window belongs to something else');
});

test('an unchanged line inside the window still supplies the citation', () => {
  // Extending a comment block that already names its issue must not be a
  // violation just because the citation line is context rather than an addition.
  const found = scanDiff(
    diffWith([' \t// Tracked in #1535.', `+\t// ${TODO_WORD}: and the offset modifier`, ' }']),
    REPO,
  );
  assert.deepEqual(found[0].refs, [1535]);
});

test('the diff parser reports the file set and skips deletions', () => {
  const diff = [
    'diff --git a/gone.go b/gone.go',
    '--- a/gone.go',
    '+++ /dev/null',
    '@@ -1,2 +0,0 @@',
    '-package gone',
    'diff --git a/kept.go b/kept.go',
    '--- a/kept.go',
    '+++ b/kept.go',
    '@@ -1,1 +1,2 @@',
    ' package kept',
    '+// added',
  ].join('\n');
  const { files, lines } = parseDiff(diff);
  assert.deepEqual(files, ['kept.go']);
  assert.deepEqual(
    lines.map((l) => [l.file, l.line, l.added]),
    [
      ['kept.go', 1, false],
      ['kept.go', 2, true],
    ],
  );
});

test('a line that merely starts with a plus inside a hunk is still content', () => {
  // `+++x` as source text (an added line whose content begins with `++`) must
  // not be mistaken for a file header once a hunk is open.
  const { lines } = parseDiff(diffWith([' a', '+++x', ' b']));
  assert.deepEqual(
    lines.map((l) => l.text),
    ['a', '++x', 'b'],
  );
});

// --- verdict -----------------------------------------------------------------

test('only an open issue tracks the work', () => {
  assert.equal(citationVerdict([1535], RESOLVED).tracked, true);
  assert.equal(citationVerdict([1486], RESOLVED).tracked, false);
  assert.equal(citationVerdict([1143], RESOLVED).tracked, false);
  assert.equal(citationVerdict([9999], RESOLVED).tracked, false);
  assert.equal(citationVerdict([], RESOLVED).tracked, false);
});

test('each rejection says which arm rejected it', () => {
  assert.match(citationVerdict([], RESOLVED).detail, /cites no issue/);
  assert.match(citationVerdict([1486], RESOLVED).detail, /closed issue/);
  assert.match(citationVerdict([1143], RESOLVED).detail, /pull request, not an issue/);
  assert.match(citationVerdict([9999], RESOLVED).detail, /names nothing/);
});

test('an issue closed as a duplicate is still closed', () => {
  // The sharpest edge in practice, and it drew blood on the first day: a body
  // cited an issue that had been closed as a duplicate and consolidated into
  // another, still-open one. It looks maximally legitimate — somebody really
  // did file it — but the work is now tracked under a number the body does not
  // name, so the citation points at a closed record. The verdict must be the
  // same as for any other closed issue, and the detail must say which.
  const verdict = citationVerdict([1486], RESOLVED);
  assert.equal(verdict.tracked, false);
  assert.match(verdict.detail, /closed issue/);
  // Repointing at the surviving open issue clears it.
  assert.equal(citationVerdict([1535], RESOLVED).tracked, true);
});

test('one open issue among several citations is enough', () => {
  assert.equal(citationVerdict([1143, 1486, 1535], RESOLVED).tracked, true);
});

// --- the issue-lookup capability probe ---------------------------------------

// GitHub answers an unreadable resource with 404, not 403, so "no such issue"
// and "this token may not read issues" reach the gate as one status code.
// Reporting the second as the first tells an author who filed the issue that
// they filed nothing. These four cases pin the discrimination: the probe must
// separate the permission fault from the missing issue, must attribute a
// repository-level failure to the repository rather than to the issues grant,
// and must stay silent when the token is fine.
const API = 'https://api.github.com';
const OK = { ok: true, status: 200, statusText: 'OK' };
const NOT_FOUND = { ok: false, status: 404, statusText: 'Not Found' };

// recordingGet — a probe transport that answers from a URL->response table and
// records the order it was asked, so the two-request sequence is pinned too.
function recordingGet(answers) {
  const calls = [];
  return {
    calls,
    get: async (url) => {
      calls.push(url);
      for (const [fragment, res] of answers) {
        if (url.includes(fragment)) return res;
      }
      throw new Error(`the probe requested an unexpected URL: ${url}`);
    },
  };
}

test('a token that can read issues leaves the lookup trusted', async () => {
  const { get, calls } = recordingGet([
    ['/issues', OK],
    [`/repos/${REPO}`, OK],
  ]);
  const verdict = await issuesReadability({ repo: REPO, apiBase: API, get });
  assert.deepEqual(verdict, { readable: true });
  assert.equal(calls.length, 2, 'the probe asks the repository first, then the issue list');
  assert.ok(calls[0].endsWith(`/repos/${REPO}`), `first call was ${calls[0]}`);
  assert.match(calls[1], /\/issues\?per_page=\d+$/);
});

test('an empty issue list is readable — 200 with no rows is not a refusal', async () => {
  // The list endpoint answers 200 and an empty array in a repository with no
  // issues, which is why a non-OK status can only mean the read was refused.
  const { get } = recordingGet([
    ['/issues', OK],
    [`/repos/${REPO}`, OK],
  ]);
  assert.equal((await issuesReadability({ repo: REPO, apiBase: API, get })).readable, true);
});

test('a 404 on the issue list is reported as a permission fault, never as the author', async () => {
  const { get } = recordingGet([
    ['/issues', NOT_FOUND],
    [`/repos/${REPO}`, OK],
  ]);
  const verdict = await issuesReadability({ repo: REPO, apiBase: API, get });
  assert.equal(verdict.readable, false);
  assert.match(verdict.reason, /cannot read issues/);
  assert.match(verdict.reason, /issues: read/);
  assert.match(verdict.reason, /not a fault in what the author wrote/);
});

test('an unreadable repository is not reported as a missing issues grant', async () => {
  // A wrong slug, an expired credential and a fork-scoped token all land here,
  // and each has a different fix from "grant issues: read". Naming the wrong
  // one sends the reader to the wrong file.
  const { get, calls } = recordingGet([[`/repos/${REPO}`, NOT_FOUND]]);
  const verdict = await issuesReadability({ repo: REPO, apiBase: API, get });
  assert.equal(verdict.readable, false);
  assert.match(verdict.reason, /cannot read the repository/);
  assert.doesNotMatch(verdict.reason, /issues: read/);
  assert.equal(calls.length, 1, 'the issue list is not probed when the repository itself is unreadable');
});

test('evaluate keeps only the candidates no citation rescues', () => {
  const body = [
    `${FOLLOWUP_WORD}: widen the ratchet`,
    '',
    `${FOLLOWUP_WORD}: fix the lexer, tracked in #1535`,
    '',
    `${FOLLOWUP_WORD}: fix the parser, tracked in #1486`,
    '',
    `${FOLLOWUP_WORD}: fix the emitter, tracked in #1143`,
  ].join('\n');
  const candidates = scanProse({
    text: body,
    surface: 'pr-body',
    locate: (l) => `line ${l}`,
    repoSlug: REPO,
  });
  assert.equal(candidates.length, 4);
  const violations = evaluate(candidates, RESOLVED);
  assert.deepEqual(
    violations.map((v) => v.location),
    ['line 1', 'line 5', 'line 7'],
  );
});

// --- surface scoping: a generated description ---------------------------------
//
// Every test below is a pair with the ones after it, and the pairing IS the
// claim. "A bot's description is not read" on its own is indistinguishable from
// an exemption; it becomes scoping only alongside "its commit messages are
// read" and "its diff is read". So the suppression test is one of five, and the
// other four are the ones that would catch this rule widening into a bypass.

const BOT = 'dependabot[bot]';
const HUMAN = 'tsouza';

// A description carrying one marker and citing nothing, so the only reason it
// could come back clean is that the surface was not read.
const GENERATED_BODY = [
  'Bumps go.opentelemetry.io/otel from 1.40.0 to 1.41.0.',
  '',
  `<li><code>223f9fd</code> sdk/metric: remove obsolete randomFloat64 ${TODO_WORD} (#7469)</li>`,
].join('\n');

const COMMITS_WITH_MARKER = [
  { sha: 'a'.repeat(40), message: `chore(deps): bump otel\n\n${FOLLOWUP_WORD}: re-pin the oracle module` },
];

const DIFF_WITH_MARKER = [
  'diff --git a/internal/chsql/emit.go b/internal/chsql/emit.go',
  '--- a/internal/chsql/emit.go',
  '+++ b/internal/chsql/emit.go',
  '@@ -20,4 +20,6 @@ func emit() {',
  ' func emit() {',
  `+\t// ${TODO_WORD}: fold the grid`,
  ' }',
].join('\n');

const EMPTY_DIFF = [
  'diff --git a/go.mod b/go.mod',
  '--- a/go.mod',
  '+++ b/go.mod',
  '@@ -3,3 +3,3 @@ module github.com/tsouza/cerberus',
  '-\tgo.opentelemetry.io/otel v1.40.0',
  '+\tgo.opentelemetry.io/otel v1.41.0',
].join('\n');

const surfacesOf = (candidates) => candidates.map((c) => c.surface).sort();

test('a generated description is quoted release notes, not this change’s commitment', () => {
  const found = candidatesFor({
    descriptionParts: [{ text: GENERATED_BODY, author: BOT, number: 100 }],
    commits: [{ sha: 'b'.repeat(40), message: 'chore(deps): bump otel' }],
    diffText: EMPTY_DIFF,
    repoSlug: REPO,
  });
  assert.deepEqual(found, [], 'the body quotes an upstream subject; nothing here is authored');
});

test('the same description on an ordinary pull request is still read', () => {
  const found = candidatesFor({
    descriptionParts: [{ text: GENERATED_BODY, author: HUMAN, number: null }],
    commits: [{ sha: 'b'.repeat(40), message: 'chore(deps): bump otel' }],
    diffText: EMPTY_DIFF,
    repoSlug: REPO,
  });
  assert.deepEqual(surfacesOf(found), ['pr-body']);
  // No number on the part (the shape a pull_request-event run with an
  // unresolvable payload produces) falls back to the un-numbered location.
  assert.equal(found[0].location, 'PR description line 3');
});

test('a marker in a generated pull request’s COMMIT MESSAGE is still reported', () => {
  const found = candidatesFor({
    descriptionParts: [{ text: GENERATED_BODY, author: BOT, number: 100 }],
    commits: COMMITS_WITH_MARKER,
    diffText: EMPTY_DIFF,
    repoSlug: REPO,
  });
  assert.deepEqual(surfacesOf(found), ['commit']);
  assert.equal(found[0].markerId, 'followup-label');
  assert.deepEqual(found[0].refs, [], 'it cites nothing, so the verdict stays untracked');
});

test('a marker in a generated pull request’s DIFF is still reported', () => {
  const found = candidatesFor({
    descriptionParts: [{ text: GENERATED_BODY, author: BOT, number: 100 }],
    commits: [{ sha: 'b'.repeat(40), message: 'chore(deps): bump otel' }],
    diffText: DIFF_WITH_MARKER,
    repoSlug: REPO,
  });
  assert.deepEqual(surfacesOf(found), ['diff']);
  assert.equal(found[0].location, 'internal/chsql/emit.go:21');
});

test('an unknown author scans the description as normal', () => {
  for (const author of [null, undefined, '', '   ']) {
    const found = candidatesFor({
      descriptionParts: [{ text: GENERATED_BODY, author, number: 100 }],
      commits: [{ sha: 'b'.repeat(40), message: 'chore(deps): bump otel' }],
      diffText: EMPTY_DIFF,
      repoSlug: REPO,
    });
    assert.deepEqual(
      surfacesOf(found),
      ['pr-body'],
      `author ${JSON.stringify(author)} must not suppress a surface`,
    );
  }
});

test('scansDescription narrows on exactly one login and nothing near it', () => {
  assert.equal(scansDescription(BOT), false);
  assert.equal(scansDescription('Dependabot[bot]'), false, 'a login is not case-sensitive');
  for (const near of ['dependabot', 'dependabot-preview[bot]', 'notdependabot[bot]', 'renovate[bot]', HUMAN]) {
    assert.equal(scansDescription(near), true, `${near} is not the generated-description account`);
  }
});

// --- the merge-queue batching bug (#1943) ------------------------------------
//
// A merge-queue batch or a push resolves its description surface from EVERY
// pull request associated with the head commit — a LIST, not one pull
// request. The bug: descriptionSurface used to concatenate every pull's body
// into one string and average their authors into a single `sharedAuthor`,
// which is null the moment the batch disagrees on one — a Dependabot pull
// request riding beside any human pull request. A null author scans as an
// ordinary surface, so Dependabot's generated changelog (which quotes
// upstream commit subjects verbatim, and upstream commit subjects are
// ordinary prose that can accidentally contain marker vocabulary — this is
// what made #1937 go red) got scanned as if a human had written it, and the
// required check failed the WHOLE batch on a marker nobody in it wrote.
//
// The fix scans PER PULL REQUEST: candidatesFor takes `descriptionParts`, one
// entry per pull request, each carrying its own text/author/number, so a
// bot's part is exempted on its own authorship regardless of what rides
// alongside it, and a human's part is scanned under that human's own
// authorship, unaffected by any bot in the same batch.
const MIXED_BATCH_BOT_PART = { text: GENERATED_BODY, author: BOT, number: 100 };

test('a merge-queue batch with a Dependabot PR and a clean human PR passes', () => {
  const humanBody =
    'Adds a guardrail around the offset modifier so slicing never crosses a partial window.';
  const found = candidatesFor({
    descriptionParts: [MIXED_BATCH_BOT_PART, { text: humanBody, author: HUMAN, number: 101 }],
    commits: [{ sha: 'c'.repeat(40), message: 'chore(deps): bump otel' }],
    diffText: EMPTY_DIFF,
    repoSlug: REPO,
  });
  assert.deepEqual(
    found,
    [],
    'the bot part is exempted on its own authorship and the human part carries no marker',
  );
});

test('a merge-queue batch fails on the human PR’s own untracked deferral, attributed to that PR', () => {
  // Same batch, but the human pull request's body genuinely defers work and
  // cites nothing. The gate must still fail — the fix is about WHO a marker is
  // attributed to, never about weakening the scan — and the reported location
  // and author must name the human pull request, not the bot riding beside it.
  const humanBody = `Adds the guardrail. The retry-budget rework is ${DEFERRED_WORD}.`;
  const found = candidatesFor({
    descriptionParts: [MIXED_BATCH_BOT_PART, { text: humanBody, author: HUMAN, number: 101 }],
    commits: [{ sha: 'c'.repeat(40), message: 'chore(deps): bump otel' }],
    diffText: EMPTY_DIFF,
    repoSlug: REPO,
  });
  assert.equal(found.length, 1, "only the human PR's marker surfaces; the bot PR stays exempt");
  assert.equal(found[0].location, 'PR #101 description line 1');
  assert.equal(found[0].markerId, 'deferral-to-later');
  const violations = evaluate(found, RESOLVED);
  assert.equal(violations.length, 1);
  assert.match(violations[0].location, /#101/, 'the violation must point at the PR that wrote it');
});

test('descriptionSurface resolves a merge-queue batch into one part per pull request', async () => {
  // The end-to-end wiring: a batch of two associated pull requests with
  // different authors used to collapse to a single concatenated blob and a
  // null shared author. It must now come back as independent parts, each
  // carrying its own author and number, with the pre-existing combined
  // text/origin kept alongside for pr-body-check.mjs.
  const head = '0123456789abcdef0123456789abcdef01234567';
  const apiBase = 'https://api.github.test';
  const real = globalThis.fetch;
  globalThis.fetch = async (url) => {
    assert.equal(url, `${apiBase}/repos/${REPO}/commits/${head}/pulls`);
    return {
      ok: true,
      status: 200,
      statusText: 'OK',
      json: async () => [
        { number: 100, body: GENERATED_BODY, user: { login: BOT } },
        { number: 101, body: 'Adds a guardrail.', user: { login: HUMAN } },
      ],
    };
  };
  try {
    const description = await descriptionSurface({
      eventName: 'merge_group',
      repo: REPO,
      head,
      token: 't',
      apiBase,
    });
    assert.equal(description.parts.length, 2);
    assert.deepEqual(
      description.parts.map((p) => [p.number, p.author]),
      [
        [100, BOT],
        [101, HUMAN],
      ],
    );
    assert.equal(description.parts[0].text, GENERATED_BODY);
    assert.equal(description.parts[1].text, 'Adds a guardrail.');
    // The combined view pr-body-check.mjs reads survives unchanged.
    assert.match(description.origin, /#100, #101/);
    assert.equal(description.text, `${GENERATED_BODY}\n\nAdds a guardrail.`);
  } finally {
    globalThis.fetch = real;
  }
});

// --- resolving the author ------------------------------------------------------

const withPayload = (contents) => {
  const path = join(mkdtempSync(join(tmpdir(), 'forbid-deferral-')), 'event.json');
  writeFileSync(path, contents);
  return path;
};

test('the author is read from the event payload, not from a caller-supplied string', () => {
  const path = withPayload(JSON.stringify({ pull_request: { user: { login: BOT } } }));
  assert.equal(eventPayloadAuthor(path), BOT);
});

test('every unreadable payload yields no author, which scans the description', () => {
  const cases = {
    'no path at all': undefined,
    'a path that does not exist': join(tmpdir(), 'forbid-deferral-absent', 'event.json'),
    'a file that is not JSON': withPayload('not json {'),
    'a payload with no pull request': withPayload(JSON.stringify({ ref: 'refs/heads/main' })),
    'a pull request with no user': withPayload(JSON.stringify({ pull_request: {} })),
    'a login that is not a string': withPayload(JSON.stringify({ pull_request: { user: { login: 7 } } })),
    'a blank login': withPayload(JSON.stringify({ pull_request: { user: { login: '  ' } } })),
  };
  for (const [what, path] of Object.entries(cases)) {
    assert.equal(eventPayloadAuthor(path), null, what);
    assert.equal(scansDescription(eventPayloadAuthor(path)), true, `${what} must still scan`);
  }
});

test('the pull request number is read from the event payload alongside the author', () => {
  const path = withPayload(JSON.stringify({ pull_request: { number: 101, user: { login: HUMAN } } }));
  assert.equal(eventPayloadNumber(path), 101);
});

test('every unreadable payload yields no number either', () => {
  const cases = {
    'no path at all': undefined,
    'a path that does not exist': join(tmpdir(), 'forbid-deferral-absent', 'event.json'),
    'a file that is not JSON': withPayload('not json {'),
    'a payload with no pull request': withPayload(JSON.stringify({ ref: 'refs/heads/main' })),
    'a pull request with no number': withPayload(JSON.stringify({ pull_request: {} })),
    'a number that is not a number': withPayload(JSON.stringify({ pull_request: { number: '101' } })),
  };
  for (const [what, path] of Object.entries(cases)) {
    assert.equal(eventPayloadNumber(path), null, what);
  }
});
