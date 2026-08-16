// ci-lane-shadow-collector.mjs — observe the native Actions checks associated
// with one validated merge-lane selection without presenting those observations
// as native test reports.
//
// The collector deliberately discovers independent workflow runs. A pull
// request workflow run is bound to the event's repository, event kind, pull
// request number, PR head SHA, and PR base SHA. Its Actions REST `head_sha` is
// the PR head, while the selection source may be GitHub's projected merge SHA.
// A merge-group run is bound to the queue event's repository, head SHA, and
// queue ref; the selection binds the event's base SHA and projected head SHA.
//
// Required environment:
//   CI_LANE_SELECTION       validated selection manifest path.
//   CI_LANE_SHADOW_OUTPUT   new observation manifest path; stale reuse fails.
//   GITHUB_EVENT_PATH       trusted runner event payload path.
//   GITHUB_EVENT_NAME       pull_request or merge_group.
//   GITHUB_REPOSITORY       trusted runner repository (`owner/name`).
//   GITHUB_SHA              projected checkout SHA selected by the fence.
//   GITHUB_RUN_ID           selector/collector workflow run ID.
//   GITHUB_RUN_ATTEMPT      selector/collector workflow attempt.
//   GITHUB_TOKEN            token with Actions read access.
//
// Optional environment:
//   CI_LANE_REGISTRY                 registry path (default .github/ci-lanes.json).
//   CI_LANE_SHADOW_POLL_SECONDS      poll interval (default 15).
//   CI_LANE_SHADOW_TIMEOUT_SECONDS   observation deadline (default 1200).
//   GITHUB_API_URL                   API origin (default https://api.github.com).
//   GITHUB_OUTPUT                    Actions step-output destination.
//
// The main program polls because independently triggered workflows need not
// start or finish in the selector workflow's scheduling order. Tests inject an
// HTTP function and never access the network. Node builtins only.

import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import {
  existsSync,
  linkSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  statSync,
  unlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";

import {
  ContractError,
  DEFAULT_REGISTRY_PATH,
  loadRegistry,
  nativeArtifactName,
  selectionManifestSHA256,
  validateReportSet,
  validateSelection,
} from "./ci-lane-contract.mjs";
import {
  nativeBundleEntries,
  validateNativeBundle,
} from "./ci-lane-native-evidence.mjs";
import { error, notice, setOutput, warning } from "./lib/gh.mjs";

export const SHADOW_OBSERVATION_SCHEMA_VERSION = 1;

const DEFAULT_API_URL = "https://api.github.com";
const DEFAULT_POLL_SECONDS = 15;
const DEFAULT_TIMEOUT_SECONDS = 20 * 60;
const MAX_PER_PAGE = 100;
const MAX_NATIVE_ARCHIVE_BYTES = 2 * 1024 * 1024;
const MAX_NATIVE_ENTRY_BYTES = 1024 * 1024;
const SHA_PATTERN = /^(?:[0-9a-f]{40}|[0-9a-f]{64})$/;
const REPOSITORY_PATTERN = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;
const POSITIVE_INTEGER_PATTERN = /^[1-9][0-9]*$/;
const API_TIMESTAMP_PATTERN =
  /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,3})?Z$/;
const FAILURE_CONCLUSIONS = new Set([
  "action_required",
  "failure",
  "stale",
  "startup_failure",
]);

function contract(label, problem) {
  throw new ContractError(label, [problem]);
}

function isObject(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function requiredString(value, path, { pattern } = {}) {
  if (typeof value !== "string" || value.trim() === "") {
    contract("CI lane shadow collector", `${path} must be a non-empty string`);
  }
  if (pattern && !pattern.test(value)) {
    contract(
      "CI lane shadow collector",
      `${path} has invalid value ${JSON.stringify(value)}`,
    );
  }
  return value;
}

function positiveInteger(value, path) {
  const text =
    typeof value === "number" && Number.isSafeInteger(value)
      ? String(value)
      : typeof value === "string"
        ? value
        : "";
  if (!POSITIVE_INTEGER_PATTERN.test(text)) {
    contract(
      "CI lane shadow collector",
      `${path} must be a positive safe integer`,
    );
  }
  const number = Number(text);
  if (!Number.isSafeInteger(number)) {
    contract(
      "CI lane shadow collector",
      `${path} exceeds JavaScript's safe integer range`,
    );
  }
  return number;
}

function nonnegativeInteger(value, path) {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value < 0) {
    contract(
      "CI lane shadow collector",
      `${path} must be a non-negative safe integer`,
    );
  }
  return value;
}

function positiveID(value, path) {
  const number = positiveInteger(value, path);
  return String(number);
}

function nullableConclusion(value, path) {
  if (value === null) return null;
  return requiredString(value, path);
}

function apiTimestamp(value, path, { nullable = false } = {}) {
  if (value === null && nullable) return null;
  const timestamp = requiredString(value, path, {
    pattern: API_TIMESTAMP_PATTERN,
  });
  if (!Number.isFinite(Date.parse(timestamp))) {
    contract(
      "CI lane shadow collector",
      `${path} is not a real UTC timestamp`,
    );
  }
  return timestamp;
}

