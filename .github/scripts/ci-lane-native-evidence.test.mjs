import assert from "node:assert/strict";
import {
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import {
  ContractError,
  deriveQualificationCorrelationNonce,
  nativePartArtifactName,
} from "./ci-lane-contract.mjs";
import {
  createNativeBundle,
  nativeBundleEntries,
  parseCaseJSON,
  parseCompatCasesJSON,
  parseGoTestJSON,
  parseJUnitXML,
  parseNeeds,
  parseNodeTAP,
  validateNativeBundle,
  writeNativeBundle,
} from "./ci-lane-native-evidence.mjs";

const source = Object.freeze({ sha: "a".repeat(40), tree: "b".repeat(40) });
const correlationNonce = deriveQualificationCorrelationNonce({
  posture: "merge",
  source,
});

function lane({
  id,
  jobs,
  contextJob,
  mergePosture = "always",
  mainPosture = "always",
  releasePosture = "required",
  artifact = false,
}) {
  return {
    id,
    executions: ["default"],
    owner: {
      workflow: ".github/workflows/ci.yml",
      jobs: [...jobs],
      context_job: contextJob,
    },
    context: { match: "exact", name: `${id}-check` },
    merge_posture: mergePosture,
    main_posture: mainPosture,
    release_posture: releasePosture,
    applicability: { source: true, artifact },
  };
}

function registryFixture() {
  return {
    schema_version: 1,
    native_evidence: {
      schema_version: 1,
      parts: [
        {
          id: "always-cases",
          lane_id: "always",
          execution_id: "default",
          invocation_mode: "source_tree",
          producer_job: "test",
          parser: "case-json-v1",
          entry: "cases.json",
        },
        {
          id: "impact-cases",
          lane_id: "impact",
          execution_id: "default",
          invocation_mode: "source_tree",
          producer_job: "run",
          parser: "case-json-v1",
          entry: "cases.json",
        },
        {
          id: "release-source-cases",
          lane_id: "release-only",
          execution_id: "default",
          invocation_mode: "source_tree",
          producer_job: "release",
          parser: "case-json-v1",
          entry: "cases.json",
        },
        {
          id: "release-artifact-cases",
          lane_id: "release-only",
          execution_id: "default",
          invocation_mode: "candidate_artifact",
          producer_job: "release",
          parser: "case-json-v1",
          entry: "cases.json",
        },
      ],
    },
    lanes: [
      lane({ id: "always", jobs: ["test", "aggregate"], contextJob: "aggregate" }),
      lane({ id: "impact", jobs: ["scope", "run", "gate"], contextJob: "gate" }),
      lane({
        id: "release-only",
        jobs: ["release"],
        contextJob: "release",
        mergePosture: "never",
        artifact: true,
      }),
    ],
  };
}

function partsFixture(registry, runAttempt = 2, posture = "merge") {
  const root = mkdtempSync(join(tmpdir(), "ci-lane-native-parts-"));
  const lanes = new Map(registry.lanes.map((item) => [item.id, item]));
  const parts = registry.native_evidence.parts.filter((part) => {
    const item = lanes.get(part.lane_id);
    if (posture === "merge") {
      return (
        item.merge_posture !== "never" &&
        part.invocation_mode === "source_tree"
      );
    }
    if (posture === "main") {
      return (
        item.main_posture !== "never" &&
        part.invocation_mode === "source_tree"
      );
    }
    return item.release_posture !== "post_publish";
  });
  for (const [index, part] of parts.entries()) {
    const artifact = join(root, nativePartArtifactName(part.id, runAttempt));
    mkdirSync(artifact, { recursive: true });
    writeFileSync(
      join(artifact, part.entry),
      `${JSON.stringify({
        schema_version: 1,
        seed: `seed-${part.id}`,
        cases: [
          { id: `${part.id}-one`, status: "passed", duration_ms: index + 1 },
          { id: `${part.id}-two`, status: "passed", duration_ms: index + 2 },
        ],
      })}\n`,
    );
  }
  return root;
}

function needs(values = {}) {
  return parseNeeds(
    JSON.stringify({
      test: { result: "success", outputs: {} },
      aggregate: { result: "success", outputs: {} },
      scope: { result: "success", outputs: {} },
      run: { result: "success", outputs: {} },
      gate: { result: "success", outputs: {} },
      ...values,
    }),
  );
}

function bundle(overrides = {}) {
  const registry = overrides.registry ?? registryFixture();
  const runAttempt = overrides.runAttempt ?? 2;
  const posture = overrides.posture ?? "merge";
  const ownedPartsRoot = overrides.partsRoot === undefined;
  const partsRoot =
    overrides.partsRoot ?? partsFixture(registry, runAttempt, posture);
  try {
    return createNativeBundle({
      workflow: ".github/workflows/ci.yml",
      needs: needs(),
      source,
      repository: "example/project",
      event: "pull_request",
      runID: "123",
      ...overrides,
      registry,
      runAttempt,
      partsRoot,
    });
  } finally {
    if (ownedPartsRoot) rmSync(partsRoot, { recursive: true, force: true });
  }
}

function execution(document, id, invocationMode = "source_tree") {
  return document.lanes.find(
    (item) =>
      item.lane_id === id && item.invocation_mode === invocationMode,
  );
}

function assertEvidence(evidence, expected) {
  assert.deepEqual(
    {
      executed: evidence.executed,
      passed: evidence.passed,
      failed: evidence.failed,
      skipped: evidence.skipped,
      duration_ms: evidence.duration_ms,
      seed: evidence.seed,
    },
    expected,
  );
  assert.match(evidence.corpus_id, /^sha256:[0-9a-f]{64}$/);
}

test("native parser families derive counts, duration, seed, and corpus from raw bytes", () => {
  const go = parseGoTestJSON(
    [
      { Action: "output", Output: "CI_NATIVE_SEED=go-seed\n" },
      { Action: "pass", Package: "example/pkg", Test: "TestPass" },
      { Action: "fail", Package: "example/pkg", Test: "TestFail" },
      { Action: "skip", Package: "example/pkg", Test: "TestSkip" },
      { Action: "fail", Package: "example/pkg", Elapsed: 0.123 },
    ]
      .map(JSON.stringify)
      .join("\n"),
  );
  assertEvidence(go, {
    executed: 3,
    passed: 1,
    failed: 1,
    skipped: 1,
    duration_ms: 123,
    seed: "go-seed",
  });

  const tap = parseNodeTAP(
    [
      "TAP version 13",
      "ok 1 - pass",
      "not ok 2 - fail",
      "ok 3 - omitted # SKIP",
      "1..3",
      "# ci-native-seed: tap-seed",
      "# duration_ms 12.5",
    ].join("\n"),
  );
  assertEvidence(tap, {
    executed: 3,
    passed: 1,
    failed: 1,
    skipped: 1,
    duration_ms: 13,
    seed: "tap-seed",
  });

  const junit = parseJUnitXML(
    '<testsuite><properties><property name="ci-native-seed" value="xml-seed"/></properties>' +
      '<testcase classname="suite" name="pass" time="0.001"/>' +
      '<testcase classname="suite" name="fail" time="0.002"><failure/></testcase>' +
      '<testcase classname="suite" name="skip" time="0.003"><skipped/></testcase>' +
      "</testsuite>",
  );
  assertEvidence(junit, {
    executed: 3,
    passed: 1,
    failed: 1,
    skipped: 1,
    duration_ms: 6,
    seed: "xml-seed",
  });

  const cases = parseCaseJSON(
    JSON.stringify({
      schema_version: 1,
      seed: null,
      cases: [
        { id: "a", status: "passed", duration_ms: 2 },
        { id: "b", status: "failed", duration_ms: 3 },
      ],
    }),
  );
  assertEvidence(cases, {
    executed: 2,
    passed: 1,
    failed: 1,
    skipped: 0,
    duration_ms: 5,
    seed: null,
  });

  const compatibility = parseCompatCasesJSON(
    JSON.stringify({
      schema_version: 1,
      head: "promql",
      seed: "compat-seed",
      cases: [
        { id: "a", passed: true, duration_ms: 4 },
        { id: "b", passed: false, duration_ms: 5 },
      ],
    }),
  );
  assertEvidence(compatibility, {
    executed: 2,
    passed: 1,
    failed: 1,
    skipped: 0,
    duration_ms: 9,
    seed: "compat-seed",
  });
});

test("native parser families fail closed on incomplete or duplicate native output", () => {
  assert.throws(
    () =>
      parseGoTestJSON(
        JSON.stringify({
          Action: "pass",
          Package: "example/pkg",
          Test: "TestWithoutPackageTerminal",
        }),
      ),
    /missing package terminal/,
  );
  assert.throws(() => parseNodeTAP("ok 1 - pass\n1..1\n"), /duration_ms/);
  assert.throws(
    () => parseJUnitXML('<testsuite><testcase name="unterminated" time="1">'),
    /unterminated testcase/,
  );
  assert.throws(
    () =>
      parseCaseJSON(
        JSON.stringify({
          schema_version: 1,
          seed: null,
          cases: [
            { id: "same", status: "passed", duration_ms: 1 },
            { id: "same", status: "passed", duration_ms: 1 },
          ],
        }),
      ),
    /unique and non-empty/,
  );
  assert.throws(
    () =>
      parseCompatCasesJSON(
        JSON.stringify({
          schema_version: 1,
          head: "promql",
          seed: null,
          cases: [
            { id: "same", passed: true, duration_ms: 1 },
            { id: "same", passed: true, duration_ms: 1 },
          ],
        }),
      ),
    /unique id/,
  );
});

test("green Actions jobs with missing native part files fail closed", () => {
  const root = mkdtempSync(join(tmpdir(), "ci-lane-native-empty-"));
  try {
    assert.throws(
      () => bundle({ partsRoot: root }),
      /native part files must exactly match the registry roster/,
    );
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("derives a canonical lane and check roster from the registry", () => {
  const document = bundle();
  assert.deepEqual(document.producer, {
    repository: "example/project",
    workflow: ".github/workflows/ci.yml",
    event: "pull_request",
    run: { id: "123", attempt: 2 },
  });
  assert.equal(document.correlation_nonce, correlationNonce);
  assert.deepEqual(
    document.lanes.map((item) => item.lane_id),
    ["always", "impact"],
  );
  assert.deepEqual(execution(document, "always").job_results, [
    { job_id: "test", result: "success" },
    { job_id: "aggregate", result: "success" },
  ]);
  assert.deepEqual(execution(document, "always").evidence, {
    executed: 2,
    passed: 2,
    failed: 0,
    skipped: 0,
    duration_ms: 3,
    seed: "seed-always-cases",
    corpus_id: execution(document, "always").evidence.corpus_id,
  });
  assert.match(
    execution(document, "always").evidence.corpus_id,
    /^sha256:[0-9a-f]{64}$/,
  );
  assert.equal(execution(document, "always").conclusion, "success");
});

test("a green context cannot hide a skipped producer", () => {
  const document = bundle({
    needs: needs({ run: { result: "skipped", outputs: {} } }),
  });
  const impact = execution(document, "impact");
  assert.equal(impact.conclusion, "skipped");
  assert.deepEqual(impact.evidence, {
    executed: 2,
    passed: 2,
    failed: 0,
    skipped: 0,
    duration_ms: 5,
    seed: "seed-impact-cases",
    corpus_id: impact.evidence.corpus_id,
  });
});

test("failure and cancellation are retained as failed native checks", () => {
  const failed = bundle({
    needs: needs({ test: { result: "failure", outputs: {} } }),
  });
  assert.equal(execution(failed, "always").conclusion, "failure");
  assert.equal(execution(failed, "always").evidence.failed, 0);

  const cancelled = bundle({
    needs: needs({ test: { result: "cancelled", outputs: {} } }),
  });
  assert.equal(execution(cancelled, "always").conclusion, "cancelled");
  assert.equal(execution(cancelled, "always").evidence.failed, 0);
});

test("missing and extra workflow needs fail closed", () => {
  const missing = needs();
  missing.delete("test");
  assert.throws(
    () => bundle({ needs: missing }),
    /native needs is missing registered job test/,
  );

  const extra = needs();
  extra.set("invented", "success");
  assert.throws(
    () => bundle({ needs: extra }),
    /native needs contains unregistered extra job invented/,
  );
});

test("downloaded bundles are closed, total, and entry-digested", () => {
  const registry = registryFixture();
  const document = bundle({ registry });
  assert.equal(
    validateNativeBundle(document, registry, { correlationNonce }),
    document,
  );
  assert.deepEqual(
    nativeBundleEntries(document).map(
      (entry) =>
        `${entry.lane_id}/${entry.execution_id}/${entry.invocation_mode}`,
    ),
    ["always/default/source_tree", "impact/default/source_tree"],
  );
  assert.ok(nativeBundleEntries(document).every((entry) => /^[0-9a-f]{64}$/.test(entry.sha256)));
  const replayedEntries = structuredClone(document);
  replayedEntries.correlation_nonce = "f".repeat(64);
  assert.notDeepEqual(
    nativeBundleEntries(replayedEntries),
    nativeBundleEntries(document),
  );

  const missing = structuredClone(document);
  missing.lanes.pop();
  assert.throws(() => validateNativeBundle(missing, registry), /missing impact\/default/);

  const dishonest = structuredClone(document);
  dishonest.lanes[0].evidence.passed = 0;
  assert.throws(() => validateNativeBundle(dishonest, registry), /counts are not derived/);

  const replayed = structuredClone(document);
  replayed.correlation_nonce = "f".repeat(64);
  assert.throws(
    () => validateNativeBundle(replayed, registry, { correlationNonce }),
    /correlationNonce does not match trusted expectation/,
  );
});

test("release bundles require a coordinator correlation nonce", () => {
  assert.throws(
    () => bundle({ posture: "release" }),
    /release evidence requires an explicit CI_LANE_CORRELATION_NONCE/,
  );
  const document = bundle({
    posture: "release",
    correlationNonce: "e".repeat(64),
    needs: needs({ release: { result: "success", outputs: {} } }),
  });
  assert.equal(document.correlation_nonce, "e".repeat(64));
  assert.ok(execution(document, "release-only", "source_tree"));
  assert.ok(execution(document, "release-only", "candidate_artifact"));
  assert.equal(
    document.lanes.filter((item) => item.lane_id === "release-only").length,
    2,
  );
});

test("never-merge lanes do not expand the native needs roster", () => {
  const document = bundle();
  assert.equal(execution(document, "release-only"), undefined);
  assert.equal(document.lanes.length, 2);
});

test("needs results use a closed schema", () => {
  for (const raw of [
    "",
    "[]",
    JSON.stringify({ job: { result: "neutral" } }),
    JSON.stringify({ "not/a/job": { result: "success" } }),
  ]) {
    assert.throws(() => parseNeeds(raw), ContractError);
  }
});

test("workflow, repository, source, and run identity are strict", () => {
  for (const overrides of [
    { workflow: "ci.yml" },
    { repository: "project" },
    { event: "pull-request" },
    { runID: "0" },
    { runAttempt: 0 },
    { source: { sha: "bad", tree: source.tree } },
  ]) {
    assert.throws(() => bundle(overrides), ContractError);
  }
});

test("bundle writer is canonical, atomic, and refuses stale reuse", (t) => {
  const root = mkdtempSync(join(tmpdir(), "ci-lane-native-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const path = join(root, "bundle.json");
  const document = bundle();
  const written = writeNativeBundle(path, document);
  assert.equal(written.body, `${JSON.stringify(document, null, 2)}\n`);
  assert.equal(readFileSync(path, "utf8"), written.body);
  assert.match(written.sha256, /^[0-9a-f]{64}$/);
  assert.throws(() => writeNativeBundle(path, document), /stale reuse/);
});
