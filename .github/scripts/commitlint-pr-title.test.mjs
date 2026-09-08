// commitlint-pr-title.test.mjs — in-process pins for the PR-title gate
// (issue #3187: the squash subject that lands on `main` was never linted).
//
// The orchestration is pinned with an INJECTED `lintOne` rather than the real
// commitlint CLI, keeping this file dependency-light (node: builtins only, per
// .github/scripts/README.md) and offline — the same idiom
// commitlint-range.test.mjs uses, and for the same reason: `defaultLintOne`'s
// wiring to the real CLI is shared with that module and pinned there.
//
// The fake reproduces the two rules that actually decide this gate's verdicts
// under .commitlintrc.json — `type-enum` and `header-max-length: 100` — so the
// length cases below are testing the real threshold, not a mock's opinion of
// it. The subjects used as fixtures are the real ones found on `main`.

import assert from 'node:assert/strict';
import { test } from 'node:test';

import { landingSubject, run } from './commitlint-pr-title.mjs';

// The repository's own type-enum, verbatim from .commitlintrc.json.
const TYPES = [
  'build', 'chore', 'ci', 'docs', 'feat', 'fix', 'perf', 'refactor', 'revert', 'style', 'test',
];
const HEADER_MAX_LENGTH = 100;

// fakeCommitlint — what commitlint@19 reports for these inputs under this
// repo's config. Deliberately only the two load-bearing rules: a fake that
// accepted everything would let every assertion below pass vacuously.
function fakeCommitlint(message) {
  const header = message.split('\n', 1)[0];
  const problems = [];
  const m = /^([a-zA-Z]+)(\([^)]*\))?!?: .+/.exec(header);
  if (!m) problems.push('subject may not be empty / type may not be empty');
  else if (!TYPES.includes(m[1])) problems.push(`type must be one of [${TYPES.join(', ')}]`);
  if (header.length > HEADER_MAX_LENGTH) {
    problems.push(`header must not be longer than ${HEADER_MAX_LENGTH} characters, current length is ${header.length}`);
  }
  return { status: problems.length === 0 ? 0 : 1, stdout: problems.join('\n'), stderr: '' };
}

const lint = (title, number) =>
  run({ title, number, lintOne: fakeCommitlint });

test('landingSubject appends the reference GitHub appends', () => {
  assert.equal(landingSubject('fix(ci): thing', 3187), 'fix(ci): thing (#3187)');
});

test('landingSubject does not double a reference the title already carries', () => {
  assert.equal(landingSubject('fix(ci): thing (#3187)', 3187), 'fix(ci): thing (#3187)');
});

test('landingSubject still appends when the title ends with a DIFFERENT reference', () => {
  // Real shape from `main`: "…(#3128) (#3140)" — a title citing another issue
  // gets its own reference appended on top.
  assert.equal(landingSubject('fix(ci): thing (#3128)', 3140), 'fix(ci): thing (#3128) (#3140)');
});

test('a conventional title passes', () => {
  assert.equal(lint('fix(ci): close the gate scope holes', 3187), 0);
});

test('a title with a type outside type-enum fails', () => {
  // Verbatim from main: 3c506aa03 landed `investigate(chclient): …`, which no
  // commitlint run would have accepted.
  assert.equal(lint('investigate(chclient): round 4 of DataShardFanoutGate server-side profiling', 3140), 1);
});

test('a title with no conventional prefix at all fails', () => {
  // Verbatim from main: d45e4c495.
  assert.equal(lint('Read exact DELTA-prefix aggregate table for instant rate()/increase()', 2514), 1);
});

test('a title that fits in 100 chars but LANDS over 100 fails', () => {
  // The whole reason this gate lints the composed subject. Exactly 100 chars of
  // title is legal as a commit subject and illegal as a squash subject, because
  // ` (#2881)` rides along.
  const title = `fix(ci): ${'x'.repeat(100 - 'fix(ci): '.length)}`;
  assert.equal(title.length, 100, 'fixture must sit exactly on the limit');
  assert.equal(
    run({ title, number: 2881, lintOne: fakeCommitlint }),
    1,
    'a 100-char title lands at 108 and must be rejected',
  );
  // And the same title one character shorter, which lands at exactly 100, passes
  // — so the assertion above is about the boundary, not about long titles.
  assert.equal(run({ title: title.slice(0, 92), number: 2881, lintOne: fakeCommitlint }), 0);
});

test('an empty or blank title FAILS rather than passing having linted nothing', () => {
  for (const empty of [undefined, null, '', '   ']) {
    assert.equal(
      run({ title: empty, number: 1, lintOne: fakeCommitlint }),
      1,
      `an empty title (${JSON.stringify(empty)}) must fail the gate`,
    );
  }
});

test('a missing or non-numeric PR number FAILS rather than linting the bare title', () => {
  // Composing the wrong subject silently is the same defect as linting none:
  // the gate would report on a string that never reaches main.
  for (const bad of [undefined, null, '', 'abc', '12a']) {
    assert.equal(
      run({ title: 'fix(ci): thing', number: bad, lintOne: fakeCommitlint }),
      1,
      `a bad PR number (${JSON.stringify(bad)}) must fail the gate`,
    );
  }
});

test('the injected linter is actually consulted (the fake is not a rubber stamp)', () => {
  // Guards this file against the failure mode it exists to prevent: if `run`
  // stopped calling lintOne, every assertion above would still pass.
  let calls = 0;
  const counting = (message) => {
    calls += 1;
    return fakeCommitlint(message);
  };
  run({ title: 'fix(ci): thing', number: 1, lintOne: counting });
  assert.equal(calls, 1, 'run() must lint through the injected linter exactly once');
});
