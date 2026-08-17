// property-fanout.mjs — runs property.yml's `Run property tests (rapid
// N=500)` step as several concurrent go test PROCESSES instead of one, so
// TestPromQL_Property_NativeHistogram no longer has to share one test
// binary's serial budget with the rest of the package — and no longer runs
// its own -rapid.checks=500 sweep as one long serial chain either.
//
// Why this is a script rather than the `go test` line it replaces
// ---------------------------------------------------------------------------
// test/property is one Go package — one test binary — and every top-level
// Test function in it runs SERIALLY unless it calls t.Parallel(), and even
// then a parallel test only resumes once every non-parallel sibling in the
// SAME package has finished. #2165 (native-histogram merge coverage) widened
// TestPromQL_Property_NativeHistogram's generated query-shape space from 3 to
// 16 shapes, and its own runtime jumped with it: 52-56s before, 900-1190s
// since, at the nightly -rapid.checks=500 depth (measured range across
// several CI runs and a local repro; rapid draws a fresh random seed each
// run, so which of the 16 shapes get exercised — and how many of the
// expensive ones — varies run to run). That is legitimate correctness
// coverage, not a regression to undo, but it is ALSO, by itself, no longer
// reliably inside a 20-minute budget. Every OTHER property test in the
// package sharing its binary stacks on top of that unconditionally
// (TestPromQL_InstantWindowSweep_FromScratch,
// TestPromQL_RangeProperty_FromScratch, TestPromQL_Property_FromScratch,
// TestLogQL_Property, TestTraceQL_Property, and every deterministic shape
// roster, ~150-200s together), and TestScanResourceBound_
// PartitionPruneEXPLAIN (the one test in the package that DOES call
// t.Parallel()) sat blocked for the entire run waiting for that serial
// chain to finish before its own body ever ran. Together that pushed the
// combined binary past the 20-minute go test -timeout on every property run
// since #2165 landed.
//
// t.Parallel() inside ONE process is not a fix. chdb-go v1.12.0's
// chdb/session.go keeps a single package-level `globalSession` shared by
// every sql.Open("chdb", "") call in a process:
//
//   var globalSession *Session
//   func NewSession(paths ...string) (*Session, error) {
//       if globalSession != nil {
//           return globalSession, nil
//       }
//       ...
//   }
//
// so two tests racing concurrently inside the same binary silently alias one
// chDB session and corrupt each other's seeded rows. Confirmed locally:
// marking every heavy property test t.Parallel() turned real, independently-
// passing tests into cross-contaminated failures — "missing series in got",
// "extra series in got", "trace-id set bound read 9 rows, expected <= 2" —
// pure data bleed between concurrently-open "isolated" sessions, not real
// property drift.
//
// SEPARATE go test PROCESSES do not share that global — each gets its own
// address space and its own globalSession — so this fans out three ways at
// once, all safe for the same reason:
//
//   1. The histogram sweep (TestPromQL_Property_NativeHistogram) moves into
//      HISTOGRAM_FANOUT processes of its own, each running a disjoint slice
//      of -rapid.checks with its own random rapid seed. Splitting the SAME
//      test's iterations across processes changes nothing about coverage —
//      the requested depth still sums to the full total, just spread across
//      concurrent processes instead of one serial loop.
//   2. TestPromQL_NativeHistogramShapeRoster runs in its OWN dedicated
//      process, not sharing a budget with any rapid-checks share. It used
//      to travel with the first checks share, and that is what caused a
//      real local failure: rapid.Check derives ITS OWN internal deadline
//      from the test's `go test -timeout` (pgregory.net/rapid's
//      checkDeadline() reads t.Deadline()) and, when it estimates it is
//      running short, quietly stops early and still reports "[rapid] OK" —
//      a PASS with fewer than the requested checks, not a failure (see
//      assertNoSilentTruncation below). With the roster sharing a share's
//      budget, the property sweep alone used 696 of a 720s (12m) deadline,
//      leaving the roster only 24s — not enough — and the hard `go test`
//      alarm fired mid-roster. Isolating the roster removes that entirely.
//   3. Everything else in the package (the "rest" leg) runs in a fourth,
//      concurrent process via -skip.
//
// No product code changes and no test is skipped: every property test
// still runs exactly once, and TestPromQL_Property_NativeHistogram's checks
// still sum to the full requested depth — assertNoSilentTruncation is the
// check that keeps that true rather than merely hoped-for. Without it, a
// too-tight per-process -timeout (this one included, before it was
// measured and corrected) degrades coverage silently: `go test` still
// exits 0, because rapid's own early-exit is a documented, intentional
// "OK" — a hollow green exactly like a feature flag quietly defaulting to
// off. The assertion turns any future recurrence of that (a slower chDB, a
// busier runner, a further-widened shape space) into a loud, actionable CI
// failure instead of a silently shrinking test-fence.
//
// Env:
//   TAGS  go build tags for the leg (chdb,agpl_oracle,chdb_agpl_oracle in
//         CI). Required.
//   GO    go executable; default `go`. Test seam.
//
// Exit: 0 when every leg passes AND no rapid-based test in any leg silently
// ran fewer checks than requested; 1 otherwise.
//
// node: builtins only — no npm deps, no setup-node needed.

