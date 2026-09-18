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

import {
  guardCorpusFileAttribution,
  isCorpusFileScopedBinding,
  isGrpcBinding,
  parseCorpusPath,
  selectBindings,
} from "./compat-execution-report.mjs";

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

// --- isCorpusFileScopedBinding / guardCorpusFileAttribution (issue #3508) ------

test("isCorpusFileScopedBinding: a corpus data file (.yml/.yaml) is file-scoped", () => {
  assert.equal(isCorpusFileScopedBinding("compatibility/prometheus/query-corpus/header.yml"), true);
  assert.equal(isCorpusFileScopedBinding("compatibility/loki/cerberus-queries/regression/anchored-regex-matcher.yaml"), true);
  assert.equal(isCorpusFileScopedBinding("compatibility/prometheus/QUERY-CORPUS/Header.YML"), true);
});

test("isCorpusFileScopedBinding: the bare head directory or a driver source file (.go) is whole-invocation-scoped", () => {
  assert.equal(isCorpusFileScopedBinding("compatibility/prometheus"), false);
  assert.equal(isCorpusFileScopedBinding("compatibility/loki"), false);
  assert.equal(isCorpusFileScopedBinding("compatibility/tempo/driver/diff.go"), false);
  assert.equal(isCorpusFileScopedBinding("compatibility/tempo/driver/grpc_diff.go"), false);
  assert.equal(isCorpusFileScopedBinding("compatibility/loki/cmd/loki-compliance-tester/status_parity.go"), false);
});

