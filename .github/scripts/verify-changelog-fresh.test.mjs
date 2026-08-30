// verify-changelog-fresh.test.mjs — pins cerberus#2739's freshness gate: a
// commit landing on a release branch after CHANGELOG.md was generated must
// be caught, not silently shipped undocumented.

import assert from 'node:assert/strict';
import { test } from 'node:test';

import { parseCommits } from './prepare-release.mjs';
import { readAppVersion, extractChangelogSection, expectedBullets, missingBullets } from './verify-changelog-fresh.mjs';

test('readAppVersion reads the quoted appVersion Chart.yaml carries', () => {
  const chart = 'version: 0.15.10\nappVersion: "1.19.0"\n';
  assert.equal(readAppVersion(chart), '1.19.0');
});

test('readAppVersion throws when the field is absent', () => {
  assert.throws(() => readAppVersion('version: 0.15.10\n'), /no appVersion/);
});

test('extractChangelogSection finds the exact version heading, any date', () => {
  const cl = [
    '# Changelog',
    '',
    '## [Unreleased]',
    '',
    '## [v1.19.0] — 2026-08-30',
    '',
    '### Fixed',
    '',
    '- **ci:** a fix',
    '',
    '## [v1.18.1] — 2026-08-29',
    '',
    '### Fixed',
    '',
    '- **x:** an older fix',
  ].join('\n');
  const section = extractChangelogSection(cl, '1.19.0');
  assert.match(section, /- \*\*ci:\*\* a fix/);
  assert.doesNotMatch(section, /an older fix/, 'must stop before the NEXT version heading');
});

test('extractChangelogSection returns null when the heading is absent', () => {
  const cl = '# Changelog\n\n## [Unreleased]\n\n## [v1.18.1] — 2026-08-29\n\n### Fixed\n\n- x\n';
  assert.equal(extractChangelogSection(cl, '1.19.0'), null);
});

test('extractChangelogSection does not cross-match a version that is a PREFIX of another', () => {
  // v1.19.0 vs v1.19.0-something would never occur (semver has no such
  // suffix here), but v1.9.0 vs v1.19.0 sharing "1.9.0" as a substring must
  // not cross-match — the heading regex anchors the FULL bracketed version.
  const cl = '## [v1.9.0] — 2026-01-01\n\n### Fixed\n\n- **x:** old\n';
  assert.equal(extractChangelogSection(cl, '1.19.0'), null);
});

test('expectedBullets renders exactly what SECTIONS-listed commits would produce, dropping unlisted types', () => {
  const parsed = parseCommits([
    'fix(ci): relieve contention',
    'test(chplan): cover a branch', // NOT in SECTIONS — must be dropped
    'chore(release): bump versions', // NOT in SECTIONS — must be dropped
    'docs(promql): fix a comment',
  ]);
  const got = expectedBullets(parsed);
  assert.deepStrictEqual(got, ['- **ci:** relieve contention', '- **promql:** fix a comment']);
});

test('expectedBullets orders by SECTIONS (Fixed before Documentation), not commit order', () => {
  const parsed = parseCommits(['docs(x): later doc', 'fix(y): earlier fix']);
  const got = expectedBullets(parsed);
  assert.deepStrictEqual(got, ['- **y:** earlier fix', '- **x:** later doc']);
});

test('missingBullets reports nothing when every expected bullet is present verbatim', () => {
  const section = '### Fixed\n\n- **ci:** relieve contention\n- **promql:** fix a thing\n';
  assert.deepStrictEqual(missingBullets(['- **ci:** relieve contention', '- **promql:** fix a thing'], section), []);
});

test('missingBullets reports the exact bullet a late-landing commit is missing', () => {
  const section = '### Fixed\n\n- **ci:** relieve contention\n';
  const missing = missingBullets(['- **ci:** relieve contention', '- **ci:** a SECOND fix that landed later'], section);
  assert.deepStrictEqual(missing, ['- **ci:** a SECOND fix that landed later']);
});

test('missingBullets does a substring check, tolerating a trailing PR-number suffix already baked into the bullet', () => {
  // desc already carries "(#2734)" verbatim (from the commit subject, per
  // GitHub's squash-merge convention) — the bullet text is compared as one
  // exact string, not word-by-word, so this is really asserting substring
  // containment works for the common case, not a special "PR number" path.
  const section = '### Fixed\n\n- **chsql:** bound emitted SQL at ClickHouse\'s own max_query_size (#2734)\n';
  assert.deepStrictEqual(
    missingBullets(["- **chsql:** bound emitted SQL at ClickHouse's own max_query_size (#2734)"], section),
    [],
  );
});

test('end-to-end: a commit added after generation is caught', () => {
  // Simulates exactly the v1.19.0 incident: CHANGELOG.md generated from an
  // earlier commit set, then a fix(ci) commit lands on the branch afterward.
  const changelogAtGenerationTime = [
    '## [v1.19.0] — 2026-08-30',
    '',
    '### Fixed',
    '',
    '- **chsql:** bound emitted SQL at ClickHouse\'s own max_query_size (#2734)',
    '',
  ].join('\n');
  const actualCommitsNow = parseCommits([
    'fix(chsql): bound emitted SQL at ClickHouse\'s own max_query_size (#2734)',
    "fix(ci): relieve roundtrip-promql-shard's CPU contention and timeout budget",
  ]);
  const section = extractChangelogSection(changelogAtGenerationTime, '1.19.0');
  const missing = missingBullets(expectedBullets(actualCommitsNow), section);
  assert.deepStrictEqual(missing, ["- **ci:** relieve roundtrip-promql-shard's CPU contention and timeout budget"]);
});

test('end-to-end: a fully backfilled CHANGELOG passes clean', () => {
  const changelogBackfilled = [
    '## [v1.19.0] — 2026-08-30',
    '',
    '### Fixed',
    '',
    '- **ci:** relieve roundtrip-promql-shard\'s CPU contention and timeout budget',
    '- **chsql:** bound emitted SQL at ClickHouse\'s own max_query_size (#2734)',
    '',
  ].join('\n');
  const actualCommitsNow = parseCommits([
    'fix(chsql): bound emitted SQL at ClickHouse\'s own max_query_size (#2734)',
    "fix(ci): relieve roundtrip-promql-shard's CPU contention and timeout budget",
  ]);
  const section = extractChangelogSection(changelogBackfilled, '1.19.0');
  assert.deepStrictEqual(missingBullets(expectedBullets(actualCommitsNow), section), []);
});
