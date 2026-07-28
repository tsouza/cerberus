// brew-smoke.mjs — post-publish Homebrew install smoke for `release.yml`'s
// `brew-smoke` job.
//
// What it is for: goreleaser's `brews:` block pushes a formula to a DIFFERENT
// repository (tsouza/homebrew-tap) with a PAT. Nothing in this repo observes
// that push. If the block is deleted, if HOMEBREW_TAP_GITHUB_TOKEN expires, or
// if the cross-repo write silently fails, the release still goes green here and
// `brew install tsouza/tap/cerberus` is discovered broken by a user.
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
//   - newest line + stable — the formula must declare EXACTLY this version;
//   - prerelease (`rc.*`)  — the formula must exist and must NOT declare it;
//   - not the highest stable tag (a maintenance backport) — the formula must
//     declare a STRICTLY NEWER version, proving this publish did not overwrite
//     the newest line's. v1.12.1, cut after v1.13.0, did exactly that.
//
// ANTI-VACUOUS: the formula's declared version is compared to the release
// version BEFORE `brew install` runs. That reads the tap's git state through
// the API, so a warm brew cache, a mirror, or a previously-installed cerberus
// cannot make a stale/never-pushed formula look healthy. The installed
// binary's `--version` is then compared with string EQUALITY against the
// de-`v`'d release version — `grep -q "$TAG"` can never match (the tag carries
// the `v`), and `grep -q "${TAG#v}"` passes on any superstring.
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
//                     formula. Selects the assertion branch, so it is required
//                     and has no default.
//   GITHUB_TOKEN      token used to read the tap's formula via the contents API.
//   GITHUB_API_URL    API base (default https://api.github.com).
//   REPO_ROOT         checkout of the release commit — the working directory for
//                     the `config-docs -check` payload run.
//
// Exit: 0 when every assertion for the branch taken holds; 1 on any missing or
// unrecognised env var, an unreadable/unparseable formula, a formula-version
// mismatch (newest + stable), match (prerelease) or not-newer-than-us
// (maintenance), a failed install, a binary resolved outside the brew prefix, a
// `--version` mismatch, or a failing payload verb.
//
// Imports only node: builtins. Run with `node .github/scripts/brew-smoke.mjs`.

import process from 'node:process';
import { spawnSync } from 'node:child_process';

// The tap goreleaser's `brews:` block writes to, and the formula path inside it.
const TAP_REPO = 'tsouza/homebrew-tap';
const TAP_FORMULA_PATH = 'Formula/cerberus.rb';

// How an operator names the formula: `brew install tsouza/tap/cerberus`.
const FORMULA_REF = 'tsouza/tap/cerberus';

// A STABLE release is exactly `<major>.<minor>.<patch>`. Anything with a
// prerelease suffix (`-rc.1`, `-RC1`) is a prerelease, which `skip_upload: auto`
// keeps out of the tap.
const STABLE_VERSION_RE = /^\d+\.\d+\.\d+$/;

// goreleaser's formula template declares `version "1.11.2"`.
const FORMULA_VERSION_RE = /^\s*version\s+"([^"]+)"/m;

// Fallback when the template stops emitting a bare `version` line: the archive
// name goreleaser builds, `cerberus_<version>_<os>_<arch>.tar.gz` (the
// `name_template` in .goreleaser.yml `archives:`).
const FORMULA_ARCHIVE_RE = /cerberus_([^_]+)_(?:linux|darwin)_(?:amd64|arm64)\.tar\.gz/;

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

