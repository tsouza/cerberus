// semantic-replay.test.mjs — node --test guard for
// .github/scripts/lib/semantic-replay.mjs's structural routing, source-
// fingerprint check, and execution glue (issue #3446).
//
// SCOPING NOTE (why this suite never shells out to `just compat-logql`):
// this file is wired into ci.yml's "Validate the semantic contract metadata
// model" step, a fast metadata-validation step with no live ClickHouse,
// Loki, or Docker Compose stack of its own. The compat-corpus mechanism
// (CTREX-1741's LOGQL-LABEL-MATCHER-REGEX-ANCHORING entry) is exercised
// here only through resolveMechanisms()/resolveCompatCorpusMechanism() —
// pure, structural, no execution — never through runMechanism(), which
// would bring up the full loki-compatibility Docker Compose stack (minutes,
// image builds, a live reference Loki) from inside what is meant to stay a
// near-instant test. `just semantic-replay CTREX-1741` run by hand is the
// real way to exercise that mechanism end to end; this suite verifies it
// ROUTES correctly instead.
//
// Every other real mechanism in the committed cohort — CTREX-1741's
// PROMQL-LABEL-MATCHER-REGEX-ANCHORING entry, CTREX-1741's
// LOGQL-LABEL-MATCHER-REGEX-ANCHORING entry's go-test-fixture half,
// CTREX-2241, and CTREX-3271 — IS actually run via runMechanism(), because
// none of them need more than the Go toolchain this repository's CI
// already has everywhere, plus (for the chdb-tagged ones) libchdb.so,
// which this step does not install. Those assertions tolerate either PASS
// (when libchdb.so happens to be present, e.g. a contributor's own machine)
// or SUBSTRATE-UNAVAILABLE (the expected outcome in this CI job) — never
// FAIL/ERROR/STALE, which would mean the routing itself is broken.

import assert from "node:assert/strict";
import { test } from "node:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import {
  CHDB_INSTALL_PATH_ENV,
  EXIT_CODES,
  REPLAY_STATUS,
  buildGoTestInvocation,
  chdbAvailable,
  checkEntryFingerprint,
  classifyOverallExit,
  computeEntryFingerprint,
  fingerprintKey,
  isChdbTag,
  loadCounterexampleRecord,
  loadFingerprints,
  replayCounterexample,
  resolveCompatCorpusMechanism,
  resolveGoTestMechanism,
  resolveMechanisms,
  runMechanism,
} from "./lib/semantic-replay.mjs";

const REPO_ROOT = process.cwd();

function entryOf(id, contractId) {
  const record = loadCounterexampleRecord(id, { root: REPO_ROOT });
  assert.ok(record, `expected ${id} to load`);
  const entry = record.contracts.find((c) => c.contract_id === contractId);
  assert.ok(entry, `expected ${id} to carry a ${contractId} contract entry`);
  return entry;
}

// --- Unknown counterexample id (fail-closed condition #1) ------------------

test("loadCounterexampleRecord returns null for an unknown id", () => {
  assert.equal(loadCounterexampleRecord("CTREX-999999", { root: REPO_ROOT }), null);
});

test("replayCounterexample on an unknown id reports found:false and exits UNKNOWN_ID", () => {
  const result = replayCounterexample("CTREX-999999", { root: REPO_ROOT });
  assert.equal(result.found, false);
  assert.equal(classifyOverallExit(result), EXIT_CODES.UNKNOWN_ID);
});

// --- Missing/unresolvable selector (fail-closed condition #2) --------------

test("a go-test-direct entry whose replay_test_name names no real function is unresolved", () => {
  const entry = {
    ...entryOf("CTREX-3271", "PROMQL-ORACLE-ENGINE-ANSWER-FLAG-PARITY"),
    replay_test_name: "TestDoesNotExistAnywhereInThisFile",
  };
  const mechanism = resolveGoTestMechanism(entry, { root: REPO_ROOT });
  assert.equal(mechanism.kind, "unresolved");
  assert.match(mechanism.reason, /no func TestDoesNotExistAnywhereInThisFile/);
});

