#!/usr/bin/env node
// semantic-lane-adapter.mjs — CLI entry point for the semantic lane adapter
// (.github/scripts/lib/semantic-lane-adapter.mjs, cerberus issue #3427).
// Loads the CI lane registry and the captured policy snapshot, then runs
// registry-wide drift diagnostics: does every lane's self-declared
// requiredness (context.protected, release_posture) match what the LIVE
// branch-policy snapshot actually says. Exits 1 and prints every mismatch
// on drift; exits 0 and prints a one-line summary otherwise.
//
// Env:
//   SEMANTIC_LANE_REGISTRY_PATH    path to ci-lanes.json (default
//                                  .github/ci-lanes.json)
//   SEMANTIC_LANE_POLICY_SNAPSHOT  path to the captured snapshot (default
//                                  test/semantic/policy-snapshot.json)

import { loadRegistry } from "./ci-lane-contract.mjs";
import {
  diagnoseRegistryDrift,
  validatePolicySnapshot,
} from "./lib/semantic-lane-adapter.mjs";
import { readFileSync } from "node:fs";

const REGISTRY_PATH = process.env.SEMANTIC_LANE_REGISTRY_PATH ?? ".github/ci-lanes.json";
const SNAPSHOT_PATH =
  process.env.SEMANTIC_LANE_POLICY_SNAPSHOT ?? "test/semantic/policy-snapshot.json";

function main() {
  const registry = loadRegistry(REGISTRY_PATH);
  const snapshot = validatePolicySnapshot(JSON.parse(readFileSync(SNAPSHOT_PATH, "utf8")));

  const problems = diagnoseRegistryDrift(registry, snapshot);
  if (problems.length > 0) {
    for (const p of problems) console.log(`::error::${p}`);
    console.log(`::error::semantic-lane-adapter: ${problems.length} drift problem(s) found`);
    process.exit(1);
  }
  console.log(
    `semantic-lane-adapter: ${registry.lanes.length} lanes checked against the ` +
      `${SNAPSHOT_PATH} snapshot, no drift`,
  );
}

main();