import process from 'node:process';
import { error, notice, log, group } from './lib/gh.mjs';
import { runLegBuffered } from './lib/spawn-tagged.mjs';

/** The randomized native-histogram sweep, fanned out across HISTOGRAM_FANOUT processes. */
export const HISTOGRAM_SWEEP_TEST = 'TestPromQL_Property_NativeHistogram';

/** The deterministic native-histogram roster, isolated in its own process — see the header. */
export const HISTOGRAM_ROSTER_TEST = 'TestPromQL_NativeHistogramShapeRoster';

/** Both tests moved out of the "rest" leg, for the -skip pattern below. */
export const HISTOGRAM_TESTS = [HISTOGRAM_SWEEP_TEST, HISTOGRAM_ROSTER_TEST];

/** Matches exactly HISTOGRAM_TESTS at the top level — never a substring. Used by the rest leg's -skip. */
export const HISTOGRAM_RUN_PATTERN = `^(${HISTOGRAM_TESTS.join('|')})$`;

/** Matches exactly HISTOGRAM_SWEEP_TEST at the top level. Used by each sweep share's -run. */
export const HISTOGRAM_SWEEP_RUN_PATTERN = `^${HISTOGRAM_SWEEP_TEST}$`;

/** Matches exactly HISTOGRAM_ROSTER_TEST at the top level. Used by the roster leg's -run. */
export const HISTOGRAM_ROSTER_RUN_PATTERN = `^${HISTOGRAM_ROSTER_TEST}$`;

/**
 * How many PROCESSES the native-histogram sweep's -rapid.checks fans out to.
 *
 * A GitHub-hosted `ubuntu-latest` runner has 4 cores. chDB threads WITHIN a
 * single query as well (the same effect chdb-roundtrip.mjs's FANOUT
 * reasoning names for the promql roundtrip leg), so process-count fan-out
 * oversubscribes well before the process count reaches the core count —
 * measured locally as a ~2x per-check slowdown (2.38s/check serial vs
 * ~5.2s/check with 3 sweep shares + the rest leg all concurrent, even on an
 * 8-core box). 3 sweep shares + 1 roster leg (light) + 1 rest leg keeps the
 * heavy concurrent processes at 4, matching the runner's core count without
 * adding a 5th.
 */
export const HISTOGRAM_FANOUT = 3;

