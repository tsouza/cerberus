#!/usr/bin/env node
// semantic-impact.mjs — CLI entry point for `just semantic-impact <base>
// <head>`. Explains a git range's effect on the semantic contract model:
// which enrolled contracts it affects, why (the exact files and lane
// dependency closure that triggered it), the evidence each requires, the
// exact existing verifier recipe to run per binding, and each binding's
// current merge/release obligation. See lib/semantic-impact.mjs for the
// full derivation and cerberus issue #3460 for the acceptance criteria.
//
// Usage:
//   node .github/scripts/semantic-impact.mjs <base> <head> [--json]
//
// Advisory only — never a CI gate, never blocking. A targeted local green
// from a recipe this prints is not, by itself, a merge/release
// qualification claim; see the caveat printed with every report.
//
// Env:
//   SEMANTIC_MODEL_DIR              directory holding the six model JSON
//                                    files (default test/semantic)
//   SEMANTIC_LANE_REGISTRY_PATH     path to ci-lanes.json (default
//                                    .github/ci-lanes.json)
//   SEMANTIC_MUTANTS_DIR            directory holding the mutation pilot's
//                                    records (default test/semantic/mutants)
//   SEMANTIC_LANE_POLICY_SNAPSHOT   path to the captured snapshot (default
//                                    test/semantic/policy-snapshot.json)

import process from "node:process";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { resolve } from "node:path";

import { DEFAULT_SEMANTIC_MODEL_DIR, loadSemanticModel } from "./lib/semantic-model.mjs";
import { DEFAULT_MUTANTS_DIR, loadMutants } from "./lib/semantic-mutation.mjs";
import { validatePolicySnapshot } from "./lib/semantic-lane-adapter.mjs";
import { loadRegistry } from "./ci-lane-contract.mjs";
import { assertSafeArg } from "./lib/gh.mjs";
import { changedFiles, revExists } from "./merge-risk.mjs";
import { buildImpactReport, renderJSON, renderText } from "./lib/semantic-impact.mjs";

const MODEL_DIR = process.env.SEMANTIC_MODEL_DIR || DEFAULT_SEMANTIC_MODEL_DIR;
const REGISTRY_PATH = process.env.SEMANTIC_LANE_REGISTRY_PATH ?? ".github/ci-lanes.json";
const MUTANTS_DIR = process.env.SEMANTIC_MUTANTS_DIR || DEFAULT_MUTANTS_DIR;
const SNAPSHOT_PATH = process.env.SEMANTIC_LANE_POLICY_SNAPSHOT ?? "test/semantic/policy-snapshot.json";

function usage() {
  process.stderr.write("usage: node .github/scripts/semantic-impact.mjs <base> <head> [--json]\n");
}

export function main(argv = process.argv.slice(2), { root = process.cwd() } = {}) {
  const asJSON = argv.includes("--json");
  const [base, head] = argv.filter((a) => a !== "--json");
  if (!base || !head) {
    usage();
    process.exit(2);
  }
  assertSafeArg(base, "base");
  assertSafeArg(head, "head");

  const model = loadSemanticModel(MODEL_DIR, { root });
  const registry = loadRegistry(REGISTRY_PATH, { root });
  const snapshot = validatePolicySnapshot(JSON.parse(readFileSync(resolve(root, SNAPSHOT_PATH), "utf8")));
  const mutants = loadMutants(MUTANTS_DIR, { root, contractIds: new Set(model.contracts.keys()) });

  const report = buildImpactReport({
    model,
    registry,
    snapshot,
    mutants,
    repoRoot: root,
    base,
    head,
    revExists,
    changedFiles,
  });

  process.stdout.write(asJSON ? renderJSON(report) : renderText(report));
}

const invokedDirectly = process.argv[1] && fileURLToPath(import.meta.url) === resolve(process.argv[1]);
if (invokedDirectly) {
  try {
    main();
  } catch (error) {
    process.stderr.write(`semantic-impact: ${error instanceof Error ? error.message : String(error)}\n`);
    process.exit(1);
  }
}
