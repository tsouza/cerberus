// semantic-counterexamples.test.mjs — node --test guard for
// test/semantic/counterexamples/*.json's loader/validator (issue #3445).
// Pairs every "must FAIL" acceptance criterion with a positive control that
// must stay green, driving the real validator
// (lib/semantic-counterexamples.mjs) over small in-memory fixtures, plus
// end-to-end checks against the real committed
// test/semantic/counterexamples/ directory and the real CLI
// (semantic-model.mjs, which loads and validates both the six-file model and
// these records in one command).

import assert from "node:assert/strict";
import { test } from "node:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

import { loadSemanticModel } from "./lib/semantic-model.mjs";
import {
  DEFAULT_COUNTEREXAMPLES_DIR,
  loadCounterexamples,
  renderCounterexamplesSummary,
  validateCounterexampleRecord,
} from "./lib/semantic-counterexamples.mjs";

const SCRIPT_DIR = fileURLToPath(new URL(".", import.meta.url));
const REPO_ROOT = process.cwd();
const CLI_PATH = join(SCRIPT_DIR, "semantic-model.mjs");

// --- Fixture builders -----------------------------------------------------

// A minimal stand-in for the six-file model's own validated shape: only
// `contracts` (id -> {..}) and `bindings` (id -> {contract, ...}) are read by
// this validator, so the fixture carries exactly those two Maps rather than
// running the full lib/semantic-model.mjs validation pass.
function fakeModel() {
  return {
    contracts: new Map([
      ["PROMQL-EXAMPLE", { id: "PROMQL-EXAMPLE" }],
      ["LOGQL-EXAMPLE", { id: "LOGQL-EXAMPLE" }],
    ]),
    bindings: new Map([
      ["BINDING-EXAMPLE-SPEC", { id: "BINDING-EXAMPLE-SPEC", contract: "PROMQL-EXAMPLE" }],
      ["BINDING-EXAMPLE-OTHER", { id: "BINDING-EXAMPLE-OTHER", contract: "LOGQL-EXAMPLE" }],
    ]),
  };
}

// Every path field must resolve on disk (see lib/semantic-counterexamples.mjs
// header), so fixtures point at this file itself — real, stable, and always
// present wherever the suite runs.
const REAL_PATH = "test/semantic/counterexamples/issue-1741.json";

function exampleRecord(overrides = {}) {
  return {
    schema_version: 1,
    id: "CTREX-9001",
    source_issue: 9001,
    source_issue_url: "https://github.com/tsouza/cerberus/issues/9001",
    title: "example counterexample",
    summary: "An example historical bug for validator coverage.",
    root_cause_subsystem: "internal/example",
    fix_pr: 9002,
    fix_pr_url: "https://github.com/tsouza/cerberus/pull/9002",
    fix_commit: "a".repeat(40),
    fix_merged_at: "2026-01-01T00:00:00Z",
    discovery_evidence_class: "manual-review",
    discovery_narrative: "Found by reading the code.",
    seed_provenance: "verified",
    seed_provenance_note: null,
    mutation_class: "boundary-condition",
    mutation_class_rationale: "An example boundary was drawn wrong.",
    contracts: [
      {
        contract_id: "PROMQL-EXAMPLE",
        related_bindings: ["BINDING-EXAMPLE-SPEC"],
        locator_path: REAL_PATH,
        locator_detail: "example locator detail",
        replay_test_path: REAL_PATH,
        replay_test_name: "ExampleTest",
        replay_command: "just example",
      },
    ],
    ...overrides,
  };
}

function validate(record) {
  const problems = [];
  const id = validateCounterexampleRecord(record, "fixture.json", fakeModel(), problems, { root: REPO_ROOT });
  return { id, problems };
}

// --- Positive control ------------------------------------------------------

test("a well-formed counterexample record validates cleanly", () => {
  const { id, problems } = validate(exampleRecord());
  assert.deepEqual(problems, []);
  assert.equal(id, "CTREX-9001");
});

// --- Schema discipline ------------------------------------------------------

test("an unknown key fails validation", () => {
  const { problems } = validate({ ...exampleRecord(), extra_field: "nope" });
  assert.ok(problems.some((p) => p.includes("extra_field") && p.includes("unknown")));
});

test("a missing required key fails validation", () => {
  const record = exampleRecord();
  delete record.summary;
  const { problems } = validate(record);
  assert.ok(problems.some((p) => p.includes("summary") && p.includes("required")));
});

test("id must be CTREX-<source_issue>", () => {
  const { problems } = validate(exampleRecord({ id: "CTREX-1234" }));
  assert.ok(problems.some((p) => p.includes("must be CTREX-<source_issue>")));
});

test("source_issue_url disagreeing with source_issue fails validation", () => {
  const { problems } = validate(
    exampleRecord({ source_issue_url: "https://github.com/tsouza/cerberus/issues/1" }),
  );
  assert.ok(problems.some((p) => p.includes("source_issue_url") && p.includes("disagrees")));
});

