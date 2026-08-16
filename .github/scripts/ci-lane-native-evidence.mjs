// ci-lane-native-evidence.mjs — emit selection-independent, attempt-qualified
// evidence from a workflow's own `needs` graph.
//
// A workflow calls this once from an unconditional terminal evidence job. The
// job passes GitHub's `toJSON(needs)` value only to attest which registered
// jobs reached which terminal result. Executed/passed/failed/skipped counts are
// parsed from the registry's exact raw-part roster; a job result is never a
// test count.
//
// Required environment:
//   CI_LANE_WORKFLOW       canonical repository-relative workflow path.
//   CI_LANE_NEEDS_JSON     GitHub `toJSON(needs)` for the evidence job.
//   CI_LANE_NATIVE_PARTS   directory populated from attempt-qualified raw-part
//                          artifacts declared by registry.native_evidence.
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
  closeSync,
  constants,
  existsSync,
  fstatSync,
  linkSync,
  lstatSync,
  openSync,
  readFileSync,
  readdirSync,
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
  nativePartArtifactName,
  NATIVE_PART_PARSERS,
} from "./ci-lane-contract.mjs";
import { capture, error, notice, setOutput } from "./lib/gh.mjs";

export const NATIVE_EVIDENCE_SCHEMA_VERSION = 2;
export const MAX_NATIVE_PART_BYTES = 64 * 1024 * 1024;

const shaPattern = /^(?:[0-9a-f]{40}|[0-9a-f]{64})$/;
const runIDPattern = /^[1-9][0-9]*$/;
const repositoryPattern = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;
const workflowPattern = /^\.github\/workflows\/[A-Za-z0-9_.-]+\.ya?ml$/;
const eventPattern = /^[a-z][a-z0-9_]*$/;
const correlationNoncePattern = /^[0-9a-f]{64}$/;
const postures = new Set(["merge", "main", "release"]);
const nativeResults = new Set(["success", "failure", "cancelled", "skipped"]);
const TAP_DEFERRED_DIRECTIVE = "TO" + "DO";

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

const nativeSeedPattern = /^[^\u0000-\u001f\u007f]{1,1024}$/;

function evidenceCounts({ passed = 0, failed = 0, skipped = 0 } = {}) {
  for (const [name, value] of Object.entries({ passed, failed, skipped })) {
    if (!Number.isSafeInteger(value) || value < 0) {
      throw new ContractError("CI lane native evidence", [
        `${name} evidence count must be a non-negative safe integer`,
      ]);
    }
  }
  return Object.freeze({
    executed: passed + failed + skipped,
    passed,
    failed,
    skipped,
  });
}

function safeIntegerSum(values, label) {
  let total = 0;
  for (const value of values) {
    if (!Number.isSafeInteger(value) || value < 0) {
      throw new ContractError("CI lane native evidence", [
        `${label} values must be non-negative safe integers`,
      ]);
    }
    total += value;
    if (!Number.isSafeInteger(total)) {
      throw new ContractError("CI lane native evidence", [
        `${label} total exceeds the safe integer range`,
      ]);
    }
  }
  return total;
}

function milliseconds(value, label, { seconds = false } = {}) {
  if (typeof value !== "number" || !Number.isFinite(value) || value < 0) {
    throw new ContractError("CI lane native evidence", [
      `${label} must be a non-negative finite number`,
    ]);
  }
  const duration = Math.round(seconds ? value * 1000 : value);
  if (!Number.isSafeInteger(duration)) {
    throw new ContractError("CI lane native evidence", [
      `${label} exceeds the safe integer range when converted to milliseconds`,
    ]);
  }
  return duration;
}

function nativeSeed(value, label) {
  if (value === null || value === undefined || value === "") return null;
  if (typeof value !== "string" || !nativeSeedPattern.test(value)) {
    throw new ContractError("CI lane native evidence", [
      `${label} must be null or a non-empty printable string no longer than 1024 bytes`,
    ]);
  }
  return value;
}

function uniqueSeed(values, label) {
  const seeds = values.filter((value) => value !== null);
  const unique = [...new Set(seeds)];
  if (unique.length > 1) {
    throw new ContractError("CI lane native evidence", [
      `${label} reports conflicting native seeds`,
    ]);
  }
  return unique[0] ?? null;
}

