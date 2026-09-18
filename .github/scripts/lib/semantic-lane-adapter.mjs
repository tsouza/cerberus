// semantic-lane-adapter.mjs — binds the semantic contract model's verifiers
// and bindings (test/semantic/, see semantic-model.mjs and cerberus issue
// #3426) to the EXISTING CI lane registry (.github/ci-lanes.json, see
// ci-lane-contract.mjs) rather than duplicating execution policy in the
// semantic layer (cerberus issue #3427).
//
// Three distinct concerns this module deliberately keeps SEPARATE, per
// #3427's own acceptance criteria:
//
//   1. EXECUTION vs STATUS. A lane can execute (its job ran, its command
//      produced a real result) without being a REQUIRED status check, and a
//      required status check can be satisfied by a short-circuited no-op on
//      an ordinary PR (compose-smoke/dashboard/profile all short-circuit off
//      an ordinary head branch — see ci-lanes.json's own `applicability` and
//      release.yml's header comment). `classifyObservedEvidence` exists
//      specifically so a no-op/missing/cancelled run can never silently
//      count as evidence a contract was actually verified.
//
//   2. THREE POLICY AUTHORITIES, not one merged set:
//        - main_ruleset：the live `main` branch ruleset's required_status_checks
//          union (a pull_request or merge_group entry into `main`).
//        - maintenance_ruleset：the live ruleset governing `release/*.x`
//          branches (a pull_request into a maintenance line).
//        - release_required_checks：release.yml's own EXPECTED set, checked
//          on a PUBLISHING push to `main` — a deliberate superset relation,
//          not equality, and the only authority that can name a lane with no
//          `pull_request:` trigger at all (migration-e2e, perf-nightly).
//      These come from a captured, offline-readable snapshot
//      (test/semantic/policy-snapshot.json, produced by
//      semantic-lane-policy-snapshot.mjs) rather than a live network call on
//      every unit test run.
//
//   3. MUTATION'S three-way split is the running example throughout: its
//      merge_posture is "impact" (diff-scoped subset on every PR/merge
//      group, for early signal, not required), its main_posture triggers
//      the full post-merge matrix, and its release_posture is "advisory" on
//      `main` — required only on a release/*.x maintenance-line PR (a fact
//      this module derives from the maintenance_ruleset snapshot, not from
//      a hand-maintained "mutation is required on branch X" fact).
//
// DRIFT DIAGNOSTICS: the registry already self-declares two requiredness
// signals — `lane.context.protected` (main-ruleset membership) and
// `lane.release_posture === "required"` (release-gate membership) — both
// validated structurally by ci-lane-contract.mjs but never cross-checked
// against what the LIVE policy actually says. `diagnoseLaneDrift` does that
// cross-check; it is what caught issue #3427's own worked example (see the
// PR this module shipped with): `governance.update-golden-guard` still
// declared `context.protected: true` after the live ruleset had the check
// removed and its workflow disabled.

import { classifyTestRef } from "./semantic-evidence-adapter.mjs";

export const TRIGGER_CONTEXTS = Object.freeze([
  "pull_request",
  "merge_group",
  "push_main",
  "maintenance_pull_request",
]);

const OBSERVED_EXECUTION_RESULTS = new Set(["pass", "fail"]);

export class SemanticLaneAdapterError extends Error {
  constructor(problems) {
    super(`semantic lane adapter:\n${problems.map((p) => `- ${p}`).join("\n")}`);
    this.problems = problems;
  }
}

function fail(problems) {
  throw new SemanticLaneAdapterError(problems);
}

/** Validates and returns the shape produced by semantic-lane-policy-snapshot.mjs. */
export function validatePolicySnapshot(snapshot) {
  const problems = [];
  if (snapshot?.schema_version !== 1) {
    problems.push("[schema] policy-snapshot.schema_version must be 1");
  }
  for (const key of ["main_ruleset", "maintenance_ruleset", "release_required_checks"]) {
    if (!(key in (snapshot ?? {}))) {
      problems.push(`[schema] policy-snapshot missing required key "${key}"`);
    }
  }
  if (problems.length > 0) fail(problems);
  if (!Array.isArray(snapshot.main_ruleset?.required_checks)) {
    fail(["[schema] policy-snapshot.main_ruleset.required_checks must be an array"]);
  }
  if (!Array.isArray(snapshot.maintenance_ruleset?.required_checks)) {
    fail(["[schema] policy-snapshot.maintenance_ruleset.required_checks must be an array"]);
  }
  if (!Array.isArray(snapshot.release_required_checks)) {
    fail(["[schema] policy-snapshot.release_required_checks must be an array"]);
  }
  return snapshot;
}

