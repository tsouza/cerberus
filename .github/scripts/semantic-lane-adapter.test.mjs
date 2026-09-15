import { test } from "node:test";
import assert from "node:assert/strict";

import {
  TRIGGER_CONTEXTS,
  SemanticLaneAdapterError,
  classifyLaneRequiredness,
  classifyObservedEvidence,
  diagnoseRegistryDrift,
  resolveBindingLanes,
  validatePolicySnapshot,
} from "./lib/semantic-lane-adapter.mjs";
import { loadRegistry, matchesGlob } from "./ci-lane-contract.mjs";
import { readFileSync } from "node:fs";
import { join } from "node:path";

const REAL_SNAPSHOT = validatePolicySnapshot(
  JSON.parse(readFileSync(join("test", "semantic", "policy-snapshot.json"), "utf8")),
);
const REAL_REGISTRY = loadRegistry();

function baseSnapshot(overrides = {}) {
  return {
    schema_version: 1,
    main_ruleset: { required_checks: ["lint", "check"] },
    maintenance_ruleset: { ruleset_id: 1, ref_patterns: [], required_checks: ["mutation"] },
    release_required_checks: ["lint", "compose-smoke"],
    ...overrides,
  };
}

function lane(overrides = {}) {
  return {
    id: "test.lane",
    context: { match: "exact", name: "lint", protected: true },
    merge_posture: "always",
    main_posture: "always",
    release_posture: "required",
    package_globs: ["internal/foo/**"],
    ...overrides,
  };
}

// --- validatePolicySnapshot -------------------------------------------------

test("validatePolicySnapshot accepts a well-formed snapshot", () => {
  assert.doesNotThrow(() => validatePolicySnapshot(baseSnapshot()));
});

test("validatePolicySnapshot rejects a missing required key", () => {
  const snap = baseSnapshot();
  delete snap.release_required_checks;
  assert.throws(() => validatePolicySnapshot(snap), SemanticLaneAdapterError);
});

test("validatePolicySnapshot rejects the wrong schema_version", () => {
  assert.throws(
    () => validatePolicySnapshot(baseSnapshot({ schema_version: 99 })),
    SemanticLaneAdapterError,
  );
});

// --- classifyLaneRequiredness: three separate authorities -------------------

test("classifyLaneRequiredness resolves pull_request against the main ruleset authority", () => {
  const result = classifyLaneRequiredness(lane(), baseSnapshot(), "pull_request");
  assert.equal(result.authority, "main_ruleset");
  assert.equal(result.required, true);
});

test("classifyLaneRequiredness resolves merge_group against the main ruleset authority", () => {
  const result = classifyLaneRequiredness(lane(), baseSnapshot(), "merge_group");
  assert.equal(result.authority, "main_ruleset");
});

test("classifyLaneRequiredness resolves maintenance_pull_request against the maintenance ruleset authority", () => {
  const mutationLane = lane({ context: { match: "exact", name: "mutation", protected: false } });
  const result = classifyLaneRequiredness(mutationLane, baseSnapshot(), "maintenance_pull_request");
  assert.equal(result.authority, "maintenance_ruleset");
  assert.equal(result.required, true, "mutation is required on a release/*.x maintenance PR");
});

test("classifyLaneRequiredness resolves push_main against release_required_checks, a separate authority", () => {
  const composeSmoke = lane({
    context: { match: "exact", name: "compose-smoke", protected: false },
  });
  const result = classifyLaneRequiredness(composeSmoke, baseSnapshot(), "push_main");
  assert.equal(result.authority, "release_required_checks");
  assert.equal(result.required, true);
});

test("classifyLaneRequiredness: mutation's three postures are genuinely distinct", () => {
  const mutationLane = lane({ context: { match: "exact", name: "mutation", protected: false } });
  const snap = baseSnapshot();
  const onMain = classifyLaneRequiredness(mutationLane, snap, "pull_request");
  const onRelease = classifyLaneRequiredness(mutationLane, snap, "push_main");
  const onMaintenance = classifyLaneRequiredness(mutationLane, snap, "maintenance_pull_request");
  assert.equal(onMain.required, false, "not required on an ordinary main PR");
  assert.equal(onRelease.required, false, "not required on the publish path");
  assert.equal(onMaintenance.required, true, "required on a release/*.x maintenance PR");
});

test("classifyLaneRequiredness rejects an unknown trigger context", () => {
  assert.throws(
    () => classifyLaneRequiredness(lane(), baseSnapshot(), "bogus"),
    SemanticLaneAdapterError,
  );
});

test("classifyLaneRequiredness reports authority=null for a lane with no context", () => {
  const noContext = lane({ context: undefined });
  const result = classifyLaneRequiredness(noContext, baseSnapshot(), "pull_request");
  assert.equal(result.authority, null);
  assert.equal(result.required, false);
});

test("classifyLaneRequiredness matches a prefix-context lane by prefix, not exact equality", () => {
  const matrixLane = lane({ context: { match: "prefix", name: "bwc-minio (", protected: false } });
  const snap = baseSnapshot({
    main_ruleset: { required_checks: ["bwc-minio (hot-cold)", "lint"] },
  });
  const result = classifyLaneRequiredness(matrixLane, snap, "pull_request");
  assert.equal(result.required, true);
});

// TRIGGER_CONTEXTS distinguishes every listed execution context from every other
test("TRIGGER_CONTEXTS lists exactly the four distinct execution contexts", () => {
  assert.deepEqual(
    [...TRIGGER_CONTEXTS].sort(),
    ["maintenance_pull_request", "merge_group", "pull_request", "push_main"].sort(),
  );
});