function corpusIdentity(parser, identities) {
  if (!Array.isArray(identities) || identities.length === 0) {
    throw new ContractError("CI lane native evidence", [
      `${parser} contains no native corpus identities`,
    ]);
  }
  for (const [index, identity] of identities.entries()) {
    if (
      typeof identity !== "string" ||
      identity === "" ||
      identity.includes("\u0000")
    ) {
      throw new ContractError("CI lane native evidence", [
        `${parser} corpus identity ${index} is invalid`,
      ]);
    }
  }
  return `sha256:${canonicalJSONSHA256({
    parser,
    identities: [...identities].sort(),
  })}`;
}

function parsedEvidence({ parser, outcomes, identities, durationMs, seed }) {
  const counts = evidenceCounts({
    passed: outcomes.filter((value) => value === "pass").length,
    failed: outcomes.filter((value) => value === "fail").length,
    skipped: outcomes.filter((value) => value === "skip").length,
  });
  return Object.freeze({
    ...counts,
    duration_ms: milliseconds(durationMs, `${parser} duration_ms`),
    seed: nativeSeed(seed, `${parser} seed`),
    corpus_id: corpusIdentity(parser, identities),
  });
}

function aggregateEvidence(parts, lane) {
  const counts = evidenceCounts({
    passed: safeIntegerSum(
      parts.map((part) => part.evidence.passed),
      `${lane.id} passed evidence`,
    ),
    failed: safeIntegerSum(
      parts.map((part) => part.evidence.failed),
      `${lane.id} failed evidence`,
    ),
    skipped: safeIntegerSum(
      parts.map((part) => part.evidence.skipped),
      `${lane.id} skipped evidence`,
    ),
  });
  const seeds = parts.map((part) => part.evidence.seed);
  if (
    lane.determinism === "deterministic" &&
    seeds.some((seed) => seed !== null)
  ) {
    throw new ContractError("CI lane native evidence", [
      `${lane.id} is deterministic but a native part reports a seed`,
    ]);
  }
  if (
    lane.determinism === "seeded" &&
    seeds.some((seed) => seed === null)
  ) {
    throw new ContractError("CI lane native evidence", [
      `${lane.id} is seeded but a native part omitted its seed`,
    ]);
  }
  const seed = uniqueSeed(seeds, lane.id);
  return Object.freeze({
    ...counts,
    duration_ms: safeIntegerSum(
      parts.map((part) => part.evidence.duration_ms),
      `${lane.id} duration evidence`,
    ),
    seed,
    corpus_id: corpusIdentity(
      "lane-native-parts-v1",
      parts.map(
        (part) =>
          JSON.stringify([part.id, part.parser, part.evidence.corpus_id]),
      ),
    ),
  });
}

function parseJSON(text, label) {
  try {
    return JSON.parse(text);
  } catch (parseError) {
    throw new ContractError("CI lane native evidence", [
      `${label} is not valid JSON: ${parseError.message}`,
    ]);
  }
}

function exactKeys(value, keys, label) {
  if (value === null || Array.isArray(value) || typeof value !== "object") {
    throw new ContractError("CI lane native evidence", [
      `${label} must be an object`,
    ]);
  }
  const actual = Object.keys(value).sort();
  const expected = [...keys].sort();
  if (JSON.stringify(actual) !== JSON.stringify(expected)) {
    throw new ContractError("CI lane native evidence", [
      `${label} keys must be exactly ${expected.join(", ")}`,
    ]);
  }
}

