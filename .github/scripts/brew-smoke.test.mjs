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
//   2. a prerelease branch that bails out instead of asserting — `t.Skip` in
//      workflow clothing, which would wave through a `skip_upload` regression;
//   3. an unparseable formula degrading into "couldn't tell, so pass".

import { test } from 'node:test';
import assert from 'node:assert/strict';

import { isStableRelease, formulaVersion, verdict } from './brew-smoke.mjs';

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
  const v = verdict({ version: '1.11.2', formulaSource: formula('1.11.2') });
  assert.deepEqual(v.problems, []);
  assert.equal(v.mustInstall, true);
});

test('stable + STALE formula: blocked before brew is ever invoked', () => {
  // The regression: brews block deleted, HOMEBREW_TAP_GITHUB_TOKEN expired, or
  // the cross-repo push silently failed. The tap keeps declaring the previous
  // version. A warm brew cache cannot mask this — it is read from the tap's git
  // state, not from brew.
  const v = verdict({ version: '1.11.2', formulaSource: formula('1.11.1') });
  assert.equal(v.problems.length, 1, `expected exactly one problem, got: ${v.problems.join('; ')}`);
  assert.match(v.problems[0], /declares version "1\.11\.1" but this release is "1\.11\.2"/);
  assert.equal(v.mustInstall, true, 'a stale formula does not excuse skipping the install branch');
});

test('prerelease + older formula: no problems, and NO install', () => {
  // `skip_upload: auto` means the prerelease wrote no formula, so the tap still
  // declares the last stable. That is the healthy state — asserted, not skipped.
  const v = verdict({ version: '1.12.0-rc.1', formulaSource: formula('1.11.2') });
  assert.deepEqual(v.problems, []);
  assert.equal(v.mustInstall, false);
});

test('prerelease + formula carrying THIS version: the skip_upload regression', () => {
  const v = verdict({ version: '1.12.0-rc.1', formulaSource: formula('1.12.0-rc.1') });
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
  const v = verdict({ version: 'v1.11.2', formulaSource: formula('1.11.2') });
  assert.equal(v.mustInstall, false, 'a `v`-prefixed string is not a stable app version');
  assert.deepEqual(v.problems, [], 'and it does not collide with the formula version either');
});

test('an unparseable formula propagates out of verdict on both branches', () => {
  assert.throws(() => verdict({ version: '1.11.2', formulaSource: 'garbage' }), /could not determine the version/);
  assert.throws(() => verdict({ version: '1.12.0-rc.1', formulaSource: 'garbage' }), /could not determine the version/);
});
