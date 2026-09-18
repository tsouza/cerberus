// semantic-model.test.mjs — node --test guard for the semantic contract
// metadata model (issue #3426). Pairs every "must FAIL" acceptance criterion
// with a positive control that must stay green, driving the real validator
// (lib/semantic-model.mjs) over small in-memory fixtures, plus a couple of
// end-to-end checks against the real CLI (semantic-model.mjs) and the real
// committed test/semantic/ directory.

import assert from "node:assert/strict";
import { test } from "node:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

import {
  CANONICAL_HEAD_IDS,
  SemanticModelError,
  computeAssurance,
  loadSemanticModel,
  validateSemanticModel,
} from "./lib/semantic-model.mjs";

// Run as `node --test .github/scripts/semantic-model.test.mjs` from the
// repository root, the same convention every other CLI wrapper in this
// directory assumes (e.g. ci-lane-contract.mjs's own `process.cwd()` use).
const SCRIPT_DIR = fileURLToPath(new URL(".", import.meta.url));
const REPO_ROOT = process.cwd();
const CLI_PATH = join(SCRIPT_DIR, "semantic-model.mjs");

// --- Fixture builders ---------------------------------------------------

function baseHeads() {
  return {
    schema_version: 2,
    heads: [
      { id: "HEAD-PROMQL", name: "PromQL", description: "d", status: "active", owner: "core", replaces: null, replaced_by: null },
      { id: "HEAD-LOGQL", name: "LogQL", description: "d", status: "active", owner: "core", replaces: null, replaced_by: null },
      { id: "HEAD-TRACEQL", name: "TraceQL", description: "d", status: "active", owner: "core", replaces: null, replaced_by: null },
    ],
  };
}

function baseCapabilities() {
  return {
    schema_version: 2,
    capabilities: [
      {
        id: "CAP-EXAMPLE",
        name: "Example capability",
        description: "d",
        status: "active",
        applicable_heads: ["HEAD-PROMQL"],
        owner: "core",
        replaces: null,
        replaced_by: null,
      },
    ],
  };
}