/**
 * Per-process go test -timeout, in minutes.
 *
 * HISTOGRAM_TIMEOUT_MINUTES covers one sweep share (~500/HISTOGRAM_FANOUT,
 * i.e. ~167 checks). Measured locally: 3 concurrent shares of ~167
 * requested checks each, running alongside the rest leg, needed ~5.0-5.5s
 * PER CHECK under that contention (derived from a 12-minute deadline that
 * was too tight and made rapid self-truncate — see the header) — so the
 * full ~167 checks need roughly 167 * 5.5s ≈ 918s (~15.3 minutes) in the
 * worst observed case. 20 minutes (unchanged from the pre-split single-
 * process ceiling) leaves comfortable headroom over that, and
 * assertNoSilentTruncation below fails the run outright if a share ever
 * needs more than its budget allows rather than accepting a quietly
 * shrunk pass.
 *
 * ROSTER_TIMEOUT_MINUTES covers only the deterministic shape roster,
 * decoupled from any checks budget; it does not use rapid.Check; even the
 * failed run that mis-timed it got through 11 of ~15 differentials in 24s
 * under the same heavy contention, so its full pass is a low-tens-of-
 * seconds cost. 5 minutes leaves at least an order of magnitude of margin.
 *
 * REST_TIMEOUT_MINUTES covers every other property test in the package —
 * every one of them completed its own full -rapid.checks=500 sweep in
 * 30-90s in the measured run, ~350s total; 10 minutes leaves more than 6x
 * headroom.
 */
export const HISTOGRAM_TIMEOUT_MINUTES = 20;
export const ROSTER_TIMEOUT_MINUTES = 5;
export const REST_TIMEOUT_MINUTES = 10;

const DEFAULT_RAPID_CHECKS = '500';
const DEFAULT_RAPID_SHRINKTIME = '5m';

/**
 * Splits `total` checks into `n` near-equal, positive integer shares
 * summing back to exactly `total` (the first `total % n` shares get one
 * extra check rather than leaving a remainder unassigned).
 */
export function splitRapidChecks(total, n) {
  const base = Math.floor(total / n);
  const remainder = total % n;
  return Array.from({ length: n }, (_, i) => base + (i < remainder ? 1 : 0));
}

/**
 * The leg commands: HISTOGRAM_FANOUT sweep shares, one roster leg, one rest
 * leg.
 *
 * Every histogram-touching leg targets `./test/property` (not `/...`)
 * because the tests it selects all live in that one package; the rest leg
 * targets `./test/property/...` with `-skip` for HISTOGRAM_RUN_PATTERN, so
 * test/property/gen and test/property/oracle/{promql,traceql} — already
 * separate packages Go builds and runs concurrently on their own — run
 * exactly once, and every OTHER top-level test in test/property runs
 * exactly once too.
 *
 * The roster leg carries no -rapid.checks / -rapid.shrinktime: it is
 * deterministic (property.RunShapeExamples over a fixed roster), not a
 * rapid.Check sweep, so those flags would be silently unused.
 *
 * Flag ordering matters: `-rapid.checks` / `-rapid.shrinktime` are
 * registered by the rapid library, not by `go test`, so they must appear
 * AFTER the package list — otherwise the toolchain misparses them as build
 * flags and drops the package path.
 */
export function legCommands({
  tags,
  go = 'go',
  rapidChecks = DEFAULT_RAPID_CHECKS,
  rapidShrinktime = DEFAULT_RAPID_SHRINKTIME,
}) {
  const common = ['-tags', tags, '-count=1', '-v'];
  const shares = splitRapidChecks(Number(rapidChecks), HISTOGRAM_FANOUT);

  const sweepLegs = shares.map((share, i) => ({
    name: `histogram-sweep ${i + 1}/${HISTOGRAM_FANOUT}`,
    argv: [
      go,
      'test',
      ...common,
      `-timeout=${HISTOGRAM_TIMEOUT_MINUTES}m`,
      './test/property',
      '-run',
      HISTOGRAM_SWEEP_RUN_PATTERN,
      `-rapid.checks=${share}`,
      `-rapid.shrinktime=${rapidShrinktime}`,
    ],
    // The requested check depth this leg must actually reach —
    // assertNoSilentTruncation cross-checks the log against this.
    expectRapidChecks: share,
  }));

  const rosterLeg = {
    name: 'histogram-roster',
    argv: [
      go,
      'test',
      ...common,
      `-timeout=${ROSTER_TIMEOUT_MINUTES}m`,
      './test/property',
      '-run',
      HISTOGRAM_ROSTER_RUN_PATTERN,
    ],
    expectRapidChecks: null,
  };

  const restLeg = {
    name: 'rest',
    argv: [
      go,
      'test',
      ...common,
      `-timeout=${REST_TIMEOUT_MINUTES}m`,
      './test/property/...',
      '-skip',
      HISTOGRAM_RUN_PATTERN,
      `-rapid.checks=${rapidChecks}`,
      `-rapid.shrinktime=${rapidShrinktime}`,
    ],
    // The rest leg runs several DIFFERENT rapid.Check-based tests, each of
    // which should independently reach the full requested depth.
    expectRapidChecks: Number(rapidChecks),
  };

  return [...sweepLegs, rosterLeg, restLeg];
}

