// ci-lane-contract.mjs — schema and fail-closed qualification contract for
// Cerberus's machine-readable test-fence registry.
//
// This contract accepts no generic or caller-supplied report evidence. Every
// producer must derive evidence counts from the lane's native output; accepting
// a caller-supplied number would turn the contract into a machine-readable
// hollow green.
//
// Modes:
//   registry (default) — validate .github/ci-lanes.json and repository paths.
//   reports            — validate one selection manifest, every report below a
//                        directory, and trusted workflow-job outcomes.
//
// Env (registry):
//   MODE                  registry | reports (default registry)
//   CI_LANE_REGISTRY      registry path (default .github/ci-lanes.json)
//
// Env (reports):
//   CI_LANE_SELECTION     selection manifest path (required)
//   CI_LANE_REPORT_DIR    recursively-read report directory (required)
//   CI_LANE_PRODUCER_MANIFEST
//                         trusted API-built producer-manifest path (required)
//   CI_LANE_POSTURE     merge | main | release
//   CI_EXPECT_SHA         exact source commit selected for qualification
//   CI_EXPECT_TREE        exact Git tree selected for qualification
//   CI_EXPECT_CORRELATION_NONCE per-qualification 64-hex correlation value
//   CI_EXPECT_CANDIDATE_DIGEST
//                         sha256 digest; required when a selected lane applies
//                         to an artifact
//   CI_EXPECT_RUN_ID      selection/qualification workflow run id
//   CI_EXPECT_RUN_ATTEMPT selection/qualification workflow attempt
//   CI_EXPECT_SELECTION_SHA256 exact selection-manifest SHA-256
//   CI_EXPECT_EVENT_KIND  workflow event used for producer-run discovery
//   CI_EXPECT_PR_NUMBER   PR number; required only for pull_request
//   CI_EXPECT_EVENT_HEAD_SHA API workflow-run head SHA
//   CI_EXPECT_EVENT_BASE_SHA event base SHA; required for PR/merge group
//   CI_EXPECT_PROJECTED_SHA exact projected checkout SHA
//   CI_EXPECT_REPOSITORY  exact owner/repository slug from trusted context
//   CI_EXPECT_BASE_SHA    merge base commit; required for merge qualification
//   CI_EXPECT_CHANGED_PATHS_JSON
//                         canonical compact changed-path JSON array; required
//                         for merge qualification
//   GITHUB_STEP_SUMMARY   optional summary destination
//
// Node builtins only. Direct execution emits ::error:: annotations and exits
// non-zero on every malformed, missing, stale, skipped, neutral, cancelled,
// mismatched, or zero-execution state.

