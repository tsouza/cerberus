#!/usr/bin/env node
// semantic-guide.mjs — CLI entry point for `just semantic-guide` /
// `just semantic-guide-check`. Builds the compact developer/agent guide
// (lib/semantic-guide.mjs, cerberus issue #3462) over the same
// semantic-report.mjs buildReport() output the full conformance report
// renders from, then either writes docs/semantic-guide.{md,json} (default)
// or checks them for drift (`--check`, no write) — mirroring semantic-
// report.mjs's own CLI exactly, including its in-memory (never touches the
// working tree) `--check` mode.
//
// Usage:
//   node .github/scripts/semantic-guide.mjs            # write the guide
//   node .github/scripts/semantic-guide.mjs --check     # fail on drift
//
// Env: same three as semantic-report.mjs (SEMANTIC_MODEL_DIR,
// SEMANTIC_LANE_REGISTRY_PATH, SEMANTIC_LANE_POLICY_SNAPSHOT) plus
// GITHUB_STEP_SUMMARY — this CLI reads the identical three inputs to build
// the identical underlying report, so the env surface is deliberately not a
// fourth, guide-specific set of names.

import process from "node:process";
import { appendFileSync, readFileSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { resolve } from "node:path";

import { DEFAULT_SEMANTIC_MODEL_DIR, loadSemanticModel } from "./lib/semantic-model.mjs";
import { validatePolicySnapshot } from "./lib/semantic-lane-adapter.mjs";
import { loadRegistry } from "./ci-lane-contract.mjs";
import { lintFixMarkdown } from "./lib/markdown-lintfix.mjs";
import { buildReport } from "./lib/semantic-report.mjs";
import {
  DEFAULT_GUIDE_JSON_PATH,
  DEFAULT_GUIDE_MD_PATH,
  buildGuide,
  renderJSON,
  renderMarkdown,
} from "./lib/semantic-guide.mjs";

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
  process.stderr.write(`::error title=Semantic guide::${oneLine}\n`);
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
  const guide = buildGuide(report, registry);
  const markdown = lintFixMarkdown(renderMarkdown(guide), root);
  return { guide, markdown, json: renderJSON(guide) };
}

function main() {
  const check = process.argv.includes("--check");
  const { guide, markdown, json } = generate();

  if (check) {
    const problems = [];
    const currentMarkdown = readIfExists(DEFAULT_GUIDE_MD_PATH);
    const currentJSON = readIfExists(DEFAULT_GUIDE_JSON_PATH);
    if (currentMarkdown !== markdown) {
      problems.push(
        `${DEFAULT_GUIDE_MD_PATH} is stale or missing — run "just semantic-guide" and commit the result`,
      );
    }
    if (currentJSON !== json) {
      problems.push(
        `${DEFAULT_GUIDE_JSON_PATH} is stale or missing — run "just semantic-guide" and commit the result`,
      );
    }
    if (problems.length > 0) {
      for (const p of problems) errorAnnotation(p);
      process.exit(1);
    }
    process.stdout.write(
      `semantic-guide: ${DEFAULT_GUIDE_MD_PATH} and ${DEFAULT_GUIDE_JSON_PATH} are fresh ` +
        `(${guide.contract_index.length} contracts indexed, ${guide.architectural_rules.length} architectural rules)\n`,
    );
    return;
  }

  writeFileSync(DEFAULT_GUIDE_MD_PATH, markdown);
  writeFileSync(DEFAULT_GUIDE_JSON_PATH, json);
  const summary =
    `## Semantic guide\n\n` +
    `- contracts indexed: **${guide.contract_index.length}**\n` +
    `- architectural rules: **${guide.architectural_rules.length}**\n` +
    `- worked examples: **${guide.worked_examples.length}**\n`;
  appendSummary(summary);
  process.stdout.write(
    `semantic-guide: wrote ${DEFAULT_GUIDE_MD_PATH} and ${DEFAULT_GUIDE_JSON_PATH} ` +
      `(${guide.contract_index.length} contracts indexed, ${guide.architectural_rules.length} architectural rules)\n`,
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