/**
 * Scans a leg's buffered output for every "[rapid] OK, passed N tests" line
 * and returns the ones where N fell short of the leg's expected depth.
 *
 * rapid.Check derives its own deadline from the test's `go test -timeout`
 * (pgregory.net/rapid's checkDeadline() reads t.Deadline()) and, when it
 * estimates it is running out of time, stops early — logging "[rapid] OK"
 * and PASSING the test with fewer than the requested checks rather than
 * failing it. `go test`'s own exit code cannot see this: it is a
 * legitimate pass by rapid's own contract. Silently shipping less coverage
 * than -rapid.checks promises is a hollow green exactly like a feature
 * flag quietly defaulting to off, so this leg fails the fan-out outright
 * instead of trusting the exit code alone.
 */
export function findTruncatedRapidRuns(output, expectRapidChecks) {
  if (expectRapidChecks == null) return [];
  const findings = [];
  const re = /\[rapid\] OK, passed (\d+) tests/g;
  for (const m of output.matchAll(re)) {
    const passed = Number(m[1]);
    if (passed < expectRapidChecks) {
      findings.push({ passed, expected: expectRapidChecks });
    }
  }
  return findings;
}

async function main() {
  const tags = process.env.TAGS ?? '';
  if (tags === '') {
    error('TAGS is required — the leg would compile without the chdb driver and run nothing');
    process.exit(1);
  }

  const go = process.env.GO ?? 'go';
  const legs = legCommands({ tags, go });
  notice(
    `property: ${legs.length} leg(s) — histogram sweep fanned out ${HISTOGRAM_FANOUT} ways, ` +
      'roster and rest isolated',
  );

  const started = Date.now();
  const results = await Promise.all(legs.map(runLegBuffered));
  const elapsedSeconds = Math.round((Date.now() - started) / 1000);

  for (const r of results) {
    group(`${r.leg.name} (exit ${r.code})`, () => log(r.out.trimEnd()));
  }

  const failed = results.filter((r) => r.code !== 0);

  const truncated = [];
  for (const r of results) {
    for (const finding of findTruncatedRapidRuns(r.out, r.leg.expectRapidChecks)) {
      truncated.push({ leg: r.leg.name, ...finding });
    }
  }
  for (const t of truncated) {
    error(
      `property: ${t.leg} silently ran only ${t.passed} of ${t.expected} requested rapid checks — ` +
        'rapid stopped early against its own deadline and still reported "OK"; this leg\'s ' +
        '-timeout needs headroom, not a passing exit code alone',
    );
  }

  if (failed.length > 0 || truncated.length > 0) {
    error(
      `property: ${failed.length} of ${results.length} leg(s) failed, ` +
        `${truncated.length} silent rapid truncation(s), after ${elapsedSeconds}s`,
    );
    process.exit(1);
  }
  notice(`property: ${results.length} leg(s) passed with full rapid depth in ${elapsedSeconds}s`);
}

// Import-safe: the tests import legCommands without running a leg.
if (process.argv[1] && process.argv[1].endsWith('property-fanout.mjs')) {
  await main();
}
