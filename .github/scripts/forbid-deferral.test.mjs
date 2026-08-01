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

import {
  CITATION_WINDOW_LINES,
  DEFERRAL_MARKERS,
  citationVerdict,
  evaluate,
  findMarkers,
  headingLevel,
  issueRefs,
  paragraphs,
  parseDiff,
  scanDiff,
  scanProse,
  sectionAt,
  stripFencedBlocks,
} from './forbid-deferral.mjs';

const lit = (...parts) => parts.join('');

const TODO_WORD = lit('TO', 'DO');
const FIXME_WORD = lit('FIX', 'ME');
const XXX_WORD = lit('XX', 'X');
const DEFERRED_WORD = lit('defer', 'red');
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
