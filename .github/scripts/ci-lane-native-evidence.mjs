// ci-lane-native-evidence.mjs — emit selection-independent, attempt-qualified
// evidence from a workflow's own `needs` graph.
//
// A workflow calls this once from an unconditional terminal evidence job. The
// job passes GitHub's `toJSON(needs)` value; this script derives the lane and
// check rosters from the registry and derives every count from those results.
// It never accepts caller-supplied counts, lane conclusions, source trees, or
// execution rosters.
//
// Required environment:
//   CI_LANE_WORKFLOW       canonical repository-relative workflow path.
//   CI_LANE_NEEDS_JSON     GitHub `toJSON(needs)` for the evidence job.
//   CI_LANE_NATIVE_OUTPUT  new bundle path (must not already exist).
//   GITHUB_EVENT_NAME      producer event.
//   GITHUB_REPOSITORY      producer repository as owner/name.
//   GITHUB_RUN_ID          producer workflow run id.
//   GITHUB_RUN_ATTEMPT     positive producer attempt.
//   GITHUB_SHA             exact commit checked out at HEAD.
//
// Optional environment:
//   CI_LANE_REGISTRY       registry path (default .github/ci-lanes.json).
//   CI_LANE_EVIDENCE_JOB   current terminal job id (default native-evidence).
//   CI_LANE_CORRELATION_NONCE explicit 64-hex nonce; mandatory for release.
//   GITHUB_OUTPUT          Actions step-output destination.
//
// Node builtins only. Malformed topology, missing/extra needs, a dirty or wrong
// checkout, and stale output reuse are hard failures. Native red/cancelled/
// skipped results are valid evidence and are recorded rather than hidden.

import { createHash } from "node:crypto";
import {
  existsSync,
  linkSync,
  statSync,
  unlinkSync,
  writeFileSync,
} from "node:fs";
import { dirname, resolve } from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";

import {
  canonicalJSONSHA256,
  ContractError,
  DEFAULT_REGISTRY_PATH,
  deriveQualificationCorrelationNonce,
  loadRegistry,
  nativeArtifactName,
} from "./ci-lane-contract.mjs";
import { capture, error, notice, setOutput } from "./lib/gh.mjs";

export const NATIVE_EVIDENCE_SCHEMA_VERSION = 1;

const shaPattern = /^(?:[0-9a-f]{40}|[0-9a-f]{64})$/;
const runIDPattern = /^[1-9][0-9]*$/;
const repositoryPattern = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;
const workflowPattern = /^\.github\/workflows\/[A-Za-z0-9_.-]+\.ya?ml$/;
const eventPattern = /^[a-z][a-z0-9_]*$/;
const correlationNoncePattern = /^[0-9a-f]{64}$/;
const postures = new Set(["merge", "main", "release"]);
const nativeResults = new Set(["success", "failure", "cancelled", "skipped"]);

function requirePattern(value, pattern, name) {
  const text = String(value ?? "").trim();
  if (!pattern.test(text)) {
    throw new ContractError("CI lane native evidence", [
      `${name} has invalid value ${JSON.stringify(text)}`,
    ]);
  }
  return text;
}

function positiveInteger(value, name) {
  const text = requirePattern(value, runIDPattern, name);
  const parsed = Number(text);
  if (!Number.isSafeInteger(parsed) || parsed < 1) {
    throw new ContractError("CI lane native evidence", [
      `${name} must be a positive safe integer`,
    ]);
  }
  return parsed;
}

function gitText(root, args) {
  return capture("git", args, { cwd: root });
}

