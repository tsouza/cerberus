// semantic-guide.test.mjs — node --test guard for the compact semantic
// developer/agent guide (lib/semantic-guide.mjs + the CLI, cerberus issue
// #3462). This module is a VIEW over semantic-report.mjs's own
// buildReport() output, so most of the correctness burden (bound-vs-
// observed rollup, correlation-safe collapse, lane obligation resolution)
// is already pinned by semantic-report.test.mjs — these tests cover the
// guide's OWN new logic: the compact index, the architectural-rule
// selection and its derived (never hand-mapped) CLAUDE.md invariant
// citation, the worked-example wiring, and the CLI's --check drift gate.

import assert from "node:assert/strict";
import { test } from "node:test";
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { join } from "node:path";

import { DEFAULT_SEMANTIC_MODEL_DIR, loadSemanticModel } from "./lib/semantic-model.mjs";
import { loadRegistry } from "./ci-lane-contract.mjs";
import { validatePolicySnapshot } from "./lib/semantic-lane-adapter.mjs";
import { buildReport } from "./lib/semantic-report.mjs";
import {
  DEFAULT_GUIDE_MD_PATH,
  WORKED_EXAMPLES,
  buildGuide,
  renderJSON,
  renderMarkdown,
} from "./lib/semantic-guide.mjs";

const SCRIPT_DIR = fileURLToPath(new URL(".", import.meta.url));
const REPO_ROOT = process.cwd();
const CLI_PATH = join(SCRIPT_DIR, "semantic-guide.mjs");

function generateAgainstRealRepo() {
  const model = loadSemanticModel(DEFAULT_SEMANTIC_MODEL_DIR, { root: REPO_ROOT });
  const registry = loadRegistry(join(REPO_ROOT, ".github/ci-lanes.json"));
  const snapshot = validatePolicySnapshot(
    JSON.parse(readFileSync(join(REPO_ROOT, "test/semantic/policy-snapshot.json"), "utf8")),
  );
  const report = buildReport(model, { registry, snapshot });
  const guide = buildGuide(report, registry);
  return { report, registry, guide, markdown: renderMarkdown(guide), json: renderJSON(guide) };
}

// --- Determinism --------------------------------------------------------

test("buildGuide + render is byte-identical across two independent builds from the same report", () => {
  const a = generateAgainstRealRepo();
  const b = generateAgainstRealRepo();
  assert.equal(a.markdown, b.markdown);
  assert.equal(a.json, b.json);
});

test("renderMarkdown never embeds a live timestamp (two builds a tick apart still match)", async () => {
  const a = generateAgainstRealRepo();
  await new Promise((r) => setTimeout(r, 5));
  const b = generateAgainstRealRepo();
  assert.equal(a.markdown, b.markdown);
});

// --- Real model: worked examples -----------------------------------------

test("real model: every WORKED_EXAMPLES contract ID still exists and resolves in the guide", () => {
  const { guide } = generateAgainstRealRepo();
  const keys = new Set(guide.worked_examples.map((e) => e.key));
  for (const key of Object.keys(WORKED_EXAMPLES)) {
    assert.ok(keys.has(key), `worked example ${key} (${WORKED_EXAMPLES[key]}) must resolve against the real committed model`);
  }
  assert.equal(guide.worked_examples.length, Object.keys(WORKED_EXAMPLES).length);
});

test("real model: the three head worked examples carry distinct authorities, and the architecture example is scope architecture", () => {
  const { guide } = generateAgainstRealRepo();
  const byKey = Object.fromEntries(guide.worked_examples.map((e) => [e.key, e]));
  assert.equal(byKey["HEAD-PROMQL"].contract.authority, "specification");
  assert.equal(byKey["HEAD-LOGQL"].contract.authority, "reference-implementation");
  assert.equal(byKey["HEAD-TRACEQL"].contract.authority, "reference-implementation");
  assert.equal(byKey.ARCHITECTURE.contract.scope, "architecture");
  assert.ok(byKey["HEAD-TRACEQL"].contract.blind_spots.length > 0, "TraceQL example must carry a documented blind spot");
});

// --- Contract index -------------------------------------------------------

test("real model: the contract index covers exactly the active contracts, no more and no fewer", () => {
  const { report, guide } = generateAgainstRealRepo();
  const activeIds = report.contracts.filter((c) => c.status === "active").map((c) => c.id).sort();
  const indexIds = guide.contract_index.map((r) => r.id).sort();
  assert.deepEqual(indexIds, activeIds);
});

