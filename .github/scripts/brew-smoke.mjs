// brew-smoke.mjs — post-publish Homebrew install smoke for `release.yml`'s
// `brew-smoke` job.
//
// What it is for: goreleaser's `homebrew_casks:` block pushes a cask to a
// DIFFERENT repository (tsouza/homebrew-tap) with a PAT. Nothing in this repo
// observes that push. If the block is deleted, if HOMEBREW_TAP_GITHUB_TOKEN
// expires, or if the cross-repo write silently fails, the release still goes
// green here and `brew install tsouza/tap/cerberus` is discovered broken by a
// user.
//
// Ordering: it MUST run after the draft -> published flip. `brew install`
// downloads the release tarball, and a draft release's assets 404 for anyone
// but the token that created them. The fix for that is ordering
// (`needs: publish`), never a retry loop and never `continue-on-error` — a
// tolerated failure here is decoration, not a gate.
//
// NOTHING IS SKIPPED — the three releases shapes just get three assertions.
// .goreleaser.yml's `skip_upload` template keeps two of them out of the tap, and
// for those the honest assertion is the NEGATIVE one. Bailing out on a "not
// applicable" flag would be `t.Skip` in workflow clothing, and would wave
// through the very `skip_upload` regressions this job exists to catch:
//   - newest line + stable — the cask must declare EXACTLY this version;
//   - prerelease (`rc.*`)  — the cask must exist and must NOT declare it;
//   - not the highest stable tag (a maintenance backport) — the cask must
//     declare a STRICTLY NEWER version, proving this publish did not overwrite
//     the newest line's. v1.12.1, cut after v1.13.0, did exactly that.
//
// ANTI-VACUOUS: the cask's declared version is compared to the release
// version BEFORE `brew install` runs. That reads the tap's git state through
// the API, so a warm brew cache, a mirror, or a previously-installed cerberus
// cannot make a stale/never-pushed cask look healthy. The installed
// binary's `--version` is then compared with string EQUALITY against the
// de-`v`'d release version — `grep -q "$TAG"` can never match (the tag carries
// the `v`), and `grep -q "${TAG#v}"` passes on any superstring.
//
// THE DOCUMENTED COMMAND: the install below is the bare
// `brew install tsouza/tap/cerberus` that README.md and docs/operations.md give
// operators, not `brew install --cask …`. The tap can hold BOTH a cask and the
// Formula/cerberus.rb it served before .goreleaser.yml moved off `brews:` —
// goreleaser writes its own file and deletes nothing — and Homebrew resolves a
// bare tap ref to the FORMULA when both exist. Under `--cask` that divergence
// is invisible: the smoke installs the cask, every operator installs the stale
// formula, and the release goes green. The tap is also probed directly for a
// leftover formula, so the failure names its cause instead of surfacing as an
// unexplained version mismatch.
//
// CROSS-PLATFORM: cerberus is a Linux-first server binary, so "the cask
// installs" has to mean it installs on LINUX, not merely on whichever runner
// happens to execute this. That is covered from two directions. This script is
// run on both a macOS and a Linux runner (see the `brew-smoke` matrix in
// release.yml and brew-verify.yml), so `brew install` is exercised under
// Linuxbrew for real. And `caskPortabilityProblems` inspects the cask SOURCE on
// every leg, catching the failure a single-OS install can never see: a cask
// that dropped its linux_* artefacts, or grew a macOS-only artifact stanza,
// installs flawlessly on macOS while being broken everywhere else.
//
// Payload verbs (proving the SHIPPED bytes work, not just that a file landed):
//   - `cerberus migrate schema` — offline DDL render; must exit 0 and emit
//     CREATE statements.
//   - `cerberus config-docs -check` — the binary's config registry rendered and
//     compared against THIS commit's docs/configuration.md. A released binary
//     built from other source goes red here.
// `migrate gate` / `migrate verify` are deliberately NOT used: they carry
// dedicated non-zero exit codes for a legitimate no-go verdict, so a healthy
// binary would red the job.
//
// Env contract:
//   RELEASE_VERSION   the published app version, BARE (`1.11.2` /
//                     `1.11.2-rc.1`) — release.yml passes
//                     `needs.gate.outputs.app_version`, which is the de-`v`'d
//                     form the binary itself reports.
//   RELEASE_IS_LATEST "true"/"false" — whether this tag is the highest stable
//                     release, i.e. the line that owns the single shared tap
//                     cask. Selects the assertion branch, so it is required
//                     and has no default.
//   GITHUB_TOKEN      token used to read the tap's cask via the contents API.
//   GITHUB_API_URL    API base (default https://api.github.com).
//   REPO_ROOT         checkout of the release commit — the working directory for
//                     the `config-docs -check` payload run.
//
// Exit: 0 when every assertion for the branch taken holds; 1 on any missing or
// unrecognised env var, an unreadable/unparseable cask, a cask-version
// mismatch (newest + stable), match (prerelease) or not-newer-than-us
// (maintenance), a failed install, a binary resolved outside the brew prefix, a
// `--version` mismatch, or a failing payload verb.
//
// Imports only node: builtins. Run with `node .github/scripts/brew-smoke.mjs`.

