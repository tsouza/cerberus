// semantic-impact.test.mjs — node:test guard for `just semantic-impact
// <base> <head>` (cerberus issue #3460).
//
// Two fixture layers, same split lane-closure.test.mjs and merge-risk.test.mjs
// already use:
//
//   - a SMALL, stubbed model + registry + graph, fast and deterministic, to
//     pin the shared-pipeline fan-out shape directly: a change to one
//     package two lanes both depend on (never declared by either lane's own
//     globs) must reach BOTH lanes' contracts, across two different heads.
//   - a REPLAY of PR #2824's real changed-file set (the change that caused
//     the #2895 outage merge-risk.test.mjs already pins) against the REAL
//     semantic model, the REAL CI lane registry, and the REAL `go list`
//     import graph — the acceptance case cerberus issue #3460 itself asks
//     for ("Replay #2902/#2895's shared-pipeline impact shape as a
//     deterministic test"). File list verbatim from `gh pr view 2824
//     --json files`, duplicated from merge-risk.test.mjs's own copy (a
//     historical fact, not logic, so it is pinned independently by each
//     layer that needs it rather than imported).

import assert from "node:assert/strict";
import test from "node:test";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { validateSemanticModel } from "./semantic-model.mjs";
import { loadRegistry } from "../ci-lane-contract.mjs";
import { validatePolicySnapshot } from "./semantic-lane-adapter.mjs";
import {
  ADVERSARIAL_NOTE,
  MERGE_RELEASE_CAVEAT,
  buildImpactReport,
  impactedContracts,
  ownerLaneIds,
  renderJSON,
  renderText,
  resolveLaneClosures,
  touchedLanes,
} from "./semantic-impact.mjs";

const REPO_ROOT = process.cwd();

// --- Synthetic fixture -------------------------------------------------------

function head(id, name) {
  return { id, name, description: "d", status: "active", owner: "core", replaces: null, replaced_by: null };
}

function contract(id, applicableHead, testRefPrefix) {
  return {
    id,
    statement: `${id} statement.`,
    scope: "head",
    applicable_heads: [applicableHead],
    applicable_capabilities: [],
    authority: "specification",
    required_evidence_classes: ["execution"],
    required_independence_groups: ["group-a"],
    blind_spots: [],
    related_contracts: [],
    inherits_from: null,
    status: "active",
    deficit_reason: null,
    owner: "core",
    replaces: null,
    replaced_by: null,
    _testRefPrefix: testRefPrefix, // consumed only by fixtureModel below
  };
}

// A minimal, valid six-document model: two head-scoped contracts (one
// PromQL, one LogQL), one shared verifier, one active binding per contract.
function fixtureModel() {
  const contracts = [
    contract("PROMQL-EXAMPLE", "HEAD-PROMQL", "test/spec/promql/example.txtar"),
    contract("LOGQL-EXAMPLE", "HEAD-LOGQL", "test/spec/logql/example.txtar"),
  ];
  const documents = {
    heads: {
      schema_version: 1,
      heads: [head("HEAD-PROMQL", "PromQL"), head("HEAD-LOGQL", "LogQL"), head("HEAD-TRACEQL", "TraceQL")],
    },
    capabilities: { schema_version: 1, capabilities: [] },
    contracts: {
      schema_version: 1,
      contracts: contracts.map(({ _testRefPrefix, ...c }) => c),
    },
    verifiers: {
      schema_version: 1,
      verifiers: [
        {
          id: "VERIFIER-EXAMPLE",
          name: "Example verifier",
          description: "d",
          detects: ["something"],
          cannot_detect: [],
          complemented_by: [],
          substrate: "chdb",
          relative_cost: "low",
          status: "active",
          replaces: null,
          replaced_by: null,
        },
      ],
    },
    bindings: {
      schema_version: 1,
      bindings: contracts.map((c) => ({
        id: `BINDING-${c.id}`,
        contract: c.id,
        verifier: "VERIFIER-EXAMPLE",
        evidence_class: "execution",
        independence_group: "group-a",
        test_ref: c._testRefPrefix,
        status: "active",
      })),
    },
    executions: { schema_version: 1, executions: [] },
  };
  return validateSemanticModel(documents);
}

