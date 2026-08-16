// release-qualification.mjs — plan and join the finite pre-publication test
// fence for one immutable release candidate.
//
// Modes (MODE or argv[2]):
//   plan               select every deterministic or seeded pre-publication
//                      lane from the registry and write the closed roster.
//   join               validate the adapter's trusted evidence index, reject
//                      every non-success or hollow result, and emit a canonical
//                      dry-run attestation over source tree + artifact digests.
//   verify-attestation verify that promotion received the exact successful
//                      attestation and candidate qualified by the join.
//
// Environment:
//   RELEASE_CANDIDATE_DIR       sealed candidate root.
//   RELEASE_CANDIDATE_DIGEST    exact sha256:<hex> candidate digest.
//   RELEASE_SOURCE_SHA          exact 40-hex source commit.
//   RELEASE_SOURCE_TREE         exact 40-hex Git tree.
//   RELEASE_APP_VERSION         exact application version.
//   RELEASE_APP_TAG             exact application tag.
//   RELEASE_CHART_VERSION       exact chart version.
//   RELEASE_QUALIFICATION_START epoch milliseconds set before candidate build.
//   RELEASE_CORRELATION_NONCE    64 lowercase hex, fresh for this qualification.
//   RELEASE_QUALIFICATION_PLAN  plan path
//                              (default build/release-qualification-plan.json).
//   RELEASE_EVIDENCE_INDEX      normalized, trusted evidence index from the
//                              cross-run adapter (join only).
//   RELEASE_ATTESTATION         attestation path
//                              (default build/release-qualification-attestation.json).
//   RELEASE_ATTESTATION_DIGEST  expected attestation digest (verify only).
//   RELEASE_QUALIFICATION_NOW   epoch milliseconds override for deterministic
//                              fault tests; Actions leaves it unset.
//   GITHUB_OUTPUT               receives plan/qualification/attestation outputs.
//
// The adapter is deliberately the only cross-workflow discovery seam. This
// module accepts no GitHub API observation directly: it validates a closed
// roster plus records whose run/job/artifact provenance the adapter has already
// resolved. Missing records and duplicate identities are both failures.

import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";

import { error, notice, setOutput } from "./lib/gh.mjs";
import { loadRegistry } from "./ci-lane-contract.mjs";
import {
  CANDIDATE_MANIFEST,
  canonicalJSON,
  sha256File,
  verifyCandidate,
} from "./release-candidate.mjs";

export const QUALIFICATION_SCHEMA_VERSION = 1;
export const EVIDENCE_INDEX_SCHEMA_VERSION = 1;
export const ATTESTATION_SCHEMA_VERSION = 1;
export const RELEASE_QUALIFICATION_LIMIT_MINUTES = 120;
export const RELEASE_QUALIFICATION_LIMIT_MS = RELEASE_QUALIFICATION_LIMIT_MINUTES * 60 * 1000;
export const QUICKSTART_LANE_ID = "e2e.quickstart";

const SHA_RE = /^[0-9a-f]{40}$/;
const DIGEST_RE = /^sha256:[0-9a-f]{64}$/;
const NONCE_RE = /^[0-9a-f]{64}$/;
const RUN_ID_RE = /^[1-9][0-9]*$/;
const FINITE_DETERMINISM = new Set(["deterministic", "seeded"]);
const SUCCESS = "success";

const PLAN_KEYS = new Set([
  "schema_version",
  "registry_schema_version",
  "source",
  "candidate_digest",
  "correlation_nonce",
  "started_at_ms",
  "deadline_at_ms",
  "lanes",
]);
const PLAN_LANE_KEYS = new Set([
  "lane_id",
  "execution_id",
  "producer",
  "invocation_mode",
  "determinism",
]);
const INDEX_KEYS = new Set([
  "schema_version",
  "source",
  "candidate_digest",
  "correlation_nonce",
  "records",
]);
const RECORD_KEYS = new Set([
  "lane_id",
  "execution_id",
  "source",
  "candidate_digest",
  "producer",
  "invocation_mode",
  "conclusion",
  "evidence",
  "trust",
]);
const SOURCE_KEYS = new Set(["sha", "tree"]);
const PRODUCER_KEYS = new Set(["workflow", "job", "run_id", "run_attempt"]);
const EVIDENCE_KEYS = new Set([
  "executed",
  "passed",
  "failed",
  "skipped",
  "duration_ms",
  "seed",
  "corpus_id",
]);
const TRUST_KEYS = new Set([
  "artifact_id",
  "artifact_digest",
  "head_sha",
  "head_tree",
  "run_id",
  "run_attempt",
  "job_conclusion",
  "completed_at_ms",
]);
const ATTESTATION_KEYS = new Set([
  "schema_version",
  "result",
  "source",
  "candidate",
  "qualification",
  "lanes",
]);
const ATTESTATION_CANDIDATE_KEYS = new Set(["digest", "image_digest", "files"]);
const ATTESTATION_FILE_KEYS = new Set(["path", "sha256", "size"]);
const ATTESTATION_QUALIFICATION_KEYS = new Set([
  "started_at_ms",
  "completed_at_ms",
  "deadline_at_ms",
  "limit_minutes",
  "correlation_nonce",
]);
const ATTESTATION_LANE_KEYS = new Set(["identity", "producer", "report_artifact", "evidence"]);
const ATTESTATION_ARTIFACT_KEYS = new Set(["id", "digest"]);