// formulaVersion — the version the tap formula declares. Tries the explicit
// `version "..."` line, falls back to the archive filename, and THROWS when
// neither parses. An unparseable formula is a failure, never a pass: silently
// returning null would let the stable branch's equality check degrade into
// "well, we couldn't tell".
export function formulaVersion(rbSource) {
  const src = String(rbSource ?? '');
  const declared = FORMULA_VERSION_RE.exec(src);
  if (declared) return declared[1];
  const archive = FORMULA_ARCHIVE_RE.exec(src);
  if (archive) return archive[1];
  throw new Error(
    `could not determine the version of ${TAP_REPO}:${TAP_FORMULA_PATH} — neither a \`version "…"\` ` +
      `declaration nor a cerberus_<version>_<os>_<arch>.tar.gz archive name is present. ` +
      `An unreadable formula is a release failure, not a pass.`,
  );
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
// the line that OWNS the single shared tap formula. Returns:
//   mustInstall — true only when the tap is expected to declare exactly this
//                 version (newest line + stable), so `brew install` must work.
//                 False on the two branches where goreleaser deliberately wrote
//                 no formula — each of which still gets its own assertion below,
//                 never a bail-out.
//   problems    — blocking problem strings; empty == the tap is in the state
//                 this release requires.
export function verdict({ version, formulaSource, isLatest }) {
  const v = String(version ?? '').trim();
  const declared = formulaVersion(formulaSource);
  const stable = isStableRelease(v);
  const newest = isLatest === true || String(isLatest).trim() === 'true';
  const problems = [];

  // Order matters: a PRERELEASE is never the highest stable tag, so it arrives
  // here with newest === false. Testing it first keeps the forward `v1.14.0-rc.1`
  // case (tap legitimately behind at v1.13.1) out of the maintenance branch's
  // "the tap must be ahead of us" assertion, which only holds for backports.
  if (!stable) {
    if (declared === v) {
      problems.push(
        `${TAP_REPO}:${TAP_FORMULA_PATH} declares prerelease version "${v}". ` +
          `.goreleaser.yml sets \`skip_upload: auto\`, so a prerelease must write NO formula — ` +
          `the stable tap is now pointing operators at a prerelease.`,
      );
    }
  } else if (!newest) {
    // MAINTENANCE branch. `skip_upload` resolves to "true" off RELEASE_IS_LATEST,
    // so this publish must have left the tap alone — and the tap must still be
    // ahead of it. `declared === v` is the exact regression that shipped v1.12.1
    // over a v1.13.0 formula and downgraded every `brew install`.
    if (compareVersions(declared, v) <= 0) {
      problems.push(
        `${TAP_REPO}:${TAP_FORMULA_PATH} declares version "${declared}", which is not newer than this ` +
          `maintenance release "${v}". This release is not the highest stable tag, so .goreleaser.yml's ` +
          `\`skip_upload\` template must have kept it out of the tap and left the newest line's formula ` +
          `in place. \`brew install ${FORMULA_REF}\` now installs an older cerberus than the newest ` +
          `released line.`,
      );
    }
  } else if (declared !== v) {
    problems.push(
      `${TAP_REPO}:${TAP_FORMULA_PATH} declares version "${declared}" but this release is "${v}". ` +
        `The formula was never pushed, or the push landed stale — goreleaser's brews block, ` +
        `HOMEBREW_TAP_GITHUB_TOKEN, or the cross-repo write is broken. ` +
        `\`brew install ${FORMULA_REF}\` would install the wrong version.`,
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
  // absent-means-true would demand a formula a backport never wrote.
  if (isLatestRaw !== 'true' && isLatestRaw !== 'false') {
    fail(
      `RELEASE_IS_LATEST must be exactly "true" or "false" (got "${isLatestRaw}") — it selects which ` +
        `state the tap is asserted to be in, so it has no safe default.`,
    );
  }
  if (!token) fail('GITHUB_TOKEN is required to read the tap formula');
  if (!repoRoot) fail('REPO_ROOT is required — the `config-docs -check` payload runs against this commit\'s docs');

  // --- read the tap's formula through the API --------------------------------
  // Both branches need it, and a 404 is a hard failure on both: a tap that has
  // no cerberus formula at all is broken regardless of what we just published.
  const url = `${apiBase}/repos/${TAP_REPO}/contents/${TAP_FORMULA_PATH}`;
  const res = await fetch(url, {
    headers: {
      Authorization: `Bearer ${token}`,
      Accept: 'application/vnd.github.raw+json',
      'X-GitHub-Api-Version': '2022-11-28',
    },
  });
  if (!res.ok) {
    fail(
      `GET ${url} -> ${res.status} ${res.statusText}. ${TAP_REPO} has no readable ${TAP_FORMULA_PATH}: ` +
        `\`brew install ${FORMULA_REF}\` is broken for every operator, whatever this release published.`,
    );
  }
  const formulaSource = await res.text();

  let decision;
  try {
    decision = verdict({ version, formulaSource, isLatest: isLatestRaw });
  } catch (e) {
    fail(e.message);
  }

  for (const p of decision.problems) ghError(`brew-smoke: ${p}`);
  if (decision.problems.length > 0) process.exit(1);

  if (!decision.mustInstall) {
    // NO-FORMULA branches — asserted above, not skipped. `verdict` has already
    // proved the tap is in the state this release requires: for a prerelease,
    // that it does NOT declare this version; for a maintenance backport, that it
    // still declares a strictly newer one. Installing here would smoke the
    // NEWEST line's binary and report it as this release's, which is worse than
    // not installing.
    ghNotice(
      `brew-smoke: ${version} did not write the tap (${isLatestRaw === 'true' ? 'prerelease' : 'not the highest stable tag'}); ` +
        `the tap correctly still declares "${formulaVersion(formulaSource)}".`,
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

  const install = sh('brew', ['install', '--formula', FORMULA_REF], { timeout: INSTALL_TIMEOUT_MS });
  if (install.status !== 0) {
    fail(`\`brew install --formula ${FORMULA_REF}\` failed. ${describe(install)}`);
  }

  // The binary under test must be the one brew just installed — a cerberus
  // already on PATH (a build artifact, a leftover) would otherwise satisfy every
  // assertion below without the tap being involved at all.
  const which = sh('command -v cerberus', [], { shell: true });
  if (which.status !== 0) {
    fail(`cerberus is not on PATH after \`brew install ${FORMULA_REF}\`. ${describe(which)}`);
  }
  const resolved = which.stdout.trim();
  if (!resolved.startsWith(brewPrefix)) {
    fail(
      `cerberus resolved to ${resolved}, which is OUTSIDE the brew prefix ${brewPrefix} — ` +
        `the smoke would be testing some other binary, not the published formula.`,
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
    `brew-smoke: ${FORMULA_REF} installed ${reported} at ${resolved} (tap formula declares ` +
      `"${formulaVersion(formulaSource)}"); \`migrate schema\` rendered ${schema.stdout.length} bytes of DDL and ` +
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
