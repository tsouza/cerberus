// ci-lane-contract.mjs — schema and structural-consistency contract for
// Cerberus's machine-readable test-fence registry (.github/ci-lanes.json).
//
// Validates that every lane's declared workflow, jobs, recipes, and package
// globs actually exist and agree with the repository, that every workflow
// file is either owned by a lane or classified as non-lane, and that each
// query head (PromQL/LogQL/TraceQL) keeps independent execution, property,
// and reference oracle coverage. `quickstart-canary.mjs` also consumes
// `loadRegistry`/`matchesGlob` directly to look up the quickstart lane's
// `package_globs` and the registry's `impact_selection.known_nonimpact_globs`
// for its own changed-path skip decision.
//
// Env:
//   CI_LANE_REGISTRY      registry path (default .github/ci-lanes.json)
//   GITHUB_STEP_SUMMARY   optional summary destination
//
// Node builtins only. Direct execution emits ::error:: annotations and exits
// non-zero on any schema, path, or cross-reference violation.

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

// The head packages a REFERENCE lane must name in its own `package_globs`
// (#2902). A reference lane drives the real HTTP head against the reference
// backend, so its query path starts at that head's API package and its query
// language, and `lib/lane-closure.mjs` derives everything downstream — the
// shared `chplan`/`optimizer`/`chsql`/`chclient`/`engine` pipeline — from
// exactly these two roots. That is why they are a contract rather than a
// convention: the closure is only as wide as the seeds it starts from, so a
// lane that stops naming its own head silently narrows back to the pre-#2902
// attribution, which is what let PR #2824 merge without `compatibility.loki`
// considering itself touched.
//
// EXECUTION lanes are deliberately outside this rule rather than exempted from
// it: a round-trip lane lowers and executes SQL without ever entering the HTTP
// head, so requiring the API package of one would assert a dependency it does
// not have.
export const CANONICAL_HEAD_QUERY_PATH_ROOTS = Object.freeze({
  promql: Object.freeze(["internal/promql", "internal/api/prom"]),
  logql: Object.freeze(["internal/logql", "internal/api/loki"]),
  traceql: Object.freeze(["internal/traceql", "internal/api/tempo"]),
});

const TOP_KEYS = new Set([
  "schema_version",
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
  "timeout_minutes",
  "slo_minutes",
  "accountable_owner",
]);
const OWNER_KEYS = new Set([
  "workflow",
  "jobs",
  "producer_jobs",
  "context_job",
]);
const CONTEXT_KEYS = new Set(["match", "name", "protected"]);
const APPLICABILITY_KEYS = new Set(["source", "artifact"]);
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

const LANE_ID_RE = /^[a-z][a-z0-9]*(?:[.-][a-z0-9]+)*$/;
const EXECUTION_ID_RE = /^[a-z0-9][a-z0-9._-]*$/;
const WORKFLOW_JOB_ID_RE = /^[A-Za-z_][A-Za-z0-9_-]*$/;
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
        if (oracleClass !== "reference") continue;
        for (const queryPathRoot of CANONICAL_HEAD_QUERY_PATH_ROOTS[head]) {
          if (providerHasCanonicalSource(provider, root, queryPathRoot)) continue;
          problems.push(
            `canonical head ${head} reference provider ${provider.id} does not cover ` +
              `${queryPathRoot} in package_globs, so the affected-path set derived from those ` +
              `globs never reaches the shared query pipeline this lane runs through (#2902)`,
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

export function renderSummary(registry) {
  const protectedCount = registry.lanes.filter(
    (lane) => lane.context.protected,
  ).length;
  const releaseRequired = registry.lanes.filter(
    (lane) => lane.release_posture === "required",
  ).length;
  const lines = [
    "## CI lane contract",
    "",
    `- registry schema: **v${registry.schema_version}**`,
    `- logical lanes: **${registry.lanes.length}**`,
    `- protected contexts represented: **${protectedCount}**`,
    `- release-required lanes represented: **${releaseRequired}**`,
    `- canonical test layers represented: **${registry.layers.length}**`,
  ];
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

function main() {
  const mode = process.env.MODE;
  if (mode !== undefined && mode !== "registry") {
    throw new ContractError("CI lane contract", [
      `MODE must be registry; got ${mode}`,
    ]);
  }
  const root = process.cwd();
  const registryPath = process.env.CI_LANE_REGISTRY || DEFAULT_REGISTRY_PATH;
  const registry = loadRegistry(registryPath, { root });
  const summary = renderSummary(registry);
  process.stdout.write(
    `ci-lane-contract: ${registry.lanes.length} lanes across ${registry.layers.length} test layers are structurally valid\n`,
  );
  appendSummary(summary);
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