export class QualificationError extends Error {
  constructor(problems) {
    super(`release qualification failed closed:\n${problems.map((p) => `- ${p}`).join("\n")}`);
    this.name = "QualificationError";
    this.problems = problems;
  }
}

function isObject(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function exactKeys(value, keys, label, problems) {
  if (!isObject(value)) {
    problems.push(`${label} must be an object`);
    return false;
  }
  const extras = Object.keys(value).filter((key) => !keys.has(key));
  const missing = [...keys].filter((key) => !(key in value));
  if (extras.length > 0) problems.push(`${label} has unknown keys: ${extras.join(", ")}`);
  if (missing.length > 0) problems.push(`${label} is missing keys: ${missing.join(", ")}`);
  return extras.length === 0 && missing.length === 0;
}

function string(value, label, problems, pattern = null) {
  if (typeof value !== "string" || value === "") {
    problems.push(`${label} must be a non-empty string`);
    return;
  }
  if (pattern && !pattern.test(value)) problems.push(`${label} has invalid value ${JSON.stringify(value)}`);
}

function integer(value, label, problems, min = 0) {
  if (!Number.isSafeInteger(value) || value < min) problems.push(`${label} must be an integer >= ${min}`);
}

function validateSource(source, label, problems) {
  if (!exactKeys(source, SOURCE_KEYS, label, problems)) return;
  string(source.sha, `${label}.sha`, problems, SHA_RE);
  string(source.tree, `${label}.tree`, problems, SHA_RE);
}

function readJSON(path, label = path) {
  if (!existsSync(path)) throw new QualificationError([`${label} does not exist: ${path}`]);
  try {
    return JSON.parse(readFileSync(path, "utf8"));
  } catch (cause) {
    throw new QualificationError([`${label} is not valid JSON: ${cause.message}`]);
  }
}

function writeCanonical(path, value) {
  writeFileSync(path, `${canonicalJSON(value)}\n`);
}

function requiredEnv(name, pattern = null) {
  const value = process.env[name];
  const problems = [];
  string(value, name, problems, pattern);
  if (problems.length > 0) throw new QualificationError(problems);
  return value;
}

function positiveMillis(name, raw = process.env[name]) {
  if (!/^[1-9][0-9]*$/.test(raw ?? "")) {
    throw new QualificationError([`${name} must be canonical positive epoch milliseconds`]);
  }
  const value = Number(raw);
  if (!Number.isSafeInteger(value)) throw new QualificationError([`${name} exceeds safe integer range`]);
  return value;
}

export function releaseLaneRoster(registry) {
  const roster = [];
  for (const lane of registry.lanes) {
    if (!FINITE_DETERMINISM.has(lane.determinism)) continue;
    if (lane.release_posture === "post_publish") continue;
    if (!lane.applicability.source && !lane.applicability.artifact) continue;
    for (const execution of lane.executions) {
      for (const invocationMode of [
        ...(lane.applicability.source ? ["source_tree"] : []),
        ...(lane.applicability.artifact ? ["candidate_artifact"] : []),
      ]) {
        roster.push({
          lane_id: lane.id,
          execution_id: execution,
          producer: {
            workflow: lane.owner.workflow,
            job: lane.owner.context_job,
          },
          invocation_mode: invocationMode,
          determinism: lane.determinism,
        });
      }
    }
  }
  roster.sort((left, right) => identity(left).localeCompare(identity(right)));
  if (!roster.some((item) => item.lane_id === QUICKSTART_LANE_ID)) {
    throw new QualificationError([`${QUICKSTART_LANE_ID} is absent from the finite release roster`]);
  }
  return roster;
}

function identity(value) {
  return `${value.lane_id}/${value.execution_id}/${value.invocation_mode}`;
}

export function createPlan({ registry, source, candidateDigest, correlationNonce, startedAtMs }) {
  const problems = [];
  validateSource(source, "plan.source", problems);
  string(candidateDigest, "plan.candidate_digest", problems, DIGEST_RE);
  string(correlationNonce, "plan.correlation_nonce", problems, NONCE_RE);
  integer(startedAtMs, "plan.started_at_ms", problems, 1);
  if (problems.length > 0) throw new QualificationError(problems);
  return {
    schema_version: QUALIFICATION_SCHEMA_VERSION,
    registry_schema_version: registry.schema_version,
    source,
    candidate_digest: candidateDigest,
    correlation_nonce: correlationNonce,
    started_at_ms: startedAtMs,
    deadline_at_ms: startedAtMs + RELEASE_QUALIFICATION_LIMIT_MS,
    lanes: releaseLaneRoster(registry),
  };
}

export function validatePlan(plan, registry, expected = {}) {
  const problems = [];
  if (!exactKeys(plan, PLAN_KEYS, "plan", problems)) throw new QualificationError(problems);
  if (plan.schema_version !== QUALIFICATION_SCHEMA_VERSION) {
    problems.push(`plan.schema_version must be ${QUALIFICATION_SCHEMA_VERSION}`);
  }
  if (plan.registry_schema_version !== registry.schema_version) {
    problems.push("plan.registry_schema_version does not match the registry");
  }
  validateSource(plan.source, "plan.source", problems);
  string(plan.candidate_digest, "plan.candidate_digest", problems, DIGEST_RE);
  string(plan.correlation_nonce, "plan.correlation_nonce", problems, NONCE_RE);
  integer(plan.started_at_ms, "plan.started_at_ms", problems, 1);
  integer(plan.deadline_at_ms, "plan.deadline_at_ms", problems, 1);
  if (plan.deadline_at_ms - plan.started_at_ms !== RELEASE_QUALIFICATION_LIMIT_MS) {
    problems.push(`plan deadline must be exactly ${RELEASE_QUALIFICATION_LIMIT_MINUTES} minutes after start`);
  }
  const expectedRoster = releaseLaneRoster(registry);
  if (!Array.isArray(plan.lanes)) problems.push("plan.lanes must be an array");
  else {
    for (let index = 0; index < plan.lanes.length; index += 1) {
      const lane = plan.lanes[index];
      const label = `plan.lanes[${index}]`;
      if (!exactKeys(lane, PLAN_LANE_KEYS, label, problems)) continue;
      string(lane.lane_id, `${label}.lane_id`, problems);
      string(lane.execution_id, `${label}.execution_id`, problems);
      if (!exactKeys(lane.producer, new Set(["workflow", "job"]), `${label}.producer`, problems)) continue;
      string(lane.producer.workflow, `${label}.producer.workflow`, problems);
      string(lane.producer.job, `${label}.producer.job`, problems);
    }
    if (canonicalJSON(plan.lanes) !== canonicalJSON(expectedRoster)) {
      problems.push("plan.lanes does not exactly equal the registry-derived finite release roster");
    }
  }
  for (const [label, actual, wanted] of [
    ["source.sha", plan.source?.sha, expected.sha],
    ["source.tree", plan.source?.tree, expected.tree],
    ["candidate_digest", plan.candidate_digest, expected.candidateDigest],
    ["correlation_nonce", plan.correlation_nonce, expected.correlationNonce],
  ]) {
    if (wanted !== undefined && actual !== wanted) problems.push(`plan.${label} is ${actual}, want ${wanted}`);
  }
  if (problems.length > 0) throw new QualificationError(problems);
  return plan;
}

function validateIndex(index, problems) {
  if (!exactKeys(index, INDEX_KEYS, "evidence_index", problems)) return;
  if (index.schema_version !== EVIDENCE_INDEX_SCHEMA_VERSION) {
    problems.push(`evidence_index.schema_version must be ${EVIDENCE_INDEX_SCHEMA_VERSION}`);
  }
  validateSource(index.source, "evidence_index.source", problems);
  string(index.candidate_digest, "evidence_index.candidate_digest", problems, DIGEST_RE);
  string(index.correlation_nonce, "evidence_index.correlation_nonce", problems, NONCE_RE);
  if (!Array.isArray(index.records)) problems.push("evidence_index.records must be an array");
}

function validateRecord(record, lane, label, problems) {
  if (!exactKeys(record, RECORD_KEYS, label, problems)) return;
  string(record.lane_id, `${label}.lane_id`, problems);
  string(record.execution_id, `${label}.execution_id`, problems);
  validateSource(record.source, `${label}.source`, problems);
  if (record.candidate_digest !== null) string(record.candidate_digest, `${label}.candidate_digest`, problems, DIGEST_RE);
  if (exactKeys(record.producer, PRODUCER_KEYS, `${label}.producer`, problems)) {
    string(record.producer.workflow, `${label}.producer.workflow`, problems);
    string(record.producer.job, `${label}.producer.job`, problems);
    string(record.producer.run_id, `${label}.producer.run_id`, problems, RUN_ID_RE);
    integer(record.producer.run_attempt, `${label}.producer.run_attempt`, problems, 1);
  }
  string(record.invocation_mode, `${label}.invocation_mode`, problems);
  string(record.conclusion, `${label}.conclusion`, problems);
  if (exactKeys(record.evidence, EVIDENCE_KEYS, `${label}.evidence`, problems)) {
    for (const key of ["executed", "passed", "failed", "skipped", "duration_ms"]) {
      integer(record.evidence[key], `${label}.evidence.${key}`, problems);
    }
    if (record.evidence.seed !== null) string(record.evidence.seed, `${label}.evidence.seed`, problems);
    string(record.evidence.corpus_id, `${label}.evidence.corpus_id`, problems);
  }
  if (exactKeys(record.trust, TRUST_KEYS, `${label}.trust`, problems)) {
    string(record.trust.artifact_id, `${label}.trust.artifact_id`, problems, RUN_ID_RE);
    string(record.trust.artifact_digest, `${label}.trust.artifact_digest`, problems, DIGEST_RE);
    string(record.trust.head_sha, `${label}.trust.head_sha`, problems, SHA_RE);
    string(record.trust.head_tree, `${label}.trust.head_tree`, problems, SHA_RE);
    string(record.trust.run_id, `${label}.trust.run_id`, problems, RUN_ID_RE);
    integer(record.trust.run_attempt, `${label}.trust.run_attempt`, problems, 1);
    string(record.trust.job_conclusion, `${label}.trust.job_conclusion`, problems);
    integer(record.trust.completed_at_ms, `${label}.trust.completed_at_ms`, problems, 1);
  }
  if (lane) {
    if (record.producer?.workflow !== lane.producer.workflow || record.producer?.job !== lane.producer.job) {
      problems.push(`${label}: producer does not match the registry owner`);
    }
    if (record.invocation_mode !== lane.invocation_mode) {
      problems.push(`${label}: invocation_mode ${record.invocation_mode} must be ${lane.invocation_mode}`);
    }
    if (lane.determinism === "deterministic" && record.evidence?.seed !== null) {
      problems.push(`${label}: deterministic evidence must carry seed=null`);
    }
    if (lane.determinism === "seeded" && (record.evidence?.seed ?? null) === null) {
      problems.push(`${label}: seeded evidence must carry its seed`);
    }
  }
}

export function evaluateQualification({ registry, plan, evidenceIndex, candidateManifest, nowMs }) {
  validatePlan(plan, registry);
  const problems = [];
  validateIndex(evidenceIndex, problems);
  integer(nowMs, "qualification.now_ms", problems, 1);
  if (nowMs > plan.deadline_at_ms) {
    problems.push(
      `qualification timed out at ${nowMs}; deadline was ${plan.deadline_at_ms} ` +
        `(${RELEASE_QUALIFICATION_LIMIT_MINUTES} minutes)`,
    );
  }
  if (evidenceIndex.source?.sha !== plan.source.sha || evidenceIndex.source?.tree !== plan.source.tree) {
    problems.push("evidence index source SHA/tree does not match the plan");
  }
  if (evidenceIndex.candidate_digest !== plan.candidate_digest) {
    problems.push("evidence index candidate digest does not match the plan");
  }
  if (evidenceIndex.correlation_nonce !== plan.correlation_nonce) {
    problems.push("evidence index correlation nonce does not match the plan");
  }

  const expected = new Map(plan.lanes.map((lane) => [identity(lane), lane]));
  const actual = new Map();
  if (Array.isArray(evidenceIndex.records)) {
    for (let index = 0; index < evidenceIndex.records.length; index += 1) {
      const record = evidenceIndex.records[index];
      const id = identity(record);
      const label = `evidence_index.records[${index}] (${id})`;
      if (actual.has(id)) {
        problems.push(`duplicate evidence record ${id}`);
        continue;
      }
      actual.set(id, record);
      const lane = expected.get(id);
      if (!lane) {
        problems.push(`unexpected evidence record ${id}`);
        continue;
      }
      validateRecord(record, lane, label, problems);
      if (record.source?.sha !== plan.source.sha || record.source?.tree !== plan.source.tree) {
        problems.push(`${id}: report source SHA/tree does not match the plan`);
      }
      const expectedDigest = lane.invocation_mode === "candidate_artifact" ? plan.candidate_digest : null;
      if (record.candidate_digest !== expectedDigest) {
        problems.push(`${id}: candidate digest does not match invocation mode and plan`);
      }
      if (record.conclusion !== SUCCESS) problems.push(`${id}: report conclusion ${record.conclusion} is not success`);
      if (record.trust?.job_conclusion !== SUCCESS) {
        problems.push(`${id}: trusted job conclusion ${record.trust?.job_conclusion} is not success`);
      }
      if (
        record.producer?.run_id !== record.trust?.run_id ||
        record.producer?.run_attempt !== record.trust?.run_attempt
      ) {
        problems.push(`${id}: report run identity does not match trusted workflow provenance`);
      }
      if (record.trust?.head_sha !== plan.source.sha || record.trust?.head_tree !== plan.source.tree) {
        problems.push(`${id}: trusted workflow source SHA/tree does not match the plan`);
      }
      if (record.trust?.completed_at_ms < plan.started_at_ms) {
        problems.push(`${id}: trusted job completed before qualification started`);
      }
      if (record.trust?.completed_at_ms > plan.deadline_at_ms) {
        problems.push(`${id}: trusted job completed after the qualification deadline`);
      }
      const evidence = record.evidence ?? {};
      if (evidence.executed <= 0) problems.push(`${id}: zero native checks/tests executed`);
      if (
        evidence.executed !== evidence.passed + evidence.failed + evidence.skipped ||
        evidence.failed !== 0 ||
        evidence.skipped !== 0 ||
        evidence.passed !== evidence.executed
      ) {
        problems.push(`${id}: native evidence is not all-executed and all-passed`);
      }
    }
  }
  for (const id of expected.keys()) {
    if (!actual.has(id)) problems.push(`missing evidence record ${id}`);
  }
  if (!actual.has(`${QUICKSTART_LANE_ID}/default/source_tree`)) {
    problems.push(`quickstart proof ${QUICKSTART_LANE_ID}/default/source_tree is absent`);
  }
  if (problems.length > 0) throw new QualificationError(problems);

  const lanes = [...actual.entries()].sort(([left], [right]) => left.localeCompare(right)).map(([id, record]) => ({
    identity: id,
    producer: record.producer,
    report_artifact: {
      id: record.trust.artifact_id,
      digest: record.trust.artifact_digest,
    },
    evidence: record.evidence,
  }));
  return {
    schema_version: ATTESTATION_SCHEMA_VERSION,
    result: SUCCESS,
    source: plan.source,
    candidate: {
      digest: plan.candidate_digest,
      image_digest: candidateManifest.image.digest,
      files: candidateManifest.files.map((file) => ({ path: file.path, sha256: file.sha256, size: file.size })),
    },
    qualification: {
      started_at_ms: plan.started_at_ms,
      completed_at_ms: nowMs,
      deadline_at_ms: plan.deadline_at_ms,
      limit_minutes: RELEASE_QUALIFICATION_LIMIT_MINUTES,
      correlation_nonce: plan.correlation_nonce,
    },
    lanes,
  };
}

export function validateAttestation(attestation, expected = {}, registry = null) {
  const problems = [];
  if (!exactKeys(attestation, ATTESTATION_KEYS, "attestation", problems)) {
    throw new QualificationError(problems);
  }
  if (attestation.schema_version !== ATTESTATION_SCHEMA_VERSION) {
    problems.push(`attestation.schema_version must be ${ATTESTATION_SCHEMA_VERSION}`);
  }
  if (attestation.result !== SUCCESS) problems.push("attestation.result must be success");
  validateSource(attestation.source, "attestation.source", problems);
  if (exactKeys(attestation.candidate, ATTESTATION_CANDIDATE_KEYS, "attestation.candidate", problems)) {
    string(attestation.candidate.digest, "attestation.candidate.digest", problems, DIGEST_RE);
    string(attestation.candidate.image_digest, "attestation.candidate.image_digest", problems, DIGEST_RE);
    if (!Array.isArray(attestation.candidate.files) || attestation.candidate.files.length === 0) {
      problems.push("attestation.candidate.files must be non-empty");
    } else {
      let previous = "";
      const seen = new Set();
      for (let index = 0; index < attestation.candidate.files.length; index += 1) {
        const file = attestation.candidate.files[index];
        const label = `attestation.candidate.files[${index}]`;
        if (!exactKeys(file, ATTESTATION_FILE_KEYS, label, problems)) continue;
        string(file.path, `${label}.path`, problems);
        string(file.sha256, `${label}.sha256`, problems, DIGEST_RE);
        integer(file.size, `${label}.size`, problems);
        if (seen.has(file.path)) problems.push(`${label}.path is duplicated`);
        if (previous !== "" && String(file.path).localeCompare(previous) <= 0) {
          problems.push(`${label}.path is not strictly sorted`);
        }
        seen.add(file.path);
        previous = file.path;
      }
    }
  }
  if (exactKeys(attestation.qualification, ATTESTATION_QUALIFICATION_KEYS, "attestation.qualification", problems)) {
    for (const key of ["started_at_ms", "completed_at_ms", "deadline_at_ms"]) {
      integer(attestation.qualification[key], `attestation.qualification.${key}`, problems, 1);
    }
    integer(attestation.qualification.limit_minutes, "attestation.qualification.limit_minutes", problems, 1);
    string(
      attestation.qualification.correlation_nonce,
      "attestation.qualification.correlation_nonce",
      problems,
      NONCE_RE,
    );
    if (
      Number.isSafeInteger(attestation.qualification.started_at_ms) &&
      Number.isSafeInteger(attestation.qualification.deadline_at_ms) &&
      attestation.qualification.deadline_at_ms - attestation.qualification.started_at_ms !==
        RELEASE_QUALIFICATION_LIMIT_MS
    ) {
      problems.push("attestation qualification deadline does not equal the hard release limit");
    }
    if (
      attestation.qualification.completed_at_ms < attestation.qualification.started_at_ms ||
      attestation.qualification.completed_at_ms > attestation.qualification.deadline_at_ms
    ) {
      problems.push("attestation completion is outside its qualification window");
    }
  }
  const expectedLanes = registry
    ? new Map(releaseLaneRoster(registry).map((lane) => [identity(lane), lane]))
    : null;
  const actualLanes = new Map();
  if (!Array.isArray(attestation.lanes) || attestation.lanes.length === 0) {
    problems.push("attestation.lanes must be non-empty");
  } else {
    let previous = "";
    for (let index = 0; index < attestation.lanes.length; index += 1) {
      const lane = attestation.lanes[index];
      const label = `attestation.lanes[${index}]`;
      if (!exactKeys(lane, ATTESTATION_LANE_KEYS, label, problems)) continue;
      string(lane.identity, `${label}.identity`, problems);
      if (actualLanes.has(lane.identity)) problems.push(`${label}.identity is duplicated`);
      if (previous !== "" && String(lane.identity).localeCompare(previous) <= 0) {
        problems.push(`${label}.identity is not strictly sorted`);
      }
      actualLanes.set(lane.identity, lane);
      previous = lane.identity;
      if (exactKeys(lane.producer, PRODUCER_KEYS, `${label}.producer`, problems)) {
        string(lane.producer.workflow, `${label}.producer.workflow`, problems);
        string(lane.producer.job, `${label}.producer.job`, problems);
        string(lane.producer.run_id, `${label}.producer.run_id`, problems, RUN_ID_RE);
        integer(lane.producer.run_attempt, `${label}.producer.run_attempt`, problems, 1);
      }
      if (exactKeys(lane.report_artifact, ATTESTATION_ARTIFACT_KEYS, `${label}.report_artifact`, problems)) {
        string(lane.report_artifact.id, `${label}.report_artifact.id`, problems, RUN_ID_RE);
        string(lane.report_artifact.digest, `${label}.report_artifact.digest`, problems, DIGEST_RE);
      }
      if (exactKeys(lane.evidence, EVIDENCE_KEYS, `${label}.evidence`, problems)) {
        for (const key of ["executed", "passed", "failed", "skipped", "duration_ms"]) {
          integer(lane.evidence[key], `${label}.evidence.${key}`, problems);
        }
        if (lane.evidence.seed !== null) string(lane.evidence.seed, `${label}.evidence.seed`, problems);
        string(lane.evidence.corpus_id, `${label}.evidence.corpus_id`, problems);
        if (
          lane.evidence.executed <= 0 ||
          lane.evidence.executed !== lane.evidence.passed + lane.evidence.failed + lane.evidence.skipped ||
          lane.evidence.failed !== 0 ||
          lane.evidence.skipped !== 0 ||
          lane.evidence.passed !== lane.evidence.executed
        ) {
          problems.push(`${label}.evidence is not a positive all-passed native result`);
        }
      }
      const planned = expectedLanes?.get(lane.identity);
      if (expectedLanes && !planned) problems.push(`${label}.identity is not in the registry-derived release roster`);
      if (planned?.determinism === "deterministic" && lane.evidence?.seed !== null) {
        problems.push(`${label}.evidence deterministic lane must carry seed=null`);
      }
      if (planned?.determinism === "seeded" && (lane.evidence?.seed ?? null) === null) {
        problems.push(`${label}.evidence seeded lane omitted its seed`);
      }
    }
  }
  if (expectedLanes) {
    for (const id of expectedLanes.keys()) {
      if (!actualLanes.has(id)) problems.push(`attestation is missing release lane ${id}`);
    }
  }
  if (!actualLanes.has(`${QUICKSTART_LANE_ID}/default/source_tree`)) {
    problems.push("attestation does not contain the required quickstart proof");
  }
  if (attestation.qualification?.limit_minutes !== RELEASE_QUALIFICATION_LIMIT_MINUTES) {
    problems.push(`attestation qualification limit must be ${RELEASE_QUALIFICATION_LIMIT_MINUTES} minutes`);
  }
  for (const [label, actual, wanted] of [
    ["source.sha", attestation.source?.sha, expected.sha],
    ["source.tree", attestation.source?.tree, expected.tree],
    ["candidate.digest", attestation.candidate?.digest, expected.candidateDigest],
    ["qualification.correlation_nonce", attestation.qualification?.correlation_nonce, expected.correlationNonce],
  ]) {
    if (wanted !== undefined && actual !== wanted) {
      problems.push(`attestation.${label} is ${actual}, want ${wanted}`);
    }
  }
  if (problems.length > 0) throw new QualificationError(problems);
  return attestation;
}

function environmentIdentity() {
  return {
    sha: requiredEnv("RELEASE_SOURCE_SHA", SHA_RE),
    tree: requiredEnv("RELEASE_SOURCE_TREE", SHA_RE),
    candidateDigest: requiredEnv("RELEASE_CANDIDATE_DIGEST", DIGEST_RE),
    correlationNonce: requiredEnv("RELEASE_CORRELATION_NONCE", NONCE_RE),
    app: requiredEnv("RELEASE_APP_VERSION"),
    appTag: requiredEnv("RELEASE_APP_TAG"),
    chart: requiredEnv("RELEASE_CHART_VERSION"),
  };
}

function verifiedCandidate(expected) {
  return verifyCandidate(resolve(process.env.RELEASE_CANDIDATE_DIR || "build/release-candidate"), {
    sha: expected.sha,
    tree: expected.tree,
    app: expected.app,
    appTag: expected.appTag,
    chart: expected.chart,
    digest: expected.candidateDigest,
  });
}

export function verifyAttestationBundle({
  candidateRoot,
  attestationPath,
  attestationDigest,
  expected,
  registry,
}) {
  const problems = [];
  string(attestationDigest, "release attestation digest", problems, DIGEST_RE);
  if (problems.length > 0) throw new QualificationError(problems);

  const candidate = verifyCandidate(resolve(candidateRoot), {
    sha: expected.sha,
    tree: expected.tree,
    app: expected.app,
    appTag: expected.appTag,
    chart: expected.chart,
    digest: expected.candidateDigest,
  });
  const path = resolve(attestationPath);
  if (!existsSync(path)) {
    throw new QualificationError([
      `release qualification attestation does not exist: ${path}`,
    ]);
  }
  const digest = sha256File(path);
  if (digest !== attestationDigest) {
    throw new QualificationError([
      `attestation digest is ${digest}, want ${attestationDigest}`,
    ]);
  }
  const attestation = validateAttestation(
    readJSON(path, "release qualification attestation"),
    expected,
    registry,
  );
  if (attestation.candidate.image_digest !== candidate.manifest.image.digest) {
    throw new QualificationError([
      "attestation image digest does not match the verified candidate",
    ]);
  }
  if (canonicalJSON(attestation.candidate.files) !== canonicalJSON(candidate.manifest.files)) {
    throw new QualificationError([
      "attestation artifact digests do not match the verified candidate manifest",
    ]);
  }
  return { attestation, candidate, digest };
}

export function verifyAttestationFromEnvironment() {
  const expected = environmentIdentity();
  return verifyAttestationBundle({
    candidateRoot:
      process.env.RELEASE_CANDIDATE_DIR || "build/release-candidate",
    attestationPath:
      process.env.RELEASE_ATTESTATION ||
      "build/release-qualification-attestation.json",
    attestationDigest: requiredEnv("RELEASE_ATTESTATION_DIGEST", DIGEST_RE),
    expected,
    registry: loadRegistry(),
  });
}

function planMode() {
  const expected = environmentIdentity();
  verifiedCandidate(expected);
  const registry = loadRegistry();
  const startedAtMs = positiveMillis("RELEASE_QUALIFICATION_START");
  const plan = createPlan({
    registry,
    source: { sha: expected.sha, tree: expected.tree },
    candidateDigest: expected.candidateDigest,
    correlationNonce: expected.correlationNonce,
    startedAtMs,
  });
  const path = resolve(process.env.RELEASE_QUALIFICATION_PLAN || "build/release-qualification-plan.json");
  writeCanonical(path, plan);
  setOutput("plan", path);
  setOutput("lane_count", String(plan.lanes.length));
  notice(
    `planned ${plan.lanes.length} finite release executions for ${expected.sha.slice(0, 12)} ` +
      `candidate ${expected.candidateDigest}`,
  );
}

function joinMode() {
  const expected = environmentIdentity();
  const candidate = verifiedCandidate(expected);
  const registry = loadRegistry();
  const planPath = resolve(process.env.RELEASE_QUALIFICATION_PLAN || "build/release-qualification-plan.json");
  const evidencePath = resolve(requiredEnv("RELEASE_EVIDENCE_INDEX"));
  const plan = validatePlan(readJSON(planPath, "release qualification plan"), registry, expected);
  const nowMs = process.env.RELEASE_QUALIFICATION_NOW
    ? positiveMillis("RELEASE_QUALIFICATION_NOW")
    : Date.now();
  const attestation = evaluateQualification({
    registry,
    plan,
    evidenceIndex: readJSON(evidencePath, "release evidence index"),
    candidateManifest: candidate.manifest,
    nowMs,
  });
  const attestationPath = resolve(
    process.env.RELEASE_ATTESTATION || "build/release-qualification-attestation.json",
  );
  writeCanonical(attestationPath, attestation);
  const digest = sha256File(attestationPath);
  setOutput("qualified", "true");
  setOutput("attestation", attestationPath);
  setOutput("attestation_digest", digest);
  notice(
    `release qualification passed ${attestation.lanes.length} executions in ` +
      `${attestation.qualification.completed_at_ms - attestation.qualification.started_at_ms} ms; ` +
      `attestation ${digest}`,
  );
}

function verifyAttestationMode() {
  const { digest } = verifyAttestationFromEnvironment();
  setOutput("qualified", "true");
  notice(`verified successful release attestation ${digest}; promotion may proceed`);
}

function main() {
  const mode = process.env.MODE || process.argv[2];
  if (mode === "plan") planMode();
  else if (mode === "join") joinMode();
  else if (mode === "verify-attestation") verifyAttestationMode();
  else {
    throw new QualificationError([
      `MODE must be plan, join, or verify-attestation; got ${JSON.stringify(mode)}`,
    ]);
  }
}

const invokedDirectly = process.argv[1] && fileURLToPath(import.meta.url) === resolve(process.argv[1]);
if (invokedDirectly) {
  try {
    main();
  } catch (cause) {
    error(cause instanceof Error ? cause.message : String(cause));
    process.exit(1);
  }
}
