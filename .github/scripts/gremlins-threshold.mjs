// gremlins-threshold.mjs — mutation-efficacy gate, extracted from the
// "enforce efficacy threshold" step in .github/workflows/mutation.yml.
//
// gremlins v0.6.0 exits 0 even when --threshold-efficacy is violated, so
// the gate is done here against the parsed report JSON. Reproduces the
// original bash exactly:
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
// THE EFFICACY RATIO CANNOT SEE A TIMED-OUT MUTANT, so this gate also bounds
// how many of them there may be.
//
// gremlins excludes TIMED_OUT from BOTH sides of its own verdict
// (internal/report/report.go: `tEfficacy = killed / (killed + lived)` and
// `MutantsTotal = lived + killed + notViable`). A mutant that timed out is
// therefore in neither the ratio nor the total, so a leg can leave most of its
// mutants unadjudicated and still report a high efficacy over the handful that
// did run. This is not hypothetical: run 33126692323 passed every leg with
// `phase2-other` at Killed 77 / Lived 1 / Timed out 262 — 98.72% efficacy over
// 78 of 340 mutants (#2695). mutation-run.mjs's `mutants_total > 0` check only
// catches the fully degenerate all-timeout case.
//
// The share is counted from `files[].mutations[].status`, the per-mutant record,
// because no aggregate field in the report exposes it.
//
// Env contract:
//   REPORT     path to the gremlins JSON report   (default: gremlins.json)
//   THRESHOLD  efficacy floor as a number, e.g. 95
//
// Exit codes: 0 = efficacy >= threshold and the timed-out share is within
// bounds, 1 = below threshold / too many timed out / bad input.

import { readFileSync } from 'node:fs';
import process from 'node:process';
import { pathToFileURL } from 'node:url';
import { error, notice } from './lib/gh.mjs';

// maxTimedOutShare is the fraction of a leg's mutants that may time out before
// the leg's efficacy stops meaning anything.
//
// Not zero: a timeout is a legitimate outcome for a mutant that genuinely does
// not terminate (an inverted loop-advance), which is the case MUTANT_TIMEOUT_MAX
// exists to bound, and a healthy leg reports a few. The measured healthy shape
// is phase4-promql-g at 8 timed out of 311 (2.6%). A quarter is far above that
// and far below the 77% that was passing, so it separates "a few mutants are
// pathological" from "this leg did not really run".
const maxTimedOutShare = 0.25;

// timedOutStatus is the per-mutation status string gremlins writes
// (internal/mutator/mutator.go). Matched exactly: a rename upstream must fail
// this gate loudly rather than silently count zero timeouts forever.
const timedOutStatus = 'TIMED OUT';

// countMutationStatuses walks the per-file mutation records and returns the
// total number of mutants and how many timed out.
export function countMutationStatuses(report) {
  let total = 0;
  let timedOut = 0;
  for (const file of report?.files ?? []) {
    for (const m of file?.mutations ?? []) {
      total++;
      if (m?.status === timedOutStatus) timedOut++;
    }
  }
  return { total, timedOut };
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

  // Checked AFTER the efficacy comparison so a leg that fails both still reports
  // the efficacy number first — that is the one an operator recognises.
  const { total, timedOut } = countMutationStatuses(report);
  if (total > 0) {
    const share = timedOut / total;
    if (share > maxTimedOutShare) {
      error(
        `gremlins timed out ${timedOut}/${total} mutants (${(share * 100).toFixed(1)}%), over the ` +
          `${(maxTimedOutShare * 100).toFixed(0)}% bound. Efficacy ${measured}% is computed over the ` +
          `killed+lived mutants ONLY, so it does not describe this leg: a timed-out mutant is in ` +
          `neither the ratio nor mutants_total. Raise the per-mutant budget or reduce what the leg ` +
          `mutates; do not read the efficacy above as a verdict.`,
      );
      process.exit(1);
    }
    notice(`gremlins timed out ${timedOut}/${total} mutants (${(share * 100).toFixed(1)}%)`);
  }

  notice(`gremlins efficacy ${measured}% >= threshold ${threshold}%`);
  process.exit(0);
}

if (process.argv[1] !== undefined && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main();
}