export function nativeCheckoutEvidence({ root, expectedSHA }) {
  const expected = requirePattern(expectedSHA, shaPattern, "GITHUB_SHA");
  const top = gitText(root, ["rev-parse", "--show-toplevel"]);
  if (top.status !== 0 || resolve(top.stdout.trim()) !== resolve(root)) {
    throw new ContractError("CI lane native evidence", [
      "native evidence must run from the repository root",
    ]);
  }
  const actual = gitText(root, ["rev-parse", "--verify", "HEAD^{commit}"]);
  if (actual.status !== 0 || actual.stdout.trim() !== expected) {
    throw new ContractError("CI lane native evidence", [
      `checkout HEAD is ${JSON.stringify(actual.stdout.trim())}, want ${expected}`,
    ]);
  }
  const status = gitText(root, [
    "status",
    "--porcelain=v1",
    "--untracked-files=all",
  ]);
  if (status.status !== 0 || status.stdout !== "") {
    throw new ContractError("CI lane native evidence", [
      status.status === 0
        ? "checkout is not clean before native evidence is derived"
        : `git status failed: ${status.stderr.trim()}`,
    ]);
  }
  const tree = gitText(root, ["rev-parse", "--verify", `${expected}^{tree}`]);
  const treeSHA = tree.stdout.trim();
  if (tree.status !== 0 || !shaPattern.test(treeSHA)) {
    throw new ContractError("CI lane native evidence", [
      `cannot derive checkout tree: ${tree.stderr.trim() || treeSHA}`,
    ]);
  }
  return Object.freeze({ sha: expected, tree: treeSHA });
}

export function parseNeeds(raw) {
  let document;
  try {
    document = JSON.parse(String(raw ?? ""));
  } catch (parseError) {
    throw new ContractError("CI lane native evidence", [
      `CI_LANE_NEEDS_JSON is not valid JSON: ${parseError.message}`,
    ]);
  }
  if (
    document === null ||
    Array.isArray(document) ||
    typeof document !== "object"
  ) {
    throw new ContractError("CI lane native evidence", [
      "CI_LANE_NEEDS_JSON must be an object keyed by workflow job id",
    ]);
  }
  const results = new Map();
  const problems = [];
  for (const [jobID, value] of Object.entries(document)) {
    if (!/^[A-Za-z_][A-Za-z0-9_-]*$/.test(jobID)) {
      problems.push(`needs key ${JSON.stringify(jobID)} is not a workflow job id`);
      continue;
    }
    if (
      value === null ||
      Array.isArray(value) ||
      typeof value !== "object" ||
      !nativeResults.has(value.result)
    ) {
      problems.push(
        `needs.${jobID}.result must be one of ${[...nativeResults].join(", ")}`,
      );
      continue;
    }
    results.set(jobID, value.result);
  }
  if (problems.length > 0) {
    throw new ContractError("CI lane native evidence", problems);
  }
  return results;
}

function laneConclusion(checks) {
  const results = checks.map((check) => check.result);
  if (results.includes("failure")) return "failure";
  if (results.includes("cancelled")) return "cancelled";
  if (results.includes("skipped")) return "skipped";
  return "success";
}

function evidenceCounts(checks) {
  return Object.freeze({
    executed: checks.length,
    passed: checks.filter((check) => check.result === "success").length,
    failed: checks.filter(
      (check) => check.result === "failure" || check.result === "cancelled",
    ).length,
    skipped: checks.filter((check) => check.result === "skipped").length,
  });
}

function postureIncludes(lane, posture) {
  if (posture === "merge") return lane.merge_posture !== "never";
  if (posture === "main") return lane.main_posture !== "never";
  return lane.release_posture !== "post_publish";
}

function nativeRoster(registry, workflow, posture) {
  return registry.lanes.filter(
    (lane) =>
      lane.owner.workflow === workflow && postureIncludes(lane, posture),
  );
}