// --- classifyObservedEvidence: no-op/missing/cancelled never count ---------

test("classifyObservedEvidence: a real pass counts as observed", () => {
  assert.equal(classifyObservedEvidence({ result: "pass" }).observed, true);
});

test("classifyObservedEvidence: a real fail counts as observed (it ran and produced a verdict)", () => {
  assert.equal(classifyObservedEvidence({ result: "fail" }).observed, true);
});

test("classifyObservedEvidence: a skipped run does not count as observed", () => {
  const outcome = classifyObservedEvidence({ result: "skipped" });
  assert.equal(outcome.observed, false);
});

test("classifyObservedEvidence: a cancelled run does not count as observed", () => {
  assert.equal(classifyObservedEvidence({ result: "cancelled" }).observed, false);
});

test("classifyObservedEvidence: a missing execution record does not count as observed", () => {
  assert.equal(classifyObservedEvidence(undefined).observed, false);
  assert.equal(classifyObservedEvidence(null).observed, false);
});

test("classifyObservedEvidence: a malformed result field does not count as observed", () => {
  assert.equal(classifyObservedEvidence({ result: 42 }).observed, false);
});

// --- diagnoseRegistryDrift ---------------------------------------------------

test("diagnoseRegistryDrift: no problems when declared and live agree", () => {
  const registry = { lanes: [lane()] };
  const problems = diagnoseRegistryDrift(registry, baseSnapshot());
  assert.deepEqual(problems, []);
});

test("diagnoseRegistryDrift: flags over-declaration (protected=true but live says not required)", () => {
  const registry = {
    lanes: [
      lane({
        id: "stale.lane",
        context: { match: "exact", name: "retired-check", protected: true },
        release_posture: "advisory",
      }),
    ],
  };
  const problems = diagnoseRegistryDrift(registry, baseSnapshot());
  assert.equal(problems.length, 1);
  assert.match(problems[0], /retired-check/);
  assert.match(problems[0], /protected=true/);
});

test("diagnoseRegistryDrift: flags under-declaration (nothing claims a live-required context)", () => {
  const registry = {
    lanes: [
      lane({
        id: "undeclared.lane",
        context: { match: "exact", name: "check", protected: false },
        release_posture: "advisory",
      }),
    ],
  };
  const problems = diagnoseRegistryDrift(registry, baseSnapshot());
  assert.equal(problems.length, 1);
  assert.match(problems[0], /protected=false/);
});

test("diagnoseRegistryDrift: two lanes sharing one context name do NOT false-positive when only one owns it", () => {
  const registry = {
    lanes: [
      lane({
        id: "owner",
        context: { match: "exact", name: "check", protected: true },
        release_posture: "advisory",
      }),
      lane({
        id: "sibling",
        context: { match: "exact", name: "check", protected: false },
        release_posture: "advisory",
      }),
    ],
  };
  const problems = diagnoseRegistryDrift(registry, baseSnapshot());
  assert.deepEqual(problems, [], "sibling's protected=false must not be flagged on its own");
});

test("diagnoseRegistryDrift: the real registry and real snapshot agree today (regression pin)", () => {
  // Pins the fix this issue shipped with: governance.update-golden-guard's
  // context.protected was corrected from true to false to match the live
  // ruleset (the check was removed and its workflow disabled). If this ever
  // fails again, the registry has drifted from the live snapshot — refresh
  // test/semantic/policy-snapshot.json (semantic-lane-policy-snapshot.mjs
  // --write) and reconcile ci-lanes.json, don't just delete this test.
  const problems = diagnoseRegistryDrift(REAL_REGISTRY, REAL_SNAPSHOT);
  assert.deepEqual(problems, []);
});

// --- resolveBindingLanes: contract-to-executable join ------------------------

test("resolveBindingLanes resolves a binding's test_ref to the lane(s) whose package_globs cover it", () => {
  const registry = {
    lanes: [
      lane({ id: "owner", package_globs: ["internal/promql/**"] }),
      lane({ id: "unrelated", package_globs: ["internal/logql/**"] }),
    ],
  };
  const binding = { id: "b1", test_ref: "internal/promql/lower.go" };
  const resolved = resolveBindingLanes(binding, registry, matchesGlob);
  assert.deepEqual(
    resolved.map((l) => l.id),
    ["owner"],
  );
});

test("resolveBindingLanes returns every lane covering the ref when more than one legitimately does", () => {
  const registry = {
    lanes: [
      lane({ id: "spec", package_globs: ["internal/promql/**"] }),
      lane({ id: "mutation", package_globs: ["internal/promql/**", "internal/chplan/**"] }),
    ],
  };
  const binding = { id: "b1", test_ref: "internal/promql/lower.go" };
  const resolved = resolveBindingLanes(binding, registry, matchesGlob);
  assert.deepEqual(
    resolved.map((l) => l.id).sort(),
    ["mutation", "spec"],
  );
});

test("resolveBindingLanes rejects a binding with no test_ref", () => {
  const registry = { lanes: [lane()] };
  assert.throws(
    () => resolveBindingLanes({ id: "b1" }, registry, matchesGlob),
    SemanticLaneAdapterError,
  );
});

test("resolveBindingLanes requires a real matchesGlob function, not a stand-in", () => {
  const registry = { lanes: [lane()] };
  const binding = { id: "b1", test_ref: "x" };
  assert.throws(() => resolveBindingLanes(binding, registry, null), SemanticLaneAdapterError);
});
