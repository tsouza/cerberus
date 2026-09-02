// gremlins-threshold.mjs — mutation-efficacy gate, extracted from the
// "enforce efficacy threshold" step in .github/workflows/mutation.yml.
//
// The lane deliberately does NOT hand gremlins a threshold, and gates here
// instead, against the parsed report JSON.
//
// Not because gremlins would ignore one: the pinned fork DOES exit non-zero on
// a violation (report.go assess -> execution.NewExitErr(EfficacyThreshold) ->
// main.go exitCode). That is exactly why the flag is withheld. gremlins exits
// mid-run, before the report is read and uploaded, so the failure surfaces as an
// opaque `gremlins exited N` from runGremlins() with no artifact to inspect and
// no measured-vs-threshold numbers. Gating after the report is written keeps
// both. Do not "simplify" this by passing --threshold-efficacy.
//
// AN UNADJUDICATED MUTANT COUNTS AGAINST THE SCORE. A MUTANT THAT STOPPED ITS
// OWN SUITE TERMINATING IS ADJUDICATED.
//
// The whole gate rests on telling those two apart, so it is worth stating as a
// rule rather than as a formula: THE SCORE MUST BE MONOTONE NON-INCREASING IN
// THE UNADJUDICATED COUNT. Moving one mutant from any other status into an
// unadjudicated one may never raise the number this gate reads. A gate that
// violates that rule pays a slower runner in efficacy points, and a green run
// then proves that the machine was busy rather than that the tests are strong.
//
// gremlins excludes both timeout statuses from BOTH sides of its own verdict
// (internal/report/report.go: `tEfficacy = killed / (killed + lived)` and
// `MutantsTotal = lived + killed + notViable`), so its ratio VIOLATES that rule
// in the worst way — a timeout leaves the denominator entirely, so starving the
// runner raises the score. This gate therefore recomputes efficacy over every
// mutant the runner ATTEMPTED to adjudicate, both timeout kinds included:
//
//   efficacy = (killed + runTimedOut) / (killed + lived + timedOut + runTimedOut)
//
// WHICH TIMEOUT IS WHICH
// ----------------------
// A mutant is leashed twice, and the pinned fork reports which leash claimed it
// (#2910/#2929 split the two bounds; #2944 made the report say which fired):
//
//   RUN TIMED OUT  the test BINARY's own `-timeout` watchdog fired and printed
//                  `panic: test timed out after `. The Go toolchain starts that
//                  clock when the binary starts, so no compile can consume it.
//                  This is a positive observation produced by the mutant's own
//                  suite: the suite did not finish. Against an original that
//                  finishes in a fraction of the bound that is a change in
//                  observable behaviour, caught by the suite's own runtime
//                  bound — which is what a detection is. Numerator.
//
//   TIMED OUT      the context deadline covering compile AND run expired. Which
//                  phase spent it is unknown: a compile that hung reaches this
//                  status identically to a run that did, and so does a mutant
//                  killed by .github/scripts/mutant-memory-guard.mjs, which reaps a memory runaway and
//                  then HOLDS precisely so that no exit status of its own can be
//                  read as a verdict (#2919, #2921). Unadjudicated. Denominator
//                  only, credited to nobody.
//
// Collapsing those two back into one number re-creates #2903 in a new place, so
// an unrecognised status string is a hard failure below rather than a silent
// zero.
//
// Why the FLAT reading — every timeout a detection — was wrong (#2903)
// -------------------------------------------------------------------
// This gate once counted every timeout as a detection, on the premise that a
// mutant which exhausts the per-mutant budget "did not do so because the machine
// was slow — it did so because the mutation broke termination". The premise was
// checked against `internal/chsql`'s recursive renderer and looked sound there.
// It was false, and it was false on that very leg.
//
// The budget then wrapped the whole `go test` child, which is a RECOMPILE, a
// LINK and then a run. Measured per mutant, on an 8-core machine, against the
// 15s budget those legs ran under:
//
//   internal/chsql   compile+link 12.7-14.4s   test run 0.31s
//   internal/promql  compile+link  8.4-8.7s    test run 2.07s
//
// The compiler was 80-98% of the budget and the mutant's own execution was a
// rounding error, so TIMED OUT recorded "the compiler was slow today", not
// "this mutation stopped the suite terminating".
//
// Two signatures in the real reports confirmed it:
//
//   - Runs 33542904091 (red) and 33551271099 (green) on `phase4-promql-lower`
//     recorded the SAME 486 kills. The whole 94.00% -> 97.01% swing was 16
//     mutants moving LIVED -> TIMED OUT, and the green runner was the SLOWER
//     one (coverage 53.2s vs 36.8s). Matching mutants across the two revisions
//     by source text, 18 of the red run's 31 survivors — short-circuit boolean
//     guards and slice-capacity arithmetic, none of which can unbound a loop —
//     were booked as detections on the green run.
//
//   - gremlins runs each mutant with `-failfast`, so a KILLED mutant exits at
//     its first failing test while a LIVED mutant must run the suite to the
//     end. Budget starvation therefore converts SURVIVORS preferentially. Run
//     33522074818 shows the endpoint of that bias: all four `internal/chsql`
//     legs reported 100.00% with `lived: 0`, over 1266 mutants of which 1230
//     timed out.
//
// Every one of those mutants is a TIMED OUT today — the compile is what consumed
// the budget, and the compile is what the backstop covers — so none of them
// would be credited by the reading below. The narrow reading is not the flat one
// coming back.
//
// WHAT MAKES CREDITING SAFE, AND WHERE IT LIVES
// ---------------------------------------------
// Not this file. Two mechanisms elsewhere are what stop a RUN TIMED OUT from
// being manufactured by a starved runner, and reverting either turns the credit
// below into a lie:
//
//   - the fork's split (#2929) and its output-read verdict: the run bound is
//     handed to `go test -timeout`, so compilation cannot spend it, and the
//     verdict comes from the child's own output rather than from an exit status
//     `go test` shares between a timeout, a build failure and a real failure.
//   - the MEASURED budget in .github/scripts/mutation-run.mjs: the run bound is this leg's own
//     recompile+link+run cycle, times the mutant fan-out, times 2 for runner
//     variance, floored at MUTANT_TIMEOUT_MIN. It is an upper bound on the
//     honest run several times over, so an honest suite cannot reach it.
//     test/regression/mutation_timeout_max_test.go pins both bounds and the fork
//     tag together for exactly this reason.
//
// The residual asymmetry is deliberate, and is stated rather than hidden: moving
// a mutant LIVED -> RUN TIMED OUT DOES raise the score, because a survivor that
// stops terminating has stopped surviving. Moving one into TIMED OUT never
// does. The budget-collapse floor below is therefore computed over NORMAL
// verdicts only — a leg that killed nothing and survived nothing has not shown
// its budget works, whatever its run timeouts say.
//
// Counts come from `files[].mutations[].status`, the per-mutant record, because
// no aggregate field in the report exposes the timed-out total.
//
// Env contract:
//   REPORT     path to the gremlins JSON report   (default: gremlins.json)
//   THRESHOLD  efficacy floor as a number, e.g. 95
//
// Exit codes: 0 = efficacy over the attempted mutants >= threshold, 1 = below
// it / nothing completed / bad input.

