// brew-smoke.test.mjs — node:test guard for the post-publish Homebrew smoke's
// pure core, run on the required `lint` lane AND as the first step of the
// `brew-smoke` job itself.
//
// release.yml has no `pull_request:` trigger, so without this suite every edit
// to the smoke would be unverified until a release is cut. The cases below pin
// the four ways the smoke could go hollow:
//
//   1. a stale/never-pushed cask reported as fine (the goreleaser
//      `homebrew_casks:` regression the whole job exists to catch);
//   2. a no-cask branch that bails out instead of asserting — `t.Skip` in
//      workflow clothing, which would wave through a `skip_upload` regression;
//   3. an unparseable cask degrading into "couldn't tell, so pass";
//   4. a maintenance backport silently overwriting the newest line's cask,
//      which is what v1.12.1 did to v1.13.0's.

import { test } from 'node:test';
import assert from 'node:assert/strict';

import { isStableRelease, caskVersion, verdict, compareVersions } from './brew-smoke.mjs';

// A minimal stand-in for what goreleaser writes into the tap.
function cask(version) {
  return [
    'cask "cerberus" do',
    `  version "${version}"`,
    '',
    '  url "https://github.com/tsouza/cerberus/releases/download/v' +
      version +
      '/cerberus_' +
      version +
      '_darwin_arm64.tar.gz"',
    '  name "cerberus"',
    '  desc "Drop-in Prometheus / Loki / Tempo HTTP gateway for ClickHouse"',
    '  homepage "https://github.com/tsouza/cerberus"',
    '',
    '  binary "cerberus"',
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

test('caskVersion reads the declaration, falls back to the archive name', () => {
  assert.equal(caskVersion(cask('1.11.2')), '1.11.2');

  // No `version "…"` line: the archive filename still carries it.
  const archiveOnly = [
    'cask "cerberus" do',
    '  url "https://github.com/tsouza/cerberus/releases/download/v1.11.2/cerberus_1.11.2_linux_amd64.tar.gz"',
    'end',
  ].join('\n');
  assert.equal(caskVersion(archiveOnly), '1.11.2');
});

test('an unparseable cask THROWS rather than passing', () => {
  assert.throws(() => caskVersion('cask "cerberus" do\nend\n'), /could not determine the version/);
  assert.throws(() => caskVersion(''), /could not determine the version/);
  assert.throws(() => caskVersion(undefined), /could not determine the version/);
});

// The release-shape vocabulary the whole table is written in. `verdict` only
// ever compares two versions, so what each case pins is the RELATION between
// them — naming the versions keeps that relation on the page instead of leaving
// the reader to diff two literals.
const RELEASED = '1.12.1'; // the version being smoked
const SAME = RELEASED; // the tap serving exactly it
const OLDER = '1.11.3'; // the tap left behind
const NEWER = '1.13.0'; // the tap already on a higher line
const RC = '1.12.1-rc.1'; // a prerelease of RELEASED
const FORWARD_RC = '1.14.0-rc.1'; // a prerelease ABOVE everything released

// Each row: what the tap declares, what is being released, whether the release
// is the highest stable tag, and the state that combination must be reported in.
// `problem` null means "this is the healthy state"; a string means the case must
// be blocked, and blocked for the stated reason rather than incidentally.
const VERDICT_CASES = [
  {
    name: 'newest line + stable + the tap serving exactly it — the only shape that installs',
    tap: SAME,
    release: RELEASED,
    isLatest: 'true',
    mustInstall: true,
    problem: null,
  },
  {
    name: 'newest line + a STALE tap — blocked before brew is ever invoked',
    // The regression: brews block deleted, HOMEBREW_TAP_GITHUB_TOKEN expired, or
    // the cross-repo push silently failed, so the tap keeps declaring the
    // previous version. Read from the tap's git state, so a warm brew cache
    // cannot mask it.
    tap: OLDER,
    release: RELEASED,
    isLatest: 'true',
    mustInstall: true, // a stale cask does not excuse skipping the install branch
    problem: `declares version "${OLDER}" but this release is "${RELEASED}"`,
  },
  {
    name: 'prerelease + a tap on the last stable — healthy, asserted rather than skipped',
    // `skip_upload` wrote no cask, so the tap legitimately still declares an
    // older version.
    tap: OLDER,
    release: RC,
    isLatest: 'false',
    mustInstall: false,
    problem: null,
  },
  {
    name: 'prerelease the tap actually PUBLISHED — the skip_upload regression',
    tap: RC,
    release: RC,
    isLatest: 'false',
    mustInstall: false,
    problem: `declares prerelease version "${RC}"`,
  },
  {
    name: 'forward prerelease above a legitimately BEHIND tap — prerelease branch, not maintenance',
    // RELEASE_IS_LATEST compares against the highest STABLE tag, so every rc
    // arrives with isLatest=false while the tap sits below it. Testing the
    // version shape first is what keeps the maintenance branch's "the tap must
    // be ahead of us" rule — true only for backports — off every rc.
    tap: NEWER,
    release: FORWARD_RC,
    isLatest: 'false',
    mustInstall: false,
    problem: null,
  },
  {
    name: 'maintenance backport under a NEWER tap — healthy, and deliberately no install',
    // Installing here would smoke the newest line's binary and report it as this
    // release's.
    tap: NEWER,
    release: RELEASED,
    isLatest: 'false',
    mustInstall: false,
    problem: null,
  },
  {
    name: 'maintenance backport that OVERWROTE the tap — the v1.12.1-over-v1.13.0 regression',
    // `skip_upload: auto` alone does not filter a stable backport, so goreleaser
    // wrote the older line's cask over the newer one and every `brew install`
    // started downgrading.
    tap: SAME,
    release: RELEASED,
    isLatest: 'false',
    mustInstall: false,
    problem: `declares version "${SAME}", which is not newer than this maintenance release "${RELEASED}"`,
  },
  {
    name: 'maintenance backport over an OLDER tap — a different cause, the same broken outcome',
    tap: OLDER,
    release: RELEASED,
    isLatest: 'false',
    mustInstall: false,
    problem: 'not newer than this maintenance release',
  },
];

for (const c of VERDICT_CASES) {
  test(`verdict: ${c.name}`, () => {
    const v = verdict({ version: c.release, caskSource: cask(c.tap), isLatest: c.isLatest });
    if (c.problem === null) {
      assert.deepEqual(v.problems, [], 'expected the healthy state');
    } else {
      assert.equal(v.problems.length, 1, `expected exactly one problem, got: ${v.problems.join('; ')}`);
      assert.ok(v.problems[0].includes(c.problem), `expected a problem naming \`${c.problem}\`, got: ${v.problems[0]}`);
    }
    assert.equal(v.mustInstall, c.mustInstall);
  });
}

test('verdict is comparing the BARE version, so a `v`-prefixed input cannot pass', () => {
  // The off-by-`v` trap: release.yml must pass `needs.gate.outputs.app_version`
  // (bare), never `app_tag`. If it ever passed the tag, the stable branch's
  // equality check fails loudly here rather than being papered over by a
  // substring match.
  const v = verdict({ version: `v${RELEASED}`, caskSource: cask(SAME), isLatest: 'true' });
  assert.equal(v.mustInstall, false, 'a `v`-prefixed string is not a stable app version');
  assert.deepEqual(v.problems, [], 'and it does not collide with the cask version either');
});

test('an unparseable cask propagates out of verdict on both branches', () => {
  assert.throws(() => verdict({ version: RELEASED, caskSource: 'garbage', isLatest: 'true' }), /could not determine the version/);
  assert.throws(() => verdict({ version: RC, caskSource: 'garbage', isLatest: 'false' }), /could not determine the version/);
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

test('isLatest is read strictly: only the exact string "true" owns the cask', () => {
  // The driver rejects anything that is not "true"/"false" before reaching here,
  // but verdict must not treat a truthy-looking value as the newest line either
  // — that would restore the equality assertion for a backport and demand a
  // cask it deliberately never wrote.
  const backport = { version: RELEASED, caskSource: cask(NEWER) };
  assert.equal(verdict({ ...backport, isLatest: true }).problems.length, 1, 'boolean true is the newest line');
  assert.deepEqual(verdict({ ...backport, isLatest: 'false' }).problems, []);
  assert.deepEqual(verdict({ ...backport, isLatest: '' }).problems, [], 'not "true" means not the newest line');
});