export function createNativeBundle({
  registry,
  workflow,
  needs,
  source,
  repository,
  event,
  runID,
  runAttempt,
  posture = "merge",
  evidenceJob = "native-evidence",
  correlationNonce,
}) {
  const workflowPath = requirePattern(
    workflow,
    workflowPattern,
    "CI_LANE_WORKFLOW",
  );
  const repositoryName = requirePattern(
    repository,
    repositoryPattern,
    "GITHUB_REPOSITORY",
  );
  const eventName = requirePattern(event, eventPattern, "GITHUB_EVENT_NAME");
  const postureName = String(posture ?? "").trim();
  if (!postures.has(postureName)) {
    throw new ContractError("CI lane native evidence", [
      `CI_LANE_NATIVE_POSTURE must be merge, main, or release`,
    ]);
  }
  const id = requirePattern(runID, runIDPattern, "GITHUB_RUN_ID");
  const attempt =
    typeof runAttempt === "number"
      ? runAttempt
      : positiveInteger(runAttempt, "GITHUB_RUN_ATTEMPT");
  if (!Number.isSafeInteger(attempt) || attempt < 1) {
    throw new ContractError("CI lane native evidence", [
      "GITHUB_RUN_ATTEMPT must be a positive safe integer",
    ]);
  }
  if (!source || !shaPattern.test(source.sha) || !shaPattern.test(source.tree)) {
    throw new ContractError("CI lane native evidence", [
      "source SHA and tree must be canonical Git object ids",
    ]);
  }
  let qualificationNonce;
  if (String(correlationNonce ?? "").trim() !== "") {
    qualificationNonce = requirePattern(
      correlationNonce,
      correlationNoncePattern,
      "CI_LANE_CORRELATION_NONCE",
    );
  } else if (postureName === "release") {
    throw new ContractError("CI lane native evidence", [
      "release evidence requires an explicit CI_LANE_CORRELATION_NONCE",
    ]);
  } else {
    qualificationNonce = deriveQualificationCorrelationNonce({
      posture: postureName,
      source,
    });
  }
  const lanes = nativeRoster(registry, workflowPath, postureName);
  if (lanes.length === 0) {
    throw new ContractError("CI lane native evidence", [
      `${workflowPath} owns no merge lanes in the registry`,
    ]);
  }
  const evidenceJobID = requirePattern(
    evidenceJob,
    /^[A-Za-z_][A-Za-z0-9_-]*$/,
    "CI_LANE_EVIDENCE_JOB",
  );
  const expectedJobs = [
    ...new Set(
      lanes.flatMap((lane) =>
        lane.owner.jobs,
      ),
    ),
  ].sort();
  const actualJobs = [...needs.keys()].sort();
  const problems = [];
  for (const job of expectedJobs) {
    if (!needs.has(job)) problems.push(`native needs is missing registered job ${job}`);
  }
  for (const job of actualJobs) {
    if (!expectedJobs.includes(job)) {
      problems.push(`native needs contains unregistered extra job ${job}`);
    }
  }
  if (problems.length > 0) {
    throw new ContractError("CI lane native evidence", problems);
  }

  const laneEvidence = lanes
    .flatMap((lane) =>
      lane.executions.map((executionID) => {
        const checks = lane.owner.jobs
          .filter((jobID) => jobID !== evidenceJobID)
          .map((jobID) => ({
            job_id: jobID,
            result: needs.get(jobID),
          }));
        return {
          lane_id: lane.id,
          execution_id: executionID,
          context: {
            match: lane.context.match,
            name: lane.context.name,
            job_id: lane.owner.context_job,
          },
          checks,
          evidence: evidenceCounts(checks),
          conclusion: laneConclusion(checks),
        };
      }),
    )
    .sort((left, right) =>
      `${left.lane_id}/${left.execution_id}`.localeCompare(
        `${right.lane_id}/${right.execution_id}`,
      ),
    );

  return Object.freeze({
    schema_version: NATIVE_EVIDENCE_SCHEMA_VERSION,
    registry_schema_version: registry.schema_version,
    posture: postureName,
    source: { sha: source.sha, tree: source.tree },
    correlation_nonce: qualificationNonce,
    producer: {
      repository: repositoryName,
      workflow: workflowPath,
      event: eventName,
      run: { id, attempt },
    },
    lanes: laneEvidence,
  });
}

