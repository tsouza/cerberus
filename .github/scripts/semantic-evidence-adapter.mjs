#!/usr/bin/env node
// semantic-evidence-adapter.mjs — CLI entry point for the semantic evidence
// adapter (.github/scripts/lib/semantic-evidence-adapter.mjs, cerberus issue
// #3428). Loads the semantic contract model (test/semantic/) plus the five
// evidence inventories it can resolve bindings against, and reports every
// binding whose test_ref dangles: names an identity that no longer exists
// in the property-shape rosters, the TXTAR spec corpus, the surface-parity
// inventory, the rejection-parity catalogue, the QL feature inventory, or
// — for a plain path / path:TestName / path#token reference — the source
// tree itself. Every binding is resolved; a test_ref no scheme can parse is
// itself reported as dangling.
//
// Env:
//   SEMANTIC_MODEL_DIR   directory holding the six semantic model JSON
//                        files (optional; default test/semantic — see
//                        lib/semantic-model.mjs)
//   GITHUB_STEP_SUMMARY  optional summary destination
//
// Node builtins only, plus a `go run` shell-out for the one Go export
// helper (test/semantic/cmd/property-shape-export). Exit 0 when every
// binding resolves; 1 (with an ::error:: annotation per dangling binding)
// otherwise.

import process from "node:process";
import { fileURLToPath } from "node:url";
import { resolve } from "node:path";

import { appendStepSummary, errorStderr } from "./lib/gh.mjs";
import { DEFAULT_SEMANTIC_MODEL_DIR, loadSemanticModel } from "./lib/semantic-model.mjs";
import {
  EVIDENCE_SYSTEMS,
  diagnoseDanglingBindings,
  loadOracleInventory,
  loadPropertyShapeExport,
  loadRejectionParityCatalogue,
  loadSurfaceParityInventory,
} from "./lib/semantic-evidence-adapter.mjs";

const ANNOTATION_TITLE = "Semantic evidence adapter";

function appendSummary(body) {
  appendStepSummary(body, { quiet: true });
}

function errorAnnotation(message) {
  errorStderr(message, { title: ANNOTATION_TITLE });
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
  const checked = model.bindings.size;

  const summary =
    `## Semantic evidence adapter\n\n` +
    `- bindings checked against the ${EVIDENCE_SYSTEMS.length} evidence systems: **${checked}**\n` +
    `- dangling: **${problems.length}**\n`;
  appendSummary(summary);

  if (problems.length > 0) {
    for (const p of problems) errorAnnotation(p);
    process.stderr.write(`semantic-evidence-adapter: ${problems.length} dangling binding(s)\n`);
    process.exit(1);
  }
  process.stdout.write(
    `semantic-evidence-adapter: ${checked} binding(s) resolved against ${EVIDENCE_SYSTEMS.join("/")} ` +
      "evidence, no dangling references\n",
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
