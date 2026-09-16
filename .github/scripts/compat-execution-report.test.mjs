// compat-execution-report.test.mjs — node --test guard for
// compat-execution-report.mjs (cerberus issue #3499): the multi-binding
// orchestration that fans one compat CI run's case-set artifact(s) out over
// every active "reference" binding under compatibility/<head>, reusing
// lib/semantic-execution-adapter.mjs's own classify/normalize functions
// with no logic of its own beyond that fan-out.

import assert from "node:assert/strict";
import { test } from "node:test";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

import { isGrpcBinding, parseCorpusPath, selectBindings } from "./compat-execution-report.mjs";

const SCRIPT_DIR = fileURLToPath(new URL(".", import.meta.url));
const REPO_ROOT = process.cwd();
const CLI_PATH = join(SCRIPT_DIR, "compat-execution-report.mjs");

function tempDir(prefix) {
  return mkdtempSync(join(tmpdir(), prefix));
}

// --- isGrpcBinding -----------------------------------------------------------

test("isGrpcBinding: matches a test_ref naming a gRPC driver file, case-insensitively", () => {
  assert.equal(isGrpcBinding("compatibility/tempo/driver/grpc_diff.go"), true);
  assert.equal(isGrpcBinding("compatibility/tempo/driver/GRPC_diff.go"), true);
});

test("isGrpcBinding: does not match the HTTP transport or the live-reference oracle", () => {
  assert.equal(isGrpcBinding("compatibility/tempo/driver/diff.go"), false);
  assert.equal(isGrpcBinding("compatibility/tempo/driver/differ.go"), false);
});

// --- parseCorpusPath -----------------------------------------------------------

test("parseCorpusPath: a single path (no colon) passes through as a plain string", () => {
  assert.equal(parseCorpusPath("compatibility/prometheus/query-corpus"), "compatibility/prometheus/query-corpus");
});

test("parseCorpusPath: colon-joined paths become an array", () => {
  assert.deepEqual(
    parseCorpusPath("compatibility/loki/upstream/loki-bench/queries:compatibility/loki/cerberus-queries"),
    ["compatibility/loki/upstream/loki-bench/queries", "compatibility/loki/cerberus-queries"],
  );
});

test("parseCorpusPath: a stray leading/trailing/doubled colon never produces an empty path segment", () => {
  assert.deepEqual(parseCorpusPath(":a:b:"), ["a", "b"]);
  assert.deepEqual(parseCorpusPath("a::b"), ["a", "b"]);
});

// --- selectBindings ------------------------------------------------------------

function modelWith(bindings) {
  return { bindings: new Map(bindings.map((b) => [b.id, b])) };
}

test("selectBindings: only active + reference-evidence bindings under compatibility/<head> are selected", () => {
  const model = modelWith([
    { id: "BINDING-A", status: "active", evidence_class: "reference", test_ref: "compatibility/prometheus" },
    { id: "BINDING-B", status: "active", evidence_class: "reference", test_ref: "compatibility/prometheus/query-corpus/header.yml" },
    { id: "BINDING-C", status: "superseded", evidence_class: "reference", test_ref: "compatibility/prometheus" },
    { id: "BINDING-D", status: "active", evidence_class: "manual-review", test_ref: "compatibility/prometheus/query-corpus/header.yml" },
    { id: "BINDING-E", status: "active", evidence_class: "reference", test_ref: "compatibility/loki" },
    { id: "BINDING-F", status: "active", evidence_class: "property", test_ref: "test/property/gen#promql.instant.selector" },
  ]);
  const selected = selectBindings(model, "prometheus").map((b) => b.id).sort();
  assert.deepEqual(selected, ["BINDING-A", "BINDING-B"]);
});

test("selectBindings: an empty result when nothing matches the head prefix", () => {
  const model = modelWith([
    { id: "BINDING-A", status: "active", evidence_class: "reference", test_ref: "compatibility/loki" },
  ]);
  assert.deepEqual(selectBindings(model, "tempo"), []);
});

// --- End-to-end: the real CLI ---------------------------------------------------