function timestampMillis(value, path) {
  const timestamp = apiTimestamp(value, path);
  return Date.parse(timestamp);
}

function normalizedRef(value) {
  const ref = requiredString(value, "event ref");
  return ref.startsWith("refs/heads/") ? ref.slice("refs/heads/".length) : ref;
}

function parseJSONFile(path, label) {
  try {
    return JSON.parse(readFileSync(path, "utf8"));
  } catch (readError) {
    contract(label, `${path}: ${readError.message}`);
  }
}

export function trustedEventIdentity({
  eventName,
  repository,
  projectedSHA,
  payload,
}) {
  const kind = requiredString(eventName, "GITHUB_EVENT_NAME");
  if (kind !== "pull_request" && kind !== "merge_group") {
    contract(
      "CI lane shadow collector",
      `GITHUB_EVENT_NAME must be pull_request or merge_group; got ${JSON.stringify(kind)}`,
    );
  }
  const trustedRepository = requiredString(
    repository,
    "GITHUB_REPOSITORY",
    { pattern: REPOSITORY_PATTERN },
  );
  const projected = requiredString(projectedSHA, "GITHUB_SHA", {
    pattern: SHA_PATTERN,
  });
  if (!isObject(payload)) {
    contract("CI lane shadow collector", "GITHUB_EVENT_PATH must contain an object");
  }
  const payloadRepository = payload.repository?.full_name;
  if (payloadRepository !== trustedRepository) {
    contract(
      "CI lane shadow collector",
      `event repository is ${JSON.stringify(payloadRepository)}, want ${trustedRepository}`,
    );
  }

  if (kind === "pull_request") {
    const pullRequest = payload.pull_request;
    if (!isObject(pullRequest)) {
      contract(
        "CI lane shadow collector",
        "pull_request event has no pull_request object",
      );
    }
    const number = positiveInteger(pullRequest.number, "event pull request number");
    const eventHeadSHA = requiredString(
      pullRequest.head?.sha,
      "event pull request head SHA",
      { pattern: SHA_PATTERN },
    );
    const eventBaseSHA = requiredString(
      pullRequest.base?.sha,
      "event pull request base SHA",
      { pattern: SHA_PATTERN },
    );
    const headRef = normalizedRef(pullRequest.head?.ref);
    const baseRef = normalizedRef(pullRequest.base?.ref);
    if (pullRequest.base?.repo?.full_name !== trustedRepository) {
      contract(
        "CI lane shadow collector",
        "pull request base repository does not match GITHUB_REPOSITORY",
      );
    }
    return Object.freeze({
      kind,
      repository: trustedRepository,
      pr_number: number,
      event_head_sha: eventHeadSHA,
      event_base_sha: eventBaseSHA,
      head_ref: headRef,
      base_ref: baseRef,
      projected_sha: projected,
    });
  }

  const mergeGroup = payload.merge_group;
  if (!isObject(mergeGroup)) {
    contract(
      "CI lane shadow collector",
      "merge_group event has no merge_group object",
    );
  }
  const eventHeadSHA = requiredString(
    mergeGroup.head_sha,
    "event merge group head SHA",
    { pattern: SHA_PATTERN },
  );
  const eventBaseSHA = requiredString(
    mergeGroup.base_sha,
    "event merge group base SHA",
    { pattern: SHA_PATTERN },
  );
  if (projected !== eventHeadSHA) {
    contract(
      "CI lane shadow collector",
      `merge-group projected SHA is ${projected}, want event head ${eventHeadSHA}`,
    );
  }
  return Object.freeze({
    kind,
    repository: trustedRepository,
    pr_number: null,
    event_head_sha: eventHeadSHA,
    event_base_sha: eventBaseSHA,
    head_ref: normalizedRef(mergeGroup.head_ref),
    base_ref: normalizedRef(mergeGroup.base_ref),
    projected_sha: projected,
  });
}

export function bindSelectionToEvent(
  selection,
  registry,
  identity,
  { runID, runAttempt } = {},
) {
  const expected = {
    posture: "merge",
    sha: identity.projected_sha,
    baseSHA: identity.event_base_sha,
  };
  if (runID !== undefined) expected.runID = positiveID(runID, "GITHUB_RUN_ID");
  if (runAttempt !== undefined) {
    expected.runAttempt = positiveInteger(
      runAttempt,
      "GITHUB_RUN_ATTEMPT",
    );
  }
  validateSelection(selection, registry, expected);
  if (
    identity.kind === "merge_group" &&
    selection.source.sha !== identity.event_head_sha
  ) {
    contract(
      "CI lane shadow collector",
      "merge-group selection source does not equal the event head SHA",
    );
  }
  return selection;
}

function repositoryAPIPath(repository) {
  return repository
    .split("/")
    .map((component) => encodeURIComponent(component))
    .join("/");
}

