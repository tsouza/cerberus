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

import {
  isStableRelease,
  caskVersion,
  verdict,
  compareVersions,
  caskPortabilityProblems,
  formulaShadowProblems,
  tapShadowingFormulaPaths,
  installedArtifactProblems,
  MACOS_ONLY_ARTIFACT_STANZAS,
} from './brew-smoke.mjs';

// Every (os, arch) pair .goreleaser.yml's `builds:` matrix produces, in the
// order goreleaser emits them.
const ALL_TARGETS = ['darwin_amd64', 'darwin_arm64', 'linux_amd64', 'linux_arm64'];

// A stand-in for what goreleaser writes into the tap. It carries the FULL
// four-target shape on purpose: a fixture that only modelled darwin would let
// the portability assertions pass vacuously, which is the exact hollowness
// those assertions exist to prevent.
function cask(version, { targets = ALL_TARGETS, artifact = '  binary "cerberus"' } = {}) {
  const urls = targets.map(
    (t) =>
      `  url "https://github.com/tsouza/cerberus/releases/download/v${version}/cerberus_${version}_${t}.tar.gz"`,
  );
  return [
    'cask "cerberus" do',
    `  version "${version}"`,
    '',
    ...urls,
    '  name "cerberus"',
    '  desc "Drop-in Prometheus / Loki / Tempo HTTP gateway for ClickHouse"',
    '  homepage "https://github.com/tsouza/cerberus"',
    '',
    artifact,
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

// --- cross-platform shape ---------------------------------------------------
// cerberus is a Linux-first server binary, but a cask that serves only darwin
// installs perfectly on the macOS runner. These cases pin the two ways the tap
// can be broken for Linux while looking green from a Mac.

test('a full four-target cask has no portability problems', () => {
  assert.deepEqual(caskPortabilityProblems(cask('1.13.0')), []);
});

test('a darwin-only cask is a portability failure', () => {
  const problems = caskPortabilityProblems(
    cask('1.13.0', { targets: ['darwin_amd64', 'darwin_arm64'] }),
  );
  assert.equal(problems.length, 1);
  assert.match(problems[0], /linux_amd64, linux_arm64/);
});

test('a single missing arch is named, not rounded off', () => {
  const problems = caskPortabilityProblems(
    cask('1.13.0', { targets: ['darwin_amd64', 'darwin_arm64', 'linux_amd64'] }),
  );
  assert.equal(problems.length, 1);
  // The "declares no artefact for …" clause names ONLY the absent target. (The
  // message also enumerates the full expected set further on, so the assertion
  // has to be anchored to that clause rather than to the message as a whole.)
  assert.match(problems[0], /declares no artefact for linux_arm64\./);
});

// The Linux gate is `check_stanza_os_requirements`:
//   return if !cask.depends_on.requires_macos? && artifacts.all? { supported_artifact?(_1) }
// One case per clause, plus the two shapes that must NOT trip it.

test('the artifact list mirrors Homebrew MACOS_ONLY_ARTIFACTS exactly', () => {
  // Transcribed from Library/Homebrew/cask/artifact.rb, DSL-stanza spelling of
  // each class in MACOS_ONLY_ARTIFACTS. Sixteen entries, and `Installer` is NOT
  // one of them — `supported_artifact?` handles it separately, on
  // `manual_install`. The list's comment claims to be an exact mirror; this is
  // what makes that claim checkable instead of decorative.
  assert.deepEqual(MACOS_ONLY_ARTIFACT_STANZAS, [
    'app',
    'audio_unit_plugin',
    'colorpicker',
    'dictionary',
    'input_method',
    'internet_plugin',
    'keyboard_layout',
    'mdimporter',
    'pkg',
    'prefpane',
    'qlplugin',
    'screen_saver',
    'service',
    'suite',
    'vst_plugin',
    'vst3_plugin',
  ]);
});

test('a macOS-only artifact stanza is a portability failure', () => {
  // MACOS_ONLY_ARTIFACTS members: the Linux installer raises for any of them,
  // so the cask stops installing under Linuxbrew even with every linux_* url
  // still present.
  for (const stanza of ['app "Cerberus.app"', 'pkg "cerberus.pkg"', 'suite "Cerberus"']) {
    const problems = caskPortabilityProblems(cask('1.13.0', { artifact: `  ${stanza}` }));
    assert.equal(problems.length, 1, stanza);
    assert.match(problems[0], /cask requires macOS/);
  }
});

test('a parenthesised artifact stanza is caught too', () => {
  // `app("Cerberus.app")` is the same stanza; a matcher keyed on whitespace
  // after the name reports the cask as portable when it is not.
  const problems = caskPortabilityProblems(cask('1.13.0', { artifact: '  app("Cerberus.app")' }));
  assert.equal(problems.length, 1);
  assert.match(problems[0], /app/);
});

test('installer is macOS-only only in its MANUAL form', () => {
  // `supported_artifact?` special-cases Installer: `return !artifact.manual_install`.
  // `Installer` is NOT in MACOS_ONLY_ARTIFACTS, so treating every `installer`
  // stanza as macOS-only would block a release Homebrew installs happily.
  const manual = caskPortabilityProblems(cask('1.13.0', { artifact: '  installer manual: "Cerberus.app"' }));
  assert.equal(manual.length, 1);
  assert.match(manual[0], /installer manual:/);

  assert.deepEqual(
    caskPortabilityProblems(cask('1.13.0', { artifact: '  installer script: { executable: "setup" }' })),
    [],
  );
});

test('a top-level depends_on macos: is a portability failure', () => {
  // The FIRST conjunct of the Linux gate, contrary to the folklore that
  // `depends_on macos` is inert off a Mac.
  const problems = caskPortabilityProblems(
    cask('1.13.0', { artifact: '  depends_on macos: ">= :catalina"\n  binary "cerberus"' }),
  );
  assert.equal(problems.length, 1);
  assert.match(problems[0], /depends_on macos:/);
});

test('the macOS-only scan does not fire on prose, comments or ruby identifiers', () => {
  // `service`, `app`, `suite` and friends are ordinary English words and
  // plausible Ruby locals. Matching them wherever they start a line would red
  // every healthy release: a stanza is a name followed by a QUOTED argument.
  const src = cask('1.13.0').replace(
    '  desc "Drop-in Prometheus / Loki / Tempo HTTP gateway for ClickHouse"',
    [
      '  desc "Drop-in gateway service for ClickHouse"',
      '  # the installer strips the quarantine xattr; app data is untouched',
      '  caveats <<~EOS',
      '    service management is manual; run `cerberus serve` yourself.',
      '    suite integration is out of scope.',
      '  EOS',
      '  postflight do',
      '    app = staged_path/"cerberus"',
      '    pkg = nil',
      '  end',
    ].join('\n'),
  );
  assert.deepEqual(caskPortabilityProblems(src), []);
});

// --- formula shadow ---------------------------------------------------------
// goreleaser writes Casks/cerberus.rb and never deletes the Formula/cerberus.rb
// the tap served before the `brews:` -> `homebrew_casks:` move. A tap holding
// both resolves the bare `brew install tsouza/tap/cerberus` to the FORMULA, so
// the cask this release pushes reaches nobody.

// The tap path the shadow lives at today.
const TAP_TODAY = ['Formula/cerberus.rb', 'README.md'];

test('a leftover tap formula is a blocking problem', () => {
  const problems = formulaShadowProblems(TAP_TODAY);
  assert.equal(problems.length, 1);
  assert.match(problems[0], /Formula\/cerberus\.rb/);
  assert.match(problems[0], /tap_migrations\.json/);
});

test('a tap serving only the cask is clean', () => {
  assert.deepEqual(formulaShadowProblems(['Casks/cerberus.rb', 'README.md']), []);
  assert.deepEqual(formulaShadowProblems([]), []);
});

test('every root Homebrew loads formulae from shadows the cask, not just Formula/', () => {
  // Tap#potential_formula_dirs is [Formula, HomebrewFormula, <tap root>], and
  // the two subdirectories are globbed RECURSIVELY (`**/*.rb`) so a sharded
  // path counts. Pinning only the historical `Formula/cerberus.rb` would report
  // a clean tap for any of the others while the bare ref still resolved to a
  // formula.
  for (const p of [
    'Formula/cerberus.rb',
    'Formula/c/cerberus.rb',
    'HomebrewFormula/cerberus.rb',
    'HomebrewFormula/c/cerberus.rb',
    'cerberus.rb',
  ]) {
    assert.deepEqual(tapShadowingFormulaPaths([p, 'README.md']), [p], p);
    assert.equal(formulaShadowProblems([p]).length, 1, p);
  }
});

test('the shadow scan does not fire on a cask, a doc, or another project', () => {
  assert.deepEqual(
    tapShadowingFormulaPaths([
      'Casks/cerberus.rb', // the file this release WRITES
      'Formula/other-tool.rb', // a different project in the same tap
      'Formula/cerberus/README.md', // not a .rb
      'docs/cerberus.rb.md',
      'tap_migrations.json',
    ]),
    [],
  );
});

// --- which artifact the bare ref resolved to --------------------------------
// `command -v` and `--version` cannot tell a cask install from a formula one.
// This is the assertion that can.

test('installing the cask and no formula is the healthy state', () => {
  assert.deepEqual(installedArtifactProblems('cerberus 1.14.0\n', ''), []);
});

test('a formula install is reported even when a cask is present too', () => {
  const problems = installedArtifactProblems('cerberus 1.14.0\n', 'cerberus 1.13.1\n');
  assert.equal(problems.length, 1);
  assert.match(problems[0], /installed a FORMULA/);
});

test('the bare ref resolving to a formula instead of the cask is TWO problems', () => {
  // The exact shape the tap is in today: the formula wins, no cask is
  // installed. Both halves have to be stated — "no cask" and "a formula".
  const problems = installedArtifactProblems('', 'cerberus 1.13.1\n');
  assert.equal(problems.length, 2);
  assert.match(problems[0], /did not install a CASK/);
  assert.match(problems[1], /installed a FORMULA/);
});

test('a name is the leading token, so a substring cannot fake either verdict', () => {
  // `cerberus-migrate` must not satisfy the cask check nor trip the formula one.
  const problems = installedArtifactProblems('cerberus-migrate 1.0.0\n', 'cerberus-tools 1.0.0\n');
  assert.equal(problems.length, 1);
  assert.match(problems[0], /did not install a CASK/);
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

test('verdict reports portability problems on every branch, not just the installing one', () => {
  // A tap that cannot serve Linux is broken TODAY, whatever this particular
  // release was allowed to write. Asserting it only on the stable-newest branch
  // would let a backport or a prerelease sail past a cask no Linux operator can
  // install.
  const darwinOnly = (v) => cask(v, { targets: ['darwin_amd64', 'darwin_arm64'] });

  for (const [label, args] of [
    ['stable newest', { version: RELEASED, caskSource: darwinOnly(RELEASED), isLatest: 'true' }],
    ['maintenance', { version: RELEASED, caskSource: darwinOnly(NEWER), isLatest: 'false' }],
    ['prerelease', { version: FORWARD_RC, caskSource: darwinOnly(NEWER), isLatest: 'false' }],
  ]) {
    const { problems } = verdict(args);
    assert.ok(
      problems.some((p) => /linux_amd64, linux_arm64/.test(p)),
      `${label} branch must surface the missing Linux artefacts`,
    );
  }
});

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
