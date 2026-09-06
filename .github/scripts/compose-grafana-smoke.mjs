// compose-grafana-smoke.mjs — run the compose-stack Grafana catch-net
// Playwright spec against an already-running `docker compose up --wait`
// stack, extracted from `just compose-grafana-smoke`'s former inline bash
// (CLAUDE.md invariant 15, issue #3098, epic #3091).
//
// BUG FIXED BY THIS EXTRACTION. The replaced recipe body installed the
// Playwright suite's npm deps with:
//
//   ( [ -f package-lock.json ] && npm ci || npm install --no-audit --no-fund )
//
// A bash `&&`/`||` chain treats ANY non-zero `npm ci` exit as "the left side
// failed", not just the ENOENT case the idiom was written for — so a
// corrupted lockfile, an out-of-sync lockfile, or a real dependency-integrity
// error all fell through to `npm install`, which silently re-resolves the
// lockfile and (almost always) exits 0. The recipe reported success on top of
// a genuine install failure. `decideInstallCommand` / `installDeps` below
// replace the whole idiom with an explicit branch on lockfile PRESENCE only:
// `npm ci` failing is now a hard, fatal error with no fallback, full stop;
// `npm install --no-audit --no-fund` runs on exactly one branch — lockfile
// absent — never as a rescue from a failed `npm ci`.
//
// Env contract:
//   PLAYWRIGHT_DIR      the Playwright suite's directory, relative to the
//                       repo root or absolute (default test/e2e/playwright)
//   PLAYWRIGHT_SPEC     spec file to run                (default compose_grafana_smoke.spec.ts)
//   GRAFANA_BASE_URL    forwarded to the spec            (default http://localhost:3000)
//   GRAFANA_URL         forwarded to the spec            (default http://localhost:3000)
//   CERBERUS_URL        forwarded to the spec            (default http://localhost:8080)
//
// Exit: 0 when the spec run passes; 1 on any install, browser-install, or
// spec failure (the failing subprocess's own exit status where non-zero).

import { spawnSync } from 'node:child_process';
import { statSync } from 'node:fs';
import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { error, log, notice } from './lib/gh.mjs';

// isRegularFile — true only when `path` exists and is a regular file, never a
// directory or other entry. Matches the replaced bash's `[ -f "$path" ]`
// exactly. Self-contained rather than shared (see chdb-install.mjs's own
// copy) — this is one predicate, not worth a lib/ module for two callers.
export function isRegularFile(path) {
  try {
    return statSync(path).isFile();
  } catch {
    return false;
  }
}

const PLAYWRIGHT_DIR = process.env.PLAYWRIGHT_DIR || 'test/e2e/playwright';
const PLAYWRIGHT_SPEC = process.env.PLAYWRIGHT_SPEC || 'compose_grafana_smoke.spec.ts';
const GRAFANA_BASE_URL = process.env.GRAFANA_BASE_URL || 'http://localhost:3000';
const GRAFANA_URL = process.env.GRAFANA_URL || 'http://localhost:3000';
const CERBERUS_URL = process.env.CERBERUS_URL || 'http://localhost:8080';

// decideInstallCommand — pure: which npm command a lockfile's presence calls
// for. Mirrors the replaced idiom's SUCCESS-path branching only — lockfile
// present -> `npm ci`, lockfile absent -> `npm install --no-audit --no-fund`
// — and nothing else, since the failure-path fallback that made the two
// branches indistinguishable on error is exactly what this extraction
// removes. Exported so the npm-ci-failure regression test can pin the branch
// choice without spawning npm.
export function decideInstallCommand(lockfileExists) {
  return lockfileExists
    ? { cmd: 'npm', args: ['ci'] }
    : { cmd: 'npm', args: ['install', '--no-audit', '--no-fund'] };
}

// runStep — spawn `cmd` with inherited stdio (npm/playwright both narrate
// their own progress) and report a structured result instead of exiting
// directly, so callers decide what "fatal" means for their own step.
function runStep(cmd, args, opts = {}) {
  const res = spawnSync(cmd, args, { stdio: 'inherit', ...opts });
  if (res.error) {
    return { ok: false, status: 1, spawnError: res.error };
  }
  return { ok: res.status === 0, status: res.status === null ? 1 : res.status };
}

// installDeps — install the Playwright suite's npm deps in `dir`, using
// `runFn` (defaults to runStep) so the regression test can stub the actual
// subprocess call. Returns the decided command plus the outcome; never
// attempts a second command regardless of the first one's result — that
// absence of a second call IS the bug fix, and the test below asserts it by
// counting invocations.
export function installDeps(dir, { lockfileExists, runFn = runStep } = {}) {
  const { cmd, args } = decideInstallCommand(lockfileExists);
  const result = runFn(cmd, args, { cwd: dir });
  return { cmd, args, lockfileExists, ...result };
}

function main() {
  log(`==> compose-grafana-smoke: installing deps in ${PLAYWRIGHT_DIR}`);
  const lockfileExists = isRegularFile(`${PLAYWRIGHT_DIR}/package-lock.json`);
  const install = installDeps(PLAYWRIGHT_DIR, { lockfileExists });
  if (!install.ok) {
    error(
      install.lockfileExists
        ? `\`npm ci\` failed (exit ${install.status}) against an existing package-lock.json in ${PLAYWRIGHT_DIR} — ` +
            'this is fatal, with no `npm install` fallback: a corrupted or out-of-sync lockfile is a real bug to fix, ' +
            'not to paper over (cerberus#3098).'
        : `\`npm install\` failed (exit ${install.status}) in ${PLAYWRIGHT_DIR}.`,
    );
    process.exit(install.status);
  }

  log('==> compose-grafana-smoke: installing Playwright browsers (chromium)');
  const browsers = runStep('npx', ['playwright', 'install', '--with-deps', 'chromium'], { cwd: PLAYWRIGHT_DIR });
  if (!browsers.ok) {
    error(`\`npx playwright install --with-deps chromium\` failed (exit ${browsers.status}) in ${PLAYWRIGHT_DIR}.`);
    process.exit(browsers.status);
  }

  log(`==> compose-grafana-smoke: running ${PLAYWRIGHT_SPEC} against ${CERBERUS_URL} / ${GRAFANA_URL}`);
  const spec = runStep('npx', ['playwright', 'test', PLAYWRIGHT_SPEC, '--reporter=list'], {
    cwd: PLAYWRIGHT_DIR,
    env: {
      ...process.env,
      GRAFANA_BASE_URL,
      GRAFANA_URL,
      CERBERUS_URL,
    },
  });
  if (!spec.ok) {
    error(`playwright spec ${PLAYWRIGHT_SPEC} failed (exit ${spec.status}).`);
    process.exit(spec.status);
  }

  notice(`compose-grafana-smoke: ${PLAYWRIGHT_SPEC} passed.`);
  process.exit(0);
}

if (process.argv[1] !== undefined && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main();
}