function fixtureRegistry() {
  return {
    lanes: [
      {
        id: "promql.lane",
        package_globs: ["internal/promql/**", "test/spec/promql/**"],
        build_tags: [],
        recipes: ["test-promql"],
        command: null,
        context: { name: "promql-check", match: "exact" },
      },
      {
        id: "logql.lane",
        package_globs: ["internal/logql/**", "test/spec/logql/**"],
        build_tags: [],
        recipes: ["test-logql"],
        command: null,
        context: { name: "logql-check", match: "exact" },
      },
    ],
  };
}

function fixtureSnapshot() {
  return {
    schema_version: 1,
    main_ruleset: { required_checks: ["promql-check"] },
    maintenance_ruleset: { required_checks: [] },
    release_required_checks: [],
  };
}

// go-list-style record, mirroring lane-closure.test.mjs's own helper. `Dir`
// is rooted at the REAL repo root (not a fake "/repo/" prefix) because
// resolveLaneClosures also derives each lane's SEEDS from the real
// filesystem (holdsGoSource) — the stub only replaces the import EDGES, so
// its `Dir` values must resolve under the same root the seeds do.
const MODULE = "github.com/tsouza/cerberus";
function record({ dir, imports = [] }) {
  return JSON.stringify(
    { ImportPath: `${MODULE}/${dir}`, Dir: `${REPO_ROOT}/${dir}`, Imports: imports.map((d) => `${MODULE}/${d}`) },
    null,
    2,
  );
}

// The shared-pipeline shape: both heads' packages import a package NEITHER
// lane's own package_globs names — the exact #2902 hole.
const SHARED_PIPELINE = [
  { dir: "internal/promql", imports: ["internal/chplan"] },
  { dir: "internal/logql", imports: ["internal/chplan"] },
  { dir: "internal/chplan", imports: [] },
];

function stubGoList(records) {
  return () => records.map(record).join("\n");
}

// --- ownerLaneIds -------------------------------------------------------------

test("ownerLaneIds names only lanes an active binding's declared globs resolve to", () => {
  const model = fixtureModel();
  const registry = fixtureRegistry();
  assert.deepEqual([...ownerLaneIds(model, registry)].sort(), ["logql.lane", "promql.lane"]);
});

test("ownerLaneIds is empty when no lane's declared globs match any test_ref", () => {
  const model = fixtureModel();
  const registry = { lanes: [{ id: "unrelated", package_globs: ["docs/**"], build_tags: [] }] };
  assert.deepEqual([...ownerLaneIds(model, registry)], []);
});

// --- resolveLaneClosures --------------------------------------------------------

test("resolveLaneClosures returns ok:true with the derived globs on success", () => {
  const registry = fixtureRegistry();
  const closures = resolveLaneClosures(registry.lanes, { repoRoot: REPO_ROOT, runGoList: stubGoList(SHARED_PIPELINE) });
  assert.equal(closures.ok, true);
  assert.ok(closures.affected.get("promql.lane").includes("internal/chplan/**"));
  assert.ok(closures.affected.get("logql.lane").includes("internal/chplan/**"));
});

test("resolveLaneClosures returns ok:false, never a narrower answer, when go list fails", () => {
  const registry = fixtureRegistry();
  const closures = resolveLaneClosures(registry.lanes, {
    repoRoot: REPO_ROOT,
    runGoList: () => {
      throw new Error("go: no such tool");
    },
  });
  assert.equal(closures.ok, false);
  assert.match(closures.error, /go: no such tool/);
});

// --- touchedLanes: declared vs derived, and the conservative fallback ----------

test("a file under a lane's OWN declared glob is a declared hit", () => {
  const registry = fixtureRegistry();
  const closures = resolveLaneClosures(registry.lanes, { repoRoot: REPO_ROOT, runGoList: stubGoList(SHARED_PIPELINE) });
  const declared = new Map([
    ["promql.lane", registry.lanes[0].package_globs],
    ["logql.lane", registry.lanes[1].package_globs],
  ]);
  const touched = touchedLanes(["internal/promql/lower.go"], registry.lanes, declared, closures);
  assert.equal(touched.get("promql.lane").touched, true);
  assert.deepEqual(
    touched.get("promql.lane").reasons.map((r) => r.kind),
    ["declared"],
  );
  assert.equal(touched.get("logql.lane").touched, false);
});

