// brew-upgrade-path.test.mjs — node:test guard for the formula -> cask upgrade
// harness's verdict, run on the required `lint` job.
//
// The harness itself needs a Homebrew installation and several minutes, so it
// runs weekly rather than per-PR. Its verdict function does not, and it is the
// part that decides whether a real migration failure is reported — a verdict
// that reads a silent no-op as success would leave the weekly lane green while
// every existing install stayed stranded, which is the same shape as the bug it
// was written for.

import { test } from 'node:test';
import assert from 'node:assert/strict';

import { upgradeOutcomeProblems } from './brew-upgrade-path.mjs';
import { brewListNames } from './brew-smoke.mjs';

const ANNOUNCED = [
  '==> tsouza/tap/cerberus has been migrated from a formula to a cask.',
  '==> brew unlink cerberus',
  '==> brew install --cask tsouza/tap/cerberus',
].join('\n');

const CASK_INSTALLED = 'cerberus 1.13.2\n';
const NEW = '1.13.2';
const OLD = '1.13.1';

const healthy = {
  updateOutput: ANNOUNCED,
  caskList: CASK_INSTALLED,
  reportedVersion: NEW,
  expectedVersion: NEW,
};

test('a migration that ran, installed and linked has no problems', () => {
  assert.deepEqual(upgradeOutcomeProblems(healthy), []);
});

test('a silent update is the reported failure', () => {
  // The bug exactly as it shipped: `brew update` succeeds, says nothing about
  // cerberus, and leaves the formula linked. Every signal here is an absence,
  // which is why the assertion is on the announcement rather than on the exit
  // status of anything.
  const problems = upgradeOutcomeProblems({
    ...healthy,
    updateOutput: '==> Updated Homebrew from abc to def.\nAlready up-to-date.',
    caskList: '',
    reportedVersion: OLD,
  });
  assert.equal(problems.length, 3, 'all three symptoms should be named, not just the first');
  assert.match(problems[0], /did not announce/);
  assert.match(problems[0], /tap_migrations\.json/, 'the failure must point at the cause');
});

test('an announced migration whose install failed is caught', () => {
  // Homebrew rescues any exception raised while installing a migration target,
  // so a failed install leaves the announcement in the log and nothing else. If
  // the announcement were the only assertion, this would pass.
  const problems = upgradeOutcomeProblems({ ...healthy, caskList: '', reportedVersion: OLD });
  assert.equal(problems.length, 2);
  assert.match(problems[0], /cask is not installed/);
  assert.match(problems[1], /reports "1\.13\.1"/);
});

test('a cask that installed but could not link is caught', () => {
  // The state the bug report carried: the Caskroom holds the new version, the
  // formula's keg still owns the binary, and `--version` disagrees with the
  // release. Asserting only that the cask is installed would call this healthy.
  const problems = upgradeOutcomeProblems({ ...healthy, reportedVersion: OLD });
  assert.equal(problems.length, 1);
  assert.match(problems[0], /could not link over the formula's keg/);
});

test('a cerberus that vanished from PATH is a failure, not a pass', () => {
  // `brew unlink` runs before the cask install, so a migration that unlinks and
  // then fails to install removes the command entirely. The harness reports that
  // as a sentinel version rather than crashing, and it must not compare equal to
  // anything.
  const problems = upgradeOutcomeProblems({ ...healthy, reportedVersion: '<not on PATH>' });
  assert.equal(problems.length, 1);
  assert.match(problems[0], /<not on PATH>/);
});

test('an absent update output is treated as no announcement, not as a crash', () => {
  // A `brew update` that died before writing anything yields undefined here.
  // Reading that as "nothing to migrate" would be the same silent pass the
  // harness exists to prevent.
  const problems = upgradeOutcomeProblems({ ...healthy, updateOutput: undefined });
  assert.equal(problems.length, 1);
  assert.match(problems[0], /did not announce/);
});

test('the version comparison is exact, not a prefix or substring', () => {
  // `1.13.20` contains `1.13.2`. A substring comparison would report a two-minor
  // drift as a match, and the whole point of this lane is the version the user
  // ends up on.
  assert.equal(upgradeOutcomeProblems({ ...healthy, reportedVersion: '1.13.20' }).length, 1);
  assert.equal(upgradeOutcomeProblems({ ...healthy, reportedVersion: 'v1.13.2' }).length, 1);
});

test('brewListNames reads the leading token, so a version cannot fake a name', () => {
  // Shared with brew-smoke's installed-artifact assertion. `brew list --versions`
  // prints `<name> <version…>`, and a search of the whole line would let a
  // package whose VERSION contains cerberus answer an is-cerberus-installed
  // question.
  assert.deepEqual([...brewListNames('cerberus 1.13.2\njq 1.7\n')].sort(), ['cerberus', 'jq']);
  assert.equal(brewListNames('cerberus-exporter 1.0\n').has('cerberus'), false);
  assert.equal(brewListNames('other 1.0-cerberus\n').has('cerberus'), false);
  assert.equal(brewListNames('').size, 0);
  assert.equal(brewListNames(null).size, 0);
});
