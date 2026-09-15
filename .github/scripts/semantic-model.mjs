// semantic-model.mjs — CLI entry point for `just semantic-check`. Loads and
// validates test/semantic/'s hand-authored JSON semantic contract model
// (schema, ID uniqueness, cross-references, inheritance/replacement cycles,
// and evidence assurance). All the logic lives in
// lib/semantic-model.mjs, which downstream issues (#3427-#3430, #3445,
// #3456) import directly to build head-specific catalogs and reports on top
// of the same validated model rather than re-parsing the files themselves.
//
// Env:
//   SEMANTIC_MODEL_DIR    directory holding the six JSON files (optional;
//                          default test/semantic)
//   GITHUB_STEP_SUMMARY   optional summary destination
//
// Node builtins only. Exit 0 when the model is valid, 1 (with an ::error::
// annotation) on any schema, reference, cycle, or assurance violation.

import process from "node:process";
import { appendFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { resolve } from "node:path";

import {
  DEFAULT_SEMANTIC_MODEL_DIR,
  loadSemanticModel,
  renderSummary,
} from "./lib/semantic-model.mjs";

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
