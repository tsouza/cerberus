// release-qualification.mjs — create and join the finite pre-publication test
// fence for one immutable release candidate.
//
// Modes (MODE or argv[2]):
//   plan               write the canonical release selection and a closed plan
//                      that pins its digest and trusted coordinator identity.
//   join               validate canonical native reports against trusted API
//                      producer provenance and write the signed input attestation.
//   verify-attestation verify the exact successful attestation before a writer
//                      can promote any part of the candidate.
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
//   RELEASE_CORRELATION_NONCE   64 lowercase hex, fresh for this qualification.
//   RELEASE_QUALIFICATION_PLAN  plan path
//                              (default build/release-qualification-plan.json).
//   RELEASE_QUALIFICATION_SELECTION canonical selection path
//                              (default build/release-qualification-selection.json).
//   RELEASE_QUALIFICATION_REPORTS JSON array of canonical native reports
//                              (join only).
//   RELEASE_QUALIFICATION_PRODUCER_MANIFEST trusted API producer manifest
//                              (join only).
//   RELEASE_ATTESTATION         attestation path
//                              (default build/release-qualification-attestation.json).
//   RELEASE_ATTESTATION_DIGEST  expected attestation digest (verify only).
//   RELEASE_QUALIFICATION_NOW   epoch milliseconds override for deterministic
//                              fault tests; Actions leaves it unset.
//   GITHUB_EVENT_NAME           push or workflow_dispatch (plan only).
//   GITHUB_REPOSITORY           trusted repository identity (plan only).
//   GITHUB_RUN_ID               coordinator run ID (plan only).
//   GITHUB_RUN_ATTEMPT          coordinator run attempt (plan only).
//   GITHUB_OUTPUT               receives plan/selection/attestation outputs.
//
// Cross-workflow discovery stays outside this module. The adapter supplies
// native report documents plus API-derived producer provenance; the canonical
// CI lane contract performs the only report-set qualification.

import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";

import {
  ContractError,
  loadRegistry,
  selectionManifestSHA256,
  validateReportSet,
  validateSelection,
} from "./ci-lane-contract.mjs";
import { error, notice, setOutput } from "./lib/gh.mjs";
import {
  canonicalJSON,
  sha256File,
  verifyCandidate,
} from "./release-candidate.mjs";

export const QUALIFICATION_SCHEMA_VERSION = 2;
export const ATTESTATION_SCHEMA_VERSION = 2;
export const RELEASE_QUALIFICATION_INVOCATIONS = 45;
export const RELEASE_QUALIFICATION_LIMIT_MINUTES = 120;
export const RELEASE_QUALIFICATION_LIMIT_MS =
  RELEASE_QUALIFICATION_LIMIT_MINUTES * 60 * 1000;
export const QUICKSTART_IDENTITY = "e2e.quickstart/default/source_tree";

const SHA_RE = /^[0-9a-f]{40}$/;
const DIGEST_RE = /^sha256:[0-9a-f]{64}$/;
const SHA256_RE = /^[0-9a-f]{64}$/;
const NONCE_RE = /^[0-9a-f]{64}$/;
const RUN_ID_RE = /^[1-9][0-9]*$/;
const REPOSITORY_RE = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;
const FINITE_DETERMINISM = new Set(["deterministic", "seeded"]);
const RELEASE_EVENTS = new Set(["push", "workflow_dispatch"]);
const SUCCESS = "success";

const SOURCE_KEYS = new Set(["sha", "tree"]);
const RUN_KEYS = new Set(["id", "attempt"]);
const EVENT_KEYS = new Set([
  "kind",
  "pr_number",
  "event_head_sha",
  "event_base_sha",
  "projected_sha",
]);
const SELECTION_REF_KEYS = new Set(["run", "manifest_sha256"]);
const PLAN_KEYS = new Set([
  "schema_version",
  "registry_schema_version",
  "selection_schema_version",
  "report_schema_version",
  "source",
  "candidate_digest",
  "correlation_nonce",
  "selection_ref",
  "event",
  "repository",
  "started_at_ms",
  "deadline_at_ms",
  "invocations",
]);
const PLAN_INVOCATION_KEYS = new Set([
  "lane_id",
  "execution_id",
  "producer",
  "invocation_mode",
  "determinism",
]);
const PLAN_PRODUCER_KEYS = new Set(["workflow", "job"]);
const ATTESTATION_KEYS = new Set([
  "schema_version",
  "result",
  "source",
  "candidate",
  "qualification",
  "lanes",
]);
const ATTESTATION_CANDIDATE_KEYS = new Set([
  "digest",
  "image_digest",
  "files",
]);
const ATTESTATION_FILE_KEYS = new Set(["path", "sha256", "size"]);
const ATTESTATION_QUALIFICATION_KEYS = new Set([
  "started_at_ms",
  "completed_at_ms",
  "deadline_at_ms",
  "limit_minutes",
  "correlation_nonce",
  "selection_ref",
  "event",
  "repository",
  "invocation_count",
]);
const ATTESTATION_LANE_KEYS = new Set([
  "identity",
  "producer",
  "report_artifact",
  "evidence",
]);
const ATTESTATION_PRODUCER_KEYS = new Set([
  "workflow",
  "job",
  "run",
  "job_name",
  "job_database_id",
]);
const ATTESTATION_ARTIFACT_KEYS = new Set([
  "id",
  "name",
  "sha256",
  "entry",
  "entry_sha256",
]);
const EVIDENCE_KEYS = new Set([
  "executed",
  "passed",
  "failed",
  "skipped",
  "duration_ms",
  "seed",
  "corpus_id",
]);