import process from 'node:process';
import { spawnSync } from 'node:child_process';

// The tap goreleaser's `homebrew_casks:` block writes to, and the cask path
// inside it. Must stay in step with .goreleaser.yml's `directory: Casks`.
const TAP_REPO = 'tsouza/homebrew-tap';
const TAP_CASK_PATH = 'Casks/cerberus.rb';

// Every tap path from which Homebrew would load a FORMULA named `cerberus`,
// shadowing the cask of the same name. The tap served `Formula/cerberus.rb`
// before .goreleaser.yml moved from `brews:` to `homebrew_casks:`; goreleaser
// writes its own file and never deletes the other one, so a tap can end up
// holding BOTH — and Homebrew resolves a bare `tsouza/tap/cerberus` to the
// FORMULA when it does. Asserted absent, not cleaned up from here: the tap is
// a different repository.
//
// Pinning the one historical path would under-detect. Homebrew's
// `Tap#potential_formula_dirs` is `[<tap>/Formula, <tap>/HomebrewFormula,
// <tap>]` — the first that EXISTS as a directory becomes `formula_dir` — and
// `formula_files` globs it recursively (`**/*.rb`) for the two subdirectories,
// so a sharded `Formula/c/cerberus.rb` is loaded exactly like a flat one. Only
// the tap root is globbed non-recursively (`*.rb`).
//
// The matcher deliberately OVER-approximates that resolution: it flags a
// shadowing path in any of the three roots rather than replicating the
// first-directory-wins precedence. A `cerberus.rb` formula anywhere in this tap
// is wrong whichever directory currently wins, and the repair is the same file
// deletion either way, so precedence would only add a way to miss one.
const TAP_FORMULA_SHADOW_RE = /^(?:(?:Formula|HomebrewFormula)\/(?:.+\/)?cerberus\.rb|cerberus\.rb)$/;

// The path the tap actually holds today, named in the repair instructions so
// the failure tells an operator which file to delete rather than describing a
// pattern.
const TAP_FORMULA_PATH = 'Formula/cerberus.rb';

// How an operator names the cask: `brew install tsouza/tap/cerberus`.
const CASK_REF = 'tsouza/tap/cerberus';

// A STABLE release is exactly `<major>.<minor>.<patch>`. Anything with a
// prerelease suffix (`-rc.1`, `-RC1`) is a prerelease, which `skip_upload: auto`
// keeps out of the tap.
const STABLE_VERSION_RE = /^\d+\.\d+\.\d+$/;

// goreleaser's cask template declares `version "1.11.2"`.
const CASK_VERSION_RE = /^\s*version\s+"([^"]+)"/m;

// Fallback if the template stops emitting a bare `version` line: the archive
// name goreleaser builds, `cerberus_<version>_<os>_<arch>.tar.gz` (the
// `name_template` in .goreleaser.yml `archives:`).
const CASK_ARCHIVE_RE = /cerberus_([^_]+)_(?:linux|darwin)_(?:amd64|arm64)\.tar\.gz/;

// Every (os, arch) target .goreleaser.yml's `builds:` matrix produces. A cask
// that omits one silently strands that platform on `brew install`, and the
// omission is INVISIBLE to a smoke that only installs on its own runner: a
// darwin-only cask installs perfectly on macOS. cerberus is a Linux-first
// server binary, so the Linux pair is the one that must never quietly vanish.
const REQUIRED_CASK_TARGETS = ['darwin_amd64', 'darwin_arm64', 'linux_amd64', 'linux_arm64'];

