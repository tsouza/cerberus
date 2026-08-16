// ci-lane-selection.mjs — derive a total, fail-closed merge-lane selection
// from the exact projected checkout. No changed path, tree identity, lane
// roster, or selector conclusion is accepted from the caller.
//
// Required environment:
//   CI_LANE_BASE_SHA          target-base commit for the projected change.
//   CI_LANE_HEAD_SHA          exact projected commit checked out at HEAD.
//   CI_LANE_SELECTION_OUTPUT  new manifest path (must not already exist).
//   GITHUB_RUN_ID             qualification workflow run id.
//   GITHUB_RUN_ATTEMPT        positive qualification attempt number.
//
// Optional environment:
//   CI_LANE_REGISTRY          registry path (default .github/ci-lanes.json).
//   GITHUB_OUTPUT             Actions step-output destination.
//
// Unknown paths, an empty diff, an unavailable merge base/diff, or a filename
// that cannot enter the canonical manifest select every conditional lane.
// Registry, checkout, run-identity, validation, and output-write failures are
// hard failures because no trustworthy full roster can be emitted without
// them. Node builtins only.

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
import { TextDecoder } from "node:util";
import { fileURLToPath } from "node:url";

import {
  ContractError,
  DEFAULT_REGISTRY_PATH,
  deriveQualificationCorrelationNonce,
  loadRegistry,
  matchesGlob,
  validateChangedPaths,
  validateSelection,
} from "./ci-lane-contract.mjs";
import { capture, error, notice, setOutput, warning } from "./lib/gh.mjs";

export const FALLBACK_REASONS = Object.freeze([
  "none",
  "empty_diff",
  "unknown_paths",
  "merge_base_unavailable",
  "diff_unavailable",
  "unrepresentable_path",
]);

const shaPattern = /^(?:[0-9a-f]{40}|[0-9a-f]{64})$/;
const runIDPattern = /^[1-9][0-9]*$/;
const decoder = new TextDecoder("utf-8", { fatal: true });

function requirePattern(value, pattern, name) {
  const text = String(value ?? "").trim();
  if (!pattern.test(text)) {
    throw new ContractError("CI lane selector", [
      `${name} has invalid value ${JSON.stringify(text)}`,
    ]);
  }
  return text;
}

export function parseRunAttempt(value) {
  const text = requirePattern(value, runIDPattern, "GITHUB_RUN_ATTEMPT");
  const attempt = Number(text);
  if (!Number.isSafeInteger(attempt)) {
    throw new ContractError("CI lane selector", [
      `GITHUB_RUN_ATTEMPT exceeds JavaScript's safe integer range: ${text}`,
    ]);
  }
  return attempt;
}

function gitText(root, args) {
  return capture("git", args, { cwd: root });
}

export function checkoutEvidence({ root, headSHA }) {
  const expected = requirePattern(headSHA, shaPattern, "CI_LANE_HEAD_SHA");
  const top = gitText(root, ["rev-parse", "--show-toplevel"]);
  if (top.status !== 0 || resolve(top.stdout.trim()) !== resolve(root)) {
    throw new ContractError("CI lane selector", [
      "selector must run from the repository root",
    ]);
  }

  const actual = gitText(root, ["rev-parse", "--verify", "HEAD^{commit}"]);
  if (actual.status !== 0 || actual.stdout.trim() !== expected) {
    throw new ContractError("CI lane selector", [
      `checkout HEAD is ${JSON.stringify(actual.stdout.trim())}, want ${expected}`,
    ]);
  }
  const status = gitText(root, [
    "status",
    "--porcelain=v1",
    "--untracked-files=all",
  ]);
  if (status.status !== 0) {
    throw new ContractError("CI lane selector", [
      `git status failed: ${status.stderr.trim()}`,
    ]);
  }
  if (status.stdout !== "") {
    throw new ContractError("CI lane selector", [
      "projected checkout is not clean before selection",
    ]);
  }

  const tree = gitText(root, ["rev-parse", "--verify", `${expected}^{tree}`]);
  const treeSHA = tree.stdout.trim();
  if (tree.status !== 0 || !shaPattern.test(treeSHA)) {
    throw new ContractError("CI lane selector", [
      `cannot derive the projected tree: ${tree.stderr.trim() || treeSHA}`,
    ]);
  }
  return Object.freeze({ sha: expected, tree: treeSHA });
}

function fallback(reason, detail) {
  return Object.freeze({ changedPaths: [], reason, detail });
}

