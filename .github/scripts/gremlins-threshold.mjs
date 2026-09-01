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
// A TIMED-OUT MUTANT IS AN UNADJUDICATED MUTANT, AND COUNTS AGAINST THE SCORE.
//
// This is the property the whole gate rests on, so it is worth stating as a
// rule rather than as a formula: THE SCORE MUST BE MONOTONE NON-INCREASING IN
// THE TIMEOUT COUNT. Moving one mutant from any other status into TIMED OUT may
// never raise the number this gate reads. A gate that violates that rule pays a
// slower runner in efficacy points, and a green run then proves that the
// machine was busy rather than that the tests are strong.
//
// gremlins excludes TIMED_OUT from BOTH sides of its own verdict
// (internal/report/report.go: `tEfficacy = killed / (killed + lived)` and
// `MutantsTotal = lived + killed + notViable`), so its ratio VIOLATES that rule
// in the worst way — a timeout leaves the denominator entirely, so starving the
// runner raises the score. This gate therefore recomputes efficacy over every
// mutant the runner ATTEMPTED to adjudicate, timeouts included:
//
//   efficacy = killed / (killed + lived + timedOut)
//
// which is monotone by construction, and is never above the ratio gremlins
// reported.
//
// Why the opposite reading was wrong (cerberus issue #2903)
// --------------------------------------------------------
// This gate previously counted a timeout as a DETECTION, on the premise that a
// mutant which exhausts the per-mutant budget "did not do so because the
// machine was slow — it did so because the mutation broke termination". The
// premise was checked against `internal/chsql`'s recursive renderer and looked
// sound there. It is false, and it is false on that very leg.
//
// The budget wraps the whole `go test` child, which is a RECOMPILE, a LINK and
// then a run. Measured per mutant, on an 8-core machine, against the 15s budget
// those legs ran under:
//
//   internal/chsql   compile+link 12.7-14.4s   test run 0.31s
//   internal/promql  compile+link  8.4-8.7s    test run 2.07s
//
// The compiler is 80-98% of the budget and the mutant's own execution is a
// rounding error, so TIMED OUT records "the compiler was slow today", not
// "this mutation stopped the suite terminating".
//
// Two signatures in the real reports confirm it:
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
// Counting timeouts as detections scored that last case — a leg that
// adjudicated 3% of its mutants and found no survivor because it never ran long
// enough to see one — as a perfect leg.
//
// The paired defence is the per-mutant budget itself: .github/scripts/
// mutation-run.mjs now MEASURES the leg's own recompile+link+run cycle and
// sizes the budget from it, so an honest leg has few timeouts to spend. This
// gate is what makes that safe to get wrong in the tight direction — an
// undersized budget now shows up as a RED leg rather than as a flattering one.
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
// Matched exactly: a rename upstream must fail this gate loudly rather than
// silently count zero of everything forever.
const timedOutStatus = 'TIMED OUT';
const killedStatus = 'KILLED';
const livedStatus = 'LIVED';

// countMutationStatuses walks the per-file mutation records and returns the
// mutant total plus the per-status counts the verdict is computed from.
export function countMutationStatuses(report) {
  let total = 0;
  let timedOut = 0;
  let killed = 0;
  let lived = 0;
  for (const file of report?.files ?? []) {
    for (const m of file?.mutations ?? []) {
      total++;
      if (m?.status === timedOutStatus) timedOut++;
      else if (m?.status === killedStatus) killed++;
      else if (m?.status === livedStatus) lived++;
    }
  }
  return { total, timedOut, killed, lived };
}

// attemptedEfficacy is the kill rate over every mutant the runner ATTEMPTED to
// adjudicate — killed, survived, or ran out of budget — or null when it
// attempted none. A timeout is an unknown, and an unknown is not a kill, so it
// sits in the denominator only.
export function attemptedEfficacy({ killed, lived, timedOut }) {
  const attempted = killed + lived + timedOut;
  if (attempted === 0) return null;
  return (killed / attempted) * 100;
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

  const completed = counts.killed + counts.lived;
  if (completed < minCompletedMutants) {
    error(
      `gremlins completed ${completed} mutant(s) — killed ${counts.killed}, lived ${counts.lived}, ` +
        `with ${counts.timedOut} timed out of ${counts.total}. Nothing ran to a verdict, so neither ` +
        `the reported ${reported}% nor the timeouts say anything about this package: this is the ` +
        `per-mutant budget collapsing, not the tests failing (#2692).`,
    );
    process.exit(1);
  }

  const measured = attemptedEfficacy(counts);
  const attempted = counts.killed + counts.lived + counts.timedOut;

  if (measured < threshold) {
    error(
      `gremlins efficacy ${measured.toFixed(2)}% < threshold ${threshold}% — ` +
        `killed ${counts.killed} of ${attempted} attempted (lived ${counts.lived}, ` +
        `timed out ${counts.timedOut}). A timed-out mutant is unadjudicated, not detected: ` +
        `it counts against this ratio (#2903). gremlins reported ${reported}% over killed+lived only.`,
    );
    process.exit(1);
  }

  if (counts.timedOut > 0) {
    notice(
      `gremlins timed out ${counts.timedOut}/${counts.total} mutants, counted as unadjudicated: ` +
        `efficacy ${measured.toFixed(2)}% (gremlins reported ${reported}% over killed+lived only)`,
    );
  }
  notice(`gremlins efficacy ${measured.toFixed(2)}% >= threshold ${threshold}%`);
  process.exit(0);
}

if (process.argv[1] !== undefined && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main();
}
