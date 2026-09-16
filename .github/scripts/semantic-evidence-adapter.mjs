#!/usr/bin/env node
// semantic-evidence-adapter.mjs — CLI entry point for the semantic evidence
// adapter (.github/scripts/lib/semantic-evidence-adapter.mjs, cerberus issue
// #3428). Loads the semantic contract model (test/semantic/) plus the five
// evidence inventories it can resolve bindings against, and reports every
// binding whose test_ref dangles: names an identity that no longer exists
// in the property-shape rosters, the TXTAR spec corpus, the surface-parity
// inventory, the rejection-parity catalogue, or the QL feature inventory.
//
// Env:
//   SEMANTIC_MODEL_DIR   directory holding the six semantic model JSON
//                        files (optional; default test/semantic — see
//                        lib/semantic-model.mjs)
//   GITHUB_STEP_SUMMARY  optional summary destination
//
// Node builtins only, plus a `go run` shell-out for the one Go export
// helper (test/semantic/cmd/property-shape-export). Exit 0 when every
// resolvable binding resolves; 1 (with an ::error:: annotation per dangling
// binding) otherwise.

import process from "node:process";
import { appendFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { resolve } from "node:path";

import { DEFAULT_SEMANTIC_MODEL_DIR, loadSemanticModel } from "./lib/semantic-model.mjs";
import {
  classifyTestRef,
  diagnoseDanglingBindings,
  loadOracleInventory,
  loadPropertyShapeExport,
  loadRejectionParityCatalogue,
  loadSurfaceParityInventory,
} from "./lib/semantic-evidence-adapter.mjs";

function appendSummary(body) {
  const path = process.env.GITHUB_STEP_SUMMARY;
  if (path) appendFileSync(path, body);
}

function errorAnnotation(message) {
  const oneLine = message.replaceAll("%", "%25").replaceAll("\r", "%0D").replaceAll("\n", "%0A");
  process.stderr.write(`::error title=Semantic evidence adapter::${oneLine}\n`);
}

function main() {
  const repoRoot = process.cwd();
  const modelDir = process.env.SEMANTIC_MODEL_DIR || DEFAULT_SEMANTIC_MODEL_DIR;
  const model = loadSemanticModel(modelDir, { root: repoRoot });

  const indices = {
    repoRoot,
    shapeExport: loadPropertyShapeExport({ repoRoot }),
    surfaceParityEntries: loadSurfaceParityInventory(repoRoot),
    rejectionCatalogueEntries: loadRejectionParityCatalogue(repoRoot),
    oracleInventory: loadOracleInventory(repoRoot),
  };

  const problems = diagnoseDanglingBindings(model, indices);
  const resolvable = [...model.bindings.values()].filter(
    (b) => classifyTestRef(b.test_ref).system !== null,
  ).length;

  const summary =
    `## Semantic evidence adapter\n\n` +
    `- bindings checked against the five identity systems: **${resolvable}**\n` +
    `- dangling: **${problems.length}**\n`;
  appendSummary(summary);

  if (problems.length > 0) {
    for (const p of problems) errorAnnotation(p);
    process.stderr.write(`semantic-evidence-adapter: ${problems.length} dangling binding(s)\n`);
    process.exit(1);
  }
  process.stdout.write(
    `semantic-evidence-adapter: ${resolvable} binding(s) resolved against property-shape/txtar/` +
      "surface-parity/rejection-parity/oracle-inventory evidence, no dangling references\n",
  );
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