// Homebrew's Linux cask installer, verbatim
// (`extend/os/linux/cask/installer.rb#check_stanza_os_requirements`):
//
//   return if !cask.depends_on.requires_macos? &&
//             artifacts.all? { |artifact| supported_artifact?(artifact) }
//   raise ::Cask::CaskError, "#{cask}: cask requires macOS."
//
//   def supported_artifact?(artifact)
//     return !artifact.manual_install if artifact.is_a?(::Cask::Artifact::Installer)
//     ::Cask::Artifact::MACOS_ONLY_ARTIFACTS.exclude?(artifact.class)
//   end
//
// So the gate is a CONJUNCTION, and the three checks below are one per clause.
//
// Clause 1 — the artifact classes in `MACOS_ONLY_ARTIFACTS` (cask/artifact.rb).
// Mirrored exactly: sixteen entries, and `Installer` is deliberately NOT one of
// them (it gets its own clause below). Adding a stanza here that Homebrew does
// not treat as macOS-only would red a release Homebrew is happy to install.
export const MACOS_ONLY_ARTIFACT_STANZAS = [
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
];

// An artifact stanza's first argument is always a quoted string — `app "X.app"`,
// `pkg "x.pkg"`, `binary "cerberus"` — so the match requires one, optionally
// through a parenthesised call. That is what separates a stanza from the two
// shapes that would otherwise fake a hit, since every name above is also an
// ordinary English word or a plausible identifier:
//   * prose in a `caveats` heredoc — `service management is manual` —
//     and comment / desc text, none of which is followed by a quote;
//   * Ruby inside a `preflight`/`postflight` block — `app = "..."`, where the
//     next token is `=`.
// It also catches `app("X.app")`, which a whitespace-only match misses.
const stanzaRe = (stanza) => new RegExp(String.raw`^\s*${stanza}\s*\(?\s*["']`, 'm');

// Clause 2 — `Installer`, special-cased by `supported_artifact?`: macOS-only
// exactly when it is a MANUAL install (`installer manual: "…"`). The scripted
// form (`installer script: {…}`) runs fine under Linuxbrew, so matching a bare
// `installer` would be a false positive.
const INSTALLER_MANUAL_RE = /^\s*installer\s+manual:/m;

// Clause 3 — `depends_on macos:`. Contrary to the folklore that it is inert off
// a Mac, `!cask.depends_on.requires_macos?` is the FIRST conjunct of the gate
// above. `requires_macos?` is `@macos_bare_set_top_level || @macos_version_set_top_level
// || @maximum_macos_set_top_level` (cask/dsl/depends_on.rb) — set only by a
// TOP-LEVEL `depends_on macos:`, never by one recorded inside an `on_*` block,
// which is why the implicit `MacOSRequirement` Homebrew attaches to every cask
// leaves it false and ordinary casks still install. goreleaser emits no
// `depends_on` at all; if it ever starts, the cask stops installing on Linux.
const DEPENDS_ON_MACOS_RE = /^\s*depends_on\s+macos:/m;

// The exact string Homebrew raises when the gate rejects a cask, quoted in the
// failure message so an operator can match it against a real `brew install` log.
const LINUX_REJECTION_MESSAGE = 'cask requires macOS.';

// The install command's timeout. A brew install that hangs must fail the job
// rather than burn the whole runner allowance.
const INSTALL_TIMEOUT_MS = 15 * 60 * 1000;

// ---------------------------------------------------------------------------
// pure core (exported for brew-smoke.test.mjs — no network, no process.exit)
// ---------------------------------------------------------------------------

// isStableRelease — true iff the bare version is a stable `X.Y.Z`. This is the
// SAME predicate goreleaser's `skip_upload: auto` applies, so the two branches
// below mirror what the tap actually receives.
export function isStableRelease(version) {
  return STABLE_VERSION_RE.test(String(version ?? '').trim());
}

// caskVersion — the version the tap cask declares. Tries the explicit
// `version "..."` line, falls back to the archive filename, and THROWS when
// neither parses. An unparseable cask is a failure, never a pass: silently
// returning null would let the stable branch's equality check degrade into
// "well, we couldn't tell".
export function caskVersion(rbSource) {
  const src = String(rbSource ?? '');
  const declared = CASK_VERSION_RE.exec(src);
  if (declared) return declared[1];
  const archive = CASK_ARCHIVE_RE.exec(src);
  if (archive) return archive[1];
  throw new Error(
    `could not determine the version of ${TAP_REPO}:${TAP_CASK_PATH} — neither a \`version "…"\` ` +
      `declaration nor a cerberus_<version>_<os>_<arch>.tar.gz archive name is present. ` +
      `An unreadable cask is a release failure, not a pass.`,
  );
}