export function deriveChangedPaths({ root, baseSHA, headSHA }) {
  const base = requirePattern(baseSHA, shaPattern, "CI_LANE_BASE_SHA");
  const head = requirePattern(headSHA, shaPattern, "CI_LANE_HEAD_SHA");
  const mergeBase = gitText(root, ["merge-base", base, head]);
  const mergeBaseSHA = mergeBase.stdout.trim();
  if (mergeBase.status !== 0 || !shaPattern.test(mergeBaseSHA)) {
    return fallback(
      "merge_base_unavailable",
      mergeBase.stderr.trim() || "git merge-base returned no commit",
    );
  }

  const diff = capture(
    "git",
    [
      "diff",
      "--no-ext-diff",
      "--no-textconv",
      "--no-renames",
      "--name-only",
      "-z",
      mergeBaseSHA,
      head,
      "--",
    ],
    { cwd: root, encoding: null },
  );
  if (diff.status !== 0) {
    return fallback(
      "diff_unavailable",
      String(diff.stderr).trim() || "git diff failed",
    );
  }

  let decoded;
  try {
    decoded = decoder.decode(diff.stdout);
  } catch (decodeError) {
    return fallback("unrepresentable_path", decodeError.message);
  }
  if (decoded !== "" && !decoded.endsWith("\0")) {
    return fallback(
      "diff_unavailable",
      "NUL-delimited git diff did not end at a path boundary",
    );
  }
  const changedPaths = [
    ...new Set(decoded.split("\0").filter((path) => path !== "")),
  ].sort();
  const problems = [];
  validateChangedPaths(changedPaths, "derived changed paths", problems);
  if (problems.length > 0) {
    return fallback("unrepresentable_path", problems.join("; "));
  }
  if (changedPaths.length === 0) {
    return fallback("empty_diff", "projected diff contains no paths");
  }
  return Object.freeze({ changedPaths, reason: "none", detail: "" });
}

function matchesAny(path, globs) {
  return globs.some((glob) => matchesGlob(path, glob));
}

export function selectionPolicy(registry, changedPaths, fallbackReason = "none") {
  if (!FALLBACK_REASONS.includes(fallbackReason)) {
    throw new ContractError("CI lane selector", [
      `unsupported fallback reason ${JSON.stringify(fallbackReason)}`,
    ]);
  }
  const paths = [...changedPaths];
  const conditional = registry.lanes.filter(
    (lane) =>
      lane.merge_posture === "impact" ||
      lane.merge_posture === "non_documentation",
  );
  const knownNonimpactGlobs =
    registry.impact_selection?.known_nonimpact_globs ?? [];
  const unknownPaths = paths.filter(
    (path) =>
      !conditional.some((lane) => matchesAny(path, lane.package_globs)) &&
      !matchesAny(path, knownNonimpactGlobs),
  );
  let reason = fallbackReason;
  if (reason === "none" && paths.length === 0) reason = "empty_diff";
  if (reason === "none" && unknownPaths.length > 0)
    reason = "unknown_paths";
  const fallbackFull = reason !== "none";
  const docsOnly =
    paths.length > 0 &&
    paths.every((path) => matchesAny(path, knownNonimpactGlobs));

  const lanes = [...registry.lanes]
    .sort((left, right) => left.id.localeCompare(right.id))
    .map((lane) => {
      const selected = () => ({
        lane_id: lane.id,
        disposition: "selected",
        executions: [...lane.executions],
        reason: null,
      });
      const omitted = (omissionReason) => ({
        lane_id: lane.id,
        disposition: "omitted",
        executions: [],
        reason: omissionReason,
      });
      if (lane.merge_posture === "always") return selected();
      if (lane.merge_posture === "never")
        return omitted("posture_excluded");
      if (fallbackFull) return selected();

      const directHit = paths.some((path) =>
        matchesAny(path, lane.package_globs),
      );
      if (lane.merge_posture === "impact") {
        if (directHit) return selected();
        return omitted(docsOnly ? "docs_only" : "not_impacted");
      }
      if (lane.merge_posture === "non_documentation") {
        if (directHit || !docsOnly) return selected();
        return omitted("docs_only");
      }
      throw new ContractError("CI lane selector", [
        `lane ${lane.id} has unsupported merge posture ${JSON.stringify(lane.merge_posture)}`,
      ]);
    });

  return Object.freeze({
    conclusion: fallbackFull ? "fallback_full" : "success",
    fallbackReason: reason,
    changedPaths: paths,
    unknownPaths,
    lanes,
  });
}

