// brew-upgrade-path.mjs — proves an EXISTING formula install is carried across
// to the cask, for `brew-verify.yml`'s `brew-migration` job.
//
// What it covers that nothing else can
// ------------------------------------
// `brew-smoke.mjs` installs from an empty runner, which is the only install
// shape CI ever performs — and a machine with no cerberus on it cannot observe
// the upgrade path at all. That blindness is not theoretical: cerberus was
// served as a formula through 1.13.1 and as a cask from 1.13.2, and the move
// shipped with no `tap_migrations.json`. Every fresh install was correct
// throughout. Every machine that already had the formula was left running the
// old binary, because:
//
//   - `brew upgrade` files a deleted formula under "Deleted Installed Formulae"
//     and moves on — a formula that no longer exists has no newer version to
//     move to;
//   - `brew install --cask …` afterwards puts the new cask in the Caskroom but
//     declines to link it, since the formula's keg still owns
//     `<prefix>/bin/cerberus`;
//   - so `cerberus --version` keeps reporting the OLD version, and no step in
//     that sequence prints an error.
//
// `brew-smoke.mjs` now asserts the tap carries the migration map, which catches
// the file being absent. This job asserts what that file is FOR: it reconstructs
// a real pre-migration machine and runs the upgrade a user would run. A map that
// exists but does not fire — wrong target shape, a Homebrew behaviour change, a
// cask that cannot link over the keg — is green under a static check and red
// here.
//
// How the pre-migration machine is reconstructed
// ----------------------------------------------
// The tap clone is rewound to the commit BEFORE the formula was deleted, the
// formula is installed from it exactly as an operator would have, and the clone
// is then allowed to catch up via `brew update`. That is the real trigger:
// Homebrew's migration runs out of `brew update`'s report of what the update
// DELETED, so it fires only for a clone that observes the deletion. Rewinding is
// what puts this runner in that position; a runner that taps at HEAD is already
// past it and can never see the migration, which is precisely why the bug
// reached users.
//
// The rewind point is derived, not pinned: the newest commit touching the
// formula path is the one that deleted it, so its parent is the last revision
// that still served it. A pinned SHA would drift out of the tap's history
// unnoticed and leave this job rewinding to something that never had a formula —
// which would install the CASK in step one and then assert, vacuously, that a
// cask is installed.
//
// Env contract:
//   TAP_MIGRATION_LEGACY_REV  optional; overrides the derived rewind point. For
//                             reproducing a specific report, not for routine
//                             runs.
//
// Exit: 0 when the migration carried the machine across, 1 otherwise. There is
// no "could not tell" exit — every branch below either asserts or fails.
//
// node: builtins only.

import { existsSync, mkdirSync } from 'node:fs';

import { capture, error as ghError, git, log, notice } from './lib/gh.mjs';
import {
  CASK_REF,
  TAP_BREW_NAME,
  TAP_FORMULA_PATH,
  TAP_MIGRATION_NAME,
  brewListNames,
  caskVersion,
} from './brew-smoke.mjs';

// What Homebrew prints when it migrates a formula to a cask within the same
// tap (`Reporter#migrate_tap_migration`). Asserting on the announcement rather
// than only on the end state is what distinguishes "the migration ran" from "the
// runner happened to end up with a cask installed for some other reason".
const MIGRATION_ANNOUNCEMENT = 'has been migrated from a formula to a cask';

// `brew install` of a real release tarball, plus a full `brew update`. Generous,
// because the failure this bounds is a hang, not slowness.
const BREW_TIMEOUT_MS = 20 * 60 * 1000;

// ---------------------------------------------------------------------------
// pure core
// ---------------------------------------------------------------------------

