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
// Env contract:
//   REPORT     path to the gremlins JSON report   (default: gremlins.json)
//   THRESHOLD  efficacy floor as a number, e.g. 95
//
// Exit codes: 0 = efficacy >= threshold, 1 = below threshold / bad input.

import { readFileSync } from 'node:fs';
import process from 'node:process';
import { error, notice } from './lib/gh.mjs';

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

notice(`gremlins efficacy ${measured}% >= threshold ${threshold}%`);
process.exit(0);
