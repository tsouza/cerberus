// brew-smoke.test.mjs — node:test guard for the post-publish Homebrew smoke's
// pure core, run on the required `lint` lane AND as the first step of the
// `brew-smoke` job itself.
//
// release.yml has no `pull_request:` trigger, so without this suite every edit
// to the smoke would be unverified until a release is cut. The cases below pin
// the three ways the smoke could go hollow:
//
//   1. a stale/never-pushed formula reported as fine (the goreleaser `brews:`
//      regression the whole job exists to catch);
//   2. a no-formula branch that bails out instead of asserting — `t.Skip` in
//      workflow clothing, which would wave through a `skip_upload` regression;
//   3. an unparseable formula degrading into "couldn't tell, so pass";
//   4. a maintenance backport silently overwriting the newest line's formula,
//      which is what v1.12.1 did to v1.13.0's.

import { test } from 'node:test';
import assert from 'node:assert/strict';

import { isStableRelease, formulaVersion, verdict, compareVersions } from './brew-smoke.mjs';

// A minimal stand-in for what goreleaser writes into the tap.
function formula(version) {
  return [
    'class Cerberus < Formula',
    '  desc "Drop-in Prometheus / Loki / Tempo HTTP gateway for ClickHouse"',
    '  homepage "https://github.com/tsouza/cerberus"',
    `  version "${version}"`,
    '  license "Apache-2.0"',
    '',
    '  on_macos do',
    `    url "https://github.com/tsouza/cerberus/releases/download/v${version}/cerberus_${version}_darwin_arm64.tar.gz"`,
    '  end',
    '',
    '  def install',
    '    bin.install "cerberus"',
    '  end',
    'end',
  ].join('\n');
}

test('isStableRelease separates stable releases from prereleases', () => {
  assert.equal(isStableRelease('1.11.2'), true);
  assert.equal(isStableRelease('10.0.0'), true);
  assert.equal(isStableRelease('1.11.2-rc.1'), false);
  assert.equal(isStableRelease('1.0.0-RC1'), false);
  assert.equal(isStableRelease('v1.11.2'), false, 'the `v`-prefixed tag is not the app version');
  assert.equal(isStableRelease(''), false);
  assert.equal(isStableRelease(undefined), false);
});

test('formulaVersion reads the declaration, falls back to the archive name', () => {
  assert.equal(formulaVersion(formula('1.11.2')), '1.11.2');

  // No `version "…"` line: the archive filename still carries it.
  const archiveOnly = [
    'class Cerberus < Formula',
    '  url "https://github.com/tsouza/cerberus/releases/download/v1.11.2/cerberus_1.11.2_linux_amd64.tar.gz"',
    'end',
  ].join('\n');
  assert.equal(formulaVersion(archiveOnly), '1.11.2');
});

test('an unparseable formula THROWS rather than passing', () => {
  assert.throws(() => formulaVersion('class Cerberus < Formula\nend\n'), /could not determine the version/);
  assert.throws(() => formulaVersion(''), /could not determine the version/);
  assert.throws(() => formulaVersion(undefined), /could not determine the version/);
});

test('stable + matching formula: no problems, install required', () => {
  const v = verdict({ version: '1.11.2', formulaSource: formula('1.11.2'), isLatest: 'true' });
  assert.deepEqual(v.problems, []);
  assert.equal(v.mustInstall, true);
});

test('stable + STALE formula: blocked before brew is ever invoked', () => {
  // The regression: brews block deleted, HOMEBREW_TAP_GITHUB_TOKEN expired, or
  // the cross-repo push silently failed. The tap keeps declaring the previous
  // version. A warm brew cache cannot mask this — it is read from the tap's git
  // state, not from brew.
  const v = verdict({ version: '1.11.2', formulaSource: formula('1.11.1'), isLatest: 'true' });
  assert.equal(v.problems.length, 1, `expected exactly one problem, got: ${v.problems.join('; ')}`);
  assert.match(v.problems[0], /declares version "1\.11\.1" but this release is "1\.11\.2"/);
  assert.equal(v.mustInstall, true, 'a stale formula does not excuse skipping the install branch');
});

test('prerelease + older formula: no problems, and NO install', () => {
  // `skip_upload: auto` means the prerelease wrote no formula, so the tap still
  // declares the last stable. That is the healthy state — asserted, not skipped.
  const v = verdict({ version: '1.12.0-rc.1', formulaSource: formula('1.11.2'), isLatest: 'false' });
  assert.deepEqual(v.problems, []);
  assert.equal(v.mustInstall, false);
});

test('prerelease + formula carrying THIS version: the skip_upload regression', () => {
  const v = verdict({ version: '1.12.0-rc.1', formulaSource: formula('1.12.0-rc.1'), isLatest: 'false' });
  assert.equal(v.problems.length, 1, `expected exactly one problem, got: ${v.problems.join('; ')}`);
  assert.match(v.problems[0], /declares prerelease version "1\.12\.0-rc\.1"/);
  assert.match(v.problems[0], /skip_upload: auto/);
  assert.equal(v.mustInstall, false);
});

