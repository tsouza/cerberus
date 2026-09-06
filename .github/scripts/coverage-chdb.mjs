// coverage-chdb.mjs — the chdb-tagged coverage lane: writes cover-chdb.out.
// Requires libchdb.so (`just chdb-install`); skips itself gracefully (rather
// than failing) when the library is absent, so a bare local `just
// coverage-chdb` without chDB installed still leaves the default-tag lane
// usable on its own. CI's caller sets COVERAGE_REQUIRE_LANES=default+chdb,
// which turns that same skip into a hard failure — see the tail of main()
// below.
//
// Extracted from the `coverage-chdb` Justfile recipe body (CLAUDE.md
// invariant 15, tsouza/cerberus#3113). #3095/#3091 already extracted the
// sibling `coverage-merge` recipe into coverage-merge.mjs the same way, but
// THIS half stayed literal bash at the time because its exact shell text was
// (and, for the recipe wrapper that now just calls this script, still
// partly is) parsed as chdb-tagged test EXECUTION EVIDENCE by four
// scanners:
//   - test/regression/tagged_test_enrollment_test.go's readCoverageChdbTaggedRun
//     and taggedCoverageChdbLaneIsFailClosed now read THIS file's source for
//     the composite tag set, the go-test argv shape, and the
//     COVERAGE_REQUIRE_LANES fail-closed tail.
//   - test/regression/coverage_recipe_fail_closed_test.go counts this file's
//     `go test -timeout` invocation instead of the old recipe-body one.
//   - test/regression/rapid_seed_pin_test.go still finds
//     CERBERUS_RAPID_SEED={{COVERAGE_RAPID_SEED}} in the RECIPE body — the
//     Justfile call site passes it through as an env var, unchanged from
//     before this extraction — so that scanner needed no update.
//   - .github/scripts/perf-coverage-fanout.test.mjs reads mainSweepArgv()
//     structurally instead of slicing the old recipe body's "main sweep"
//     line as text.
// See tsouza/cerberus#3113 for the full account of what moved where and why.
//
// Env:
//   CHDB_INSTALL_PATH      (required) libchdb.so path. The Justfile recipe
//                          always passes CHDB_INSTALL_PATH from
//                          just/chdb.just; missing it here is a caller bug,
//                          not a legitimate "libchdb.so absent" skip.
//   CERBERUS_RAPID_SEED    (required, only once the libchdb.so branch below
//                          actually runs) forwarded verbatim to `go test`
//                          and to perf-coverage-fanout.mjs — see
//                          just/test.just's own COVERAGE_RAPID_SEED doc
//                          comment for why the pin has to be identical
//                          across both coverage lanes.
//   SKIP_RATCHET_FANOUT    (optional) `1` skips the local
//                          perf-coverage-fanout.mjs call (CI's coverage-chdb
//                          job sets this; its own coverage-chdb-ratchet
//                          matrix runs those shards instead, on their own
//                          runners).
//   COVERAGE_REQUIRE_LANES (optional) `default+chdb` hard-fails (exit 1)
//                          unless cover-chdb.out was actually produced.
//   GO                     go executable; default `go`. Test seam.
//
// Exit: 0 on a graceful skip or a successful run; 1 on a failed `go test`, a
// failed fan-out, or COVERAGE_REQUIRE_LANES=default+chdb with no
// cover-chdb.out produced.

