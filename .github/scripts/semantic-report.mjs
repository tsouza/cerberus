#!/usr/bin/env node
// semantic-report.mjs — CLI entry point for `just semantic-report` /
// `just semantic-report-check`. Builds the deterministic semantic
// conformance report (lib/semantic-report.mjs, cerberus issue #3435) from
// the validated semantic contract model (test/semantic/), the CI lane
// registry (.github/ci-lanes.json) and its captured policy snapshot
// (test/semantic/policy-snapshot.json), then either writes
// docs/semantic-conformance.{md,json} (default) or checks them for drift
// (`--check`, no write) — mirroring config-docs.yml's own
// "regenerate, then diff" gate for docs/configuration.md, except the
// comparison happens in-memory here rather than via `git diff`, so this
// mode never touches the working tree and is safe inside a read-only CI
// job.
//
// Usage:
//   node .github/scripts/semantic-report.mjs            # write the report
//   node .github/scripts/semantic-report.mjs --check     # fail on drift
//
// Env:
//   SEMANTIC_MODEL_DIR              directory holding the six model JSON
//                                    files (default test/semantic)
//   SEMANTIC_LANE_REGISTRY_PATH     path to ci-lanes.json (default
//                                    .github/ci-lanes.json)
//   SEMANTIC_LANE_POLICY_SNAPSHOT   path to the captured snapshot (default
//                                    test/semantic/policy-snapshot.json)
//   GITHUB_STEP_SUMMARY             optional summary destination

import process from "node:process";
import { appendFileSync, readFileSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { resolve } from "node:path";

import { DEFAULT_SEMANTIC_MODEL_DIR, loadSemanticModel } from "./lib/semantic-model.mjs";
import { validatePolicySnapshot } from "./lib/semantic-lane-adapter.mjs";
import { loadRegistry } from "./ci-lane-contract.mjs";
import {
  DEFAULT_REPORT_JSON_PATH,
  DEFAULT_REPORT_MD_PATH,
  buildReport,
  renderJSON,
  renderMarkdown,
} from "./lib/semantic-report.mjs";

const MODEL_DIR = process.env.SEMANTIC_MODEL_DIR || DEFAULT_SEMANTIC_MODEL_DIR;
const REGISTRY_PATH = process.env.SEMANTIC_LANE_REGISTRY_PATH ?? ".github/ci-lanes.json";
const SNAPSHOT_PATH =
  process.env.SEMANTIC_LANE_POLICY_SNAPSHOT ?? "test/semantic/policy-snapshot.json";

function appendSummary(body) {
  const path = process.env.GITHUB_STEP_SUMMARY;
  if (path) appendFileSync(path, body);
}

function errorAnnotation(message) {
  const oneLine = message.replaceAll("%", "%25").replaceAll("\r", "%0D").replaceAll("\n", "%0A");
  process.stderr.write(`::error title=Semantic conformance report::${oneLine}\n`);
}

function readIfExists(path) {
  try {
    return readFileSync(path, "utf8");
  } catch (error) {
    if (error.code === "ENOENT") return null;
    throw error;
  }
}

export function generate(root = process.cwd()) {
  const model = loadSemanticModel(MODEL_DIR, { root });
  const registry = loadRegistry(REGISTRY_PATH);
  const snapshot = validatePolicySnapshot(JSON.parse(readFileSync(SNAPSHOT_PATH, "utf8")));
  const report = buildReport(model, { registry, snapshot });
  return { report, markdown: renderMarkdown(report), json: renderJSON(report) };
}

function main() {
  const check = process.argv.includes("--check");
  const { report, markdown, json } = generate();

  if (check) {
    const problems = [];
    const currentMarkdown = readIfExists(DEFAULT_REPORT_MD_PATH);
    const currentJSON = readIfExists(DEFAULT_REPORT_JSON_PATH);
    if (currentMarkdown !== markdown) {
      problems.push(
        `${DEFAULT_REPORT_MD_PATH} is stale or missing — run "just semantic-report" and commit the result`,
      );
    }
    if (currentJSON !== json) {
      problems.push(
        `${DEFAULT_REPORT_JSON_PATH} is stale or missing — run "just semantic-report" and commit the result`,
      );
    }
    if (problems.length > 0) {
      for (const p of problems) errorAnnotation(p);
      process.exit(1);
    }
    process.stdout.write(
      `semantic-report: ${DEFAULT_REPORT_MD_PATH} and ${DEFAULT_REPORT_JSON_PATH} are fresh ` +
        `(${report.counts.contracts.total} contracts, ${report.counts.bindings.active} active bindings)\n`,
    );
    return;
  }

  writeFileSync(DEFAULT_REPORT_MD_PATH, markdown);
  writeFileSync(DEFAULT_REPORT_JSON_PATH, json);
  const summary =
    `## Semantic conformance report\n\n` +
    `- contracts: **${report.counts.contracts.total}** (assured: ${report.assurance_summary.assured_count})\n` +
    `- active bindings: **${report.counts.bindings.active}**\n` +
    `- executions on record: **${report.counts.executions.total}**\n`;
  appendSummary(summary);
  process.stdout.write(
    `semantic-report: wrote ${DEFAULT_REPORT_MD_PATH} and ${DEFAULT_REPORT_JSON_PATH} ` +
      `(${report.counts.contracts.total} contracts, ${report.counts.bindings.active} active bindings)\n`,
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
