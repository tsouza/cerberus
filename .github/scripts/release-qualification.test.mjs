import assert from "node:assert/strict";
import { test } from "node:test";

import {
  QualificationError,
  RELEASE_QUALIFICATION_LIMIT_MS,
  createPlan,
  evaluateQualification,
  releaseLaneRoster,
  validateAttestation,
} from "./release-qualification.mjs";

const SHA = "a".repeat(40);
const TREE = "b".repeat(40);
const CANDIDATE = `sha256:${"c".repeat(64)}`;
const REPORT_DIGEST = `sha256:${"d".repeat(64)}`;
const NONCE = "e".repeat(64);

function lane(id, { artifact = false, seeded = false } = {}) {
  return {
    id,
    executions: ["default"],
    owner: {
      workflow: `.github/workflows/${id.replaceAll(".", "-")}.yml`,
      context_job: id.replaceAll(".", "-"),
    },
    determinism: seeded ? "seeded" : "deterministic",
    release_posture: "required",
    applicability: { source: true, artifact },
  };
}

const registry = {
  schema_version: 1,
  lanes: [lane("e2e.quickstart"), lane("migration.e2e", { artifact: true })],
};

function fixture() {
  const startedAtMs = 1_000_000;
  const plan = createPlan({
    registry,
    source: { sha: SHA, tree: TREE },
    candidateDigest: CANDIDATE,
    correlationNonce: NONCE,
    startedAtMs,
  });
  const records = plan.lanes.map((planned, index) => ({
    lane_id: planned.lane_id,
    execution_id: planned.execution_id,
    source: { sha: SHA, tree: TREE },
    candidate_digest:
      planned.invocation_mode === "candidate_artifact" ? CANDIDATE : null,
    producer: {
      workflow: planned.producer.workflow,
      job: planned.producer.job,
      run_id: String(100 + index),
      run_attempt: 1,
    },
    invocation_mode: planned.invocation_mode,
    conclusion: "success",
    evidence: {
      executed: 3,
      passed: 3,
      failed: 0,
      skipped: 0,
      duration_ms: 250,
      seed: null,
      corpus_id: `${planned.lane_id}-native`,
    },
    trust: {
      artifact_id: String(900 + index),
      artifact_digest: REPORT_DIGEST,
      head_sha: SHA,
      head_tree: TREE,
      run_id: String(100 + index),
      run_attempt: 1,
      job_conclusion: "success",
      completed_at_ms: startedAtMs + 1000 + index,
    },
  }));
  return {
    plan,
    index: {
      schema_version: 1,
      source: { sha: SHA, tree: TREE },
      candidate_digest: CANDIDATE,
      correlation_nonce: NONCE,
      records,
    },
    candidateManifest: {
      image: { digest: `sha256:${"e".repeat(64)}` },
      files: [{ path: "app/image/index.json", size: 5, sha256: `sha256:${"f".repeat(64)}` }],
    },
    nowMs: startedAtMs + 2000,
  };
}

function evaluate(value) {
  return evaluateQualification({
    registry,
    plan: value.plan,
    evidenceIndex: value.index,
    candidateManifest: value.candidateManifest,
    nowMs: value.nowMs,
  });
}

test("release roster includes every finite pre-publication source/artifact lane", () => {
  const extended = {
    ...registry,
    lanes: [
      ...registry.lanes,
      { ...lane("advisory.seed", { seeded: true }), release_posture: "advisory" },
      { ...lane("public.monitor"), release_posture: "post_publish" },
      { ...lane("observation"), determinism: "observational" },
    ],
  };
  assert.deepEqual(
    releaseLaneRoster(extended).map((item) => `${item.lane_id}/${item.invocation_mode}`),
    [
      "advisory.seed/source_tree",
      "e2e.quickstart/source_tree",
      "migration.e2e/candidate_artifact",
      "migration.e2e/source_tree",
    ],
  );
});

test("successful join attests the exact tree, candidate, image, files, and quickstart", () => {
  const value = fixture();
  const attestation = evaluate(value);
  assert.equal(attestation.result, "success");
  assert.deepEqual(attestation.source, { sha: SHA, tree: TREE });
  assert.equal(attestation.candidate.digest, CANDIDATE);
  assert.equal(attestation.lanes.length, 3);
  assert(attestation.lanes.some((item) => item.identity === "e2e.quickstart/default/source_tree"));
  assert(attestation.lanes.some((item) => item.identity === "migration.e2e/default/source_tree"));
  assert(attestation.lanes.some((item) => item.identity === "migration.e2e/default/candidate_artifact"));
  assert.deepEqual(
    validateAttestation(attestation, {
      sha: SHA,
      tree: TREE,
      candidateDigest: CANDIDATE,
      correlationNonce: NONCE,
    }),
    attestation,
  );
});