test("SHARED PIPELINE: a package neither lane declares reaches BOTH lanes, as a derived hit", () => {
  // The #2902 shape, reproduced at the smallest possible scale: promql.lane
  // and logql.lane declare disjoint globs, but both import internal/chplan.
  const registry = fixtureRegistry();
  const closures = resolveLaneClosures(registry.lanes, { repoRoot: REPO_ROOT, runGoList: stubGoList(SHARED_PIPELINE) });
  const declared = new Map([
    ["promql.lane", registry.lanes[0].package_globs],
    ["logql.lane", registry.lanes[1].package_globs],
  ]);
  const touched = touchedLanes(["internal/chplan/plan.go"], registry.lanes, declared, closures);
  for (const laneId of ["promql.lane", "logql.lane"]) {
    assert.equal(touched.get(laneId).touched, true, `${laneId} must see the shared package`);
    assert.deepEqual(touched.get(laneId).reasons.map((r) => r.kind), ["derived"]);
  }
});

test("a file matching no lane's declared OR derived globs touches nothing", () => {
  const registry = fixtureRegistry();
  const closures = resolveLaneClosures(registry.lanes, { repoRoot: REPO_ROOT, runGoList: stubGoList(SHARED_PIPELINE) });
  const declared = new Map([
    ["promql.lane", registry.lanes[0].package_globs],
    ["logql.lane", registry.lanes[1].package_globs],
  ]);
  const touched = touchedLanes(["docs/engine.md"], registry.lanes, declared, closures);
  assert.equal(touched.get("promql.lane").touched, false);
  assert.equal(touched.get("logql.lane").touched, false);
});

test("CONSERVATIVE: an unresolvable import graph marks every lane touched, with a reason", () => {
  const registry = fixtureRegistry();
  const closures = { ok: false, affected: null, error: "no go toolchain" };
  const touched = touchedLanes(["docs/engine.md"], registry.lanes, new Map(), closures);
  for (const laneId of ["promql.lane", "logql.lane"]) {
    const t = touched.get(laneId);
    assert.equal(t.touched, true);
    assert.equal(t.conservative, true);
    assert.match(t.reasons[0].detail, /no go toolchain/);
  }
});

// --- impactedContracts: the cross-head fan-out, through the model -------------

test("SHARED PIPELINE at the CONTRACT level: one shared-package change reaches BOTH heads' contracts", () => {
  const model = fixtureModel();
  const registry = fixtureRegistry();
  const snapshot = fixtureSnapshot();
  const closures = resolveLaneClosures(registry.lanes, { repoRoot: REPO_ROOT, runGoList: stubGoList(SHARED_PIPELINE) });
  const declared = new Map([
    ["promql.lane", registry.lanes[0].package_globs],
    ["logql.lane", registry.lanes[1].package_globs],
  ]);
  const touched = touchedLanes(["internal/chplan/plan.go"], registry.lanes, declared, closures);

  const contracts = impactedContracts(model, registry, snapshot, touched);
  assert.deepEqual(
    contracts.map((c) => c.id),
    ["LOGQL-EXAMPLE", "PROMQL-EXAMPLE"],
  );
  for (const c of contracts) {
    assert.equal(c.bound_evidence.assured, true);
    assert.deepEqual(c.adversarial_bindings, []);
    assert.equal(c.adversarial_note, ADVERSARIAL_NOTE);
    assert.equal(c.bindings[0].triggers[0].reasons[0].kind, "derived");
  }
});

test("a contract with no touched lane is absent, not reported as zero-impact", () => {
  const model = fixtureModel();
  const registry = fixtureRegistry();
  const snapshot = fixtureSnapshot();
  const touched = new Map([
    ["promql.lane", { touched: false, conservative: false, reasons: [] }],
    ["logql.lane", { touched: false, conservative: false, reasons: [] }],
  ]);
  assert.deepEqual(impactedContracts(model, registry, snapshot, touched), []);
});