export function validateNativeBundle(document, registry, expected = {}) {
  const problems = [];
  const keys = [
    "schema_version",
    "registry_schema_version",
    "posture",
    "source",
    "correlation_nonce",
    "producer",
    "lanes",
  ];
  const exact = (value, expectedKeys, path) => {
    if (value === null || Array.isArray(value) || typeof value !== "object") {
      problems.push(`${path} must be an object`);
      return false;
    }
    const actual = Object.keys(value).sort();
    const wanted = [...expectedKeys].sort();
    if (JSON.stringify(actual) !== JSON.stringify(wanted)) {
      problems.push(`${path} keys must be exactly ${wanted.join(", ")}`);
      return false;
    }
    return true;
  };
  if (!exact(document, keys, "native bundle")) {
    throw new ContractError("invalid CI lane native bundle", problems);
  }
  if (document.schema_version !== NATIVE_EVIDENCE_SCHEMA_VERSION)
    problems.push("native bundle schema_version is unsupported");
  if (document.registry_schema_version !== registry.schema_version)
    problems.push("native bundle registry schema does not match");
  if (!postures.has(document.posture)) problems.push("native bundle posture is invalid");
  if (!correlationNoncePattern.test(document.correlation_nonce))
    problems.push("native bundle correlation_nonce is invalid");
  if (!exact(document.source, ["sha", "tree"], "native bundle source")) {
    // exact() records the shape problem.
  } else if (
    !shaPattern.test(document.source.sha) ||
    !shaPattern.test(document.source.tree)
  ) {
    problems.push("native bundle source SHA/tree is invalid");
  }
  if (
    exact(
      document.producer,
      ["repository", "workflow", "event", "run"],
      "native bundle producer",
    )
  ) {
    if (!repositoryPattern.test(document.producer.repository))
      problems.push("native bundle producer repository is invalid");
    if (!workflowPattern.test(document.producer.workflow))
      problems.push("native bundle producer workflow is invalid");
    if (!eventPattern.test(document.producer.event))
      problems.push("native bundle producer event is invalid");
    if (exact(document.producer.run, ["id", "attempt"], "native bundle producer run")) {
      if (!runIDPattern.test(document.producer.run.id))
        problems.push("native bundle producer run id is invalid");
      if (!Number.isSafeInteger(document.producer.run.attempt) || document.producer.run.attempt < 1)
        problems.push("native bundle producer run attempt is invalid");
    }
  }

  for (const [field, actual] of [
    ["posture", document.posture],
    ["workflow", document.producer?.workflow],
    ["repository", document.producer?.repository],
    ["event", document.producer?.event],
    ["runID", document.producer?.run?.id],
    ["runAttempt", document.producer?.run?.attempt],
    ["sha", document.source?.sha],
    ["tree", document.source?.tree],
    ["correlationNonce", document.correlation_nonce],
  ]) {
    if (expected[field] !== undefined && actual !== expected[field]) {
      problems.push(`native bundle ${field} does not match trusted expectation`);
    }
  }

  const lanes = nativeRoster(
    registry,
    document.producer?.workflow,
    document.posture,
  );
  const expectedEntries = new Map(
    lanes.flatMap((lane) =>
      lane.executions.map((executionID) => [
        `${lane.id}/${executionID}`,
        lane,
      ]),
    ),
  );
  const actualEntries = new Map();
  if (!Array.isArray(document.lanes)) {
    problems.push("native bundle lanes must be an array");
  } else {
    for (let index = 0; index < document.lanes.length; index += 1) {
      const entry = document.lanes[index];
      const at = `native bundle lanes[${index}]`;
      if (
        !exact(
          entry,
          ["lane_id", "execution_id", "context", "checks", "evidence", "conclusion"],
          at,
        )
      ) continue;
      const identity = `${entry.lane_id}/${entry.execution_id}`;
      const lane = expectedEntries.get(identity);
      if (!lane) problems.push(`${at} is unexpected: ${identity}`);
      if (actualEntries.has(identity)) problems.push(`${at} duplicates ${identity}`);
      actualEntries.set(identity, entry);
      if (lane && JSON.stringify(entry.context) !== JSON.stringify({
        match: lane.context.match,
        name: lane.context.name,
        job_id: lane.owner.context_job,
      })) problems.push(`${at} context does not match the registry`);
      const expectedJobs = lane
        ? lane.owner.jobs.filter((job) => job !== (expected.evidenceJob || "native-evidence"))
        : [];
      const checkJobs = Array.isArray(entry.checks)
        ? entry.checks.map((check) => check?.job_id)
        : [];
      if (JSON.stringify(checkJobs) !== JSON.stringify(expectedJobs))
        problems.push(`${at} checks do not match the registry job roster`);
      if (!Array.isArray(entry.checks)) {
        problems.push(`${at} checks must be an array`);
        continue;
      }
      for (const check of entry.checks) {
        if (!exact(check, ["job_id", "result"], `${at} check`)) continue;
        if (!nativeResults.has(check.result)) problems.push(`${at} check result is invalid`);
      }
      const counts = evidenceCounts(entry.checks);
      if (JSON.stringify(entry.evidence) !== JSON.stringify(counts))
        problems.push(`${at} evidence counts are not derived from checks`);
      if (entry.conclusion !== laneConclusion(entry.checks))
        problems.push(`${at} conclusion is not derived from checks`);
    }
  }
  for (const identity of expectedEntries.keys()) {
    if (!actualEntries.has(identity)) problems.push(`native bundle is missing ${identity}`);
  }
  if (problems.length > 0)
    throw new ContractError("invalid CI lane native bundle", problems);
  return document;
}