// upgradeOutcomeProblems — did the update actually carry this machine across?
//
// Three independent things have to hold, and each is stated separately because
// they fail for different reasons and the repair differs:
//
//   1. Homebrew announced the migration. Silence here is the shipped bug — the
//      map missing, misspelled, or pointing at another tap.
//   2. The cask ended up installed. The announcement can be printed and the
//      install still fail (an untrusted tap, a download error); Homebrew
//      swallows that failure by design so a broken migration cannot abort an
//      update.
//   3. The binary on PATH reports the cask's version. This is the one the user
//      sees. It fails on its own when the cask installed but could not link over
//      the formula's keg — the exact shape the bug report carried, where the
//      Caskroom held 1.13.2 and `cerberus --version` said 1.13.1.
export function upgradeOutcomeProblems({ updateOutput, caskList, reportedVersion, expectedVersion }) {
  const problems = [];

  if (!String(updateOutput ?? '').includes(MIGRATION_ANNOUNCEMENT)) {
    problems.push(
      `\`brew update\` did not announce the formula -> cask migration ("${MIGRATION_ANNOUNCEMENT}"). ` +
        `Homebrew only migrates a deleted package it can find in the tap's tap_migrations.json, and a ` +
        `miss is silent — the update reports success and leaves the old formula linked. Check that ` +
        `tap_migrations.json maps "${TAP_MIGRATION_NAME}" to "${TAP_BREW_NAME}".`,
    );
  }

  if (!brewListNames(caskList).has(TAP_MIGRATION_NAME)) {
    problems.push(
      `the ${TAP_MIGRATION_NAME} cask is not installed after \`brew update\` (\`brew list --cask ` +
        `--versions\` does not list it). Homebrew rescues any exception raised while installing a ` +
        `migration target so a failed migration cannot abort the update, which means the install can ` +
        `fail without failing anything visible.`,
    );
  }

  if (reportedVersion !== expectedVersion) {
    problems.push(
      `the cerberus on PATH reports "${reportedVersion}", but the tap's cask declares ` +
        `"${expectedVersion}". The migration did not reach the user: this is what a cask that installed ` +
        `into the Caskroom but could not link over the formula's keg looks like — the new version is on ` +
        `disk, the old binary is still what runs, and nothing printed an error.`,
    );
  }

  return problems;
}

// ---------------------------------------------------------------------------
// driver
// ---------------------------------------------------------------------------

function fail(msg) {
  ghError(`brew-upgrade-path: ${msg}`);
  process.exit(1);
}

function describe(res) {
  return `exit=${res.status}\n${res.stdout}\n${res.stderr}`.trim();
}

// brew — run a brew subcommand, returning the capture. HOMEBREW_NO_AUTO_UPDATE
// keeps an incidental `brew install` from performing the very update whose
// output this job needs to read, which would consume the migration before the
// step that asserts on it ever ran.
function brew(args, { timeout = BREW_TIMEOUT_MS } = {}) {
  return capture('brew', args, {
    timeout,
    env: { ...process.env, HOMEBREW_NO_AUTO_UPDATE: '1', HOMEBREW_NO_ENV_HINTS: '1' },
  });
}

function brewOrFail(args, what, opts) {
  const res = brew(args, opts);
  if (res.status !== 0) fail(`${what} failed. ${describe(res)}`);
  return res;
}

function tapGit(tapRepo, args, what) {
  const res = git(['-C', tapRepo, ...args]);
  if (res.status !== 0) fail(`${what} failed. ${describe(res)}`);
  return res.stdout.trim();
}