test("merge/release obligations come through per binding, from the real lane-adapter join", () => {
  const model = fixtureModel();
  const registry = fixtureRegistry();
  const snapshot = fixtureSnapshot();
  const touched = new Map([
    ["promql.lane", { touched: true, conservative: false, reasons: [{ file: "internal/promql/lower.go", glob: "internal/promql/**", kind: "declared" }] }],
    ["logql.lane", { touched: false, conservative: false, reasons: [] }],
  ]);
  const [contract] = impactedContracts(model, registry, snapshot, touched);
  assert.equal(contract.id, "PROMQL-EXAMPLE");
  assert.deepEqual(contract.bindings[0].obligations, [
    { lane_id: "promql.lane", context_name: "promql-check", merge_required: true, release_required: false },
  ]);
});

// --- buildImpactReport: unknown-input conservative widening --------------------

function stubRevExists(known) {
  return (ref) => known.has(ref);
}

test("a normal range reports the touched lanes' contracts, with unknown_input null", () => {
  const model = fixtureModel();
  const registry = fixtureRegistry();
  const snapshot = fixtureSnapshot();
  const report = buildImpactReport({
    model,
    registry,
    snapshot,
    repoRoot: REPO_ROOT,
    base: "base-ref",
    head: "head-ref",
    revExists: stubRevExists(new Set(["base-ref", "head-ref"])),
    changedFiles: () => ["internal/chplan/plan.go"],
    runGoList: stubGoList(SHARED_PIPELINE),
  });
  assert.equal(report.unknown_input, null);
  assert.deepEqual(report.contracts.map((c) => c.id), ["LOGQL-EXAMPLE", "PROMQL-EXAMPLE"]);
  assert.equal(report.touched_lane_count, 2);
  assert.equal(report.owner_lane_count, 2);
  assert.equal(report.known_nonimpact_file_count, 0);
});

test("KNOWN NONIMPACT: a lane declaring a literal '**' glob is not touched by a docs-only diff", () => {
  // Mirrors quickstart-canary.mjs's own consumer of
  // impact_selection.known_nonimpact_globs: without this filter, a lane
  // whose OWN declared package_globs is the literal wildcard "**" would
  // read as touched by any path string, docs included.
  const model = fixtureModel();
  const registry = {
    ...fixtureRegistry(),
    lanes: [...fixtureRegistry().lanes, { id: "lint.lane", package_globs: ["**"], build_tags: [] }],
    impact_selection: { known_nonimpact_globs: ["docs/**", "CLAUDE*"] },
  };
  const report = buildImpactReport({
    model,
    registry,
    snapshot: fixtureSnapshot(),
    repoRoot: REPO_ROOT,
    base: "base-ref",
    head: "head-ref",
    revExists: stubRevExists(new Set(["base-ref", "head-ref"])),
    changedFiles: () => ["docs/engine.md", "CLAUDE.md"],
    runGoList: stubGoList(SHARED_PIPELINE),
  });
  assert.equal(report.changed_file_count, 2);
  assert.equal(report.known_nonimpact_file_count, 2);
  assert.equal(report.touched_lane_count, 0);
  assert.deepEqual(report.contracts, []);
});

test("KNOWN NONIMPACT never suppresses a REAL trigger sitting alongside a docs change", () => {
  const model = fixtureModel();
  const registry = {
    ...fixtureRegistry(),
    impact_selection: { known_nonimpact_globs: ["docs/**"] },
  };
  const report = buildImpactReport({
    model,
    registry,
    snapshot: fixtureSnapshot(),
    repoRoot: REPO_ROOT,
    base: "base-ref",
    head: "head-ref",
    revExists: stubRevExists(new Set(["base-ref", "head-ref"])),
    changedFiles: () => ["docs/engine.md", "internal/promql/lower.go"],
    runGoList: stubGoList(SHARED_PIPELINE),
  });
  assert.equal(report.known_nonimpact_file_count, 1);
  assert.deepEqual(report.contracts.map((c) => c.id), ["PROMQL-EXAMPLE"]);
});