function writeMinimalModel(dir, bindings) {
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
    contracts: bindings.map((b, i) => ({
      id: `PROMQL-EXAMPLE-${i}`,
      statement: "An example statement.",
      scope: "head",
      applicable_heads: ["HEAD-PROMQL"],
      applicable_capabilities: [],
      authority: "specification",
      required_evidence_classes: [b.evidence_class],
      required_independence_groups: [],
      blind_spots: [],
      related_contracts: [],
      inherits_from: null,
      status: "active",
      deficit_reason: null,
      owner: "core",
      replaces: null,
      replaced_by: null,
    })),
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
        substrate: "reference-stack",
        relative_cost: "high",
        status: "active",
        replaces: null,
        replaced_by: null,
      },
    ],
  };
  const bindingsDoc = {
    schema_version: 2,
    bindings: bindings.map((b, i) => ({
      id: b.id,
      contract: `PROMQL-EXAMPLE-${i}`,
      verifier: "VERIFIER-EXAMPLE",
      evidence_class: b.evidence_class,
      independence_group: "reference-differential",
      test_ref: b.test_ref,
      status: b.status,
    })),
  };
  const executions = { schema_version: 2, executions: [] };
  writeFileSync(join(dir, "heads.json"), JSON.stringify(heads));
  writeFileSync(join(dir, "capabilities.json"), JSON.stringify(capabilities));
  writeFileSync(join(dir, "contracts.json"), JSON.stringify(contracts));
  writeFileSync(join(dir, "verifiers.json"), JSON.stringify(verifiers));
  writeFileSync(join(dir, "bindings.json"), JSON.stringify(bindingsDoc));
  writeFileSync(join(dir, "executions.json"), JSON.stringify(executions));
}

function runCli(env) {
  return spawnSync(process.execPath, [CLI_PATH], {
    cwd: REPO_ROOT,
    env: { ...process.env, ...env },
    encoding: "utf8",
  });
}

test("CLI: prometheus-shaped model — every active reference binding under the head gets its own record from the one case set", () => {
  const modelDir = tempDir("compat-exec-cli-model-");
  const dataDir = tempDir("compat-exec-cli-data-");
  try {
    writeMinimalModel(modelDir, [
      { id: "BINDING-PROMQL-A", evidence_class: "reference", test_ref: "compatibility/prometheus", status: "active" },
      { id: "BINDING-PROMQL-B", evidence_class: "reference", test_ref: "compatibility/prometheus/query-corpus/header.yml", status: "active" },
      { id: "BINDING-PROMQL-REVIEW", evidence_class: "manual-review", test_ref: "compatibility/prometheus/query-corpus/header.yml", status: "active" },
    ]);
    const casesPath = join(dataDir, "compat-cases.json");
    writeFileSync(casesPath, JSON.stringify({ head: "prometheus", cases: [{ id: "q1", passed: true }, { id: "q2", passed: true }] }));
    const result = runCli({
      HEAD: "prometheus",
      MODEL_DIR: modelDir,
      CASES_PATH: casesPath,
      CANDIDATE_SHA: "abc1234",
      RUN_REF: "https://example.invalid/run/1",
      OBSERVED_AT: "2026-09-16T00:00:00Z",
      GITHUB_SHA: "",
      GITHUB_EVENT_NAME: "",
    });
    assert.equal(result.status, 0, result.stderr);
    const records = JSON.parse(result.stdout);
    assert.equal(records.length, 2);
    const bindingIds = records.map((r) => r.binding).sort();
    assert.deepEqual(bindingIds, ["BINDING-PROMQL-A", "BINDING-PROMQL-B"]);
    for (const r of records) {
      assert.equal(r.selection, "executed");
      assert.equal(r.result, "pass");
      assert.equal(r.source_sha, "abc1234");
    }
  } finally {
    rmSync(modelDir, { recursive: true, force: true });
    rmSync(dataDir, { recursive: true, force: true });
  }
});

