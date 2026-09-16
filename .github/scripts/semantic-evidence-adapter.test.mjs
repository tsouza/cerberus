// semantic-evidence-adapter.test.mjs — node --test guard for the semantic
// evidence adapter (.github/scripts/lib/semantic-evidence-adapter.mjs,
// cerberus issue #3428). Pairs each of the issue's own acceptance criteria
// with a concrete positive and negative fixture, using the REAL repository
// tree wherever the claim is about real committed evidence (the property
// shape rosters, the TXTAR corpus, the surface-parity/rejection-parity
// ledgers, the QL feature inventory) and small temp-dir fixtures wherever
// the claim is about a hypothetical drift (a removed/renamed identity).
//
// Run as `node --test .github/scripts/semantic-evidence-adapter.test.mjs`
// from the repository root — the same convention semantic-model.test.mjs
// and ci-lane-contract.test.mjs already use.

import assert from "node:assert/strict";
import { test } from "node:test";
import { mkdtempSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { loadSemanticModel } from "./lib/semantic-model.mjs";
import {
  classifyObservedEvidence,
  classifyTestRef,
  diagnoseDanglingBindings,
  discoverPropertyRosterFamilies,
  loadOracleInventory,
  loadPropertyShapeExport,
  loadRejectionParityCatalogue,
  loadSurfaceParityInventory,
  loadTxtarFixture,
  parseTxtar,
  resolveBindingEvidence,
  resolveOracleInventoryEvidence,
  resolvePropertyShapeEvidence,
  resolveRejectionParityEvidence,
  resolveSurfaceParityEvidence,
  resolveTxtarEvidence,
} from "./lib/semantic-evidence-adapter.mjs";

const REPO_ROOT = process.cwd();

function tempDir(prefix) {
  return mkdtempSync(join(tmpdir(), prefix));
}

// A small, self-contained shape export doc, standing in for a real
// property-shape-export run — used everywhere a test only needs to prove
// resolution/dangling-detection logic, not the real Go toolchain.
function fakeShapeExport() {
  return {
    schema_version: 1,
    rosters: {
      "gen.PromQLShapeIDs": ["promql.instant.selector", "promql.instant.sum"],
      "gen.LogQLShapeIDs": ["logql.stream.selector"],
    },
  };
}

function fakeRunExporter(doc) {
  return () => doc;
}

// --- classifyTestRef ------------------------------------------------------

test("classifyTestRef dispatches each of the five schemes", () => {
  assert.equal(
    classifyTestRef("test/property/gen#promql.instant.selector").system,
    "property-shape",
  );
  assert.equal(classifyTestRef("test/spec/promql/foo.txtar").system, "txtar-fixture");
  assert.equal(
    classifyTestRef("test/spec/promql/foo.txtar#sql_optimized").system,
    "txtar-fixture",
  );
  assert.equal(
    classifyTestRef("test/surface-parity#promql/fn:acos").system,
    "surface-parity-symbol",
  );
  assert.equal(
    classifyTestRef("internal/promql/lower.go:lowerFoo#a1b2c3d4").system,
    "rejection-parity-site",
  );
  assert.equal(
    classifyTestRef("internal/promql/lower.go:lowerFoo#a1b2c3d4-2").system,
    "rejection-parity-site",
  );
  assert.equal(classifyTestRef("test/oracle/inventory#promql/agg:avg").system, "oracle-inventory-case");
});

test("classifyTestRef returns system:null for a test_ref outside these five systems", () => {
  // Real existing bindings.json test_refs for other verifiers (#3427's own
  // lane adapter, or a static-scan verifier) — not dangling, just not ours.
  assert.equal(classifyTestRef("compatibility/prometheus").system, null);
  assert.equal(classifyTestRef("internal/engine").system, null);
  assert.equal(classifyTestRef("").system, null);
  assert.equal(classifyTestRef(undefined).system, null);
});

// --- TXTAR parsing ---------------------------------------------------------

test("parseTxtar splits sections in order, trimming marker whitespace", () => {
  const text = "-- query.promql --\nup\n-- sql --\nSELECT 1\n";
  const sections = parseTxtar(text);
  assert.deepEqual(
    sections.map((s) => s.name),
    ["query.promql", "sql"],
  );
  assert.equal(sections[0].content, "up\n");
  assert.equal(sections[1].content, "SELECT 1\n");
});

test("parseTxtar discards preamble content before the first marker", () => {
  const sections = parseTxtar("ignored preamble\n-- a --\nbody\n");
  assert.equal(sections.length, 1);
  assert.equal(sections[0].name, "a");
});

// --- property-shape resolution ---------------------------------------------

test("resolvePropertyShapeEvidence resolves a real shape ID to its owning roster", () => {
  const r = resolvePropertyShapeEvidence(
    "test/property/gen#promql.instant.sum",
    fakeShapeExport(),
  );
  assert.equal(r.ok, true);
  assert.equal(r.detail.rosterCall, "gen.PromQLShapeIDs");
});

test("resolvePropertyShapeEvidence names the missing shape ID on drift", () => {
  const r = resolvePropertyShapeEvidence(
    "test/property/gen#promql.instant.does-not-exist",
    fakeShapeExport(),
  );
  assert.equal(r.ok, false);
  assert.equal(r.problems.length, 1);
  assert.match(r.problems[0], /promql\.instant\.does-not-exist/);
  assert.match(r.problems[0], /gen\.\*ShapeIDs/);
});

// --- TXTAR fixture resolution -----------------------------------------------

test("resolveTxtarEvidence resolves a real committed fixture with no section fragment", () => {
  const r = resolveTxtarEvidence(
    "test/spec/promql/map_key_order_sum_without_label.txtar",
    REPO_ROOT,
  );
  assert.equal(r.ok, true);
  assert.ok(r.detail.sections.includes("sql"));
  assert.ok(r.detail.sections.includes("sql_optimized"));
});

test("resolveTxtarEvidence resolves a claim scoped to the optimized-plan section", () => {
  const r = resolveTxtarEvidence(
    "test/spec/promql/map_key_order_sum_without_label.txtar#sql_optimized",
    REPO_ROOT,
  );
  assert.equal(r.ok, true);
  assert.equal(r.detail.section, "sql_optimized");
});

test("resolveTxtarEvidence names the missing fixture on removal/renaming", () => {
  const r = resolveTxtarEvidence("test/spec/promql/does_not_exist.txtar", REPO_ROOT);
  assert.equal(r.ok, false);
  assert.match(r.problems[0], /does_not_exist\.txtar/);
  assert.match(r.problems[0], /does not exist/);
});

test("resolveTxtarEvidence names the missing section when the fixture exists but lacks it", () => {
  const r = resolveTxtarEvidence(
    "test/spec/promql/map_key_order_sum_without_label.txtar#no_such_section",
    REPO_ROOT,
  );
  assert.equal(r.ok, false);
  assert.match(r.problems[0], /no_such_section/);
  assert.match(r.problems[0], /sections present/);
});

test("resolveTxtarEvidence: a fixture MOVING to a new path only requires updating the test_ref", () => {
  const dir = tempDir("cerberus-txtar-move-");
  try {
    mkdirSync(join(dir, "test/spec/promql"), { recursive: true });
    const body = "-- query.promql --\nup\n-- sql --\nSELECT 1\n";
    writeFileSync(join(dir, "test/spec/promql/old_name.txtar"), body);

    const before = resolveTxtarEvidence("test/spec/promql/old_name.txtar", dir);
    assert.equal(before.ok, true);
    const beforeMove = resolveTxtarEvidence("test/spec/promql/new_name.txtar", dir);
    assert.equal(beforeMove.ok, false, "the new path must not resolve before the file moves");

    // Simulate the source move: same content, new path. No contract touched.
    mkdirSync(join(dir, "test/spec/promql"), { recursive: true });
    writeFileSync(join(dir, "test/spec/promql/new_name.txtar"), body);

    const afterOldPath = resolveTxtarEvidence("test/spec/promql/old_name.txtar", dir);
    const afterNewPath = resolveTxtarEvidence("test/spec/promql/new_name.txtar", dir);
    assert.equal(afterOldPath.ok, true, "old path still resolves until its own file is deleted");
    assert.equal(afterNewPath.ok, true, "new path resolves once the binding's test_ref is updated to it");
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

// --- surface-parity resolution -----------------------------------------------

test("resolveSurfaceParityEvidence resolves against the real merged inventory", () => {
  const entries = loadSurfaceParityInventory(REPO_ROOT);
  assert.ok(entries.length > 0, "surface-parity inventory must not be empty");
  const sample = entries[0];
  const testRef = `test/surface-parity#${sample.head}/${sample.symbol}`;
  const r = resolveSurfaceParityEvidence(testRef, entries);
  assert.equal(r.ok, true);
  assert.equal(r.detail.symbol, sample.symbol);
});

test("resolveSurfaceParityEvidence names the missing symbol on drift", () => {
  const r = resolveSurfaceParityEvidence(
    "test/surface-parity#promql/fn:definitely-not-a-real-symbol",
    loadSurfaceParityInventory(REPO_ROOT),
  );
  assert.equal(r.ok, false);
  assert.match(r.problems[0], /fn:definitely-not-a-real-symbol/);
});

// --- rejection-parity resolution ----------------------------------------------

test("resolveRejectionParityEvidence resolves against the real merged catalogue", () => {
  const entries = loadRejectionParityCatalogue(REPO_ROOT);
  assert.ok(entries.length > 0, "rejection-parity catalogue must not be empty");
  const sample = entries[0];
  const r = resolveRejectionParityEvidence(sample.site, entries);
  assert.equal(r.ok, true);
  assert.equal(r.detail.site, sample.site);
});

test("resolveRejectionParityEvidence names the missing site on drift", () => {
  const r = resolveRejectionParityEvidence(
    "internal/promql/lower.go:notARealFunction#deadbeef",
    loadRejectionParityCatalogue(REPO_ROOT),
  );
  assert.equal(r.ok, false);
  assert.match(r.problems[0], /notARealFunction#deadbeef/);
});

// --- oracle-inventory resolution -----------------------------------------------

test("resolveOracleInventoryEvidence resolves against the real QL feature inventory", () => {
  const inv = loadOracleInventory(REPO_ROOT);
  assert.ok(inv.promql.length > 0, "promql feature inventory must not be empty");
  const sample = inv.promql[0];
  const r = resolveOracleInventoryEvidence(`test/oracle/inventory#promql/${sample.id}`, inv);
  assert.equal(r.ok, true);
  assert.equal(r.detail.id, sample.id);
});

test("resolveOracleInventoryEvidence names the missing row on drift", () => {
  const inv = loadOracleInventory(REPO_ROOT);
  const r = resolveOracleInventoryEvidence("test/oracle/inventory#promql/not-a-real-row", inv);
  assert.equal(r.ok, false);
  assert.match(r.problems[0], /not-a-real-row/);
});

// --- classifyObservedEvidence -----------------------------------------------

test("classifyObservedEvidence: pass and fail are observed; error/missing are not", () => {
  assert.equal(classifyObservedEvidence({ result: "pass" }).observed, true);
  assert.equal(classifyObservedEvidence({ result: "fail" }).observed, true);
  assert.equal(classifyObservedEvidence({ result: "error" }).observed, false);
  assert.equal(classifyObservedEvidence(null).observed, false);
  assert.equal(classifyObservedEvidence({}).observed, false);
});

// --- resolveBindingEvidence / diagnoseDanglingBindings ------------------------

test("resolveBindingEvidence returns system:null for a binding outside the five systems", () => {
  const r = resolveBindingEvidence({ test_ref: "internal/engine" }, {});
  assert.equal(r.system, null);
  assert.equal(r.ok, true);
});

test("diagnoseDanglingBindings names the binding id, contract and missing identity", () => {
  const model = {
    bindings: new Map([
      [
        "BINDING-X",
        {
          id: "BINDING-X",
          contract: "PROMQL-X",
          at: "bindings.json.bindings[0]",
          test_ref: "test/property/gen#promql.instant.does-not-exist",
        },
      ],
    ]),
  };
  const problems = diagnoseDanglingBindings(model, { shapeExport: fakeShapeExport() });
  assert.equal(problems.length, 1);
  assert.match(problems[0], /BINDING-X/);
  assert.match(problems[0], /PROMQL-X/);
  assert.match(problems[0], /promql\.instant\.does-not-exist/);
});

test("diagnoseDanglingBindings reports nothing for a binding outside the five systems", () => {
  const model = {
    bindings: new Map([
      ["BINDING-Y", { id: "BINDING-Y", contract: "ARCH-Y", at: "x", test_ref: "internal/engine" }],
    ]),
  };
  assert.deepEqual(diagnoseDanglingBindings(model, {}), []);
});

// --- Contract IDs are independent of where their evidence lives --------------

test("a binding's test_ref move never requires touching the contract it evidences", () => {
  const model = loadSemanticModel("test/semantic", { root: REPO_ROOT });
  const contract = model.contracts.get("PROMQL-RANGE-VECTOR-ALIGNMENT");
  assert.ok(contract, "fixture contract must exist in the real committed model");
  const before = JSON.stringify({ id: contract.id, statement: contract.statement });

  // Resolve the SAME contract's binding evidence against two different
  // test_ref values (a "before move" and an "after move" fixture path) —
  // the contract object itself is never touched by either resolution.
  const binding = [...model.bindings.values()].find((b) => b.contract === contract.id);
  assert.ok(binding, "fixture binding must exist");
  resolveTxtarEvidence(binding.test_ref, REPO_ROOT);
  resolveTxtarEvidence("test/spec/promql/some_other_fixture.txtar", REPO_ROOT);

  const after = JSON.stringify({ id: contract.id, statement: contract.statement });
  assert.equal(before, after, "resolving evidence must never mutate the contract it evidences");
});

// --- Property-shape roster family discovery -----------------------------------

test("discoverPropertyRosterFamilies finds exactly the real six live-roster families", () => {
  const families = discoverPropertyRosterFamilies({ repoRoot: REPO_ROOT });
  const names = families.map((f) => f.family).sort();
  assert.deepEqual(names, [
    "instant_window",
    "logql",
    "promql",
    "promql_exp_histogram",
    "promql_range",
    "traceql",
  ]);
  for (const f of families) {
    assert.equal(f.hasLiveRoster, true, `${f.family} must have live-roster wiring`);
    assert.equal(f.hasRandomRunner, true, `${f.family} must also have a randomized-runner call`);
    assert.deepEqual(f.problems, []);
    assert.ok(f.shapeIDs.length > 0, `${f.family} roster must be non-empty`);
  }
});

test("discoverPropertyRosterFamilies excludes a package-internal file that merely defines the runners", () => {
  // test/property/framework_validator_test.go declares `package property`
  // (not `property_test`) and calls the bare RunShapeExamples/RunShapeCases
  // symbols directly against synthetic in-test rosters — it must never be
  // mistaken for a seventh live-roster family.
  const families = discoverPropertyRosterFamilies({ repoRoot: REPO_ROOT });
  assert.ok(!families.some((f) => f.family === "framework_validator"));
});

test("discoverPropertyRosterFamilies: removing a family's file drops it from discovery", () => {
  const dir = tempDir("cerberus-roster-families-");
  try {
    mkdirSync(join(dir, "test/property"), { recursive: true });
    const wired = [
      "package property_test",
      "",
      "func TestPromQL_PropertyShapeRoster(t *testing.T) {",
      "\tproperty.RunShapeExamples(",
      "\t\tt,",
      "\t\tgen.PromQLShapeIDs(),",
      "\t)",
      "}",
      "",
      "func TestPromQL_Property_FromScratch(t *testing.T) {",
      "\tproperty.Run(t, property.Config{})",
      "}",
      "",
    ].join("\n");
    writeFileSync(join(dir, "test/property/promql_test.go"), wired);

    const before = discoverPropertyRosterFamilies({
      repoRoot: dir,
      runExporter: fakeRunExporter(fakeShapeExport()),
    });
    assert.deepEqual(
      before.map((f) => f.family),
      ["promql"],
    );

    rmSync(join(dir, "test/property/promql_test.go"));
    const after = discoverPropertyRosterFamilies({
      repoRoot: dir,
      runExporter: fakeRunExporter(fakeShapeExport()),
    });
    assert.deepEqual(after, []);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("discoverPropertyRosterFamilies flags a live-roster file missing its randomized runner", () => {
  const dir = tempDir("cerberus-roster-families-");
  try {
    mkdirSync(join(dir, "test/property"), { recursive: true });
    const wiredNoRandom = [
      "package property_test",
      "",
      "func TestLogQL_PropertyShapeRoster(t *testing.T) {",
      "\tproperty.RunShapeExamples(",
      "\t\tt,",
      "\t\tgen.LogQLShapeIDs(),",
      "\t)",
      "}",
      "",
    ].join("\n");
    writeFileSync(join(dir, "test/property/logql_test.go"), wiredNoRandom);

    const families = discoverPropertyRosterFamilies({
      repoRoot: dir,
      runExporter: fakeRunExporter(fakeShapeExport()),
    });
    assert.equal(families.length, 1);
    assert.equal(families[0].hasRandomRunner, false);
    assert.match(families[0].problems.join("\n"), /randomized-runner/);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

// --- Go export helper: real invocation, determinism, no build tag ------------

test("loadPropertyShapeExport (real go run) returns the six known rosters, deterministically", () => {
  const first = loadPropertyShapeExport({ repoRoot: REPO_ROOT });
  const second = loadPropertyShapeExport({ repoRoot: REPO_ROOT });
  assert.deepEqual(first, second, "the exporter must be a deterministic function of current source");
  assert.deepEqual(
    Object.keys(first.rosters).sort(),
    [
      "gen.ExpHistogramShapeIDs",
      "gen.InstantWindowShapeIDs",
      "gen.LogQLShapeIDs",
      "gen.PromQLRangeShapeIDs",
      "gen.PromQLShapeIDs",
      "gen.TraceQLShapeIDs",
    ],
  );
  for (const ids of Object.values(first.rosters)) {
    assert.ok(Array.isArray(ids) && ids.length > 0);
  }
});

test("the default property-shape-export runner never passes a build tag", () => {
  // Structural check on the adapter's own source: the exporter binary is
  // untagged by design (see its own header comment), so the invocation that
  // runs it must never gate that behind `-tags`, or an ordinary `go run`
  // from a caller who forgets the flag would silently miss it.
  const src = readFileSync(
    join(REPO_ROOT, ".github/scripts/lib/semantic-evidence-adapter.mjs"),
    "utf8",
  );
  assert.ok(!src.includes("-tags"), "the exporter invocation must carry no build tag");
});

// --- Read-only-by-construction: the adapter never writes ----------------------

test("the adapter module source contains no write APIs", () => {
  const src = readFileSync(
    join(REPO_ROOT, ".github/scripts/lib/semantic-evidence-adapter.mjs"),
    "utf8",
  );
  for (const forbidden of ["writeFileSync", "appendFileSync", "rmSync", "unlinkSync", "copyFileSync"]) {
    assert.ok(!src.includes(forbidden), `adapter must never call ${forbidden}`);
  }
});

test("regenerating a fixture's own bytes never touches contracts.json (byte-identical before/after)", () => {
  const contractsPath = join(REPO_ROOT, "test/semantic/contracts.json");
  const before = readFileSync(contractsPath, "utf8");

  // Exercise every read-only entry point this module exposes against the
  // real fixture BINDING-PROMQL-RANGE-ALIGNMENT-SPEC references, plus a
  // full model load+diagnose pass — none of it is capable of writing to
  // contracts.json by construction (see the meta-test above), and this
  // proves it end to end against the real files rather than only by source
  // inspection.
  const model = loadSemanticModel("test/semantic", { root: REPO_ROOT });
  const indices = {
    repoRoot: REPO_ROOT,
    shapeExport: loadPropertyShapeExport({ repoRoot: REPO_ROOT }),
    surfaceParityEntries: loadSurfaceParityInventory(REPO_ROOT),
    rejectionCatalogueEntries: loadRejectionParityCatalogue(REPO_ROOT),
    oracleInventory: loadOracleInventory(REPO_ROOT),
  };
  diagnoseDanglingBindings(model, indices);

  const after = readFileSync(contractsPath, "utf8");
  assert.equal(before, after);
});

// --- loadTxtarFixture / loaders against real files ----------------------------

test("loadTxtarFixture reads a real fixture and reports its sections", () => {
  const { sections } = loadTxtarFixture(REPO_ROOT, "test/spec/promql/map_key_order_sum_without_label.txtar");
  assert.ok(sections.some((s) => s.name === "query.promql"));
  assert.ok(sections.some((s) => s.name === "sql_optimized"));
  assert.ok(sections.some((s) => s.name === "args_optimized"));
});

test("loadParityEnrolmentBaseline / loadOracleInventory / loadSurfaceParityInventory / loadRejectionParityCatalogue read real committed ledgers", async () => {
  const { loadParityEnrolmentBaseline } = await import("./lib/semantic-evidence-adapter.mjs");
  for (const head of ["promql", "logql", "traceql"]) {
    const roster = loadParityEnrolmentBaseline(REPO_ROOT, head);
    assert.ok(Array.isArray(roster));
  }
  const inv = loadOracleInventory(REPO_ROOT);
  assert.ok(inv.promql.length > 0 && inv.logql.length > 0 && inv.traceql.length > 0);
  assert.ok(loadSurfaceParityInventory(REPO_ROOT).length > 0);
  assert.ok(loadRejectionParityCatalogue(REPO_ROOT).length > 0);
});
