// perf-nightly-aggregate.mjs — the `perf-nightly` job's verdict
// (perf-nightly.yml): the context release.yml's preflight waits on.
//
// perf-nightly.yml runs the nightly sentinels once per ClickHouse server of
// its matrix (`perf-nightly-sentinels (<clickhouse>)`) and the real-data
// replays in their own job (`perf-nightly-replays`). This job passes only
// when both `needs` results are `success`: a matrix job's result is
// `success` only when every leg succeeded, so one red server fails it, and a
// skipped or cancelled leg fails it rather than reading as green. The one
// exception is a recalibration dispatch (CALIBRATE=true), which skips the
// replays by design.
//
// It also writes each perf-nightly job's wall time of this run attempt to
// the step summary, read from the run's own job list. The table is a report,
// never the verdict: a failure to read it is logged and does not change the
// exit code.
//
// On a recalibration dispatch every sentinel leg measures one runner's
// rounds and uploads them as nightly-rounds-<clickhouse>-<runner>; this job
// downloads them all under ROUNDS_DIR and combines them into one bound file
// at BASELINE_OUT (test/perf/nightly/calibrate, which derives each bound from
// every runner's rounds of its server through calibratedBound). The step
// output `written` says whether that file exists to upload, which it does
// also when the combine refused a sentinel (it kept its committed bound); a
// refusal, a combine that wrote nothing, and a leg of the calibrate matrix
// (lib/perf-nightly-matrix.mjs) whose rounds are missing each fail the
// verdict.
//
// Env:
//   SENTINELS_RESULT  needs.perf-nightly-sentinels.result
//   REPLAYS_RESULT    needs.perf-nightly-replays.result
//   CALIBRATE         "true" on a recalibration dispatch
//   GH_TOKEN          a token with actions: read on REPO
//   REPO              owner/name
//   RUN_ID            github.run_id
//   RUN_ATTEMPT       github.run_attempt
//   GITHUB_API_URL    optional; DEFAULT_API_BASE otherwise
//   ROUNDS_DIR        CALIBRATE only: where every leg's rounds artifact was downloaded
//   BASELINE_OUT      CALIBRATE only: where the combined bound file is written
//   GITHUB_OUTPUT     receives `written` (true/false) on a recalibration
//
// Exit: 0 when both results are success (and, recalibrating, the combine
// wrote the bound file with no refusal), 1 otherwise.

import { existsSync, readdirSync, readFileSync, rmSync } from 'node:fs';
import { basename, dirname, join } from 'node:path';
import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { DEFAULT_API_BASE, ghPaginate } from './lib/gh-api.mjs';
import { appendStepSummary, capture, error, notice, setOutput, warning } from './lib/gh.mjs';
import {
  PERF_NIGHTLY_JOB,
  PERF_NIGHTLY_WORKFLOW,
  readCalibrationRunners,
  readClickHouseMatrix,
} from './lib/perf-nightly-matrix.mjs';

// JOB_PREFIX selects the jobs this workflow runs out of the run's job list.
const JOB_PREFIX = 'perf-nightly';

const SECOND_MS = 1000;
const MINUTE_S = 60;

// COMBINE_PACKAGE combines every runner's rounds into the committed bound
// file, which it reads from the checkout.
export const COMBINE_PACKAGE = './test/perf/nightly/calibrate';

// ROUNDS_ARTIFACT_PREFIX names each calibration leg's rounds artifact,
// `nightly-rounds-<clickhouse>-<runner>` (perf-nightly.yml).
export const ROUNDS_ARTIFACT_PREFIX = 'nightly-rounds-';

// roundsFiles — every rounds file under dir, sorted: download-artifact puts
// each artifact in its own directory, named after it. A missing dir has none.
export function roundsFiles(dir) {
  if (!dir || !existsSync(dir)) return [];
  return readdirSync(dir, { recursive: true })
    .filter((rel) => String(rel).endsWith('.json'))
    .map((rel) => join(dir, String(rel)))
    .sort();
}

