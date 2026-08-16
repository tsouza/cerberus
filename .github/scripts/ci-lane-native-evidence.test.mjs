import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import {
  ContractError,
  deriveQualificationCorrelationNonce,
} from "./ci-lane-contract.mjs";
import {
  createNativeBundle,
  nativeBundleEntries,
  parseNeeds,
  validateNativeBundle,
  writeNativeBundle,
} from "./ci-lane-native-evidence.mjs";

const source = Object.freeze({ sha: "a".repeat(40), tree: "b".repeat(40) });
const correlationNonce = deriveQualificationCorrelationNonce({
  posture: "merge",
  source,
});

function lane({ id, jobs, contextJob, mergePosture = "always" }) {
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
  };
}

function registryFixture() {
  return {
    schema_version: 1,
    lanes: [
      lane({ id: "always", jobs: ["test", "aggregate"], contextJob: "aggregate" }),
      lane({ id: "impact", jobs: ["scope", "run", "gate"], contextJob: "gate" }),
      lane({ id: "release-only", jobs: ["release"], contextJob: "release", mergePosture: "never" }),
    ],
  };
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
  return createNativeBundle({
    registry: registryFixture(),
    workflow: ".github/workflows/ci.yml",
    needs: needs(),
    source,
    repository: "example/project",
    event: "pull_request",
    runID: "123",
    runAttempt: 2,
    ...overrides,
  });
}

function execution(document, id) {
  return document.lanes.find((item) => item.lane_id === id);
}

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
  assert.deepEqual(execution(document, "always").checks, [
    { job_id: "test", result: "success" },
    { job_id: "aggregate", result: "success" },
  ]);
  assert.deepEqual(execution(document, "always").evidence, {
    executed: 2,
    passed: 2,
    failed: 0,
    skipped: 0,
  });
  assert.equal(execution(document, "always").conclusion, "success");
});

test("a green context cannot hide a skipped producer", () => {
  const document = bundle({
    needs: needs({ run: { result: "skipped", outputs: {} } }),
  });
  const impact = execution(document, "impact");
  assert.equal(impact.conclusion, "skipped");
  assert.deepEqual(impact.evidence, {
    executed: 3,
    passed: 2,
    failed: 0,
    skipped: 1,
  });
});

test("failure and cancellation are retained as failed native checks", () => {
  const failed = bundle({
    needs: needs({ test: { result: "failure", outputs: {} } }),
  });
  assert.equal(execution(failed, "always").conclusion, "failure");
  assert.equal(execution(failed, "always").evidence.failed, 1);

  const cancelled = bundle({
    needs: needs({ test: { result: "cancelled", outputs: {} } }),
  });
  assert.equal(execution(cancelled, "always").conclusion, "cancelled");
  assert.equal(execution(cancelled, "always").evidence.failed, 1);
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
    nativeBundleEntries(document).map((entry) => `${entry.lane_id}/${entry.execution_id}`),
    ["always/default", "impact/default"],
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
  assert.equal(
    bundle({
      posture: "release",
      correlationNonce: "e".repeat(64),
      needs: needs({ release: { result: "success", outputs: {} } }),
    }).correlation_nonce,
    "e".repeat(64),
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
