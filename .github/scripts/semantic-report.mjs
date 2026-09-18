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
import { readFileSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { resolve } from "node:path";

import { appendStepSummary, errorStderr } from "./lib/gh.mjs";
import { DEFAULT_SEMANTIC_MODEL_DIR, loadSemanticModel } from "./lib/semantic-model.mjs";
import { validatePolicySnapshot } from "./lib/semantic-lane-adapter.mjs";
import { loadRegistry } from "./ci-lane-contract.mjs";
import { lintFixMarkdown } from "./lib/markdown-lintfix.mjs";
import {
  DEFAULT_MUTANT_EXECUTIONS_PATH,
  DEFAULT_MUTANTS_DIR,
  loadMutantExecutions,
  loadMutants,
} from "./lib/semantic-mutation.mjs";
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
const MUTANTS_DIR = process.env.SEMANTIC_MUTANTS_DIR || DEFAULT_MUTANTS_DIR;
const MUTANT_EXECUTIONS_PATH =
  process.env.SEMANTIC_MUTANT_EXECUTIONS_PATH ?? DEFAULT_MUTANT_EXECUTIONS_PATH;

const ANNOTATION_TITLE = "Semantic conformance report";

function appendSummary(body) {
  appendStepSummary(body, { quiet: true });
}

function errorAnnotation(message) {
  errorStderr(message, { title: ANNOTATION_TITLE });
}

function readIfExists(path) {
  try {
    return readFileSync(path, "utf8");
  } catch (error) {
    if (error.code === "ENOENT") return null;
    throw error;
  }
}

// renderMarkdown's own output is plain, unformatted Markdown: simple
// `| --- | --- |` table separators, and no attempt at the repo's own house
// style (MD032 blank-line-around-list, MD049/MD050 emphasis-marker choice,
// MD060 table-column alignment, …). lintFixMarkdown (lib/markdown-
// lintfix.mjs) runs the SAME two fixers lefthook's own pre-commit hooks run
// on staged Markdown, in the same order, against a throwaway temp copy —
// shared with semantic-guide.mjs's CLI rather than reimplemented here, per
// CLAUDE.md's DRY invariant.

// loadReport builds the report object every generated semantic document
// projects from — the model, the lane registry and policy snapshot, and the
// mutation pilot corpus with its ledger, all from the same env-driven
// paths. semantic-guide.mjs imports it rather than repeating the loading,
// so the guide can never be built from a different input set than the
// report it is a view over.
export function loadReport(root = process.cwd()) {
  const model = loadSemanticModel(MODEL_DIR, { root });
  const registry = loadRegistry(REGISTRY_PATH);
  const snapshot = validatePolicySnapshot(JSON.parse(readFileSync(SNAPSHOT_PATH, "utf8")));
  const mutants = loadMutants(MUTANTS_DIR, { root, contractIds: new Set(model.contracts.keys()) });
  const mutantExecutions = loadMutantExecutions(MUTANT_EXECUTIONS_PATH, {
    root,
    mutantIds: new Set(mutants.keys()),
  });
  const report = buildReport(model, { registry, snapshot, mutants, mutantExecutions });
  return { model, registry, snapshot, report };
}

export function generate(root = process.cwd()) {
  const { report } = loadReport(root);
  const markdown = lintFixMarkdown(renderMarkdown(report), root);
  return { report, markdown, json: renderJSON(report) };
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
    `- executions on record: **${report.counts.executions.total}**\n` +
    `- semantic mutation pilot: **${report.mutation_cohort.semantic_cohort.rates.total}** real records ` +
    `(kill rate ${report.mutation_cohort.semantic_cohort.rates.kill_rate ?? "n/a"}, denominator ` +
    `${report.mutation_cohort.semantic_cohort.rates.denominator}${report.mutation_cohort.semantic_cohort.rates.small_sample ? ", small sample" : ""})\n`;
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