// missingLegs — the rounds artifacts the calibrate matrix (servers x
// runners) should have uploaded and files does not hold, by name. A server
// with no leg at all is otherwise silent: the combine leaves its bound set as
// it is, and the refusals it reports are per sentinel of a server it saw.
export function missingLegs(files, servers, runners) {
  const uploaded = new Set(files.map((f) => basename(dirname(f))));
  return servers.flatMap((server) =>
    runners.map((runner) => `${ROUNDS_ARTIFACT_PREFIX}${server}-${runner}`).filter((name) => !uploaded.has(name)),
  );
}

// combineArgs — the `go` argv that combines files into out.
export function combineArgs(files, out) {
  return ['run', COMBINE_PACKAGE, '-out', out, ...files];
}

// calibrationFailures — what fails a recalibration's verdict: a leg whose
// rounds are missing, no rounds to combine, or a combine that exited non-zero
// (a refused sentinel, or no file written at all — `written` tells the two
// apart).
export function calibrationFailures({ files, missing, status, written }) {
  const failures = missing.map((name) => `calibration: no ${name} rounds were uploaded`);
  if (files.length === 0) return [...failures, 'calibration: no leg uploaded calibration rounds'];
  if (status === 0) return failures;
  failures.push(
    written
      ? 'calibration: the combine refused at least one sentinel, which kept its committed bound (see its ::error:: lines)'
      : 'calibration: the combine wrote no bound file',
  );
  return failures;
}

// runGoCombine — run the combine over files into out; its output streams to
// this job's log.
function runGoCombine(files, out) {
  const res = capture('go', combineArgs(files, out));
  process.stdout.write(res.stdout);
  process.stderr.write(res.stderr);
  return res.status;
}

// combineRounds — combine every rounds file under env.ROUNDS_DIR into
// env.BASELINE_OUT, set the `written` output, and return the failures.
// workflowText is perf-nightly.yml, whose calibrate matrix names the legs
// expected; combine runs the combine and returns its exit status.
export function combineRounds(env, { workflowText, combine = runGoCombine, output = setOutput } = {}) {
  const out = env.BASELINE_OUT;
  if (!out) return ['calibration: BASELINE_OUT names no file to write'];
  const servers = readClickHouseMatrix(workflowText, PERF_NIGHTLY_JOB);
  const runners = readCalibrationRunners(workflowText);
  if (!servers || !runners) return [`calibration: ${PERF_NIGHTLY_WORKFLOW} names no calibrate matrix to expect legs from`];
  rmSync(out, { force: true });
  const files = roundsFiles(env.ROUNDS_DIR);
  const status = files.length > 0 ? combine(files, out) : 0;
  const written = existsSync(out);
  output('written', String(written));
  return calibrationFailures({ files, missing: missingLegs(files, servers, runners), status, written });
}

// verdict — the failures of the two `needs` results, empty when both passed.
export function verdict({ sentinels, replays, calibrate }) {
  const failures = [];
  if (sentinels !== 'success') failures.push(`perf-nightly-sentinels: ${sentinels || 'unknown'}`);
  const replaysOK = replays === 'success' || (calibrate && replays === 'skipped');
  if (!replaysOK) failures.push(`perf-nightly-replays: ${replays || 'unknown'}`);
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
  const calibrate = process.env.CALIBRATE === 'true';
  const failures = verdict({
    sentinels: process.env.SENTINELS_RESULT,
    replays: process.env.REPLAYS_RESULT,
    calibrate,
  });
  if (calibrate) {
    failures.push(...combineRounds(process.env, { workflowText: readFileSync(PERF_NIGHTLY_WORKFLOW, 'utf8') }));
  }
  try {
    await reportWallTimes(process.env);
  } catch (e) {
    warning(`perf-nightly-aggregate: could not read the legs' wall times: ${e.message}`);
  }
  if (failures.length > 0) {
    for (const f of failures) error(`perf-nightly: ${f}`);
    process.exit(1);
  }
  notice(
    calibrate
      ? 'perf-nightly: every calibration leg succeeded and the combine wrote the recalibrated bound file'
      : 'perf-nightly: every sentinel leg and the replays succeeded',
  );
}

const invokedDirectly = process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href;
if (invokedDirectly) {
  main().catch((e) => {
    error(`perf-nightly-aggregate failed: ${e.message}`);
    process.exit(1);
  });
}