export function parseGoTestJSON(text) {
  const terminal = new Map();
  const packageTerminal = new Map();
  const nativeSeeds = [];
  const lines = String(text ?? "")
    .split(/\r?\n/)
    .filter((line) => line.trim() !== "");
  for (let index = 0; index < lines.length; index += 1) {
    const event = parseJSON(lines[index], `go-test-json line ${index + 1}`);
    if (typeof event?.Action !== "string") {
      throw new ContractError("CI lane native evidence", [
        `go-test-json line ${index + 1} has no Action`,
      ]);
    }
    if (typeof event.Output === "string") {
      const seedMatch = /^CI_NATIVE_SEED=(.+)\r?\n?$/.exec(event.Output);
      if (seedMatch) nativeSeeds.push(nativeSeed(seedMatch[1], "go-test-json seed"));
    }
    if (!new Set(["pass", "fail", "skip"]).has(event.Action)) continue;
    const packageName = String(event.Package ?? "");
    if (typeof event.Test !== "string" || event.Test === "") {
      if (packageName === "" || packageTerminal.has(packageName)) {
        throw new ContractError("CI lane native evidence", [
          packageName === ""
            ? `go-test-json line ${index + 1} has a package terminal with no Package`
            : `go-test-json repeats package terminal result for ${packageName}`,
        ]);
      }
      packageTerminal.set(packageName, {
        action: event.Action,
        duration_ms: milliseconds(
          event.Elapsed,
          `go-test-json package ${packageName} Elapsed`,
          { seconds: true },
        ),
      });
      continue;
    }
    if (packageName === "") {
      throw new ContractError("CI lane native evidence", [
        `go-test-json terminal result for ${event.Test} has no Package`,
      ]);
    }
    const key = `${packageName}\u0000${event.Test}`;
    if (terminal.has(key)) {
      throw new ContractError("CI lane native evidence", [
        `go-test-json repeats terminal result for ${packageName}/${event.Test}`,
      ]);
    }
    terminal.set(key, event.Action);
  }
  const testPackages = new Set(
    [...terminal.keys()].map((key) => key.split("\u0000", 1)[0]),
  );
  const missingPackageTerminals = [...testPackages].filter(
    (packageName) => !packageTerminal.has(packageName),
  );
  if (missingPackageTerminals.length > 0) {
    throw new ContractError("CI lane native evidence", [
      `go-test-json is missing package terminal result(s): ${missingPackageTerminals.sort().join(", ")}`,
    ]);
  }
  return parsedEvidence({
    parser: "go-test-json-v1",
    outcomes: [...terminal.values()],
    identities: [...terminal.keys()].map((key) =>
      JSON.stringify(key.split("\u0000")),
    ),
    durationMs: safeIntegerSum(
      [...testPackages].map(
        (packageName) => packageTerminal.get(packageName).duration_ms,
      ),
      "go-test-json package durations",
    ),
    seed: uniqueSeed(nativeSeeds, "go-test-json"),
  });
}

export function parseNodeTAP(text) {
  const outcomes = [];
  const identities = [];
  const plans = [];
  const durations = [];
  const nativeSeeds = [];
  for (const raw of String(text ?? "").split(/\r?\n/)) {
    if (/^1\.\.[0-9]+(?:\s|$)/.test(raw)) {
      plans.push(Number(raw.match(/^1\.\.([0-9]+)/)[1]));
      continue;
    }
    const durationMatch = /^#\s*duration_ms(?::|\s)\s*([0-9]+(?:\.[0-9]+)?)\s*$/i.exec(
      raw,
    );
    if (durationMatch) {
      durations.push(
        milliseconds(
          Number(durationMatch[1]),
          "node TAP duration_ms summary",
        ),
      );
      continue;
    }
    const seedMatch = /^#\s*ci-native-seed:\s*(.+)\s*$/i.exec(raw);
    if (seedMatch) {
      nativeSeeds.push(nativeSeed(seedMatch[1], "node TAP seed"));
      continue;
    }
    const directiveIndex = raw.search(/\s+#\s*(?:SKIP|TO[D]O)\b/i);
    const assertion = directiveIndex < 0 ? raw : raw.slice(0, directiveIndex);
    const directive =
      directiveIndex < 0
        ? ""
        : /(?:SKIP|TO[D]O)/i.exec(raw.slice(directiveIndex))[0].toUpperCase();
    const match = /^(ok|not ok)\s+([1-9][0-9]*)(?:\s+-\s+(.+?))?\s*$/i.exec(
      assertion,
    );
    if (!match) continue;
    const ordinal = Number(match[2]);
    if (ordinal !== outcomes.length + 1) {
      throw new ContractError("CI lane native evidence", [
        `node TAP result ordinal ${ordinal} is not the expected ${outcomes.length + 1}`,
      ]);
    }
    const name = String(match[3] ?? "").trim();
    outcomes.push(
      directive === "SKIP" || directive === TAP_DEFERRED_DIRECTIVE
        ? "skip"
        : match[1].toLowerCase() === "ok"
          ? "pass"
          : "fail",
    );
    identities.push(JSON.stringify([ordinal, name]));
  }
  if (plans.length !== 1 || plans[0] !== outcomes.length) {
    throw new ContractError("CI lane native evidence", [
      `node TAP top-level plan must exactly match its ${outcomes.length} result(s)`,
    ]);
  }
  if (durations.length !== 1) {
    throw new ContractError("CI lane native evidence", [
      `node TAP must contain exactly one top-level duration_ms summary; found ${durations.length}`,
    ]);
  }
  return parsedEvidence({
    parser: "node-tap-v1",
    outcomes,
    identities,
    durationMs: durations[0],
    seed: uniqueSeed(nativeSeeds, "node TAP"),
  });
}

