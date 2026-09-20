import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { extractReleaseSection, forceLeaseArg, insertReleaseSection, syncBranchName, validateInputs } from './maintenance-changelog-sync.mjs';

const release = `## [v1.21.1] — 2026-09-20

### Fixed

- **schema:** avoid duplicate body text index (#3622)`;
const gitShaHexLength = 40;
const fullGitSha = 'a'.repeat(gitShaHexLength);
const main = `# Changelog

## [Unreleased]

### Added

- **telemetry:** new controls

## [v1.21.0] — 2026-09-20

### Fixed

- old fix
`;

test('extractReleaseSection returns exactly one tagged section', () => {
  const withRelease = main.replace('## [v1.21.0]', `${release}\n\n## [v1.21.0]`);
  assert.equal(extractReleaseSection(withRelease, 'v1.21.1'), release);
  assert.throws(() => extractReleaseSection(main, 'v1.21.1'), /has no v1.21.1 section/);
});

test('insert preserves unreleased content and orders the patch before older releases', () => {
  const synced = insertReleaseSection(main, release, 'v1.21.1');
  assert.ok(synced.indexOf('**telemetry:** new controls') < synced.indexOf('## [v1.21.1]'));
  assert.ok(synced.indexOf('## [v1.21.1]') < synced.indexOf('## [v1.21.0]'));
  assert.equal(extractReleaseSection(synced, 'v1.21.1'), release);
});

test('insert is idempotent and rejects conflicting published history', () => {
  const synced = insertReleaseSection(main, release, 'v1.21.1');
  assert.equal(insertReleaseSection(synced, release, 'v1.21.1'), synced);
  assert.throws(() => insertReleaseSection(synced, `${release}\n- different`, 'v1.21.1'), /conflicting/);
});

test('inputs admit only maintenance release branches and stable tags', () => {
  assert.doesNotThrow(() => validateInputs({ tag: 'v1.21.1', branch: 'release/1.21.x', repository: 'tsouza/cerberus' }));
  assert.throws(() => validateInputs({ tag: 'v1.21.1', branch: 'main', repository: 'tsouza/cerberus' }), /RELEASE_BRANCH/);
  assert.throws(() => syncBranchName('v1.21.1-rc.1'), /invalid release tag/);
  assert.equal(syncBranchName('v1.21.1'), 'chore/changelog-1.21.1');
});

test('push lease is absent-safe on first run and exact-SHA safe on a retry', () => {
  assert.equal(forceLeaseArg('chore/changelog-1.21.1', ''),
    '--force-with-lease=refs/heads/chore/changelog-1.21.1:');
  assert.equal(forceLeaseArg('chore/changelog-1.21.1', `${fullGitSha}\trefs/heads/chore/changelog-1.21.1`),
    `--force-with-lease=refs/heads/chore/changelog-1.21.1:${fullGitSha}`);
  assert.throws(() => forceLeaseArg('chore/changelog-1.21.1', 'not-a-sha\trefs/heads/x'), /invalid remote SHA/);
});

test('release workflow opens the sync PR only after a successful maintenance publish', () => {
  const workflow = readFileSync(new URL('../workflows/release.yml', import.meta.url), 'utf8');
  const start = workflow.indexOf('\n  maintenance-changelog-sync:');
  const end = workflow.indexOf('\n  brew-smoke:', start);
  const job = workflow.slice(start, end);
  assert.match(job, /needs: \[gate, publish\]/);
  assert.match(job, /startsWith\(github\.ref, 'refs\/heads\/release\/'\)/);
  assert.match(job, /endsWith\(github\.ref, '\.x'\)/);
  assert.match(job, /needs\.publish\.result == 'success'/);
  assert.match(job, /pull-requests: write/);
  assert.match(job, /node \.github\/scripts\/maintenance-changelog-sync\.mjs/);
  assert.doesNotMatch(job, /git push[^\n]*main/);
  assert.match(workflow, /RELEASE_SELF_JOBS:[\s\S]*maintenance-changelog-sync/);
});