/**
 * Classifies whether `lane` is a required status check for a given trigger
 * context, against the LIVE policy snapshot — never against the registry's
 * own self-declared posture fields, which is exactly what lets
 * `diagnoseLaneDrift` below catch the two from disagreeing.
 *
 * Returns { required, authority, contextName } where `authority` names
 * which of the three policy authorities was consulted (or null if the lane
 * carries no `context.name`, e.g. a lane whose only signal is a workflow
 * artifact rather than a named check-run).
 */
export function classifyLaneRequiredness(lane, snapshot, triggerContext) {
  if (!TRIGGER_CONTEXTS.includes(triggerContext)) {
    fail([`[schema] unknown trigger context "${triggerContext}"`]);
  }
  const contextName = lane?.context?.name ?? null;
  if (contextName === null) {
    return { required: false, authority: null, contextName: null };
  }
  const authority =
    triggerContext === "pull_request" || triggerContext === "merge_group"
      ? "main_ruleset"
      : triggerContext === "maintenance_pull_request"
        ? "maintenance_ruleset"
        : "release_required_checks";
  const requiredSet =
    authority === "main_ruleset"
      ? snapshot.main_ruleset.required_checks
      : authority === "maintenance_ruleset"
        ? snapshot.maintenance_ruleset.required_checks
        : snapshot.release_required_checks;
  // Context matching mirrors ci-lane-contract.mjs's own two match modes: a
  // matrix lane (e.g. "bwc-minio (") matches any live context that starts
  // with its declared prefix, an ordinary lane matches exactly.
  const required =
    lane.context.match === "prefix"
      ? requiredSet.some((c) => c.startsWith(contextName))
      : requiredSet.includes(contextName);
  return { required, authority, contextName };
}

/**
 * Cross-checks the registry's self-declared requiredness signals
 * (`context.protected`, `release_posture === "required"`) against what the
 * live policy snapshot actually says, one drift check per DISTINCT context
 * name — not per lane. More than one lane can legitimately share one
 * required-context name (a matrix aggregator and an individual leg, or two
 * jobs that both post the same check name); only ONE of them needs to
 * declare `protected: true` for that context to be correctly represented,
 * so comparing lane-by-lane would false-positive on every non-owning lane
 * that shares a required name. The two real drift shapes this catches:
 *
 *   under-declared   the live snapshot requires this context, but no lane
 *                     with that context name claims it (protected/required
 *                     is false everywhere) — the registry has fallen behind
 *                     a newly-required check.
 *   over-declared     at least one lane claims protected/required, but the
 *                     live snapshot no longer requires that context — the
 *                     registry has fallen behind a de-gated check (this is
 *                     exactly how issue #3427 found
 *                     governance.update-golden-guard still declaring
 *                     context.protected: true after the live check was
 *                     removed and its workflow disabled).
 *
 * Returns an array of problem strings.
 */
export function diagnoseRegistryDrift(registry, snapshot) {
  const problems = [];
  const byContextName = new Map();
  for (const lane of registry.lanes) {
    const contextName = lane?.context?.name ?? null;
    if (contextName === null) continue;
    if (!byContextName.has(contextName)) byContextName.set(contextName, []);
    byContextName.get(contextName).push(lane);
  }

  for (const [contextName, lanes] of byContextName) {
    const representative = lanes[0];

    const liveMain = classifyLaneRequiredness(representative, snapshot, "pull_request");
    const declaredMainRequired = lanes.some((l) => l.context.protected === true);
    if (declaredMainRequired !== liveMain.required) {
      const owners = lanes.map((l) => l.id).join(", ");
      problems.push(
        `[drift] context "${contextName}" (lanes: ${owners}) declares ` +
          `context.protected=${declaredMainRequired} but the live main-ruleset snapshot says ` +
          `required=${liveMain.required}`,
      );
    }

    const liveRelease = classifyLaneRequiredness(representative, snapshot, "push_main");
    const declaredReleaseRequired = lanes.some((l) => l.release_posture === "required");
    if (declaredReleaseRequired !== liveRelease.required) {
      const owners = lanes.map((l) => `${l.id}=${l.release_posture}`).join(", ");
      problems.push(
        `[drift] context "${contextName}" (lanes: ${owners}) declares ` +
          `release-required=${declaredReleaseRequired} but the live release_required_checks ` +
          `snapshot says required=${liveRelease.required}`,
      );
    }
  }

  return problems;
}