test("CLI: tempo-shaped model — a gRPC-named binding routes to CASES_PATH_GRPC, the HTTP-named one to CASES_PATH", () => {
  const modelDir = tempDir("compat-exec-cli-model-");
  const dataDir = tempDir("compat-exec-cli-data-");
  try {
    writeMinimalModel(modelDir, [
      { id: "BINDING-TRACEQL-TRANSPORT-ARM-HTTP", evidence_class: "reference", test_ref: "compatibility/tempo/driver/diff.go", status: "active" },
      { id: "BINDING-TRACEQL-TRANSPORT-ARM-GRPC", evidence_class: "reference", test_ref: "compatibility/tempo/driver/grpc_diff.go", status: "active" },
    ]);
    const casesPath = join(dataDir, "compat-cases.json");
    const casesPathGrpc = join(dataDir, "compat-cases-grpc.json");
    writeFileSync(casesPath, JSON.stringify({ head: "tempo", cases: [{ id: "q1", passed: true }] }));
    writeFileSync(casesPathGrpc, JSON.stringify({ head: "tempo", cases: [{ id: "q1", passed: false }] }));
    const result = runCli({
      HEAD: "tempo",
      MODEL_DIR: modelDir,
      CASES_PATH: casesPath,
      CASES_PATH_GRPC: casesPathGrpc,
      CANDIDATE_SHA: "abc1234",
      RUN_REF: "https://example.invalid/run/1",
      OBSERVED_AT: "2026-09-16T00:00:00Z",
      GITHUB_SHA: "",
      GITHUB_EVENT_NAME: "",
    });
    assert.equal(result.status, 0, result.stderr);
    const records = JSON.parse(result.stdout);
    assert.equal(records.length, 2);
    const http = records.find((r) => r.binding === "BINDING-TRACEQL-TRANSPORT-ARM-HTTP");
    const grpc = records.find((r) => r.binding === "BINDING-TRACEQL-TRANSPORT-ARM-GRPC");
    // A real fail is STILL executed evidence (of failure), not non-evidence —
    // see classifyRevisionBinding's own contract, exercised end to end here.
    assert.equal(http.selection, "executed");
    assert.equal(http.result, "pass");
    assert.equal(grpc.selection, "executed");
    assert.equal(grpc.result, "fail");
  } finally {
    rmSync(modelDir, { recursive: true, force: true });
    rmSync(dataDir, { recursive: true, force: true });
  }
});

test("CLI: a zero-case set is real non-evidence (selected_not_run), reported on stderr", () => {
  const modelDir = tempDir("compat-exec-cli-model-");
  const dataDir = tempDir("compat-exec-cli-data-");
  try {
    writeMinimalModel(modelDir, [
      { id: "BINDING-LOGQL-A", evidence_class: "reference", test_ref: "compatibility/loki", status: "active" },
    ]);
    const casesPath = join(dataDir, "compat-cases.json");
    writeFileSync(casesPath, JSON.stringify({ head: "loki", cases: [] }));
    const result = runCli({
      HEAD: "loki",
      MODEL_DIR: modelDir,
      CASES_PATH: casesPath,
      CANDIDATE_SHA: "abc1234",
      GITHUB_SHA: "",
      GITHUB_EVENT_NAME: "",
    });
    assert.equal(result.status, 0, result.stderr);
    const [record] = JSON.parse(result.stdout);
    assert.equal(record.selection, "selected_not_run");
    assert.equal(record.result, "error");
    assert.match(result.stderr, /1\/1 record\(s\) are NON-EVIDENCE/);
  } finally {
    rmSync(modelDir, { recursive: true, force: true });
    rmSync(dataDir, { recursive: true, force: true });
  }
});

test("CLI: no matching active reference binding under the head is a hard error, not a silent empty report", () => {
  const modelDir = tempDir("compat-exec-cli-model-");
  const dataDir = tempDir("compat-exec-cli-data-");
  try {
    writeMinimalModel(modelDir, [
      { id: "BINDING-LOGQL-A", evidence_class: "reference", test_ref: "compatibility/loki", status: "active" },
    ]);
    const casesPath = join(dataDir, "compat-cases.json");
    writeFileSync(casesPath, JSON.stringify({ head: "tempo", cases: [] }));
    const result = runCli({
      HEAD: "tempo",
      MODEL_DIR: modelDir,
      CASES_PATH: casesPath,
      CANDIDATE_SHA: "abc1234",
      GITHUB_SHA: "",
      GITHUB_EVENT_NAME: "",
    });
    assert.equal(result.status, 1);
    assert.match(result.stdout + result.stderr, /no active "reference" binding/);
  } finally {
    rmSync(modelDir, { recursive: true, force: true });
    rmSync(dataDir, { recursive: true, force: true });
  }
});