function xmlAttribute(opening, name, label, { required = true } = {}) {
  const escaped = name.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const match = new RegExp(`\\s${escaped}\\s*=\\s*(["'])([\\s\\S]*?)\\1`, "i").exec(
    opening,
  );
  if (!match) {
    if (!required) return null;
    throw new ContractError("CI lane native evidence", [
      `${label} is missing XML attribute ${name}`,
    ]);
  }
  return match[2]
    .replace(/&quot;/g, '"')
    .replace(/&apos;/g, "'")
    .replace(/&lt;/g, "<")
    .replace(/&gt;/g, ">")
    .replace(/&amp;/g, "&");
}

export function parseJUnitXML(text) {
  const xml = String(text ?? "");
  if (!/<testsuites?\b/i.test(xml)) {
    throw new ContractError("CI lane native evidence", [
      "JUnit has no testsuite root",
    ]);
  }
  const cases =
    xml.match(
      /<testcase\b[^>]*\/\s*>|<testcase\b[^>]*>[\s\S]*?<\/testcase\s*>/gi,
    ) ?? [];
  const openings = xml.match(/<testcase\b/gi) ?? [];
  if (cases.length !== openings.length) {
    throw new ContractError("CI lane native evidence", [
      "JUnit contains an unterminated testcase",
    ]);
  }
  const outcomes = [];
  const identities = [];
  const durations = [];
  for (const [index, testcase] of cases.entries()) {
    const opening = /^<testcase\b[^>]*>/i.exec(testcase)?.[0];
    if (!opening) {
      throw new ContractError("CI lane native evidence", [
        `JUnit testcase ${index} has no opening element`,
      ]);
    }
    const name = xmlAttribute(opening, "name", `JUnit testcase ${index}`);
    const className =
      xmlAttribute(opening, "classname", `JUnit testcase ${index}`, {
        required: false,
      }) ?? "";
    const file =
      xmlAttribute(opening, "file", `JUnit testcase ${index}`, {
        required: false,
      }) ?? "";
    const seconds = Number(
      xmlAttribute(opening, "time", `JUnit testcase ${index}`),
    );
    durations.push(
      milliseconds(seconds, `JUnit testcase ${index} time`, { seconds: true }),
    );
    identities.push(JSON.stringify([className, file, name]));
    if (/<(?:failure|error)\b/i.test(testcase)) outcomes.push("fail");
    else if (/<skipped\b/i.test(testcase)) outcomes.push("skip");
    else outcomes.push("pass");
  }
  const nativeSeeds = [];
  for (const property of xml.match(/<property\b[^>]*\/?\s*>/gi) ?? []) {
    if (
      xmlAttribute(property, "name", "JUnit property", { required: false }) ===
      "ci-native-seed"
    ) {
      nativeSeeds.push(
        nativeSeed(
          xmlAttribute(property, "value", "JUnit ci-native-seed property"),
          "JUnit seed",
        ),
      );
    }
  }
  return parsedEvidence({
    parser: "junit-xml-v1",
    outcomes,
    identities,
    durationMs: safeIntegerSum(durations, "JUnit testcase durations"),
    seed: uniqueSeed(nativeSeeds, "JUnit"),
  });
}