// caskPortabilityProblems — the CROSS-PLATFORM shape assertions, evaluated on
// whatever cask the tap currently holds. Deliberately independent of which
// release is being smoked and of which runner is running: `brew install` on the
// smoke's own OS proves only that OS, and cerberus's primary platform (Linux)
// is the one no macOS runner can vouch for. These two checks encode the exact
// mechanism Homebrew uses, so a goreleaser template change that darwin-only's
// the cask goes red on the release that ships it rather than on the first
// Linux operator to hit it.
export function caskPortabilityProblems(rbSource) {
  const src = String(rbSource ?? '');
  const problems = [];

  const missing = REQUIRED_CASK_TARGETS.filter((t) => !src.includes(`_${t}.tar.gz`));
  if (missing.length > 0) {
    problems.push(
      `${TAP_REPO}:${TAP_CASK_PATH} declares no artefact for ${missing.join(', ')}. ` +
        `.goreleaser.yml's \`builds:\` matrix produces all of ${REQUIRED_CASK_TARGETS.join(', ')}, so a ` +
        `cask missing any of them means the homebrew_casks template stopped emitting that platform. ` +
        `\`brew install ${CASK_REF}\` is broken for it — and a macOS-only smoke cannot see that.`,
    );
  }

  // One entry per clause of `check_stanza_os_requirements` (see the constants
  // above): a macOS-only artifact class, a MANUAL installer, or a top-level
  // `depends_on macos:`. Any one of them makes the Linux installer raise.
  const declared = MACOS_ONLY_ARTIFACT_STANZAS.filter((stanza) => stanzaRe(stanza).test(src));
  if (INSTALLER_MANUAL_RE.test(src)) declared.push('installer manual:');
  if (DEPENDS_ON_MACOS_RE.test(src)) declared.push('depends_on macos:');

  if (declared.length > 0) {
    problems.push(
      `${TAP_REPO}:${TAP_CASK_PATH} declares ${declared.join(', ')}, which Homebrew's Linux cask ` +
        `installer refuses: \`check_stanza_os_requirements\` proceeds only for a cask with no top-level ` +
        `\`depends_on macos:\` AND no macOS-only artifact, and otherwise raises ` +
        `"<cask>: ${LINUX_REJECTION_MESSAGE}". This cask therefore no longer installs under Linuxbrew ` +
        `at all. cerberus ships a plain \`binary\` artifact and no \`depends_on\` precisely to stay ` +
        `installable on both.`,
    );
  }

  return problems;
}

// tapShadowingFormulaPaths — every path in the tap from which Homebrew would
// load a FORMULA named `cerberus`. Pure, so the resolution rule is unit-tested
// against the shapes a tap can actually take rather than trusted.
export function tapShadowingFormulaPaths(paths) {
  return (paths ?? []).filter((p) => TAP_FORMULA_SHADOW_RE.test(String(p)));
}

// formulaShadowProblems — the tap must serve the cask under the name operators
// actually type. Homebrew resolves a bare `<tap>/<name>` to a FORMULA when the
// tap holds both a formula and a cask of that name, warning "Treating … as a
// formula" and installing it. So a leftover Formula/cerberus.rb does not merely
// clutter the tap: it takes over `brew install tsouza/tap/cerberus` entirely,
// pinning every operator to whatever stale version that file declares while the
// freshly-pushed cask sits beside it, unused.
//
// This cannot be fixed from cerberus's release run — goreleaser writes
// Casks/cerberus.rb and has no way to delete a path it does not own — so it is
// asserted here and repaired in the tap repository.
export function formulaShadowProblems(shadowPaths) {
  const found = tapShadowingFormulaPaths(shadowPaths);
  if (found.length === 0) return [];
  return [
    `${TAP_REPO} still serves ${found.join(', ')} alongside ${TAP_CASK_PATH}. Homebrew resolves the ` +
      `bare \`brew install ${CASK_REF}\` — the command README.md and docs/operations.md give operators — ` +
      `to the FORMULA when a tap holds both, so every install would get the formula's version and ignore ` +
      `the cask this release just pushed. Delete ${TAP_FORMULA_PATH} from ${TAP_REPO} and add a ` +
      `tap_migrations.json mapping {"cerberus": "${TAP_REPO.split('/')[0]}/tap"} so existing formula ` +
      `installs are told how to move across.`,
  ];
}