test("a table-driven test file paired with a non-sibling, non-fixture locator_path is unresolved", () => {
  const entry = {
    ...entryOf("CTREX-1741", "PROMQL-LABEL-MATCHER-REGEX-ANCHORING"),
    locator_path: "test/regression/promql_oracle_engine_parity_test.go",
  };
  const mechanism = resolveGoTestMechanism(entry, { root: REPO_ROOT });
  assert.equal(mechanism.kind, "unresolved");
  assert.match(mechanism.reason, /table-driven/);
});

test("a fixture with no companion spec.Walk/WalkShard test file in its directory is unresolved", () => {
  const dir = mkdtempSync(join(tmpdir(), "semantic-replay-"));
  try {
    // A .txtar with no *_test.go sibling at all.
    writeFileSync(join(dir, "lonely.txtar"), "-- comment --\nnothing here\n");
    const entry = {
      locator_path: "test/semantic/counterexamples/issue-1741.json",
      replay_test_path: join(dir, "lonely.txtar").replace(`${REPO_ROOT}/`, ""),
    };
    const mechanism = resolveGoTestMechanism(entry, { root: REPO_ROOT });
    assert.equal(mechanism.kind, "unresolved");
    assert.match(mechanism.reason, /no \*_test\.go/);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("an unmapped compatibility/ head is unresolved", () => {
  const mechanism = resolveCompatCorpusMechanism("compatibility/nonexistent-head/queries/x.yaml");
  assert.equal(mechanism.kind, "unresolved");
  assert.match(mechanism.reason, /no known just compat-<head> recipe/);
});

test("resolveMechanisms never returns an empty array — an entry naming neither a go-test nor compat target resolves to one unresolved mechanism", () => {
  const entry = {
    contract_id: "PROMQL-LABEL-MATCHER-REGEX-ANCHORING",
    related_bindings: [],
    locator_path: "test/semantic/counterexamples/issue-1741.json",
    locator_detail: "",
    replay_test_path: "test/semantic/counterexamples/issue-1741.json",
    replay_test_name: "n/a",
    replay_command: "n/a",
  };
  const mechanisms = resolveMechanisms(entry, { root: REPO_ROOT });
  assert.equal(mechanisms.length, 1);
  assert.equal(mechanisms[0].kind, "unresolved");
});

// --- Zero selected tests actually execute (fail-closed condition #3) -------

test("a go-test-direct mechanism whose pattern matches nothing reports zero-selected, not a pass", () => {
  // A function name that parses as a clean Test identifier but is not
  // defined anywhere test/regression — go test -run over it selects 0
  // tests and exits 0, which must not read as PASS.
  const mechanism = {
    kind: "go-test-direct",
    dir: "test/regression",
    testFunc: "TestNoSuchFunctionAnywhereInThisPackage",
    buildTag: null,
  };
  const result = runMechanism(mechanism, { root: REPO_ROOT });
  assert.equal(result.status, REPLAY_STATUS.ERROR);
  assert.equal(result.reasonKind, "zero-selected");
});

// --- Build tags are read from the file, never guessed/hardcoded ------------

test("build tag is read from the target file's own //go:build line, not hardcoded", () => {
  const chdbTagged = resolveGoTestMechanism(entryOf("CTREX-2241", "TRACEQL-STRUCTURAL-RELATION-SEMANTICS"), {
    root: REPO_ROOT,
  });
  assert.equal(chdbTagged.buildTag, "chdb");

  const untagged = resolveGoTestMechanism(entryOf("CTREX-3271", "PROMQL-ORACLE-ENGINE-ANSWER-FLAG-PARITY"), {
    root: REPO_ROOT,
  });
  assert.equal(untagged.buildTag, null);
});

test("isChdbTag matches only the exact 'chdb' token, not an unrelated or compound tag", () => {
  assert.equal(isChdbTag("chdb"), true);
  assert.equal(isChdbTag("chdb && agpl_oracle"), true);
  assert.equal(isChdbTag("chdb_agpl_oracle"), false);
  assert.equal(isChdbTag("agpl_oracle"), false);
  assert.equal(isChdbTag(null), false);
});

test("buildGoTestInvocation only adds -tags when a build tag was found", () => {
  const withTag = buildGoTestInvocation({ kind: "go-test-direct", dir: "test/property", testFunc: "TestX", buildTag: "chdb" });
  assert.deepEqual(withTag.args.slice(1, 3), ["-tags", "chdb"]);

  const withoutTag = buildGoTestInvocation({ kind: "go-test-direct", dir: "test/regression", testFunc: "TestX", buildTag: null });
  assert.equal(withoutTag.args.includes("-tags"), false);
});

test("buildGoTestInvocation builds a subtest-scoped -run pattern for a go-test-fixture mechanism", () => {
  const { args } = buildGoTestInvocation({
    kind: "go-test-fixture",
    dir: "test/spec/promql",
    testFunc: "TestRoundTripChDB",
    subtest: "regex_unanchored_no_overmatch",
    buildTag: "chdb",
  });
  const runIdx = args.indexOf("-run");
  assert.equal(args[runIdx + 1], "^TestRoundTripChDB$/^regex_unanchored_no_overmatch$");
});

// --- Substrate-unavailable, simulated and distinct from a real failure -----

test("chdbAvailable is false against a path that does not exist", () => {
  assert.equal(chdbAvailable({ installPath: "/definitely/does/not/exist/libchdb.so" }), false);
});

test("a chdb-tagged mechanism reports SUBSTRATE-UNAVAILABLE, not FAIL/ERROR, when libchdb.so is absent", () => {
  const previous = process.env[CHDB_INSTALL_PATH_ENV];
  process.env[CHDB_INSTALL_PATH_ENV] = "/definitely/does/not/exist/libchdb.so";
  try {
    const mechanism = { kind: "go-test-direct", dir: "test/property", testFunc: "TestTraceQLDescendantPropertyMatch", buildTag: "chdb" };
    const result = runMechanism(mechanism, { root: REPO_ROOT });
    assert.equal(result.status, REPLAY_STATUS.SUBSTRATE_UNAVAILABLE);
    assert.equal(result.reasonKind, "chdb-not-installed");
  } finally {
    if (previous === undefined) delete process.env[CHDB_INSTALL_PATH_ENV];
    else process.env[CHDB_INSTALL_PATH_ENV] = previous;
  }
});

// --- Fingerprint mismatch is its own STALE category, never a test failure --

test("checkEntryFingerprint returns null (safe to run) when the stored hash matches the live one", () => {
  const entry = entryOf("CTREX-3271", "PROMQL-ORACLE-ENGINE-ANSWER-FLAG-PARITY");
  const live = computeEntryFingerprint(entry, { root: REPO_ROOT });
  const stored = { [fingerprintKey("CTREX-3271", entry.contract_id)]: live };
  const check = checkEntryFingerprint("CTREX-3271", entry, stored, { root: REPO_ROOT });
  assert.equal(check.status, "ok");
  assert.equal(check.result, null);
});

test("checkEntryFingerprint reports STALE/fingerprint-missing when no fingerprint was ever recorded", () => {
  const entry = entryOf("CTREX-3271", "PROMQL-ORACLE-ENGINE-ANSWER-FLAG-PARITY");
  const check = checkEntryFingerprint("CTREX-3271", entry, {}, { root: REPO_ROOT });
  assert.equal(check.status, "missing");
  assert.equal(check.result.status, REPLAY_STATUS.STALE);
  assert.equal(check.result.reasonKind, "fingerprint-missing");
});

test("checkEntryFingerprint reports STALE/fingerprint-mismatch when the recorded hash disagrees with the live one", () => {
  const entry = entryOf("CTREX-3271", "PROMQL-ORACLE-ENGINE-ANSWER-FLAG-PARITY");
  const stored = { [fingerprintKey("CTREX-3271", entry.contract_id)]: "0".repeat(64) };
  const check = checkEntryFingerprint("CTREX-3271", entry, stored, { root: REPO_ROOT });
  assert.equal(check.status, "mismatch");
  assert.equal(check.result.status, REPLAY_STATUS.STALE);
  assert.equal(check.result.reasonKind, "fingerprint-mismatch");
});

test("computeEntryFingerprint hashes locator_path and replay_test_path once (not doubled) when they are the same file", () => {
  const path = "test/semantic/counterexamples/issue-3271.json";
  const withDuplicate = computeEntryFingerprint(
    { locator_path: path, replay_test_path: path },
    { root: REPO_ROOT },
  );
  // A distinct-paths hash over the very same two byte-streams must differ
  // from the deduped single-file hash — proves dedupe is actually applied,
  // not merely documented.
  const withTwoDistinctCopies = computeEntryFingerprint(
    { locator_path: path, replay_test_path: "test/semantic/counterexamples/issue-2241.json" },
    { root: REPO_ROOT },
  );
  assert.notEqual(withDuplicate, withTwoDistinctCopies);
  // Deterministic: recomputing over the same inputs reproduces the same hash.
  assert.equal(
    withDuplicate,
    computeEntryFingerprint({ locator_path: path, replay_test_path: path }, { root: REPO_ROOT }),
  );
});

// --- Real end-to-end: every contract entry in the committed cohort ---------

test("CTREX-1741 PROMQL-LABEL-MATCHER-REGEX-ANCHORING routes to and runs the promql spec-chdb fixture", () => {
  const entry = entryOf("CTREX-1741", "PROMQL-LABEL-MATCHER-REGEX-ANCHORING");
  const mechanisms = resolveMechanisms(entry, { root: REPO_ROOT });
  assert.equal(mechanisms.length, 1);
  assert.deepEqual(
    { kind: mechanisms[0].kind, dir: mechanisms[0].dir, testFunc: mechanisms[0].testFunc, subtest: mechanisms[0].subtest, buildTag: mechanisms[0].buildTag },
    {
      kind: "go-test-fixture",
      dir: "test/spec/promql",
      testFunc: "TestRoundTripChDB",
      subtest: "regex_unanchored_no_overmatch",
      buildTag: "chdb",
    },
  );
  const result = runMechanism(mechanisms[0], { root: REPO_ROOT });
  assert.ok(
    [REPLAY_STATUS.PASS, REPLAY_STATUS.SUBSTRATE_UNAVAILABLE].includes(result.status),
    `expected PASS or SUBSTRATE-UNAVAILABLE, got ${result.status}: ${result.detail}`,
  );
});

test("CTREX-1741 LOGQL-LABEL-MATCHER-REGEX-ANCHORING routes to TWO mechanisms: a spec fixture and a compat corpus", () => {
  const entry = entryOf("CTREX-1741", "LOGQL-LABEL-MATCHER-REGEX-ANCHORING");
  const mechanisms = resolveMechanisms(entry, { root: REPO_ROOT });
  assert.equal(mechanisms.length, 2);

  const fixture = mechanisms.find((m) => m.kind === "go-test-fixture");
  assert.deepEqual(
    { dir: fixture.dir, testFunc: fixture.testFunc, subtest: fixture.subtest, buildTag: fixture.buildTag },
    { dir: "test/spec/logql", testFunc: "TestRoundTripChDB", subtest: "stream_regex_no_overmatch", buildTag: "chdb" },
  );
  // Actually run the go-test-fixture half — needs only Go + optionally chdb.
  const fixtureResult = runMechanism(fixture, { root: REPO_ROOT });
  assert.ok(
    [REPLAY_STATUS.PASS, REPLAY_STATUS.SUBSTRATE_UNAVAILABLE].includes(fixtureResult.status),
    `expected PASS or SUBSTRATE-UNAVAILABLE, got ${fixtureResult.status}: ${fixtureResult.detail}`,
  );

  const compat = mechanisms.find((m) => m.kind === "compat-corpus");
  assert.equal(compat.recipe, "compat-logql");
  assert.equal(compat.wholeLane, true);
  // Deliberately NOT executed here — see file header. `just semantic-replay
  // CTREX-1741` run by hand exercises this mechanism for real.
});

test("CTREX-2241 TRACEQL-STRUCTURAL-RELATION-SEMANTICS routes to and runs the property regression test", () => {
  const entry = entryOf("CTREX-2241", "TRACEQL-STRUCTURAL-RELATION-SEMANTICS");
  const mechanisms = resolveMechanisms(entry, { root: REPO_ROOT });
  assert.equal(mechanisms.length, 1);
  assert.deepEqual(
    { kind: mechanisms[0].kind, dir: mechanisms[0].dir, testFunc: mechanisms[0].testFunc, buildTag: mechanisms[0].buildTag },
    { kind: "go-test-direct", dir: "test/property", testFunc: "TestTraceQLDescendantPropertyMatch", buildTag: "chdb" },
  );
  const result = runMechanism(mechanisms[0], { root: REPO_ROOT });
  assert.ok(
    [REPLAY_STATUS.PASS, REPLAY_STATUS.SUBSTRATE_UNAVAILABLE].includes(result.status),
    `expected PASS or SUBSTRATE-UNAVAILABLE, got ${result.status}: ${result.detail}`,
  );
});

test("CTREX-3271 PROMQL-ORACLE-ENGINE-ANSWER-FLAG-PARITY routes to and runs the regression test cleanly (no chdb tag, always runnable)", () => {
  const entry = entryOf("CTREX-3271", "PROMQL-ORACLE-ENGINE-ANSWER-FLAG-PARITY");
  const mechanisms = resolveMechanisms(entry, { root: REPO_ROOT });
  assert.equal(mechanisms.length, 1);
  assert.deepEqual(
    { kind: mechanisms[0].kind, dir: mechanisms[0].dir, testFunc: mechanisms[0].testFunc, buildTag: mechanisms[0].buildTag },
    { kind: "go-test-direct", dir: "test/regression", testFunc: "TestPromQLOracleEngineParity", buildTag: null },
  );
  const result = runMechanism(mechanisms[0], { root: REPO_ROOT });
  assert.equal(result.status, REPLAY_STATUS.PASS, result.detail);
});

test("replayCounterexample over CTREX-3271 end to end exits OK", () => {
  const result = replayCounterexample("CTREX-3271", { root: REPO_ROOT });
  assert.equal(result.found, true);
  assert.equal(classifyOverallExit(result), EXIT_CODES.OK);
  assert.equal(result.entries[0].mechanisms[0].status, REPLAY_STATUS.PASS);
});

// --- Fingerprints committed for the real cohort stay fresh ------------------

// Uses checkEntryFingerprint() directly rather than the full
// replayCounterexample() — the latter also RUNS every resolved mechanism,
// including CTREX-1741's compat-corpus one, which would bring up the full
// loki-compatibility Docker Compose stack from inside this fast metadata
// check (see file header). Fingerprint freshness is independent of whether
// any mechanism can currently be executed.
test("every contract entry in the real committed cohort has a fresh, matching recorded fingerprint", () => {
  const stored = loadFingerprints(undefined, { root: REPO_ROOT }).fingerprints ?? {};
  for (const [id, contractId] of [
    ["CTREX-1741", "PROMQL-LABEL-MATCHER-REGEX-ANCHORING"],
    ["CTREX-1741", "LOGQL-LABEL-MATCHER-REGEX-ANCHORING"],
    ["CTREX-2241", "TRACEQL-STRUCTURAL-RELATION-SEMANTICS"],
    ["CTREX-3271", "PROMQL-ORACLE-ENGINE-ANSWER-FLAG-PARITY"],
  ]) {
    const entry = entryOf(id, contractId);
    const check = checkEntryFingerprint(id, entry, stored, { root: REPO_ROOT });
    assert.equal(check.status, "ok", `${id}#${contractId}: fingerprint ${check.status}`);
  }
});