function pagedURL(baseURL, pathname, query, page) {
  const url = new URL(pathname, `${baseURL.replace(/\/$/, "")}/`);
  for (const [name, value] of Object.entries(query)) {
    url.searchParams.set(name, String(value));
  }
  url.searchParams.set("per_page", String(MAX_PER_PAGE));
  url.searchParams.set("page", String(page));
  return url.toString();
}

async function paginated({ requestJSON, buildURL, field, label }) {
  const rows = [];
  for (let page = 1; ; page += 1) {
    const document = await requestJSON(buildURL(page));
    if (!isObject(document) || !Array.isArray(document[field])) {
      contract(
        "CI lane shadow collector",
        `${label} response must contain an ${field} array`,
      );
    }
    nonnegativeInteger(document.total_count, `${label}.total_count`);
    rows.push(...document[field]);
    if (document[field].length < MAX_PER_PAGE) break;
  }
  return rows;
}

function pullRequestAssociationMatches(association, identity) {
  if (!isObject(association)) return false;
  if (
    !Number.isSafeInteger(association.number) ||
    association.number !== identity.pr_number
  ) {
    return false;
  }
  return (
    association.head?.sha === identity.event_head_sha &&
    association.base?.sha === identity.event_base_sha &&
    normalizedCandidateRef(association.head?.ref) === identity.head_ref &&
    normalizedCandidateRef(association.base?.ref) === identity.base_ref
  );
}

function normalizedCandidateRef(value) {
  if (typeof value !== "string" || value.trim() === "") return null;
  return value.startsWith("refs/heads/")
    ? value.slice("refs/heads/".length)
    : value;
}

function runMatchesIdentity(run, identity, workflows) {
  if (!isObject(run)) return false;
  if (!workflows.has(run.path)) return false;
  if (run.repository?.full_name !== identity.repository) return false;
  if (run.event !== identity.kind) return false;
  if (run.head_sha !== identity.event_head_sha) return false;
  if (normalizedCandidateRef(run.head_branch) !== identity.head_ref) return false;
  if (identity.kind === "pull_request") {
    return (
      Array.isArray(run.pull_requests) &&
      run.pull_requests.some((association) =>
        pullRequestAssociationMatches(association, identity),
      )
    );
  }
  return true;
}

function normalizeRun(run) {
  return {
    workflow: requiredString(run.path, "workflow run path"),
    run_id: positiveID(run.id, "workflow run id"),
    run_attempt: positiveInteger(run.run_attempt, "workflow run attempt"),
    event: requiredString(run.event, "workflow run event"),
    head_sha: requiredString(run.head_sha, "workflow run head SHA", {
      pattern: SHA_PATTERN,
    }),
    head_branch: requiredString(run.head_branch, "workflow run head branch"),
    status: requiredString(run.status, "workflow run status"),
    conclusion: nullableConclusion(
      run.conclusion,
      "workflow run conclusion",
    ),
    created_at: apiTimestamp(run.created_at, "workflow run created_at"),
    updated_at: apiTimestamp(run.updated_at, "workflow run updated_at"),
  };
}

function compareIDs(left, right) {
  const a = BigInt(left);
  const b = BigInt(right);
  return a < b ? -1 : a > b ? 1 : 0;
}

export function newestRunFirst(left, right) {
  const created =
    timestampMillis(right.created_at, "workflow run created_at") -
    timestampMillis(left.created_at, "workflow run created_at");
  if (created !== 0) return created;
  const id = compareIDs(right.run_id, left.run_id);
  if (id !== 0) return id;
  return right.run_attempt - left.run_attempt;
}

function canonicalRunOrder(left, right) {
  const workflow = left.workflow.localeCompare(right.workflow);
  return workflow === 0 ? newestRunFirst(left, right) : workflow;
}

export async function discoverWorkflowRuns({
  registry,
  identity,
  requestJSON,
  apiBase = DEFAULT_API_URL,
}) {
  if (typeof requestJSON !== "function") {
    contract("CI lane shadow collector", "requestJSON must be a function");
  }
  const workflows = new Set(registry.lanes.map((lane) => lane.owner.workflow));
  const repository = repositoryAPIPath(identity.repository);
  const candidates = await paginated({
    requestJSON,
    field: "workflow_runs",
    label: "workflow runs",
    buildURL: (page) =>
      pagedURL(
        apiBase,
        `/repos/${repository}/actions/runs`,
        { event: identity.kind, head_sha: identity.event_head_sha },
        page,
      ),
  });

  const unique = new Map();
  for (const candidate of candidates) {
    if (!runMatchesIdentity(candidate, identity, workflows)) continue;
    const run = normalizeRun(candidate);
    const key = `${run.run_id}:${run.run_attempt}`;
    const previous = unique.get(key);
    if (previous && JSON.stringify(previous) !== JSON.stringify(run)) {
      contract(
        "CI lane shadow collector",
        `workflow run ${key} appeared with conflicting evidence`,
      );
    }
    unique.set(key, run);
  }
  return [...unique.values()].sort(canonicalRunOrder);
}