// installedArtifactProblems — after the bare `brew install`, WHICH of the two
// things the tap can serve did Homebrew actually pick?
//
// The install command is deliberately the undisambiguated one operators are
// given, so the smoke has to state the resolution rather than assume it. Every
// other post-install assertion is blind to this: `command -v cerberus` and
// `--version` are identical whether a cask or a formula put the binary there,
// and a formula shadowing the cask is caught only indirectly, by the version it
// happens to declare being the wrong one. That backstop evaporates the moment a
// shadowing formula declares the SAME version, and it never applies on the two
// branches that publish without installing. This is the direct statement:
// cerberus must be installed as a CASK, and must not be installed as a formula.
//
// Inputs are the stdout of `brew list --cask --versions` / `brew list --formula
// --versions`, whose lines are `<name> <version…>`; the leading token is the
// name. Those commands list what IS installed and exit 0 either way, so the
// assertion never rests on an error code meaning what we hope it means.
export function installedArtifactProblems(caskList, formulaList) {
  const names = (out) =>
    new Set(
      String(out ?? '')
        .split('\n')
        .map((line) => line.trim().split(/\s+/)[0])
        .filter(Boolean),
    );

  const problems = [];
  if (!names(caskList).has('cerberus')) {
    problems.push(
      `\`brew install ${CASK_REF}\` did not install a CASK named cerberus — \`brew list --cask ` +
        `--versions\` does not list it. The documented bare ref resolved to something else, so the cask ` +
        `this release pushed is not what an operator running that command receives.`,
    );
  }
  if (names(formulaList).has('cerberus')) {
    problems.push(
      `\`brew install ${CASK_REF}\` installed a FORMULA named cerberus (\`brew list --formula ` +
        `--versions\` lists it). ${TAP_REPO} is serving a formula that shadows the cask; the bare ref ` +
        `Homebrew gives operators resolves to it, not to ${TAP_CASK_PATH}.`,
    );
  }
  return problems;
}

// compareVersions — semver-enough ordering for the two shapes goreleaser ever
// puts in play: `X.Y.Z` and `X.Y.Z-<prerelease>`. Numeric triple first, then a
// version WITH a prerelease suffix sorts below the same triple without one.
// Returns <0, 0, >0. A non-numeric component sorts as 0 rather than NaN so a
// malformed input can never make an ordering assertion vacuously true.
export function compareVersions(a, b) {
  const parts = (s) =>
    String(s ?? '')
      .trim()
      .split('-', 1)[0]
      .split('.')
      .map((n) => (/^\d+$/.test(n) ? Number(n) : 0));
  const pre = (s) => String(s ?? '').trim().includes('-');

  const [pa, pb] = [parts(a), parts(b)];
  for (let i = 0; i < Math.max(pa.length, pb.length); i++) {
    const d = (pa[i] ?? 0) - (pb[i] ?? 0);
    if (d !== 0) return d < 0 ? -1 : 1;
  }
  if (pre(a) === pre(b)) return 0;
  return pre(a) ? -1 : 1;
}