function main() {
  if (capture('brew', ['--version']).status !== 0) {
    fail('Homebrew is not on PATH on this runner.');
  }

  // --- reconstruct a pre-migration machine ---------------------------------
  brewOrFail(['tap', TAP_BREW_NAME], `\`brew tap ${TAP_BREW_NAME}\``);
  const tapRepo = brewOrFail(['--repo', TAP_BREW_NAME], `\`brew --repo ${TAP_BREW_NAME}\``).stdout.trim();

  // A shallow tap clone has no history to rewind INTO, and `git rev-list` on it
  // answers from the truncated graph rather than erroring — so the depth is
  // removed before anything is derived from history.
  if (existsSync(`${tapRepo}/.git/shallow`)) {
    tapGit(tapRepo, ['fetch', '--unshallow'], 'unshallowing the tap clone');
  }
  tapGit(tapRepo, ['fetch', 'origin'], 'fetching the tap');

  const override = process.env.TAP_MIGRATION_LEGACY_REV?.trim();
  let legacyRev = override;
  if (!legacyRev) {
    const deletion = tapGit(
      tapRepo,
      ['rev-list', '-1', 'origin/HEAD', '--', TAP_FORMULA_PATH],
      `finding the commit that deleted ${TAP_FORMULA_PATH}`,
    );
    if (!deletion) {
      fail(
        `no commit in ${TAP_BREW_NAME} ever touched ${TAP_FORMULA_PATH}, so there is no pre-migration ` +
          `state to reconstruct. Either the tap's history was rewritten, or the formula lived at another ` +
          `path — this job cannot assert the upgrade path without one, and must not pass by default.`,
      );
    }
    legacyRev = `${deletion}^`;
  }

  // The rewind point must actually SERVE a formula. Without this, a history
  // rewrite or a path change would land the job on a revision that only has the
  // cask — step one would install the cask, and every assertion below would pass
  // while testing nothing.
  const hasFormula = git(['-C', tapRepo, 'cat-file', '-e', `${legacyRev}:${TAP_FORMULA_PATH}`]);
  if (hasFormula.status !== 0) {
    fail(
      `${legacyRev} does not contain ${TAP_FORMULA_PATH}, so rewinding to it would not reconstruct a ` +
        `formula install. ${describe(hasFormula)}`,
    );
  }

  // The version the machine must END on: what the tap's cask declares at HEAD.
  const caskSource = tapGit(
    tapRepo,
    ['show', `origin/HEAD:Casks/${TAP_MIGRATION_NAME}.rb`],
    'reading the tap cask at HEAD',
  );
  const expectedVersion = caskVersion(caskSource);
  if (!expectedVersion) {
    fail(`the tap's cask at origin/HEAD declares no version, so there is nothing to assert against.`);
  }

  log(`rewinding ${TAP_BREW_NAME} to ${legacyRev} (parent of the commit that deleted the formula)`);
  tapGit(tapRepo, ['reset', '--hard', legacyRev], `rewinding the tap to ${legacyRev}`);

  // The BARE ref, which at this revision resolves to the formula — Homebrew
  // prefers a formula over a same-named cask, which is how operators ended up
  // with formula installs in the first place.
  brewOrFail(['install', CASK_REF], `\`brew install ${CASK_REF}\` at ${legacyRev}`);

  const formulaList = brewOrFail(['list', '--formula', '--versions'], '`brew list --formula --versions`');
  if (!brewListNames(formulaList.stdout).has(TAP_MIGRATION_NAME)) {
    fail(
      `the setup install did not produce a FORMULA install (\`brew list --formula --versions\` does not ` +
        `list ${TAP_MIGRATION_NAME}). There is no pre-migration state to migrate, so everything below ` +
        `would assert against a machine that was already on the cask.`,
    );
  }
  // Homebrew performs the migration only when a Caskroom already exists;
  // otherwise it prints the same announcement followed by the commands to run by
  // hand. Both are correct behaviour, but only the first has an outcome to
  // assert, so the fixture is completed here the same way the tap rewind
  // completes it — this is the state of any machine that has ever installed a
  // cask, and Homebrew creates the directory on the first one. A runner image
  // that has never installed a cask would otherwise steer this job onto the
  // print-only branch and red it for a reason that is not a defect.
  const caskroom = `${brewOrFail(['--prefix'], '`brew --prefix`').stdout.trim()}/Caskroom`;
  mkdirSync(caskroom, { recursive: true });

  notice(`brew-upgrade-path: reconstructed a formula install at ${legacyRev}; expecting ${expectedVersion} after update.`);

  // --- run the upgrade an operator would run --------------------------------
  // `brew update` is the trigger: the migration is driven by the report of what
  // the update deleted, so it fires only for a clone that observes the deletion.
  // Its exit status is deliberately not asserted — an unrelated tap failing to
  // fetch reds it without saying anything about cerberus, and the assertions
  // below read the outcome directly.
  const update = capture('brew', ['update'], {
    timeout: BREW_TIMEOUT_MS,
    env: { ...process.env, HOMEBREW_NO_ENV_HINTS: '1' },
  });
  log(update.stdout);
  log(update.stderr);

  const caskList = brewOrFail(['list', '--cask', '--versions'], '`brew list --cask --versions`');

  // The migration unlinks the formula and links the cask, so what `cerberus`
  // resolves to now is the whole question. A failure to resolve is itself an
  // outcome — an unlinked formula with no linked cask leaves the command gone —
  // and is reported as an empty version rather than crashing the job.
  const which = capture('command -v cerberus', [], { shell: true });
  const resolved = which.status === 0 ? which.stdout.trim() : '';
  const versionRes = resolved ? capture(resolved, ['--version']) : { status: 1, stdout: '', stderr: '' };
  const reported = versionRes.status === 0 ? versionRes.stdout.trim() : '<not on PATH>';

  const problems = upgradeOutcomeProblems({
    updateOutput: `${update.stdout}\n${update.stderr}`,
    caskList: caskList.stdout,
    reportedVersion: reported,
    expectedVersion,
  });
  for (const p of problems) ghError(`brew-upgrade-path: ${p}`);
  if (problems.length > 0) process.exit(1);

  notice(
    `brew-upgrade-path: a ${legacyRev} formula install was migrated to the cask by \`brew update\`; ` +
      `${resolved} reports ${reported}.`,
  );
}

const invokedDirectly = process.argv[1] && import.meta.url === new URL(`file://${process.argv[1]}`).href;
if (invokedDirectly) main();