/**
 * Classifies whether an execution record counts as OBSERVED SEMANTIC
 * EVIDENCE (acceptance criterion: "a no-op, missing, cancelled or
 * unexecuted job never counts as observed semantic evidence"). Only a real
 * pass or fail verdict — the binding's verifier actually ran to completion
 * and produced a result — counts; anything else (a short-circuited no-op, a
 * cancelled run, a job GitHub never scheduled) does not, regardless of what
 * check-run conclusion GitHub itself reports for the wrapping job.
 */
export function classifyObservedEvidence(execution) {
  if (!execution || typeof execution.result !== "string") {
    return { observed: false, reason: "missing or malformed execution record" };
  }
  if (!OBSERVED_EXECUTION_RESULTS.has(execution.result)) {
    return {
      observed: false,
      reason: `result "${execution.result}" is not a real verdict (no-op/missing/cancelled never count)`,
    };
  }
  return { observed: true, reason: null };
}

// A lane whose declared scope is the whole tree says nothing about WHICH
// test a binding's evidence runs in: `ci.lint` (`**`), `ci.link-check`
// (`**/*.md`), `security.codeql` (`**/*.go`) and every `governance.*` lane
// (PR body, deferral markers, release-gate drift — checks on the change
// itself, not on any test) match every test_ref string there is. Listing
// them as an obligation of every binding padded every row of the report
// with `ci.lint (merge-required)` and, because `ci.lint` sorts first, made
// `just lint` the "canonical execution" of contracts about range-vector
// alignment. They are excluded from the join here, at the source.
const GOVERNANCE_LANE_PREFIX = "governance.";
const TREE_WIDE_GLOB_PREFIX = "**";

/** True when a lane's scope is the whole tree (a `**`-rooted glob) or it is a governance lane. */
export function isCatchAllLane(lane) {
  if (typeof lane?.id === "string" && lane.id.startsWith(GOVERNANCE_LANE_PREFIX)) return true;
  return (lane?.package_globs ?? []).some((glob) => glob.startsWith(TREE_WIDE_GLOB_PREFIX));
}

/**
 * Resolves a semantic binding's `test_ref` to the CI lane(s) whose
 * `package_globs` cover it, connecting a contract's evidence trail to the
 * actual owning workflow/job/required-context — the "contract-to-executable
 * join" this adapter exists to provide without hand-duplicating lane data.
 * Returns an array (a test_ref can legitimately fall under more than one
 * lane's package_globs, e.g. a file covered by both a spec lane and a
 * mutation lane's package selection), never including a catch-all lane
 * (isCatchAllLane above).
 *
 * A source-path test_ref (lib/semantic-evidence-adapter.mjs's sixth
 * scheme) is matched by its PATH, not the raw string: `file.go:TestName`
 * matches the globs that name `file.go`, and a bare directory such as
 * `compatibility/prometheus` matches `compatibility/prometheus/**` — the
 * lane that runs the harness — rather than nothing. A lane also owns a
 * source-path binding when its registry `command` names that path
 * verbatim (`ci.agpl-clean` runs `node .github/scripts/agpl-clean.mjs`):
 * a gate script's own path is rarely inside the package_globs of the lane
 * that executes it, since those globs declare what TRIGGERS the lane.
 */
export function resolveBindingLanes(binding, registry, matchesGlob) {
  if (typeof matchesGlob !== "function") {
    fail(["[schema] resolveBindingLanes requires a matchesGlob(path, glob) function"]);
  }
  const testRef = binding?.test_ref;
  if (typeof testRef !== "string" || testRef.length === 0) {
    fail([`[schema] binding "${binding?.id}" has no test_ref to resolve`]);
  }
  const candidates = [testRef];
  const classified = classifyTestRef(testRef);
  if (classified.system === "source-path") {
    candidates.push(classified.path, `${classified.path}/`);
  }
  const runsPath = (lane) =>
    classified.system === "source-path" &&
    typeof lane.command === "string" &&
    lane.command.includes(classified.path);
  return registry.lanes.filter(
    (lane) =>
      !isCatchAllLane(lane) &&
      ((lane.package_globs ?? []).some((glob) => candidates.some((c) => matchesGlob(c, glob))) || runsPath(lane)),
  );
}