import { existsSync } from 'node:fs';
import { spawn, spawnSync } from 'node:child_process';
import { join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import process from 'node:process';

import { capture, error, isNonEmptyFile, log } from './lib/gh.mjs';
import { RATCHET_FANOUT } from './perf-coverage-fanout.mjs';

/** The composite build-tag set every chdb-tagged package in this lane needs. */
export const CHDB_TAGS = 'chdb,agpl_oracle,chdb_agpl_oracle';

/**
 * Shard 1 of RATCHET_FANOUT: the main sweep below carries TestCardinalityRatchet's
 * first shard itself; perf-coverage-fanout.mjs runs the remaining
 * RATCHET_FANOUT-1 shards (see that module's header for the full story of
 * why the corpus is sharded at all). Importing RATCHET_FANOUT directly here
 * — rather than restating the shard count as a literal, the way the old
 * Justfile recipe had to (`just` recipes cannot import a JS constant) —
 * removes the drift risk perf-coverage-fanout.test.mjs used to have to pin
 * across the language boundary.
 */
const PERF_SHARD_INDEX = 1;

/** Per CLAUDE.md invariant 13: named rather than a bare `60`. */
const MAIN_SWEEP_TIMEOUT_MINUTES = 60;

const COVERAGE_PROFILE = 'cover-chdb.out';
const FANOUT_SCRIPT = new URL('./perf-coverage-fanout.mjs', import.meta.url);

/**
 * argv for the main sweep's `go test` invocation. Exported so
 * coverage-chdb.test.mjs (and test/regression/tagged_test_enrollment_test.go's
 * readCoverageChdbTaggedRun) can verify its shape directly, the same pattern
 * perf-coverage-fanout.mjs's own legCommands() already established, instead
 * of parsing this file's source as unstructured text.
 */
export function mainSweepArgv(coverpkg) {
  return [
    'test',
    '-timeout', `${MAIN_SWEEP_TIMEOUT_MINUTES}m`,
    '-tags', CHDB_TAGS,
    '-coverpkg', coverpkg,
    '-coverprofile', COVERAGE_PROFILE,
    './...',
  ];
}

/**
 * Rewrites the tail of go test's -coverpkg echo, the same one-line trim
 * `just coverage-default`'s own awk filter still applies directly in bash:
 * -coverpkg makes go test echo the FULL package list back on every "of
 * statements in ..." summary line, which is ~5 KiB repeated per package
 * result — this rewrites only that sentence's tail; everything else
 * (results, failures) passes through unchanged.
 */
export function filterCoverpkgLine(line) {
  return line.replace(/of statements in github\.com\/.*/, 'of statements');
}

/**
 * Streams `child`'s stdout to `sink` (default process.stdout), filtered
 * line-by-line through filterCoverpkgLine — mirrors the bash pipeline's
 * `go test ... | awk '{ ...; print; fflush() }'`. Only stdout is piped, the
 * same as the bash version: `cmd1 | cmd2` never touches cmd1's stderr, which
 * the caller leaves on `inherit` instead. A trailing partial line (no final
 * newline) still gets flushed once the stream ends. `sink` is a test seam —
 * coverage-chdb.test.mjs injects a collecting stream instead of monkey-
 * patching the real process.stdout, which would race other tests' own
 * output under the test runner's default concurrency.
 */
export function pipeFilteredStdout(child, sink = process.stdout) {
  let carry = '';
  child.stdout.on('data', (chunk) => {
    carry += chunk.toString();
    const lines = carry.split('\n');
    carry = lines.pop();
    for (const l of lines) sink.write(`${filterCoverpkgLine(l)}\n`);
  });
  child.stdout.on('end', () => {
    if (carry) sink.write(`${filterCoverpkgLine(carry)}\n`);
  });
}

/** Runs the main `go test` sweep, resolving to its exit code. */
function runMainSweep(argv, env, cwd, go, stdout) {
  return new Promise((resolve) => {
    const child = spawn(go, argv, {
      cwd,
      env: { ...process.env, ...env },
      stdio: ['ignore', 'pipe', 'inherit'],
    });
    pipeFilteredStdout(child, stdout);
    child.on('close', (code) => resolve(code ?? 1));
  });
}

/**
 * main — the recipe body. `cwd`/`env`/`go`/`stdout` are test seams (default
 * process.cwd() / process.env / env.GO ?? 'go' / process.stdout); the CLI
 * entry point below always uses the real ones. Returns the exit status the
 * CLI should use; never throws.
 */
export async function main({ cwd = process.cwd(), env = process.env, go = env.GO || 'go', stdout = process.stdout } = {}) {
  const p = (name) => join(cwd, name);

  const chdbPath = env.CHDB_INSTALL_PATH;
  if (!chdbPath) {
    error('CHDB_INSTALL_PATH is required — the coverage-chdb recipe always passes it from just/chdb.just');
    return 1;
  }

  if (!existsSync(chdbPath)) {
    log('==> libchdb.so not found, skipping chdb lane');
  } else {
    log('==> chdb-tagged coverage');

    const rapidSeed = env.CERBERUS_RAPID_SEED;
    if (!rapidSeed) {
      error('CERBERUS_RAPID_SEED is required — an unpinned rapid draw would move this lane\'s coverage run to run');
      return 1;
    }

    const listResult = capture(go, ['list', '-tags', CHDB_TAGS, './...'], { cwd, env: { ...process.env, ...env } });
    if (listResult.status !== 0) {
      error(`${go} list -tags ${CHDB_TAGS} ./...: ${listResult.stderr.trim()}`);
      return 1;
    }
    const coverpkg = listResult.stdout.split('\n').filter(Boolean).join(',');

    const testEnv = {
      ...env,
      CERBERUS_RAPID_SEED: rapidSeed,
      PERF_SHARD_INDEX: String(PERF_SHARD_INDEX),
      PERF_SHARD_COUNT: String(RATCHET_FANOUT),
    };
    const code = await runMainSweep(mainSweepArgv(coverpkg), testEnv, cwd, go, stdout);
    if (code !== 0) return code;

    if (env.SKIP_RATCHET_FANOUT === '1') {
      log('==> SKIP_RATCHET_FANOUT=1: shards 2..PERF_SHARD_COUNT run on their own coverage-chdb-ratchet CI matrix legs, not here');
    } else {
      const fanout = spawnSync(process.execPath, [fileURLToPath(FANOUT_SCRIPT)], {
        cwd,
        stdio: 'inherit',
        env: { ...process.env, ...env, CERBERUS_RAPID_SEED: rapidSeed, TAGS: CHDB_TAGS, COVERPKG: coverpkg },
      });
      const fanoutCode = fanout.status ?? 1;
      if (fanoutCode !== 0) return fanoutCode;
    }
  }

  // Unconditional (reached whether or not the libchdb.so branch above ran):
  // a bare local `just coverage-chdb` stays a graceful skip, but a caller
  // that opts in via COVERAGE_REQUIRE_LANES=default+chdb is asserting the
  // chdb lane MUST have produced real evidence — CI sets this so an install
  // that silently no-ops fails here instead of only surfacing three steps
  // later as a narrower merged profile.
  if (env.COVERAGE_REQUIRE_LANES === 'default+chdb' && !isNonEmptyFile(p(COVERAGE_PROFILE))) {
    error(`COVERAGE_REQUIRE_LANES=default+chdb but ${COVERAGE_PROFILE} was not produced (libchdb.so missing?)`);
    return 1;
  }
  return 0;
}

// Import-safe: coverage-chdb.test.mjs and perf-coverage-fanout.test.mjs both
// import mainSweepArgv()/CHDB_TAGS without triggering a real run.
const invokedDirectly = process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href;
if (invokedDirectly) process.exit(await main());