function normalizeJob(job) {
  const status = requiredString(job.status, "workflow job status");
  const conclusion = nullableConclusion(
    job.conclusion,
    "workflow job conclusion",
  );
  const startedAt = apiTimestamp(job.started_at, "workflow job started_at", {
    nullable: status !== "completed",
  });
  const completedAt = apiTimestamp(
    job.completed_at,
    "workflow job completed_at",
    { nullable: status !== "completed" },
  );
  let normalizedStartedAt = startedAt;
  let duration = null;
  if (startedAt !== null && completedAt !== null) {
    duration = Date.parse(completedAt) - Date.parse(startedAt);
    if (!Number.isSafeInteger(duration) || duration < 0) {
      // Actions assigns synthetic timestamps to jobs skipped before runner
      // allocation and can report their start after their completion. A
      // skipped job executed no runner time, so canonicalize that documented
      // API shape to a zero-duration instant. Every executed conclusion keeps
      // the strict chronological check.
      if (status === "completed" && conclusion === "skipped") {
        normalizedStartedAt = completedAt;
        duration = 0;
      } else {
        contract(
          "CI lane shadow collector",
          "workflow job completion precedes its start",
        );
      }
    }
  }
  return {
    id: positiveID(job.id, "workflow job id"),
    name: requiredString(job.name, "workflow job name"),
    status,
    conclusion,
    started_at: normalizedStartedAt,
    completed_at: completedAt,
    duration_ms: duration,
  };
}

function canonicalJobOrder(left, right) {
  const id = compareIDs(left.id, right.id);
  return id === 0 ? left.name.localeCompare(right.name) : id;
}

async function jobsForRun({ run, identity, requestJSON, apiBase }) {
  const repository = repositoryAPIPath(identity.repository);
  const jobs = await paginated({
    requestJSON,
    field: "jobs",
    label: `jobs for run ${run.run_id} attempt ${run.run_attempt}`,
    buildURL: (page) =>
      pagedURL(
        apiBase,
        `/repos/${repository}/actions/runs/${run.run_id}/attempts/${run.run_attempt}/jobs`,
        { filter: "all" },
        page,
      ),
  });
  const unique = new Map();
  for (const candidate of jobs) {
    const job = normalizeJob(candidate);
    if (unique.has(job.id)) {
      contract(
        "CI lane shadow collector",
        `workflow run ${run.run_id} attempt ${run.run_attempt} returned duplicate job id ${job.id}`,
      );
    }
    unique.set(job.id, job);
  }
  return [...unique.values()].sort(canonicalJobOrder);
}

export async function hydrateWorkflowRuns({
  runs,
  identity,
  requestJSON,
  apiBase = DEFAULT_API_URL,
}) {
  const hydrated = await Promise.all(
    runs.map(async (run) => ({
      ...run,
      jobs: await jobsForRun({
        run,
        identity,
        requestJSON,
        apiBase,
      }),
    })),
  );
  return hydrated.sort(canonicalRunOrder);
}

function normalizeArtifact(artifact) {
  const id = positiveID(artifact?.id, "workflow artifact id");
  const name = requiredString(artifact?.name, "workflow artifact name");
  const digest = requiredString(artifact?.digest, "workflow artifact digest", {
    pattern: /^sha256:[0-9a-f]{64}$/,
  });
  if (artifact?.expired !== false) {
    contract("CI lane shadow collector", `workflow artifact ${name} is expired`);
  }
  return {
    id,
    name,
    sha256: digest.slice("sha256:".length),
    archive_download_url: requiredString(
      artifact?.archive_download_url,
      "workflow artifact archive URL",
    ),
  };
}