test("real model: every contract index row with a resolvable lane carries a `just` or `node` runnable command, never a bare prose summary", () => {
  const { guide } = generateAgainstRealRepo();
  for (const row of guide.contract_index) {
    if (!row.execution) continue;
    assert.match(
      row.execution.command,
      /^(just |node )/,
      `${row.id}'s canonical execution must start with an actual invocation: ${row.execution.command}`,
    );
  }
});

// --- Architectural rules ---------------------------------------------------

test("real model: architectural rules are exactly the active architecture-scope contracts", () => {
  const { report, guide } = generateAgainstRealRepo();
  const wantIds = report.contracts
    .filter((c) => c.scope === "architecture" && c.status === "active")
    .map((c) => c.id)
    .sort();
  const gotIds = guide.architectural_rules.map((r) => r.id).sort();
  assert.deepEqual(gotIds, wantIds);
});

test("rendered markdown escapes free-text contract data so a glob like internal/chsql/** survives byte-for-byte (never silently corrected by the markdown autofixer)", () => {
  const { markdown } = generateAgainstRealRepo();
  assert.match(markdown, /internal\/chsql\/\\\*\\\* composes ClickHouse SQL/);
  assert.doesNotMatch(markdown, /internal\/chsql\/\*\*composes/);
});

test("real model: an architectural rule's CLAUDE.md invariant citation is DERIVED from its own statement text, never invented", () => {
  const { guide } = generateAgainstRealRepo();
  const byId = Object.fromEntries(guide.architectural_rules.map((r) => [r.id, r]));
  // Known citations at the time this test was written — see contracts.json.
  assert.equal(byId["ARCH-NO-QUERY-RESULT-CACHING"].claude_md_invariant, 12);
  assert.equal(byId["ARCH-HEAD-EMIT-001"].claude_md_invariant, 10);
  // A real architectural rule with no citation reports null, never a guess.
  assert.equal(byId["ARCH-CAP-FILTER-001"].claude_md_invariant, null);
  for (const rule of guide.architectural_rules) {
    if (rule.claude_md_invariant !== null) {
      assert.match(
        rule.statement,
        new RegExp(`CLAUDE\\.md invariant ${rule.claude_md_invariant}\\b`),
        `${rule.id}'s cited invariant must actually appear in its own statement text`,
      );
    }
  }
});

// --- Process framing: never a self-approval / gate claim -------------------

test("rendered guide states the process explicitly, without claiming to be a merge gate or a self-approval surface", () => {
  const { markdown } = generateAgainstRealRepo();
  assert.match(markdown, /Not a merge gate/);
  assert.match(markdown, /hand-authored, reviewed source, not a self-service approval surface/);
  assert.match(markdown, /never required to complete the flow/);
});

test("adversarial-evidence section states 'none yet' explicitly and never blocks the other steps", () => {
  const { markdown } = generateAgainstRealRepo();
  assert.match(markdown, /no mutation\/adversarial evidence class exists yet/);
  assert.match(markdown, /Optional adversarial probes/);
});

test("TraceQL/Tempo structural example, its blind spot and its required complement are all directly linked", () => {
  const { markdown } = generateAgainstRealRepo();
  assert.match(markdown, /TRACEQL-STRUCTURAL-RELATION-SEMANTICS/);
  assert.match(markdown, /Tempo/);
  assert.match(markdown, /blind spots/);
  assert.match(markdown, /required complement not yet supplied/);
});

// --- CLI ---------------------------------------------------------------

test("CLI --check: exits 0 against the committed docs, exits 1 the moment they drift", () => {
  assert.ok(existsSync(join(REPO_ROOT, DEFAULT_GUIDE_MD_PATH)), "docs/semantic-guide.md must be committed");
  const clean = spawnSync("node", [CLI_PATH, "--check"], { cwd: REPO_ROOT, encoding: "utf8" });
  assert.equal(clean.status, 0, clean.stderr);

  const mdPath = join(REPO_ROOT, DEFAULT_GUIDE_MD_PATH);
  const backup = readFileSync(mdPath, "utf8");
  try {
    writeFileSync(mdPath, `${backup}\nhand-edited drift\n`);
    const dirty = spawnSync("node", [CLI_PATH, "--check"], { cwd: REPO_ROOT, encoding: "utf8" });
    assert.equal(dirty.status, 1);
    assert.match(dirty.stderr, /stale or missing/);
  } finally {
    writeFileSync(mdPath, backup);
  }
});
