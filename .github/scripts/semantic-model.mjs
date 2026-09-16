// semantic-model.mjs — CLI entry point for `just semantic-check`. Loads and
// validates test/semantic/'s hand-authored JSON semantic contract model
// (schema, ID uniqueness, cross-references, inheritance/replacement cycles,
// and evidence assurance), then loads and validates
// test/semantic/counterexamples/*.json (issue #3445) against that same
// model — the historical-bug records join a source issue, its fix, the
// contract(s) it violated, its discovery mechanism, and its current replay
// owner into one record, and their contract/binding references, locator
// paths and replay-test paths are checked against the live model and the
// live filesystem here, not merely parsed. All the logic lives in
// lib/semantic-model.mjs (the six-file model) and
// lib/semantic-counterexamples.mjs (the counterexample records), which
// downstream issues (#3427-#3430, #3456) import directly to build
// head-specific catalogs and reports on top of the same validated model
// rather than re-parsing the files themselves.
//
// Env:
//   SEMANTIC_MODEL_DIR             directory holding the six JSON files
//                                   (optional; default test/semantic)
//   SEMANTIC_COUNTEREXAMPLES_DIR   directory holding the counterexample
//                                   records (optional; default
//                                   test/semantic/counterexamples)
//   GITHUB_STEP_SUMMARY            optional summary destination
//
// Node builtins only. Exit 0 when the model and its counterexample records
// are valid, 1 (with an ::error:: annotation) on any schema, reference,
// cycle, or assurance violation.

import process from "node:process";
import { appendFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { resolve } from "node:path";

import {
  DEFAULT_SEMANTIC_MODEL_DIR,
  loadSemanticModel,
  renderSummary,
} from "./lib/semantic-model.mjs";
import {
  DEFAULT_COUNTEREXAMPLES_DIR,
  loadCounterexamples,
  renderCounterexamplesSummary,
} from "./lib/semantic-counterexamples.mjs";

function appendSummary(body) {
  const path = process.env.GITHUB_STEP_SUMMARY;
  if (path) appendFileSync(path, body);
}

function errorAnnotation(message) {
  const oneLine = message
    .replaceAll("%", "%25")
    .replaceAll("\r", "%0D")
    .replaceAll("\n", "%0A");
  process.stderr.write(`::error title=Semantic contract model::${oneLine}\n`);
}

function main() {
  const dir = process.env.SEMANTIC_MODEL_DIR || DEFAULT_SEMANTIC_MODEL_DIR;
  const model = loadSemanticModel(dir, { root: process.cwd() });
  const summary = renderSummary(model);
  process.stdout.write(
    `semantic-model: ${model.contracts.size} contracts (${model.assurance.assured.length} assured) across ${model.heads.size} heads are structurally valid\n`,
  );
  appendSummary(summary);

  const counterexamplesDir =
    process.env.SEMANTIC_COUNTEREXAMPLES_DIR || DEFAULT_COUNTEREXAMPLES_DIR;
  const counterexamples = loadCounterexamples(model, counterexamplesDir, { root: process.cwd() });
  const counterexamplesSummary = renderCounterexamplesSummary(counterexamples);
  process.stdout.write(
    `semantic-counterexamples: ${counterexamples.size} historical-bug records are structurally valid\n`,
  );
  appendSummary(`${counterexamplesSummary}\n`);
}

const invokedDirectly =
  process.argv[1] && fileURLToPath(import.meta.url) === resolve(process.argv[1]);
if (invokedDirectly) {
  try {
    main();
  } catch (error) {
    errorAnnotation(error instanceof Error ? error.message : String(error));
    process.exit(1);
  }
}