import { createHash } from "node:crypto";
import process from "node:process";
import {
  appendFileSync,
  existsSync,
  readFileSync,
  readdirSync,
  statSync,
} from "node:fs";
import { isAbsolute, join, normalize, relative, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";

export const REGISTRY_SCHEMA_VERSION = 1;
export const SELECTION_SCHEMA_VERSION = 1;
export const REPORT_SCHEMA_VERSION = 2;
export const DEFAULT_REGISTRY_PATH = ".github/ci-lanes.json";
export const MERGE_P95_SLO_MINUTES = 20;
export const RELEASE_QUALIFICATION_SLO_MINUTES = 120;

const CANONICAL_LAYERS = [
  "1",
  "2a",
  "2b",
  "3",
  "4",
  "5",
  "6a",
  "6b",
  "6c",
  "6d",
  "6e",
  "6f",
  "7",
  "7b",
  "8",
  "9",
  "10",
  "11",
  "12",
  "13",
  "14",
];

// Each query head needs three genuinely different semantic signals. These are
// policy facts, not lane IDs: providers may be renamed or split without
// weakening the fence, but deleting or reclassifying a provider must fail.
export const CANONICAL_HEAD_ORACLE_FLOORS = Object.freeze({
  promql: Object.freeze({ layer: "6a", classes: Object.freeze(["execution", "property", "reference"]) }),
  logql: Object.freeze({ layer: "6b", classes: Object.freeze(["execution", "property", "reference"]) }),
  traceql: Object.freeze({ layer: "6c", classes: Object.freeze(["execution", "property", "reference"]) }),
});

const CANONICAL_HEAD_ORACLE_SOURCE_ROOTS = Object.freeze({
  promql: Object.freeze({
    execution: "test/spec/promql",
    reference: "compatibility/prometheus",
  }),
  logql: Object.freeze({
    execution: "test/spec/logql",
    reference: "compatibility/loki",
  }),
  traceql: Object.freeze({
    execution: "test/spec/traceql",
    reference: "compatibility/tempo",
  }),
});

const TOP_KEYS = new Set([
  "schema_version",
  "selection_schema_version",
  "report_schema_version",
  "rollout",
  "impact_selection",
  "layers",
  "lanes",
  "non_lane_workflows",
]);
const LAYER_KEYS = new Set(["id", "name"]);
const IMPACT_SELECTION_KEYS = new Set(["known_nonimpact_globs"]);
const LANE_KEYS = new Set([
  "id",
  "description",
  "owner",
  "executions",
  "context",
  "purpose",
  "layers",
  "oracle_class",
  "recipes",
  "command",
  "build_tags",
  "package_globs",
  "substrate",
  "risk_domains",
  "merge_posture",
  "main_posture",
  "release_posture",
  "determinism",
  "applicability",
  "selector",
  "timeout_minutes",
  "slo_minutes",
  "accountable_owner",
  "report_schema_version",
]);
const OWNER_KEYS = new Set([
  "workflow",
  "jobs",
  "producer_jobs",
  "context_job",
]);
const CONTEXT_KEYS = new Set(["match", "name", "protected"]);
const APPLICABILITY_KEYS = new Set(["source", "artifact"]);
const SELECTOR_POLICY_KEYS = new Set(["unknown_paths", "failure"]);
const NON_LANE_KEYS = new Set(["workflow", "reason"]);

const PURPOSES = new Set([
  "correctness",
  "governance",
  "performance",
  "quality",
  "release",
  "security",
]);
const ORACLE_CLASSES = new Set([
  "consumer",
  "coverage",
  "execution",
  "golden",
  "migration",
  "monitor",
  "mutation",
  "packaging",
  "performance",
  "property",
  "reference",
  "resilience",
  "security",
  "static",
]);
const SUBSTRATES = new Set([
  "chdb",
  "codeql",
  "docker-compose",
  "github-api",
  "homebrew",
  "k3d",
  "public-network",
  "real-clickhouse",
  "reference-stack",
  "runner",
]);
const MERGE_POSTURES = new Set([
  "always",
  "impact",
  "never",
  "non_documentation",
]);
const MAIN_POSTURES = new Set(["always", "coalesced", "never"]);
const RELEASE_POSTURES = new Set(["required", "advisory", "post_publish"]);
const DETERMINISM = new Set(["deterministic", "observational", "seeded"]);
const NON_LANE_REASONS = new Set([
  "automation",
  "dependency_maintenance",
  "infrastructure",
  "labeling",
  "publication",
  "release_preparation",
]);
const SELECTION_POSTURES = new Set(["merge", "main", "release"]);
const SELECTOR_CONCLUSIONS = new Set(["success", "fallback_full"]);
const DISPOSITIONS = new Set(["selected", "omitted"]);
const OMISSION_REASONS = new Set([
  "advisory",
  "docs_only",
  "not_impacted",
  "post_publish",
  "posture_excluded",
  "superseded_main",
]);
const REPORT_CONCLUSIONS = new Set([
  "action_required",
  "cancelled",
  "failure",
  "neutral",
  "skipped",
  "stale",
  "startup_failure",
  "success",
  "timed_out",
]);
const INVOCATION_MODES = new Set([
  "candidate_artifact",
  "public_monitor",
  "source_tree",
]);
const JOB_CONCLUSIONS = REPORT_CONCLUSIONS;

const SELECTION_KEYS = new Set([
  "schema_version",
  "registry_schema_version",
  "report_schema_version",
  "posture",
  "source",
  "candidate_digest",
  "correlation_nonce",
  "run",
  "selector",
  "lanes",
]);
const SOURCE_KEYS = new Set(["sha", "tree"]);
const RUN_KEYS = new Set(["id", "attempt"]);
const SELECTION_SELECTOR_KEYS = new Set([
  "conclusion",
  "base_sha",
  "head_sha",
  "changed_paths",
  "unknown_paths",
]);
const SELECTION_LANE_KEYS = new Set([
  "lane_id",
  "disposition",
  "executions",
  "reason",
]);

const REPORT_KEYS = new Set([
  "schema_version",
  "registry_schema_version",
  "lane_id",
  "execution_id",
  "posture",
  "source",
  "candidate_digest",
  "correlation_nonce",
  "selection_ref",
  "producer",
  "invocation",
  "evidence",
  "conclusion",
]);
const SELECTION_REF_KEYS = new Set(["run", "manifest_sha256"]);
const PRODUCER_KEYS = new Set(["workflow", "job", "run", "artifact"]);
const PRODUCER_ARTIFACT_KEYS = new Set([
  "id",
  "name",
  "sha256",
  "entry",
  "entry_sha256",
]);
const INVOCATION_KEYS = new Set([
  "mode",
  "recipe",
  "command",
  "build_tags",
  "selected_domains",
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
const PRODUCER_MANIFEST_KEYS = new Set([
  "schema_version",
  "correlation_nonce",
  "selection_ref",
  "producers",
]);
const TRUSTED_PRODUCER_KEYS = new Set([
  "workflow",
  "run",
  "source",
  "correlation_nonce",
  "event",
  "repository",
  "conclusion",
  "jobs",
  "artifacts",
]);
const TRUSTED_JOB_KEYS = new Set([
  "job",
  "name",
  "database_id",
  "conclusion",
]);
const TRUSTED_ARTIFACT_KEYS = new Set([
  "id",
  "name",
  "sha256",
  "entries",
]);
const TRUSTED_ARTIFACT_ENTRY_KEYS = new Set([
  "lane_id",
  "execution_id",
  "sha256",
]);
// Pull-request Actions runs expose the branch-head SHA through the REST API,
// while checkout uses the projected merge commit. Keep both identities: run
// discovery binds the event head/base/PR tuple; native evidence binds the
// projected commit and tree. Equating the two rejects honest PR evidence.
const EVENT_IDENTITY_KEYS = new Set([
  "kind",
  "pr_number",
  "event_head_sha",
  "event_base_sha",
  "projected_sha",
]);
const QUALIFICATION_EXPECTATION_KEYS = new Set([
  "posture",
  "sha",
  "tree",
  "candidateDigest",
  "correlationNonce",
  "runID",
  "runAttempt",
  "selectionManifestSHA256",
  "eventIdentity",
  "repository",
  "baseSHA",
  "changedPaths",
]);

const LANE_ID_RE = /^[a-z][a-z0-9]*(?:[.-][a-z0-9]+)*$/;
const EXECUTION_ID_RE = /^[a-z0-9][a-z0-9._-]*$/;
const WORKFLOW_JOB_ID_RE = /^[A-Za-z_][A-Za-z0-9_-]*$/;
const SHA_RE = /^(?:[0-9a-f]{40}|[0-9a-f]{64})$/;
const DIGEST_RE = /^sha256:[0-9a-f]{64}$/;
const RUN_ID_RE = /^[1-9][0-9]*$/;
const SHA256_RE = /^[0-9a-f]{64}$/;
const CORRELATION_NONCE_RE = /^[0-9a-f]{64}$/;
const REPOSITORY_RE = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;
const EVENT_KINDS = new Set([
  "merge_group",
  "pull_request",
  "push",
  "schedule",
  "workflow_dispatch",
]);
const DOMAIN_RE = /^[a-z0-9][a-z0-9-]*$/;
const UNSUPPORTED_GLOB_META_RE = /[?\[\]{}]/;

export class ContractError extends Error {
  constructor(label, problems) {
    super(`${label}:\n${problems.map((p) => `- ${p}`).join("\n")}`);
    this.name = "ContractError";
    this.problems = problems;
  }
}

function isObject(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function exactObject(value, keys, path, problems) {
  if (!isObject(value)) {
    problems.push(`${path} must be an object`);
    return false;
  }
  for (const key of keys) {
    if (!Object.hasOwn(value, key)) problems.push(`${path}.${key} is required`);
  }
  for (const key of Object.keys(value)) {
    if (!keys.has(key)) problems.push(`${path}.${key} is unknown`);
  }
  return true;
}

function stringValue(value, path, problems, { pattern } = {}) {
  if (typeof value !== "string" || value.trim() === "") {
    problems.push(`${path} must be a non-empty string`);
    return false;
  }
  if (pattern && !pattern.test(value)) {
    problems.push(`${path} has invalid value ${JSON.stringify(value)}`);
    return false;
  }
  return true;
}

function enumValue(value, allowed, path, problems) {
  if (!allowed.has(value)) {
    problems.push(
      `${path} must be one of ${[...allowed].join(", ")}; got ${JSON.stringify(value)}`,
    );
    return false;
  }
  return true;
}

function integerValue(value, path, problems, { min = 0 } = {}) {
  if (!Number.isInteger(value) || value < min) {
    problems.push(
      `${path} must be an integer >= ${min}; got ${JSON.stringify(value)}`,
    );
    return false;
  }
  return true;
}

function booleanValue(value, path, problems) {
  if (typeof value !== "boolean") {
    problems.push(`${path} must be boolean; got ${JSON.stringify(value)}`);
    return false;
  }
  return true;
}

function stringArray(
  value,
  path,
  problems,
  { allowEmpty = false, pattern } = {},
) {
  if (!Array.isArray(value)) {
    problems.push(`${path} must be an array`);
    return false;
  }
  if (!allowEmpty && value.length === 0)
    problems.push(`${path} must not be empty`);
  const seen = new Set();
  for (let i = 0; i < value.length; i += 1) {
    if (!stringValue(value[i], `${path}[${i}]`, problems, { pattern }))
      continue;
    if (seen.has(value[i]))
      problems.push(`${path} contains duplicate ${JSON.stringify(value[i])}`);
    seen.add(value[i]);
  }
  return true;
}

function parseJSONFile(path, label = path) {
  let body;
  try {
    body = readFileSync(path, "utf8");
  } catch (error) {
    throw new ContractError(label, [`cannot read ${path}: ${error.message}`]);
  }
  try {
    return JSON.parse(body);
  } catch (error) {
    throw new ContractError(label, [
      `${path} is not valid JSON: ${error.message}`,
    ]);
  }
}

function repositoryPath(
  root,
  value,
  path,
  problems,
  { mustExist = false } = {},
) {
  if (!stringValue(value, path, problems)) return null;
  if (isAbsolute(value)) {
    problems.push(
      `${path} must be repository-relative, got absolute path ${value}`,
    );
    return null;
  }
  const clean = normalize(value);
  if (clean === ".." || clean.startsWith(`..${sep}`)) {
    problems.push(`${path} escapes the repository: ${value}`);
    return null;
  }
  const absolute = resolve(root, clean);
  const back = relative(resolve(root), absolute);
  if (back === ".." || back.startsWith(`..${sep}`)) {
    problems.push(`${path} escapes the repository: ${value}`);
    return null;
  }
  if (mustExist && !existsSync(absolute))
    problems.push(`${path} does not exist: ${value}`);
  return absolute;
}

function canonicalRepositoryPath(
  value,
  path,
  problems,
  { allowGlob = false } = {},
) {
  if (!stringValue(value, path, problems)) return false;
  if (value.includes("\\")) {
    problems.push(`${path} must use forward slashes, not backslashes`);
    return false;
  }
  if (isAbsolute(value)) {
    problems.push(
      `${path} must be repository-relative, got absolute path ${value}`,
    );
    return false;
  }
  if (!allowGlob && value.includes("*")) {
    problems.push(`${path} must be a literal repository path, not a glob`);
    return false;
  }
  if (allowGlob && UNSUPPORTED_GLOB_META_RE.test(value)) {
    problems.push(
      `${path} uses unsupported glob syntax ${JSON.stringify(value)}`,
    );
    return false;
  }
  const segments = value.split("/");
  if (
    segments.some(
      (segment) => segment === "" || segment === "." || segment === "..",
    )
  ) {
    problems.push(
      `${path} is not a normalized repository-relative path: ${value}`,
    );
    return false;
  }
  const clean = normalize(value);
  if (clean !== value || clean === ".." || clean.startsWith(`..${sep}`)) {
    problems.push(
      `${path} is not a normalized repository-relative path: ${value}`,
    );
    return false;
  }
  return true;
}

function validateRepositoryGlob(root, value, path, problems) {
  if (!canonicalRepositoryPath(value, path, problems, { allowGlob: true }))
    return;
  const segments = value.split("/");
  const wildcardIndex = segments.findIndex((segment) => segment.includes("*"));
  if (wildcardIndex === -1) {
    if (!existsSync(resolve(root, value)))
      problems.push(`${path} does not exist: ${value}`);
    return;
  }
  const staticPrefix = segments.slice(0, wildcardIndex).join("/");
  if (staticPrefix === "") return;
  const absolutePrefix = resolve(root, staticPrefix);
  if (!existsSync(absolutePrefix)) {
    problems.push(`${path} has stale static directory prefix ${staticPrefix}`);
    return;
  }
  if (!statSync(absolutePrefix).isDirectory()) {
    problems.push(`${path} static prefix is not a directory: ${staticPrefix}`);
  }
}

export function validateChangedPaths(value, path, problems) {
  if (!stringArray(value, path, problems, { allowEmpty: true })) return false;
  for (let i = 0; i < value.length; i += 1) {
    canonicalRepositoryPath(value[i], `${path}[${i}]`, problems);
  }
  const sorted = [...value].sort();
  if (!value.every((entry, index) => entry === sorted[index])) {
    problems.push(`${path} must be sorted lexicographically`);
  }
  return true;
}

function globRegExp(glob) {
  let pattern = "^";
  for (let i = 0; i < glob.length; i += 1) {
    const char = glob[i];
    if (char === "*" && glob[i + 1] === "*") {
      i += 1;
      if (glob[i + 1] === "/") {
        i += 1;
        pattern += "(?:.*/)?";
      } else {
        pattern += ".*";
      }
    } else if (char === "*") {
      pattern += "[^/]*";
    } else {
      pattern += char.replace(/[|\\{}()[\]^$+?.]/g, "\\$&");
    }
  }
  return new RegExp(`${pattern}$`);
}

export function matchesGlob(path, glob) {
  return globRegExp(glob).test(path);
}

function justRecipeCommands(root) {
  const text = readFileSync(join(root, "Justfile"), "utf8");
  const recipes = new Map();
  let current = null;
  for (const line of text.split("\n")) {
    // A recipe may declare parameters before the colon and dependencies after
    // it. Just assignments use `:=`, so the negative lookahead keeps settings
    // and variables out of the recipe namespace without discarding dependency
    // recipes such as `e2e-up: e2e-down`.
    const match = /^([A-Za-z0-9_-]+)(?:\s+[^:]*)?:(?!=)/.exec(line);
    if (match) {
      current = match[1];
      recipes.set(current, []);
      continue;
    }
    if (current !== null && /^\s+/.test(line)) {
      const command = line.trim().replace(/^@/, "");
      if (command !== "" && !command.startsWith("#")) {
        recipes.get(current).push(command);
      }
      continue;
    }
    if (line.trim() !== "" && !line.trim().startsWith("#")) current = null;
  }
  return recipes;
}

function workflowJobCommands(root, workflow, jobs) {
  if (
    typeof workflow !== "string" ||
    !Array.isArray(jobs) ||
    !existsSync(resolve(root, workflow))
  ) {
    return [];
  }
  const lines = readFileSync(resolve(root, workflow), "utf8").split("\n");
  const jobsIndex = lines.findIndex((line) => /^\s*jobs:\s*(?:#.*)?$/.test(line));
  if (jobsIndex === -1) return [];
  const jobsIndent = /^\s*/.exec(lines[jobsIndex])[0].length;
  const jobIndent = jobsIndent + 2;
  const wanted = new Set(jobs);
  const commands = [];
  let active = false;

  for (let i = jobsIndex + 1; i < lines.length; i += 1) {
    const line = lines[i];
    const trimmed = line.trim();
    if (trimmed === "" || trimmed.startsWith("#")) continue;
    const indent = /^\s*/.exec(line)[0].length;
    if (indent <= jobsIndent) break;
    const job = /^\s*([A-Za-z_][A-Za-z0-9_-]*):\s*(?:#.*)?$/.exec(line);
    if (indent === jobIndent && job) {
      active = wanted.has(job[1]);
      continue;
    }
    if (!active) continue;

    const run = /^\s*(?:-\s*)?run:\s*(.*)$/.exec(line);
    if (!run) continue;
    const value = run[1].trim();
    if (!/^[|>][-+]?\s*$/.test(value)) {
      if (value !== "" && !value.startsWith("#")) commands.push(value);
      continue;
    }

    for (let j = i + 1; j < lines.length; j += 1) {
      const bodyLine = lines[j];
      const bodyTrimmed = bodyLine.trim();
      if (bodyTrimmed === "") continue;
      const bodyIndent = /^\s*/.exec(bodyLine)[0].length;
      if (bodyIndent <= indent) break;
      if (!bodyTrimmed.startsWith("#")) commands.push(bodyTrimmed);
    }
  }
  return commands;
}

function normalizedCommand(value) {
  return value.trim().replace(/\s+/g, " ");
}

function commandAppears(commands, evidence) {
  if (typeof evidence !== "string" || evidence.trim() === "") return false;
  const needle = normalizedCommand(evidence);
  return commands.some((command) => normalizedCommand(command).includes(needle));
}

function recipeInvocationAppears(commands, recipe) {
  const escaped = recipe.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const invocation = new RegExp(
    `(?:^|[\\s;&|()])just\\s+(?:-[^\\s]+\\s+)*${escaped}(?:$|[\\s;&|()])`,
  );
  return commands.some((command) => invocation.test(normalizedCommand(command)));
}

function providerHasCommandEvidence(lane, root, recipeCommands) {
  const commands = workflowJobCommands(
    root,
    lane.owner?.workflow,
    lane.owner?.jobs,
  );
  if (commandAppears(commands, lane.command)) return true;
  if (!Array.isArray(lane.recipes)) return false;
  return lane.recipes.some((recipe) => {
    if (recipeInvocationAppears(commands, recipe)) return true;
    const adapterCommands = recipeCommands.get(recipe) ?? [];
    return adapterCommands.some((command) => commandAppears(commands, command));
  });
}

function repositoryFilesUnder(root, repositoryDirectory) {
  const directory = resolve(root, repositoryDirectory);
  if (!existsSync(directory) || !statSync(directory).isDirectory()) return [];
  const files = [];
  const pending = [directory];
  while (pending.length > 0) {
    const current = pending.pop();
    for (const entry of readdirSync(current, { withFileTypes: true })) {
      const absolute = join(current, entry.name);
      if (entry.isDirectory()) pending.push(absolute);
      else if (entry.isFile()) {
        files.push(relative(resolve(root), absolute).split(sep).join("/"));
      }
    }
  }
  return files;
}

function providerHasCanonicalSource(lane, root, sourceRoot) {
  if (!Array.isArray(lane.package_globs)) return false;
  const globs = lane.package_globs.filter((glob) => typeof glob === "string");
  return repositoryFilesUnder(root, sourceRoot).some((file) =>
    globs.some((glob) => matchesGlob(file, glob)),
  );
}

function workflowFiles(root) {
  const dir = join(root, ".github", "workflows");
  return readdirSync(dir, { withFileTypes: true })
    .filter(
      (entry) =>
        entry.isFile() &&
        (entry.name.endsWith(".yml") || entry.name.endsWith(".yaml")),
    )
    .map((entry) => `.github/workflows/${entry.name}`)
    .sort();
}

function validateCanonicalHeadOracleFloors(lanes, root, recipeCommands, problems) {
  if (!Array.isArray(lanes)) return;
  for (const [head, requirement] of Object.entries(CANONICAL_HEAD_ORACLE_FLOORS)) {
    for (const oracleClass of requirement.classes) {
      const providers = lanes.filter(
        (lane) =>
          isObject(lane) &&
          lane.oracle_class === oracleClass &&
          Array.isArray(lane.layers) &&
          lane.layers.includes(requirement.layer) &&
          Array.isArray(lane.risk_domains) &&
          lane.risk_domains.includes(head) &&
          (lane.main_posture === "always" ||
            lane.main_posture === "coalesced") &&
          lane.release_posture === "required" &&
          lane.applicability?.source === true,
      );
      if (providers.length === 0) {
        problems.push(
            `canonical head ${head} layer ${requirement.layer} requires a source-applicable ` +
            `${oracleClass} oracle with risk domain ${head}, main_posture always or coalesced, and ` +
            `release_posture required`,
        );
      }
      if (oracleClass !== "execution" && oracleClass !== "reference") continue;
      for (const provider of providers) {
        if (!providerHasCommandEvidence(provider, root, recipeCommands)) {
          problems.push(
            `canonical head ${head} ${oracleClass} provider ${provider.id} has no command ` +
              `evidence in its declared workflow jobs`,
          );
        }
        const sourceRoot = CANONICAL_HEAD_ORACLE_SOURCE_ROOTS[head][oracleClass];
        if (!providerHasCanonicalSource(provider, root, sourceRoot)) {
          problems.push(
            `canonical head ${head} ${oracleClass} provider ${provider.id} has no real ` +
              `oracle/test source under ${sourceRoot} covered by package_globs`,
          );
        }
      }
    }
  }
}

export function validateRegistry(document, { root = process.cwd() } = {}) {
  const problems = [];
  if (!exactObject(document, TOP_KEYS, "registry", problems)) {
    throw new ContractError("invalid CI lane registry", problems);
  }

  if (document.schema_version !== REGISTRY_SCHEMA_VERSION) {
    problems.push(`registry.schema_version must be ${REGISTRY_SCHEMA_VERSION}`);
  }
  if (document.selection_schema_version !== SELECTION_SCHEMA_VERSION) {
    problems.push(
      `registry.selection_schema_version must be ${SELECTION_SCHEMA_VERSION}`,
    );
  }
  if (document.report_schema_version !== REPORT_SCHEMA_VERSION) {
    problems.push(
      `registry.report_schema_version must be ${REPORT_SCHEMA_VERSION}`,
    );
  }
  if (document.rollout !== "shadow") {
    problems.push(
      `registry.rollout must be shadow in schema v${REGISTRY_SCHEMA_VERSION}`,
    );
  }

  if (
    exactObject(
      document.impact_selection,
      IMPACT_SELECTION_KEYS,
      "registry.impact_selection",
      problems,
    ) &&
    stringArray(
      document.impact_selection.known_nonimpact_globs,
      "registry.impact_selection.known_nonimpact_globs",
      problems,
    )
  ) {
    for (
      let i = 0;
      i < document.impact_selection.known_nonimpact_globs.length;
      i += 1
    ) {
      validateRepositoryGlob(
        root,
        document.impact_selection.known_nonimpact_globs[i],
        `registry.impact_selection.known_nonimpact_globs[${i}]`,
        problems,
      );
    }
  }

  if (!Array.isArray(document.layers)) {
    problems.push("registry.layers must be an array");
  } else {
    const ids = [];
    for (let i = 0; i < document.layers.length; i += 1) {
      const layer = document.layers[i];
      const at = `registry.layers[${i}]`;
      if (!exactObject(layer, LAYER_KEYS, at, problems)) continue;
      stringValue(layer.id, `${at}.id`, problems);
      stringValue(layer.name, `${at}.name`, problems);
      ids.push(layer.id);
    }
    if (JSON.stringify(ids) !== JSON.stringify(CANONICAL_LAYERS)) {
      problems.push(
        `registry.layers IDs must be the canonical ordered set ${CANONICAL_LAYERS.join(", ")}`,
      );
    }
  }

  const knownLayers = new Set(CANONICAL_LAYERS);
  const coveredLayers = new Set();
  const laneIDs = new Set();
  const protectedContexts = new Set();
  const recipeCommands = justRecipeCommands(root);
  const recipes = new Set(recipeCommands.keys());
  const ownedWorkflows = new Set();

  if (!Array.isArray(document.lanes) || document.lanes.length === 0) {
    problems.push("registry.lanes must be a non-empty array");
  } else {
    let previousID = "";
    for (let i = 0; i < document.lanes.length; i += 1) {
      const lane = document.lanes[i];
      const at = `registry.lanes[${i}]`;
      if (!exactObject(lane, LANE_KEYS, at, problems)) continue;

      if (stringValue(lane.id, `${at}.id`, problems, { pattern: LANE_ID_RE })) {
        if (laneIDs.has(lane.id))
          problems.push(`${at}.id duplicates lane ${lane.id}`);
        if (previousID !== "" && lane.id.localeCompare(previousID) <= 0) {
          problems.push(
            `${at}.id ${lane.id} is not strictly sorted after ${previousID}`,
          );
        }
        laneIDs.add(lane.id);
        previousID = lane.id;
      }
      stringValue(lane.description, `${at}.description`, problems);

      if (exactObject(lane.owner, OWNER_KEYS, `${at}.owner`, problems)) {
        const workflow = lane.owner.workflow;
        repositoryPath(root, workflow, `${at}.owner.workflow`, problems, {
          mustExist: true,
        });
        if (typeof workflow === "string") ownedWorkflows.add(workflow);
        stringArray(lane.owner.jobs, `${at}.owner.jobs`, problems, {
          pattern: WORKFLOW_JOB_ID_RE,
        });
        if (
          stringArray(
            lane.owner.producer_jobs,
            `${at}.owner.producer_jobs`,
            problems,
            {
              allowEmpty: true,
              pattern: WORKFLOW_JOB_ID_RE,
            },
          )
        ) {
          for (const job of lane.owner.producer_jobs) {
            if (
              !Array.isArray(lane.owner.jobs) ||
              !lane.owner.jobs.includes(job)
            ) {
              problems.push(
                `${at}.owner.producer_jobs contains ${job}, which is not in owner.jobs`,
              );
            }
            if (job === lane.owner.context_job) {
              problems.push(
                `${at}.owner.producer_jobs must exclude the context_job ${job}`,
              );
            }
          }
        }
        if (
          stringValue(
            lane.owner.context_job,
            `${at}.owner.context_job`,
            problems,
            {
              pattern: WORKFLOW_JOB_ID_RE,
            },
          )
        ) {
          if (
            !Array.isArray(lane.owner.jobs) ||
            !lane.owner.jobs.includes(lane.owner.context_job)
          ) {
            problems.push(`${at}.owner.context_job must appear in owner.jobs`);
          }
        }
      }
      stringArray(lane.executions, `${at}.executions`, problems, {
        pattern: EXECUTION_ID_RE,
      });

      if (exactObject(lane.context, CONTEXT_KEYS, `${at}.context`, problems)) {
        enumValue(
          lane.context.match,
          new Set(["exact", "prefix"]),
          `${at}.context.match`,
          problems,
        );
        stringValue(lane.context.name, `${at}.context.name`, problems);
        booleanValue(
          lane.context.protected,
          `${at}.context.protected`,
          problems,
        );
        if (lane.context.protected && lane.context.match !== "exact") {
          problems.push(
            `${at}.context: a protected context must use exact matching`,
          );
        }
        if (lane.context.protected && typeof lane.context.name === "string") {
          if (protectedContexts.has(lane.context.name)) {
            problems.push(
              `${at}.context.name duplicates protected context ${lane.context.name}`,
            );
          }
          protectedContexts.add(lane.context.name);
        }
      }

      enumValue(lane.purpose, PURPOSES, `${at}.purpose`, problems);
      if (
        stringArray(lane.layers, `${at}.layers`, problems, { allowEmpty: true })
      ) {
        for (const layer of lane.layers) {
          if (!knownLayers.has(layer))
            problems.push(`${at}.layers contains unknown layer ${layer}`);
          else coveredLayers.add(layer);
        }
      }
      enumValue(
        lane.oracle_class,
        ORACLE_CLASSES,
        `${at}.oracle_class`,
        problems,
      );
      if (
        stringArray(lane.recipes, `${at}.recipes`, problems, {
          allowEmpty: true,
        })
      ) {
        for (const recipe of lane.recipes) {
          if (!recipes.has(recipe))
            problems.push(`${at}.recipes names missing Just recipe ${recipe}`);
        }
      }
      stringValue(lane.command, `${at}.command`, problems);
      stringArray(lane.build_tags, `${at}.build_tags`, problems, {
        allowEmpty: true,
      });
      if (stringArray(lane.package_globs, `${at}.package_globs`, problems)) {
        for (let j = 0; j < lane.package_globs.length; j += 1) {
          validateRepositoryGlob(
            root,
            lane.package_globs[j],
            `${at}.package_globs[${j}]`,
            problems,
          );
        }
      }
      enumValue(lane.substrate, SUBSTRATES, `${at}.substrate`, problems);
      stringArray(lane.risk_domains, `${at}.risk_domains`, problems, {
        pattern: DOMAIN_RE,
      });
      enumValue(
        lane.merge_posture,
        MERGE_POSTURES,
        `${at}.merge_posture`,
        problems,
      );
      enumValue(
        lane.main_posture,
        MAIN_POSTURES,
        `${at}.main_posture`,
        problems,
      );
      enumValue(
        lane.release_posture,
        RELEASE_POSTURES,
        `${at}.release_posture`,
        problems,
      );
      enumValue(lane.determinism, DETERMINISM, `${at}.determinism`, problems);

      if (
        exactObject(
          lane.applicability,
          APPLICABILITY_KEYS,
          `${at}.applicability`,
          problems,
        )
      ) {
        const sourceOK = booleanValue(
          lane.applicability.source,
          `${at}.applicability.source`,
          problems,
        );
        const artifactOK = booleanValue(
          lane.applicability.artifact,
          `${at}.applicability.artifact`,
          problems,
        );
        if (
          sourceOK &&
          artifactOK &&
          !lane.applicability.source &&
          !lane.applicability.artifact
        ) {
          problems.push(
            `${at}.applicability must select source, artifact, or both`,
          );
        }
        if (lane.merge_posture !== "never" && !lane.applicability.source) {
          problems.push(`${at}: a merge lane must apply to source`);
        }
        if (
          lane.release_posture === "post_publish" &&
          !lane.applicability.artifact
        ) {
          problems.push(`${at}: a post-publish lane must apply to an artifact`);
        }
      }

      if (
        exactObject(
          lane.selector,
          SELECTOR_POLICY_KEYS,
          `${at}.selector`,
          problems,
        )
      ) {
        if (lane.selector.unknown_paths !== "full") {
          problems.push(`${at}.selector.unknown_paths must be full`);
        }
        if (lane.selector.failure !== "full")
          problems.push(`${at}.selector.failure must be full`);
      }
      integerValue(lane.timeout_minutes, `${at}.timeout_minutes`, problems, {
        min: 1,
      });
      integerValue(lane.slo_minutes, `${at}.slo_minutes`, problems, { min: 1 });
      if (
        Number.isInteger(lane.timeout_minutes) &&
        Number.isInteger(lane.slo_minutes) &&
        lane.slo_minutes > lane.timeout_minutes
      ) {
        problems.push(`${at}.slo_minutes must not exceed timeout_minutes`);
      }
      if (
        Number.isInteger(lane.slo_minutes) &&
        lane.merge_posture !== "never" &&
        lane.slo_minutes > MERGE_P95_SLO_MINUTES
      ) {
        problems.push(
          `${at}.slo_minutes must be <= ${MERGE_P95_SLO_MINUTES} for a merge lane`,
        );
      }
      if (
        Number.isInteger(lane.slo_minutes) &&
        lane.release_posture === "required" &&
        lane.slo_minutes > RELEASE_QUALIFICATION_SLO_MINUTES
      ) {
        problems.push(
          `${at}.slo_minutes must be <= ${RELEASE_QUALIFICATION_SLO_MINUTES} for a release-required lane`,
        );
      }
      stringValue(lane.accountable_owner, `${at}.accountable_owner`, problems, {
        pattern: DOMAIN_RE,
      });
      if (lane.report_schema_version !== document.report_schema_version) {
        problems.push(
          `${at}.report_schema_version must equal registry.report_schema_version`,
        );
      }
      if (
        lane.release_posture === "required" &&
        lane.determinism === "observational"
      ) {
        problems.push(
          `${at}: an observational lane cannot be release-required`,
        );
      }
    }
  }

  validateCanonicalHeadOracleFloors(
    document.lanes,
    root,
    recipeCommands,
    problems,
  );

  for (const layer of CANONICAL_LAYERS) {
    if (!coveredLayers.has(layer))
      problems.push(`canonical test layer ${layer} has no registered lane`);
  }

  const nonLaneWorkflows = new Set();
  if (!Array.isArray(document.non_lane_workflows)) {
    problems.push("registry.non_lane_workflows must be an array");
  } else {
    let previous = "";
    for (let i = 0; i < document.non_lane_workflows.length; i += 1) {
      const item = document.non_lane_workflows[i];
      const at = `registry.non_lane_workflows[${i}]`;
      if (!exactObject(item, NON_LANE_KEYS, at, problems)) continue;
      repositoryPath(root, item.workflow, `${at}.workflow`, problems, {
        mustExist: true,
      });
      enumValue(item.reason, NON_LANE_REASONS, `${at}.reason`, problems);
      if (nonLaneWorkflows.has(item.workflow))
        problems.push(`${at}.workflow is duplicated`);
      if (ownedWorkflows.has(item.workflow))
        problems.push(`${at}.workflow also owns a lane`);
      if (previous !== "" && item.workflow.localeCompare(previous) <= 0) {
        problems.push(
          `${at}.workflow is not strictly sorted after ${previous}`,
        );
      }
      nonLaneWorkflows.add(item.workflow);
      previous = item.workflow;
    }
  }

  for (const workflow of workflowFiles(root)) {
    if (!ownedWorkflows.has(workflow) && !nonLaneWorkflows.has(workflow)) {
      problems.push(
        `${workflow} is neither owned by a lane nor classified as non-lane`,
      );
    }
  }
  for (const workflow of [...ownedWorkflows, ...nonLaneWorkflows]) {
    if (!workflowFiles(root).includes(workflow))
      problems.push(`${workflow} is not a workflow file`);
  }

  if (problems.length > 0)
    throw new ContractError("invalid CI lane registry", problems);
  return document;
}

export function loadRegistry(
  path = DEFAULT_REGISTRY_PATH,
  { root = process.cwd() } = {},
) {
  const absolute = resolve(root, path);
  return validateRegistry(parseJSONFile(absolute, "CI lane registry"), {
    root,
  });
}

function validateSource(source, path, problems) {
  if (!exactObject(source, SOURCE_KEYS, path, problems)) return;
  stringValue(source.sha, `${path}.sha`, problems, { pattern: SHA_RE });
  stringValue(source.tree, `${path}.tree`, problems, { pattern: SHA_RE });
}

function validateRun(run, path, problems) {
  if (!exactObject(run, RUN_KEYS, path, problems)) return;
  stringValue(run.id, `${path}.id`, problems, { pattern: RUN_ID_RE });
  integerValue(run.attempt, `${path}.attempt`, problems, { min: 1 });
}

function validateSelectionRef(value, path, problems) {
  if (!exactObject(value, SELECTION_REF_KEYS, path, problems)) return;
  validateRun(value.run, `${path}.run`, problems);
  stringValue(value.manifest_sha256, `${path}.manifest_sha256`, problems, {
    pattern: SHA256_RE,
  });
}

function validateArtifact(value, keys, path, problems) {
  if (!exactObject(value, keys, path, problems)) return;
  stringValue(value.id, `${path}.id`, problems, { pattern: RUN_ID_RE });
  stringValue(value.name, `${path}.name`, problems);
  stringValue(value.sha256, `${path}.sha256`, problems, {
    pattern: SHA256_RE,
  });
}

export function selectionManifestSHA256(document) {
  return createHash("sha256")
    .update(`${JSON.stringify(document, null, 2)}\n`)
    .digest("hex");
}

function canonicalJSONValue(value) {
  if (Array.isArray(value)) return value.map(canonicalJSONValue);
  if (!isObject(value)) return value;
  return Object.fromEntries(
    Object.keys(value)
      .sort()
      .map((key) => [key, canonicalJSONValue(value[key])]),
  );
}

export function canonicalJSONSHA256(document) {
  return createHash("sha256")
    .update(`${JSON.stringify(canonicalJSONValue(document))}\n`)
    .digest("hex");
}

// Event-driven merge/main producers cannot share a coordinator-generated
// random value, so their correlation nonce is deterministically namespaced by
// posture and the exact projected Git objects. Release qualification must pass
// an explicit, per-invocation random nonce instead; callers may not derive one
// through this helper for release evidence.
export function deriveQualificationCorrelationNonce({ posture, source }) {
  const problems = [];
  if (posture !== "merge" && posture !== "main") {
    problems.push("derived qualification correlation is only valid for merge or main posture");
  }
  validateSource(source, "qualification correlation source", problems);
  if (problems.length > 0) {
    throw new ContractError("invalid qualification correlation", problems);
  }
  return canonicalJSONSHA256({
    domain: "ci-lane-qualification",
    posture,
    source: { sha: source.sha, tree: source.tree },
  });
}

export function nativeArtifactName(workflow, run) {
  const basename = String(workflow ?? "")
    .replace(/^.*\//, "")
    .replace(/\.ya?ml$/, "")
    .replace(/[^A-Za-z0-9_.-]+/g, "-");
  return `ci-native-${basename}-${run.id}-${run.attempt}`;
}

function validateDigest(value, path, problems, { required = false } = {}) {
  if (value === null) {
    if (required) problems.push(`${path} is required`);
    return;
  }
  stringValue(value, path, problems, { pattern: DIGEST_RE });
}

function postureExpectation(lane, posture) {
  if (posture === "merge") return lane.merge_posture;
  if (posture === "main") return lane.main_posture;
  return lane.release_posture;
}

export function validateSelection(document, registry, expected = {}) {
  const problems = [];
  if (!exactObject(document, SELECTION_KEYS, "selection", problems)) {
    throw new ContractError("invalid CI lane selection", problems);
  }
  if (document.schema_version !== registry.selection_schema_version) {
    problems.push(
      "selection.schema_version does not match registry.selection_schema_version",
    );
  }
  if (document.registry_schema_version !== registry.schema_version) {
    problems.push(
      "selection.registry_schema_version does not match registry.schema_version",
    );
  }
  if (document.report_schema_version !== registry.report_schema_version) {
    problems.push(
      "selection.report_schema_version does not match registry.report_schema_version",
    );
  }
  enumValue(
    document.posture,
    SELECTION_POSTURES,
    "selection.posture",
    problems,
  );
  validateSource(document.source, "selection.source", problems);
  validateDigest(
    document.candidate_digest,
    "selection.candidate_digest",
    problems,
  );
  stringValue(
    document.correlation_nonce,
    "selection.correlation_nonce",
    problems,
    { pattern: CORRELATION_NONCE_RE },
  );
  validateRun(document.run, "selection.run", problems);

  if (expected.posture !== undefined && document.posture !== expected.posture) {
    problems.push(
      `selection.posture is ${document.posture}, want ${expected.posture}`,
    );
  }
  if (expected.sha !== undefined && document.source?.sha !== expected.sha) {
    problems.push(
      `selection.source.sha is ${document.source?.sha}, want ${expected.sha}`,
    );
  }
  if (expected.tree !== undefined && document.source?.tree !== expected.tree) {
    problems.push(
      `selection.source.tree is ${document.source?.tree}, want ${expected.tree}`,
    );
  }
  if (
    expected.candidateDigest !== undefined &&
    document.candidate_digest !== expected.candidateDigest
  ) {
    problems.push(
      `selection.candidate_digest is ${document.candidate_digest}, want ${expected.candidateDigest}`,
    );
  }
  if (
    expected.correlationNonce !== undefined &&
    document.correlation_nonce !== expected.correlationNonce
  ) {
    problems.push(
      `selection.correlation_nonce is ${document.correlation_nonce}, want ${expected.correlationNonce}`,
    );
  }
  if (expected.runID !== undefined && document.run?.id !== expected.runID) {
    problems.push(
      `selection.run.id is ${document.run?.id}, want ${expected.runID}`,
    );
  }
  if (
    expected.runAttempt !== undefined &&
    document.run?.attempt !== expected.runAttempt
  ) {
    problems.push(
      `selection.run.attempt is ${document.run?.attempt}, want ${expected.runAttempt}`,
    );
  }
  if (
    expected.baseSHA !== undefined &&
    document.selector?.base_sha !== expected.baseSHA
  ) {
    problems.push(
      `selection.selector.base_sha is ${document.selector?.base_sha}, want ${expected.baseSHA}`,
    );
  }
  if (
    expected.changedPaths !== undefined &&
    !sameStringArray(document.selector?.changed_paths, expected.changedPaths)
  ) {
    problems.push(
      "selection.selector.changed_paths does not match trusted expected paths",
    );
  }

  if (
    exactObject(
      document.selector,
      SELECTION_SELECTOR_KEYS,
      "selection.selector",
      problems,
    )
  ) {
    enumValue(
      document.selector.conclusion,
      SELECTOR_CONCLUSIONS,
      "selection.selector.conclusion",
      problems,
    );
    if (document.selector.base_sha !== null) {
      stringValue(
        document.selector.base_sha,
        "selection.selector.base_sha",
        problems,
        {
          pattern: SHA_RE,
        },
      );
    }
    if (document.selector.head_sha !== null) {
      stringValue(
        document.selector.head_sha,
        "selection.selector.head_sha",
        problems,
        {
          pattern: SHA_RE,
        },
      );
    }
    validateChangedPaths(
      document.selector.changed_paths,
      "selection.selector.changed_paths",
      problems,
    );
    validateChangedPaths(
      document.selector.unknown_paths,
      "selection.selector.unknown_paths",
      problems,
    );
  }

  const lanesByID = new Map(registry.lanes.map((lane) => [lane.id, lane]));
  const selected = new Map();
  if (!Array.isArray(document.lanes)) {
    problems.push("selection.lanes must be an array");
  } else {
    let previous = "";
    for (let i = 0; i < document.lanes.length; i += 1) {
      const item = document.lanes[i];
      const at = `selection.lanes[${i}]`;
      if (!exactObject(item, SELECTION_LANE_KEYS, at, problems)) continue;
      if (
        stringValue(item.lane_id, `${at}.lane_id`, problems, {
          pattern: LANE_ID_RE,
        })
      ) {
        if (!lanesByID.has(item.lane_id))
          problems.push(`${at}.lane_id is not registered`);
        if (selected.has(item.lane_id))
          problems.push(`${at}.lane_id is duplicated`);
        if (previous !== "" && item.lane_id.localeCompare(previous) <= 0) {
          problems.push(
            `${at}.lane_id is not strictly sorted after ${previous}`,
          );
        }
        selected.set(item.lane_id, item);
        previous = item.lane_id;
      }
      enumValue(item.disposition, DISPOSITIONS, `${at}.disposition`, problems);
      stringArray(item.executions, `${at}.executions`, problems, {
        allowEmpty: item.disposition !== "selected",
        pattern: EXECUTION_ID_RE,
      });
      if (item.disposition === "selected") {
        if (item.reason !== null)
          problems.push(`${at}.reason must be null when selected`);
      } else {
        if (!Array.isArray(item.executions) || item.executions.length !== 0) {
          problems.push(`${at}.executions must be empty when omitted`);
        }
        enumValue(item.reason, OMISSION_REASONS, `${at}.reason`, problems);
      }
    }
  }

  for (const lane of registry.lanes) {
    const item = selected.get(lane.id);
    if (!item) {
      problems.push(`selection is not total: lane ${lane.id} is missing`);
      continue;
    }
    if (
      item.disposition === "selected" &&
      !sameStringArray(item.executions, lane.executions)
    ) {
      problems.push(
        `${lane.id} selected executions must exactly equal registry roster ${JSON.stringify(lane.executions)}`,
      );
    }
    const posture = postureExpectation(lane, document.posture);
    if (document.posture === "merge") {
      if (posture === "always" && item.disposition !== "selected") {
        problems.push(`${lane.id} is merge-always and must be selected`);
      }
      if (posture === "impact" && item.disposition === "omitted") {
        if (!new Set(["docs_only", "not_impacted"]).has(item.reason)) {
          problems.push(
            `${lane.id} is merge-impact and has invalid omission reason ${item.reason}`,
          );
        }
      }
      if (
        posture === "non_documentation" &&
        item.disposition === "omitted" &&
        item.reason !== "docs_only"
      ) {
        problems.push(
          `${lane.id} is merge-non-documentation and may omit only as docs_only`,
        );
      }
      if (
        posture === "never" &&
        (item.disposition !== "omitted" || item.reason !== "posture_excluded")
      ) {
        problems.push(
          `${lane.id} is merge-never and must be omitted as posture_excluded`,
        );
      }
    } else if (document.posture === "main") {
      if (posture === "always" && item.disposition !== "selected") {
        problems.push(`${lane.id} is main-always and must be selected`);
      }
      if (posture === "coalesced" && item.disposition !== "selected") {
        problems.push(
          `${lane.id} is main-coalesced and must be selected until trusted supersession proof exists`,
        );
      }
      if (
        posture === "never" &&
        (item.disposition !== "omitted" || item.reason !== "posture_excluded")
      ) {
        problems.push(
          `${lane.id} is main-never and must be omitted as posture_excluded`,
        );
      }
    } else if (document.posture === "release") {
      if (posture === "required" && item.disposition !== "selected") {
        problems.push(`${lane.id} is release-required and must be selected`);
      }
      if (
        posture === "advisory" &&
        item.disposition === "omitted" &&
        item.reason !== "advisory"
      ) {
        problems.push(
          `${lane.id} is release-advisory and must omit only as advisory`,
        );
      }
      if (
        posture === "post_publish" &&
        (item.disposition !== "omitted" || item.reason !== "post_publish")
      ) {
        problems.push(
          `${lane.id} is post-publish and cannot enter pre-publication qualification`,
        );
      }
    }
  }

  if (document.posture === "merge" && isObject(document.selector)) {
    if (document.selector.base_sha === null) {
      problems.push("merge selection.selector.base_sha is required");
    }
    if (document.selector.head_sha === null) {
      problems.push("merge selection.selector.head_sha is required");
    } else if (document.selector.head_sha !== document.source?.sha) {
      problems.push(
        "merge selection.selector.head_sha must equal selection.source.sha",
      );
    }

    const changedPaths = Array.isArray(document.selector.changed_paths)
      ? document.selector.changed_paths.filter(
          (path) => typeof path === "string",
        )
      : [];
    const conditionalLanes = registry.lanes.filter(
      (lane) =>
        lane.merge_posture === "impact" ||
        lane.merge_posture === "non_documentation",
    );
    const directlyImpacted = new Set();
    const computedUnknown = [];
    const knownNonimpactGlobs =
      registry.impact_selection?.known_nonimpact_globs ?? [];
    for (const path of changedPaths) {
      const matching = conditionalLanes.filter((lane) =>
        lane.package_globs.some((glob) => matchesGlob(path, glob)),
      );
      for (const lane of matching) directlyImpacted.add(lane.id);
      if (
        matching.length === 0 &&
        !knownNonimpactGlobs.some((glob) => matchesGlob(path, glob))
      ) {
        computedUnknown.push(path);
      }
    }
    if (!sameStringArray(document.selector.unknown_paths, computedUnknown)) {
      problems.push(
        `selection.selector.unknown_paths must exactly equal computed unknown paths ${JSON.stringify(computedUnknown)}`,
      );
    }
    if (
      (changedPaths.length === 0 || computedUnknown.length > 0) &&
      document.selector.conclusion !== "fallback_full"
    ) {
      problems.push(
        "empty or unmatched merge paths require selector conclusion fallback_full",
      );
    }
    if (document.selector.conclusion === "success") {
      const docsOnly =
        changedPaths.length > 0 &&
        changedPaths.every((path) =>
          knownNonimpactGlobs.some((glob) => matchesGlob(path, glob)),
        );
      for (const lane of conditionalLanes) {
        const item = selected.get(lane.id);
        const directHit = directlyImpacted.has(lane.id);
        const shouldSelect =
          directHit ||
          (lane.merge_posture === "non_documentation" && !docsOnly);
        if (shouldSelect && item?.disposition !== "selected") {
          const kind =
            lane.merge_posture === "non_documentation"
              ? "non-documentation"
              : "impacted";
          problems.push(`selector success must select ${kind} lane ${lane.id}`);
          continue;
        }
        if (!shouldSelect && item?.disposition === "selected") {
          problems.push(
            `selector success must omit unimpacted lane ${lane.id}`,
          );
          continue;
        }
        if (!shouldSelect && item?.disposition === "omitted") {
          const expectedReason = docsOnly ? "docs_only" : "not_impacted";
          if (item.reason !== expectedReason) {
            problems.push(
              `selector success must omit ${lane.id} as ${expectedReason}`,
            );
          }
        }
      }
    }
    if (document.selector.conclusion === "fallback_full") {
      for (const lane of conditionalLanes) {
        if (selected.get(lane.id)?.disposition !== "selected") {
          problems.push(`fallback_full must select ${lane.id}`);
        }
      }
    }
  } else if (isObject(document.selector)) {
    if (
      document.selector.base_sha !== null ||
      document.selector.head_sha !== null
    ) {
      problems.push(
        "non-merge selection selector base_sha/head_sha must be null",
      );
    }
    if (
      Array.isArray(document.selector.changed_paths) &&
      document.selector.changed_paths.length > 0
    ) {
      problems.push("non-merge selection selector changed_paths must be empty");
    }
    if (
      Array.isArray(document.selector.unknown_paths) &&
      document.selector.unknown_paths.length > 0
    ) {
      problems.push("non-merge selection selector unknown_paths must be empty");
    }
  }

  const selectedArtifactLane = registry.lanes.some(
    (lane) =>
      selected.get(lane.id)?.disposition === "selected" &&
      lane.applicability.artifact,
  );
  if (selectedArtifactLane && document.candidate_digest === null) {
    problems.push(
      "selection.candidate_digest is required because an artifact lane is selected",
    );
  }
  if (document.posture === "release" && document.candidate_digest === null) {
    problems.push(
      "release selection.candidate_digest is required for the immutable candidate",
    );
  }
  if (document.posture !== "release" && document.candidate_digest !== null) {
    problems.push("only release selection may bind a candidate digest");
  }
  if (document.posture === "merge" || document.posture === "main") {
    try {
      const derived = deriveQualificationCorrelationNonce({
        posture: document.posture,
        source: document.source,
      });
      if (document.correlation_nonce !== derived) {
        problems.push(
          `${document.posture} selection.correlation_nonce does not match its projected Git objects`,
        );
      }
    } catch (error) {
      if (!(error instanceof ContractError)) throw error;
    }
  }

  if (problems.length > 0)
    throw new ContractError("invalid CI lane selection", problems);
  return document;
}

function sameStringSet(left, right) {
  if (
    !Array.isArray(left) ||
    !Array.isArray(right) ||
    left.length !== right.length
  )
    return false;
  const a = [...left].sort();
  const b = [...right].sort();
  return a.every((value, index) => value === b[index]);
}

function sameStringArray(left, right) {
  return (
    Array.isArray(left) &&
    Array.isArray(right) &&
    left.length === right.length &&
    left.every((value, index) => value === right[index])
  );
}

export function validateReport(document, registry) {
  const problems = [];
  if (!exactObject(document, REPORT_KEYS, "report", problems)) {
    throw new ContractError("invalid CI lane report", problems);
  }
  if (document.schema_version !== registry.report_schema_version) {
    problems.push(
      "report.schema_version does not match registry.report_schema_version",
    );
  }
  if (document.registry_schema_version !== registry.schema_version) {
    problems.push(
      "report.registry_schema_version does not match registry.schema_version",
    );
  }
  stringValue(document.lane_id, "report.lane_id", problems, {
    pattern: LANE_ID_RE,
  });
  stringValue(document.execution_id, "report.execution_id", problems, {
    pattern: EXECUTION_ID_RE,
  });
  enumValue(document.posture, SELECTION_POSTURES, "report.posture", problems);
  validateSource(document.source, "report.source", problems);
  validateDigest(
    document.candidate_digest,
    "report.candidate_digest",
    problems,
  );
  stringValue(
    document.correlation_nonce,
    "report.correlation_nonce",
    problems,
    { pattern: CORRELATION_NONCE_RE },
  );
  validateSelectionRef(
    document.selection_ref,
    "report.selection_ref",
    problems,
  );
  enumValue(
    document.conclusion,
    REPORT_CONCLUSIONS,
    "report.conclusion",
    problems,
  );

  const lane = registry.lanes.find(
    (candidate) => candidate.id === document.lane_id,
  );
  if (!lane)
    problems.push(`report.lane_id ${document.lane_id} is not registered`);

  if (
    exactObject(
      document.producer,
      PRODUCER_KEYS,
      "report.producer",
      problems,
    ) &&
    lane
  ) {
    if (document.producer.workflow !== lane.owner.workflow) {
      problems.push(
        "report.producer.workflow does not match the registry owner",
      );
    }
    if (document.producer.job !== lane.owner.context_job) {
      problems.push("report.producer.job must be the registry context_job");
    }
    validateRun(document.producer.run, "report.producer.run", problems);
    validateArtifact(
      document.producer.artifact,
      PRODUCER_ARTIFACT_KEYS,
      "report.producer.artifact",
      problems,
    );
    if (
      isObject(document.producer.run) &&
      isObject(document.producer.artifact)
    ) {
      const expectedArtifactName = nativeArtifactName(
        document.producer.workflow,
        document.producer.run,
      );
      if (document.producer.artifact.name !== expectedArtifactName) {
        problems.push(
          `report.producer.artifact.name must be ${expectedArtifactName}`,
        );
      }
      const expectedEntry = `${document.lane_id}/${document.execution_id}`;
      if (document.producer.artifact.entry !== expectedEntry) {
        problems.push(
          `report.producer.artifact.entry must be ${expectedEntry}`,
        );
      }
      stringValue(
        document.producer.artifact.entry_sha256,
        "report.producer.artifact.entry_sha256",
        problems,
        { pattern: SHA256_RE },
      );
    }
  }

  if (
    exactObject(
      document.invocation,
      INVOCATION_KEYS,
      "report.invocation",
      problems,
    )
  ) {
    enumValue(
      document.invocation.mode,
      INVOCATION_MODES,
      "report.invocation.mode",
      problems,
    );
    if (document.invocation.recipe !== null) {
      stringValue(
        document.invocation.recipe,
        "report.invocation.recipe",
        problems,
      );
    }
    stringValue(
      document.invocation.command,
      "report.invocation.command",
      problems,
    );
    stringArray(
      document.invocation.build_tags,
      "report.invocation.build_tags",
      problems,
      {
        allowEmpty: true,
      },
    );
    stringArray(
      document.invocation.selected_domains,
      "report.invocation.selected_domains",
      problems,
      {
        pattern: DOMAIN_RE,
      },
    );
    if (lane) {
      if (
        document.invocation.recipe !== null &&
        !lane.recipes.includes(document.invocation.recipe)
      ) {
        problems.push("report.invocation.recipe is not declared by the lane");
      }
      if (lane.recipes.length === 0 && document.invocation.recipe !== null) {
        problems.push(
          "report.invocation.recipe must be null for a lane with no recipe",
        );
      }
      if (document.invocation.command !== lane.command) {
        problems.push(
          "report.invocation.command does not match the registry command",
        );
      }
      if (!sameStringSet(document.invocation.build_tags, lane.build_tags)) {
        problems.push(
          "report.invocation.build_tags does not match the registry build-tag set",
        );
      }
      for (const domain of document.invocation.selected_domains ?? []) {
        if (!lane.risk_domains.includes(domain)) {
          problems.push(
            `report.invocation.selected_domains contains undeclared domain ${domain}`,
          );
        }
      }
      if (document.invocation.mode === "source_tree") {
        if (!lane.applicability.source)
          problems.push("source_tree report belongs to a non-source lane");
        if (document.candidate_digest !== null) {
          problems.push("source_tree report must not carry candidate_digest");
        }
      }
      if (document.invocation.mode === "candidate_artifact") {
        if (!lane.applicability.artifact) {
          problems.push(
            "candidate_artifact report belongs to a non-artifact lane",
          );
        }
        validateDigest(
          document.candidate_digest,
          "report.candidate_digest",
          problems,
          { required: true },
        );
      }
      if (document.invocation.mode === "public_monitor") {
        if (lane.release_posture !== "post_publish") {
          problems.push(
            "public_monitor report belongs to a pre-publication lane",
          );
        }
        validateDigest(
          document.candidate_digest,
          "report.candidate_digest",
          problems,
          { required: true },
        );
      }
    }
  }

  if (
    exactObject(document.evidence, EVIDENCE_KEYS, "report.evidence", problems)
  ) {
    for (const key of [
      "executed",
      "passed",
      "failed",
      "skipped",
      "duration_ms",
    ]) {
      integerValue(document.evidence[key], `report.evidence.${key}`, problems);
    }
    if (
      ["executed", "passed", "failed", "skipped"].every((key) =>
        Number.isInteger(document.evidence[key]),
      ) &&
      document.evidence.executed !==
        document.evidence.passed +
          document.evidence.failed +
          document.evidence.skipped
    ) {
      problems.push(
        "report.evidence.executed must equal passed + failed + skipped",
      );
    }
    if (document.evidence.seed !== null) {
      stringValue(document.evidence.seed, "report.evidence.seed", problems);
    }
    stringValue(
      document.evidence.corpus_id,
      "report.evidence.corpus_id",
      problems,
    );
    if (
      lane?.determinism === "deterministic" &&
      document.evidence.seed !== null
    ) {
      problems.push("deterministic lane report must carry seed=null");
    }
    if (document.conclusion === "success" && document.evidence.failed !== 0) {
      problems.push("successful report cannot carry failed evidence");
    }
  }

  if (problems.length > 0)
    throw new ContractError("invalid CI lane report", problems);
  return document;
}

export function validateEventIdentity(value, path, problems) {
  if (!exactObject(value, EVENT_IDENTITY_KEYS, path, problems)) return;
  enumValue(value.kind, EVENT_KINDS, `${path}.kind`, problems);
  if (value.pr_number !== null) {
    integerValue(value.pr_number, `${path}.pr_number`, problems, { min: 1 });
  }
  stringValue(value.event_head_sha, `${path}.event_head_sha`, problems, {
    pattern: SHA_RE,
  });
  if (value.event_base_sha !== null) {
    stringValue(value.event_base_sha, `${path}.event_base_sha`, problems, {
      pattern: SHA_RE,
    });
  }
  stringValue(value.projected_sha, `${path}.projected_sha`, problems, {
    pattern: SHA_RE,
  });
  if (value.kind === "pull_request") {
    if (value.pr_number === null)
      problems.push(`${path}.pr_number is required for pull_request`);
    if (value.event_base_sha === null)
      problems.push(`${path}.event_base_sha is required for pull_request`);
  } else if (value.pr_number !== null) {
    problems.push(`${path}.pr_number must be null outside pull_request`);
  }
  if (value.kind === "merge_group") {
    if (value.event_base_sha === null)
      problems.push(`${path}.event_base_sha is required for merge_group`);
    if (value.event_head_sha !== value.projected_sha) {
      problems.push(
        `${path}.event_head_sha must equal projected_sha for merge_group`,
      );
    }
  }
}

export function validateProducerManifest(document, registry) {
  const problems = [];
  if (!exactObject(document, PRODUCER_MANIFEST_KEYS, "producer_manifest", problems)) {
    throw new ContractError("invalid trusted producer manifest", problems);
  }
  if (document.schema_version !== REPORT_SCHEMA_VERSION) {
    problems.push(
      `producer_manifest.schema_version must be ${REPORT_SCHEMA_VERSION}`,
    );
  }
  stringValue(
    document.correlation_nonce,
    "producer_manifest.correlation_nonce",
    problems,
    { pattern: CORRELATION_NONCE_RE },
  );
  validateSelectionRef(
    document.selection_ref,
    "producer_manifest.selection_ref",
    problems,
  );

  const knownWorkflows = new Set(
    registry.lanes.map((lane) => lane.owner.workflow),
  );
  const knownJobs = new Map();
  const knownEntries = new Map();
  for (const lane of registry.lanes) {
    if (!knownJobs.has(lane.owner.workflow))
      knownJobs.set(lane.owner.workflow, new Set());
    for (const job of lane.owner.jobs)
      knownJobs.get(lane.owner.workflow).add(job);
    if (!knownEntries.has(lane.owner.workflow))
      knownEntries.set(lane.owner.workflow, new Set());
    for (const executionID of lane.executions) {
      knownEntries
        .get(lane.owner.workflow)
        .add(`${lane.id}/${executionID}`);
    }
  }
  if (!Array.isArray(document.producers)) {
    problems.push("producer_manifest.producers must be an array");
  } else {
    const workflows = new Set();
    for (let i = 0; i < document.producers.length; i += 1) {
      const producer = document.producers[i];
      const at = `producer_manifest.producers[${i}]`;
      if (!exactObject(producer, TRUSTED_PRODUCER_KEYS, at, problems))
        continue;
      if (
        stringValue(producer.workflow, `${at}.workflow`, problems) &&
        !knownWorkflows.has(producer.workflow)
      ) {
        problems.push(`${at}.workflow is not registered`);
      }
      if (workflows.has(producer.workflow)) {
        problems.push(
          `${at}.workflow duplicates ${producer.workflow}; the collector must choose exactly one newest run/attempt`,
        );
      }
      workflows.add(producer.workflow);
      validateRun(producer.run, `${at}.run`, problems);
      validateSource(producer.source, `${at}.source`, problems);
      stringValue(
        producer.correlation_nonce,
        `${at}.correlation_nonce`,
        problems,
        { pattern: CORRELATION_NONCE_RE },
      );
      if (producer.correlation_nonce !== document.correlation_nonce) {
        problems.push(
          `${at}.correlation_nonce does not match producer_manifest.correlation_nonce`,
        );
      }
      validateEventIdentity(producer.event, `${at}.event`, problems);
      stringValue(producer.repository, `${at}.repository`, problems, {
        pattern: REPOSITORY_RE,
      });
      enumValue(
        producer.conclusion,
        JOB_CONCLUSIONS,
        `${at}.conclusion`,
        problems,
      );

      if (!Array.isArray(producer.jobs) || producer.jobs.length === 0) {
        problems.push(`${at}.jobs must be a non-empty array`);
      } else {
        const jobs = new Set();
        for (let j = 0; j < producer.jobs.length; j += 1) {
          const job = producer.jobs[j];
          const jobAt = `${at}.jobs[${j}]`;
          if (!exactObject(job, TRUSTED_JOB_KEYS, jobAt, problems)) continue;
          if (
            stringValue(job.job, `${jobAt}.job`, problems, {
              pattern: WORKFLOW_JOB_ID_RE,
            }) &&
            !knownJobs.get(producer.workflow)?.has(job.job)
          ) {
            problems.push(`${jobAt}.job is not registered for the workflow`);
          }
          if (jobs.has(job.job)) problems.push(`${jobAt}.job is duplicated`);
          jobs.add(job.job);
          stringValue(job.name, `${jobAt}.name`, problems);
          stringValue(job.database_id, `${jobAt}.database_id`, problems, {
            pattern: RUN_ID_RE,
          });
          enumValue(
            job.conclusion,
            JOB_CONCLUSIONS,
            `${jobAt}.conclusion`,
            problems,
          );
        }
      }

      if (!Array.isArray(producer.artifacts) || producer.artifacts.length !== 1) {
        problems.push(`${at}.artifacts must contain exactly one native bundle`);
      } else {
        const artifactIDs = new Set();
        const artifactNames = new Set();
        for (let j = 0; j < producer.artifacts.length; j += 1) {
          const artifact = producer.artifacts[j];
          const artifactAt = `${at}.artifacts[${j}]`;
          validateArtifact(
            artifact,
            TRUSTED_ARTIFACT_KEYS,
            artifactAt,
            problems,
          );
          const expectedName = nativeArtifactName(
            producer.workflow,
            producer.run,
          );
          if (artifact?.name !== expectedName) {
            problems.push(`${artifactAt}.name must be ${expectedName}`);
          }
          if (!Array.isArray(artifact?.entries) || artifact.entries.length === 0) {
            problems.push(`${artifactAt}.entries must be a non-empty array`);
          } else {
            const entries = new Set();
            for (let k = 0; k < artifact.entries.length; k += 1) {
              const entry = artifact.entries[k];
              const entryAt = `${artifactAt}.entries[${k}]`;
              if (
                !exactObject(
                  entry,
                  TRUSTED_ARTIFACT_ENTRY_KEYS,
                  entryAt,
                  problems,
                )
              ) {
                continue;
              }
              stringValue(entry.lane_id, `${entryAt}.lane_id`, problems, {
                pattern: LANE_ID_RE,
              });
              stringValue(
                entry.execution_id,
                `${entryAt}.execution_id`,
                problems,
                { pattern: EXECUTION_ID_RE },
              );
              stringValue(entry.sha256, `${entryAt}.sha256`, problems, {
                pattern: SHA256_RE,
              });
              const identity = `${entry.lane_id}/${entry.execution_id}`;
              if (entries.has(identity)) {
                problems.push(`${entryAt} duplicates native entry ${identity}`);
              }
              if (!knownEntries.get(producer.workflow)?.has(identity)) {
                problems.push(
                  `${entryAt} is not registered for ${producer.workflow}`,
                );
              }
              entries.add(identity);
            }
          }
          if (artifactIDs.has(artifact?.id))
            problems.push(`${artifactAt}.id is duplicated`);
          if (artifactNames.has(artifact?.name))
            problems.push(`${artifactAt}.name is duplicated`);
          artifactIDs.add(artifact?.id);
          artifactNames.add(artifact?.name);
        }
      }
    }
  }
  if (problems.length > 0)
    throw new ContractError("invalid trusted producer manifest", problems);
  return document;
}

function reportIdentity(report) {
  return `${report.lane_id}/${report.execution_id}`;
}

function expectedInvocationMode(lane, posture) {
  if (posture === "release" && lane.applicability.artifact)
    return "candidate_artifact";
  return "source_tree";
}

function validateQualificationExpectations(expected, registry, selection) {
  const problems = [];
  if (
    !exactObject(expected, QUALIFICATION_EXPECTATION_KEYS, "expected", problems)
  ) {
    throw new ContractError(
      "invalid CI lane qualification expectations",
      problems,
    );
  }
  enumValue(expected.posture, SELECTION_POSTURES, "expected.posture", problems);
  stringValue(expected.sha, "expected.sha", problems, { pattern: SHA_RE });
  stringValue(expected.tree, "expected.tree", problems, { pattern: SHA_RE });
  stringValue(
    expected.correlationNonce,
    "expected.correlationNonce",
    problems,
    { pattern: CORRELATION_NONCE_RE },
  );
  stringValue(expected.runID, "expected.runID", problems, {
    pattern: RUN_ID_RE,
  });
  integerValue(expected.runAttempt, "expected.runAttempt", problems, {
    min: 1,
  });
  stringValue(
    expected.selectionManifestSHA256,
    "expected.selectionManifestSHA256",
    problems,
    { pattern: SHA256_RE },
  );
  validateEventIdentity(
    expected.eventIdentity,
    "expected.eventIdentity",
    problems,
  );
  stringValue(expected.repository, "expected.repository", problems, {
    pattern: REPOSITORY_RE,
  });
  if (expected.eventIdentity?.projected_sha !== expected.sha) {
    problems.push(
      "expected.eventIdentity.projected_sha must equal expected.sha",
    );
  }
  if (selection?.correlation_nonce !== expected.correlationNonce) {
    problems.push(
      "expected.correlationNonce must equal selection.correlation_nonce",
    );
  }

  if (expected.candidateDigest !== undefined) {
    validateDigest(
      expected.candidateDigest,
      "expected.candidateDigest",
      problems,
    );
  }
  if (expected.posture === "release" && expected.candidateDigest == null) {
    problems.push(
      "expected.candidateDigest is required for release qualification",
    );
  }
  if (expected.posture === "merge") {
    stringValue(expected.baseSHA, "expected.baseSHA", problems, {
      pattern: SHA_RE,
    });
    validateChangedPaths(
      expected.changedPaths,
      "expected.changedPaths",
      problems,
    );
    if (
      expected.eventIdentity?.kind !== "pull_request" &&
      expected.eventIdentity?.kind !== "merge_group"
    ) {
      problems.push(
        "merge qualification event must be pull_request or merge_group",
      );
    }
    if (
      (expected.eventIdentity?.kind === "pull_request" ||
        expected.eventIdentity?.kind === "merge_group") &&
      expected.eventIdentity.event_base_sha !== expected.baseSHA
    ) {
      problems.push(
        "expected.eventIdentity.event_base_sha must equal expected.baseSHA for merge qualification",
      );
    }
  }

  const selectedIDs = new Set(
    Array.isArray(selection?.lanes)
      ? selection.lanes
          .filter((item) => item?.disposition === "selected")
          .map((item) => item.lane_id)
      : [],
  );
  const selectedArtifactLane = registry.lanes.some(
    (lane) => selectedIDs.has(lane.id) && lane.applicability.artifact,
  );
  if (selectedArtifactLane && expected.candidateDigest == null) {
    problems.push(
      "expected.candidateDigest is required for selected artifact evidence",
    );
  }

  if (problems.length > 0) {
    throw new ContractError(
      "invalid CI lane qualification expectations",
      problems,
    );
  }
  return expected;
}

export function validateReportSet({
  registry,
  selection,
  reports,
  producerManifest,
  expected = {},
}) {
  validateQualificationExpectations(expected, registry, selection);
  validateSelection(selection, registry, expected);
  validateProducerManifest(producerManifest, registry);
  const problems = [];
  const computedSelectionDigest = selectionManifestSHA256(selection);
  if (computedSelectionDigest !== expected.selectionManifestSHA256) {
    problems.push(
      "selection manifest SHA-256 does not match the trusted expected digest",
    );
  }
  const selectionRef = {
    run: selection.run,
    manifest_sha256: expected.selectionManifestSHA256,
  };
  if (
    producerManifest.selection_ref.run.id !== selectionRef.run.id ||
    producerManifest.selection_ref.run.attempt !== selectionRef.run.attempt ||
    producerManifest.selection_ref.manifest_sha256 !==
      selectionRef.manifest_sha256
  ) {
    problems.push(
      "trusted producer manifest selection_ref does not match the current selection",
    );
  }
  if (
    producerManifest.correlation_nonce !== selection.correlation_nonce ||
    producerManifest.correlation_nonce !== expected.correlationNonce
  ) {
    problems.push(
      "trusted producer manifest correlation_nonce does not match the current qualification",
    );
  }

  const lanesByID = new Map(registry.lanes.map((lane) => [lane.id, lane]));
  const expectedReports = new Map();
  const expectedWorkflows = new Set();
  for (const item of selection.lanes) {
    if (item.disposition !== "selected") continue;
    expectedWorkflows.add(lanesByID.get(item.lane_id).owner.workflow);
    for (const execution of item.executions) {
      expectedReports.set(`${item.lane_id}/${execution}`, {
        lane: lanesByID.get(item.lane_id),
        execution,
      });
    }
  }

  const trustedByWorkflow = new Map();
  for (const producer of producerManifest.producers) {
    trustedByWorkflow.set(producer.workflow, producer);
    if (!expectedWorkflows.has(producer.workflow)) {
      problems.push(
        `unexpected trusted producer workflow ${producer.workflow}; no selected lane owns it`,
      );
    }
    if (
      producer.source.sha !== selection.source.sha ||
      producer.source.tree !== selection.source.tree
    ) {
      problems.push(
        `${producer.workflow}: trusted producer projected SHA/tree does not match the selection`,
      );
    }
    if (
      EVENT_IDENTITY_KEYS.size !== Object.keys(producer.event).length ||
      [...EVENT_IDENTITY_KEYS].some(
        (key) => producer.event[key] !== expected.eventIdentity[key],
      )
    ) {
      problems.push(
        `${producer.workflow}: trusted producer event identity does not match the qualification event`,
      );
    }
    if (producer.repository !== expected.repository) {
      problems.push(
        `${producer.workflow}: trusted producer repository does not match the qualification repository`,
      );
    }
    // Workflow-level conclusion is telemetry, not a lane verdict. A workflow
    // may contain an unselected red job alongside a selected green lane; only
    // the selected context and its native entry decide qualification.
  }
  for (const workflow of expectedWorkflows) {
    if (!trustedByWorkflow.has(workflow)) {
      problems.push(`trusted producer run for ${workflow} is missing`);
    }
  }

  const actual = new Map();
  const claimedEntries = new Set();
  for (let i = 0; i < reports.length; i += 1) {
    let report;
    try {
      report = validateReport(reports[i], registry);
    } catch (error) {
      if (error instanceof ContractError) {
        problems.push(
          ...error.problems.map((problem) => `reports[${i}]: ${problem}`),
        );
        continue;
      }
      throw error;
    }
    const identity = reportIdentity(report);
    if (actual.has(identity)) {
      problems.push(`duplicate report ${identity}`);
      continue;
    }
    actual.set(identity, report);
    if (!expectedReports.has(identity)) {
      problems.push(
        `unexpected report ${identity}; the selection did not dispatch it`,
      );
      continue;
    }
    const lane = lanesByID.get(report.lane_id);
    if (report.posture !== selection.posture) {
      problems.push(
        `${identity}: posture ${report.posture} does not match ${selection.posture}`,
      );
    }
    if (
      report.source.sha !== selection.source.sha ||
      report.source.tree !== selection.source.tree
    ) {
      problems.push(
        `${identity}: source SHA/tree does not match the selection`,
      );
    }
    if (
      report.selection_ref.run.id !== selectionRef.run.id ||
      report.selection_ref.run.attempt !== selectionRef.run.attempt ||
      report.selection_ref.manifest_sha256 !== selectionRef.manifest_sha256
    ) {
      problems.push(
        `${identity}: selection_ref does not match the current selection`,
      );
    }
    if (report.correlation_nonce !== selection.correlation_nonce) {
      problems.push(
        `${identity}: correlation_nonce does not match the current qualification`,
      );
    }
    const mode = expectedInvocationMode(lane, selection.posture);
    if (report.invocation.mode !== mode) {
      problems.push(
        `${identity}: invocation mode ${report.invocation.mode} must be ${mode}`,
      );
    }
    if (
      mode === "candidate_artifact" &&
      report.candidate_digest !== selection.candidate_digest
    ) {
      problems.push(
        `${identity}: candidate digest does not match the selection`,
      );
    }
    if (mode === "source_tree" && report.candidate_digest !== null) {
      problems.push(
        `${identity}: source-tree evidence must not bind an artifact digest`,
      );
    }
    if (report.conclusion !== "success") {
      problems.push(
        `${identity}: conclusion ${report.conclusion} is not success`,
      );
    }
    if (report.evidence.executed <= 0) {
      problems.push(`${identity}: zero tests/checks executed`);
    }
    if (
      report.evidence.failed !== 0 ||
      report.evidence.skipped !== 0 ||
      report.evidence.passed !== report.evidence.executed
    ) {
      problems.push(
        `${identity}: evidence is not an all-executed, all-passed result`,
      );
    }
    if (lane.determinism === "seeded" && report.evidence.seed === null) {
      problems.push(`${identity}: seeded lane omitted its seed`);
    }

    const trusted = trustedByWorkflow.get(report.producer.workflow);
    if (!trusted) continue;
    if (
      report.producer.run.id !== trusted.run.id ||
      report.producer.run.attempt !== trusted.run.attempt
    ) {
      problems.push(
        `${identity}: producer run identity does not match the trusted workflow run`,
      );
    }
    const context = trusted.jobs.find(
      (job) => job.job === report.producer.job,
    );
    if (!context) {
      problems.push(
        `${identity}: trusted context job ${report.producer.job} is missing`,
      );
    } else {
      const contextNameMatches =
        lane.context.match === "exact"
          ? context.name === lane.context.name
          : context.name.startsWith(lane.context.name);
      if (!contextNameMatches) {
        problems.push(
          `${identity}: trusted context check name ${context.name} does not match ${lane.context.name}`,
        );
      }
      if (context.conclusion !== "success") {
        problems.push(
          `${identity}: trusted context job ${report.producer.job} is ${context.conclusion}`,
        );
      }
    }
    const artifact = trusted.artifacts.find(
      (candidate) => candidate.id === report.producer.artifact.id,
    );
    if (!artifact) {
      problems.push(
        `${identity}: trusted artifact ${report.producer.artifact.id} is missing`,
      );
    } else if (
      artifact.name !== report.producer.artifact.name ||
      artifact.sha256 !== report.producer.artifact.sha256
    ) {
      problems.push(
        `${identity}: producer artifact name or SHA-256 does not match trusted API provenance`,
      );
    } else {
      const entry = artifact.entries.find(
        (candidate) =>
          candidate.lane_id === report.lane_id &&
          candidate.execution_id === report.execution_id,
      );
      if (!entry) {
        problems.push(
          `${identity}: trusted native bundle entry is missing`,
        );
      } else if (
        entry.sha256 !== report.producer.artifact.entry_sha256
      ) {
        problems.push(
          `${identity}: native bundle entry SHA-256 does not match trusted provenance`,
        );
      }
    }
    const entryIdentity =
      `${report.producer.workflow}#${report.producer.run.id}#` +
      `${report.producer.run.attempt}#${report.producer.artifact.id}#` +
      `${report.producer.artifact.entry}`;
    if (claimedEntries.has(entryIdentity)) {
      problems.push(`${identity}: native bundle entry is reused by another report`);
    }
    claimedEntries.add(entryIdentity);
  }

  for (const identity of expectedReports.keys()) {
    if (!actual.has(identity)) problems.push(`missing report ${identity}`);
  }
  for (const workflow of expectedWorkflows) {
    const producer = trustedByWorkflow.get(workflow);
    if (producer?.artifacts.length !== 1) continue;
    const artifact = producer.artifacts[0];
    const consumed = [...claimedEntries].some((identity) =>
      identity.startsWith(
        `${workflow}#${producer.run.id}#${producer.run.attempt}#${artifact.id}#`,
      ),
    );
    if (!consumed) {
      problems.push(
        `${workflow}: trusted native artifact ${artifact.name} was not consumed by a selected report`,
      );
    }
  }
  if (problems.length > 0)
    throw new ContractError("CI lane qualification failed closed", problems);
  return { expected: expectedReports.size, received: actual.size };
}

function readReports(dir) {
  const reports = [];
  const walk = (current) => {
    for (const entry of readdirSync(current, { withFileTypes: true })) {
      const path = join(current, entry.name);
      if (entry.isDirectory()) walk(path);
      else if (entry.isFile() && entry.name.endsWith(".json")) {
        reports.push({ path, document: parseJSONFile(path, "CI lane report") });
      }
    }
  };
  if (!existsSync(dir) || !statSync(dir).isDirectory()) {
    throw new ContractError("CI lane reports", [
      `report directory does not exist: ${dir}`,
    ]);
  }
  walk(dir);
  reports.sort((a, b) => a.path.localeCompare(b.path));
  return reports;
}

export function renderSummary(registry, result = null) {
  const protectedCount = registry.lanes.filter(
    (lane) => lane.context.protected,
  ).length;
  const releaseRequired = registry.lanes.filter(
    (lane) => lane.release_posture === "required",
  ).length;
  const lines = [
    "## CI lane contract",
    "",
    `- registry schema: **v${registry.schema_version}** (${registry.rollout})`,
    `- logical lanes: **${registry.lanes.length}**`,
    `- protected contexts represented: **${protectedCount}**`,
    `- release-required lanes represented: **${releaseRequired}**`,
    `- canonical test layers represented: **${registry.layers.length}**`,
  ];
  if (result) {
    lines.push(
      `- qualified reports: **${result.received}/${result.expected}**`,
    );
  }
  return `${lines.join("\n")}\n`;
}

function appendSummary(body) {
  const path = process.env.GITHUB_STEP_SUMMARY;
  if (path) appendFileSync(path, body);
}

function errorAnnotation(message) {
  const oneLine = message
    .replaceAll("%", "%25")
    .replaceAll("\r", "%0D")
    .replaceAll("\n", "%0A");
  process.stderr.write(`::error title=CI lane contract::${oneLine}\n`);
}

export function parseExpectedRunAttempt(value) {
  if (value === undefined) return undefined;
  if (!/^[1-9][0-9]*$/.test(value)) {
    throw new ContractError("CI lane contract", [
      `CI_EXPECT_RUN_ATTEMPT must be a canonical positive integer; got ${JSON.stringify(value)}`,
    ]);
  }
  const attempt = Number(value);
  if (!Number.isSafeInteger(attempt)) {
    throw new ContractError("CI lane contract", [
      `CI_EXPECT_RUN_ATTEMPT exceeds JavaScript's safe integer range; got ${JSON.stringify(value)}`,
    ]);
  }
  return attempt;
}

export function qualificationExpectations(env = process.env) {
  const problems = [];
  const posture = env.CI_LANE_POSTURE;
  const sha = env.CI_EXPECT_SHA;
  const tree = env.CI_EXPECT_TREE;
  const correlationNonce = env.CI_EXPECT_CORRELATION_NONCE;
  const runID = env.CI_EXPECT_RUN_ID;
  const selectionManifestSHA256 = env.CI_EXPECT_SELECTION_SHA256;
  const eventKind = env.CI_EXPECT_EVENT_KIND;
  const eventHeadSHA = env.CI_EXPECT_EVENT_HEAD_SHA;
  const projectedSHA = env.CI_EXPECT_PROJECTED_SHA;
  const repository = env.CI_EXPECT_REPOSITORY;
  for (const [name, value] of [
    ["CI_LANE_POSTURE", posture],
    ["CI_EXPECT_SHA", sha],
    ["CI_EXPECT_TREE", tree],
    ["CI_EXPECT_CORRELATION_NONCE", correlationNonce],
    ["CI_EXPECT_RUN_ID", runID],
    ["CI_EXPECT_RUN_ATTEMPT", env.CI_EXPECT_RUN_ATTEMPT],
    ["CI_EXPECT_SELECTION_SHA256", selectionManifestSHA256],
    ["CI_EXPECT_EVENT_KIND", eventKind],
    ["CI_EXPECT_EVENT_HEAD_SHA", eventHeadSHA],
    ["CI_EXPECT_PROJECTED_SHA", projectedSHA],
    ["CI_EXPECT_REPOSITORY", repository],
  ]) {
    if (typeof value !== "string" || value === "")
      problems.push(`${name} is required`);
  }
  enumValue(posture, SELECTION_POSTURES, "CI_LANE_POSTURE", problems);
  stringValue(sha, "CI_EXPECT_SHA", problems, { pattern: SHA_RE });
  stringValue(tree, "CI_EXPECT_TREE", problems, { pattern: SHA_RE });
  stringValue(
    correlationNonce,
    "CI_EXPECT_CORRELATION_NONCE",
    problems,
    { pattern: CORRELATION_NONCE_RE },
  );
  stringValue(runID, "CI_EXPECT_RUN_ID", problems, { pattern: RUN_ID_RE });
  stringValue(
    selectionManifestSHA256,
    "CI_EXPECT_SELECTION_SHA256",
    problems,
    { pattern: SHA256_RE },
  );
  stringValue(repository, "CI_EXPECT_REPOSITORY", problems, {
    pattern: REPOSITORY_RE,
  });

  let runAttempt;
  if (env.CI_EXPECT_RUN_ATTEMPT !== undefined) {
    try {
      runAttempt = parseExpectedRunAttempt(env.CI_EXPECT_RUN_ATTEMPT);
    } catch (error) {
      if (error instanceof ContractError) problems.push(...error.problems);
      else throw error;
    }
  }

  let candidateDigest;
  if (
    env.CI_EXPECT_CANDIDATE_DIGEST !== undefined &&
    env.CI_EXPECT_CANDIDATE_DIGEST !== ""
  ) {
    candidateDigest = env.CI_EXPECT_CANDIDATE_DIGEST;
    validateDigest(candidateDigest, "CI_EXPECT_CANDIDATE_DIGEST", problems);
  }

  let prNumber = null;
  if (env.CI_EXPECT_PR_NUMBER !== undefined && env.CI_EXPECT_PR_NUMBER !== "") {
    if (!/^[1-9][0-9]*$/.test(env.CI_EXPECT_PR_NUMBER)) {
      problems.push("CI_EXPECT_PR_NUMBER must be a canonical positive integer");
    } else {
      prNumber = Number(env.CI_EXPECT_PR_NUMBER);
      if (!Number.isSafeInteger(prNumber)) {
        problems.push("CI_EXPECT_PR_NUMBER exceeds JavaScript's safe integer range");
      }
    }
  }
  const eventIdentity = {
    kind: eventKind,
    pr_number: prNumber,
    event_head_sha: eventHeadSHA,
    event_base_sha: env.CI_EXPECT_EVENT_BASE_SHA || null,
    projected_sha: projectedSHA,
  };
  validateEventIdentity(eventIdentity, "CI event identity", problems);
  if (projectedSHA !== sha) {
    problems.push("CI_EXPECT_PROJECTED_SHA must equal CI_EXPECT_SHA");
  }
  if (posture === "release" && candidateDigest === undefined) {
    problems.push(
      "CI_EXPECT_CANDIDATE_DIGEST is required for release qualification",
    );
  }

  let baseSHA;
  let changedPaths;
  if (posture === "merge") {
    baseSHA = env.CI_EXPECT_BASE_SHA;
    if (typeof baseSHA !== "string" || baseSHA === "") {
      problems.push("CI_EXPECT_BASE_SHA is required for merge qualification");
    } else {
      stringValue(baseSHA, "CI_EXPECT_BASE_SHA", problems, { pattern: SHA_RE });
    }
    const rawChangedPaths = env.CI_EXPECT_CHANGED_PATHS_JSON;
    if (typeof rawChangedPaths !== "string" || rawChangedPaths === "") {
      problems.push(
        "CI_EXPECT_CHANGED_PATHS_JSON is required for merge qualification",
      );
    } else {
      try {
        changedPaths = JSON.parse(rawChangedPaths);
        if (JSON.stringify(changedPaths) !== rawChangedPaths) {
          problems.push(
            "CI_EXPECT_CHANGED_PATHS_JSON must use canonical compact JSON",
          );
        }
        validateChangedPaths(
          changedPaths,
          "CI_EXPECT_CHANGED_PATHS_JSON",
          problems,
        );
      } catch (error) {
        problems.push(
          `CI_EXPECT_CHANGED_PATHS_JSON is not valid JSON: ${error.message}`,
        );
      }
    }
  }

  if (problems.length > 0)
    throw new ContractError("CI lane contract", problems);
  return {
    posture,
    sha,
    tree,
    correlationNonce,
    candidateDigest,
    runID,
    runAttempt,
    selectionManifestSHA256,
    eventIdentity,
    repository,
    baseSHA,
    changedPaths,
  };
}

function main() {
  const root = process.cwd();
  const registryPath = process.env.CI_LANE_REGISTRY || DEFAULT_REGISTRY_PATH;
  const registry = loadRegistry(registryPath, { root });
  const mode = process.env.MODE || process.argv[2] || "registry";
  if (mode === "registry") {
    const summary = renderSummary(registry);
    process.stdout.write(
      `ci-lane-contract: ${registry.lanes.length} lanes across ${registry.layers.length} test layers are structurally valid\n`,
    );
    appendSummary(summary);
    return;
  }
  if (mode !== "reports") {
    throw new ContractError("CI lane contract", [
      `MODE must be registry or reports; got ${mode}`,
    ]);
  }

  const selectionPath = process.env.CI_LANE_SELECTION;
  const reportDir = process.env.CI_LANE_REPORT_DIR;
  const producerManifestPath = process.env.CI_LANE_PRODUCER_MANIFEST;
  if (!selectionPath)
    throw new ContractError("CI lane contract", [
      "CI_LANE_SELECTION is required",
    ]);
  if (!reportDir)
    throw new ContractError("CI lane contract", [
      "CI_LANE_REPORT_DIR is required",
    ]);
  if (!producerManifestPath) {
    throw new ContractError("CI lane contract", [
      "CI_LANE_PRODUCER_MANIFEST is required",
    ]);
  }
  const reportsWithPaths = readReports(resolve(root, reportDir));
  const result = validateReportSet({
    registry,
    selection: parseJSONFile(resolve(root, selectionPath), "CI lane selection"),
    reports: reportsWithPaths.map((entry) => entry.document),
    producerManifest: parseJSONFile(
      resolve(root, producerManifestPath),
      "trusted producer manifest",
    ),
    expected: qualificationExpectations(process.env),
  });
  process.stdout.write(
    `ci-lane-contract: qualified ${result.received} report(s)\n`,
  );
  appendSummary(renderSummary(registry, result));
}

const invokedDirectly =
  process.argv[1] &&
  fileURLToPath(import.meta.url) === resolve(process.argv[1]);
if (invokedDirectly) {
  try {
    main();
  } catch (error) {
    errorAnnotation(error instanceof Error ? error.message : String(error));
    process.exit(1);
  }
}
