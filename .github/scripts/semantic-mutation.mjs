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
// every exit path, including SIGINT/SIGTERM.

import { appendFileSync, rmSync } from "node:fs";
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
import { error, log, notice } from "./lib/gh.mjs";

function parseArgs(argv) {
  let mutantId = null;
  let detectorId = null;
  for (let i = 0; i < argv.length; i += 1) {
    const arg = argv[i];
    if (arg === "--detector") {
      detectorId = argv[i + 1] ?? null;
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
  // never double-removes.
  const onSignal = (signal, exitCode) => {
    cleanup();
    notice(`semantic-mutation: cleaned up ${scratchDir} after ${signal}`);
    process.exit(exitCode);
  };
  process.on("SIGINT", () => onSignal("SIGINT", 130));
  process.on("SIGTERM", () => onSignal("SIGTERM", 143));

  let result;
  try {
    result = await runMutant({ record, root, scratchDir, detectorId });
  } finally {
    cleanup();
  }

  const matched = result.status === record.expected_detection;
  log(JSON.stringify(result, null, 2));
  appendSummary(
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
}

const invokedDirectly = process.argv[1] && fileURLToPath(import.meta.url) === resolve(process.argv[1]);
if (invokedDirectly) {
  main().catch((cause) => {
    error(`semantic-mutation: ${cause instanceof Error ? cause.message : String(cause)}`);
    process.exitCode = 1;
  });
}
