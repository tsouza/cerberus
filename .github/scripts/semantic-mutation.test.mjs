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
import { copyFileSync, mkdtempSync, mkdirSync, rmSync, writeFileSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawnSync } from "node:child_process";
import { EventEmitter } from "node:events";

import {
  CLASSIFICATIONS,
  DEFAULT_MUTANTS_DIR,
  aggregateClassifications,
  applyTransformation,
  classifyGoTestOutput,
  createScratchDir,
  loadMutantExecutions,
  loadMutants,
  renderMutantsSummary,
  runGoTest,
  runMutant,
  scratchRootFor,
  selectDetectors,
  sha256Hex,
  validateMutantRecord,
} from "./lib/semantic-mutation.mjs";
import { SemanticModelError } from "./lib/semantic-model.mjs";

const REPO_ROOT = process.cwd();
// Real, committed, always-present files — the same discipline
// semantic-counterexamples.test.mjs uses for its own path fixtures.
const REAL_TARGET_PATH = "test/semantic/mutants/testdata/fixtures/arith.go";
const REAL_PATCH_PATH = "test/semantic/mutants/patches/killed-invert.patch";
const REAL_SOURCE_FINGERPRINT = sha256Hex(readFileSync(join(REPO_ROOT, REAL_TARGET_PATH)));

// computeMutatedFingerprint independently reproduces what applyTransformation
// itself will do (copy the pristine target into a scratch dir preserving its
// relative path, then `git apply`), so tests that need a record's
// expected_mutated_fingerprint never depend on a committed JSON file's own
// value to define what "correct" means — this is the SAME real git
// operation, computed once and shared by every test below that needs it.
function computeMutatedFingerprint(patchPath) {
  const probe = mkdtempSync(join(tmpdir(), "semantic-mutation-probe-"));
  try {
    const scratchTarget = join(probe, REAL_TARGET_PATH);
    mkdirSync(join(probe, "test/semantic/mutants/testdata/fixtures"), { recursive: true });
    copyFileSync(join(REPO_ROOT, REAL_TARGET_PATH), scratchTarget);
    const applied = spawnSync(
      "git",
      ["apply", "--unsafe-paths", `--directory=${probe}`, join(REPO_ROOT, patchPath)],
      { cwd: REPO_ROOT, encoding: "utf8" },
    );
    assert.equal(applied.status, 0, `probe patch application failed: ${applied.stderr}`);
    return sha256Hex(readFileSync(scratchTarget));
  } finally {
    rmSync(probe, { recursive: true, force: true });
  }
}