test("guardCorpusFileAttribution: a file-scoped binding's executed/fail record degrades to unavailable non-evidence", () => {
  const rec = guardCorpusFileAttribution(
    { selection: "executed", result: "fail", selection_reason: null, binding: "BINDING-X" },
    "compatibility/prometheus/query-corpus/header.yml",
  );
  assert.equal(rec.selection, "unavailable");
  assert.equal(rec.result, "error");
  assert.match(rec.selection_reason, /#3508/);
  // Every other field survives the override untouched.
  assert.equal(rec.binding, "BINDING-X");
});

test("guardCorpusFileAttribution: a file-scoped binding's executed/pass record is left completely alone", () => {
  const rec = guardCorpusFileAttribution(
    { selection: "executed", result: "pass", selection_reason: null },
    "compatibility/prometheus/query-corpus/header.yml",
  );
  assert.deepEqual(rec, { selection: "executed", result: "pass", selection_reason: null });
});

test("guardCorpusFileAttribution: a driver-scoped (whole-invocation) binding's fail is never touched", () => {
  const rec = guardCorpusFileAttribution({ selection: "executed", result: "fail", selection_reason: null }, "compatibility/prometheus");
  assert.deepEqual(rec, { selection: "executed", result: "fail", selection_reason: null });
});

test("guardCorpusFileAttribution: a non-executed record (e.g. already unavailable/stale) is never re-stamped", () => {
  const rec = guardCorpusFileAttribution(
    { selection: "stale", result: "error", selection_reason: "corpus drifted" },
    "compatibility/prometheus/query-corpus/header.yml",
  );
  assert.deepEqual(rec, { selection: "stale", result: "error", selection_reason: "corpus drifted" });
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

test("selectBindings: the head prefix match is anchored at a path segment boundary (issue #3511 item 3)", () => {
  const model = modelWith([
    { id: "BINDING-LOKI", status: "active", evidence_class: "reference", test_ref: "compatibility/loki" },
    // A hypothetical future compatibility/loki-foo binding must NOT be
    // pulled into HEAD=loki's fan-out merely because "compatibility/loki-foo"
    // starts with the string "compatibility/loki".
    { id: "BINDING-LOKI-FOO", status: "active", evidence_class: "reference", test_ref: "compatibility/loki-foo/driver.go" },
  ]);
  assert.deepEqual(selectBindings(model, "loki").map((b) => b.id), ["BINDING-LOKI"]);
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

test("CLI (issue #3508): an UNRELATED case failing does not stamp a corpus-file-scoped binding as fail, but the whole-run binding still is", () => {
  const modelDir = tempDir("compat-exec-cli-model-");
  const dataDir = tempDir("compat-exec-cli-data-");
  try {
    writeMinimalModel(modelDir, [
      { id: "BINDING-PROMQL-RANGE-ALIGNMENT-COMPAT", evidence_class: "reference", test_ref: "compatibility/prometheus", status: "active" },
      { id: "BINDING-PROMQL-NATIVE-HISTOGRAM-COMPAT", evidence_class: "reference", test_ref: "compatibility/prometheus/query-corpus/header.yml", status: "active" },
    ]);
    const casesPath = join(dataDir, "compat-cases.json");
    // Only "native-histogram-query" fails; "unrelated-query" — reproduced
    // from issue #3508's own confirmation — passes. Neither the whole-run
    // binding nor the file-scoped one is actually about the SAME case, but
    // only the whole-run binding is sound to fail on it.
    writeFileSync(
      casesPath,
      JSON.stringify({
        head: "prometheus",
        cases: [
          { id: "unrelated-query", passed: true },
          { id: "native-histogram-query", passed: false },
        ],
      }),
    );
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
    const whole = records.find((r) => r.binding === "BINDING-PROMQL-RANGE-ALIGNMENT-COMPAT");
    const fileScoped = records.find((r) => r.binding === "BINDING-PROMQL-NATIVE-HISTOGRAM-COMPAT");
    // Sound: the whole-driver-invocation binding really is evidenced by the
    // full case set's aggregate, so its "fail" stands.
    assert.equal(whole.selection, "executed");
    assert.equal(whole.result, "fail");
    // Unsound before the fix: this binding's own test_ref names ONE corpus
    // file, and the only failing case cannot be attributed to it, so it
    // must NOT be reported as executed/fail.
    assert.equal(fileScoped.selection, "unavailable");
    assert.equal(fileScoped.result, "error");
    assert.match(fileScoped.selection_reason, /#3508/);
  } finally {
    rmSync(modelDir, { recursive: true, force: true });
    rmSync(dataDir, { recursive: true, force: true });
  }
});

test("CLI (issue #3509): a missing/malformed gRPC case set degrades only its own binding — the HTTP-arm record still lands", () => {
  const modelDir = tempDir("compat-exec-cli-model-");
  const dataDir = tempDir("compat-exec-cli-data-");
  try {
    writeMinimalModel(modelDir, [
      { id: "BINDING-TRACEQL-TRANSPORT-ARM-HTTP", evidence_class: "reference", test_ref: "compatibility/tempo/driver/diff.go", status: "active" },
      { id: "BINDING-TRACEQL-ORACLE-AUTHORITY-LIVE-REFERENCE", evidence_class: "reference", test_ref: "compatibility/tempo/driver/differ.go", status: "active" },
      { id: "BINDING-TRACEQL-TRANSPORT-ARM-GRPC", evidence_class: "reference", test_ref: "compatibility/tempo/driver/grpc_diff.go", status: "active" },
    ]);
    const casesPath = join(dataDir, "compat-cases.json");
    const casesPathGrpc = join(dataDir, "compat-cases-grpc.json");
    writeFileSync(casesPath, JSON.stringify({ head: "tempo", cases: [{ id: "q1", passed: true }] }));
    writeFileSync(casesPathGrpc, "{ not valid json"); // the harness step exited 0 but produced garbage
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
    // The whole report must still be produced — a bad gRPC artifact must
    // never abort the HTTP-arm and live-reference-oracle records too.
    assert.equal(result.status, 0, result.stderr);
    const records = JSON.parse(result.stdout);
    assert.equal(records.length, 3);
    const http = records.find((r) => r.binding === "BINDING-TRACEQL-TRANSPORT-ARM-HTTP");
    const oracle = records.find((r) => r.binding === "BINDING-TRACEQL-ORACLE-AUTHORITY-LIVE-REFERENCE");
    const grpc = records.find((r) => r.binding === "BINDING-TRACEQL-TRANSPORT-ARM-GRPC");
    assert.equal(http.selection, "executed");
    assert.equal(http.result, "pass");
    assert.equal(oracle.selection, "executed");
    assert.equal(oracle.result, "pass");
    assert.equal(grpc.selection, "unavailable");
    assert.equal(grpc.result, "error");
    assert.match(grpc.selection_reason, /could not be loaded/);
  } finally {
    rmSync(modelDir, { recursive: true, force: true });
    rmSync(dataDir, { recursive: true, force: true });
  }
});

test("CLI (issue #3509): a missing/malformed CASES_PATH (the non-gRPC arm) degrades only the binding(s) that depend on it too", () => {
  const modelDir = tempDir("compat-exec-cli-model-");
  const dataDir = tempDir("compat-exec-cli-data-");
  try {
    writeMinimalModel(modelDir, [
      { id: "BINDING-LOGQL-A", evidence_class: "reference", test_ref: "compatibility/loki", status: "active" },
    ]);
    const casesPath = join(dataDir, "compat-cases.json"); // never written — genuinely missing
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
    assert.equal(record.selection, "unavailable");
    assert.equal(record.result, "error");
    assert.match(record.selection_reason, /could not be loaded/);
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
