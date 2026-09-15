#!/usr/bin/env node
// semantic-lane-policy-snapshot.mjs — captures the LIVE branch-policy
// authorities into a versioned, offline-readable snapshot for the semantic
// lane adapter (see .github/scripts/lib/semantic-lane-adapter.mjs, cerberus
// issue #3427). Read-only: this script never writes policy, only reads it
// and records what it saw.
//
// Reuses release-gate-drift.mjs's own live-ruleset reader
// (readRequiredContexts, over `GET /repos/{o}/{r}/rules/branches/{branch}`,
// proven sufficient with the plain workflow token — see that file's own
// comment on why no elevated credential is needed) and its RELEASE_REQUIRED_
// CHECKS parser (parseCheckLists) instead of re-implementing either; the two
// scripts diagnose overlapping drift and must never disagree about what
// "live" means.
//
// Three distinct authorities are captured, matching #3427's own acceptance
// criterion that they be recorded SEPARATELY rather than merged into one
// set:
//   main_ruleset              required_status_checks union across every
//                              required_status_checks rule on the `main`
//                              branch ruleset(s) — via readRequiredContexts.
//   maintenance_ruleset       same, for the ruleset governing `release/*.x`
//                              branches. No literal `release/*.x` branch is
//                              guaranteed to exist at capture time (there is
//                              usually none outside an active backport), so
//                              this one is read by RULESET ID directly
//                              (`GET /repos/{o}/{r}/rulesets/{id}`) rather
//                              than by branch — the one live read this
//                              script adds that release-gate-drift.mjs does
//                              not already do.
//   release_required_checks   parsed from release.yml's own
//                              RELEASE_REQUIRED_CHECKS block via
//                              parseCheckLists — the push-to-main publish
//                              preflight's EXPECTED set, a deliberately
//                              separate authority from either ruleset.
//
// Usage: node .github/scripts/semantic-lane-policy-snapshot.mjs [--write]
//   --write   overwrite test/semantic/policy-snapshot.json with what was
//             captured (default: print to stdout and diff against the
//             checked-in snapshot, exiting 1 on drift without --write).
//
// Env: GITHUB_TOKEN, GITHUB_REPOSITORY, GITHUB_API_URL — same as
// release-gate-drift.mjs.
//
// MANUAL, NOT SCHEDULED: unlike release-gate-drift.mjs's own live check
// (which runs on a schedule against `github.token`), this script is NOT
// wired into any CI workflow, because the Rulesets API it needs
// (`GET /repos/{o}/{r}/rulesets` and `/rulesets/{id}`, for the maintenance-
// line ruleset) has no corresponding Actions `permissions:` scope at all —
// `administration` is not a valid workflow permission, so `github.token` can
// never read it, at any privilege level. Re-provisioning a PAT/secret for
// this is exactly what release-gate-drift.yml's own header comment warns
// against (a credential nobody provisions makes a scheduled check silently
// never run at all — worse than not having the check). Run this by hand
// with a maintainer's own `gh auth token` (or an equivalent PAT with repo
// admin read) whenever main's ruleset, the maintenance-lines ruleset, or
// release.yml's RELEASE_REQUIRED_CHECKS changes, and commit the refreshed
// snapshot alongside that change. The PR-time `semantic-lane-adapter` CI
// check only validates ci-lanes.json against this checked-in snapshot
// (fully offline) — it cannot itself detect the snapshot going stale.

import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import {
  apiJson,
  parseCheckLists,
  readRequiredContexts,
  tokenHeaders,
} from "./release-gate-drift.mjs";

const SNAPSHOT_PATH = join("test", "semantic", "policy-snapshot.json");
const RELEASE_WORKFLOW_PATH = join(".github", "workflows", "release.yml");
const MAINTENANCE_RULESET_NAME = "release maintenance lines";
const SCHEMA_VERSION = 1;

async function captureMaintenanceRuleset({ apiBase, repo, token }) {
  const headers = tokenHeaders(token);
  const rulesets = await apiJson(
    `${apiBase}/repos/${repo}/rulesets`,
    headers,
    "list repository rulesets",
  );
  const match = rulesets.find((r) => r.name === MAINTENANCE_RULESET_NAME);
  if (!match) {
    throw new Error(
      `no ruleset named "${MAINTENANCE_RULESET_NAME}" found — did it get renamed? ` +
        `update MAINTENANCE_RULESET_NAME in this script to match.`,
    );
  }
  const full = await apiJson(
    `${apiBase}/repos/${repo}/rulesets/${match.id}`,
    headers,
    `read ruleset ${match.id}`,
  );
  const refPatterns = full.conditions?.ref_name?.include ?? [];
  const checks = (full.rules ?? [])
    .filter((rule) => rule.type === "required_status_checks")
    .flatMap((rule) => rule.parameters.required_status_checks.map((c) => c.context))
    .filter((v, i, arr) => arr.indexOf(v) === i)
    .sort();
  return { rulesetId: match.id, refPatterns, requiredChecks: checks };
}

async function capture() {
  const repo = process.env.GITHUB_REPOSITORY;
  const token = process.env.GITHUB_TOKEN;
  const apiBase = process.env.GITHUB_API_URL || "https://api.github.com";
  if (!repo) throw new Error("GITHUB_REPOSITORY is unset");
  if (!token) throw new Error("GITHUB_TOKEN is unset — the live policy capture cannot run");

  const { contexts: mainRequired } = await readRequiredContexts({
    apiBase,
    repo,
    branch: "main",
    token,
  });
  const maintenance = await captureMaintenanceRuleset({ apiBase, repo, token });
  const { required: releaseRequired } = parseCheckLists(
    readFileSync(RELEASE_WORKFLOW_PATH, "utf8"),
  );

  return {
    schema_version: SCHEMA_VERSION,
    captured_at: new Date().toISOString(),
    main_ruleset: {
      required_checks: [...mainRequired].sort(),
    },
    maintenance_ruleset: {
      ruleset_id: maintenance.rulesetId,
      ref_patterns: maintenance.refPatterns,
      required_checks: maintenance.requiredChecks,
    },
    release_required_checks: [...releaseRequired],
  };
}

async function main() {
  const write = process.argv.includes("--write");
  const snapshot = await capture();

  if (write) {
    writeFileSync(SNAPSHOT_PATH, JSON.stringify(snapshot, null, 2) + "\n");
    console.log(`::notice::wrote ${SNAPSHOT_PATH}`);
    return;
  }

  let existing = null;
  try {
    existing = JSON.parse(readFileSync(SNAPSHOT_PATH, "utf8"));
  } catch {
    console.log("::error::no checked-in snapshot to diff against; run with --write first");
    process.exit(1);
  }

  const comparable = (s) => JSON.stringify({ ...s, captured_at: null });
  if (comparable(existing) !== comparable(snapshot)) {
    console.log("::error::live policy has drifted from the checked-in snapshot");
    console.log(JSON.stringify(snapshot, null, 2));
    process.exit(1);
  }
  console.log("policy snapshot: no drift");
}

main().catch((err) => {
  console.log(`::error::${err.message}`);
  process.exit(1);
});