import { readFileSync } from 'node:fs';
import process from 'node:process';
import { pathToFileURL } from 'node:url';
import { error, notice } from './lib/gh.mjs';

// minCompletedMutants is how many mutants must reach a NORMAL verdict (KILLED
// or LIVED) before this leg's numbers describe anything.
//
// This guards the BUDGET-COLLAPSE signature (#2692), where the leg reported
// Killed 0 / Lived 0 / Timed out 295. The ratio above already fails that case —
// 0/295 is 0% — but it fails it with the message of a weak test suite, and the
// remedy for a collapsed budget is not "write more tests". The floor exists to
// name the right cause.
//
// One is the honest minimum: it separates "nothing completed" from "something
// did". Any larger figure would be a guess about how many mutants a leg ought
// to have.
const minCompletedMutants = 1;

// The per-mutation status strings gremlins writes (internal/mutator/mutator.go).
// Matched exactly, and the set is CLOSED: a status this gate does not recognise
// is a hard failure, not a mutant quietly dropped from both sides of the ratio.
//
// That closure is what makes the two timeout kinds safe to score differently. A
// fork that renames a status, or adds one — as it DID when RUN TIMED OUT was
// split out of TIMED OUT — would otherwise leave the new status uncounted,
// which takes mutants out of the denominator and raises every leg's score.
// Failing loudly costs one red run; counting zero of them costs the number its
// meaning.
const timedOutStatus = 'TIMED OUT';
const runTimedOutStatus = 'RUN TIMED OUT';
const killedStatus = 'KILLED';
const livedStatus = 'LIVED';