function exampleContract(overrides = {}) {
  return {
    id: "PROMQL-EXAMPLE",
    statement: "An example statement describing a real obligation.",
    scope: "head",
    applicable_heads: ["HEAD-PROMQL"],
    applicable_capabilities: ["CAP-EXAMPLE"],
    authority: "specification",
    required_evidence_classes: ["execution"],
    required_independence_groups: ["group-a", "group-b"],
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

function baseContracts(overrides = {}) {
  return { schema_version: 2, contracts: [exampleContract(overrides)] };
}

function baseVerifiers() {
  return {
    schema_version: 2,
    verifiers: [
      {
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
      },
    ],
  };
}

function exampleBinding(overrides = {}) {
  return {
    id: "BINDING-EXAMPLE-A",
    contract: "PROMQL-EXAMPLE",
    verifier: "VERIFIER-EXAMPLE",
    evidence_class: "execution",
    independence_group: "group-a",
    test_ref: "test/spec/promql/example.txtar",
    status: "active",
    ...overrides,
  };
}

function fullyAssuredBindings() {
  return {
    schema_version: 2,
    bindings: [
      exampleBinding({ id: "BINDING-EXAMPLE-A", independence_group: "group-a" }),
      exampleBinding({ id: "BINDING-EXAMPLE-B", independence_group: "group-b" }),
    ],
  };
}

function baseExecutions() {
  return { schema_version: 2, executions: [] };
}

// A complete, valid document set. Individual tests deep-clone and mutate.
function validDocs() {
  return structuredClone({
    heads: baseHeads(),
    capabilities: baseCapabilities(),
    contracts: baseContracts(),
    verifiers: baseVerifiers(),
    bindings: fullyAssuredBindings(),
    executions: baseExecutions(),
  });
}

function problemsOf(fn) {
  try {
    fn();
  } catch (error) {
    assert.ok(error instanceof SemanticModelError, `expected SemanticModelError, got ${error}`);
    return error.problems;
  }
  assert.fail("expected validation to throw");
}

function tagsOf(problems) {
  return problems.map((p) => p.match(/^\[(\w+)\]/)?.[1]);
}

// --- Positive control ----------------------------------------------------

test("a well-formed model validates and fully assures its one active contract", () => {
  const model = validateSemanticModel(validDocs());
  assert.deepEqual(model.assurance.assured, ["PROMQL-EXAMPLE"]);
  assert.deepEqual(model.assurance.excluded, { draft: [], superseded: [], explicit_deficit: [] });
});

test("universal applicable_heads expands equally to all three canonical heads", () => {
  const docs = validDocs();
  docs.capabilities.capabilities[0].applicable_heads = "universal";
  const model = validateSemanticModel(docs);
  const cap = model.capabilities.get("CAP-EXAMPLE");
  assert.deepEqual([...cap.applicableHeads].sort(), [...CANONICAL_HEAD_IDS].sort());
  assert.equal(cap.applicableHeads.size, 3);
});

test("an ARCH contract's universal applicability also expands to all three heads", () => {
  const docs = validDocs();
  docs.contracts.contracts = [
    exampleContract({
      id: "ARCH-EXAMPLE",
      scope: "architecture",
      applicable_heads: "universal",
      applicable_capabilities: [],
      required_independence_groups: [],
    }),
  ];
  docs.bindings.bindings = [
    exampleBinding({ id: "BINDING-EXAMPLE-A", contract: "ARCH-EXAMPLE", independence_group: "group-a" }),
  ];
  const model = validateSemanticModel(docs);
  const contract = model.contracts.get("ARCH-EXAMPLE");
  assert.equal(contract.applicableHeads.size, 3);
  assert.deepEqual(model.assurance.assured, ["ARCH-EXAMPLE"]);
});

// --- Duplicate / reused IDs ------------------------------------------------

test("a duplicate ID within one file fails validation", () => {
  const docs = validDocs();
  docs.contracts.contracts.push(exampleContract({ id: "PROMQL-EXAMPLE" }));
  const problems = problemsOf(() => validateSemanticModel(docs));
  assert.ok(problems.some((p) => p.includes("duplicate") && p.includes("PROMQL-EXAMPLE")));
});

test("a duplicate ID within the verifiers file fails validation", () => {
  const docs = validDocs();
  docs.verifiers.verifiers.push({ ...docs.verifiers.verifiers[0] });
  const problems = problemsOf(() => validateSemanticModel(docs));
  assert.ok(problems.some((p) => p.includes("duplicate") && p.includes("VERIFIER-EXAMPLE")));
});

test("a duplicate ID within the bindings file fails validation", () => {
  const docs = validDocs();
  docs.bindings.bindings.push(exampleBinding({ id: "BINDING-EXAMPLE-A", independence_group: "group-b" }));
  const problems = problemsOf(() => validateSemanticModel(docs));
  assert.ok(problems.some((p) => p.includes("duplicate") && p.includes("BINDING-EXAMPLE-A")));
});

// Every kind's ID pattern (HEAD-/CAP-/PROMQL-/LOGQL-/TRACEQL-/SIGNAL-/ARCH-/
// VERIFIER-/BINDING-/EXEC-) is a disjoint prefix, so no two different kinds
// can ever produce the same ID string — a head can never collide with a
// verifier no matter what either one is named. That is exactly why there is
// no separate cross-kind uniqueness pass: it could never fire.
test("no two record kinds can share an ID prefix, by construction", () => {
  const prefixesByKind = {
    head: "HEAD-",
    capability: "CAP-",
    contract: ["PROMQL-", "LOGQL-", "TRACEQL-", "SIGNAL-", "ARCH-"],
    verifier: "VERIFIER-",
    binding: "BINDING-",
    execution: "EXEC-",
  };
  const allPrefixes = Object.values(prefixesByKind).flat();
  const seen = new Set();
  for (const prefix of allPrefixes) {
    for (const other of seen) {
      assert.ok(
        !prefix.startsWith(other) && !other.startsWith(prefix),
        `${prefix} and ${other} are not disjoint`,
      );
    }
    seen.add(prefix);
  }
});

// --- Unknown keys / heads / capabilities -----------------------------------

test("an unknown key on a contract fails validation", () => {
  const docs = validDocs();
  docs.contracts.contracts[0].unexpected_field = "surprise";
  const problems = problemsOf(() => validateSemanticModel(docs));
  assert.ok(problems.some((p) => p.startsWith("[schema]") && p.includes("unexpected_field is unknown")));
});

test("a missing required key on a head fails validation", () => {
  const docs = validDocs();
  delete docs.heads.heads[0].owner;
  const problems = problemsOf(() => validateSemanticModel(docs));
  assert.ok(problems.some((p) => p.startsWith("[schema]") && p.includes(".owner is required")));
});

test("an unknown head reference fails validation", () => {
  const docs = validDocs();
  // Use signal scope, which tolerates an explicit head list, so the only
  // failure this fixture can produce is the unknown-head reference itself.
  docs.contracts.contracts[0] = exampleContract({
    id: "SIGNAL-EXAMPLE",
    scope: "signal",
    applicable_heads: ["HEAD-BOGUS"],
    applicable_capabilities: [],
  });
  docs.bindings.bindings.forEach((b) => (b.contract = "SIGNAL-EXAMPLE"));
  const problems = problemsOf(() => validateSemanticModel(docs));
  assert.ok(problems.some((p) => p.startsWith("[reference]") && p.includes("not a canonical head")));
});

test("an unknown capability reference fails validation", () => {
  const docs = validDocs();
  docs.contracts.contracts[0].applicable_capabilities = ["CAP-DOES-NOT-EXIST"];
  const problems = problemsOf(() => validateSemanticModel(docs));
  assert.ok(problems.some((p) => p.startsWith("[reference]") && p.includes("unknown capability CAP-DOES-NOT-EXIST")));
});

test("capability head membership must be stated explicitly, never inferred", () => {
  const docs = validDocs();
  delete docs.capabilities.capabilities[0].applicable_heads;
  const problems = problemsOf(() => validateSemanticModel(docs));
  assert.ok(problems.some((p) => p.startsWith("[schema]") && p.includes("applicable_heads is required")));
});

test("a non-array, non-universal applicable_heads value fails validation", () => {
  const docs = validDocs();
  docs.capabilities.capabilities[0].applicable_heads = true;
  const problems = problemsOf(() => validateSemanticModel(docs));
  assert.ok(problems.some((p) => p.includes('must be "universal" or an array of head IDs')));
});

// --- Dangling references ---------------------------------------------------

test("a binding referencing an unknown contract fails validation", () => {
  const docs = validDocs();
  docs.bindings.bindings[0].contract = "PROMQL-DOES-NOT-EXIST";
  const problems = problemsOf(() => validateSemanticModel(docs));
  assert.ok(problems.some((p) => p.startsWith("[reference]") && p.includes("references unknown contract PROMQL-DOES-NOT-EXIST")));
});

test("a binding referencing an unknown verifier fails validation", () => {
  const docs = validDocs();
  docs.bindings.bindings[0].verifier = "VERIFIER-DOES-NOT-EXIST";
  const problems = problemsOf(() => validateSemanticModel(docs));
  assert.ok(problems.some((p) => p.startsWith("[reference]") && p.includes("references unknown verifier VERIFIER-DOES-NOT-EXIST")));
});

test("an execution referencing an unknown binding fails validation", () => {
  const docs = validDocs();
  docs.executions.executions.push({
    id: "EXEC-EXAMPLE",
    binding: "BINDING-DOES-NOT-EXIST",
    observed_at: "2026-09-10T00:00:00Z",
    result: "pass",
    run_ref: "https://example.invalid/run/1",
    selection: "executed",
    selection_reason: null,
    source_sha: null,
    run_id: null,
    run_attempt: null,
    job: null,
    event: null,
    substrate: null,
    reference_version: null,
    dataset_fingerprint: null,
  });
  const problems = problemsOf(() => validateSemanticModel(docs));
  assert.ok(problems.some((p) => p.startsWith("[reference]") && p.includes("references unknown binding BINDING-DOES-NOT-EXIST")));
});

// --- Execution records must be real observations, never typed-in ----------

function exampleExecution(overrides = {}) {
  return {
    id: "EXEC-EXAMPLE",
    binding: "BINDING-EXAMPLE-A",
    observed_at: "2026-09-10T00:00:00Z",
    result: "pass",
    run_ref: "https://github.com/tsouza/cerberus/actions/runs/1234567890",
    selection: "executed",
    selection_reason: null,
    source_sha: "0123456789abcdef0123456789abcdef01234567",
    run_id: "1234567890",
    run_attempt: "1",
    job: "check",
    event: "push",
    substrate: "chdb",
    reference_version: null,
    dataset_fingerprint: null,
    ...overrides,
  };
}

test("an executed record naming a real run and a real commit validates", () => {
  const docs = validDocs();
  docs.executions.executions.push(exampleExecution());
  const model = validateSemanticModel(docs);
  assert.equal(model.executions.size, 1);
});

test("an executed record whose run_ref is an all-zero placeholder run fails validation", () => {
  for (const runRef of [
    "https://github.com/tsouza/cerberus/actions/runs/0000000000",
    "https://github.com/tsouza/cerberus/actions/runs/0",
  ]) {
    const docs = validDocs();
    docs.executions.executions.push(exampleExecution({ run_ref: runRef }));
    const problems = problemsOf(() => validateSemanticModel(docs));
    assert.ok(
      problems.some((p) => p.startsWith("[schema]") && p.includes("run_ref") && p.includes("placeholder")),
      `${runRef}: ${problems.join("; ")}`,
    );
  }
});

test("an executed record with no source_sha fails validation: an observation must name the commit it ran against", () => {
  const docs = validDocs();
  docs.executions.executions.push(exampleExecution({ source_sha: null }));
  const problems = problemsOf(() => validateSemanticModel(docs));
  assert.ok(
    problems.some((p) => p.startsWith("[schema]") && p.includes("source_sha") && p.includes("executed")),
    problems.join("; "),
  );
});

test("a non-executed selection may still carry a null source_sha (it is non-evidence by construction)", () => {
  const docs = validDocs();
  docs.executions.executions.push(
    exampleExecution({
      result: "error",
      selection: "unavailable",
      selection_reason: "report could not be resolved",
      source_sha: null,
    }),
  );
  const model = validateSemanticModel(docs);
  assert.equal(model.executions.size, 1);
});

test("a related_contracts reference to an unknown contract fails validation", () => {
  const docs = validDocs();
  docs.contracts.contracts[0].related_contracts = ["PROMQL-DOES-NOT-EXIST"];
  const problems = problemsOf(() => validateSemanticModel(docs));
  assert.ok(problems.some((p) => p.startsWith("[reference]") && p.includes("related_contracts references unknown contract")));
});

// --- Cyclic inheritance / replacement ---------------------------------------

test("cyclic contract inheritance fails validation", () => {
  const docs = validDocs();
  docs.contracts.contracts = [
    exampleContract({ id: "PROMQL-A", inherits_from: "PROMQL-B", applicable_capabilities: [] }),
    exampleContract({ id: "PROMQL-B", inherits_from: "PROMQL-A", applicable_capabilities: [] }),
  ];
  docs.bindings.bindings.forEach((b) => (b.contract = "PROMQL-A"));
  const problems = problemsOf(() => validateSemanticModel(docs));
  assert.ok(problems.some((p) => p.startsWith("[cycle]") && p.includes("inheritance")));
});

test("a cyclic replacement link fails validation", () => {
  const docs = validDocs();
  docs.contracts.contracts = [
    exampleContract({
      id: "PROMQL-A",
      status: "superseded",
      replaces: "PROMQL-B",
      replaced_by: "PROMQL-B",
      required_evidence_classes: ["execution"],
      required_independence_groups: [],
      applicable_capabilities: [],
    }),
    exampleContract({
      id: "PROMQL-B",
      status: "superseded",
      replaces: "PROMQL-A",
      replaced_by: "PROMQL-A",
      required_evidence_classes: ["execution"],
      required_independence_groups: [],
      applicable_capabilities: [],
    }),
  ];
  docs.bindings.bindings = [];
  const problems = problemsOf(() => validateSemanticModel(docs));
  assert.ok(problems.some((p) => p.startsWith("[cycle]") && p.includes("replacement")));
});

test("a well-formed replacement chain (not a cycle) validates cleanly", () => {
  const docs = validDocs();
  docs.contracts.contracts = [
    exampleContract({
      id: "PROMQL-OLD",
      status: "superseded",
      replaces: null,
      replaced_by: "PROMQL-EXAMPLE",
      required_evidence_classes: ["execution"],
      required_independence_groups: [],
      applicable_capabilities: [],
    }),
    exampleContract({ id: "PROMQL-EXAMPLE", replaces: "PROMQL-OLD", replaced_by: null }),
  ];
  const model = validateSemanticModel(docs);
  assert.deepEqual(model.assurance.excluded.superseded, ["PROMQL-OLD"]);
});

test("a one-sided replacement link (mutuality violated) fails validation", () => {
  const docs = validDocs();
  docs.contracts.contracts = [
    exampleContract({
      id: "PROMQL-OLD",
      status: "superseded",
      replaces: null,
      replaced_by: "PROMQL-EXAMPLE",
      required_evidence_classes: ["execution"],
      required_independence_groups: [],
      applicable_capabilities: [],
    }),
    // PROMQL-EXAMPLE never names PROMQL-OLD back via `replaces`.
    exampleContract({ id: "PROMQL-EXAMPLE" }),
  ];
  const problems = problemsOf(() => validateSemanticModel(docs));
  assert.ok(problems.some((p) => p.startsWith("[reference]") && p.includes("does not name it back")));
});

test("a superseded record without replaced_by fails validation", () => {
  const docs = validDocs();
  docs.contracts.contracts[0].status = "superseded";
  docs.contracts.contracts[0].replaced_by = null;
  const problems = problemsOf(() => validateSemanticModel(docs));
  assert.ok(problems.some((p) => p.startsWith("[schema]") && p.includes("must set replaced_by")));
});

// --- Missing evidence vs malformed metadata are distinct failure classes ----

test("malformed metadata fails with a [schema] problem and no [assurance] problem", () => {
  const docs = validDocs();
  docs.contracts.contracts[0].unexpected_field = "surprise";
  const problems = problemsOf(() => validateSemanticModel(docs));
  assert.ok(tagsOf(problems).includes("schema"));
  assert.ok(!tagsOf(problems).includes("assurance"));
});

test("missing required evidence fails with an [assurance] problem and no [schema] problem", () => {
  const docs = validDocs();
  docs.bindings.bindings = []; // schema-clean; the active contract simply has no evidence
  const problems = problemsOf(() => validateSemanticModel(docs));
  assert.ok(tagsOf(problems).includes("assurance"));
  assert.ok(!tagsOf(problems).includes("schema"));
  assert.ok(problems.some((p) => p.includes("required evidence class(es): execution")));
  assert.ok(problems.some((p) => p.includes("required independence group(s): group-a, group-b")));
});

// --- Status states can never silently read as passing -----------------------

test("a draft contract with no evidence validates but is excluded from assured", () => {
  const docs = validDocs();
  docs.contracts.contracts[0].status = "draft";
  docs.bindings.bindings = [];
  const model = validateSemanticModel(docs);
  assert.deepEqual(model.assurance.assured, []);
  assert.deepEqual(model.assurance.excluded.draft, ["PROMQL-EXAMPLE"]);
});

test("an active contract with no evidence fails validation outright", () => {
  const docs = validDocs();
  docs.bindings.bindings = [];
  assert.throws(() => validateSemanticModel(docs), SemanticModelError);
});

test("an explicit_deficit contract with a deficit_reason validates but is excluded from assured", () => {
  const docs = validDocs();
  docs.contracts.contracts[0].status = "explicit_deficit";
  docs.contracts.contracts[0].deficit_reason = "Known gap, tracked deliberately.";
  docs.bindings.bindings = [];
  const model = validateSemanticModel(docs);
  assert.deepEqual(model.assurance.assured, []);
  assert.deepEqual(model.assurance.excluded.explicit_deficit, ["PROMQL-EXAMPLE"]);
});

test("an explicit_deficit contract without a deficit_reason fails validation", () => {
  const docs = validDocs();
  docs.contracts.contracts[0].status = "explicit_deficit";
  docs.contracts.contracts[0].deficit_reason = null;
  const problems = problemsOf(() => validateSemanticModel(docs));
  assert.ok(problems.some((p) => p.startsWith("[schema]") && p.includes("deficit_reason")));
});

test("a deficit_reason set on a non-explicit_deficit contract fails validation", () => {
  const docs = validDocs();
  docs.contracts.contracts[0].deficit_reason = "should not be set";
  const problems = problemsOf(() => validateSemanticModel(docs));
  assert.ok(problems.some((p) => p.startsWith("[schema]") && p.includes("deficit_reason must be null")));
});

// --- Independence groups are real, not decorative ---------------------------

test("several bindings sharing one independence group cannot satisfy a two-group requirement", () => {
  const docs = validDocs();
  docs.bindings.bindings = [
    exampleBinding({ id: "BINDING-EXAMPLE-A", independence_group: "group-a" }),
    exampleBinding({ id: "BINDING-EXAMPLE-B", independence_group: "group-a" }),
    exampleBinding({ id: "BINDING-EXAMPLE-C", independence_group: "group-a" }),
  ];
  const problems = problemsOf(() => validateSemanticModel(docs));
  assert.ok(
    problems.some(
      (p) => p.startsWith("[assurance]") && p.includes("required independence group(s): group-b"),
    ),
  );
});

test("directly: computeAssurance treats a group as covered only once no matter how many bindings share it", () => {
  const docs = validDocs();
  docs.bindings.bindings = [
    exampleBinding({ id: "BINDING-EXAMPLE-A", independence_group: "group-a" }),
    exampleBinding({ id: "BINDING-EXAMPLE-B", independence_group: "group-a" }),
  ];
  const problems = [];
  // Build the model by hand, bypassing the throwing wrapper, to inspect
  // computeAssurance's verdict directly rather than only the aggregate throw.
  const model = { contracts: new Map(), bindings: new Map() };
  model.contracts.set("PROMQL-EXAMPLE", {
    ...exampleContract(),
    at: "contracts.json.contracts[0]",
  });
  for (const b of docs.bindings.bindings) model.bindings.set(b.id, b);
  const assurance = computeAssurance(model, problems);
  assert.deepEqual(assurance.assured, []);
  assert.ok(problems.some((p) => p.includes("group-b")));
});

test("two bindings spanning both required independence groups fully assures the contract", () => {
  const model = validateSemanticModel(validDocs());
  assert.deepEqual(model.assurance.assured, ["PROMQL-EXAMPLE"]);
});

test("a draft binding does not count toward an active contract's assurance", () => {
  const docs = validDocs();
  docs.bindings.bindings[1].status = "draft";
  const problems = problemsOf(() => validateSemanticModel(docs));
  assert.ok(problems.some((p) => p.startsWith("[assurance]") && p.includes("group-b")));
});

// --- Removal of a head ------------------------------------------------------

test("removing a canonical head from heads.json fails validation", () => {
  const docs = validDocs();
  docs.heads.heads = docs.heads.heads.filter((h) => h.id !== "HEAD-TRACEQL");
  const problems = problemsOf(() => validateSemanticModel(docs));
  assert.ok(problems.some((p) => p.startsWith("[reference]") && p.includes("must declare exactly the canonical heads")));
});

// --- Empty statement ---------------------------------------------------------

test("an empty contract statement fails validation", () => {
  const docs = validDocs();
  docs.contracts.contracts[0].statement = "";
  const problems = problemsOf(() => validateSemanticModel(docs));
  assert.ok(problems.some((p) => p.startsWith("[schema]") && p.includes(".statement must be a non-empty string")));
});

// --- A test-binding record posing as a contract record ----------------------

test("a binding-shaped record inside contracts.json fails validation as a contract", () => {
  const docs = validDocs();
  docs.contracts.contracts.push({
    id: "BINDING-IMPOSTOR",
    contract: "PROMQL-EXAMPLE",
    verifier: "VERIFIER-EXAMPLE",
    evidence_class: "execution",
    independence_group: "group-a",
    test_ref: "test/spec/promql/example.txtar",
    status: "active",
  });
  const problems = problemsOf(() => validateSemanticModel(docs));
  assert.ok(problems.some((p) => p.includes(".statement is required")));
  assert.ok(problems.some((p) => p.includes(".contract is unknown")));
  assert.ok(problems.some((p) => p.includes(".verifier is unknown")));
  // The impostor's ID also never matches a contract-scope prefix.
  assert.ok(problems.some((p) => p.includes(".id has invalid value")));
});

// --- End-to-end: the real committed example model ----------------------------

test("the real committed test/semantic/ directory loads and validates cleanly", () => {
  const model = loadSemanticModel("test/semantic", { root: REPO_ROOT });
  assert.equal(model.heads.size, 3);
  assert.ok(model.contracts.size >= 1);
  assert.ok(model.assurance.assured.length >= 1);
});

// --- End-to-end: the real CLI ------------------------------------------------

function runCli(dir) {
  return spawnSync(process.execPath, [CLI_PATH], {
    cwd: REPO_ROOT,
    env: { ...process.env, SEMANTIC_MODEL_DIR: dir },
    encoding: "utf8",
  });
}

test("CLI: the real committed example model exits 0", () => {
  const result = runCli("test/semantic");
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /semantic-model: \d+ contracts/);
});

test("CLI: a broken model exits 1 with a ::error:: annotation", () => {
  const dir = mkdtempSync(join(tmpdir(), "semantic-model-cli-"));
  try {
    const docs = validDocs();
    docs.contracts.contracts[0].statement = ""; // empty statement, a required negative case
    writeFileSync(join(dir, "heads.json"), JSON.stringify(docs.heads));
    writeFileSync(join(dir, "capabilities.json"), JSON.stringify(docs.capabilities));
    writeFileSync(join(dir, "contracts.json"), JSON.stringify(docs.contracts));
    writeFileSync(join(dir, "verifiers.json"), JSON.stringify(docs.verifiers));
    writeFileSync(join(dir, "bindings.json"), JSON.stringify(docs.bindings));
    writeFileSync(join(dir, "executions.json"), JSON.stringify(docs.executions));
    const result = runCli(dir);
    assert.equal(result.status, 1);
    assert.match(result.stderr, /::error title=Semantic contract model::/);
    assert.match(result.stderr, /statement must be a non-empty string/);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