test("CLI: HEAD and CASES_PATH are required", () => {
  const result1 = runCli({ HEAD: "", CASES_PATH: "", GITHUB_SHA: "", GITHUB_EVENT_NAME: "" });
  assert.equal(result1.status, 1);
  assert.match(result1.stdout + result1.stderr, /HEAD is required/);

  const result2 = runCli({ HEAD: "prometheus", CASES_PATH: "", GITHUB_SHA: "", GITHUB_EVENT_NAME: "" });
  assert.equal(result2.status, 1);
  assert.match(result2.stdout + result2.stderr, /CASES_PATH is required/);
});

test("CLI: writes to OUT when given, with a confirmation line on stdout instead of the raw JSON", () => {
  const modelDir = tempDir("compat-exec-cli-model-");
  const dataDir = tempDir("compat-exec-cli-data-");
  try {
    writeMinimalModel(modelDir, [
      { id: "BINDING-LOGQL-A", evidence_class: "reference", test_ref: "compatibility/loki", status: "active" },
    ]);
    const casesPath = join(dataDir, "compat-cases.json");
    writeFileSync(casesPath, JSON.stringify({ head: "loki", cases: [{ id: "q1", passed: true }] }));
    const outPath = join(dataDir, "compat-executions-loki.json");
    const result = runCli({
      HEAD: "loki",
      MODEL_DIR: modelDir,
      CASES_PATH: casesPath,
      OUT: outPath,
      CANDIDATE_SHA: "abc1234",
      GITHUB_SHA: "",
      GITHUB_EVENT_NAME: "",
    });
    assert.equal(result.status, 0, result.stderr);
    assert.match(result.stdout, /wrote 1 record\(s\) for loki/);
    const written = JSON.parse(readFileSync(outPath, "utf8"));
    assert.equal(written.length, 1);
    assert.equal(written[0].binding, "BINDING-LOGQL-A");
  } finally {
    rmSync(modelDir, { recursive: true, force: true });
    rmSync(dataDir, { recursive: true, force: true });
  }
});

test("CLI: a loki-shaped two-root CORPUS_PATH detects drift in EITHER root, not just the first", () => {
  const modelDir = tempDir("compat-exec-cli-model-");
  const dataDir = tempDir("compat-exec-cli-data-");
  const rootA = tempDir("compat-exec-cli-corpus-a-");
  const rootB = tempDir("compat-exec-cli-corpus-b-");
  try {
    writeMinimalModel(modelDir, [
      { id: "BINDING-LOGQL-A", evidence_class: "reference", test_ref: "compatibility/loki", status: "active" },
    ]);
    writeFileSync(join(rootA, "a.yaml"), "query: 1\n");
    writeFileSync(join(rootB, "b.yaml"), "query: 2\n");
    const casesPath = join(dataDir, "compat-cases.json");
    writeFileSync(casesPath, JSON.stringify({ head: "loki", cases: [{ id: "q1", passed: true }] }));

    const runWithCorpus = () => {
      const r = runCli({
        HEAD: "loki",
        MODEL_DIR: modelDir,
        CASES_PATH: casesPath,
        CORPUS_PATH: `${rootA}:${rootB}`,
        CANDIDATE_SHA: "abc1234",
        GITHUB_SHA: "",
        GITHUB_EVENT_NAME: "",
      });
      assert.equal(r.status, 0, r.stderr);
      return JSON.parse(r.stdout)[0].dataset_fingerprint;
    };

    const before = runWithCorpus();
    writeFileSync(join(rootB, "b.yaml"), "query: 3\n"); // only the SECOND root (cerberus-queries analogue) changes
    const after = runWithCorpus();
    assert.notEqual(
      before,
      after,
      "a fingerprint over only the first corpus root would have missed drift confined to the second",
    );
  } finally {
    rmSync(modelDir, { recursive: true, force: true });
    rmSync(dataDir, { recursive: true, force: true });
    rmSync(rootA, { recursive: true, force: true });
    rmSync(rootB, { recursive: true, force: true });
  }
});
