// semantic-execution-adapter.test.mjs — node --test guard for
// lib/semantic-execution-adapter.mjs and its CLI (cerberus issue #3459).
// Pairs every acceptance-criterion non-evidence class (wrong SHA, wrong
// reference, stale corpus fingerprint, zero selected, aggregate no-op)
// with a real-shaped report sample and a positive "executed" control, plus
// end-to-end CLI runs over each of the three modes.

import assert from "node:assert/strict";
import { test } from "node:test";
import { mkdtempSync, mkdirSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

import {
  SemanticExecutionAdapterError,
  classifyRevisionBinding,
  githubRunContext,
  hashBytes,
  hashCorpus,
  hashFile,
  parseCaseSet,
  parseGoTestJSONShapeResults,
  propertyShapeObservation,
  toExecutionRecord,
} from "./lib/semantic-execution-adapter.mjs";

const SCRIPT_DIR = fileURLToPath(new URL(".", import.meta.url));
const REPO_ROOT = process.cwd();
const CLI_PATH = join(SCRIPT_DIR, "semantic-execution-adapter.mjs");

function tempDir(prefix) {
  return mkdtempSync(join(tmpdir(), prefix));
}

// --- classifyRevisionBinding: the five non-evidence classes + the positive control ---

const CANDIDATE = { sourceSha: "abc1234def5678" };
const BASE_OBSERVATION = {
  sourceSha: "abc1234def5678",
  referenceVersion: null,
  datasetFingerprint: null,
  selectedCount: 5,
  ranCount: 5,
  aggregateNoOp: false,
  result: "pass",
};

test("classifyRevisionBinding: a matching, fully-run, real verdict is executed evidence", () => {
  const c = classifyRevisionBinding(CANDIDATE, BASE_OBSERVATION);
  assert.equal(c.selection, "executed");
  assert.equal(c.evidence, true);
  assert.equal(c.result, "pass");
  assert.equal(c.reason, null);
});

test("classifyRevisionBinding: a real fail is STILL executed evidence (of failure), not non-evidence", () => {
  const c = classifyRevisionBinding(CANDIDATE, { ...BASE_OBSERVATION, result: "fail" });
  assert.equal(c.selection, "executed");
  assert.equal(c.evidence, true);
  assert.equal(c.result, "fail");
});

test("classifyRevisionBinding: wrong SHA yields non-evidence (unavailable)", () => {
  const c = classifyRevisionBinding(CANDIDATE, { ...BASE_OBSERVATION, sourceSha: "deadbeef0000" });
  assert.equal(c.selection, "unavailable");
  assert.equal(c.evidence, false);
  assert.equal(c.result, "error");
  assert.match(c.reason, /does not match candidate/);
});

test("classifyRevisionBinding: a missing (null) source_sha yields non-evidence, never a false match", () => {
  const c = classifyRevisionBinding(CANDIDATE, { ...BASE_OBSERVATION, sourceSha: null });
  assert.equal(c.selection, "unavailable");
  assert.equal(c.evidence, false);
});

test("classifyRevisionBinding: wrong reference version (when asserted) yields non-evidence", () => {
  const c = classifyRevisionBinding(
    { ...CANDIDATE, referenceVersion: "tempo-2.5.0" },
    { ...BASE_OBSERVATION, referenceVersion: "tempo-2.4.0" },
  );
  assert.equal(c.selection, "unavailable");
  assert.equal(c.evidence, false);
  assert.match(c.reason, /reference_version/);
});

test("classifyRevisionBinding: reference version is never checked unless the candidate asserts one", () => {
  const c = classifyRevisionBinding(CANDIDATE, { ...BASE_OBSERVATION, referenceVersion: "whatever" });
  assert.equal(c.selection, "executed");
  assert.equal(c.evidence, true);
});

test("classifyRevisionBinding: stale corpus fingerprint (when asserted) yields non-evidence", () => {
  const c = classifyRevisionBinding(
    { ...CANDIDATE, datasetFingerprint: "f".repeat(64) },
    { ...BASE_OBSERVATION, datasetFingerprint: "0".repeat(64) },
  );
  assert.equal(c.selection, "stale");
  assert.equal(c.evidence, false);
  assert.match(c.reason, /dataset_fingerprint/);
});

test("classifyRevisionBinding: zero selected tests yields non-evidence (selected_not_run)", () => {
  const c = classifyRevisionBinding(CANDIDATE, { ...BASE_OBSERVATION, selectedCount: 0, ranCount: 0 });
  assert.equal(c.selection, "selected_not_run");
  assert.equal(c.evidence, false);
});

test("classifyRevisionBinding: aggregate no-op yields non-evidence even when the SHA matches", () => {
  const c = classifyRevisionBinding(CANDIDATE, { ...BASE_OBSERVATION, aggregateNoOp: true });
  assert.equal(c.selection, "no_op");
  assert.equal(c.evidence, false);
});

test("classifyRevisionBinding: no-op is checked before SHA, so a mismatched SHA on a no-op still reports no_op", () => {
  const c = classifyRevisionBinding(CANDIDATE, { ...BASE_OBSERVATION, sourceSha: "other", aggregateNoOp: true });
  assert.equal(c.selection, "no_op");
});

test("classifyRevisionBinding: selected but none ran yields non-evidence (unavailable)", () => {
  const c = classifyRevisionBinding(CANDIDATE, { ...BASE_OBSERVATION, selectedCount: 3, ranCount: 0 });
  assert.equal(c.selection, "unavailable");
  assert.equal(c.evidence, false);
});

test("classifyRevisionBinding: a non pass/fail result yields non-evidence (unavailable)", () => {
  const c = classifyRevisionBinding(CANDIDATE, { ...BASE_OBSERVATION, result: "flaky" });
  assert.equal(c.selection, "unavailable");
  assert.equal(c.evidence, false);
});

test("classifyRevisionBinding: candidate.sourceSha is required", () => {
  assert.throws(() => classifyRevisionBinding({}, BASE_OBSERVATION), SemanticExecutionAdapterError);
  assert.throws(() => classifyRevisionBinding(null, BASE_OBSERVATION), SemanticExecutionAdapterError);
});

// --- toExecutionRecord ------------------------------------------------------

test("toExecutionRecord: an executed record carries a null selection_reason", () => {
  const rec = toExecutionRecord({
    id: "EXEC-X-20260101T000000",
    binding: "BINDING-X",
    observedAt: "2026-01-01T00:00:00Z",
    runRef: "https://example.invalid/run/1",
    classification: { selection: "executed", evidence: true, result: "pass", reason: null },
    context: { sourceSha: "abc1234" },
  });
  assert.equal(rec.selection, "executed");
  assert.equal(rec.selection_reason, null);
  assert.equal(rec.result, "pass");
  assert.equal(rec.source_sha, "abc1234");
  // Every EXECUTION_KEYS field present, matching exactObject's own
  // "never omit, express absence as null" discipline.
  for (const key of ["run_id", "run_attempt", "job", "event", "substrate", "reference_version", "dataset_fingerprint"]) {
    assert.equal(rec[key], null, `${key} should default to null`);
  }
});

test("toExecutionRecord: a non-evidence record carries result error and the classifier's own reason", () => {
  const rec = toExecutionRecord({
    id: "EXEC-X-20260101T000000",
    binding: "BINDING-X",
    observedAt: "2026-01-01T00:00:00Z",
    runRef: "https://example.invalid/run/1",
    classification: { selection: "stale", evidence: false, result: "error", reason: "corpus changed" },
  });
  assert.equal(rec.selection, "stale");
  assert.equal(rec.result, "error");
  assert.equal(rec.selection_reason, "corpus changed");
});

// --- Hashing -----------------------------------------------------------------

test("hashBytes is a 64-char lowercase hex sha256 digest", () => {
  const h = hashBytes(Buffer.from("hello"));
  assert.match(h, /^[0-9a-f]{64}$/);
});

test("hashFile matches hashBytes over the same content", () => {
  const dir = tempDir("semantic-exec-hash-");
  try {
    const path = join(dir, "corpus.txtar");
    writeFileSync(path, "-- case one --\nquery\n");
    assert.equal(hashFile(path), hashBytes(Buffer.from("-- case one --\nquery\n")));
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("hashCorpus over a directory changes when a file's content changes", () => {
  const dir = tempDir("semantic-exec-corpus-");
  try {
    writeFileSync(join(dir, "a.yaml"), "query: 1\n");
    writeFileSync(join(dir, "b.yaml"), "query: 2\n");
    const before = hashCorpus(dir);
    writeFileSync(join(dir, "b.yaml"), "query: 3\n");
    const after = hashCorpus(dir);
    assert.notEqual(before, after);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("hashCorpus over a directory changes when a file is added, even with all prior bytes unchanged", () => {
  const dir = tempDir("semantic-exec-corpus-");
  try {
    writeFileSync(join(dir, "a.yaml"), "query: 1\n");
    const before = hashCorpus(dir);
    writeFileSync(join(dir, "b.yaml"), "query: 2\n");
    const after = hashCorpus(dir);
    assert.notEqual(before, after);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("hashCorpus over a directory is independent of directory-listing order", () => {
  const dirA = tempDir("semantic-exec-corpus-a-");
  const dirB = tempDir("semantic-exec-corpus-b-");
  try {
    writeFileSync(join(dirA, "a.yaml"), "1\n");
    writeFileSync(join(dirA, "z.yaml"), "2\n");
    writeFileSync(join(dirB, "z.yaml"), "2\n");
    writeFileSync(join(dirB, "a.yaml"), "1\n");
    assert.equal(hashCorpus(dirA), hashCorpus(dirB));
  } finally {
    rmSync(dirA, { recursive: true, force: true });
    rmSync(dirB, { recursive: true, force: true });
  }
});

test("hashCorpus (array form): folds several corpus roots into one digest, independent of argument order", () => {
  const dirA = tempDir("semantic-exec-corpus-multi-a-");
  const dirB = tempDir("semantic-exec-corpus-multi-b-");
  try {
    writeFileSync(join(dirA, "a.yaml"), "1\n");
    writeFileSync(join(dirB, "b.yaml"), "2\n");
    assert.equal(hashCorpus([dirA, dirB]), hashCorpus([dirB, dirA]));
  } finally {
    rmSync(dirA, { recursive: true, force: true });
    rmSync(dirB, { recursive: true, force: true });
  }
});

test("hashCorpus (array form): drift in EITHER root changes the combined digest", () => {
  const dirA = tempDir("semantic-exec-corpus-multi-a-");
  const dirB = tempDir("semantic-exec-corpus-multi-b-");
  try {
    writeFileSync(join(dirA, "a.yaml"), "1\n");
    writeFileSync(join(dirB, "b.yaml"), "2\n");
    const before = hashCorpus([dirA, dirB]);
    writeFileSync(join(dirB, "b.yaml"), "3\n"); // only the SECOND root changes
    const after = hashCorpus([dirA, dirB]);
    assert.notEqual(before, after, "a single-root fingerprint over dirA alone would have missed this");
  } finally {
    rmSync(dirA, { recursive: true, force: true });
    rmSync(dirB, { recursive: true, force: true });
  }
});

test("hashCorpus (array form): a single-element array does NOT collide with the plain single-path form", () => {
  const dir = tempDir("semantic-exec-corpus-single-");
  try {
    writeFileSync(join(dir, "a.yaml"), "1\n");
    // The array form always folds in the path string alongside each root's
    // digest, so it is a DIFFERENT algorithm from the plain single-path
    // call even with one element — callers (compat-execution-report.mjs)
    // use the plain string form for a single root to keep that digest
    // byte-identical to before the array form existed.
    assert.notEqual(hashCorpus([dir]), hashCorpus(dir));
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("hashCorpus (array form): an empty array is rejected rather than hashing nothing", () => {
  assert.throws(() => hashCorpus([]), SemanticExecutionAdapterError);
});

// --- githubRunContext --------------------------------------------------------

test("githubRunContext reads the standard Actions env vars with no explicit wiring", () => {
  const ctx = githubRunContext({
    GITHUB_SHA: "abc123",
    GITHUB_RUN_ID: "42",
    GITHUB_RUN_ATTEMPT: "1",
    GITHUB_JOB: "tempo",
    GITHUB_EVENT_NAME: "pull_request",
  });
  assert.deepEqual(ctx, {
    sourceSha: "abc123",
    runId: "42",
    runAttempt: "1",
    job: "tempo",
    event: "pull_request",
  });
});

test("githubRunContext narrows an unknown event to null rather than smuggling an unvalidated string", () => {
  const ctx = githubRunContext({ GITHUB_EVENT_NAME: "not-a-real-event" });
  assert.equal(ctx.event, null);
});

// --- parseGoTestJSONShapeResults / propertyShapeObservation (property lane) ---

// A realistic `go test -json` stream shape: package-level run/pass framing,
// a parent test, and two subtests named exactly by ShapeID (matching
// test/property/framework.go's t.Run(string(propertyShapeID), ...)).
function goTestJSONFixture() {
  const lines = [
    { Time: "2026-09-16T00:00:00Z", Action: "run", Package: "p", Test: "TestPromQL_Property_FromScratch" },
    { Time: "2026-09-16T00:00:01Z", Action: "run", Package: "p", Test: "TestPromQL_Property_FromScratch/promql.instant.selector" },
    { Time: "2026-09-16T00:00:02Z", Action: "pass", Package: "p", Test: "TestPromQL_Property_FromScratch/promql.instant.selector", Elapsed: 0.4 },
    { Time: "2026-09-16T00:00:02Z", Action: "run", Package: "p", Test: "TestPromQL_Property_FromScratch/promql.instant.sum" },
    { Time: "2026-09-16T00:00:03Z", Action: "fail", Package: "p", Test: "TestPromQL_Property_FromScratch/promql.instant.sum", Elapsed: 0.6 },
    { Time: "2026-09-16T00:00:04Z", Action: "fail", Package: "p", Test: "TestPromQL_Property_FromScratch" },
    { Time: "2026-09-16T00:00:04Z", Action: "fail", Package: "p" },
  ];
  return lines.map((l) => JSON.stringify(l)).join("\n") + "\n";
}

test("parseGoTestJSONShapeResults collapses each subtest to its terminal action, keyed by its last path segment", () => {
  const bySegment = parseGoTestJSONShapeResults(goTestJSONFixture());
  assert.equal(bySegment.get("promql.instant.selector"), "pass");
  assert.equal(bySegment.get("promql.instant.sum"), "fail");
  // The parent test's OWN terminal event ("TestPromQL_Property_FromScratch",
  // no further "/") also lands under its own (non-shape) key — harmless,
  // since propertyShapeObservation only ever looks up real ShapeIDs.
  assert.equal(bySegment.get("TestPromQL_Property_FromScratch"), "fail");
});

test("parseGoTestJSONShapeResults tolerates a stray non-JSON line without failing the whole parse", () => {
  const text = goTestJSONFixture() + "PASS\nok  \tsomepkg\t1.234s\n";
  const bySegment = parseGoTestJSONShapeResults(text);
  assert.equal(bySegment.get("promql.instant.selector"), "pass");
});

test("parseGoTestJSONShapeResults over an empty stream returns an empty map", () => {
  assert.equal(parseGoTestJSONShapeResults("").size, 0);
  assert.equal(parseGoTestJSONShapeResults("\n\n").size, 0);
});

test("propertyShapeObservation: a shape present and passing is selected+ran with result pass", () => {
  const bySegment = parseGoTestJSONShapeResults(goTestJSONFixture());
  const obs = propertyShapeObservation(bySegment, "promql.instant.selector");
  assert.deepEqual(obs, { selectedCount: 1, ranCount: 1, result: "pass" });
});

test("propertyShapeObservation: a shape absent from the stream is NOT selected (zero, not a guess)", () => {
  const bySegment = parseGoTestJSONShapeResults(goTestJSONFixture());
  const obs = propertyShapeObservation(bySegment, "promql.instant.never_seen");
  assert.deepEqual(obs, { selectedCount: 0, ranCount: 0, result: "error" });
});

test("propertyShapeObservation: a t.Skip'd shape (defensive — this repo never emits one) is selected but not a real result", () => {
  const bySegment = new Map([["promql.skipped", "skip"]]);
  const obs = propertyShapeObservation(bySegment, "promql.skipped");
  assert.deepEqual(obs, { selectedCount: 1, ranCount: 1, result: "error" });
});

// --- parseCaseSet (compat lane) -----------------------------------------------

test("parseCaseSet: every case agreeing yields result pass", () => {
  const obs = parseCaseSet({ head: "tempo", cases: [{ id: "a", passed: true }, { id: "b", passed: true }] });
  assert.deepEqual(obs, { head: "tempo", selectedCount: 2, ranCount: 2, result: "pass" });
});

test("parseCaseSet: one disagreeing case yields result fail for the whole aggregate", () => {
  const obs = parseCaseSet({ head: "tempo", cases: [{ id: "a", passed: true }, { id: "b", passed: false }] });
  assert.equal(obs.result, "fail");
});

test("parseCaseSet: zero cases (aggregate no-op / empty corpus) yields result error, never a vacuous pass", () => {
  const obs = parseCaseSet({ head: "tempo", cases: [] });
  assert.deepEqual(obs, { head: "tempo", selectedCount: 0, ranCount: 0, result: "error" });
});

test("parseCaseSet: a malformed document (no head) throws rather than guessing", () => {
  assert.throws(() => parseCaseSet({ cases: [] }), SemanticExecutionAdapterError);
  assert.throws(() => parseCaseSet({ head: "tempo" }), SemanticExecutionAdapterError);
  assert.throws(() => parseCaseSet(null), SemanticExecutionAdapterError);
});

// --- End-to-end: the real CLI -------------------------------------------------

// A minimal, valid six-document semantic model with one property-shape
// binding and one compat binding, written to a temp SEMANTIC_MODEL_DIR so
// the CLI's loadSemanticModel() call resolves against it.
function writeMinimalModel(dir) {
  const heads = {
    schema_version: 2,
    heads: [
      { id: "HEAD-PROMQL", name: "PromQL", description: "d", status: "active", owner: "core", replaces: null, replaced_by: null },
      { id: "HEAD-LOGQL", name: "LogQL", description: "d", status: "active", owner: "core", replaces: null, replaced_by: null },
      { id: "HEAD-TRACEQL", name: "TraceQL", description: "d", status: "active", owner: "core", replaces: null, replaced_by: null },
    ],
  };
  const capabilities = { schema_version: 2, capabilities: [] };
  const contracts = {
    schema_version: 2,
    contracts: [
      {
        id: "PROMQL-EXAMPLE",
        statement: "An example statement.",
        scope: "head",
        applicable_heads: ["HEAD-PROMQL"],
        applicable_capabilities: [],
        authority: "specification",
        required_evidence_classes: ["property"],
        required_independence_groups: [],
        blind_spots: [],
        related_contracts: [],
        inherits_from: null,
        status: "active",
        deficit_reason: null,
        owner: "core",
        replaces: null,
        replaced_by: null,
      },
    ],
  };
  const verifiers = {
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
  const bindings = {
    schema_version: 2,
    bindings: [
      {
        id: "BINDING-PROPERTY-EXAMPLE",
        contract: "PROMQL-EXAMPLE",
        verifier: "VERIFIER-EXAMPLE",
        evidence_class: "property",
        independence_group: "property",
        test_ref: "test/property/gen#promql.instant.selector",
        status: "active",
      },
      {
        id: "BINDING-COMPAT-EXAMPLE",
        contract: "PROMQL-EXAMPLE",
        verifier: "VERIFIER-EXAMPLE",
        evidence_class: "reference",
        independence_group: "reference-differential",
        test_ref: "compatibility/prometheus",
        status: "active",
      },
    ],
  };
  const executions = { schema_version: 2, executions: [] };
  writeFileSync(join(dir, "heads.json"), JSON.stringify(heads));
  writeFileSync(join(dir, "capabilities.json"), JSON.stringify(capabilities));
  writeFileSync(join(dir, "contracts.json"), JSON.stringify(contracts));
  writeFileSync(join(dir, "verifiers.json"), JSON.stringify(verifiers));
  writeFileSync(join(dir, "bindings.json"), JSON.stringify(bindings));
  writeFileSync(join(dir, "executions.json"), JSON.stringify(executions));
}

function runCli(env) {
  return spawnSync(process.execPath, [CLI_PATH], {
    cwd: REPO_ROOT,
    env: { ...process.env, ...env },
    encoding: "utf8",
  });
}

test("CLI MODE=property: emits one record for the matching shape binding, executed+pass", () => {
  const modelDir = tempDir("semantic-exec-cli-model-");
  const dataDir = tempDir("semantic-exec-cli-data-");
  try {
    writeMinimalModel(modelDir);
    const gotestPath = join(dataDir, "gotest.json");
    writeFileSync(gotestPath, goTestJSONFixture());
    const result = runCli({
      MODE: "property",
      MODEL_DIR: modelDir,
      GOTEST_JSON_PATH: gotestPath,
      CANDIDATE_SHA: "abc1234",
      RUN_REF: "https://example.invalid/run/1",
      OBSERVED_AT: "2026-09-16T00:00:00Z",
      GITHUB_SHA: "",
      GITHUB_EVENT_NAME: "",
    });
    assert.equal(result.status, 0, result.stderr);
    const records = JSON.parse(result.stdout);
    assert.equal(records.length, 1);
    assert.equal(records[0].binding, "BINDING-PROPERTY-EXAMPLE");
    assert.equal(records[0].selection, "executed");
    assert.equal(records[0].result, "pass");
    assert.equal(records[0].source_sha, "abc1234");
  } finally {
    rmSync(modelDir, { recursive: true, force: true });
    rmSync(dataDir, { recursive: true, force: true });
  }
});

test("CLI MODE=property: a shape absent from the stream reports selected_not_run, not silence", () => {
  const modelDir = tempDir("semantic-exec-cli-model-");
  const dataDir = tempDir("semantic-exec-cli-data-");
  try {
    writeMinimalModel(modelDir);
    const gotestPath = join(dataDir, "gotest.json");
    // A stream that ran SOMETHING (so it is not an aggregate no-op) but
    // never touched this binding's own shape.
    writeFileSync(
      gotestPath,
      `${JSON.stringify({ Action: "pass", Test: "TestOther/some.other.shape" })}\n`,
    );
    const result = runCli({
      MODE: "property",
      MODEL_DIR: modelDir,
      GOTEST_JSON_PATH: gotestPath,
      CANDIDATE_SHA: "abc1234",
      RUN_REF: "https://example.invalid/run/1",
      OBSERVED_AT: "2026-09-16T00:00:00Z",
      GITHUB_SHA: "",
      GITHUB_EVENT_NAME: "",
    });
    assert.equal(result.status, 0, result.stderr);
    const records = JSON.parse(result.stdout);
    assert.equal(records[0].selection, "selected_not_run");
    assert.equal(records[0].result, "error");
    assert.match(result.stderr, /1\/1 record\(s\) are NON-EVIDENCE/);
  } finally {
    rmSync(modelDir, { recursive: true, force: true });
    rmSync(dataDir, { recursive: true, force: true });
  }
});

test("CLI MODE=property: an empty go-test-json stream is an aggregate no-op for every shape binding", () => {
  const modelDir = tempDir("semantic-exec-cli-model-");
  const dataDir = tempDir("semantic-exec-cli-data-");
  try {
    writeMinimalModel(modelDir);
    const gotestPath = join(dataDir, "gotest.json");
    writeFileSync(gotestPath, "");
    const result = runCli({
      MODE: "property",
      MODEL_DIR: modelDir,
      GOTEST_JSON_PATH: gotestPath,
      CANDIDATE_SHA: "abc1234",
      RUN_REF: "https://example.invalid/run/1",
      OBSERVED_AT: "2026-09-16T00:00:00Z",
      GITHUB_SHA: "",
      GITHUB_EVENT_NAME: "",
    });
    assert.equal(result.status, 0, result.stderr);
    const records = JSON.parse(result.stdout);
    assert.equal(records[0].selection, "no_op");
  } finally {
    rmSync(modelDir, { recursive: true, force: true });
    rmSync(dataDir, { recursive: true, force: true });
  }
});

test("CLI MODE=compat: a fully-passing case set against the right binding is executed+pass, with a real corpus fingerprint", () => {
  const modelDir = tempDir("semantic-exec-cli-model-");
  const dataDir = tempDir("semantic-exec-cli-data-");
  try {
    writeMinimalModel(modelDir);
    const casesPath = join(dataDir, "compat-cases.json");
    writeFileSync(casesPath, JSON.stringify({ head: "prometheus", cases: [{ id: "q1", passed: true }, { id: "q2", passed: true }] }));
    const corpusDir = join(dataDir, "corpus");
    mkdirSync(corpusDir);
    writeFileSync(join(corpusDir, "q1.yaml"), "query: up\n");
    const result = runCli({
      MODE: "compat",
      MODEL_DIR: modelDir,
      CASES_PATH: casesPath,
      BINDING: "BINDING-COMPAT-EXAMPLE",
      CORPUS_PATH: corpusDir,
      REFERENCE_VERSION: "prometheus-2.53.0",
      CANDIDATE_SHA: "abc1234",
      RUN_REF: "https://example.invalid/run/1",
      OBSERVED_AT: "2026-09-16T00:00:00Z",
      GITHUB_SHA: "",
      GITHUB_EVENT_NAME: "",
    });
    assert.equal(result.status, 0, result.stderr);
    const records = JSON.parse(result.stdout);
    assert.equal(records.length, 1);
    assert.equal(records[0].binding, "BINDING-COMPAT-EXAMPLE");
    assert.equal(records[0].selection, "executed");
    assert.equal(records[0].result, "pass");
    assert.equal(records[0].reference_version, "prometheus-2.53.0");
    assert.match(records[0].dataset_fingerprint, /^[0-9a-f]{64}$/);
  } finally {
    rmSync(modelDir, { recursive: true, force: true });
    rmSync(dataDir, { recursive: true, force: true });
  }
});

test("CLI MODE=compat: an unknown BINDING is rejected rather than silently fanning out", () => {
  const modelDir = tempDir("semantic-exec-cli-model-");
  const dataDir = tempDir("semantic-exec-cli-data-");
  try {
    writeMinimalModel(modelDir);
    const casesPath = join(dataDir, "compat-cases.json");
    writeFileSync(casesPath, JSON.stringify({ head: "prometheus", cases: [] }));
    const result = runCli({
      MODE: "compat",
      MODEL_DIR: modelDir,
      CASES_PATH: casesPath,
      BINDING: "BINDING-DOES-NOT-EXIST",
      CANDIDATE_SHA: "abc1234",
      GITHUB_SHA: "",
      GITHUB_EVENT_NAME: "",
    });
    assert.equal(result.status, 1);
    assert.match(result.stderr, /::error title=Semantic execution adapter::/);
    assert.match(result.stderr, /not an active binding/);
  } finally {
    rmSync(modelDir, { recursive: true, force: true });
    rmSync(dataDir, { recursive: true, force: true });
  }
});

test("CLI MODE=verify: a stale corpus against a re-hashed candidate is flagged, not silently still valid", () => {
  const modelDir = tempDir("semantic-exec-cli-model-");
  const dataDir = tempDir("semantic-exec-cli-data-");
  try {
    writeMinimalModel(modelDir);
    // Seed one already-recorded, revision-bound execution directly into
    // executions.json — the shape a real appended record would carry.
    const executions = {
      schema_version: 2,
      executions: [
        {
          id: "EXEC-COMPAT-EXAMPLE-1",
          binding: "BINDING-COMPAT-EXAMPLE",
          observed_at: "2026-09-01T00:00:00Z",
          result: "pass",
          run_ref: "https://example.invalid/run/1",
          selection: "executed",
          selection_reason: null,
          source_sha: "abc1234",
          run_id: "1",
          run_attempt: "1",
          job: "prometheus",
          event: "push",
          substrate: "reference-stack",
          reference_version: null,
          dataset_fingerprint: "0".repeat(64),
        },
      ],
    };
    writeFileSync(join(modelDir, "executions.json"), JSON.stringify(executions));

    const corpusDir = join(dataDir, "corpus");
    mkdirSync(corpusDir);
    writeFileSync(join(corpusDir, "q1.yaml"), "query: up\n"); // hashes to something other than "0"*64

    const result = runCli({
      MODE: "verify",
      MODEL_DIR: modelDir,
      EXECUTION_ID: "EXEC-COMPAT-EXAMPLE-1",
      CANDIDATE_SHA: "abc1234",
      CORPUS_PATH: corpusDir,
      GITHUB_SHA: "",
      GITHUB_EVENT_NAME: "",
    });
    assert.equal(result.status, 0, result.stderr);
    const [verdict] = JSON.parse(result.stdout);
    assert.equal(verdict.still_valid_evidence, false);
    assert.equal(verdict.classification.selection, "stale");
  } finally {
    rmSync(modelDir, { recursive: true, force: true });
    rmSync(dataDir, { recursive: true, force: true });
  }
});

test("CLI: MODE is required and validated", () => {
  const result = runCli({ MODE: "", GITHUB_SHA: "", GITHUB_EVENT_NAME: "" });
  assert.equal(result.status, 1);
  assert.match(result.stderr, /MODE must be one of property, compat, verify/);
});

// --- SOFT_FAIL (issue #3499's CI wiring: a protected/release-required lane
// cannot use `continue-on-error: true`, so the CLI itself has to guarantee
// it never fails the job when this is set) ------------------------------

test("CLI: SOFT_FAIL=1 turns an error that would exit 1 into a ::warning:: and exit 0", () => {
  const result = runCli({ MODE: "", SOFT_FAIL: "1", GITHUB_SHA: "", GITHUB_EVENT_NAME: "" });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /::warning title=Semantic execution adapter \(soft-fail\)::/);
  assert.match(result.stdout, /MODE must be one of property, compat, verify/);
  assert.doesNotMatch(result.stderr, /::error/);
});

test("CLI: SOFT_FAIL unset (or anything other than \"1\") keeps the default hard-fail behavior", () => {
  const result = runCli({ MODE: "", SOFT_FAIL: "true", GITHUB_SHA: "", GITHUB_EVENT_NAME: "" });
  assert.equal(result.status, 1);
  assert.match(result.stderr, /::error title=Semantic execution adapter::/);
});

test("CLI: SOFT_FAIL=1 never masks a genuinely successful run — the real records still print", () => {
  const modelDir = tempDir("semantic-exec-cli-model-");
  const dataDir = tempDir("semantic-exec-cli-data-");
  try {
    writeMinimalModel(modelDir);
    const gotestPath = join(dataDir, "gotest.json");
    writeFileSync(gotestPath, goTestJSONFixture());
    const result = runCli({
      MODE: "property",
      SOFT_FAIL: "1",
      MODEL_DIR: modelDir,
      GOTEST_JSON_PATH: gotestPath,
      CANDIDATE_SHA: "abc1234",
      RUN_REF: "https://example.invalid/run/1",
      OBSERVED_AT: "2026-09-16T00:00:00Z",
      GITHUB_SHA: "",
      GITHUB_EVENT_NAME: "",
    });
    assert.equal(result.status, 0, result.stderr);
    const records = JSON.parse(result.stdout);
    assert.equal(records[0].selection, "executed");
  } finally {
    rmSync(modelDir, { recursive: true, force: true });
    rmSync(dataDir, { recursive: true, force: true });
  }
});
