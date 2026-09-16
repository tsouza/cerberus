// semantic-report.test.mjs — node --test guard for the semantic conformance
// report generator (lib/semantic-report.mjs + the CLI, cerberus issue
// #3435). Small in-memory fixtures drive the correlation-safe rollup and
// bound-vs-observed acceptance criteria directly; a handful of end-to-end
// checks run the real CLI against the real committed test/semantic/ corpus,
// the real ci-lanes.json and the real policy-snapshot.json.

import assert from "node:assert/strict";
import { test } from "node:test";
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { join } from "node:path";

import { DEFAULT_SEMANTIC_MODEL_DIR, loadSemanticModel, validateSemanticModel } from "./lib/semantic-model.mjs";
import { loadRegistry } from "./ci-lane-contract.mjs";
import { validatePolicySnapshot } from "./lib/semantic-lane-adapter.mjs";
import {
  DEFAULT_REPORT_JSON_PATH,
  DEFAULT_REPORT_MD_PATH,
  WORKED_EXAMPLE_CONTRACT_ID,
  bindingObservedStatus,
  buildReport,
  classifyExecutionObservation,
  contractObservedRollup,
  evidenceSystemCounts,
  headContractIndex,
  crossHeadContracts,
  latestExecution,
  renderJSON,
  renderMarkdown,
  rollupObserved,
  verifierComplementGaps,
} from "./lib/semantic-report.mjs";

const SCRIPT_DIR = fileURLToPath(new URL(".", import.meta.url));
const REPO_ROOT = process.cwd();
const CLI_PATH = join(SCRIPT_DIR, "semantic-report.mjs");

// --- Fixture builders --------------------------------------------------

function heads() {
  return {
    schema_version: 1,
    heads: [
      { id: "HEAD-PROMQL", name: "PromQL", description: "d", status: "active", owner: "core", replaces: null, replaced_by: null },
      { id: "HEAD-LOGQL", name: "LogQL", description: "d", status: "active", owner: "core", replaces: null, replaced_by: null },
      { id: "HEAD-TRACEQL", name: "TraceQL", description: "d", status: "active", owner: "core", replaces: null, replaced_by: null },
    ],
  };
}

function capabilities() {
  return { schema_version: 1, capabilities: [] };
}

function contract(overrides = {}) {
  return {
    id: "PROMQL-EXAMPLE",
    statement: "An example statement.",
    scope: "head",
    applicable_heads: ["HEAD-PROMQL"],
    applicable_capabilities: [],
    authority: "specification",
    required_evidence_classes: ["execution"],
    required_independence_groups: ["group-a"],
    blind_spots: [],
    related_contracts: [],
    inherits_from: null,
    status: "active",
    deficit_reason: null,
    owner: "core",
    replaces: null,
    replaced_by: null,
    ...overrides,
  };
}

function verifier(overrides = {}) {
  return {
    id: "VERIFIER-EXAMPLE",
    name: "Example verifier",
    description: "d",
    detects: ["something"],
    cannot_detect: [],
    complemented_by: [],
    substrate: "chdb",
    relative_cost: "low",
    status: "active",
    replaces: null,
    replaced_by: null,
    ...overrides,
  };
}

function binding(overrides = {}) {
  return {
    id: "BINDING-EXAMPLE",
    contract: "PROMQL-EXAMPLE",
    verifier: "VERIFIER-EXAMPLE",
    evidence_class: "execution",
    independence_group: "group-a",
    test_ref: "test/spec/promql/example.txtar",
    status: "active",
    ...overrides,
  };
}

function execution(overrides = {}) {
  return {
    id: "EXEC-EXAMPLE",
    binding: "BINDING-EXAMPLE",
    observed_at: "2026-09-10T00:00:00Z",
    result: "pass",
    run_ref: "https://example.invalid/run/1",
    ...overrides,
  };
}

function buildModel({ contracts = [contract()], verifiers = [verifier()], bindings = [binding()], executions = [] } = {}) {
  const documents = {
    heads: heads(),
    capabilities: capabilities(),
    contracts: { schema_version: 1, contracts },
    verifiers: { schema_version: 1, verifiers },
    bindings: { schema_version: 1, bindings },
    executions: { schema_version: 1, executions },
  };
  return validateSemanticModel(documents);
}