// The statuses that carry no verdict about the test suite: a mutant no test
// reaches, one no compiler accepts, one the run never got to. They are outside
// both sides of the ratio, and they are enumerated so that they are outside it
// DELIBERATELY rather than by falling through an unrecognised branch.
const unscoredStatuses = ['NOT COVERED', 'NOT VIABLE', 'SKIPPED', 'RUNNABLE'];

const knownStatuses = new Set([
  timedOutStatus,
  runTimedOutStatus,
  killedStatus,
  livedStatus,
  ...unscoredStatuses,
]);

// countMutationStatuses walks the per-file mutation records and returns the
// mutant total plus the per-status counts the verdict is computed from.
// `unknown` collects every status string outside the closed set above, keyed by
// the string, so the caller can fail on a report it does not understand instead
// of scoring one.
export function countMutationStatuses(report) {
  let total = 0;
  let timedOut = 0;
  let runTimedOut = 0;
  let killed = 0;
  let lived = 0;
  const unknown = new Map();
  for (const file of report?.files ?? []) {
    for (const m of file?.mutations ?? []) {
      total++;
      const status = m?.status;
      if (status === timedOutStatus) timedOut++;
      else if (status === runTimedOutStatus) runTimedOut++;
      else if (status === killedStatus) killed++;
      else if (status === livedStatus) lived++;
      else if (!knownStatuses.has(status)) {
        const key = String(status);
        unknown.set(key, (unknown.get(key) ?? 0) + 1);
      }
    }
  }
  return { total, timedOut, runTimedOut, killed, lived, unknown };
}

// attemptedEfficacy is the detection rate over every mutant the runner
// ATTEMPTED to adjudicate, or null when it attempted none.
//
// Two things are detections. A KILLED mutant made a test fail. A RUN TIMED OUT
// mutant made the suite stop finishing, reported by the test binary itself
// against a bound no compile can spend — a change in observable behaviour that
// the suite's own runtime bound caught. A TIMED OUT mutant is neither: the
// compile+run backstop cannot say which phase spent it, so it is an unknown,
// and an unknown is not a detection. It sits in the denominator only.
export function attemptedEfficacy({ killed, lived, timedOut, runTimedOut = 0 }) {
  const attempted = killed + lived + timedOut + runTimedOut;
  if (attempted === 0) return null;
  return ((killed + runTimedOut) / attempted) * 100;
}