export function nativeBundleEntries(document) {
  return document.lanes.map((entry) => ({
    lane_id: entry.lane_id,
    execution_id: entry.execution_id,
    sha256: canonicalJSONSHA256({
      correlation_nonce: document.correlation_nonce,
      entry,
    }),
  }));
}

export function writeNativeBundle(path, document) {
  const output = resolve(path);
  const parent = dirname(output);
  if (!existsSync(parent) || !statSync(parent).isDirectory()) {
    throw new ContractError("CI lane native evidence", [
      `native output directory does not exist: ${parent}`,
    ]);
  }
  if (existsSync(output)) {
    throw new ContractError("CI lane native evidence", [
      `native output already exists; refusing stale reuse: ${output}`,
    ]);
  }
  const body = `${JSON.stringify(document, null, 2)}\n`;
  const temporary = `${output}.${process.pid}.tmp`;
  try {
    writeFileSync(temporary, body, { encoding: "utf8", flag: "wx" });
    linkSync(temporary, output);
  } finally {
    if (existsSync(temporary)) unlinkSync(temporary);
  }
  return Object.freeze({
    path: output,
    body,
    sha256: createHash("sha256").update(body).digest("hex"),
  });
}

function main(env = process.env) {
  const root = resolve(process.cwd());
  const output = String(env.CI_LANE_NATIVE_OUTPUT ?? "").trim();
  if (output === "") {
    throw new ContractError("CI lane native evidence", [
      "CI_LANE_NATIVE_OUTPUT is required",
    ]);
  }
  const registry = loadRegistry(
    env.CI_LANE_REGISTRY || DEFAULT_REGISTRY_PATH,
    { root },
  );
  const source = nativeCheckoutEvidence({
    root,
    expectedSHA: env.GITHUB_SHA,
  });
  const bundle = createNativeBundle({
    registry,
    workflow: env.CI_LANE_WORKFLOW,
    needs: parseNeeds(env.CI_LANE_NEEDS_JSON),
    source,
    repository: env.GITHUB_REPOSITORY,
    event: env.GITHUB_EVENT_NAME,
    runID: env.GITHUB_RUN_ID,
    runAttempt: env.GITHUB_RUN_ATTEMPT,
    posture: env.CI_LANE_NATIVE_POSTURE || "merge",
    evidenceJob: env.CI_LANE_EVIDENCE_JOB || "native-evidence",
    correlationNonce: env.CI_LANE_CORRELATION_NONCE,
  });
  const written = writeNativeBundle(output, bundle);
  setOutput("native_path", written.path);
  setOutput("native_sha256", written.sha256);
  setOutput("native_lane_count", String(bundle.lanes.length));
  setOutput("native_correlation_nonce", bundle.correlation_nonce);
  setOutput("native_artifact_name", nativeArtifactName(bundle.producer.workflow, bundle.producer.run));
  notice(
    `native evidence recorded ${bundle.lanes.length} lane execution(s) from ${bundle.producer.workflow}`,
  );
}

const invokedDirectly =
  process.argv[1] && fileURLToPath(import.meta.url) === resolve(process.argv[1]);
if (invokedDirectly) {
  try {
    main();
  } catch (nativeError) {
    error(nativeError instanceof Error ? nativeError.message : String(nativeError), {
      title: "CI lane native evidence failed closed",
    });
    process.exit(1);
  }
}