export function parseCaseJSON(text) {
  const document = parseJSON(String(text ?? ""), "case-json-v1");
  exactKeys(document, ["schema_version", "seed", "cases"], "case-json-v1");
  if (document.schema_version !== 1 || !Array.isArray(document.cases)) {
    throw new ContractError("CI lane native evidence", [
      "case-json-v1 schema_version must be 1 and cases must be an array",
    ]);
  }
  const seen = new Set();
  const durations = [];
  const outcomes = document.cases.map((item, index) => {
    exactKeys(
      item,
      ["id", "status", "duration_ms"],
      `case-json-v1 cases[${index}]`,
    );
    if (typeof item.id !== "string" || item.id.trim() === "" || seen.has(item.id)) {
      throw new ContractError("CI lane native evidence", [
        `case-json-v1 cases[${index}].id must be unique and non-empty`,
      ]);
    }
    seen.add(item.id);
    if (!new Set(["passed", "failed", "skipped"]).has(item.status)) {
      throw new ContractError("CI lane native evidence", [
        `case-json-v1 cases[${index}].status is invalid`,
      ]);
    }
    durations.push(
      milliseconds(
        item.duration_ms,
        `case-json-v1 cases[${index}].duration_ms`,
      ),
    );
    return { passed: "pass", failed: "fail", skipped: "skip" }[item.status];
  });
  return parsedEvidence({
    parser: "case-json-v1",
    outcomes,
    identities: [...seen],
    durationMs: safeIntegerSum(durations, "case-json-v1 case durations"),
    seed: nativeSeed(document.seed, "case-json-v1 seed"),
  });
}

export function parseCompatCasesJSON(text) {
  const document = parseJSON(String(text ?? ""), "compat-cases-json-v1");
  exactKeys(
    document,
    ["schema_version", "head", "seed", "cases"],
    "compat-cases-json-v1",
  );
  if (
    document.schema_version !== 1 ||
    typeof document.head !== "string" ||
    document.head.trim() === "" ||
    !Array.isArray(document.cases)
  ) {
    throw new ContractError("CI lane native evidence", [
      "compat-cases-json-v1 requires schema_version 1, a non-empty head, and a cases array",
    ]);
  }
  const seen = new Set();
  const outcomes = [];
  const durations = [];
  for (let index = 0; index < document.cases.length; index += 1) {
    const item = document.cases[index];
    exactKeys(
      item,
      ["id", "passed", "duration_ms"],
      `compat-cases-json-v1 cases[${index}]`,
    );
    if (
      typeof item.id !== "string" ||
      item.id.trim() === "" ||
      seen.has(item.id) ||
      typeof item.passed !== "boolean"
    ) {
      throw new ContractError("CI lane native evidence", [
        `compat-cases-json-v1 cases[${index}] must have a unique id and boolean passed`,
      ]);
    }
    seen.add(item.id);
    outcomes.push(item.passed ? "pass" : "fail");
    durations.push(
      milliseconds(
        item.duration_ms,
        `compat-cases-json-v1 cases[${index}].duration_ms`,
      ),
    );
  }
  return parsedEvidence({
    parser: "compat-cases-json-v1",
    outcomes,
    identities: [...seen].map((id) => JSON.stringify([document.head, id])),
    durationMs: safeIntegerSum(
      durations,
      "compat-cases-json-v1 case durations",
    ),
    seed: nativeSeed(document.seed, "compat-cases-json-v1 seed"),
  });
}

export function parseNativePart(parser, text) {
  if (!NATIVE_PART_PARSERS.includes(parser)) {
    throw new ContractError("CI lane native evidence", [
      `native part parser ${JSON.stringify(parser)} is not registered`,
    ]);
  }
  if (parser === "go-test-json-v1") return parseGoTestJSON(text);
  if (parser === "node-tap-v1") return parseNodeTAP(text);
  if (parser === "junit-xml-v1") return parseJUnitXML(text);
  if (parser === "case-json-v1") return parseCaseJSON(text);
  return parseCompatCasesJSON(text);
}

