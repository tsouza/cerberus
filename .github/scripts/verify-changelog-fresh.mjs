// verify-changelog-fresh.mjs — the freshness half of cerberus#2739.
//
// prepare-release.mjs generates CHANGELOG.md's new `[vX.Y.Z]` section, the
// chart's `artifacthub.io/changes` annotation, and the release PR's body
// exactly ONCE, at PR-creation time, from the conventional commits between
// the last tag and the branch tip at that moment. Nothing re-derives or
// re-checks any of the three against the branch's ACTUAL final commit range
// before the PR merges — so a commit landing on a release branch afterward
// (a CI-timeout fix pushed directly, or a `main` merge pulling in an
// unrelated fix) silently falls out of all three. That happened for real on
// the v1.19.0 cycle: PR #2736's CHANGELOG section and PR body both omitted
// two genuine `fix(ci):` commits that landed after the PR opened, caught only
// because a human happened to notice the PR body looked stale.
//
// This script closes the CHANGELOG.md half of that gap: it re-derives the
// SAME commit range prepare-release.mjs would (commitsSinceLastTag, the
// identical `--no-merges` `git log` call), re-renders the SAME bullets
// (parseCommits + bullets, the identical functions, not a re-implementation
// that could drift from them), and asserts every one of those bullets is
// present verbatim inside CHANGELOG.md's `[v<appVersion>]` section, where
// appVersion is read from the release branch's own Chart.yaml. A commit
// present in the range but absent from the section fails the run with the
// missing bullet's exact text, so a late-landing fix is caught before merge
// instead of silently shipping undocumented.
//
// Scope: CHANGELOG.md only, not the chart annotation or the PR body — both
// mirror CHANGELOG.md's own generation and the PR body is free-form prose a
// maintainer edits, so re-deriving a byte-exact expectation for either would
// false-positive on legitimate hand-editing. CHANGELOG.md is the one of the
// three with a stable, mechanically-checkable "commit -> bullet" contract.
//
// Wired into pr-hygiene.yml's existing `pr-body` job (a required check that
// already re-triggers on `synchronize`, i.e. every push to an open PR) as an
// additional step gated to `release/*` head branches — no new required
// context to add to branch protection, and it re-runs on exactly the event
// that can make the CHANGELOG stale again.
//
// Env:
//   CHART_FILE      path to the chart being released; default
//                    deploy/helm/cerberus/Chart.yaml
//   CHANGELOG_FILE   path to the changelog; default CHANGELOG.md
//
// Exit: 0 when every commit-derived bullet is present; 1 otherwise, or on a
// missing appVersion / changelog section.
//
// node: builtins only — no npm deps, no setup-node needed.

import { readFileSync } from 'node:fs';
import process from 'node:process';
import { error, notice } from './lib/gh.mjs';
import { parseCommits, bullets, SECTIONS, commitsSinceLastTag } from './prepare-release.mjs';

const CHART_FILE = process.env.CHART_FILE || 'deploy/helm/cerberus/Chart.yaml';
const CHANGELOG_FILE = process.env.CHANGELOG_FILE || 'CHANGELOG.md';

/** Reads `appVersion:` out of a Chart.yaml, matching prepare-release.mjs's own regex. */
export function readAppVersion(chartText) {
  const m = /^appVersion:\s*"?([^"\n]+)"?$/m.exec(chartText);
  if (!m) throw new Error(`${CHART_FILE}: no appVersion: field found`);
  return m[1].trim();
}

// Escapes every regex metacharacter, not just `.` — appVersion is read from
// Chart.yaml (readAppVersion), untrusted input as far as this function is
// concerned, and a partial escape (the earlier `.replace(/\./g, ...)` this
// replaced) still lets any OTHER metacharacter — a stray `\`, `(`, `*` — warp
// the compiled pattern instead of matching literally.
function escapeRegExp(s) {
  return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

/**
 * Extracts one `## [vX.Y.Z] — <date>` section's body out of a changelog: from
 * that exact heading (any date) to the next `## [` heading, or EOF. Returns
 * null if the heading is absent — the caller decides whether that is an
 * error (it always is here: a release branch staging appVersion X must have
 * a `## [vX]` section, or nothing was generated for it at all).
 */
export function extractChangelogSection(text, appVersion) {
  const headingRe = new RegExp(`^## \\[v${escapeRegExp(appVersion)}\\].*$`, 'm');
  const m = headingRe.exec(text);
  if (!m) return null;
  const start = m.index + m[0].length;
  const rest = text.slice(start);
  const next = rest.search(/\n## \[/);
  return next === -1 ? rest : rest.slice(0, next);
}

/**
 * Every bullet [renderChangelogSection] would emit for `parsed`, flattened
 * into one array (not grouped by section) since presence — not position — is
 * what this check verifies. Mirrors [renderChangelogSection]'s own type loop
 * rather than importing it directly, because that function also renders the
 * `### <heading>` lines this check does not need to assert on.
 */
export function expectedBullets(parsed) {
  const out = [];
  for (const [type] of SECTIONS) {
    const entries = parsed.groups[type];
    if (entries && entries.length) out.push(...bullets(entries).split('\n'));
  }
  return out;
}

/** The bullets `sectionText` is missing from `expected`, in `expected`'s own order. */
export function missingBullets(expected, sectionText) {
  return expected.filter((b) => !sectionText.includes(b));
}

function main() {
  const chartText = readFileSync(CHART_FILE, 'utf8');
  const appVersion = readAppVersion(chartText);

  const changelogText = readFileSync(CHANGELOG_FILE, 'utf8');
  const section = extractChangelogSection(changelogText, appVersion);
  if (section === null) {
    error(`${CHANGELOG_FILE}: no "## [v${appVersion}]" section — this release branch's own Chart.yaml ` +
      `stages appVersion ${appVersion}, so a matching section must exist`);
    process.exit(1);
  }

  const parsed = parseCommits(commitsSinceLastTag());
  const expected = expectedBullets(parsed);
  const missing = missingBullets(expected, section);

  if (missing.length > 0) {
    error(
      `${CHANGELOG_FILE}'s [v${appVersion}] section is missing ${missing.length} commit(s) present in the ` +
        `branch's actual history since the last tag — a commit landed after CHANGELOG.md was generated:\n` +
        missing.map((b) => `  ${b}`).join('\n') +
        `\nRegenerate by hand (or re-run prepare-release.yml) so the shipped notes match the shipped commits.`,
    );
    process.exit(1);
  }

  notice(`${CHANGELOG_FILE}: [v${appVersion}] accounts for every commit since the last tag (${expected.length} bullet(s))`);
}

// Import-safe: the tests import the helpers without running main().
if (process.argv[1] && process.argv[1].endsWith('verify-changelog-fresh.mjs')) {
  main();
}
