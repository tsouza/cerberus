// compat-step-summary.mjs — emit the one-row parity-score markdown table
// to $GITHUB_STEP_SUMMARY, extracted from the THREE identical "Append
// score to step summary" steps in .github/workflows/compatibility.yml
// (prometheus / tempo / loki).
//
// Reproduces the original bash exactly: read percent / passed / total
// from the head's compat-score.json (when present) and append a markdown
// table; when the file is absent, print the same "no compat-score.json"
// notice and exit 0 (the step ran under `if: always()` and must not fail
// the job — the separate "Fail job" step re-raises the real failure).
//
// Env contract:
//   HEAD    head name for the table row + section heading (prometheus|tempo|loki)
//   SCORE   path to that head's compat-score.json
//
// Exit codes: always 0 (housekeeping step; never gates).

import { existsSync, readFileSync } from 'node:fs';
import process from 'node:process';
import { appendStepSummary, error, log } from './lib/gh.mjs';

const head = process.env.HEAD || '';
const scorePath = process.env.SCORE || '';

if (!head || !scorePath) {
  error('compat-step-summary.mjs: HEAD and SCORE env vars are required');
  // Match the housekeeping contract: do not fail the job on a wiring slip.
  process.exit(0);
}

if (!existsSync(scorePath)) {
  log('no compat-score.json produced (harness step likely failed before scorer ran)');
  process.exit(0);
}

let score;
try {
  score = JSON.parse(readFileSync(scorePath, 'utf8'));
} catch (e) {
  log(`could not parse ${scorePath}: ${e.message}`);
  process.exit(0);
}

const { percent, passed, total } = score;

const summary = [
  `### compatibility/${head}`,
  '',
  '| head | passed/total | percent |',
  '|------|--------------|---------|',
  `| ${head} | ${passed}/${total} | ${percent}% |`,
  '',
].join('\n');

appendStepSummary(summary);
process.exit(0);
