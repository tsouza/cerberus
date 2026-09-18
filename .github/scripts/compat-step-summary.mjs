// compat-step-summary.mjs — the per-head housekeeping after a compatibility
// harness ran: log the tally of the raw report (prometheus lanes) and append
// the one-row parity-score markdown table to $GITHUB_STEP_SUMMARY.
//
// One script for every lane in .github/workflows/compatibility.yml. The three
// prometheus lanes (prometheus, prometheus-forced-route, prometheus-floor)
// used to carry an identical inline `jq` "Summarise report" step, and the
// forced-route / floor lanes an inline bash copy of the table this script
// already rendered for the other heads — invariant 15 (no inline parsing or
// branching in a workflow step) and one copy of the table shape.
//
// Env contract:
//   HEAD    (required) the head name: the table row label unless LABEL is set,
//           and the section heading unless TITLE is set (prometheus|tempo|loki)
//   SCORE   (required) path to that head's compat-score.json
//   TITLE   (optional) the section heading, for a lane that is a variant of a
//           head (`compatibility/prometheus-floor (ClickHouse 24.8)`)
//   LABEL   (optional) the table row label (`prometheus (floor)`)
//   REPORT  (optional) path to the upstream tester's raw report.json; when
//           set, its tally — total / passed / diffs / unexpected failures —
//           is logged, or a notice when the file is absent
//
// Exit codes: always 0 (housekeeping step; never gates — the separate "Fail
// job" step re-raises the harness's real failure).

import { existsSync, readFileSync } from 'node:fs';
import process from 'node:process';
import { fileURLToPath } from 'node:url';
import { resolve } from 'node:path';

import { appendStepSummary, error, log } from './lib/gh.mjs';

// reportTally — the upstream compatibility tester's report, partitioned.
// The tester encodes "no error" as an EMPTY STRING, not JSON null (`"diff":
// ""` / `"unexpectedFailure": ""` for a passing query); both are read as
// "nothing", so a pass is a result with neither.
export function reportTally(report) {
  const results = Array.isArray(report?.results) ? report.results : [];
  const text = (v) => (v == null ? '' : String(v));
  const failed = (r) => text(r?.unexpectedFailure) !== '';
  const differs = (r) => text(r?.diff) !== '';
  return {
    total: results.length,
    passed: results.filter((r) => !failed(r) && !differs(r)).length,
    diffs: results.filter(differs).length,
    unexpected_failures: results.filter(failed).length,
  };
}

// summaryTable — the markdown block appended to the step summary.
export function summaryTable({ title, label, passed, total, percent }) {
  return [
    `### ${title}`,
    '',
    '| head | passed/total | percent |',
    '|------|--------------|---------|',
    `| ${label} | ${passed}/${total} | ${percent}% |`,
    '',
  ].join('\n');
}

function main() {
  const head = process.env.HEAD || '';
  const scorePath = process.env.SCORE || '';
  const title = process.env.TITLE || `compatibility/${head}`;
  const label = process.env.LABEL || head;
  const reportPath = process.env.REPORT || '';

  if (!head || !scorePath) {
    error('compat-step-summary.mjs: HEAD and SCORE env vars are required');
    // Match the housekeeping contract: do not fail the job on a wiring slip.
    return;
  }

  if (reportPath) {
    if (!existsSync(reportPath)) {
      log('no report produced (harness step likely failed before tester ran)');
    } else {
      try {
        log(JSON.stringify(reportTally(JSON.parse(readFileSync(reportPath, 'utf8'))), null, 2));
      } catch (e) {
        log(`could not parse ${reportPath}: ${e.message}`);
      }
    }
  }

  if (!existsSync(scorePath)) {
    log('no compat-score.json produced (harness step likely failed before scorer ran)');
    return;
  }

  let score;
  try {
    score = JSON.parse(readFileSync(scorePath, 'utf8'));
  } catch (e) {
    log(`could not parse ${scorePath}: ${e.message}`);
    return;
  }

  const { percent, passed, total } = score;
  appendStepSummary(summaryTable({ title, label, passed, total, percent }));
}

const invokedDirectly = process.argv[1] && fileURLToPath(import.meta.url) === resolve(process.argv[1]);
if (invokedDirectly) {
  main();
  process.exit(0);
}
