#!/usr/bin/env node
// semantic-mutation.mjs — CLI entry point for `just semantic-mutate
// <mutant-id>`. Loads one record from test/semantic/mutants/ (issue #3448),
// runs the full isolated-execution protocol against it, prints the result
// as JSON, and exits 0 only when the observed classification matches the
// record's own declared expected_detection — the same "did this regress"
// posture .github/scripts/gremlins-threshold.mjs and
// forbid-contradicted-mutants.mjs already apply to the unrelated gremlins
// lane. All execution logic lives in lib/semantic-mutation.mjs; this file is
// argv parsing, process lifecycle (scratch dir + signal cleanup) and output.
//
// Usage:
//   node .github/scripts/semantic-mutation.mjs <mutant-id> [--detector <id>]
//
// Env:
//   SEMANTIC_MUTANTS_DIR   directory holding the *.json records
//                          (optional; default test/semantic/mutants)
//   RUNNER_TEMP            scratch base directory (optional; falls back to
//                          the OS temp dir — see scratchRootFor)
//   GITHUB_STEP_SUMMARY    optional summary destination
//
// Exit codes: 0 when the observed classification matches expected_detection;
// 1 on a mismatch, a usage error (unknown mutant/detector, zero detectors
// selected), or a caught exception. Cleanup of the scratch directory runs on
// every exit path, including SIGINT/SIGTERM — but only AFTER the result is
// printed: the printed JSON's own scratch_dir/overlay_path fields must name
// paths that still exist at the moment they are printed, not ones already
// removed by an earlier cleanup() call.

import { appendFileSync, rmSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { resolve } from "node:path";
import process from "node:process";

import {
  DEFAULT_MUTANTS_DIR,
  createScratchDir,
  loadMutants,
  renderMutantsSummary,
  runMutant,
  scratchRootFor,
} from "./lib/semantic-mutation.mjs";
import { error, log, notice } from "./lib/gh.mjs";

// SIGINT/SIGTERM exit codes follow the POSIX convention of 128 + signal
// number (SIGINT = 2, SIGTERM = 15) that most shells and CI runners already
// interpret this way — named here so neither is a bare magic number.
const SIGINT_EXIT_CODE = 130;
const SIGTERM_EXIT_CODE = 143;

function parseArgs(argv) {
  let mutantId = null;
  let detectorId = null;
  for (let i = 0; i < argv.length; i += 1) {
    const arg = argv[i];
    if (arg === "--detector") {
      const value = argv[i + 1];
      if (value === undefined) {
        throw new Error("--detector requires a value");
      }
      detectorId = value;
      i += 1;
    } else if (!arg.startsWith("-") && mutantId === null) {
      mutantId = arg;
    } else {
      throw new Error(`unrecognised argument: ${arg}`);
    }
  }
  if (mutantId === null) {
    throw new Error("usage: semantic-mutation.mjs <mutant-id> [--detector <id>]");
  }
  return { mutantId, detectorId };
}

function appendSummary(body) {
  const path = process.env.GITHUB_STEP_SUMMARY;
  if (path) appendFileSync(path, body);
}

async function main() {
  const { mutantId, detectorId } = parseArgs(process.argv.slice(2));
  const root = process.cwd();
  const dir = process.env.SEMANTIC_MUTANTS_DIR || DEFAULT_MUTANTS_DIR;

  const records = loadMutants(dir, { root });
  const record = records.get(mutantId);
  if (!record) {
    throw new Error(
      `no mutant record named ${mutantId} under ${dir} (known: ${[...records.keys()].join(", ")})`,
    );
  }

  const scratchDir = createScratchDir(scratchRootFor());
  let cleanedUp = false;
  const cleanup = () => {
    if (cleanedUp) return;
    cleanedUp = true;
    rmSync(scratchDir, { recursive: true, force: true });
  };
  // Cleanup on interruption: a SIGINT/SIGTERM during the run must not leave
  // the scratch directory behind. Registered before any async work starts,
  // and idempotent so an ordinary completion's own cleanup() call below
  // never double-removes. There is no await between scratchDir's creation
  // above and these registrations, so there is no window where a signal
  // could arrive with a scratch directory that exists but no handler yet
  // armed to clean it up.
  const onSignal = (signal, exitCode) => {
    cleanup();
    notice(`semantic-mutation: cleaned up ${scratchDir} after ${signal}`);
    process.exit(exitCode);
  };
  process.on("SIGINT", () => onSignal("SIGINT", SIGINT_EXIT_CODE));
  process.on("SIGTERM", () => onSignal("SIGTERM", SIGTERM_EXIT_CODE));

  try {
    const result = await runMutant({ record, root, scratchDir, detectorId });
    const matched = result.status === record.expected_detection;

    // Printed and summarized BEFORE cleanup runs (in the finally below), so
    // scratch_dir/overlay_path in the JSON and the memory-ledger evidence
    // already folded into each detector run both still name paths that
    // exist at the moment a reader sees them.
    log(JSON.stringify(result, null, 2));
    appendSummary(
      `${renderMutantsSummary(records)}\n` +
        `- \`${result.mutant_id}\`: observed **${result.status}**, expected **${record.expected_detection}** ` +
        `— ${matched ? "match" : "MISMATCH"}\n`,
    );

    if (matched) {
      notice(
        `semantic-mutation: ${result.mutant_id} classified ${result.status}, matching its declared expected_detection`,
      );
      process.exitCode = 0;
    } else {
      error(
        `semantic-mutation: ${result.mutant_id} classified ${result.status}, expected ${record.expected_detection} ` +
          `(reason: ${result.reason ?? "n/a"}; detail: ${result.detail ?? "n/a"})`,
      );
      process.exitCode = 1;
    }
  } finally {
    cleanup();
  }
}

const invokedDirectly = process.argv[1] && fileURLToPath(import.meta.url) === resolve(process.argv[1]);
if (invokedDirectly) {
  main().catch((cause) => {
    error(`semantic-mutation: ${cause instanceof Error ? cause.message : String(cause)}`);
    process.exitCode = 1;
  });
}