// verdict — the pure decision every branch shares. Takes `isLatest`: whether the
// release being smoked is the highest stable tag in the repo, i.e. whether it is
// the line that OWNS the single shared tap cask. Returns:
//   mustInstall — true only when the tap is expected to declare exactly this
//                 version (newest line + stable), so `brew install` must work.
//                 False on the two branches where goreleaser deliberately wrote
//                 no cask — each of which still gets its own assertion below,
//                 never a bail-out.
//   problems    — blocking problem strings; empty == the tap is in the state
//                 this release requires.
export function verdict({ version, caskSource, isLatest }) {
  const v = String(version ?? '').trim();
  const declared = caskVersion(caskSource);
  const stable = isStableRelease(v);
  const newest = isLatest === true || String(isLatest).trim() === 'true';

  // Portability is asserted on EVERY branch, not just the one that installs.
  // Whatever cask the tap holds right now is the one every `brew install` gets;
  // if it cannot serve Linux, that is broken today regardless of whether this
  // particular release was the one allowed to write it.
  const problems = caskPortabilityProblems(caskSource);

  // Order matters: a PRERELEASE is never the highest stable tag, so it arrives
  // here with newest === false. Testing it first keeps the forward `v1.14.0-rc.1`
  // case (tap legitimately behind at v1.13.1) out of the maintenance branch's
  // "the tap must be ahead of us" assertion, which only holds for backports.
  if (!stable) {
    if (declared === v) {
      problems.push(
        `${TAP_REPO}:${TAP_CASK_PATH} declares prerelease version "${v}". ` +
          `.goreleaser.yml sets \`skip_upload: auto\`, so a prerelease must write NO cask — ` +
          `the stable tap is now pointing operators at a prerelease.`,
      );
    }
  } else if (!newest) {
    // MAINTENANCE branch. `skip_upload` resolves to "true" off RELEASE_IS_LATEST,
    // so this publish must have left the tap alone — and the tap must still be
    // ahead of it. `declared === v` is the exact regression that shipped v1.12.1
    // over a v1.13.0 cask and downgraded every `brew install`.
    if (compareVersions(declared, v) <= 0) {
      problems.push(
        `${TAP_REPO}:${TAP_CASK_PATH} declares version "${declared}", which is not newer than this ` +
          `maintenance release "${v}". This release is not the highest stable tag, so .goreleaser.yml's ` +
          `\`skip_upload\` template must have kept it out of the tap and left the newest line's cask ` +
          `in place. \`brew install ${CASK_REF}\` now installs an older cerberus than the newest ` +
          `released line.`,
      );
    }
  } else if (declared !== v) {
    problems.push(
      `${TAP_REPO}:${TAP_CASK_PATH} declares version "${declared}" but this release is "${v}". ` +
        `The cask was never pushed, or the push landed stale — goreleaser's homebrew_casks block, ` +
        `HOMEBREW_TAP_GITHUB_TOKEN, or the cross-repo write is broken. ` +
        `\`brew install ${CASK_REF}\` would install the wrong version.`,
    );
  }

  return { mustInstall: newest && stable, problems };
}

// ---------------------------------------------------------------------------
// driver
// ---------------------------------------------------------------------------

function ghNotice(msg) {
  process.stdout.write(`::notice::${String(msg).replace(/\r?\n/g, '%0A')}\n`);
}

function ghError(msg) {
  process.stdout.write(`::error::${String(msg).replace(/\r?\n/g, '%0A')}\n`);
}

function fail(msg) {
  ghError(`brew-smoke: ${msg}`);
  process.exit(1);
}

// sh — run a command, capture stdout/stderr/status. No shell unless `shell` is
// set (only the PATH resolution probe needs one).
function sh(cmd, args, { cwd, shell = false, timeout } = {}) {
  const res = spawnSync(cmd, args, {
    cwd,
    shell,
    encoding: 'utf8',
    timeout,
    maxBuffer: 64 * 1024 * 1024,
  });
  return {
    status: res.status,
    stdout: res.stdout ?? '',
    stderr: res.stderr ?? '',
    error: res.error,
  };
}

function describe(res) {
  return `exit=${res.status}${res.error ? ` error=${res.error.message}` : ''}\n${res.stdout}\n${res.stderr}`.trim();
}

