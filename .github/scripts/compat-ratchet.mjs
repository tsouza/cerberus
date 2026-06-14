// compat-ratchet.mjs — parity-regression RATCHET for the required
// compatibility/{prometheus,loki,tempo} checks.
//
// Background: PR #503 ("compat is informational", task #68) downgraded
// per-case parity diffs to report-only — the three harnesses score
// parity into compat-score.json and exit 0 even when parity drifts, so a
// real numeric regression on the main route used to merge GREEN. This
// script restores a GATE: it reads the run's compat-score.json for one
// head and the committed per-head baseline, and FAILS the job when the
// run drops below baseline. Noise WITHIN the baseline (a run that matches
// or exceeds it) passes; only a real DROP gates.
//
// Why this can't flake: the three differs compare with absolute +
// relative epsilon tolerance (1e-9) over canonical-key-sorted result
// sets against a deterministic seed, so passed/total are stable run to
// run (verified: three consecutive green main runs produced byte-
// identical 574/574, 116/116, 48/48). The ratchet compares integers, not
// floats — there is no float/timing/ordering surface left to jitter. It
// is NOT an allow-list: it never names individual cases or excuses a real
// failure; it only pins the aggregate floor and rejects any drop below
// it. (Contrast the deleted expected-failures.json, which allow-listed
// specific known-failing cases — that is exactly what this is not.)
//
// Two floors per head, both gating:
//   - passed: a run with FEWER agreeing cases than baseline is a real
//     regression -> fail.
//   - total : a run whose corpus SHRANK below baseline is rejected ->
//     fail, so a regression can't hide by silently dropping a failing
//     case from the corpus (which would otherwise keep `passed` flat
//     while parity actually got worse).
//
// Raising the floor: when the harness legitimately gains passing cases
// or the corpus grows, BUMP the matching entry in
// compatibility/parity-baseline.json in the same PR. The ratchet tells
// you the exact new numbers in its pass log.
//
// Env contract:
//   HEAD      head name: prometheus | loki | tempo (selects the baseline
//             entry + labels the messages).
//   SCORE     path to that head's compat-score.json (the run's score).
//   BASELINE  path to the committed baseline JSON
//             (default: compatibility/parity-baseline.json).
//
// Exit codes:
//   0  run is at or above baseline on both floors (no regression).
//   1  run dropped below baseline (regression), OR the baseline / score
//      file is missing or malformed (a missing score means the harness
//      never produced one — that is itself a hard failure the separate
//      "Fail job" step also re-raises; failing here keeps the ratchet
//      honest rather than silently passing on absent data).

import { existsSync, readFileSync } from 'node:fs';
import process from 'node:process';
import { error, log, notice } from './lib/gh.mjs';

const DEFAULT_BASELINE = 'compatibility/parity-baseline.json';

const head = process.env.HEAD || '';
const scorePath = process.env.SCORE || '';
const baselinePath = process.env.BASELINE || DEFAULT_BASELINE;

function fail(message, props) {
  error(message, props);
  process.exit(1);
}

if (!head) {
  fail('compat-ratchet: HEAD env var is required (prometheus|loki|tempo)', {
    title: 'compat ratchet misconfigured',
  });
}
if (!scorePath) {
  fail('compat-ratchet: SCORE env var (path to compat-score.json) is required', {
    title: 'compat ratchet misconfigured',
  });
}

function readJson(path, label) {
  if (!existsSync(path)) {
    fail(
      `compat-ratchet: ${label} not found at ${path} — the harness must have ` +
        `failed before producing it; cannot verify parity against baseline`,
      { title: `compatibility/${head} ratchet: missing ${label}` },
    );
  }
  try {
    return JSON.parse(readFileSync(path, 'utf8'));
  } catch (e) {
    fail(`compat-ratchet: could not parse ${label} at ${path}: ${e.message}`, {
      title: `compatibility/${head} ratchet: malformed ${label}`,
    });
  }
  return undefined; // unreachable; fail() exits.
}

const baseline = readJson(baselinePath, 'baseline');
const entry = baseline?.heads?.[head];
if (!entry || typeof entry.passed !== 'number' || typeof entry.total !== 'number') {
  fail(
    `compat-ratchet: no baseline entry for head '${head}' in ${baselinePath} ` +
      `(expected heads.${head}.{passed,total} as numbers)`,
    { title: `compatibility/${head} ratchet: no baseline entry` },
  );
}

const score = readJson(scorePath, 'compat-score.json');
if (typeof score.passed !== 'number' || typeof score.total !== 'number') {
  fail(
    `compat-ratchet: ${scorePath} is missing numeric passed/total fields`,
    { title: `compatibility/${head} ratchet: malformed score` },
  );
}

const { passed: basePassed, total: baseTotal } = entry;
const { passed: runPassed, total: runTotal } = score;

const passedRegressed = runPassed < basePassed;
const totalShrank = runTotal < baseTotal;

if (passedRegressed || totalShrank) {
  const lines = [`compatibility/${head} PARITY REGRESSION vs committed baseline:`];
  if (passedRegressed) {
    lines.push(
      `  passing cases dropped: baseline ${basePassed}, this run ${runPassed} ` +
        `(${basePassed - runPassed} case(s) that used to agree with the reference now diverge)`,
    );
  }
  if (totalShrank) {
    lines.push(
      `  corpus shrank: baseline total ${baseTotal}, this run ${runTotal} ` +
        `(a shrunk corpus can mask a regression by dropping a failing case — rejected)`,
    );
  }
  lines.push(
    `Fix the regression at the source (this is the source of truth for the ${head} head). ` +
      `If the drop is a deliberate, documented corpus change, update ` +
      `${baselinePath} (heads.${head}) in the same PR with the rationale — ` +
      `but never to mask a real parity bug.`,
  );
  fail(lines.join('\n'), { title: `compatibility/${head} parity regression` });
}

// At or above baseline. If the run IMPROVED, nudge the maintainer to
// raise the floor so the ratchet keeps tracking the new best.
if (runPassed > basePassed || runTotal > baseTotal) {
  notice(
    `compatibility/${head} improved over baseline (baseline ${basePassed}/${baseTotal}, ` +
      `this run ${runPassed}/${runTotal}). Bump heads.${head} in ${baselinePath} to ` +
      `{ "passed": ${runPassed}, "total": ${runTotal} } to ratchet the floor up.`,
    { title: `compatibility/${head} ratchet: floor can be raised` },
  );
} else {
  log(
    `compat-ratchet: compatibility/${head} at baseline ` +
      `(${runPassed}/${runTotal} == ${basePassed}/${baseTotal}) — no regression.`,
  );
}

process.exit(0);