// main runs the gate. Guarded below so a test can import the helpers without
// the import running the gate as a side effect.
function main() {
  const reportPath = process.env.REPORT || 'gremlins.json';
  const thresholdRaw = process.env.THRESHOLD;

  if (thresholdRaw === undefined || thresholdRaw === '') {
    error('gremlins-threshold.mjs: THRESHOLD env var is required');
    process.exit(1);
  }

  const threshold = Number(thresholdRaw);
  if (!Number.isFinite(threshold)) {
    error(`gremlins-threshold.mjs: THRESHOLD="${thresholdRaw}" is not a number`);
    process.exit(1);
  }

  let report;
  try {
    report = JSON.parse(readFileSync(reportPath, 'utf8'));
  } catch (e) {
    error(`gremlins-threshold.mjs: cannot read/parse ${reportPath}: ${e.message}`);
    process.exit(1);
  }

  // Read but do not gate on gremlins' own ratio. It is killed/(killed+lived),
  // which is always >= the ratio gated below, so a comparison against it could
  // never fire where the one below passed. It is still parsed so that a rename
  // or a malformed report fails loudly instead of being scored as zero
  // timeouts, and it is reported so the log shows both numbers.
  const reported = Number(report.test_efficacy);
  if (!Number.isFinite(reported)) {
    // jq -r '.test_efficacy' on a missing key yields "null"; surface that.
    error(`gremlins-threshold.mjs: .test_efficacy missing or non-numeric in ${reportPath}`);
    process.exit(1);
  }

  const counts = countMutationStatuses(report);

  // A status this gate has no branch for is a report it cannot score. Silently
  // ignoring one removes those mutants from the denominator, which raises the
  // leg's number for a reason that has nothing to do with its tests — the exact
  // shape of every defect this gate exists to stop. It is also the concrete
  // failure mode of the fork bump that introduced RUN TIMED OUT: an older gate
  // paired with a newer fork would have scored those mutants out of existence.
  if (counts.unknown.size > 0) {
    const listed = [...counts.unknown.entries()].map(([s, n]) => `${JSON.stringify(s)} x${n}`).join(', ');
    error(
      `gremlins-threshold.mjs: ${reportPath} carries mutation status(es) this gate does not know: ` +
        `${listed}. A status with no branch here would leave those mutants out of BOTH sides of the ` +
        `ratio and raise this leg's score for free, so it fails instead. Add the status to this ` +
        `script's closed set and decide, explicitly, which side of the ratio it belongs on.`,
    );
    process.exit(1);
  }

  if (counts.total === 0) {
    // No per-mutation records to recompute from. There is nothing to say about
    // timeouts, so gremlins' own ratio is the only number available.
    if (reported < threshold) {
      error(`gremlins efficacy ${reported}% < threshold ${threshold}%`);
      process.exit(1);
    }
    notice(`gremlins efficacy ${reported}% >= threshold ${threshold}%`);
    process.exit(0);
  }

  // NORMAL verdicts only — killed or lived. A run-phase timeout is adjudicated
  // and is credited below, but it is NOT evidence that the budget worked: a
  // collapsed run bound manufactures exactly that status, so counting one here
  // would let the collapse signature this floor exists to name pass as a leg
  // that adjudicated something.
  const completed = counts.killed + counts.lived;
  if (completed < minCompletedMutants) {
    error(
      `gremlins completed ${completed} mutant(s) — killed ${counts.killed}, lived ${counts.lived}, ` +
        `with ${counts.timedOut} timed out and ${counts.runTimedOut} run-timed-out of ${counts.total}. ` +
        `Nothing ran to a normal verdict, so neither the reported ${reported}% nor the timeouts say ` +
        `anything about this package: this is the per-mutant budget collapsing, not the tests ` +
        `failing (#2692).`,
    );
    process.exit(1);
  }

  const measured = attemptedEfficacy(counts);
  const attempted = counts.killed + counts.lived + counts.timedOut + counts.runTimedOut;
  const detected = counts.killed + counts.runTimedOut;

  if (measured < threshold) {
    error(
      `gremlins efficacy ${measured.toFixed(2)}% < threshold ${threshold}% — ` +
        `detected ${detected} of ${attempted} attempted (killed ${counts.killed}, run timed out ` +
        `${counts.runTimedOut}, lived ${counts.lived}, timed out ${counts.timedOut}). A mutant the ` +
        `compile+run backstop claimed is unadjudicated, not detected: it counts against this ratio ` +
        `(#2903). gremlins reported ${reported}% over killed+lived only.`,
    );
    process.exit(1);
  }

  if (counts.runTimedOut > 0) {
    // Both numbers, always. The delta between them is how much of this leg's
    // score rests on the run-phase credit, which is the one thing a reader
    // reviewing #2944's decision needs and cannot recompute from the log.
    const uncredited = attemptedEfficacy({
      killed: counts.killed,
      lived: counts.lived,
      timedOut: counts.timedOut + counts.runTimedOut,
      runTimedOut: 0,
    });
    notice(
      `gremlins run-timed-out ${counts.runTimedOut}/${counts.total} mutants, counted as DETECTIONS: ` +
        `the test binary's own -timeout watchdog fired, which no compile can consume (#2944). ` +
        `efficacy ${measured.toFixed(2)}%; without that credit it would be ` +
        `${uncredited.toFixed(2)}% over the same ${attempted} attempted`,
    );
  }
  if (counts.timedOut > 0) {
    notice(
      `gremlins timed out ${counts.timedOut}/${counts.total} mutants on the compile+run backstop, ` +
        `counted as unadjudicated: efficacy ${measured.toFixed(2)}% ` +
        `(gremlins reported ${reported}% over killed+lived only)`,
    );
  }
  notice(`gremlins efficacy ${measured.toFixed(2)}% >= threshold ${threshold}%`);
  process.exit(0);
}

if (process.argv[1] !== undefined && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main();
}