test("UNKNOWN INPUT: an unresolvable ref widens to every owner lane, never to nothing", () => {
  const model = fixtureModel();
  const registry = fixtureRegistry();
  const snapshot = fixtureSnapshot();
  const report = buildImpactReport({
    model,
    registry,
    snapshot,
    repoRoot: REPO_ROOT,
    base: "no-such-ref",
    head: "head-ref",
    revExists: stubRevExists(new Set(["head-ref"])),
    changedFiles: () => {
      throw new Error("should never be called when a ref does not resolve");
    },
    runGoList: stubGoList(SHARED_PIPELINE),
  });
  assert.match(report.unknown_input, /no-such-ref/);
  assert.equal(report.touched_lane_count, report.owner_lane_count);
  assert.deepEqual(report.contracts.map((c) => c.id).sort(), ["LOGQL-EXAMPLE", "PROMQL-EXAMPLE"]);
});

test("UNKNOWN INPUT: changedFiles throwing (a bad range) also widens rather than reporting nothing", () => {
  const model = fixtureModel();
  const registry = fixtureRegistry();
  const snapshot = fixtureSnapshot();
  const report = buildImpactReport({
    model,
    registry,
    snapshot,
    repoRoot: REPO_ROOT,
    base: "base-ref",
    head: "head-ref",
    revExists: stubRevExists(new Set(["base-ref", "head-ref"])),
    changedFiles: () => {
      throw new Error("git diff exploded");
    },
    runGoList: stubGoList(SHARED_PIPELINE),
  });
  assert.match(report.unknown_input, /git diff exploded/);
  assert.equal(report.touched_lane_count, report.owner_lane_count);
});

test("an import-graph failure on an otherwise-known range also widens, not narrows", () => {
  const model = fixtureModel();
  const registry = fixtureRegistry();
  const snapshot = fixtureSnapshot();
  const report = buildImpactReport({
    model,
    registry,
    snapshot,
    repoRoot: REPO_ROOT,
    base: "base-ref",
    head: "head-ref",
    revExists: stubRevExists(new Set(["base-ref", "head-ref"])),
    changedFiles: () => ["docs/engine.md"], // matches no declared glob at all
    runGoList: () => {
      throw new Error("go list: broken module");
    },
  });
  assert.equal(report.unknown_input, null); // the diff itself WAS computed
  assert.equal(report.touched_lane_count, report.owner_lane_count); // but the graph could not be, so: widen
  assert.deepEqual(report.contracts.map((c) => c.id).sort(), ["LOGQL-EXAMPLE", "PROMQL-EXAMPLE"]);
});

// --- Rendering -----------------------------------------------------------------

test("renderText names every impacted contract, its evidence, and the merge/release caveat", () => {
  const model = fixtureModel();
  const registry = fixtureRegistry();
  const snapshot = fixtureSnapshot();
  const report = buildImpactReport({
    model,
    registry,
    snapshot,
    repoRoot: REPO_ROOT,
    base: "base-ref",
    head: "head-ref",
    revExists: stubRevExists(new Set(["base-ref", "head-ref"])),
    changedFiles: () => ["internal/chplan/plan.go"],
    runGoList: stubGoList(SHARED_PIPELINE),
  });
  const text = renderText(report);
  assert.match(text, /PROMQL-EXAMPLE/);
  assert.match(text, /LOGQL-EXAMPLE/);
  assert.match(text, /required evidence classes: execution/);
  assert.equal(text.includes(MERGE_RELEASE_CAVEAT), true);
  assert.equal(text.includes(ADVERSARIAL_NOTE), true);
});

test("renderJSON round-trips through JSON.parse with the same contract set", () => {
  const model = fixtureModel();
  const registry = fixtureRegistry();
  const snapshot = fixtureSnapshot();
  const report = buildImpactReport({
    model,
    registry,
    snapshot,
    repoRoot: REPO_ROOT,
    base: "base-ref",
    head: "head-ref",
    revExists: stubRevExists(new Set(["base-ref", "head-ref"])),
    changedFiles: () => ["internal/chplan/plan.go"],
    runGoList: stubGoList(SHARED_PIPELINE),
  });
  const parsed = JSON.parse(renderJSON(report));
  assert.deepEqual(parsed.contracts.map((c) => c.id), ["LOGQL-EXAMPLE", "PROMQL-EXAMPLE"]);
});

