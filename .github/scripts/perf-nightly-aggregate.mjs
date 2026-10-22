// perf-nightly-aggregate.mjs — the `perf-nightly` job's verdict
// (perf-nightly.yml): the context release.yml's preflight waits on.
//
// perf-nightly.yml runs the nightly sentinels once per ClickHouse server of
// its matrix (`perf-nightly-sentinels (<clickhouse>)`) and the real-data
// replays in their own job (`perf-nightly-replays`). This job passes only
// when both `needs` results are `success`: a matrix job's result is
// `success` only when every leg succeeded, so one red server fails it, and a
// skipped or cancelled leg fails it rather than reading as green.
//
// It also writes each perf-nightly job's wall time of this run attempt to
// the step summary, read from the run's own job list. The table is a report,
// never the verdict: a failure to read it is logged and does not change the
// exit code.
//
// Env:
//   SENTINELS_RESULT  needs.perf-nightly-sentinels.result
//   REPLAYS_RESULT    needs.perf-nightly-replays.result
//   GH_TOKEN          a token with actions: read on REPO
//   REPO              owner/name
//   RUN_ID            github.run_id
//   RUN_ATTEMPT       github.run_attempt
//   GITHUB_API_URL    optional; DEFAULT_API_BASE otherwise
//
// Exit: 0 when both results are success, 1 otherwise.

import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { DEFAULT_API_BASE, ghPaginate } from './lib/gh-api.mjs';
import { appendStepSummary, error, notice, warning } from './lib/gh.mjs';

// JOB_PREFIX selects the jobs this workflow runs out of the run's job list.
const JOB_PREFIX = 'perf-nightly';

const SECOND_MS = 1000;
const MINUTE_S = 60;

// verdict — the failures of the two `needs` results, empty when both passed.
export function verdict({ sentinels, replays }) {
  const failures = [];
  if (sentinels !== 'success') failures.push(`perf-nightly-sentinels: ${sentinels || 'unknown'}`);
  if (replays !== 'success') failures.push(`perf-nightly-replays: ${replays || 'unknown'}`);
  return failures;
}

// formatDuration — whole seconds as "<m>m <s>s".
export function formatDuration(seconds) {
  const s = Math.round(seconds);
  return `${Math.floor(s / MINUTE_S)}m ${s % MINUTE_S}s`;
}

// wallTimeTable — a markdown table of each finished perf-nightly job's wall
// time (started_at to completed_at), sorted by name. The aggregate job
// itself, still running, has no completed_at and is left out.
export function wallTimeTable(jobs) {
  const rows = jobs
    .filter((j) => j.name.startsWith(JOB_PREFIX) && j.started_at && j.completed_at)
    .map((j) => ({
      name: j.name,
      conclusion: j.conclusion,
      seconds: (Date.parse(j.completed_at) - Date.parse(j.started_at)) / SECOND_MS,
    }))
    .sort((a, b) => a.name.localeCompare(b.name));
  return [
    '### perf-nightly wall time per leg',
    '',
    '| job | conclusion | wall time |',
    '|---|---|---|',
    ...rows.map((r) => `| ${r.name} | ${r.conclusion} | ${formatDuration(r.seconds)} |`),
    '',
  ].join('\n');
}

async function reportWallTimes(env) {
  const api = env.GITHUB_API_URL || DEFAULT_API_BASE;
  const url = `${api}/repos/${env.REPO}/actions/runs/${env.RUN_ID}/attempts/${env.RUN_ATTEMPT}/jobs`;
  const jobs = await ghPaginate({
    url,
    token: env.GH_TOKEN,
    what: "list this run attempt's jobs",
    pick: (page) => page.jobs ?? [],
  });
  appendStepSummary(wallTimeTable(jobs));
}

async function main() {
  const failures = verdict({ sentinels: process.env.SENTINELS_RESULT, replays: process.env.REPLAYS_RESULT });
  try {
    await reportWallTimes(process.env);
  } catch (e) {
    warning(`perf-nightly-aggregate: could not read the legs' wall times: ${e.message}`);
  }
  if (failures.length > 0) {
    for (const f of failures) error(`perf-nightly: ${f}`);
    process.exit(1);
  }
  notice('perf-nightly: every sentinel leg and the replays succeeded');
}

const invokedDirectly = process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href;
if (invokedDirectly) {
  main().catch((e) => {
    error(`perf-nightly-aggregate failed: ${e.message}`);
    process.exit(1);
  });
}
