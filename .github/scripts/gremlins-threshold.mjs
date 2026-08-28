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
// Reproduces the original bash exactly:
//
//   measured=$(jq -r '.test_efficacy' gremlins.json)
//   if awk 'BEGIN { exit (m < t) ? 1 : 0 }'; then  # m < t  -> fail
//     ::notice:: measured >= threshold
//   else
//     ::error::  measured <  threshold ; exit 1
//
// The comparison is `measured < threshold` -> FAIL (strict less-than),
// i.e. `measured >= threshold` passes. Matches the awk `m < t` semantics.
//
// A TIMED-OUT MUTANT IS A DETECTED MUTANT, and gremlins counts it as neither
// killed nor lived.
//
// gremlins excludes TIMED_OUT from BOTH sides of its own verdict
// (internal/report/report.go: `tEfficacy = killed / (killed + lived)` and
// `MutantsTotal = lived + killed + notViable`), so a leg where most mutants time
// out reports a confident-looking ratio computed over the few that did not. Run
// 33156989462's `phase2-builder` reported 97.50% over 40 of 399 mutants; 279
// timed out and appear in no number the gate could previously read.
//
// The orthodox reading, and the one the measurements support, is that those 279
// were KILLED. `internal/chsql` recompiles, links and runs in 1.04s at eight
// cores and 2.42s at one, against a 15s budget — a 6x-15x margin — so a mutant
// that exhausts it did not do so because the machine was slow. It did so because
// the mutation broke termination, which is exactly what negating a conditional
// in a RECURSIVE renderer does: the mutator statistics put the timeouts in
// CONDITIONALS_NEGATION (75%) and CONDITIONALS_BOUNDARY (93%), not in
// INVERT_LOOPCTRL (4 mutants in the whole leg). The suite caught each one by
// hanging on it.
//
// Efficacy is therefore recomputed here with timeouts counted as detections.
// That is MORE permissive than gremlins' own ratio, which is why it is paired
// with minCompletedMutants: the one thing this must never wave through is
// #2692's budget collapse, where nothing completed and the timeouts prove
// nothing about the tests.
//
// Counts come from `files[].mutations[].status`, the per-mutant record, because
// no aggregate field in the report exposes the timed-out total.
//
// Env contract:
//   REPORT     path to the gremlins JSON report   (default: gremlins.json)
//   THRESHOLD  efficacy floor as a number, e.g. 95
//
// Exit codes: 0 = efficacy >= threshold both as gremlins reports it and with
// timeouts counted as detections, 1 = below either / nothing completed / bad
// input.

import { readFileSync } from 'node:fs';
import process from 'node:process';
import { pathToFileURL } from 'node:url';
import { error, notice } from './lib/gh.mjs';

// minCompletedMutants is how many mutants must reach a NORMAL verdict (KILLED
// or LIVED) before this leg's numbers describe anything.
//
// This guards the BUDGET-COLLAPSE signature, not pathological mutants. When the
// per-mutant budget collapsed (#2692) the leg reported Killed 0 / Lived 0 /
// Timed out 295: nothing ran, so the timeouts are evidence about the budget
// rather than about the tests. Counting them as detections — which is what this
// gate otherwise does — would score that 295/295 = 100%, so the collapse needs
// its own floor and cannot be caught by a ratio.
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

// detectedEfficacy is the kill rate with timeouts counted as detections, or
// null when no mutant reached any verdict at all.
export function detectedEfficacy({ killed, lived, timedOut }) {
  const detected = killed + timedOut;
  if (detected + lived === 0) return null;
  return (detected / (detected + lived)) * 100;
}

// main runs the gate. Guarded below so a test can import
// countMutationStatuses without the import running the gate as a side effect.
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

  const measured = Number(report.test_efficacy);
  if (!Number.isFinite(measured)) {
    // jq -r '.test_efficacy' on a missing key yields "null"; surface that.
    error(`gremlins-threshold.mjs: .test_efficacy missing or non-numeric in ${reportPath}`);
    process.exit(1);
  }

  // awk `m < t` -> exit 1 (fail). So measured < threshold fails the gate.
  if (measured < threshold) {
    error(`gremlins efficacy ${measured}% < threshold ${threshold}%`);
    process.exit(1);
  }

  const counts = countMutationStatuses(report);
  if (counts.total === 0) {
    // No per-mutation records to recompute from; gremlins' own ratio already
    // cleared the bar above.
    notice(`gremlins efficacy ${measured}% >= threshold ${threshold}%`);
    process.exit(0);
  }

  const completed = counts.killed + counts.lived;
  if (completed < minCompletedMutants) {
    error(
      `gremlins completed ${completed} mutant(s) — killed ${counts.killed}, lived ${counts.lived}, ` +
        `with ${counts.timedOut} timed out of ${counts.total}. Nothing ran to a verdict, so neither ` +
        `the reported ${measured}% nor the timeouts say anything about this package: this is the ` +
        `per-mutant budget collapsing, not the tests failing (#2692).`,
    );
    process.exit(1);
  }

  // No second threshold comparison here, deliberately. Counting timeouts into
  // both the numerator and the denominator moves the ratio TOWARD 100%, so the
  // detected rate is always >= the rate gremlins reported: a leg that cleared
  // the bar above clears it again by construction. Gating on it would be dead
  // code that reads like a second opinion.
  const detected = detectedEfficacy(counts);

  if (counts.timedOut > 0) {
    notice(
      `gremlins timed out ${counts.timedOut}/${counts.total} mutants, counted as detected: ` +
        `efficacy ${detected.toFixed(2)}% (gremlins reported ${measured}% over killed+lived only)`,
    );
  }
  notice(`gremlins efficacy ${measured}% >= threshold ${threshold}%`);
  process.exit(0);
}

if (process.argv[1] !== undefined && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main();
}