function laneConclusion(jobResults, evidence) {
  const results = jobResults.map((job) => job.result);
  if (results.includes("failure") || evidence.failed > 0) return "failure";
  if (results.includes("cancelled")) return "cancelled";
  if (results.includes("skipped") || evidence.skipped > 0) return "skipped";
  return "success";
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

function nativeInvocationModes(lane, posture) {
  const modes = [];
  if (lane.applicability?.source) modes.push("source_tree");
  if (posture === "release" && lane.applicability?.artifact) {
    modes.push("candidate_artifact");
  }
  return modes;
}

function walkPartFiles(root, relative = "") {
  const directory = resolve(root, relative);
  const stat = lstatSync(directory);
  if (!stat.isDirectory() || stat.isSymbolicLink()) {
    throw new ContractError("CI lane native evidence", [
      `native part path is not a real directory: ${directory}`,
    ]);
  }
  const files = [];
  for (const name of readdirSync(directory).sort()) {
    const childRelative = relative === "" ? name : `${relative}/${name}`;
    const child = resolve(root, childRelative);
    const childStat = lstatSync(child);
    if (childStat.isSymbolicLink()) {
      throw new ContractError("CI lane native evidence", [
        `native part tree contains a symbolic link: ${childRelative}`,
      ]);
    }
    if (childStat.isDirectory()) files.push(...walkPartFiles(root, childRelative));
    else if (childStat.isFile()) files.push(childRelative);
    else {
      throw new ContractError("CI lane native evidence", [
        `native part tree contains a non-file entry: ${childRelative}`,
      ]);
    }
  }
  return files;
}

function expectedNativeParts(registry, lanes, runAttempt, posture) {
  const identities = new Set(
    lanes.flatMap((lane) =>
      lane.executions.flatMap((executionID) =>
        nativeInvocationModes(lane, posture).map(
          (invocationMode) =>
            `${lane.id}/${executionID}/${invocationMode}`,
        ),
      ),
    ),
  );
  const parts = (registry.native_evidence?.parts ?? [])
    .filter((part) =>
      identities.has(
        `${part.lane_id}/${part.execution_id}/${part.invocation_mode}`,
      ),
    )
    .map((part) => ({
      ...part,
      artifact: nativePartArtifactName(part.id, runAttempt),
    }))
    .sort((left, right) =>
      `${left.lane_id}/${left.execution_id}/${left.invocation_mode}/${left.id}`.localeCompare(
        `${right.lane_id}/${right.execution_id}/${right.invocation_mode}/${right.id}`,
      ),
    );
  const covered = new Set(
    parts.map(
      (part) =>
        `${part.lane_id}/${part.execution_id}/${part.invocation_mode}`,
    ),
  );
  const missing = [...identities].filter((identity) => !covered.has(identity)).sort();
  if (missing.length > 0) {
    throw new ContractError("CI lane native evidence", [
      ...missing.map((identity) => `registry has no native part for ${identity}`),
    ]);
  }
  return parts;
}

export function readNativeParts({ registry, lanes, runAttempt, posture, partsRoot }) {
  const root = resolve(String(partsRoot ?? ""));
  if (!existsSync(root)) {
    throw new ContractError("CI lane native evidence", [
      `native part directory does not exist: ${root}`,
    ]);
  }
  const roster = expectedNativeParts(registry, lanes, runAttempt, posture);
  const expectedFiles = roster.map((part) => `${part.artifact}/${part.entry}`).sort();
  const actualFiles = walkPartFiles(root).sort();
  // download-artifact v8 preserves one directory per artifact when several
  // artifacts match, but extracts a single match directly into `path`. Both
  // layouts are closed and unambiguous: the flat form is accepted only when
  // the registry expects exactly one part, whose body still carries and is
  // validated against that sole registered identity.
  const nestedLayout =
    JSON.stringify(actualFiles) === JSON.stringify(expectedFiles);
  const flatLayout =
    roster.length === 1 &&
    JSON.stringify(actualFiles) === JSON.stringify([roster[0].entry]);
  if (!nestedLayout && !flatLayout) {
    throw new ContractError("CI lane native evidence", [
      `native part files must exactly match the registry roster; got ${JSON.stringify(actualFiles)}, want ${JSON.stringify(expectedFiles)}`,
    ]);
  }
  return roster.map((part) => {
    const path = nestedLayout
      ? resolve(root, part.artifact, part.entry)
      : resolve(root, part.entry);
    let descriptor;
    let body;
    try {
      descriptor = openSync(path, constants.O_RDONLY | constants.O_NOFOLLOW);
      const stat = fstatSync(descriptor);
      if (!stat.isFile() || stat.size > MAX_NATIVE_PART_BYTES) {
        throw new ContractError("CI lane native evidence", [
          `native part ${part.id} must be a regular file no larger than ${MAX_NATIVE_PART_BYTES} bytes`,
        ]);
      }
      body = readFileSync(descriptor);
    } catch (readError) {
      if (readError instanceof ContractError) throw readError;
      throw new ContractError("CI lane native evidence", [
        `native part ${part.id} cannot be opened safely: ${readError.message}`,
      ]);
    } finally {
      if (descriptor !== undefined) closeSync(descriptor);
    }
    const evidence = parseNativePart(part.parser, body.toString("utf8"));
    if (evidence.executed <= 0) {
      throw new ContractError("CI lane native evidence", [
        `native part ${part.id} executed zero tests/checks`,
      ]);
    }
    return Object.freeze({
      id: part.id,
      lane_id: part.lane_id,
      execution_id: part.execution_id,
      invocation_mode: part.invocation_mode,
      producer_job: part.producer_job,
      parser: part.parser,
      artifact: part.artifact,
      entry: part.entry,
      sha256: createHash("sha256").update(body).digest("hex"),
      evidence,
    });
  });
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
  partsRoot,
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
  const knownWorkflowJobs = new Set(
    registry.lanes
      .filter((lane) => lane.owner.workflow === workflowPath)
      .flatMap((lane) => lane.owner.jobs),
  );
  const actualJobs = [...needs.keys()].sort();
  const problems = [];
  for (const job of expectedJobs) {
    if (!needs.has(job)) problems.push(`native needs is missing registered job ${job}`);
  }
  for (const job of actualJobs) {
    if (!knownWorkflowJobs.has(job)) {
      problems.push(`native needs contains unregistered extra job ${job}`);
    }
  }
  if (problems.length > 0) {
    throw new ContractError("CI lane native evidence", problems);
  }

  const parsedParts = readNativeParts({
    registry,
    lanes,
    runAttempt: attempt,
    posture: postureName,
    partsRoot,
  });

  const laneEvidence = lanes
    .flatMap((lane) =>
      lane.executions.flatMap((executionID) =>
        nativeInvocationModes(lane, postureName).map((invocationMode) => {
          const jobResults = lane.owner.jobs
            .filter((jobID) => jobID !== evidenceJobID)
            .map((jobID) => ({
              job_id: jobID,
              result: needs.get(jobID),
            }));
          const parts = parsedParts
            .filter(
              (part) =>
                part.lane_id === lane.id &&
                part.execution_id === executionID &&
                part.invocation_mode === invocationMode,
            )
            .map((part) => ({
              id: part.id,
              producer_job: part.producer_job,
              parser: part.parser,
              artifact: part.artifact,
              entry: part.entry,
              sha256: part.sha256,
              evidence: part.evidence,
            }));
          const evidence = aggregateEvidence(parts, lane);
          return {
            lane_id: lane.id,
            execution_id: executionID,
            invocation_mode: invocationMode,
            context: {
              match: lane.context.match,
              name: lane.context.name,
              job_id: lane.owner.context_job,
            },
            job_results: jobResults,
            parts,
            evidence,
            conclusion: laneConclusion(jobResults, evidence),
          };
        }),
      ),
    )
    .sort((left, right) =>
      `${left.lane_id}/${left.execution_id}/${left.invocation_mode}`.localeCompare(
        `${right.lane_id}/${right.execution_id}/${right.invocation_mode}`,
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
      lane.executions.flatMap((executionID) =>
        nativeInvocationModes(lane, document.posture).map(
          (invocationMode) => [
            `${lane.id}/${executionID}/${invocationMode}`,
            lane,
          ],
        ),
      ),
    ),
  );
  let expectedParts = [];
  try {
    expectedParts = expectedNativeParts(
      registry,
      lanes,
      document.producer?.run?.attempt,
      document.posture,
    );
  } catch (partError) {
    problems.push(
      ...(partError instanceof ContractError
        ? partError.problems
        : [String(partError)]),
    );
  }
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
          ["lane_id", "execution_id", "invocation_mode", "context", "job_results", "parts", "evidence", "conclusion"],
          at,
        )
      ) continue;
      const identity =
        `${entry.lane_id}/${entry.execution_id}/${entry.invocation_mode}`;
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
      const resultJobs = Array.isArray(entry.job_results)
        ? entry.job_results.map((job) => job?.job_id)
        : [];
      if (JSON.stringify(resultJobs) !== JSON.stringify(expectedJobs))
        problems.push(`${at} job_results do not match the registry job roster`);
      if (!Array.isArray(entry.job_results)) {
        problems.push(`${at} job_results must be an array`);
        continue;
      }
      for (const job of entry.job_results) {
        if (!exact(job, ["job_id", "result"], `${at} job result`)) continue;
        if (!nativeResults.has(job.result)) problems.push(`${at} job result is invalid`);
      }
      const wantedParts = expectedParts.filter(
        (part) =>
          part.lane_id === entry.lane_id &&
          part.execution_id === entry.execution_id &&
          part.invocation_mode === entry.invocation_mode,
      );
      const wantedPartIDs = wantedParts.map((part) => part.id);
      const actualPartIDs = Array.isArray(entry.parts)
        ? entry.parts.map((part) => part?.id)
        : [];
      if (JSON.stringify(actualPartIDs) !== JSON.stringify(wantedPartIDs)) {
        problems.push(`${at} parts do not match the registry part roster`);
      }
      if (!Array.isArray(entry.parts)) {
        problems.push(`${at} parts must be an array`);
        continue;
      }
      for (let partIndex = 0; partIndex < entry.parts.length; partIndex += 1) {
        const part = entry.parts[partIndex];
        const partAt = `${at} parts[${partIndex}]`;
        if (!exact(part, ["id", "producer_job", "parser", "artifact", "entry", "sha256", "evidence"], partAt)) continue;
        const registered = wantedParts[partIndex];
        if (
          registered &&
          JSON.stringify({
            id: part.id,
            producer_job: part.producer_job,
            parser: part.parser,
            artifact: part.artifact,
            entry: part.entry,
          }) !==
            JSON.stringify({
              id: registered.id,
              producer_job: registered.producer_job,
              parser: registered.parser,
              artifact: registered.artifact,
              entry: registered.entry,
            })
        ) {
          problems.push(`${partAt} identity does not match the registry`);
        }
        if (!/^[0-9a-f]{64}$/.test(part.sha256)) {
          problems.push(`${partAt} sha256 is invalid`);
        }
        if (
          !exact(
            part.evidence,
            [
              "executed",
              "passed",
              "failed",
              "skipped",
              "duration_ms",
              "seed",
              "corpus_id",
            ],
            `${partAt} evidence`,
          )
        ) continue;
        for (const key of ["executed", "passed", "failed", "skipped"]) {
          if (!Number.isSafeInteger(part.evidence[key]) || part.evidence[key] < 0) {
            problems.push(`${partAt} evidence.${key} must be a non-negative safe integer`);
          }
        }
        if (
          part.evidence.executed !==
          part.evidence.passed + part.evidence.failed + part.evidence.skipped
        ) {
          problems.push(`${partAt} evidence totals are inconsistent`);
        }
        if (part.evidence.executed <= 0) {
          problems.push(`${partAt} executed zero tests/checks`);
        }
        if (
          !Number.isSafeInteger(part.evidence.duration_ms) ||
          part.evidence.duration_ms < 0
        ) {
          problems.push(`${partAt} evidence.duration_ms must be a non-negative safe integer`);
        }
        if (
          part.evidence.seed !== null &&
          (typeof part.evidence.seed !== "string" ||
            !nativeSeedPattern.test(part.evidence.seed))
        ) {
          problems.push(`${partAt} evidence.seed is invalid`);
        }
        if (!/^sha256:[0-9a-f]{64}$/.test(part.evidence.corpus_id)) {
          problems.push(`${partAt} evidence.corpus_id is invalid`);
        }
      }
      let counts;
      try {
        counts = aggregateEvidence(entry.parts, lane);
      } catch (countError) {
        problems.push(countError instanceof Error ? countError.message : String(countError));
      }
      if (counts && JSON.stringify(entry.evidence) !== JSON.stringify(counts))
        problems.push(`${at} evidence counts are not derived from native parts`);
      if (counts && entry.conclusion !== laneConclusion(entry.job_results, counts))
        problems.push(`${at} conclusion is not derived from native parts and job provenance`);
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
    invocation_mode: entry.invocation_mode,
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
  const partsRoot = String(env.CI_LANE_NATIVE_PARTS ?? "").trim();
  if (partsRoot === "") {
    throw new ContractError("CI lane native evidence", [
      "CI_LANE_NATIVE_PARTS is required",
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
    partsRoot,
  });
  const written = writeNativeBundle(output, bundle);
  setOutput("native_path", written.path);
  setOutput("native_bundle_sha256", written.sha256);
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