function emptyRegistryAndSnapshot() {
  return {
    registry: { lanes: [] },
    snapshot: {
      schema_version: 1,
      main_ruleset: { required_checks: [] },
      maintenance_ruleset: { required_checks: [] },
      release_required_checks: [],
    },
  };
}

// --- classifyExecutionObservation / latestExecution ---------------------

test("classifyExecutionObservation: only pass/fail count as observed", () => {
  assert.equal(classifyExecutionObservation(execution({ result: "pass" })), "pass");
  assert.equal(classifyExecutionObservation(execution({ result: "fail" })), "fail");
  assert.equal(classifyExecutionObservation(execution({ result: "error" })), "unknown");
  assert.equal(classifyExecutionObservation(null), "unknown");
});

test("latestExecution picks the most recent observed_at, not insertion order", () => {
  const model = buildModel({
    executions: [
      execution({ id: "EXEC-OLD", observed_at: "2026-01-01T00:00:00Z", result: "fail" }),
      execution({ id: "EXEC-NEW", observed_at: "2026-09-01T00:00:00Z", result: "pass" }),
    ],
  });
  const latest = latestExecution(model, "BINDING-EXAMPLE");
  assert.equal(latest.id, "EXEC-NEW");
  assert.equal(latest.result, "pass");
});

test("bindingObservedStatus reports unknown, not a silent pass, when no execution exists", () => {
  const model = buildModel({ executions: [] });
  const b = model.bindings.get("BINDING-EXAMPLE");
  const observed = bindingObservedStatus(model, b);
  assert.equal(observed.status, "unknown");
  assert.equal(observed.observed_at, null);
  assert.equal(observed.execution_count, 0);
});

// --- rollupObserved: the correlation-safe collapse (acceptance criterion) --

test("rollupObserved: a fail anywhere wins over any number of passes", () => {
  assert.equal(rollupObserved(["pass", "pass", "pass", "fail"]), "fail");
});

test("rollupObserved: many correlated passes collapse to exactly the same verdict as one", () => {
  const one = rollupObserved(["pass"]);
  const many = rollupObserved(["pass", "pass", "pass", "pass", "pass"]);
  assert.equal(one, "pass");
  assert.equal(many, "pass");
});

test("rollupObserved: no applicable binding is unknown, never assumed pass", () => {
  assert.equal(rollupObserved([]), "unknown");
  assert.equal(rollupObserved(["unknown", "unknown"]), "unknown");
});

// --- contractObservedRollup: bound vs observed, conjunctive over requirements

test("contractObservedRollup: fully observed passing when every required class/group has a real pass", () => {
  const model = buildModel({ executions: [execution({ result: "pass" })] });
  const c = model.contracts.get("PROMQL-EXAMPLE");
  const active = [model.bindings.get("BINDING-EXAMPLE")];
  const rollup = contractObservedRollup(model, c, active);
  assert.equal(rollup.overall, "pass");
  assert.equal(rollup.byEvidenceClass.execution.status, "pass");
  assert.equal(rollup.byIndependenceGroup["group-a"].status, "pass");
});

test("contractObservedRollup: bound evidence with zero executions overall reports unknown, never pass", () => {
  const model = buildModel({ executions: [] });
  const c = model.contracts.get("PROMQL-EXAMPLE");
  const active = [model.bindings.get("BINDING-EXAMPLE")];
  const rollup = contractObservedRollup(model, c, active);
  assert.equal(rollup.overall, "unknown");
});

test("contractObservedRollup: two independence groups both required — one unseen holds the whole contract at unknown, not partial-pass", () => {
  const model = buildModel({
    contracts: [
      contract({
        required_evidence_classes: ["execution"],
        required_independence_groups: ["group-a", "group-b"],
      }),
    ],
    bindings: [
      binding({ id: "BINDING-A", independence_group: "group-a" }),
      binding({ id: "BINDING-B", independence_group: "group-b" }),
    ],
    executions: [execution({ binding: "BINDING-A", result: "pass" })],
  });
  const c = model.contracts.get("PROMQL-EXAMPLE");
  const active = [model.bindings.get("BINDING-A"), model.bindings.get("BINDING-B")];
  const rollup = contractObservedRollup(model, c, active);
  assert.equal(rollup.byIndependenceGroup["group-a"].status, "pass");
  assert.equal(rollup.byIndependenceGroup["group-b"].status, "unknown");
  assert.equal(rollup.overall, "unknown");
});