test('verdict is comparing the BARE version, so a `v`-prefixed input cannot pass', () => {
  // The off-by-`v` trap: release.yml must pass `needs.gate.outputs.app_version`
  // (bare), never `app_tag`. If it ever passed the tag, the stable branch's
  // equality check fails loudly here rather than being papered over by a
  // substring match.
  const v = verdict({ version: 'v1.11.2', formulaSource: formula('1.11.2'), isLatest: 'true' });
  assert.equal(v.mustInstall, false, 'a `v`-prefixed string is not a stable app version');
  assert.deepEqual(v.problems, [], 'and it does not collide with the formula version either');
});

test('an unparseable formula propagates out of verdict on both branches', () => {
  assert.throws(() => verdict({ version: '1.11.2', formulaSource: 'garbage', isLatest: 'true' }), /could not determine the version/);
  assert.throws(() => verdict({ version: '1.12.0-rc.1', formulaSource: 'garbage', isLatest: 'false' }), /could not determine the version/);
});

test('compareVersions orders the two shapes goreleaser produces', () => {
  assert.equal(compareVersions('1.13.0', '1.12.1'), 1);
  assert.equal(compareVersions('1.12.1', '1.13.0'), -1);
  assert.equal(compareVersions('1.12.1', '1.12.1'), 0);
  assert.equal(compareVersions('1.13.0', '1.9.9'), 1, 'numeric, not lexicographic');
  assert.equal(compareVersions('2.0.0', '1.99.99'), 1);

  // A prerelease sorts below the same triple released.
  assert.equal(compareVersions('1.13.0-rc.1', '1.13.0'), -1);
  assert.equal(compareVersions('1.13.0', '1.13.0-rc.1'), 1);
  assert.equal(compareVersions('1.14.0-rc.1', '1.13.1'), 1);

  // Malformed components sort as 0 rather than NaN: NaN comparisons are all
  // false, which would make `compareVersions(declared, v) <= 0` silently pass.
  assert.equal(compareVersions('garbage', '1.0.0'), -1);
  assert.equal(compareVersions(undefined, '0.0.0'), 0);
});

test('maintenance backport + newer formula: no problems, and NO install', () => {
  // v1.12.1 published while the tap is on v1.13.0. `skip_upload` resolves to
  // "true" off RELEASE_IS_LATEST, so the tap is untouched and still ahead. This
  // is the healthy state — asserted, not skipped.
  const v = verdict({ version: '1.12.1', formulaSource: formula('1.13.0'), isLatest: 'false' });
  assert.deepEqual(v.problems, []);
  assert.equal(v.mustInstall, false, 'installing would smoke the NEWEST line, not this release');
});

test('maintenance backport that OVERWROTE the tap: the v1.12.1-over-v1.13.0 regression', () => {
  // The bug this branch exists for: `skip_upload: auto` alone does not filter a
  // stable backport, so goreleaser wrote the older line's formula over the
  // newer one and every `brew install` started downgrading.
  const v = verdict({ version: '1.12.1', formulaSource: formula('1.12.1'), isLatest: 'false' });
  assert.equal(v.problems.length, 1, `expected exactly one problem, got: ${v.problems.join('; ')}`);
  assert.match(v.problems[0], /declares version "1\.12\.1", which is not newer than this maintenance release "1\.12\.1"/);
  assert.equal(v.mustInstall, false);
});

test('maintenance backport over an OLDER formula is also blocked', () => {
  // Not the same regression, but the same broken outcome: the tap is behind the
  // newest released line, so `brew install` is serving stale bytes.
  const v = verdict({ version: '1.12.1', formulaSource: formula('1.11.3'), isLatest: 'false' });
  assert.equal(v.problems.length, 1, `expected exactly one problem, got: ${v.problems.join('; ')}`);
  assert.match(v.problems[0], /not newer than this maintenance release/);
});

test('a forward prerelease takes the prerelease branch, not the maintenance one', () => {
  // RELEASE_IS_LATEST compares against the highest STABLE tag, so v1.14.0-rc.1
  // arrives with isLatest=false while the tap legitimately sits on v1.13.1 —
  // BEHIND it. The maintenance branch's "the tap must be ahead of us" assertion
  // does not hold here, and testing the version shape first is what keeps every
  // release-candidate from reding this job.
  const v = verdict({ version: '1.14.0-rc.1', formulaSource: formula('1.13.1'), isLatest: 'false' });
  assert.deepEqual(v.problems, []);
  assert.equal(v.mustInstall, false);
});

test('isLatest is read strictly: only the exact string "true" owns the formula', () => {
  // The driver rejects anything that is not "true"/"false" before reaching here,
  // but verdict must not treat a truthy-looking value as the newest line either
  // — that would restore the equality assertion for a backport and demand a
  // formula it deliberately never wrote.
  const stale = { version: '1.12.1', formulaSource: formula('1.13.0') };
  assert.deepEqual(verdict({ ...stale, isLatest: true }).problems.length, 1, 'boolean true is the newest line');
  assert.deepEqual(verdict({ ...stale, isLatest: 'false' }).problems, []);
  assert.deepEqual(verdict({ ...stale, isLatest: '' }).problems, [], 'not "true" means not the newest line');
});