test("fix_pr_url disagreeing with fix_pr fails validation", () => {
  const { problems } = validate(exampleRecord({ fix_pr_url: "https://github.com/tsouza/cerberus/pull/1" }));
  assert.ok(problems.some((p) => p.includes("fix_pr_url") && p.includes("disagrees")));
});

test("a non-40-hex fix_commit fails validation", () => {
  const { problems } = validate(exampleRecord({ fix_commit: "deadbeef" }));
  assert.ok(problems.some((p) => p.includes("fix_commit")));
});

test("an invalid discovery_evidence_class fails validation", () => {
  const { problems } = validate(exampleRecord({ discovery_evidence_class: "vibes" }));
  assert.ok(problems.some((p) => p.includes("discovery_evidence_class")));
});

test("an invalid mutation_class fails validation", () => {
  const { problems } = validate(exampleRecord({ mutation_class: "gremlins-invert-negatives" }));
  assert.ok(problems.some((p) => p.includes("mutation_class")));
});

// --- Provenance discipline (acceptance criterion: three distinct values) --

test("seed_provenance must be one of the three distinct provenance values", () => {
  const { problems } = validate(exampleRecord({ seed_provenance: "trust-me" }));
  assert.ok(problems.some((p) => p.includes("seed_provenance")));
});

test("a non-verified seed_provenance with a null note fails validation", () => {
  const { problems } = validate(
    exampleRecord({ seed_provenance: "reconstructed", seed_provenance_note: null }),
  );
  assert.ok(problems.some((p) => p.includes("seed_provenance_note") && p.includes("required")));
});

test("a verified seed_provenance with a non-null note fails validation", () => {
  const { problems } = validate(
    exampleRecord({ seed_provenance: "verified", seed_provenance_note: "should be null" }),
  );
  assert.ok(problems.some((p) => p.includes("seed_provenance_note") && p.includes("must be null")));
});

test("a reconstructed seed_provenance with a real note validates cleanly", () => {
  const { problems } = validate(
    exampleRecord({ seed_provenance: "reconstructed", seed_provenance_note: "the original seed rotated out" }),
  );
  assert.deepEqual(problems, []);
});

// --- Cross-references -------------------------------------------------------

test("an unknown contract_id fails validation", () => {
  const record = exampleRecord();
  record.contracts[0].contract_id = "PROMQL-DOES-NOT-EXIST";
  const { problems } = validate(record);
  assert.ok(problems.some((p) => p.includes("unknown contract")));
});

test("an unknown related_bindings entry fails validation", () => {
  const record = exampleRecord();
  record.contracts[0].related_bindings = ["BINDING-DOES-NOT-EXIST"];
  const { problems } = validate(record);
  assert.ok(problems.some((p) => p.includes("unknown binding")));
});

test("a related binding whose own contract disagrees with the entry's contract_id fails validation", () => {
  const record = exampleRecord();
  // BINDING-EXAMPLE-OTHER binds LOGQL-EXAMPLE, not this entry's PROMQL-EXAMPLE.
  record.contracts[0].related_bindings = ["BINDING-EXAMPLE-OTHER"];
  const { problems } = validate(record);
  assert.ok(problems.some((p) => p.includes("binds LOGQL-EXAMPLE")));
});

test("an empty related_bindings array fails validation", () => {
  const record = exampleRecord();
  record.contracts[0].related_bindings = [];
  const { problems } = validate(record);
  assert.ok(problems.some((p) => p.includes("related_bindings") && p.includes("must not be empty")));
});

test("an empty contracts array fails validation", () => {
  const { problems } = validate(exampleRecord({ contracts: [] }));
  assert.ok(problems.some((p) => p.includes("contracts") && p.includes("must not be empty")));
});

test("a duplicate contract_id within one record's contracts array fails validation", () => {
  const record = exampleRecord();
  record.contracts = [record.contracts[0], { ...record.contracts[0] }];
  const { problems } = validate(record);
  assert.ok(problems.some((p) => p.includes("duplicate within this record")));
});

// --- Filesystem evidence discipline (acceptance criterion: no dangling artifacts) --

test("a locator_path that does not exist on disk fails validation", () => {
  const record = exampleRecord();
  record.contracts[0].locator_path = "test/semantic/counterexamples/does-not-exist.json";
  const { problems } = validate(record);
  assert.ok(problems.some((p) => p.includes("locator_path") && p.includes("does not exist on disk")));
});

test("a replay_test_path that does not exist on disk fails validation", () => {
  const record = exampleRecord();
  record.contracts[0].replay_test_path = "test/semantic/counterexamples/does-not-exist.json";
  const { problems } = validate(record);
  assert.ok(problems.some((p) => p.includes("replay_test_path") && p.includes("does not exist on disk")));
});

