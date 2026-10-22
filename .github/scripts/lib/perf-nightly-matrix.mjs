// perf-nightly-matrix.mjs — the ClickHouse servers perf-nightly runs on.
//
// perf-nightly.yml runs its sentinels once per server of the
// `perf-nightly-sentinels` job's matrix, and perf-nightly-selfcheck.yml runs
// one self-check leg per server of the same list. Both declare it as the
// one-line flow sequence `clickhouse: ['24.8', '25.9', '26.6']` under
// `strategy.matrix`; this module reads it back for the checks that hold the
// list to its sources (clickhouse-version-sync.mjs check h) and to the
// self-check's mutations (perf-nightly-selfcheck.test.mjs).
//
// Exports:
//   PERF_NIGHTLY_WORKFLOW / PERF_NIGHTLY_JOB
//   SELFCHECK_WORKFLOW / SELFCHECK_JOB
//                         where each matrix lives.
//   readClickHouseMatrix(workflowText, jobName)
//                         the job's `clickhouse` matrix values, in order, or
//                         null when the job or the line is missing.

export const PERF_NIGHTLY_WORKFLOW = '.github/workflows/perf-nightly.yml';
export const PERF_NIGHTLY_JOB = 'perf-nightly-sentinels';
export const SELFCHECK_WORKFLOW = '.github/workflows/perf-nightly-selfcheck.yml';
export const SELFCHECK_JOB = 'perf-nightly-selfcheck';

// jobBlock — one top-level job's text, bounded by the next 2-space-indented
// `key:` line (or EOF). cerberus's workflows indent jobs by two spaces.
function jobBlock(text, jobName) {
  const re = new RegExp(`\\n  ${jobName}:\\n([\\s\\S]*?)(?=\\n  [A-Za-z0-9_-]+:\\n|$)`);
  const m = re.exec(text);
  return m ? m[1] : null;
}

export function readClickHouseMatrix(workflowText, jobName) {
  const block = jobBlock(workflowText, jobName);
  if (block === null) return null;
  const m = /\n\s+clickhouse:\s*\[([^\]\n]*)\]/.exec(block);
  if (!m) return null;
  return m[1]
    .split(',')
    .map((v) => v.trim().replace(/^['"]|['"]$/g, ''))
    .filter((v) => v.length > 0);
}