function exampleMutantRecord(overrides = {}) {
  return {
    schema_version: 1,
    id: "MUTANT-SYNTH-EXAMPLE",
    title: "example mutant",
    synthetic: true,
    synthetic_rationale: "unit-test fixture",
    violated_contracts: ["SYNTHETIC-EXAMPLE"],
    transformation: {
      target_path: REAL_TARGET_PATH,
      patch_path: REAL_PATCH_PATH,
      source_fingerprint: REAL_SOURCE_FINGERPRINT,
      expected_mutated_fingerprint: "b".repeat(64),
    },
    detectors: [
      {
        id: "example-detector",
        package: "./test/semantic/mutants/testdata/fixtures",
        test_run: "^TestExample$",
        build_tags: [],
        timeout_seconds: 15,
      },
    ],
    expected_detection: "killed",
    equivalence_review: null,
    isolation: { requires_chdb: false, memory_max: "1GiB", memory_hold: "150s" },
    notes: null,
    linked_issue: null,
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

test("rejects an unknown top-level key", () => {
  const { problems } = validate({ ...exampleMutantRecord(), extra: true });
  assert.ok(problems.some((p) => p.includes(".extra is unknown")));
});

test("rejects a wrong schema_version", () => {
  const { problems } = validate(exampleMutantRecord({ schema_version: 2 }));
  assert.ok(problems.some((p) => p.includes("schema_version must be 1")));
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

test("rejects a malformed source_fingerprint", () => {
  const { problems } = validate(
    exampleMutantRecord({
      transformation: { ...exampleMutantRecord().transformation, source_fingerprint: "not-hex" },
    }),
  );
  assert.ok(problems.some((p) => p.includes("source_fingerprint")));
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
      equivalence_review: {
        reviewer: "x",
        reviewed_at: "2026-01-01T00:00:00Z",
        source_fingerprint: REAL_SOURCE_FINGERPRINT,
        rationale: "y",
      },
    }),
  );
  assert.ok(problems.some((p) => p.includes("must be null unless expected_detection")));
});

test("equivalence_review.source_fingerprint must agree with transformation.source_fingerprint", () => {
  const { problems } = validate(
    exampleMutantRecord({
      expected_detection: "equivalent-reviewed",
      equivalence_review: {
        reviewer: "x",
        reviewed_at: "2026-01-01T00:00:00Z",
        source_fingerprint: "c".repeat(64),
        rationale: "y",
      },
    }),
  );
  assert.ok(problems.some((p) => p.includes("disagrees with transformation.source_fingerprint")));
});

test("rejects a malformed isolation.memory_max", () => {
  const { problems } = validate(
    exampleMutantRecord({
      isolation: { requires_chdb: false, memory_max: "1gigabyte", memory_hold: "150s" },
    }),
  );
  assert.ok(problems.some((p) => p.includes("memory_max")));
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

test("aggregateClassifications: killed from any detector wins outright", () => {
  assert.equal(aggregateClassifications(["survived", "killed", "timeout"]), "killed");
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

test("applyTransformation: a source_fingerprint mismatch fails closed and NEVER invokes git — the unmodified candidate is never silently tested", () => {
  const scratchDir = mkdtempSync(join(tmpdir(), "semantic-mutation-test-"));
  let spawnCalls = 0;
  try {
    const record = exampleMutantRecord({
      transformation: { ...exampleMutantRecord().transformation, source_fingerprint: "f".repeat(64) },
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
    assert.equal(result.reason, "source-fingerprint-mismatch");
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
    assert.equal(applyCalls, 1);
  } finally {
    rmSync(scratchDir, { recursive: true, force: true });
  }
});

test("applyTransformation: a real git apply materializes the mutated file and its fingerprint matches the record", () => {
  const scratchDir = mkdtempSync(join(tmpdir(), "semantic-mutation-test-"));
  try {
    const record = exampleMutantRecord({
      transformation: {
        target_path: REAL_TARGET_PATH,
        patch_path: REAL_PATCH_PATH,
        source_fingerprint: REAL_SOURCE_FINGERPRINT,
        expected_mutated_fingerprint: computeMutatedFingerprint(REAL_PATCH_PATH),
      },
    });

    const result = applyTransformation({ record, root: REPO_ROOT, scratchDir });
    assert.equal(result.ok, true);
    assert.equal(result.observedSourceFingerprint, REAL_SOURCE_FINGERPRINT);
    assert.equal(readFileSync(result.mutatedAbsPath, "utf8").includes("if v < hi {"), true);
    // The real target file on disk must be untouched.
    assert.equal(sha256Hex(readFileSync(join(REPO_ROOT, REAL_TARGET_PATH))), REAL_SOURCE_FINGERPRINT);
  } finally {
    rmSync(scratchDir, { recursive: true, force: true });
  }
});

// --- runMutant orchestration: injected fakes, no Go toolchain needed ------

function fakeCleanPass() {
  return { exitCode: 0, signal: null, stdout: "PASS\n", stderr: "", timedOut: false, durationMs: 1 };
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
      transformation: { ...exampleMutantRecord().transformation, source_fingerprint: "e".repeat(64) },
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
    assert.equal(result.reason, "source-fingerprint-mismatch");
    // Called once for the clean control; the mutant run never happens.
    assert.equal(calls, 1);
  } finally {
    rmSync(scratchRoot, { recursive: true, force: true });
  }
});

test("runMutant: equivalence_review reclassifies a survivor only when its fingerprint still matches the live source", async () => {
  const scratchRoot = mkdtempSync(join(tmpdir(), "semantic-mutation-scratch-"));
  const scratchDir = createScratchDir(scratchRoot);
  try {
    const record = exampleMutantRecord({
      expected_detection: "equivalent-reviewed",
      equivalence_review: {
        reviewer: "test-suite",
        reviewed_at: "2026-01-01T00:00:00Z",
        source_fingerprint: REAL_SOURCE_FINGERPRINT,
        rationale: "test double",
      },
      transformation: {
        target_path: REAL_TARGET_PATH,
        patch_path: REAL_PATCH_PATH,
        source_fingerprint: REAL_SOURCE_FINGERPRINT,
        expected_mutated_fingerprint: computeMutatedFingerprint(REAL_PATCH_PATH),
      },
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

test("runMutant: a stale equivalence_review (fingerprint disagrees) fails closed to a bare survived", async () => {
  const scratchRoot = mkdtempSync(join(tmpdir(), "semantic-mutation-scratch-"));
  const scratchDir = createScratchDir(scratchRoot);
  try {
    // A record whose equivalence_review.source_fingerprint does NOT match
    // transformation.source_fingerprint could never pass validateMutantRecord
    // (that mismatch is itself a schema error), so this constructs the
    // record by hand past the validator to test runMutant's own runtime
    // defense-in-depth re-check, independent of whether validation ran.
    const record = exampleMutantRecord({
      expected_detection: "survived",
      equivalence_review: null,
      transformation: {
        target_path: REAL_TARGET_PATH,
        patch_path: REAL_PATCH_PATH,
        source_fingerprint: REAL_SOURCE_FINGERPRINT,
        expected_mutated_fingerprint: computeMutatedFingerprint(REAL_PATCH_PATH),
      },
    });
    // Attach a review whose fingerprint is simply wrong — simulating one
    // authored against a since-changed target file.
    record.equivalence_review = {
      reviewer: "test-suite",
      reviewed_at: "2026-01-01T00:00:00Z",
      source_fingerprint: "d".repeat(64),
      rationale: "stale",
    };

    const result = await runMutant({
      record,
      root: REPO_ROOT,
      scratchDir,
      runGoTestFn: async () => fakeCleanPass(),
    });
    assert.equal(result.status, "survived");
  } finally {
    rmSync(scratchRoot, { recursive: true, force: true });
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

// --- loadMutantExecutions (test/semantic/mutant-executions.json) -----------

function writeLedger(dir, doc) {
  const path = join(dir, "mutant-executions.json");
  writeFileSync(path, JSON.stringify(doc));
  return path;
}

test("loadMutantExecutions: a missing ledger file returns an empty Map, not an error", () => {
  const dir = mkdtempSync(join(tmpdir(), "mutant-exec-"));
  try {
    const result = loadMutantExecutions("does-not-exist.json", { root: dir });
    assert.equal(result.size, 0);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("loadMutantExecutions: loads a valid ledger and cross-checks mutant references", () => {
  const dir = mkdtempSync(join(tmpdir(), "mutant-exec-"));
  try {
    const doc = {
      schema_version: 1,
      executions: [
        {
          id: "MUTEXEC-EXAMPLE-20260916",
          mutant: "MUTANT-SYNTH-EXAMPLE",
          observed_at: "2026-09-16T00:00:00Z",
          status: "killed",
          run_ref: "local",
          source_sha: null,
          detectors: [{ id: "d1", classification: "killed" }],
        },
      ],
    };
    const path = writeLedger(dir, doc);
    const result = loadMutantExecutions(path, {
      root: dir,
      mutantIds: new Set(["MUTANT-SYNTH-EXAMPLE"]),
    });
    assert.equal(result.size, 1);
    assert.equal(result.get("MUTANT-SYNTH-EXAMPLE").status, "killed");
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("loadMutantExecutions: rejects a reference to an unknown mutant when mutantIds is given", () => {
  const dir = mkdtempSync(join(tmpdir(), "mutant-exec-"));
  try {
    const doc = {
      schema_version: 1,
      executions: [
        {
          id: "MUTEXEC-EXAMPLE-20260916",
          mutant: "MUTANT-DOES-NOT-EXIST",
          observed_at: "2026-09-16T00:00:00Z",
          status: "killed",
          run_ref: "local",
          source_sha: null,
          detectors: [],
        },
      ],
    };
    const path = writeLedger(dir, doc);
    assert.throws(
      () => loadMutantExecutions(path, { root: dir, mutantIds: new Set(["MUTANT-SYNTH-EXAMPLE"]) }),
      SemanticModelError,
    );
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("loadMutantExecutions: rejects an unknown status (only the seven CLASSIFICATIONS are valid)", () => {
  const dir = mkdtempSync(join(tmpdir(), "mutant-exec-"));
  try {
    const doc = {
      schema_version: 1,
      executions: [
        {
          id: "MUTEXEC-EXAMPLE-20260916",
          mutant: "MUTANT-SYNTH-EXAMPLE",
          observed_at: "2026-09-16T00:00:00Z",
          status: "passed",
          run_ref: "local",
          source_sha: null,
          detectors: [],
        },
      ],
    };
    const path = writeLedger(dir, doc);
    assert.throws(() => loadMutantExecutions(path, { root: dir }), SemanticModelError);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("loadMutantExecutions: the most recently observed_at record wins per mutant", () => {
  const dir = mkdtempSync(join(tmpdir(), "mutant-exec-"));
  try {
    const doc = {
      schema_version: 1,
      executions: [
        {
          id: "MUTEXEC-EXAMPLE-A",
          mutant: "MUTANT-SYNTH-EXAMPLE",
          observed_at: "2026-09-01T00:00:00Z",
          status: "survived",
          run_ref: "local",
          source_sha: null,
          detectors: [],
        },
        {
          id: "MUTEXEC-EXAMPLE-B",
          mutant: "MUTANT-SYNTH-EXAMPLE",
          observed_at: "2026-09-16T00:00:00Z",
          status: "killed",
          run_ref: "local",
          source_sha: null,
          detectors: [],
        },
      ],
    };
    const path = writeLedger(dir, doc);
    const result = loadMutantExecutions(path, { root: dir });
    assert.equal(result.get("MUTANT-SYNTH-EXAMPLE").status, "killed");
    assert.equal(result.get("MUTANT-SYNTH-EXAMPLE").id, "MUTEXEC-EXAMPLE-B");
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("loadMutantExecutions: the real committed ledger (if present) loads and cross-checks against the real corpus", () => {
  const records = loadMutants();
  const result = loadMutantExecutions(undefined, { mutantIds: new Set(records.keys()) });
  // Every execution in the committed ledger must resolve to a real mutant
  // and report one of the seven closed classifications — loadMutantExecutions
  // itself already enforces this; this is an end-to-end confirmation over
  // the real file, not a re-statement of the schema.
  for (const [mutantId, execution] of result) {
    assert.ok(records.has(mutantId));
    assert.ok(CLASSIFICATIONS.includes(execution.status));
  }
});
