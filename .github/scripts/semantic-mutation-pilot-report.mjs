#!/usr/bin/env node
// semantic-mutation-pilot-report.mjs — CLI entry point for the SCHEDULED,
// informational semantic-mutation pilot lane
// (quality.semantic-mutation-pilot, .github/ci-lanes.json;
// .github/workflows/semantic-mutation-pilot.yml; cerberus issue #3452).
//
// Runs every REAL (non-synthetic) mutant record for real, in-process via
// lib/semantic-mutation.mjs's own runMutant — the SAME execution engine
// `just semantic-mutate` / semantic-mutation-corpus.mjs already drive, never
// a duplicated re-implementation — and prints each run's own revision-bound
// observation as a ready-to-review test/semantic/mutant-executions.json
// entry to the job summary. This script NEVER writes that ledger itself:
// exactly like test/semantic/executions.json's own adapter
// (lib/semantic-execution-adapter.mjs's header says the ledger stays
// hand-authored/reviewed), a human reads this summary and appends what it
// finds worth keeping — the same discipline, applied to the mutation pilot.
//
// INFORMATIONAL, NOT A GATE. This lane's own declared posture
// (.github/ci-lanes.json's quality.semantic-mutation-pilot:
// merge_posture "never", main_posture "never", release_posture "advisory")
// means it can never become a required status check, so it structurally
// cannot make a failed required `mutation` (gremlins) lane — or the
// REQUIRED ci.check lane's own semantic-mutation-corpus.mjs step, which
// already gates every record's expected_detection on every PR — look green.
// This script therefore never fails the workflow merely because an observed
// classification disagrees with a record's declared expected_detection; it
// only exits non-zero on a genuine script/harness error (a load failure, an
// exception inside runMutant itself).
//
// Usage: node .github/scripts/semantic-mutation-pilot-report.mjs
// Env: SEMANTIC_MUTANTS_DIR (optional), RUNNER_TEMP (optional),
//      GITHUB_STEP_SUMMARY (optional), GITHUB_SHA/GITHUB_SERVER_URL/
//      GITHUB_REPOSITORY/GITHUB_RUN_ID (optional — populated automatically
//      by GitHub Actions; absent locally, where source_sha/run_ref simply
//      report null/a placeholder rather than failing the run).

import { rmSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { resolve } from "node:path";
import process from "node:process";

import {
  DEFAULT_MUTANTS_DIR,
  createScratchDir,
  loadMutants,
  runMutant,
  scratchRootFor,
} from "./lib/semantic-mutation.mjs";
import { defaultRunRef, githubRunContext } from "./lib/semantic-execution-adapter.mjs";
import { appendStepSummary, error, log, notice } from "./lib/gh.mjs";
import { stampedRecordId } from "./lib/semantic-model.mjs";

function appendSummary(body) {
  appendStepSummary(body, { quiet: true });
}

// The mutant-ledger id namespace (MUTEXEC-, not EXEC-), stamped the same
// way lib/semantic-execution-adapter.mjs's execIdFor stamps executions.json
// ids — one shared stampedRecordId, two prefixes.
function mutantExecIdFor(mutantId, observedAt) {
  return stampedRecordId("MUTEXEC", mutantId.replace(/^MUTANT-/, ""), observedAt);
}

export async function runPilot({
  root = process.cwd(),
  dir = DEFAULT_MUTANTS_DIR,
  env = process.env,
  runMutantFn = runMutant,
} = {}) {
  const records = loadMutants(dir, { root });
  const real = [...records.values()]
    .filter((r) => !r.synthetic)
    .sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));

  const run = githubRunContext(env);
  const runRef = defaultRunRef(env) ?? "(no run_ref available — not running under GitHub Actions)";
  const observedAt = new Date().toISOString();

  const entries = [];
  let mismatches = 0;

  for (const record of real) {
    const scratchDir = createScratchDir(scratchRootFor(env));
    let result;
    try {
      // eslint-disable-next-line no-await-in-loop -- sequential by design, mirroring semantic-mutation-corpus.mjs's own runCorpus: one mutant at a time, so a stuck detector's wall-clock budget is never shared with a sibling.
      result = await runMutantFn({ record, root, scratchDir });
    } finally {
      rmSync(scratchDir, { recursive: true, force: true });
    }
    const matched = result.status === record.expected_detection;
    if (!matched) mismatches += 1;
    log(
      `${record.id}: observed ${result.status}, declared ${record.expected_detection} — ` +
        `${matched ? "match" : "MISMATCH (informational — does not fail this lane)"}`,
    );
    entries.push({
      id: mutantExecIdFor(record.id, observedAt),
      mutant: record.id,
      observed_at: observedAt,
      status: result.status,
      run_ref: runRef,
      source_sha: run.sourceSha,
      detectors: (result.mutant_runs ?? []).map((r) => ({
        id: r.detector,
        classification: r.classification,
        duration_ms: r.durationMs ?? null,
      })),
    });
  }

  return { total: real.length, mismatches, entries };
}

async function main() {
  const root = process.cwd();
  const dir = process.env.SEMANTIC_MUTANTS_DIR || DEFAULT_MUTANTS_DIR;
  const { total, mismatches, entries } = await runPilot({ root, dir });

  const summaryBody =
    `## Semantic mutation pilot (informational — not a merge/release gate)\n\n` +
    `Ran **${total}** real, per-head record(s) end to end. **${mismatches}** disagreed with their own ` +
    `declared \`expected_detection\` (informational only — the REQUIRED \`ci.check\` lane's ` +
    `\`semantic-mutation-corpus.mjs\` step already gates this on every PR).\n\n` +
    "Suggested `test/semantic/mutant-executions.json` entries (review before appending — this script " +
    "never writes the ledger itself):\n\n```json\n" +
    JSON.stringify(entries, null, 2) +
    "\n```\n";
  appendSummary(summaryBody);
  notice(`semantic-mutation-pilot: ran ${total} real record(s), ${mismatches} disagreement(s) (informational)`);
}

const invokedDirectly = process.argv[1] && fileURLToPath(import.meta.url) === resolve(process.argv[1]);
if (invokedDirectly) {
  main().catch((cause) => {
    error(`semantic-mutation-pilot: ${cause instanceof Error ? cause.message : String(cause)}`);
    process.exitCode = 1;
  });
}