test("contractObservedRollup: a correlated fail among many passing duplicates still fails the group", () => {
  const model = buildModel({
    bindings: [
      binding({ id: "BINDING-A" }),
      binding({ id: "BINDING-B" }),
      binding({ id: "BINDING-C" }),
    ],
    executions: [
      execution({ id: "EXEC-A", binding: "BINDING-A", result: "pass" }),
      execution({ id: "EXEC-B", binding: "BINDING-B", result: "pass" }),
      execution({ id: "EXEC-C", binding: "BINDING-C", result: "fail" }),
    ],
  });
  const c = model.contracts.get("PROMQL-EXAMPLE");
  const active = [model.bindings.get("BINDING-A"), model.bindings.get("BINDING-B"), model.bindings.get("BINDING-C")];
  const rollup = contractObservedRollup(model, c, active);
  assert.equal(rollup.byIndependenceGroup["group-a"].status, "fail");
  assert.equal(rollup.overall, "fail");
});

// --- verifierComplementGaps -------------------------------------------------

test("verifierComplementGaps: a documented complement not in active use is reported", () => {
  const model = buildModel({
    verifiers: [verifier({ id: "VERIFIER-A", complemented_by: ["VERIFIER-B"] }), verifier({ id: "VERIFIER-B" })],
    bindings: [binding({ verifier: "VERIFIER-A" })],
  });
  const active = [model.bindings.get("BINDING-EXAMPLE")];
  const gaps = verifierComplementGaps(model, active);
  assert.deepEqual(gaps, [{ verifier: "VERIFIER-A", missing_complements: ["VERIFIER-B"] }]);
});

test("verifierComplementGaps: no gap once the complement is also in active use", () => {
  const model = buildModel({
    verifiers: [verifier({ id: "VERIFIER-A", complemented_by: ["VERIFIER-B"] }), verifier({ id: "VERIFIER-B" })],
    bindings: [
      binding({ id: "BINDING-A", verifier: "VERIFIER-A", independence_group: "group-a" }),
      binding({ id: "BINDING-B", verifier: "VERIFIER-B", independence_group: "group-a" }),
    ],
  });
  const active = [model.bindings.get("BINDING-A"), model.bindings.get("BINDING-B")];
  const gaps = verifierComplementGaps(model, active);
  assert.deepEqual(gaps, []);
});

// --- evidence systems / head indexing ---------------------------------------

test("evidenceSystemCounts classifies property-shape, txtar-fixture and unclassified test_refs", () => {
  const model = buildModel({
    bindings: [
      binding({ id: "BINDING-A", test_ref: "test/property/gen#promql.example" }),
      binding({ id: "BINDING-B", test_ref: "test/spec/promql/example.txtar", independence_group: "group-a" }),
      binding({ id: "BINDING-C", test_ref: "internal/engine", independence_group: "group-a" }),
    ],
  });
  const counts = evidenceSystemCounts(model);
  assert.equal(counts["property-shape"], 1);
  assert.equal(counts["txtar-fixture"], 1);
  assert.equal(counts.unclassified, 1);
});

test("headContractIndex and crossHeadContracts partition by scope, never double-counting a head contract as cross-head", () => {
  const model = buildModel({
    contracts: [
      contract({ id: "PROMQL-A" }),
      contract({
        id: "SIGNAL-A",
        scope: "signal",
        applicable_heads: ["HEAD-PROMQL", "HEAD-LOGQL"],
        applicable_capabilities: [],
        status: "draft",
      }),
      contract({
        id: "ARCH-A",
        scope: "architecture",
        applicable_heads: "universal",
        status: "draft",
      }),
    ],
    bindings: [binding({ contract: "PROMQL-A" })],
  });
  const byHead = headContractIndex(model);
  assert.deepEqual(byHead["HEAD-PROMQL"], ["PROMQL-A"]);
  assert.deepEqual(byHead["HEAD-LOGQL"], []);
  const cross = crossHeadContracts(model);
  assert.deepEqual(cross.signal, ["SIGNAL-A"]);
  assert.deepEqual(cross.architecture, ["ARCH-A"]);
});