export function unpackNativeArchive(archive) {
  if (!Buffer.isBuffer(archive) || archive.length === 0) {
    contract("CI lane shadow collector", "native artifact archive is empty");
  }
  if (archive.length > MAX_NATIVE_ARCHIVE_BYTES) {
    contract("CI lane shadow collector", "native artifact archive exceeds its size limit");
  }
  const directory = mkdtempSync(join(tmpdir(), "ci-native-artifact-"));
  const archivePath = join(directory, "bundle.zip");
  try {
    writeFileSync(archivePath, archive, { flag: "wx" });
    const listing = spawnSync("unzip", ["-Z1", archivePath], {
      encoding: "utf8",
      maxBuffer: MAX_NATIVE_ENTRY_BYTES,
    });
    if (listing.status !== 0) {
      contract("CI lane shadow collector", "native artifact is not a readable ZIP archive");
    }
    const entries = listing.stdout.split(/\r?\n/).filter(Boolean);
    if (
      entries.length !== 1 ||
      entries[0] !== "ci-native-evidence.json" ||
      entries[0].includes("..") ||
      entries[0].startsWith("/")
    ) {
      contract(
        "CI lane shadow collector",
        "native artifact must contain only ci-native-evidence.json",
      );
    }
    const extracted = spawnSync("unzip", ["-p", archivePath, entries[0]], {
      encoding: "utf8",
      maxBuffer: MAX_NATIVE_ENTRY_BYTES,
    });
    if (extracted.status !== 0 || extracted.error) {
      contract("CI lane shadow collector", "native evidence entry could not be read safely");
    }
    try {
      return JSON.parse(extracted.stdout);
    } catch (parseError) {
      contract(
        "CI lane shadow collector",
        `native evidence entry is not JSON: ${parseError.message}`,
      );
    }
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
}

export async function collectNativeBundles({
  registry,
  selection,
  identity,
  workflowRuns,
  requestJSON,
  requestBinary,
  apiBase = DEFAULT_API_URL,
}) {
  const selectedWorkflows = new Set(
    selection.lanes
      .filter((item) => item.disposition === "selected")
      .map(
        (item) =>
          registry.lanes.find((lane) => lane.id === item.lane_id).owner.workflow,
      ),
  );
  const newest = new Map();
  for (const run of [...workflowRuns].sort(canonicalRunOrder)) {
    if (selectedWorkflows.has(run.workflow) && !newest.has(run.workflow))
      newest.set(run.workflow, run);
  }
  const repository = repositoryAPIPath(identity.repository);
  const bundles = new Map();
  for (const workflow of [...selectedWorkflows].sort()) {
    const run = newest.get(workflow);
    if (!run) continue;
    const expectedName = nativeArtifactName(workflow, {
      id: run.run_id,
      attempt: run.run_attempt,
    });
    const artifacts = await paginated({
      requestJSON,
      field: "artifacts",
      label: `artifacts for run ${run.run_id}`,
      buildURL: (page) =>
        pagedURL(
          apiBase,
          `/repos/${repository}/actions/runs/${run.run_id}/artifacts`,
          {},
          page,
        ),
    });
    const matches = artifacts
      .map(normalizeArtifact)
      .filter((artifact) => artifact.name === expectedName);
    if (matches.length !== 1) continue;
    const [artifact] = matches;
    const archive = await requestBinary(artifact.archive_download_url);
    const archiveSHA = createHash("sha256").update(archive).digest("hex");
    if (archiveSHA !== artifact.sha256) {
      contract(
        "CI lane shadow collector",
        `${workflow}: downloaded native artifact digest does not match the Actions API`,
      );
    }
    const bundle = validateNativeBundle(unpackNativeArchive(archive), registry, {
      posture: "merge",
      workflow,
      repository: identity.repository,
      event: identity.kind,
      runID: run.run_id,
      runAttempt: run.run_attempt,
      sha: selection.source.sha,
      tree: selection.source.tree,
      correlationNonce: selection.correlation_nonce,
    });
    bundles.set(workflow, { run, artifact, bundle });
  }
  return bundles;
}

function contextMatches(context, jobName) {
  return context.match === "exact"
    ? jobName === context.name
    : jobName.startsWith(context.name);
}

export function jobObservationState(job) {
  if (job.status !== "completed") {
    return { state: "pending", reason: "job_not_terminal" };
  }
  if (job.conclusion === "success") return { state: "success", reason: null };
  if (job.conclusion === "skipped") {
    return { state: "skipped", reason: "job_skipped" };
  }
  if (job.conclusion === "neutral") {
    return { state: "neutral", reason: "job_neutral" };
  }
  if (job.conclusion === "cancelled") {
    return { state: "cancelled", reason: "job_cancelled" };
  }
  if (job.conclusion === "timed_out") {
    return { state: "timed_out", reason: "job_timed_out" };
  }
  if (FAILURE_CONCLUSIONS.has(job.conclusion)) {
    return { state: "red", reason: `job_${job.conclusion}` };
  }
  return { state: "red", reason: "unsupported_terminal_conclusion" };
}

export function createNativeQualification({
  registry,
  selection,
  identity,
  bundles,
}) {
  const selectionRef = {
    run: { ...selection.run },
    manifest_sha256: selectionManifestSHA256(selection),
  };
  const lanes = new Map(registry.lanes.map((lane) => [lane.id, lane]));
  const producers = [];
  const reports = [];
  for (const [workflow, native] of [...bundles.entries()].sort(([left], [right]) =>
    left.localeCompare(right),
  )) {
    const jobs = [];
    const seenJobs = new Set();
    for (const entry of native.bundle.lanes) {
      const lane = lanes.get(entry.lane_id);
      const matches = native.run.jobs.filter((job) =>
        contextMatches(lane.context, job.name),
      );
      if (matches.length !== 1) continue;
      const job = matches[0];
      const key = `${lane.owner.context_job}/${job.id}`;
      if (seenJobs.has(key)) continue;
      seenJobs.add(key);
      jobs.push({
        job: lane.owner.context_job,
        name: job.name,
        database_id: job.id,
        conclusion: job.conclusion,
      });
    }
    const entries = nativeBundleEntries(native.bundle);
    producers.push({
      workflow,
      run: { id: native.run.run_id, attempt: native.run.run_attempt },
      source: { ...native.bundle.source },
      correlation_nonce: native.bundle.correlation_nonce,
      event: {
        kind: identity.kind,
        pr_number: identity.pr_number,
        event_head_sha: identity.event_head_sha,
        event_base_sha: identity.event_base_sha,
        projected_sha: identity.projected_sha,
      },
      repository: identity.repository,
      conclusion: native.run.conclusion,
      jobs,
      artifacts: [
        {
          id: native.artifact.id,
          name: native.artifact.name,
          sha256: native.artifact.sha256,
          entries,
        },
      ],
    });

    for (const selected of selection.lanes.filter(
      (item) =>
        item.disposition === "selected" &&
        lanes.get(item.lane_id).owner.workflow === workflow,
    )) {
      const lane = lanes.get(selected.lane_id);
      for (const executionID of selected.executions) {
        const entry = native.bundle.lanes.find(
          (candidate) =>
            candidate.lane_id === lane.id &&
            candidate.execution_id === executionID,
        );
        if (!entry) continue;
        const entryDigest = entries.find(
          (candidate) =>
            candidate.lane_id === lane.id &&
            candidate.execution_id === executionID,
        ).sha256;
        const context = native.run.jobs.filter((job) =>
          contextMatches(lane.context, job.name),
        );
        const duration = context.length === 1 ? context[0].duration_ms ?? 0 : 0;
        reports.push({
          schema_version: registry.report_schema_version,
          registry_schema_version: registry.schema_version,
          lane_id: lane.id,
          execution_id: executionID,
          posture: selection.posture,
          source: { ...selection.source },
          candidate_digest: null,
          correlation_nonce: selection.correlation_nonce,
          selection_ref: selectionRef,
          producer: {
            workflow,
            job: lane.owner.context_job,
            run: { id: native.run.run_id, attempt: native.run.run_attempt },
            artifact: {
              id: native.artifact.id,
              name: native.artifact.name,
              sha256: native.artifact.sha256,
              entry: `${lane.id}/${executionID}`,
              entry_sha256: entryDigest,
            },
          },
          invocation: {
            mode: "source_tree",
            recipe: lane.recipes[0] ?? null,
            command: lane.command,
            build_tags: [...lane.build_tags],
            selected_domains: [...lane.risk_domains],
          },
          evidence: {
            ...entry.evidence,
            duration_ms: duration,
            seed: lane.determinism === "seeded" ? selection.source.tree : null,
            corpus_id: `${lane.id}/${executionID}`,
          },
          conclusion: entry.conclusion,
        });
      }
    }
  }
  const producerManifest = {
    schema_version: registry.report_schema_version,
    correlation_nonce: selection.correlation_nonce,
    selection_ref: selectionRef,
    producers,
  };
  const expected = {
    posture: selection.posture,
    sha: selection.source.sha,
    tree: selection.source.tree,
    correlationNonce: selection.correlation_nonce,
    candidateDigest: undefined,
    runID: selection.run.id,
    runAttempt: selection.run.attempt,
    selectionManifestSHA256: selectionRef.manifest_sha256,
    eventIdentity: {
      kind: identity.kind,
      pr_number: identity.pr_number,
      event_head_sha: identity.event_head_sha,
      event_base_sha: identity.event_base_sha,
      projected_sha: identity.projected_sha,
    },
    repository: identity.repository,
    baseSHA: selection.selector.base_sha,
    changedPaths: [...selection.selector.changed_paths],
  };
  const result = validateReportSet({
    registry,
    selection,
    reports,
    producerManifest,
    expected,
  });
  return { producerManifest, reports, result };
}

function laneObservation({ lane, laneSelection, run }) {
  const sources = [];
  if (lane.context.protected) sources.push("protected");
  if (laneSelection.disposition === "selected") sources.push("selected");
  sources.sort();

  const common = {
    lane_id: lane.id,
    workflow: lane.owner.workflow,
    context: {
      match: lane.context.match,
      name: lane.context.name,
      protected: lane.context.protected,
    },
    selection_disposition: laneSelection.disposition,
    selection_reason: laneSelection.reason,
    observation_sources: sources,
  };
  if (!run) {
    return {
      ...common,
      run: null,
      job_ids: [],
      job_name: null,
      status: null,
      conclusion: null,
      duration_ms: null,
      state: "missing",
      reason: "workflow_run_missing",
      terminal_success: false,
    };
  }
  const matched = run.jobs.filter((job) =>
    contextMatches(lane.context, job.name),
  );
  const runRef = { id: run.run_id, attempt: run.run_attempt };
  if (matched.length === 0) {
    return {
      ...common,
      run: runRef,
      job_ids: [],
      job_name: null,
      status: null,
      conclusion: null,
      duration_ms: null,
      state: "missing",
      reason: "context_missing",
      terminal_success: false,
    };
  }
  if (matched.length > 1) {
    return {
      ...common,
      run: runRef,
      job_ids: matched.map((job) => job.id).sort(compareIDs),
      job_name: null,
      status: null,
      conclusion: null,
      duration_ms: null,
      state: "duplicate",
      reason: "context_duplicate",
      terminal_success: false,
    };
  }
  const [job] = matched;
  const outcome = jobObservationState(job);
  return {
    ...common,
    run: runRef,
    job_ids: [job.id],
    job_name: job.name,
    status: job.status,
    conclusion: job.conclusion,
    duration_ms: job.duration_ms,
    state: outcome.state,
    reason: outcome.reason,
    terminal_success: outcome.state === "success",
  };
}

export function createShadowObservationManifest({
  registry,
  selection,
  identity,
  workflowRuns,
  collectedAt,
}) {
  const timestamp = apiTimestamp(collectedAt, "collected_at");
  const selectionByID = new Map(
    selection.lanes.map((laneSelection) => [
      laneSelection.lane_id,
      laneSelection,
    ]),
  );
  const newestByWorkflow = new Map();
  for (const run of [...workflowRuns].sort(canonicalRunOrder)) {
    if (!newestByWorkflow.has(run.workflow)) {
      newestByWorkflow.set(run.workflow, run);
    }
  }
  const laneObservations = [...registry.lanes]
    .sort((left, right) => left.id.localeCompare(right.id))
    .flatMap((lane) => {
      const laneSelection = selectionByID.get(lane.id);
      if (
        laneSelection.disposition !== "selected" &&
        !lane.context.protected
      ) {
        return [];
      }
      return [
        laneObservation({
          lane,
          laneSelection,
          run: newestByWorkflow.get(lane.owner.workflow),
        }),
      ];
    });

  return {
    schema_version: SHADOW_OBSERVATION_SCHEMA_VERSION,
    registry_schema_version: registry.schema_version,
    selection_ref: {
      run: { ...selection.run },
      manifest_sha256: selectionManifestSHA256(selection),
    },
    selection_source: { ...selection.source },
    correlation_nonce: selection.correlation_nonce,
    event: {
      kind: identity.kind,
      pr_number: identity.pr_number,
      event_head_sha: identity.event_head_sha,
      event_base_sha: identity.event_base_sha,
      projected_sha: identity.projected_sha,
    },
    repository: identity.repository,
    event_refs: { head: identity.head_ref, base: identity.base_ref },
    collected_at: timestamp,
    workflow_runs: [...workflowRuns].sort(canonicalRunOrder),
    lane_observations: laneObservations,
  };
}

export async function collectShadowObservation({
  registry,
  selection,
  identity,
  requestJSON,
  apiBase = DEFAULT_API_URL,
  collectedAt = new Date().toISOString(),
}) {
  bindSelectionToEvent(selection, registry, identity);
  const runs = await discoverWorkflowRuns({
    registry,
    identity,
    requestJSON,
    apiBase,
  });
  const workflowRuns = await hydrateWorkflowRuns({
    runs,
    identity,
    requestJSON,
    apiBase,
  });
  return createShadowObservationManifest({
    registry,
    selection,
    identity,
    workflowRuns,
    collectedAt,
  });
}

export function shadowObservationSettled(document) {
  return (
    document.workflow_runs.every((run) => run.status === "completed") &&
    document.lane_observations.every(
      (observation) =>
        observation.state !== "missing" && observation.state !== "pending",
    )
  );
}

export function assertFreshOutput(path) {
  const output = resolve(path);
  const parent = dirname(output);
  if (!existsSync(parent) || !statSync(parent).isDirectory()) {
    contract(
      "CI lane shadow collector",
      `observation output directory does not exist: ${parent}`,
    );
  }
  if (existsSync(output)) {
    contract(
      "CI lane shadow collector",
      `observation output already exists; refusing stale reuse: ${output}`,
    );
  }
  return output;
}

export function writeShadowObservation(path, document) {
  const output = assertFreshOutput(path);
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

function nonnegativeSeconds(value, name, fallback) {
  if (value === undefined || String(value).trim() === "") return fallback;
  const text = String(value);
  if (!/^(?:0|[1-9][0-9]*)$/.test(text)) {
    contract("CI lane shadow collector", `${name} must be a non-negative integer`);
  }
  const seconds = Number(text);
  if (!Number.isSafeInteger(seconds)) {
    contract("CI lane shadow collector", `${name} exceeds the safe integer range`);
  }
  return seconds;
}

function delay(milliseconds) {
  return new Promise((resolveDelay) => setTimeout(resolveDelay, milliseconds));
}

function githubRequest({ token }) {
  const authorization = requiredString(token, "GITHUB_TOKEN");
  return async (url) => {
    const response = await fetch(url, {
      headers: {
        Accept: "application/vnd.github+json",
        Authorization: `Bearer ${authorization}`,
        "X-GitHub-Api-Version": "2022-11-28",
      },
    });
    if (!response.ok) {
      contract(
        "CI lane shadow collector",
        `GitHub API ${response.status} for ${new URL(url).pathname}`,
      );
    }
    return response.json();
  };
}

function githubBinaryRequest({ token }) {
  const authorization = requiredString(token, "GITHUB_TOKEN");
  return async (url) => {
    const response = await fetch(url, {
      headers: {
        Accept: "application/vnd.github+json",
        Authorization: `Bearer ${authorization}`,
        "X-GitHub-Api-Version": "2022-11-28",
      },
      redirect: "follow",
    });
    if (!response.ok || !response.body) {
      contract(
        "CI lane shadow collector",
        `GitHub artifact download failed with HTTP ${response.status}`,
      );
    }
    const chunks = [];
    let size = 0;
    for await (const chunk of response.body) {
      size += chunk.length;
      if (size > MAX_NATIVE_ARCHIVE_BYTES) {
        contract(
          "CI lane shadow collector",
          "GitHub artifact download exceeds its size limit",
        );
      }
      chunks.push(Buffer.from(chunk));
    }
    return Buffer.concat(chunks);
  };
}

export async function main(env = process.env) {
  const root = resolve(process.cwd());
  const selectionPath = requiredString(
    env.CI_LANE_SELECTION,
    "CI_LANE_SELECTION",
  );
  const outputPath = assertFreshOutput(
    requiredString(env.CI_LANE_SHADOW_OUTPUT, "CI_LANE_SHADOW_OUTPUT"),
  );
  const registry = loadRegistry(
    env.CI_LANE_REGISTRY || DEFAULT_REGISTRY_PATH,
    { root },
  );
  const selection = parseJSONFile(
    resolve(root, selectionPath),
    "CI lane shadow collector",
  );
  const payload = parseJSONFile(
    requiredString(env.GITHUB_EVENT_PATH, "GITHUB_EVENT_PATH"),
    "CI lane shadow collector",
  );
  const identity = trustedEventIdentity({
    eventName: env.GITHUB_EVENT_NAME,
    repository: env.GITHUB_REPOSITORY,
    projectedSHA: env.GITHUB_SHA,
    payload,
  });
  bindSelectionToEvent(selection, registry, identity, {
    runID: env.GITHUB_RUN_ID,
    runAttempt: env.GITHUB_RUN_ATTEMPT,
  });

  const pollSeconds = nonnegativeSeconds(
    env.CI_LANE_SHADOW_POLL_SECONDS,
    "CI_LANE_SHADOW_POLL_SECONDS",
    DEFAULT_POLL_SECONDS,
  );
  const timeoutSeconds = nonnegativeSeconds(
    env.CI_LANE_SHADOW_TIMEOUT_SECONDS,
    "CI_LANE_SHADOW_TIMEOUT_SECONDS",
    DEFAULT_TIMEOUT_SECONDS,
  );
  const apiBase = env.GITHUB_API_URL || DEFAULT_API_URL;
  const requestJSON = githubRequest({ token: env.GITHUB_TOKEN });
  const requestBinary = githubBinaryRequest({ token: env.GITHUB_TOKEN });
  const deadline = Date.now() + timeoutSeconds * 1000;
  let observation;
  for (;;) {
    observation = await collectShadowObservation({
      registry,
      selection,
      identity,
      requestJSON,
      apiBase,
    });
    if (shadowObservationSettled(observation) || Date.now() >= deadline) break;
    const remaining = deadline - Date.now();
    await delay(Math.min(pollSeconds * 1000, remaining));
  }

  const settled = shadowObservationSettled(observation);
  if (settled) {
    const bundles = await collectNativeBundles({
      registry,
      selection,
      identity,
      workflowRuns: observation.workflow_runs,
      requestJSON,
      requestBinary,
      apiBase,
    });
    const qualification = createNativeQualification({
      registry,
      selection,
      identity,
      bundles,
    });
    observation = {
      ...observation,
      native_qualification: {
        producer_manifest: qualification.producerManifest,
        reports: qualification.reports,
        expected: qualification.result.expected,
        received: qualification.result.received,
      },
    };
  }
  const written = writeShadowObservation(outputPath, observation);
  setOutput("shadow_observation_path", written.path);
  setOutput("shadow_observation_sha256", written.sha256);
  setOutput("shadow_observation_settled", String(settled));
  if (!settled) {
    warning(
      "CI lane shadow collection reached its deadline with missing or pending native evidence",
      { title: "merge-fence shadow observation incomplete" },
    );
  }
  notice(
    `Recorded native Actions evidence for ${observation.workflow_runs.length} workflow run(s)`,
  );
  return observation;
}

const invokedDirectly =
  process.argv[1] && fileURLToPath(import.meta.url) === resolve(process.argv[1]);
if (invokedDirectly) {
  try {
    await main();
  } catch (collectorError) {
    error(
      collectorError instanceof Error
        ? collectorError.message
        : String(collectorError),
      { title: "merge-fence shadow collector failed closed" },
    );
    process.exit(1);
  }
}