// ---------------------------------------------------------------------------
// ACCEPTANCE: replay PR #2824's real file set against the REAL model,
// registry, snapshot and `go list` import graph (cerberus issue #3460's own
// "Replay #2902/#2895's shared-pipeline impact shape as a deterministic
// test"). File list verbatim, duplicated from merge-risk.test.mjs.
// ---------------------------------------------------------------------------

const PR_2824_FILES = [
  "cmd/cerberus/bootstrap_config_test.go",
  "cmd/cerberus/chopt_reprobe.go",
  "cmd/cerberus/main.go",
  "docs/clickhouse-optimizations.md",
  "docs/configuration.md",
  "internal/api/info/info.go",
  "internal/api/info/info_test.go",
  "internal/chclient/client.go",
  "internal/chclient/result_cache.go",
  "internal/chclient/result_cache_hit_integration_test.go",
  "internal/chclient/result_cache_metrics.go",
  "internal/chclient/result_cache_probe.go",
  "internal/chclient/result_cache_probe_integration_test.go",
  "internal/chclient/result_cache_probe_test.go",
  "internal/chclient/ts_grid_probe.go",
  "internal/chopt/capability.go",
  "internal/chopt/registry.go",
  "internal/chopt/resolve.go",
  "internal/chopt/resolve_test.go",
  "internal/config/config.go",
  "internal/config/envdocs.go",
  "internal/engine/query_settings_rules.go",
  "internal/engine/result_cache_test.go",
  "test/perf/solver_decision_ratchet_test.go",
];

function loadReal() {
  // Deferred, real imports of the actual model/registry/snapshot loaders —
  // done here rather than at module scope so the synthetic fixture tests
  // above never pay for disk I/O or a `go` toolchain they do not need.
  return import("./semantic-model.mjs").then(({ loadSemanticModel }) => {
    const model = loadSemanticModel("test/semantic", { root: REPO_ROOT });
    const registry = loadRegistry(".github/ci-lanes.json", { root: REPO_ROOT });
    const snapshot = validatePolicySnapshot(
      JSON.parse(readFileSync(resolve(REPO_ROOT, "test/semantic/policy-snapshot.json"), "utf8")),
    );
    return { model, registry, snapshot };
  });
}

test("ACCEPTANCE: PR #2824's file set reaches compatibility.loki through the DERIVED closure only", async () => {
  const { model, registry, snapshot } = await loadReal();
  const owners = [...ownerLaneIds(model, registry)].sort();
  const lanesById = new Map(registry.lanes.map((l) => [l.id, l]));
  const ownerLanes = owners.map((id) => lanesById.get(id)).filter(Boolean);
  const declared = new Map(ownerLanes.map((l) => [l.id, l.package_globs ?? []]));
  const closures = resolveLaneClosures(ownerLanes, { repoRoot: REPO_ROOT });
  assert.equal(closures.ok, true, closures.error);

  const touched = touchedLanes(PR_2824_FILES, ownerLanes, declared, closures);
  const loki = touched.get("compatibility.loki");
  assert.ok(loki, "compatibility.loki must be an owner lane in the real registry");
  assert.equal(loki.touched, true, "the shared pipeline this PR moved must reach the LogQL reference lane");
  assert.ok(
    loki.reasons.every((r) => r.kind === "derived"),
    "none of PR #2824's files match compatibility.loki’s own declared globs — every hit must be derived",
  );

  const contracts = impactedContracts(model, registry, snapshot, touched);
  const anchoring = contracts.find((c) => c.id === "LOGQL-LABEL-MATCHER-REGEX-ANCHORING");
  assert.ok(anchoring, "LOGQL-LABEL-MATCHER-REGEX-ANCHORING must be reported impacted");
  const binding = anchoring.bindings.find((b) => b.id === "BINDING-LOGQL-LABEL-MATCHER-ANCHORING-COMPAT");
  assert.ok(binding, "its compatibility binding must be present");
  const obligation = binding.obligations.find((o) => o.lane_id === "compatibility.loki");
  assert.deepEqual(obligation, {
    lane_id: "compatibility.loki",
    context_name: "compatibility/loki",
    merge_required: false,
    release_required: true,
  });
});
