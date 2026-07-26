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
// PRERELEASES ARE NOT SKIPPED. `skip_upload: auto` in .goreleaser.yml means an
// `rc.*` release writes NO formula, so the honest assertion for a prerelease is
// the NEGATIVE one: the formula must still exist AND must NOT declare this
// version. Bailing out on a "not applicable" flag would be `t.Skip` in workflow
// clothing, and would wave through a `skip_upload` regression that started
// publishing prerelease formulas to the stable tap.
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
//   GITHUB_TOKEN      token used to read the tap's formula via the contents API.
//   GITHUB_API_URL    API base (default https://api.github.com).
//   REPO_ROOT         checkout of the release commit — the working directory for
//                     the `config-docs -check` payload run.
//
// Exit: 0 when every assertion for the branch taken holds; 1 on any missing env
// var, an unreadable/unparseable formula, a formula-version mismatch (stable) or
// match (prerelease), a failed install, a binary resolved outside the brew
// prefix, a `--version` mismatch, or a failing payload verb.
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

// verdict — the pure decision both branches share. Returns:
//   mustInstall — true for a stable release (the formula is expected to declare
//                 this version and `brew install` must work), false for a
//                 prerelease (no formula is published for it).
//   problems    — blocking problem strings; empty == the tap is in the state
//                 this release requires.
export function verdict({ version, formulaSource }) {
  const v = String(version ?? '').trim();
  const declared = formulaVersion(formulaSource);
  const stable = isStableRelease(v);
  const problems = [];

  if (stable) {
    if (declared !== v) {
      problems.push(
        `${TAP_REPO}:${TAP_FORMULA_PATH} declares version "${declared}" but this release is "${v}". ` +
          `The formula was never pushed, or the push landed stale — goreleaser's brews block, ` +
          `HOMEBREW_TAP_GITHUB_TOKEN, or the cross-repo write is broken. ` +
          `\`brew install ${FORMULA_REF}\` would install the wrong version.`,
      );
    }
  } else if (declared === v) {
    problems.push(
      `${TAP_REPO}:${TAP_FORMULA_PATH} declares prerelease version "${v}". ` +
        `.goreleaser.yml sets \`skip_upload: auto\`, so a prerelease must write NO formula — ` +
        `the stable tap is now pointing operators at a prerelease.`,
    );
  }

  return { mustInstall: stable, problems };
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
  const token = process.env.GITHUB_TOKEN;
  const apiBase = process.env.GITHUB_API_URL || 'https://api.github.com';
  const repoRoot = process.env.REPO_ROOT;

  if (!version) fail('RELEASE_VERSION is required (the BARE published version, e.g. 1.11.2)');
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
    decision = verdict({ version, formulaSource });
  } catch (e) {
    fail(e.message);
  }

  for (const p of decision.problems) ghError(`brew-smoke: ${p}`);
  if (decision.problems.length > 0) process.exit(1);

  if (!decision.mustInstall) {
    // PRERELEASE branch — asserted, not skipped. The formula exists and does NOT
    // declare this version, which is exactly what `skip_upload: auto` promises.
    ghNotice(
      `brew-smoke: prerelease ${version}: the tap correctly still declares ` +
        `"${formulaVersion(formulaSource)}"; no formula is published for prereleases (skip_upload: auto).`,
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