test("failed, cancelled, skipped, neutral, timed-out, and stale conclusions block", () => {
  for (const conclusion of [
    "failure",
    "cancelled",
    "skipped",
    "neutral",
    "timed_out",
    "stale",
  ]) {
    const value = fixture();
    value.index.records[0].conclusion = conclusion;
    assert.throws(
      () => evaluate(value),
      (error) => error instanceof QualificationError && error.message.includes(conclusion),
      conclusion,
    );
  }
});

test("a non-success trusted job blocks even when its report claims success", () => {
  const value = fixture();
  value.index.records[0].trust.job_conclusion = "cancelled";
  assert.throws(() => evaluate(value), /trusted job conclusion cancelled/);
});

test("absent report blocks and quickstart absence is named explicitly", () => {
  const value = fixture();
  value.index.records = value.index.records.filter(
    (record) => record.lane_id !== "e2e.quickstart",
  );
  assert.throws(
    () => evaluate(value),
    (error) =>
      error instanceof QualificationError &&
      /missing evidence record e2e\.quickstart\/default\/source_tree/.test(error.message) &&
      /quickstart proof/.test(error.message),
  );
});

test("zero, skipped, failed, or arithmetically hollow native evidence blocks", () => {
  for (const evidence of [
    { executed: 0, passed: 0, failed: 0, skipped: 0 },
    { executed: 3, passed: 2, failed: 0, skipped: 1 },
    { executed: 3, passed: 2, failed: 1, skipped: 0 },
    { executed: 3, passed: 2, failed: 0, skipped: 0 },
  ]) {
    const value = fixture();
    Object.assign(value.index.records[0].evidence, evidence);
    assert.throws(() => evaluate(value), /native/);
  }
});

test("candidate digest mismatch blocks source index and artifact report variants", () => {
  {
    const value = fixture();
    value.index.candidate_digest = `sha256:${"0".repeat(64)}`;
    assert.throws(() => evaluate(value), /index candidate digest/);
  }
  {
    const value = fixture();
    const artifact = value.index.records.find(
      (record) => record.lane_id === "migration.e2e" && record.invocation_mode === "candidate_artifact",
    );
    artifact.candidate_digest = `sha256:${"1".repeat(64)}`;
    assert.throws(() => evaluate(value), /candidate digest does not match/);
  }
});

test("wrong SHA/tree, malformed provenance, and run mismatch each block", () => {
  for (const mutate of [
    (value) => {
      value.index.records[0].source.tree = "2".repeat(40);
    },
    (value) => {
      value.index.records[0].trust.artifact_digest = "bad";
    },
    (value) => {
      value.index.records[0].trust.run_id = "999";
    },
  ]) {
    const value = fixture();
    mutate(value);
    assert.throws(() => evaluate(value), QualificationError);
  }
});

test("missing or stale correlation is rejected before reports can qualify", () => {
  {
    const value = fixture();
    value.index.correlation_nonce = "f".repeat(64);
    assert.throws(() => evaluate(value), /correlation nonce/);
  }
  {
    const value = fixture();
    delete value.index.correlation_nonce;
    assert.throws(() => evaluate(value), /correlation_nonce/);
  }
});

test("promotion rejects malformed, incomplete, hollow, and stale attestations", () => {
  const baseline = evaluate(fixture());
  for (const [name, mutate] of [
    ["malformed candidate file", (value) => { delete value.candidate.files[0].sha256; }],
    ["incomplete roster", (value) => { value.lanes = value.lanes.filter((lane) => !lane.identity.includes("candidate_artifact")); }],
    ["hollow native evidence", (value) => { value.lanes[0].evidence.executed = 0; value.lanes[0].evidence.passed = 0; }],
    ["stale correlation", (value) => { value.qualification.correlation_nonce = "0".repeat(64); }],
    ["late completion", (value) => { value.qualification.completed_at_ms = value.qualification.deadline_at_ms + 1; }],
  ]) {
    const attestation = structuredClone(baseline);
    mutate(attestation);
    assert.throws(
      () => validateAttestation(attestation, {
        sha: SHA,
        tree: TREE,
        candidateDigest: CANDIDATE,
        correlationNonce: NONCE,
      }, registry),
      QualificationError,
      name,
    );
  }
});

test("the 120-minute deadline rejects late join and late lane completion", () => {
  {
    const value = fixture();
    value.nowMs = value.plan.started_at_ms + RELEASE_QUALIFICATION_LIMIT_MS + 1;
    assert.throws(() => evaluate(value), /qualification timed out/);
  }
  {
    const value = fixture();
    value.index.records[0].trust.completed_at_ms = value.plan.deadline_at_ms + 1;
    assert.throws(() => evaluate(value), /completed after the qualification deadline/);
  }
  {
    const value = fixture();
    value.index.records[0].trust.completed_at_ms = value.plan.started_at_ms - 1;
    assert.throws(() => evaluate(value), /completed before qualification started/);
  }
});