// --- Determinism ---------------------------------------------------------

test("buildReport + render is byte-identical across two independent builds from the same model", () => {
  const model = buildModel({ executions: [execution()] });
  const { registry, snapshot } = emptyRegistryAndSnapshot();
  const reportA = buildReport(model, { registry, snapshot });
  const reportB = buildReport(model, { registry, snapshot });
  assert.equal(renderMarkdown(reportA), renderMarkdown(reportB));
  assert.equal(renderJSON(reportA), renderJSON(reportB));
});

test("renderMarkdown never embeds a live timestamp (two builds a tick apart still match)", async () => {
  const model = buildModel({ executions: [execution()] });
  const { registry, snapshot } = emptyRegistryAndSnapshot();
  const before = renderMarkdown(buildReport(model, { registry, snapshot }));
  await new Promise((r) => setTimeout(r, 5));
  const after = renderMarkdown(buildReport(model, { registry, snapshot }));
  assert.equal(before, after);
});

// --- End-to-end: the real committed model, the real CLI ---------------------

test("real model: TRACEQL structural lookup surfaces its tests, reference-implementation authority, count-only cannot_detect and a required complement gap", () => {
  const { report } = generateAgainstRealRepo();
  const contractRecord = report.contracts.find((c) => c.id === WORKED_EXAMPLE_CONTRACT_ID);
  assert.ok(contractRecord, `${WORKED_EXAMPLE_CONTRACT_ID} must exist in the real committed model`);
  assert.equal(contractRecord.authority, "reference-implementation");
  assert.ok(contractRecord.bindings.length >= 2, "expects at least its property + spec bindings");
  assert.ok(
    contractRecord.bindings.every((b) => Array.isArray(b.verifier.cannot_detect) && b.verifier.cannot_detect.length > 0),
    "every bound verifier documents what it cannot detect",
  );
  const gapVerifiers = contractRecord.complement_gaps.map((g) => g.verifier);
  assert.ok(gapVerifiers.length > 0, "expects a documented complement this contract's active bindings do not supply");
});

test("real model: counts denominator matches the enrolled contract total, and unenrolled scope is stated in the rendered doc", () => {
  const { report, markdown } = generateAgainstRealRepo();
  assert.equal(
    report.counts.contracts.active +
      report.counts.contracts.draft +
      report.counts.contracts.superseded +
      report.counts.contracts.explicit_deficit,
    report.counts.contracts.total,
  );
  assert.ok(markdown.includes("ENROLLED"), "the scope disclaimer must be present in the rendered document");
});

test("real model: an active contract's observed evidence is never 'pass' unless every required class/group actually has a recorded pass", () => {
  const { report } = generateAgainstRealRepo();
  for (const c of report.contracts) {
    if (c.observed_evidence.overall !== "pass") continue;
    for (const v of Object.values(c.observed_evidence.byEvidenceClass)) assert.equal(v.status, "pass");
    for (const v of Object.values(c.observed_evidence.byIndependenceGroup)) assert.equal(v.status, "pass");
  }
});

test("CLI --check: exits 0 against the committed docs, exits 1 the moment they drift", () => {
  assert.ok(existsSync(join(REPO_ROOT, DEFAULT_REPORT_MD_PATH)), "docs/semantic-conformance.md must be committed");
  const clean = spawnSync("node", [CLI_PATH, "--check"], { cwd: REPO_ROOT, encoding: "utf8" });
  assert.equal(clean.status, 0, clean.stderr);

  const mdPath = join(REPO_ROOT, DEFAULT_REPORT_MD_PATH);
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

function generateAgainstRealRepo() {
  const model = loadSemanticModel(DEFAULT_SEMANTIC_MODEL_DIR, { root: REPO_ROOT });
  const registry = loadRegistry(join(REPO_ROOT, ".github/ci-lanes.json"));
  const snapshot = validatePolicySnapshot(
    JSON.parse(readFileSync(join(REPO_ROOT, "test/semantic/policy-snapshot.json"), "utf8")),
  );
  const report = buildReport(model, { registry, snapshot });
  return { report, markdown: renderMarkdown(report), json: renderJSON(report) };
}