export class QualificationError extends Error {
  constructor(problems) {
    super(
      `release qualification failed closed:\n${problems
        .map((problem) => `- ${problem}`)
        .join("\n")}`,
    );
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
  if (extras.length > 0) {
    problems.push(`${label} has unknown keys: ${extras.join(", ")}`);
  }
  if (missing.length > 0) {
    problems.push(`${label} is missing keys: ${missing.join(", ")}`);
  }
  return extras.length === 0 && missing.length === 0;
}

function string(value, label, problems, pattern = null) {
  if (typeof value !== "string" || value === "") {
    problems.push(`${label} must be a non-empty string`);
    return false;
  }
  if (pattern && !pattern.test(value)) {
    problems.push(`${label} has invalid value ${JSON.stringify(value)}`);
    return false;
  }
  return true;
}

function integer(value, label, problems, min = 0) {
  if (!Number.isSafeInteger(value) || value < min) {
    problems.push(`${label} must be an integer >= ${min}`);
    return false;
  }
  return true;
}

function validateSource(source, label, problems) {
  if (!exactKeys(source, SOURCE_KEYS, label, problems)) return;
  string(source.sha, `${label}.sha`, problems, SHA_RE);
  string(source.tree, `${label}.tree`, problems, SHA_RE);
}

function validateRun(run, label, problems) {
  if (!exactKeys(run, RUN_KEYS, label, problems)) return;
  string(run.id, `${label}.id`, problems, RUN_ID_RE);
  integer(run.attempt, `${label}.attempt`, problems, 1);
}

function validateEvent(event, source, label, problems) {
  if (!exactKeys(event, EVENT_KEYS, label, problems)) return;
  string(event.kind, `${label}.kind`, problems);
  if (!RELEASE_EVENTS.has(event.kind)) {
    problems.push(`${label}.kind must be push or workflow_dispatch`);
  }
  if (event.pr_number !== null) problems.push(`${label}.pr_number must be null`);
  string(event.event_head_sha, `${label}.event_head_sha`, problems, SHA_RE);
  if (event.event_base_sha !== null) {
    problems.push(`${label}.event_base_sha must be null`);
  }
  string(event.projected_sha, `${label}.projected_sha`, problems, SHA_RE);
  if (
    event.event_head_sha !== source?.sha ||
    event.projected_sha !== source?.sha
  ) {
    problems.push(`${label} must bind the exact release source SHA`);
  }
}

function validateSelectionRef(reference, label, problems) {
  if (!exactKeys(reference, SELECTION_REF_KEYS, label, problems)) return;
  validateRun(reference.run, `${label}.run`, problems);
  string(
    reference.manifest_sha256,
    `${label}.manifest_sha256`,
    problems,
    SHA256_RE,
  );
}

function readJSON(path, label = path) {
  if (!existsSync(path)) {
    throw new QualificationError([`${label} does not exist: ${path}`]);
  }
  try {
    return JSON.parse(readFileSync(path, "utf8"));
  } catch (cause) {
    throw new QualificationError([
      `${label} is not valid JSON: ${cause.message}`,
    ]);
  }
}

function writeCanonical(path, value) {
  writeFileSync(path, `${canonicalJSON(value)}\n`);
}

function writeSelection(path, value) {
  // selectionManifestSHA256 deliberately covers the standard indented
  // selection representation, including its terminating newline.
  writeFileSync(path, `${JSON.stringify(value, null, 2)}\n`);
}

function requiredEnv(name, pattern = null) {
  const value = process.env[name];
  const problems = [];
  string(value, name, problems, pattern);
  if (problems.length > 0) throw new QualificationError(problems);
  return value;
}

function positiveIntegerEnv(name) {
  const raw = process.env[name];
  if (!/^[1-9][0-9]*$/.test(raw ?? "")) {
    throw new QualificationError([
      `${name} must be a canonical positive integer`,
    ]);
  }
  const value = Number(raw);
  if (!Number.isSafeInteger(value)) {
    throw new QualificationError([`${name} exceeds safe integer range`]);
  }
  return value;
}

function positiveMillis(name, raw = process.env[name]) {
  if (!/^[1-9][0-9]*$/.test(raw ?? "")) {
    throw new QualificationError([
      `${name} must be canonical positive epoch milliseconds`,
    ]);
  }
  const value = Number(raw);
  if (!Number.isSafeInteger(value)) {
    throw new QualificationError([`${name} exceeds safe integer range`]);
  }
  return value;
}

function identity(value) {
  return `${value.lane_id}/${value.execution_id}/${value.invocation_mode}`;
}

function selectedInvocationRoster(selection, registry) {
  const lanes = new Map(registry.lanes.map((lane) => [lane.id, lane]));
  const roster = [];
  for (const item of selection.lanes ?? []) {
    if (item.disposition !== "selected") continue;
    const lane = lanes.get(item.lane_id);
    if (!lane) continue;
    const modes = [];
    if (lane.applicability.source) modes.push("source_tree");
    if (lane.applicability.artifact) modes.push("candidate_artifact");
    for (const executionID of item.executions) {
      for (const invocationMode of modes) {
        roster.push({
          lane_id: lane.id,
          execution_id: executionID,
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
  return roster.sort((left, right) => identity(left).localeCompare(identity(right)));
}

export function releaseLaneRoster(registry) {
  const roster = [];
  for (const lane of registry.lanes) {
    if (!FINITE_DETERMINISM.has(lane.determinism)) continue;
    if (lane.release_posture === "post_publish") continue;
    for (const executionID of lane.executions) {
      const modes = [];
      if (lane.applicability.source) modes.push("source_tree");
      if (lane.applicability.artifact) modes.push("candidate_artifact");
      for (const invocationMode of modes) {
        roster.push({
          lane_id: lane.id,
          execution_id: executionID,
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
  const problems = [];
  if (!roster.some((item) => identity(item) === QUICKSTART_IDENTITY)) {
    problems.push(`${QUICKSTART_IDENTITY} is absent from release qualification`);
  }
  if (roster.length !== RELEASE_QUALIFICATION_INVOCATIONS) {
    problems.push(
      `release qualification must contain exactly ${RELEASE_QUALIFICATION_INVOCATIONS} ` +
        `invocations; registry produced ${roster.length}`,
    );
  }
  if (problems.length > 0) throw new QualificationError(problems);
  return roster;
}

export function createReleaseSelection({
  registry,
  source,
  candidateDigest,
  correlationNonce,
  run,
}) {
  const lanes = registry.lanes
    .map((lane) => {
      if (lane.release_posture === "post_publish") {
        return {
          lane_id: lane.id,
          disposition: "omitted",
          executions: [],
          reason: "post_publish",
        };
      }
      if (!FINITE_DETERMINISM.has(lane.determinism)) {
        return {
          lane_id: lane.id,
          disposition: "omitted",
          executions: [],
          reason: "advisory",
        };
      }
      return {
        lane_id: lane.id,
        disposition: "selected",
        executions: [...lane.executions],
        reason: null,
      };
    })
    .sort((left, right) => left.lane_id.localeCompare(right.lane_id));
  const selection = {
    schema_version: registry.selection_schema_version,
    registry_schema_version: registry.schema_version,
    report_schema_version: registry.report_schema_version,
    posture: "release",
    source: { ...source },
    candidate_digest: candidateDigest,
    correlation_nonce: correlationNonce,
    run: { ...run },
    selector: {
      conclusion: "success",
      base_sha: null,
      head_sha: null,
      changed_paths: [],
      unknown_paths: [],
    },
    lanes,
  };
  try {
    validateSelection(selection, registry, {
      posture: "release",
      sha: source.sha,
      tree: source.tree,
      candidateDigest,
      correlationNonce,
      runID: run.id,
      runAttempt: run.attempt,
    });
  } catch (cause) {
    if (cause instanceof ContractError) {
      throw new QualificationError(cause.problems);
    }
    throw cause;
  }
  if (
    canonicalJSON(selectedInvocationRoster(selection, registry)) !==
    canonicalJSON(releaseLaneRoster(registry))
  ) {
    throw new QualificationError([
      "release selection does not dispatch the exact finite invocation roster",
    ]);
  }
  return selection;
}

export function createPlan({
  registry,
  selection,
  eventIdentity,
  repository,
  startedAtMs,
}) {
  const plan = {
    schema_version: QUALIFICATION_SCHEMA_VERSION,
    registry_schema_version: registry.schema_version,
    selection_schema_version: registry.selection_schema_version,
    report_schema_version: registry.report_schema_version,
    source: { ...selection.source },
    candidate_digest: selection.candidate_digest,
    correlation_nonce: selection.correlation_nonce,
    selection_ref: {
      run: { ...selection.run },
      manifest_sha256: selectionManifestSHA256(selection),
    },
    event: { ...eventIdentity },
    repository,
    started_at_ms: startedAtMs,
    deadline_at_ms: startedAtMs + RELEASE_QUALIFICATION_LIMIT_MS,
    invocations: releaseLaneRoster(registry),
  };
  return validatePlan(plan, registry, selection);
}

export function validatePlan(plan, registry, selection, expected = {}) {
  const problems = [];
  if (!exactKeys(plan, PLAN_KEYS, "plan", problems)) {
    throw new QualificationError(problems);
  }
  if (plan.schema_version !== QUALIFICATION_SCHEMA_VERSION) {
    problems.push(`plan.schema_version must be ${QUALIFICATION_SCHEMA_VERSION}`);
  }
  for (const [field, actual, wanted] of [
    ["registry_schema_version", plan.registry_schema_version, registry.schema_version],
    [
      "selection_schema_version",
      plan.selection_schema_version,
      registry.selection_schema_version,
    ],
    ["report_schema_version", plan.report_schema_version, registry.report_schema_version],
  ]) {
    if (actual !== wanted) problems.push(`plan.${field} does not match the registry`);
  }
  validateSource(plan.source, "plan.source", problems);
  string(plan.candidate_digest, "plan.candidate_digest", problems, DIGEST_RE);
  string(plan.correlation_nonce, "plan.correlation_nonce", problems, NONCE_RE);
  validateSelectionRef(plan.selection_ref, "plan.selection_ref", problems);
  validateEvent(plan.event, plan.source, "plan.event", problems);
  string(plan.repository, "plan.repository", problems, REPOSITORY_RE);
  integer(plan.started_at_ms, "plan.started_at_ms", problems, 1);
  integer(plan.deadline_at_ms, "plan.deadline_at_ms", problems, 1);
  if (
    plan.deadline_at_ms - plan.started_at_ms !==
    RELEASE_QUALIFICATION_LIMIT_MS
  ) {
    problems.push(
      `plan deadline must be exactly ${RELEASE_QUALIFICATION_LIMIT_MINUTES} minutes after start`,
    );
  }

  try {
    validateSelection(selection, registry, {
      posture: "release",
      sha: plan.source.sha,
      tree: plan.source.tree,
      candidateDigest: plan.candidate_digest,
      correlationNonce: plan.correlation_nonce,
      runID: plan.selection_ref.run.id,
      runAttempt: plan.selection_ref.run.attempt,
    });
  } catch (cause) {
    if (cause instanceof ContractError) {
      problems.push(...cause.problems.map((problem) => `selection: ${problem}`));
    } else {
      throw cause;
    }
  }
  if (
    plan.selection_ref.manifest_sha256 !== selectionManifestSHA256(selection)
  ) {
    problems.push("plan selection manifest SHA-256 does not match the selection");
  }
  if (
    plan.selection_ref.run.id !== selection.run?.id ||
    plan.selection_ref.run.attempt !== selection.run?.attempt
  ) {
    problems.push("plan selection run does not match the selection");
  }

  let expectedRoster;
  try {
    expectedRoster = releaseLaneRoster(registry);
  } catch (cause) {
    if (cause instanceof QualificationError) problems.push(...cause.problems);
    else throw cause;
  }
  if (!Array.isArray(plan.invocations)) {
    problems.push("plan.invocations must be an array");
  } else {
    for (let index = 0; index < plan.invocations.length; index += 1) {
      const invocation = plan.invocations[index];
      const label = `plan.invocations[${index}]`;
      if (!exactKeys(invocation, PLAN_INVOCATION_KEYS, label, problems)) continue;
      string(invocation.lane_id, `${label}.lane_id`, problems);
      string(invocation.execution_id, `${label}.execution_id`, problems);
      string(invocation.invocation_mode, `${label}.invocation_mode`, problems);
      string(invocation.determinism, `${label}.determinism`, problems);
      if (
        exactKeys(
          invocation.producer,
          PLAN_PRODUCER_KEYS,
          `${label}.producer`,
          problems,
        )
      ) {
        string(
          invocation.producer.workflow,
          `${label}.producer.workflow`,
          problems,
        );
        string(invocation.producer.job, `${label}.producer.job`, problems);
      }
    }
    if (
      expectedRoster &&
      canonicalJSON(plan.invocations) !== canonicalJSON(expectedRoster)
    ) {
      problems.push(
        "plan.invocations does not equal the registry-derived finite release roster",
      );
    }
    if (
      canonicalJSON(plan.invocations) !==
      canonicalJSON(selectedInvocationRoster(selection, registry))
    ) {
      problems.push("plan.invocations does not equal the canonical selection");
    }
  }

  for (const [label, actual, wanted] of [
    ["source.sha", plan.source?.sha, expected.sha],
    ["source.tree", plan.source?.tree, expected.tree],
    ["candidate_digest", plan.candidate_digest, expected.candidateDigest],
    ["correlation_nonce", plan.correlation_nonce, expected.correlationNonce],
    [
      "selection_ref.manifest_sha256",
      plan.selection_ref?.manifest_sha256,
      expected.selectionManifestSHA256,
    ],
    ["repository", plan.repository, expected.repository],
  ]) {
    if (wanted !== undefined && actual !== wanted) {
      problems.push(`plan.${label} is ${actual}, want ${wanted}`);
    }
  }
  if (
    expected.eventIdentity !== undefined &&
    canonicalJSON(plan.event) !== canonicalJSON(expected.eventIdentity)
  ) {
    problems.push("plan.event does not match the trusted expected event");
  }
  if (problems.length > 0) throw new QualificationError(problems);
  return plan;
}

function qualificationExpected(plan) {
  return {
    posture: "release",
    sha: plan.source.sha,
    tree: plan.source.tree,
    candidateDigest: plan.candidate_digest,
    correlationNonce: plan.correlation_nonce,
    runID: plan.selection_ref.run.id,
    runAttempt: plan.selection_ref.run.attempt,
    selectionManifestSHA256: plan.selection_ref.manifest_sha256,
    eventIdentity: plan.event,
    repository: plan.repository,
    baseSHA: undefined,
    changedPaths: undefined,
  };
}

function attestedLanes(reports, producerManifest, registry) {
  const lanes = new Map(registry.lanes.map((lane) => [lane.id, lane]));
  const producers = new Map(
    producerManifest.producers.map((producer) => [producer.workflow, producer]),
  );
  return reports
    .map((report) => {
      const lane = lanes.get(report.lane_id);
      const trusted = producers.get(report.producer.workflow);
      const context = trusted.jobs.find((job) => {
        if (job.job !== report.producer.job) return false;
        return lane.context.match === "exact"
          ? job.name === lane.context.name
          : job.name.startsWith(lane.context.name);
      });
      return {
        identity: `${report.lane_id}/${report.execution_id}/${report.invocation.mode}`,
        producer: {
          workflow: report.producer.workflow,
          job: report.producer.job,
          run: { ...report.producer.run },
          job_name: context.name,
          job_database_id: context.database_id,
        },
        report_artifact: { ...report.producer.artifact },
        evidence: { ...report.evidence },
      };
    })
    .sort((left, right) => left.identity.localeCompare(right.identity));
}

export function evaluateQualification({
  registry,
  plan,
  selection,
  reports,
  producerManifest,
  candidateManifest,
  nowMs,
}) {
  validatePlan(plan, registry, selection);
  const problems = [];
  integer(nowMs, "qualification.now_ms", problems, 1);
  if (nowMs < plan.started_at_ms) {
    problems.push("qualification completed before it started");
  }
  if (nowMs > plan.deadline_at_ms) {
    problems.push(
      `qualification timed out at ${nowMs}; deadline was ${plan.deadline_at_ms} ` +
        `(${RELEASE_QUALIFICATION_LIMIT_MINUTES} minutes)`,
    );
  }
  if (!Array.isArray(reports)) {
    problems.push("release reports must be a JSON array");
  }
  if (!isObject(producerManifest)) {
    problems.push("trusted producer manifest must be an object");
  }
  if (problems.length > 0) throw new QualificationError(problems);

  let result;
  try {
    result = validateReportSet({
      registry,
      selection,
      reports,
      producerManifest,
      expected: qualificationExpected(plan),
    });
  } catch (cause) {
    if (cause instanceof ContractError) {
      throw new QualificationError(cause.problems);
    }
    throw cause;
  }
  if (
    result.expected !== RELEASE_QUALIFICATION_INVOCATIONS ||
    result.received !== RELEASE_QUALIFICATION_INVOCATIONS
  ) {
    throw new QualificationError([
      `canonical report validation must receive exactly ${RELEASE_QUALIFICATION_INVOCATIONS} ` +
        `invocations; expected ${result.expected}, received ${result.received}`,
    ]);
  }

  const lanes = attestedLanes(reports, producerManifest, registry);
  if (!lanes.some((lane) => lane.identity === QUICKSTART_IDENTITY)) {
    throw new QualificationError([
      `required quickstart proof ${QUICKSTART_IDENTITY} is absent`,
    ]);
  }
  return {
    schema_version: ATTESTATION_SCHEMA_VERSION,
    result: SUCCESS,
    source: { ...plan.source },
    candidate: {
      digest: plan.candidate_digest,
      image_digest: candidateManifest.image.digest,
      files: candidateManifest.files.map((file) => ({
        path: file.path,
        sha256: file.sha256,
        size: file.size,
      })),
    },
    qualification: {
      started_at_ms: plan.started_at_ms,
      completed_at_ms: nowMs,
      deadline_at_ms: plan.deadline_at_ms,
      limit_minutes: RELEASE_QUALIFICATION_LIMIT_MINUTES,
      correlation_nonce: plan.correlation_nonce,
      selection_ref: structuredClone(plan.selection_ref),
      event: { ...plan.event },
      repository: plan.repository,
      invocation_count: RELEASE_QUALIFICATION_INVOCATIONS,
    },
    lanes,
  };
}

function validateEvidence(evidence, label, planned, problems) {
  if (!exactKeys(evidence, EVIDENCE_KEYS, label, problems)) return;
  for (const key of ["executed", "passed", "failed", "skipped", "duration_ms"]) {
    integer(evidence[key], `${label}.${key}`, problems);
  }
  if (evidence.seed !== null) {
    string(evidence.seed, `${label}.seed`, problems);
  }
  string(evidence.corpus_id, `${label}.corpus_id`, problems);
  if (
    evidence.executed <= 0 ||
    evidence.executed !== evidence.passed + evidence.failed + evidence.skipped ||
    evidence.failed !== 0 ||
    evidence.skipped !== 0 ||
    evidence.passed !== evidence.executed
  ) {
    problems.push(`${label} is not a positive all-passed native result`);
  }
  if (planned?.determinism === "deterministic" && evidence.seed !== null) {
    problems.push(`${label} deterministic lane must carry seed=null`);
  }
  if (planned?.determinism === "seeded" && evidence.seed === null) {
    problems.push(`${label} seeded lane omitted its seed`);
  }
}

export function validateAttestation(attestation, expected = {}, registry = null) {
  const problems = [];
  if (!exactKeys(attestation, ATTESTATION_KEYS, "attestation", problems)) {
    throw new QualificationError(problems);
  }
  if (attestation.schema_version !== ATTESTATION_SCHEMA_VERSION) {
    problems.push(
      `attestation.schema_version must be ${ATTESTATION_SCHEMA_VERSION}`,
    );
  }
  if (attestation.result !== SUCCESS) {
    problems.push("attestation.result must be success");
  }
  validateSource(attestation.source, "attestation.source", problems);
  if (
    exactKeys(
      attestation.candidate,
      ATTESTATION_CANDIDATE_KEYS,
      "attestation.candidate",
      problems,
    )
  ) {
    string(
      attestation.candidate.digest,
      "attestation.candidate.digest",
      problems,
      DIGEST_RE,
    );
    string(
      attestation.candidate.image_digest,
      "attestation.candidate.image_digest",
      problems,
      DIGEST_RE,
    );
    if (
      !Array.isArray(attestation.candidate.files) ||
      attestation.candidate.files.length === 0
    ) {
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

  if (
    exactKeys(
      attestation.qualification,
      ATTESTATION_QUALIFICATION_KEYS,
      "attestation.qualification",
      problems,
    )
  ) {
    for (const key of ["started_at_ms", "completed_at_ms", "deadline_at_ms"]) {
      integer(
        attestation.qualification[key],
        `attestation.qualification.${key}`,
        problems,
        1,
      );
    }
    integer(
      attestation.qualification.limit_minutes,
      "attestation.qualification.limit_minutes",
      problems,
      1,
    );
    string(
      attestation.qualification.correlation_nonce,
      "attestation.qualification.correlation_nonce",
      problems,
      NONCE_RE,
    );
    validateSelectionRef(
      attestation.qualification.selection_ref,
      "attestation.qualification.selection_ref",
      problems,
    );
    validateEvent(
      attestation.qualification.event,
      attestation.source,
      "attestation.qualification.event",
      problems,
    );
    string(
      attestation.qualification.repository,
      "attestation.qualification.repository",
      problems,
      REPOSITORY_RE,
    );
    if (
      attestation.qualification.invocation_count !==
      RELEASE_QUALIFICATION_INVOCATIONS
    ) {
      problems.push(
        `attestation.qualification.invocation_count must be ${RELEASE_QUALIFICATION_INVOCATIONS}`,
      );
    }
    if (
      attestation.qualification.deadline_at_ms -
        attestation.qualification.started_at_ms !==
      RELEASE_QUALIFICATION_LIMIT_MS
    ) {
      problems.push(
        "attestation qualification deadline does not equal the hard release limit",
      );
    }
    if (
      attestation.qualification.completed_at_ms <
        attestation.qualification.started_at_ms ||
      attestation.qualification.completed_at_ms >
        attestation.qualification.deadline_at_ms
    ) {
      problems.push(
        "attestation completion is outside its qualification window",
      );
    }
  }

  let expectedLanes = null;
  if (registry) {
    try {
      expectedLanes = new Map(
        releaseLaneRoster(registry).map((lane) => [identity(lane), lane]),
      );
    } catch (cause) {
      if (cause instanceof QualificationError) problems.push(...cause.problems);
      else throw cause;
    }
  }
  const actualLanes = new Map();
  if (!Array.isArray(attestation.lanes)) {
    problems.push("attestation.lanes must be an array");
  } else {
    if (attestation.lanes.length !== RELEASE_QUALIFICATION_INVOCATIONS) {
      problems.push(
        `attestation.lanes must contain exactly ${RELEASE_QUALIFICATION_INVOCATIONS} identities`,
      );
    }
    let previous = "";
    for (let index = 0; index < attestation.lanes.length; index += 1) {
      const lane = attestation.lanes[index];
      const label = `attestation.lanes[${index}]`;
      if (!exactKeys(lane, ATTESTATION_LANE_KEYS, label, problems)) continue;
      string(lane.identity, `${label}.identity`, problems);
      if (actualLanes.has(lane.identity)) {
        problems.push(`${label}.identity is duplicated`);
      }
      if (previous !== "" && String(lane.identity).localeCompare(previous) <= 0) {
        problems.push(`${label}.identity is not strictly sorted`);
      }
      actualLanes.set(lane.identity, lane);
      previous = lane.identity;
      const planned = expectedLanes?.get(lane.identity);
      if (expectedLanes && !planned) {
        problems.push(`${label}.identity is not in the finite release roster`);
      }
      if (
        exactKeys(
          lane.producer,
          ATTESTATION_PRODUCER_KEYS,
          `${label}.producer`,
          problems,
        )
      ) {
        string(
          lane.producer.workflow,
          `${label}.producer.workflow`,
          problems,
        );
        string(lane.producer.job, `${label}.producer.job`, problems);
        validateRun(lane.producer.run, `${label}.producer.run`, problems);
        string(lane.producer.job_name, `${label}.producer.job_name`, problems);
        string(
          lane.producer.job_database_id,
          `${label}.producer.job_database_id`,
          problems,
          RUN_ID_RE,
        );
        if (
          planned &&
          (lane.producer.workflow !== planned.producer.workflow ||
            lane.producer.job !== planned.producer.job)
        ) {
          problems.push(`${label}.producer does not match the registry owner`);
        }
      }
      if (
        exactKeys(
          lane.report_artifact,
          ATTESTATION_ARTIFACT_KEYS,
          `${label}.report_artifact`,
          problems,
        )
      ) {
        string(
          lane.report_artifact.id,
          `${label}.report_artifact.id`,
          problems,
          RUN_ID_RE,
        );
        string(
          lane.report_artifact.name,
          `${label}.report_artifact.name`,
          problems,
        );
        string(
          lane.report_artifact.sha256,
          `${label}.report_artifact.sha256`,
          problems,
          SHA256_RE,
        );
        string(
          lane.report_artifact.entry,
          `${label}.report_artifact.entry`,
          problems,
        );
        string(
          lane.report_artifact.entry_sha256,
          `${label}.report_artifact.entry_sha256`,
          problems,
          SHA256_RE,
        );
        if (lane.report_artifact.entry !== lane.identity) {
          problems.push(`${label}.report_artifact.entry must equal its identity`);
        }
      }
      validateEvidence(lane.evidence, `${label}.evidence`, planned, problems);
    }
  }
  if (expectedLanes) {
    for (const expectedIdentity of expectedLanes.keys()) {
      if (!actualLanes.has(expectedIdentity)) {
        problems.push(`attestation is missing release lane ${expectedIdentity}`);
      }
    }
  }
  if (!actualLanes.has(QUICKSTART_IDENTITY)) {
    problems.push(
      `attestation does not contain required quickstart proof ${QUICKSTART_IDENTITY}`,
    );
  }
  if (
    attestation.qualification?.limit_minutes !==
    RELEASE_QUALIFICATION_LIMIT_MINUTES
  ) {
    problems.push(
      `attestation qualification limit must be ${RELEASE_QUALIFICATION_LIMIT_MINUTES} minutes`,
    );
  }

  for (const [label, actual, wanted] of [
    ["source.sha", attestation.source?.sha, expected.sha],
    ["source.tree", attestation.source?.tree, expected.tree],
    ["candidate.digest", attestation.candidate?.digest, expected.candidateDigest],
    [
      "candidate.image_digest",
      attestation.candidate?.image_digest,
      expected.imageDigest,
    ],
    [
      "qualification.correlation_nonce",
      attestation.qualification?.correlation_nonce,
      expected.correlationNonce,
    ],
    [
      "qualification.selection_ref.manifest_sha256",
      attestation.qualification?.selection_ref?.manifest_sha256,
      expected.selectionManifestSHA256,
    ],
    [
      "qualification.repository",
      attestation.qualification?.repository,
      expected.repository,
    ],
  ]) {
    if (wanted !== undefined && actual !== wanted) {
      problems.push(`attestation.${label} is ${actual}, want ${wanted}`);
    }
  }
  if (
    expected.eventIdentity !== undefined &&
    canonicalJSON(attestation.qualification?.event) !==
      canonicalJSON(expected.eventIdentity)
  ) {
    problems.push("attestation qualification event does not match expected event");
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
  return verifyCandidate(
    resolve(process.env.RELEASE_CANDIDATE_DIR || "build/release-candidate"),
    {
      sha: expected.sha,
      tree: expected.tree,
      app: expected.app,
      appTag: expected.appTag,
      chart: expected.chart,
      digest: expected.candidateDigest,
    },
  );
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
    {
      ...expected,
      imageDigest: candidate.manifest.image.digest,
    },
    registry,
  );
  if (
    canonicalJSON(attestation.candidate.files) !==
    canonicalJSON(candidate.manifest.files)
  ) {
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
  const eventKind = requiredEnv("GITHUB_EVENT_NAME");
  if (!RELEASE_EVENTS.has(eventKind)) {
    throw new QualificationError([
      `GITHUB_EVENT_NAME must be push or workflow_dispatch; got ${JSON.stringify(eventKind)}`,
    ]);
  }
  const run = {
    id: requiredEnv("GITHUB_RUN_ID", RUN_ID_RE),
    attempt: positiveIntegerEnv("GITHUB_RUN_ATTEMPT"),
  };
  const selection = createReleaseSelection({
    registry,
    source: { sha: expected.sha, tree: expected.tree },
    candidateDigest: expected.candidateDigest,
    correlationNonce: expected.correlationNonce,
    run,
  });
  const eventIdentity = {
    kind: eventKind,
    pr_number: null,
    event_head_sha: expected.sha,
    event_base_sha: null,
    projected_sha: expected.sha,
  };
  const plan = createPlan({
    registry,
    selection,
    eventIdentity,
    repository: requiredEnv("GITHUB_REPOSITORY", REPOSITORY_RE),
    startedAtMs: positiveMillis("RELEASE_QUALIFICATION_START"),
  });
  const selectionPath = resolve(
    process.env.RELEASE_QUALIFICATION_SELECTION ||
      "build/release-qualification-selection.json",
  );
  const planPath = resolve(
    process.env.RELEASE_QUALIFICATION_PLAN ||
      "build/release-qualification-plan.json",
  );
  writeSelection(selectionPath, selection);
  writeCanonical(planPath, plan);
  setOutput("selection", selectionPath);
  setOutput("selection_digest", plan.selection_ref.manifest_sha256);
  setOutput("plan", planPath);
  setOutput("lane_count", String(plan.invocations.length));
  notice(
    `planned ${plan.invocations.length} finite release invocations for ` +
      `${expected.sha.slice(0, 12)} candidate ${expected.candidateDigest}`,
  );
}

function joinMode() {
  const expected = environmentIdentity();
  const candidate = verifiedCandidate(expected);
  const registry = loadRegistry();
  const selectionPath = resolve(
    process.env.RELEASE_QUALIFICATION_SELECTION ||
      "build/release-qualification-selection.json",
  );
  const planPath = resolve(
    process.env.RELEASE_QUALIFICATION_PLAN ||
      "build/release-qualification-plan.json",
  );
  const reportsPath = resolve(requiredEnv("RELEASE_QUALIFICATION_REPORTS"));
  const producerManifestPath = resolve(
    requiredEnv("RELEASE_QUALIFICATION_PRODUCER_MANIFEST"),
  );
  const selection = readJSON(selectionPath, "release qualification selection");
  const plan = validatePlan(
    readJSON(planPath, "release qualification plan"),
    registry,
    selection,
    expected,
  );
  const nowMs = process.env.RELEASE_QUALIFICATION_NOW
    ? positiveMillis("RELEASE_QUALIFICATION_NOW")
    : Date.now();
  const attestation = evaluateQualification({
    registry,
    plan,
    selection,
    reports: readJSON(reportsPath, "release qualification reports"),
    producerManifest: readJSON(
      producerManifestPath,
      "release trusted producer manifest",
    ),
    candidateManifest: candidate.manifest,
    nowMs,
  });
  const attestationPath = resolve(
    process.env.RELEASE_ATTESTATION ||
      "build/release-qualification-attestation.json",
  );
  writeCanonical(attestationPath, attestation);
  const digest = sha256File(attestationPath);
  setOutput("qualified", "true");
  setOutput("attestation", attestationPath);
  setOutput("attestation_digest", digest);
  notice(
    `release qualification passed ${attestation.lanes.length} invocations in ` +
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

const invokedDirectly =
  process.argv[1] && fileURLToPath(import.meta.url) === resolve(process.argv[1]);
if (invokedDirectly) {
  try {
    main();
  } catch (cause) {
    error(cause instanceof Error ? cause.message : String(cause));
    process.exit(1);
  }
}