async function main() {
  const version = (process.env.RELEASE_VERSION ?? '').trim();
  const isLatestRaw = (process.env.RELEASE_IS_LATEST ?? '').trim();
  const token = process.env.GITHUB_TOKEN;
  const apiBase = process.env.GITHUB_API_URL || 'https://api.github.com';
  const repoRoot = process.env.REPO_ROOT;

  if (!version) fail('RELEASE_VERSION is required (the BARE published version, e.g. 1.11.2)');
  // Required, and required to be one of exactly two words. Defaulting a missing
  // value either way would silently pick an assertion branch: absent-means-false
  // would stop asserting the tap was written on every mainline release, and
  // absent-means-true would demand a cask a backport never wrote.
  if (isLatestRaw !== 'true' && isLatestRaw !== 'false') {
    fail(
      `RELEASE_IS_LATEST must be exactly "true" or "false" (got "${isLatestRaw}") — it selects which ` +
        `state the tap is asserted to be in, so it has no safe default.`,
    );
  }
  if (!token) fail('GITHUB_TOKEN is required to read the tap cask');
  if (!repoRoot) fail('REPO_ROOT is required — the `config-docs -check` payload runs against this commit\'s docs');

  const tapHeaders = (accept) => ({
    Authorization: `Bearer ${token}`,
    Accept: accept,
    'X-GitHub-Api-Version': '2022-11-28',
  });

  // tapFile — read one path from the tap. Returns the source, or null on 404.
  // Any other status is an unknown tap state and fails the caller rather than
  // being read as "absent", which would turn a rate limit or an outage into a
  // silent pass on the assertions below.
  const tapFile = async (path) => {
    const url = `${apiBase}/repos/${TAP_REPO}/contents/${path}`;
    const res = await fetch(url, { headers: tapHeaders('application/vnd.github.raw+json') });
    if (res.status === 404) return null;
    if (!res.ok) {
      fail(`GET ${url} -> ${res.status} ${res.statusText}. ${TAP_REPO} is not readable.`);
    }
    return res.text();
  };

  // tapPaths — every blob path in the tap, in ONE request. A per-path probe
  // would have to enumerate Homebrew's formula roots up front and would still
  // miss a sharded `Formula/c/cerberus.rb`; the whole tree is a few entries.
  const tapPaths = async () => {
    const url = `${apiBase}/repos/${TAP_REPO}/git/trees/HEAD?recursive=1`;
    const res = await fetch(url, { headers: tapHeaders('application/vnd.github+json') });
    if (!res.ok) {
      fail(`GET ${url} -> ${res.status} ${res.statusText}. ${TAP_REPO} is not readable.`);
    }
    const body = await res.json();
    // A truncated tree is a PARTIAL answer, and the assertion it feeds is an
    // absence check — reading it as a complete listing would turn "we did not
    // look at the whole tap" into "the tap is clean".
    if (body.truncated) {
      fail(
        `GET ${url} returned a TRUNCATED tree. The formula-shadow check is an absence assertion, so a ` +
          `partial listing cannot answer it — it would report a clean tap because it never saw the rest.`,
      );
    }
    return (body.tree ?? []).filter((e) => e.type === 'blob').map((e) => e.path);
  };

  // --- read the tap's cask through the API ------------------------------------
  // Both branches need it, and a 404 is a hard failure on both: a tap that has
  // no cerberus cask at all is broken regardless of what we just published.
  const caskSource = await tapFile(TAP_CASK_PATH);
  if (caskSource === null) {
    fail(
      `${TAP_REPO} has no readable ${TAP_CASK_PATH}: ` +
        `\`brew install ${CASK_REF}\` is broken for every operator, whatever this release published.`,
    );
  }

  // A leftover formula does not just sit there — it WINS the bare install. This
  // is checked before the version assertions because a shadowed cask makes every
  // one of them describe a file no operator ever receives.
  const shadow = formulaShadowProblems(await tapPaths());

  let decision;
  try {
    decision = verdict({ version, caskSource, isLatest: isLatestRaw });
  } catch (e) {
    fail(e.message);
  }

  const problems = [...shadow, ...decision.problems];
  for (const p of problems) ghError(`brew-smoke: ${p}`);
  if (problems.length > 0) process.exit(1);

  if (!decision.mustInstall) {
    // NO-CASK branches — asserted above, not skipped. `verdict` has already
    // proved the tap is in the state this release requires: for a prerelease,
    // that it does NOT declare this version; for a maintenance backport, that it
    // still declares a strictly newer one. Installing here would smoke the
    // NEWEST line's binary and report it as this release's, which is worse than
    // not installing.
    ghNotice(
      `brew-smoke: ${version} did not write the tap (${isLatestRaw === 'true' ? 'prerelease' : 'not the highest stable tag'}); ` +
        `the tap correctly still declares "${caskVersion(caskSource)}".`,
    );
    process.exit(0);
  }

  // --- STABLE branch: install from the tap and smoke the published binary -----
  const prefixRes = sh('brew', ['--prefix']);
  if (prefixRes.status !== 0) {
    fail(
      `\`brew --prefix\` did not resolve — Homebrew is not available on this runner. ${describe(prefixRes)}`,
    );
  }
  const brewPrefix = prefixRes.stdout.trim();

  // The BARE ref, exactly as README.md / docs/operations.md / docs/migration.md
  // tell operators to type it. `--cask` was used here previously on the theory
  // that the disambiguator would make a same-named formula fail loudly; it does
  // the opposite. When a tap serves both, `--cask` silently resolves to the
  // cask while the bare name Homebrew gives operators resolves to the FORMULA,
  // so the smoke would install one artifact and every user the other. Smoking
  // the documented command is the only way that divergence can be seen.
  const install = sh('brew', ['install', CASK_REF], { timeout: INSTALL_TIMEOUT_MS });
  if (install.status !== 0) {
    fail(`\`brew install ${CASK_REF}\` failed. ${describe(install)}`);
  }

  // WHICH of the two things the tap can serve did the bare ref resolve to? The
  // install command is the ambiguous one on purpose, so the answer is asserted
  // rather than assumed — see installedArtifactProblems. Both `brew list`
  // invocations must themselves succeed: an errored listing is not evidence of
  // anything, and treating its empty stdout as "no formula installed" would
  // pass this check for the wrong reason.
  const listed = (kind) => {
    const res = sh('brew', ['list', `--${kind}`, '--versions']);
    if (res.status !== 0) {
      fail(`\`brew list --${kind} --versions\` failed, so what got installed cannot be established. ${describe(res)}`);
    }
    return res.stdout;
  };
  const wrongArtifact = installedArtifactProblems(listed('cask'), listed('formula'));
  if (wrongArtifact.length > 0) {
    for (const p of wrongArtifact) ghError(`brew-smoke: ${p}`);
    process.exit(1);
  }

  // The binary under test must be the one brew just installed — a cerberus
  // already on PATH (a build artifact, a leftover) would otherwise satisfy every
  // assertion below without the tap being involved at all.
  const which = sh('command -v cerberus', [], { shell: true });
  if (which.status !== 0) {
    fail(`cerberus is not on PATH after \`brew install ${CASK_REF}\`. ${describe(which)}`);
  }
  const resolved = which.stdout.trim();
  if (!resolved.startsWith(brewPrefix)) {
    fail(
      `cerberus resolved to ${resolved}, which is OUTSIDE the brew prefix ${brewPrefix} — ` +
        `the smoke would be testing some other binary, not the published cask.`,
    );
  }

  const versionRes = sh(resolved, ['--version']);
  if (versionRes.status !== 0) {
    fail(`\`${resolved} --version\` failed. ${describe(versionRes)}`);
  }
  const reported = versionRes.stdout.trim();
  if (reported !== version) {
    fail(
      `the installed binary reports version "${reported}", expected exactly "${version}". ` +
        `(The comparison is against the BARE app version, not the \`v\`-prefixed tag: comparing to the ` +
        `tag is a guaranteed off-by-\`v\` mismatch.) ${resolved}`,
    );
  }

  // Payload 1: the offline DDL render must produce real CREATE statements.
  const schema = sh(resolved, ['migrate', 'schema'], { cwd: repoRoot });
  if (schema.status !== 0) {
    fail(`\`cerberus migrate schema\` failed on the published binary. ${describe(schema)}`);
  }
  if (!schema.stdout.includes('CREATE')) {
    fail(
      `\`cerberus migrate schema\` produced no CREATE statement (${schema.stdout.length} bytes) — ` +
        `an empty render would satisfy a naive non-empty check while shipping nothing usable.`,
    );
  }

  // Payload 2: the shipped binary's config registry must match THIS commit's
  // docs/configuration.md. A binary built from other source goes red here.
  const docs = sh(resolved, ['config-docs', '-check'], { cwd: repoRoot });
  if (docs.status !== 0) {
    fail(
      `\`cerberus config-docs -check\` failed against ${repoRoot}/docs/configuration.md — ` +
        `the published binary's config registry does not match the source this release was cut from. ` +
        `${describe(docs)}`,
    );
  }

  ghNotice(
    `brew-smoke: ${CASK_REF} installed ${reported} at ${resolved} (tap cask declares ` +
      `"${caskVersion(caskSource)}"); \`migrate schema\` rendered ${schema.stdout.length} bytes of DDL and ` +
      `\`config-docs -check\` matches this commit's docs/configuration.md.`,
  );
  process.exit(0);
}

// Only dispatch when run as a script — importing the pure core for
// brew-smoke.test.mjs must not fire the network driver or exit the test runner.
const invokedDirectly = process.argv[1] && import.meta.url === new URL(`file://${process.argv[1]}`).href;

if (invokedDirectly) {
  main().catch((e) => {
    ghError(`brew-smoke: unexpected failure (${e.message})`);
    process.exit(1);
  });
}