export function createSelection({
  registry,
  baseSHA,
  source,
  runID,
  runAttempt,
  changedPaths,
  fallbackReason = "none",
  correlationNonce,
}) {
  const base = requirePattern(baseSHA, shaPattern, "CI_LANE_BASE_SHA");
  const id = requirePattern(runID, runIDPattern, "GITHUB_RUN_ID");
  const attempt =
    typeof runAttempt === "number" ? runAttempt : parseRunAttempt(runAttempt);
  if (!Number.isSafeInteger(attempt) || attempt < 1) {
    throw new ContractError("CI lane selector", [
      `GITHUB_RUN_ATTEMPT must be a positive safe integer; got ${JSON.stringify(runAttempt)}`,
    ]);
  }
  const policy = selectionPolicy(registry, changedPaths, fallbackReason);
  const qualificationNonce =
    correlationNonce ??
    deriveQualificationCorrelationNonce({ posture: "merge", source });
  const document = {
    schema_version: registry.selection_schema_version,
    registry_schema_version: registry.schema_version,
    report_schema_version: registry.report_schema_version,
    posture: "merge",
    source: { sha: source.sha, tree: source.tree },
    candidate_digest: null,
    correlation_nonce: qualificationNonce,
    run: { id, attempt },
    selector: {
      conclusion: policy.conclusion,
      base_sha: base,
      head_sha: source.sha,
      changed_paths: [...policy.changedPaths],
      unknown_paths: [...policy.unknownPaths],
    },
    lanes: policy.lanes,
  };
  validateSelection(document, registry, {
    posture: "merge",
    sha: source.sha,
    tree: source.tree,
    correlationNonce: qualificationNonce,
    runID: id,
    runAttempt: attempt,
    baseSHA: base,
    changedPaths: policy.changedPaths,
  });
  return Object.freeze({ document, fallbackReason: policy.fallbackReason });
}

export function writeSelection(path, document) {
  const output = resolve(path);
  const parent = dirname(output);
  if (!existsSync(parent) || !statSync(parent).isDirectory()) {
    throw new ContractError("CI lane selector", [
      `selection output directory does not exist: ${parent}`,
    ]);
  }
  if (existsSync(output)) {
    throw new ContractError("CI lane selector", [
      `selection output already exists; refusing stale reuse: ${output}`,
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
    sha256: createHash("sha256").update(body).digest("hex"),
    body,
  });
}

function main(env = process.env) {
  const root = resolve(process.cwd());
  const baseSHA = requirePattern(
    env.CI_LANE_BASE_SHA,
    shaPattern,
    "CI_LANE_BASE_SHA",
  );
  const headSHA = requirePattern(
    env.CI_LANE_HEAD_SHA,
    shaPattern,
    "CI_LANE_HEAD_SHA",
  );
  const output = String(env.CI_LANE_SELECTION_OUTPUT ?? "").trim();
  if (output === "") {
    throw new ContractError("CI lane selector", [
      "CI_LANE_SELECTION_OUTPUT is required",
    ]);
  }
  const runID = requirePattern(env.GITHUB_RUN_ID, runIDPattern, "GITHUB_RUN_ID");
  const runAttempt = parseRunAttempt(env.GITHUB_RUN_ATTEMPT);
  const registry = loadRegistry(
    env.CI_LANE_REGISTRY || DEFAULT_REGISTRY_PATH,
    { root },
  );
  const source = checkoutEvidence({ root, headSHA });
  const diff = deriveChangedPaths({ root, baseSHA, headSHA });
  if (diff.reason !== "none") {
    warning(
      `CI lane selector is running the full conditional fence (${diff.reason}): ${diff.detail}`,
      { title: "merge-fence selector fallback" },
    );
  }
  const selection = createSelection({
    registry,
    baseSHA,
    source,
    runID,
    runAttempt,
    changedPaths: diff.changedPaths,
    fallbackReason: diff.reason,
  });
  const written = writeSelection(output, selection.document);
  const selected = selection.document.lanes.filter(
    (lane) => lane.disposition === "selected",
  );
  const executions = selected.flatMap((lane) =>
    lane.executions.map((executionID) => ({
      lane_id: lane.lane_id,
      execution_id: executionID,
    })),
  );
  for (const [name, value] of [
    ["selection_path", written.path],
    ["selection_sha256", written.sha256],
    ["source_sha", source.sha],
    ["source_tree", source.tree],
    ["correlation_nonce", selection.document.correlation_nonce],
    ["base_sha", baseSHA],
    ["changed_paths_json", JSON.stringify(selection.document.selector.changed_paths)],
    ["unknown_paths_json", JSON.stringify(selection.document.selector.unknown_paths)],
    ["selector_conclusion", selection.document.selector.conclusion],
    ["selected_lane_ids_json", JSON.stringify(selected.map((lane) => lane.lane_id))],
    ["selected_executions_json", JSON.stringify(executions)],
    ["fallback_reason", selection.fallbackReason],
  ]) {
    setOutput(name, value);
  }
  notice(
    `merge-fence selector chose ${executions.length} execution(s) across ${selected.length} lane(s); ` +
      `conclusion=${selection.document.selector.conclusion}`,
  );
}

const invokedDirectly =
  process.argv[1] && fileURLToPath(import.meta.url) === resolve(process.argv[1]);
if (invokedDirectly) {
  try {
    main();
  } catch (selectionError) {
    error(selectionError instanceof Error ? selectionError.message : String(selectionError), {
      title: "merge-fence selector failed closed",
    });
    process.exit(1);
  }
}