// --- loadCounterexamples: directory-level behaviour -------------------------

test("loadCounterexamples reads one record per file and rejects a cross-file duplicate id", () => {
  const dir = mkdtempSync(join(tmpdir(), "semantic-counterexamples-"));
  try {
    const a = exampleRecord();
    const b = exampleRecord({ title: "a second file claiming the same id" });
    writeFileSync(join(dir, "a.json"), JSON.stringify(a));
    writeFileSync(join(dir, "b.json"), JSON.stringify(b));
    assert.throws(
      () => loadCounterexamples(fakeModel(), dir, { root: REPO_ROOT }),
      (error) => /duplicate across counterexample records/.test(error.message),
    );
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("loadCounterexamples over a single well-formed record returns it keyed by id", () => {
  const dir = mkdtempSync(join(tmpdir(), "semantic-counterexamples-"));
  try {
    writeFileSync(join(dir, "only.json"), JSON.stringify(exampleRecord()));
    const records = loadCounterexamples(fakeModel(), dir, { root: REPO_ROOT });
    assert.equal(records.size, 1);
    assert.ok(records.has("CTREX-9001"));
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("renderCounterexamplesSummary tallies by seed_provenance", () => {
  const records = new Map([
    ["CTREX-1", { seed_provenance: "verified" }],
    ["CTREX-2", { seed_provenance: "reconstructed" }],
    ["CTREX-3", { seed_provenance: "not-replayed" }],
    ["CTREX-4", { seed_provenance: "verified" }],
  ]);
  const summary = renderCounterexamplesSummary(records);
  assert.match(summary, /counterexamples: \*\*4\*\*/);
  assert.match(summary, /verified: 2/);
  assert.match(summary, /reconstructed: 1/);
  assert.match(summary, /not-replayed: 1/);
});

// --- End-to-end: the real committed cohort ----------------------------------

test("the real committed test/semantic/counterexamples/ directory loads and validates cleanly against the real model", () => {
  const model = loadSemanticModel("test/semantic", { root: REPO_ROOT });
  const records = loadCounterexamples(model, DEFAULT_COUNTEREXAMPLES_DIR, { root: REPO_ROOT });
  assert.equal(records.size, 3);
  assert.deepEqual(
    [...records.keys()].sort(),
    ["CTREX-1741", "CTREX-2241", "CTREX-3271"],
  );
});

test("the seeded cohort spans all three distinct seed_provenance values (acceptance criterion)", () => {
  const model = loadSemanticModel("test/semantic", { root: REPO_ROOT });
  const records = loadCounterexamples(model, DEFAULT_COUNTEREXAMPLES_DIR, { root: REPO_ROOT });
  const provenances = new Set([...records.values()].map((r) => r.seed_provenance));
  assert.deepEqual([...provenances].sort(), ["not-replayed", "reconstructed", "verified"]);
});

test("the seeded cohort covers all three heads (acceptance criterion)", () => {
  const model = loadSemanticModel("test/semantic", { root: REPO_ROOT });
  const records = loadCounterexamples(model, DEFAULT_COUNTEREXAMPLES_DIR, { root: REPO_ROOT });
  const headPrefixes = new Set();
  for (const [, record] of records) {
    for (const entry of record.contracts) headPrefixes.add(entry.contract_id.split("-")[0]);
  }
  assert.deepEqual([...headPrefixes].sort(), ["LOGQL", "PROMQL", "TRACEQL"]);
});

// --- End-to-end: the real CLI ------------------------------------------------

function runCli(env) {
  return spawnSync(process.execPath, [CLI_PATH], {
    cwd: REPO_ROOT,
    env: { ...process.env, ...env },
    encoding: "utf8",
  });
}

test("CLI: the real committed counterexample cohort exits 0 through just semantic-check's own entry point", () => {
  const result = runCli({});
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /semantic-counterexamples: 3 historical-bug records/);
});

test("CLI: a counterexample record with a dangling binding reference exits 1 with a ::error:: annotation", () => {
  const dir = mkdtempSync(join(tmpdir(), "semantic-counterexamples-cli-"));
  try {
    const record = exampleRecord();
    record.contracts[0].related_bindings = ["BINDING-DOES-NOT-EXIST"];
    // The real model's real contract id is used here (not fakeModel's), since
    // the CLI validates counterexamples against the real six-file model.
    record.contracts[0].contract_id = "PROMQL-LABEL-MATCHER-REGEX-ANCHORING";
    writeFileSync(join(dir, "broken.json"), JSON.stringify(record));
    const result = runCli({ SEMANTIC_COUNTEREXAMPLES_DIR: dir });
    assert.equal(result.status, 1);
    assert.match(result.stderr, /::error title=Semantic contract model::/);
    assert.match(result.stderr, /references unknown binding BINDING-DOES-NOT-EXIST/);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
