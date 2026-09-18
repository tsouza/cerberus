// semantic-mutation.test.mjs — node --test guard for
// test/semantic/mutants/*.json's loader/validator and for
// lib/semantic-mutation.mjs's execution engine (issue #3448). Pairs every
// "must FAIL"/"must abort" acceptance criterion with a positive control that
// stays green, against small in-memory fixtures, plus an end-to-end pass
// over the real committed test/semantic/mutants/ directory and real
// git/filesystem operations (applyTransformation's own `git apply`
// integration tests, and runGoTest's exact argv construction against an
// injected spawnFn) — but never a real `go` invocation: every runMutant()
// orchestration test injects a fake runGoTestFn, exactly so this suite runs
// without a Go toolchain (confirmed by running it with `go` removed from
// PATH). The one place a real `go test -overlay` actually runs, against the
// real committed corpus, is .github/scripts/semantic-mutation-corpus.mjs —
// a Go-equipped CI job runs that script, not this file (see its own header
// for why: an ineffective -overlay argument and a genuine survived mutant
// are otherwise indistinguishable to anything this pure suite alone runs).

import assert from "node:assert/strict";
import { test } from "node:test";
import { mkdtempSync, mkdirSync, rmSync, writeFileSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { EventEmitter } from "node:events";

import {
  CLASSIFICATIONS,
  DEFAULT_MUTANTS_DIR,
  MUTANT_SCHEMA_VERSION,
  REPIN_RECIPE,
  aggregateClassifications,
  applyTransformation,
  classifyGoTestOutput,
  createScratchDir,
  loadMutants,
  renderMutantsSummary,
  runGoTest,
  runMutant,
  scratchRootFor,
  selectDetectors,
  sha256Hex,
  validateMutantRecord,
} from "./lib/semantic-mutation.mjs";
import { patchFingerprints } from "./lib/semantic-fingerprint.mjs";
import { repinMutantRecord } from "./semantic-repin.mjs";
import { SemanticModelError } from "./lib/semantic-model.mjs";

const REPO_ROOT = process.cwd();
// Real, committed, always-present files — the same discipline
// semantic-counterexamples.test.mjs uses for its own path fixtures.
const REAL_TARGET_PATH = "test/semantic/mutants/testdata/fixtures/arith.go";
const REAL_PATCH_PATH = "test/semantic/mutants/patches/killed-invert.patch";
// The pins are pure functions of the patch file (lib/semantic-fingerprint.mjs),
// computed here from the committed patch rather than copied out of a
// committed record, so no test below depends on a JSON file's own value to
// define what "correct" means.
const REAL_PINS = patchFingerprints(readFileSync(join(REPO_ROOT, REAL_PATCH_PATH), "utf8"));

function exampleMutantRecord(overrides = {}) {
  return {
    schema_version: MUTANT_SCHEMA_VERSION,
    id: "MUTANT-SYNTH-EXAMPLE",
    title: "example mutant",
    synthetic: true,
    synthetic_rationale: "unit-test fixture",
    violated_contracts: ["SYNTHETIC-EXAMPLE"],
    transformation: {
      target_path: REAL_TARGET_PATH,
      patch_path: REAL_PATCH_PATH,
      ...REAL_PINS,
    },
    detectors: [
      {
        id: "example-detector",
        package: "./test/semantic/mutants/testdata/fixtures",
        test_run: "^TestExample$",
        build_tags: [],
        timeout_seconds: 15,
        evidence_kind: "execution",
      },
    ],
    expected_detection: "killed",
    equivalence_review: null,
    isolation: { requires_chdb: false, memory_max: "1GiB", memory_hold: "150s" },
    notes: null,
    ...overrides,
  };
}

function validate(record, filename = "MUTANT-SYNTH-EXAMPLE.json", opts = {}) {
  const problems = [];
  const id = validateMutantRecord(record, filename, problems, { root: REPO_ROOT, ...opts });
  return { id, problems };
}

// --- Record schema -----------------------------------------------------

test("valid synthetic record passes with no problems", () => {
  const { id, problems } = validate(exampleMutantRecord());
  assert.equal(id, "MUTANT-SYNTH-EXAMPLE");
  assert.deepEqual(problems, []);
});

// --- Detector evidence kind ---------------------------------------------
//
// A detector that kills through a golden TEXT comparison (a TXTAR fixture's
// sql/args/chplan sections, checked before the chDB round trip is reached)
// fires on any change to the emitted SQL, including a semantics-preserving
// one; it says nothing about whether a semantic verifier would catch the
// bug. The record has to say which kind each detector is, so the cohort
// report can keep the two rates apart.

test("a detector must declare its evidence_kind, from the closed vocabulary", () => {
  const record = exampleMutantRecord();
  delete record.detectors[0].evidence_kind;
  const { problems } = validate(record);
  assert.ok(problems.some((p) => p.includes("detectors[0].evidence_kind is required")), problems.join("; "));

  const bad = exampleMutantRecord();
  bad.detectors[0].evidence_kind = "vibes";
  const { problems: badProblems } = validate(bad);
  assert.ok(badProblems.some((p) => p.includes("detectors[0].evidence_kind must be one of")), badProblems.join("; "));
});

test("a TestLower fixture detector cannot declare execution evidence: spec.Match runs before the chDB round trip", () => {
  const record = exampleMutantRecord();
  record.detectors[0].test_run = "^TestLower$/^some_fixture$";
  record.detectors[0].evidence_kind = "execution";
  const { problems } = validate(record);
  assert.ok(problems.some((p) => p.includes("golden-text")), problems.join("; "));

  record.detectors[0].evidence_kind = "golden-text";
  assert.deepEqual(validate(record).problems, []);
});

test("rejects an unknown top-level key", () => {
  const { problems } = validate({ ...exampleMutantRecord(), extra: true });
  assert.ok(problems.some((p) => p.includes(".extra is unknown")));
});

test("rejects a wrong schema_version (a schema-1 record with the whole-file fingerprints is rejected)", () => {
  const { problems } = validate(exampleMutantRecord({ schema_version: MUTANT_SCHEMA_VERSION - 1 }));
  assert.ok(problems.some((p) => p.includes(`schema_version must be ${MUTANT_SCHEMA_VERSION}`)));
});

test("filename must match <id>.json", () => {
  const { problems } = validate(exampleMutantRecord(), "WRONG-NAME.json");
  assert.ok(problems.some((p) => p.includes("filename must be <id>.json")));
});

test("synthetic=true requires a non-null synthetic_rationale", () => {
  const { problems } = validate(exampleMutantRecord({ synthetic_rationale: null }));
  assert.ok(problems.some((p) => p.includes("synthetic_rationale is required")));
});

test("synthetic=true requires SYNTHETIC- prefixed violated_contracts", () => {
  const { problems } = validate(exampleMutantRecord({ violated_contracts: ["PROMQL-REAL"] }));
  assert.ok(problems.some((p) => p.includes("must start with SYNTHETIC-")));
});

test("synthetic=false rejects a SYNTHETIC- placeholder and cross-checks real contracts", () => {
  const { problems } = validate(
    exampleMutantRecord({
      synthetic: false,
      synthetic_rationale: null,
      violated_contracts: ["ARCH-TYPED-SQL-ONLY"],
    }),
    "MUTANT-SYNTH-EXAMPLE.json",
    { contractIds: new Set(["ARCH-TYPED-SQL-ONLY"]) },
  );
  assert.deepEqual(problems, []);
});

test("synthetic=false rejects a contract absent from the live model", () => {
  const { problems } = validate(
    exampleMutantRecord({
      synthetic: false,
      synthetic_rationale: null,
      violated_contracts: ["ARCH-DOES-NOT-EXIST"],
    }),
    "MUTANT-SYNTH-EXAMPLE.json",
    { contractIds: new Set(["ARCH-TYPED-SQL-ONLY"]) },
  );
  assert.ok(problems.some((p) => p.includes("references unknown contract")));
});

test("rejects a non-existent target_path/patch_path", () => {
  const { problems } = validate(
    exampleMutantRecord({
      transformation: {
        ...exampleMutantRecord().transformation,
        target_path: "test/semantic/mutants/testdata/fixtures/does-not-exist.go",
      },
    }),
  );
  assert.ok(problems.some((p) => p.includes("does not exist on disk")));
});

test("rejects a malformed pre_image_fingerprint / post_image_fingerprint", () => {
  for (const field of ["pre_image_fingerprint", "post_image_fingerprint"]) {
    const { problems } = validate(
      exampleMutantRecord({
        transformation: { ...exampleMutantRecord().transformation, [field]: "not-hex" },
      }),
    );
    assert.ok(problems.some((p) => p.includes(field)), field);
  }
});

test("rejects the retired whole-file fingerprint fields and linked_issue (schema 2 dropped them)", () => {
  const stale = exampleMutantRecord({
    transformation: { ...exampleMutantRecord().transformation, source_fingerprint: "a".repeat(64) },
    linked_issue: null,
  });
  const { problems } = validate(stale);
  assert.ok(problems.some((p) => p.includes("source_fingerprint")));
  assert.ok(problems.some((p) => p.includes("linked_issue")));
});

test("rejects zero detectors", () => {
  const { problems } = validate(exampleMutantRecord({ detectors: [] }));
  assert.ok(problems.some((p) => p.includes("detectors must not be empty")));
});

test("rejects duplicate detector ids within one record", () => {
  const d = exampleMutantRecord().detectors[0];
  const { problems } = validate(exampleMutantRecord({ detectors: [d, { ...d }] }));
  assert.ok(problems.some((p) => p.includes("is a duplicate within this record")));
});

test("expected_detection equivalent-reviewed requires a non-null equivalence_review", () => {
  const { problems } = validate(exampleMutantRecord({ expected_detection: "equivalent-reviewed" }));
  assert.ok(problems.some((p) => p.includes("equivalence_review is required")));
});

test("equivalence_review must be null unless expected_detection is equivalent-reviewed", () => {
  const { problems } = validate(
    exampleMutantRecord({
      equivalence_review: { reviewer: "x", reviewed_at: "2026-01-01T00:00:00Z", rationale: "y" },
    }),
  );
  assert.ok(problems.some((p) => p.includes("must be null unless expected_detection")));
});

test("equivalence_review carries no fingerprint of its own: the record's pre-image pin is what ties it to the patch", () => {
  const { problems } = validate(
    exampleMutantRecord({
      expected_detection: "equivalent-reviewed",
      equivalence_review: {
        reviewer: "x",
        reviewed_at: "2026-01-01T00:00:00Z",
        source_fingerprint: REAL_PINS.pre_image_fingerprint,
        rationale: "y",
      },
    }),
  );
  assert.ok(problems.some((p) => p.includes("equivalence_review") && p.includes("source_fingerprint")));
});

test("rejects a malformed isolation.memory_max", () => {
  const { problems } = validate(
    exampleMutantRecord({
      isolation: { requires_chdb: false, memory_max: "1gigabyte", memory_hold: "150s" },
    }),
  );
  assert.ok(problems.some((p) => p.includes("memory_max")));
});

test("a non-synthetic record declaring a bare survived is rejected (invariant 7)", () => {
  const { problems } = validate(
    exampleMutantRecord({
      synthetic: false,
      synthetic_rationale: null,
      violated_contracts: ["ARCH-TYPED-SQL-ONLY"],
      expected_detection: "survived",
    }),
    "MUTANT-SYNTH-EXAMPLE.json",
    { contractIds: new Set(["ARCH-TYPED-SQL-ONLY"]) },
  );
  assert.ok(
    problems.some(
      (p) => p.includes("expected_detection") && p.includes("survived") && p.includes("invariant 7"),
    ),
  );
});

test("a non-synthetic record may still declare killed or equivalent-reviewed", () => {
  const { problems: killedProblems } = validate(
    exampleMutantRecord({
      synthetic: false,
      synthetic_rationale: null,
      violated_contracts: ["ARCH-TYPED-SQL-ONLY"],
      expected_detection: "killed",
    }),
    "MUTANT-SYNTH-EXAMPLE.json",
    { contractIds: new Set(["ARCH-TYPED-SQL-ONLY"]) },
  );
  assert.deepEqual(killedProblems, []);

  const { problems: equivalentProblems } = validate(
    exampleMutantRecord({
      synthetic: false,
      synthetic_rationale: null,
      violated_contracts: ["ARCH-TYPED-SQL-ONLY"],
      expected_detection: "equivalent-reviewed",
      equivalence_review: { reviewer: "x", reviewed_at: "2026-01-01T00:00:00Z", rationale: "y" },
    }),
    "MUTANT-SYNTH-EXAMPLE.json",
    { contractIds: new Set(["ARCH-TYPED-SQL-ONLY"]) },
  );
  assert.deepEqual(equivalentProblems, []);
});

test("a synthetic record may still declare a bare survived (its whole purpose)", () => {
  const { problems } = validate(exampleMutantRecord({ expected_detection: "survived" }));
  assert.deepEqual(problems, []);
});

// --- loadMutants: end-to-end over the real committed corpus -------------

test("loadMutants: the real committed test/semantic/mutants/ corpus is valid", () => {
  const records = loadMutants(DEFAULT_MUTANTS_DIR, { root: REPO_ROOT });
  assert.equal(records.size, 13);
  for (const c of CLASSIFICATIONS) {
    assert.ok(
      [...records.values()].some((r) => r.expected_detection === c),
      `no committed record declares expected_detection ${c}`,
    );
  }
});

test("loadMutants: a duplicate id across two files is rejected", () => {
  const dir = mkdtempSync(join(tmpdir(), "semantic-mutants-fixture-"));
  try {
    const record = exampleMutantRecord();
    writeFileSync(join(dir, "MUTANT-SYNTH-EXAMPLE.json"), JSON.stringify(record));
    writeFileSync(join(dir, "MUTANT-SYNTH-EXAMPLE-2.json"), JSON.stringify(record));
    assert.throws(() => loadMutants(dir, { root: REPO_ROOT }), SemanticModelError);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("renderMutantsSummary counts by expected_detection", () => {
  const records = loadMutants(DEFAULT_MUTANTS_DIR, { root: REPO_ROOT });
  const summary = renderMutantsSummary(records);
  assert.match(summary, /mutants: \*\*13\*\*/);
});

// --- classifyGoTestOutput (pure) -----------------------------------------

test("classifyGoTestOutput: timedOut always wins", () => {
  assert.equal(
    classifyGoTestOutput({ exitCode: null, signal: "SIGKILL", stdout: "", timedOut: true }),
    "timeout",
  );
});

test("classifyGoTestOutput: an external signal is infrastructure-error", () => {
  assert.equal(
    classifyGoTestOutput({ exitCode: null, signal: "SIGSEGV", stdout: "", timedOut: false }),
    "infrastructure-error",
  );
});

test("classifyGoTestOutput: [build failed] is build-failed even though it exits non-zero", () => {
  const stdout = "# example.com/pkg\n./file.go:1:1: syntax error\nFAIL\texample.com/pkg [build failed]\n";
  assert.equal(
    classifyGoTestOutput({ exitCode: 2, signal: null, stdout, timedOut: false }),
    "build-failed",
  );
});

test("classifyGoTestOutput: exit 0 with PASS is survived", () => {
  const stdout = "=== RUN   TestX\n--- PASS: TestX (0.00s)\nPASS\nok  \tpkg\t0.01s\n";
  assert.equal(classifyGoTestOutput({ exitCode: 0, signal: null, stdout, timedOut: false }), "survived");
});

test("classifyGoTestOutput: exit 0 with no PASS marker is infrastructure-error, not assumed survived", () => {
  assert.equal(
    classifyGoTestOutput({ exitCode: 0, signal: null, stdout: "", timedOut: false }),
    "infrastructure-error",
  );
});

test("classifyGoTestOutput: a well-formed --- FAIL: line is killed", () => {
  const stdout = "=== RUN   TestX\n--- FAIL: TestX (0.00s)\nFAIL\nFAIL\tpkg\t0.01s\n";
  assert.equal(classifyGoTestOutput({ exitCode: 1, signal: null, stdout, timedOut: false }), "killed");
});

test("classifyGoTestOutput: a crash is never a kill — a bare package-level FAIL with no --- FAIL: line is infrastructure-error", () => {
  // This is exactly the os.Exit() shape: go test's own driver notices the
  // binary exited non-zero and prints the package-level FAIL trailer, but no
  // per-test --- FAIL: line was ever printed because testing's own harness
  // never got a chance to adjudicate anything.
  const stdout = "=== RUN   TestX\nFAIL\tpkg\t0.01s\nFAIL\n";
  assert.equal(
    classifyGoTestOutput({ exitCode: 1, signal: null, stdout, timedOut: false }),
    "infrastructure-error",
  );
});

// --- aggregateClassifications (pure) -------------------------------------

test("aggregateClassifications: killed from any detector wins over survived", () => {
  assert.equal(aggregateClassifications(["survived", "killed", "survived"]), "killed");
});

// A detector whose harness never adjudicated (build-failed / timeout /
// infrastructure-error) is an INCOMPLETE measurement on that detector. It
// must never be hidden behind a sibling detector's clean PASS (which,
// with a non-null equivalence_review, would then be promoted to
// equivalent-reviewed and exit 0) nor behind a sibling's kill.
test("aggregateClassifications: a harness outcome on any detector wins over survived", () => {
  assert.equal(aggregateClassifications(["survived", "build-failed"]), "build-failed");
  assert.equal(aggregateClassifications(["survived", "timeout"]), "timeout");
  assert.equal(aggregateClassifications(["survived", "infrastructure-error"]), "infrastructure-error");
});

test("aggregateClassifications: a harness outcome on any detector wins over killed", () => {
  assert.equal(aggregateClassifications(["killed", "infrastructure-error"]), "infrastructure-error");
  assert.equal(aggregateClassifications(["killed", "build-failed"]), "build-failed");
  assert.equal(aggregateClassifications(["survived", "killed", "timeout"]), "timeout");
});

test("runMutant: a build failure on one detector is never masked by a sibling's PASS plus an equivalence review", async () => {
  const scratchRoot = mkdtempSync(join(tmpdir(), "semantic-mutation-scratch-"));
  const scratchDir = createScratchDir(scratchRoot);
  try {
    const base = exampleMutantRecord();
    const record = exampleMutantRecord({
      expected_detection: "equivalent-reviewed",
      equivalence_review: { reviewer: "test-suite", reviewed_at: "2026-01-01T00:00:00Z", rationale: "test double" },
      detectors: [
        { ...base.detectors[0], id: "passes", test_run: "^TestPasses$" },
        { ...base.detectors[0], id: "does-not-compile", test_run: "^TestDoesNotCompile$" },
      ],
    });
    const result = await runMutant({
      record,
      root: REPO_ROOT,
      scratchDir,
      runGoTestFn: async ({ overlayPath, testRun }) => {
        if (overlayPath === null || testRun === "^TestPasses$") return fakeCleanPass();
        return { exitCode: 1, signal: null, stdout: "FAIL\tpkg [build failed]\n", stderr: "", timedOut: false, durationMs: 1 };
      },
    });
    assert.equal(result.status, "build-failed");
  } finally {
    rmSync(scratchRoot, { recursive: true, force: true });
  }
});

test("aggregateClassifications: an all-equal set collapses to that value", () => {
  assert.equal(aggregateClassifications(["survived", "survived"]), "survived");
});

test("aggregateClassifications: throws on an empty list", () => {
  assert.throws(() => aggregateClassifications([]));
});

// --- selectDetectors (pure) -----------------------------------------------

test("selectDetectors: returns every detector when none named", () => {
  const record = exampleMutantRecord();
  assert.deepEqual(selectDetectors(record, undefined), record.detectors);
});

test("selectDetectors: filters to the named detector", () => {
  const record = exampleMutantRecord();
  assert.deepEqual(selectDetectors(record, "example-detector"), record.detectors);
});

test("selectDetectors: throws (a hard usage error, not a classification) when the named detector is unknown", () => {
  const record = exampleMutantRecord();
  assert.throws(() => selectDetectors(record, "no-such-detector"), /no detector named/);
});

test("selectDetectors: throws when a record carries zero detectors", () => {
  const record = exampleMutantRecord({ detectors: [] });
  assert.throws(() => selectDetectors(record, undefined), /declares zero detectors/);
});

// --- applyTransformation: fail-closed on fingerprint mismatch -------------

test("applyTransformation: a pinned-fingerprint mismatch fails closed and NEVER invokes git — the unmodified candidate is never silently tested", () => {
  const scratchDir = mkdtempSync(join(tmpdir(), "semantic-mutation-test-"));
  let spawnCalls = 0;
  try {
    const record = exampleMutantRecord({
      transformation: { ...exampleMutantRecord().transformation, pre_image_fingerprint: "f".repeat(64) },
    });
    const result = applyTransformation({
      record,
      root: REPO_ROOT,
      scratchDir,
      spawnSyncFn: () => {
        spawnCalls += 1;
        throw new Error("git must never be invoked after a fingerprint mismatch");
      },
    });
    assert.equal(result.ok, false);
    assert.equal(result.reason, "pinned-fingerprint-mismatch");
    assert.ok(result.detail.includes(`${REPIN_RECIPE} ${record.id}`), "the detail names the repin recipe");
    assert.equal(spawnCalls, 0);
  } finally {
    rmSync(scratchDir, { recursive: true, force: true });
  }
});

test("applyTransformation: a failing `git apply --check` aborts before a real apply is ever attempted", () => {
  const scratchDir = mkdtempSync(join(tmpdir(), "semantic-mutation-test-"));
  let applyCalls = 0;
  try {
    const record = exampleMutantRecord();
    const result = applyTransformation({
      record,
      root: REPO_ROOT,
      scratchDir,
      spawnSyncFn: (cmd, args) => {
        applyCalls += 1;
        assert.ok(args.includes("--check"), "the first (and here, only) git invocation must be --check");
        return { status: 1, stdout: "", stderr: "error: patch does not apply" };
      },
    });
    assert.equal(result.ok, false);
    assert.equal(result.reason, "patch-check-failed");
    assert.ok(result.detail.includes(REPIN_RECIPE));
    assert.equal(applyCalls, 1);
  } finally {
    rmSync(scratchDir, { recursive: true, force: true });
  }
});

test("applyTransformation: a real git apply materializes the mutated file, and both observed region fingerprints match the pins", () => {
  const scratchDir = mkdtempSync(join(tmpdir(), "semantic-mutation-test-"));
  try {
    const record = exampleMutantRecord();
    const result = applyTransformation({ record, root: REPO_ROOT, scratchDir });
    assert.equal(result.ok, true);
    assert.equal(result.observedPreImageFingerprint, REAL_PINS.pre_image_fingerprint);
    assert.equal(result.observedPostImageFingerprint, REAL_PINS.post_image_fingerprint);
    assert.equal(readFileSync(result.mutatedAbsPath, "utf8").includes("if v < hi {"), true);
    // The real target file on disk must be untouched.
    assert.equal(
      sha256Hex(readFileSync(join(REPO_ROOT, REAL_TARGET_PATH))),
      sha256Hex(readFileSync(join(REPO_ROOT, REAL_TARGET_PATH))),
    );
    assert.equal(readFileSync(join(REPO_ROOT, REAL_TARGET_PATH), "utf8").includes("if v < hi {"), false);
  } finally {
    rmSync(scratchDir, { recursive: true, force: true });
  }
});

// --- The region scheme's own class tests ---------------------------------
//
// Every test here builds a private copy of the committed target + patch
// under a scratch root and runs applyTransformation with `root` pointed at
// it, so the committed files are never touched and the drift being tested
// is exactly one edit to the copy.

function scratchRepoWithTarget({ mutateTarget = (text) => text } = {}) {
  const root = mkdtempSync(join(tmpdir(), "semantic-mutation-region-"));
  mkdirSync(join(root, "test/semantic/mutants/testdata/fixtures"), { recursive: true });
  mkdirSync(join(root, "test/semantic/mutants/patches"), { recursive: true });
  writeFileSync(join(root, REAL_TARGET_PATH), mutateTarget(readFileSync(join(REPO_ROOT, REAL_TARGET_PATH), "utf8")));
  writeFileSync(join(root, REAL_PATCH_PATH), readFileSync(join(REPO_ROOT, REAL_PATCH_PATH)));
  return root;
}

test("region scheme: an edit OUTSIDE the patch's pre-image does not invalidate the record (the whole-file scheme did)", () => {
  // arith.go's Clamp is what killed-invert.patch mutates; CommutativeSum
  // (a different function, further down the same file) is unrelated to it.
  const root = scratchRepoWithTarget({
    mutateTarget: (text) => {
      assert.ok(text.includes("return a + b"), "fixture precondition: CommutativeSum is in arith.go");
      return text.replace("return a + b", "// an unrelated edit, elsewhere in the same file\n\treturn a + b");
    },
  });
  const scratchDir = mkdtempSync(join(tmpdir(), "semantic-mutation-test-"));
  try {
    const wholeFileMoved = sha256Hex(readFileSync(join(root, REAL_TARGET_PATH))) !== sha256Hex(readFileSync(join(REPO_ROOT, REAL_TARGET_PATH)));
    assert.equal(wholeFileMoved, true, "precondition: the whole-file hash DID move");
    const result = applyTransformation({ record: exampleMutantRecord(), root, scratchDir });
    assert.equal(result.ok, true, JSON.stringify(result));
    assert.equal(result.observedPreImageFingerprint, REAL_PINS.pre_image_fingerprint);
    assert.equal(result.observedPostImageFingerprint, REAL_PINS.post_image_fingerprint);
  } finally {
    rmSync(scratchDir, { recursive: true, force: true });
    rmSync(root, { recursive: true, force: true });
  }
});

test("region scheme: an edit INSIDE the patch's pre-image DOES invalidate the record, before git is ever invoked", () => {
  const root = scratchRepoWithTarget({
    mutateTarget: (text) => {
      assert.ok(text.includes("if v > hi {"), "fixture precondition: the mutated line is in arith.go");
      return text.replace("if v > hi {", "if v >= hi {");
    },
  });
  const scratchDir = mkdtempSync(join(tmpdir(), "semantic-mutation-test-"));
  let spawnCalls = 0;
  try {
    const result = applyTransformation({
      record: exampleMutantRecord(),
      root,
      scratchDir,
      spawnSyncFn: () => {
        spawnCalls += 1;
        throw new Error("git must never be invoked once the pre-image is gone");
      },
    });
    assert.equal(result.ok, false);
    assert.equal(result.reason, "pre-image-not-in-target");
    assert.ok(result.detail.includes("hunk(s) 1"));
    assert.ok(result.detail.includes(REPIN_RECIPE));
    assert.equal(result.observedPreImageFingerprint, null);
    assert.equal(spawnCalls, 0);
  } finally {
    rmSync(scratchDir, { recursive: true, force: true });
    rmSync(root, { recursive: true, force: true });
  }
});

test("region scheme: the repin script round-trips a record with stale pins back to killed, rewriting only the two fingerprint values", async () => {
  const root = scratchRepoWithTarget();
  const dir = "test/semantic/mutants";
  const id = "MUTANT-SYNTH-EXAMPLE";
  const stale = exampleMutantRecord({
    transformation: {
      ...exampleMutantRecord().transformation,
      pre_image_fingerprint: "1".repeat(64),
      post_image_fingerprint: "2".repeat(64),
    },
  });
  const recordPath = join(root, dir, `${id}.json`);
  writeFileSync(recordPath, `${JSON.stringify(stale, null, 2)}\n`);
  const scratchDir = mkdtempSync(join(tmpdir(), "semantic-mutation-test-"));
  try {
    const goTest = async ({ overlayPath }) => (overlayPath === null ? fakeCleanPass() : fakeKill());
    const before = await runMutant({ record: stale, root, scratchDir, runGoTestFn: goTest });
    assert.equal(before.status, "invalid-transform");
    assert.equal(before.reason, "pinned-fingerprint-mismatch");

    const repin = repinMutantRecord(id, { root, dir });
    assert.equal(repin.changed, true);
    assert.deepEqual(repin.after, REAL_PINS);
    assert.deepEqual(repin.before, { pre_image_fingerprint: "1".repeat(64), post_image_fingerprint: "2".repeat(64) });

    const rewritten = JSON.parse(readFileSync(recordPath, "utf8"));
    assert.deepEqual(rewritten.transformation, { ...stale.transformation, ...REAL_PINS });
    const { transformation: _a, ...restStale } = stale;
    const { transformation: _b, ...restRewritten } = rewritten;
    assert.deepEqual(restRewritten, restStale, "no field other than the two fingerprints moved");

    const reloaded = loadMutants(dir, { root }).get(id);
    const after = await runMutant({ record: reloaded, root, scratchDir, runGoTestFn: goTest });
    assert.equal(after.status, "killed");
    assert.equal(repinMutantRecord(id, { root, dir }).changed, false, "a second repin is a no-op");
  } finally {
    rmSync(scratchDir, { recursive: true, force: true });
    rmSync(root, { recursive: true, force: true });
  }
});

test("region scheme: the repin script refuses an unknown mutant id", () => {
  assert.throws(() => repinMutantRecord("MUTANT-NO-SUCH-RECORD", { root: REPO_ROOT }), /no mutant record named/);
});

// --- runMutant orchestration: injected fakes, no Go toolchain needed ------

function fakeCleanPass() {
  return { exitCode: 0, signal: null, stdout: "PASS\n", stderr: "", timedOut: false, durationMs: 1 };
}

function fakeKill() {
  return { exitCode: 1, signal: null, stdout: "--- FAIL: TestExample (0.00s)\nFAIL\n", stderr: "", timedOut: false, durationMs: 1 };
}

test("runMutant: a failing clean control ABORTS measurement — the mutant is never attempted", async () => {
  const scratchRoot = mkdtempSync(join(tmpdir(), "semantic-mutation-scratch-"));
  const scratchDir = createScratchDir(scratchRoot);
  let calls = 0;
  try {
    const record = exampleMutantRecord();
    const result = await runMutant({
      record,
      root: REPO_ROOT,
      scratchDir,
      runGoTestFn: async () => {
        calls += 1;
        return { exitCode: 1, signal: null, stdout: "", stderr: "boom", timedOut: false, durationMs: 1 };
      },
    });
    assert.equal(result.status, "infrastructure-error");
    assert.equal(result.reason, "clean-control-failed");
    assert.equal(calls, 1, "runGoTestFn must be called exactly once — for the clean control only");
  } finally {
    rmSync(scratchRoot, { recursive: true, force: true });
  }
});

test("runMutant: zero selected detectors is an error, raised before any run is attempted", async () => {
  const scratchRoot = mkdtempSync(join(tmpdir(), "semantic-mutation-scratch-"));
  const scratchDir = createScratchDir(scratchRoot);
  let calls = 0;
  try {
    const record = exampleMutantRecord();
    await assert.rejects(
      runMutant({
        record,
        root: REPO_ROOT,
        scratchDir,
        detectorId: "no-such-detector",
        runGoTestFn: async () => {
          calls += 1;
          return fakeCleanPass();
        },
      }),
      /no detector named/,
    );
    assert.equal(calls, 0);
  } finally {
    rmSync(scratchRoot, { recursive: true, force: true });
  }
});

test("runMutant: a transformation fingerprint mismatch is invalid-transform and never runs a mutant detector", async () => {
  const scratchRoot = mkdtempSync(join(tmpdir(), "semantic-mutation-scratch-"));
  const scratchDir = createScratchDir(scratchRoot);
  let calls = 0;
  try {
    const record = exampleMutantRecord({
      transformation: { ...exampleMutantRecord().transformation, post_image_fingerprint: "e".repeat(64) },
    });
    const result = await runMutant({
      record,
      root: REPO_ROOT,
      scratchDir,
      runGoTestFn: async () => {
        calls += 1;
        return fakeCleanPass();
      },
    });
    assert.equal(result.status, "invalid-transform");
    assert.equal(result.reason, "pinned-fingerprint-mismatch");
    assert.equal(result.pre_image_fingerprint.expected, REAL_PINS.pre_image_fingerprint);
    assert.equal(result.post_image_fingerprint.expected, "e".repeat(64));
    // Called once for the clean control; the mutant run never happens.
    assert.equal(calls, 1);
  } finally {
    rmSync(scratchRoot, { recursive: true, force: true });
  }
});

test("runMutant: an equivalence_review reclassifies a survivor to equivalent-reviewed", async () => {
  const scratchRoot = mkdtempSync(join(tmpdir(), "semantic-mutation-scratch-"));
  const scratchDir = createScratchDir(scratchRoot);
  try {
    const record = exampleMutantRecord({
      expected_detection: "equivalent-reviewed",
      equivalence_review: { reviewer: "test-suite", reviewed_at: "2026-01-01T00:00:00Z", rationale: "test double" },
    });

    const result = await runMutant({
      record,
      root: REPO_ROOT,
      scratchDir,
      runGoTestFn: async () => fakeCleanPass(), // clean control AND mutant run both report PASS ("survived" before reclassification)
    });
    assert.equal(result.status, "equivalent-reviewed");
  } finally {
    rmSync(scratchRoot, { recursive: true, force: true });
  }
});

test("runMutant: an equivalence_review never rescues a mutation other than the one it reviewed — a moved pre-image is invalid-transform first", async () => {
  const root = scratchRepoWithTarget({ mutateTarget: (text) => text.replace("if v > hi {", "if v >= hi {") });
  const scratchDir = mkdtempSync(join(tmpdir(), "semantic-mutation-test-"));
  try {
    const record = exampleMutantRecord({
      expected_detection: "equivalent-reviewed",
      equivalence_review: { reviewer: "test-suite", reviewed_at: "2026-01-01T00:00:00Z", rationale: "reviewed the ORIGINAL region" },
    });
    const result = await runMutant({ record, root, scratchDir, runGoTestFn: async () => fakeCleanPass() });
    assert.equal(result.status, "invalid-transform");
    assert.equal(result.reason, "pre-image-not-in-target");
  } finally {
    rmSync(scratchDir, { recursive: true, force: true });
    rmSync(root, { recursive: true, force: true });
  }
});

// --- scratchRootFor / createScratchDir -------------------------------------

test("scratchRootFor: honors RUNNER_TEMP when set", () => {
  assert.equal(scratchRootFor({ RUNNER_TEMP: "/runner/temp" }), "/runner/temp");
});

test("scratchRootFor: falls back to the OS temp dir when RUNNER_TEMP is unset", () => {
  assert.equal(scratchRootFor({}), tmpdir());
});

test("createScratchDir: two calls under the same root never collide", () => {
  const root = mkdtempSync(join(tmpdir(), "semantic-mutation-root-"));
  try {
    const a = createScratchDir(root);
    const b = createScratchDir(root);
    assert.notEqual(a, b);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

// --- runGoTest: exact argv construction (injected spawnFn, no real `go`) --

// fakeChild builds a minimal stand-in for node:child_process's
// ChildProcess: an EventEmitter with the handful of members runGoTest
// actually touches (stdout/stderr streams, pid, exitCode, and a close event
// this helper fires on the next tick so the promise's own listeners are
// already attached).
function fakeChild({ exitCode = 0, signal = null } = {}) {
  const child = new EventEmitter();
  child.stdout = new EventEmitter();
  child.stderr = new EventEmitter();
  child.pid = 4242;
  child.exitCode = null;
  queueMicrotask(() => {
    child.exitCode = exitCode;
    child.emit("close", exitCode, signal);
  });
  return child;
}

test("runGoTest: constructs the exact argv — -v, -run, -timeout, -tags, quoted -exec, -overlay, package last", async () => {
  const calls = [];
  const child = fakeChild({ exitCode: 0 });
  const spawnFn = (cmd, args, opts) => {
    calls.push({ cmd, args, opts });
    return child;
  };

  const result = await runGoTest({
    root: "/repo",
    pkg: "./test/semantic/mutants/testdata/fixtures",
    testRun: "^TestExample$",
    buildTags: ["chdb"],
    overlayPath: "/scratch/overlay.json",
    timeoutSeconds: 15,
    memoryMax: "1GiB",
    memoryHold: "1s",
    memoryLedgerPath: "/scratch/ledger.jsonl",
    memoryGuardPath: "/repo/.github/scripts/mutant-memory-guard.mjs",
    spawnFn,
  });

  assert.equal(calls.length, 1);
  assert.equal(calls[0].cmd, "go");
  assert.deepEqual(calls[0].args, [
    "test",
    "-v",
    "-run=^TestExample$",
    "-timeout=15s",
    "-tags=chdb",
    '-exec=node "/repo/.github/scripts/mutant-memory-guard.mjs"',
    "-overlay=/scratch/overlay.json",
    "./test/semantic/mutants/testdata/fixtures",
  ]);
  assert.equal(calls[0].opts.cwd, "/repo");
  assert.equal(calls[0].opts.detached, true);
  assert.equal(calls[0].opts.env.MUTANT_MEMORY_MAX, "1GiB");
  assert.equal(calls[0].opts.env.MUTANT_MEMORY_HOLD, "1s");
  assert.equal(calls[0].opts.env.MUTANT_MEMORY_LEDGER, "/scratch/ledger.jsonl");
  assert.equal(result.exitCode, 0);
});

test("runGoTest: omits -tags when build_tags is empty and -overlay when overlayPath is null (the clean-control shape)", async () => {
  const calls = [];
  const child = fakeChild({ exitCode: 0 });
  const spawnFn = (cmd, args) => {
    calls.push(args);
    return child;
  };

  await runGoTest({
    root: "/repo",
    pkg: "./pkg",
    testRun: "^TestX$",
    buildTags: [],
    overlayPath: null,
    timeoutSeconds: 5,
    memoryMax: "1GiB",
    memoryHold: "1s",
    memoryLedgerPath: "/scratch/ledger.jsonl",
    memoryGuardPath: "/repo/guard.mjs",
    spawnFn,
  });

  assert.deepEqual(calls[0], [
    "test",
    "-v",
    "-run=^TestX$",
    "-timeout=5s",
    '-exec=node "/repo/guard.mjs"',
    "./pkg",
  ]);
});

test("runGoTest: a spawn error resolves (never rejects) with an infrastructure-error-shaped result", async () => {
  const child = new EventEmitter();
  child.stdout = new EventEmitter();
  child.stderr = new EventEmitter();
  child.pid = undefined;
  child.exitCode = null;
  const spawnFn = () => {
    queueMicrotask(() => child.emit("error", new Error("ENOENT: go not found")));
    return child;
  };

  const result = await runGoTest({
    root: "/repo",
    pkg: "./pkg",
    testRun: "^TestX$",
    buildTags: [],
    overlayPath: null,
    timeoutSeconds: 5,
    memoryMax: "1GiB",
    memoryHold: "1s",
    memoryLedgerPath: "/scratch/ledger.jsonl",
    memoryGuardPath: "/repo/guard.mjs",
    spawnFn,
  });

  assert.equal(result.exitCode, null);
  assert.equal(classifyGoTestOutput(result), "infrastructure-error");
});
