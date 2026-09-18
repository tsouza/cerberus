// semantic-mutation.mjs — record loader/validator and execution engine for
// test/semantic/mutants/*.json (issue #3448), which implements the
// isolated-execution protocol issue #3447's spike approved: apply a
// hand-authored, contract-linked mutation as a unified diff via `go test
// -overlay`, never touching the caller's own checkout.
//
// WHY OVERLAY, NOT A DISPOSABLE WORKTREE — the spike compared both on one
// source revision and measured overlay 2-4x faster in steady state, with
// zero disk footprint per mutant and zero `.git` state touched (so zero
// contention with this repo's already-heavy concurrent-worktree usage), and
// — the operationally decisive point — a killed run leaves nothing to sweep,
// where a killed worktree run does. See #3447's own spike comment for the
// measurements; this module implements its recommendation, not a redesign.
//
// WHAT THIS MODULE DOES NOT DO — it is the RUNNER only. It never authors a
// real per-head query-language mutation (#3449-#3451's job once this exists)
// and it is not a seventh evidence class for lib/semantic-model.mjs's
// computeAssurance(): a mutant record's `violated_contracts` is informational
// cross-referencing, the same non-authority lib/semantic-counterexamples.mjs
// already established for counterexample records, and this module never
// touches that computation.
//
// THE SEVEN OUTCOMES (CLASSIFICATIONS below) are a closed, ordered set and
// are never collapsed into one another (a multi-detector record reports
// the harness outcome of any detector whose harness never adjudicated
// ahead of its siblings' verdicts — see aggregateClassifications):
//
//   killed               a detector's own go test run failed with a
//                         well-formed FAIL — the suite caught the mutation.
//   survived              a detector's own go test run reported a clean PASS
//                         against the mutated candidate — nothing caught it.
//   equivalent-reviewed   a survived mutant carrying an AUDITED, hand-written
//                         equivalence_review whose own source_fingerprint
//                         still matches the live target file. This is never
//                         an automatic exemption list (repo invariant 7):
//                         the review is prose a human wrote, and it goes
//                         stale — falls back to a bare `survived` needing
//                         re-review — the moment the target file's fingerprint
//                         no longer matches.
//   invalid-transform      the declared source_fingerprint disagrees with the
//                         live target file, or `git apply` rejects the patch,
//                         or the post-patch content's fingerprint disagrees
//                         with the record's own expected_mutated_fingerprint.
//                         Fails BEFORE any detector ever runs, so a stale
//                         mutant can never silently test the unmodified
//                         candidate.
//   build-failed          the mutated overlay compiles to nothing: go test's
//                         own `[build failed]` signature, distinct from a
//                         real assertion failure.
//   timeout                the detector did not finish inside its declared
//                         timeout_seconds. Go's own `-timeout` flag is the
//                         PRIMARY mechanism (it dumps every goroutine's
//                         stack before the process exits on its own,
//                         printing `panic: test timed out after ...`); this
//                         runner's own wall-clock SIGKILL is only a
//                         BACKSTOP, a few seconds later, for the case where
//                         Go's own watchdog somehow does not fire.
//   infrastructure-error   anything else that leaves no well-formed PASS/FAIL
//                         to read: a clean CONTROL run that itself failed
//                         (the harness could not establish a baseline, so
//                         measurement never proceeds to the mutant at all —
//                         "a failing clean control aborts measurement"), a
//                         process killed by an external signal, or a mutated
//                         process that exits without go test's own harness
//                         ever reporting PASS or FAIL (e.g. a direct
//                         os.Exit() bypassing it, or a panic on a goroutine
//                         the testing package is not supervising, which
//                         tears the whole process down without ever
//                         printing a per-test `--- FAIL:` line). ONLY AN
//                         OUTCOME THE DETECTOR'S OWN HARNESS ADJUDICATED
//                         COUNTS AS A KILL: a panic testing's own harness
//                         CAUGHT and reported via a `--- FAIL:` line is a
//                         real adjudication and IS `killed` (see the
//                         `killed` bullet above and classifyGoTestOutput's
//                         own header) — it is specifically an unadjudicated
//                         process death, one no test harness ever got a
//                         chance to report on, that can never be a kill.
//
// Isolation: every write lands under a per-invocation mkdtemp() scratch
// directory (RUNNER_TEMP when set, the OS temp dir otherwise), which is
// unique per process by construction — concurrent runs never share a path,
// and nothing is ever written into the real target file. The memory bound on
// each detector run reuses .github/scripts/mutant-memory-guard.mjs entirely
// unchanged, via `go test -exec`, exactly as its own header documents.
//
// Node builtins only.

import { spawn, spawnSync } from "node:child_process";
import {
  copyFileSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";

import {
  OBSERVED_AT_RE,
  SemanticModelError,
  enumValue,
  exactObject,
  existingPathValue,
  fail,
  isObject,
  nullableStringValue,
  parseJSONFile,
  positiveIntegerValue,
  sha256Hex,
  stringArray,
  stringValue,
} from "./semantic-model.mjs";

// Re-exported for the callers that fingerprint through this module's own
// name (lib/semantic-mutation-report.mjs, the runner's tests); the
// implementation is lib/semantic-model.mjs's, shared with the execution
// adapter's corpus fingerprints.
export { sha256Hex };
import { byteSize, goDurationSeconds } from "../mutant-memory-guard.mjs";

export const MUTANT_SCHEMA_VERSION = 1;
export const DEFAULT_MUTANTS_DIR = "test/semantic/mutants";
export const DEFAULT_MEMORY_GUARD =
  ".github/scripts/mutant-memory-guard.mjs";

const MUTANT_ID_RE = /^MUTANT-[A-Z][A-Z0-9]*(?:-[A-Z0-9]+)*$/;
const SYNTHETIC_CONTRACT_RE = /^SYNTHETIC-[A-Z][A-Z0-9]*(?:-[A-Z0-9]+)*$/;
const CONTRACT_ID_RE = /^[A-Z][A-Z0-9]*(?:-[A-Z0-9]+)*$/;
export const SHA256_HEX_RE = /^[0-9a-f]{64}$/;

// The closed, ordered outcome vocabulary. Ordered because a mutant carrying
// more than one detector needs one deterministic precedence when detectors
// disagree — see aggregateClassifications() below — never because one
// outcome "outranks" another in severity.
export const CLASSIFICATIONS = Object.freeze([
  "killed",
  "survived",
  "equivalent-reviewed",
  "invalid-transform",
  "build-failed",
  "timeout",
  "infrastructure-error",
]);
const CLASSIFICATION_SET = new Set(CLASSIFICATIONS);

// A non-synthetic record's expected_detection is restricted to this subset:
// "killed" (the detector caught the mutation) or "equivalent-reviewed" (a
// survivor with an audited, fingerprint-pinned equivalence_review — see
// validateEquivalenceReview). Every other outcome in CLASSIFICATIONS either
// reports a real live detector gap ("survived") or a harness condition that
// means the record cannot make its claim yet ("invalid-transform",
// "build-failed", "timeout", "infrastructure-error") — declaring one of
// those on real (non-synthetic) evidence would be an expected-failure/
// tolerance-list entry with a JSON file for a face, exactly what repo
// invariant 7 forbids. A synthetic record is exempt: its entire purpose is
// exercising every runner outcome, including the non-"killed" ones.
const NON_SYNTHETIC_CLASSIFICATIONS = new Set(["killed", "equivalent-reviewed"]);

const MUTANT_KEYS = new Set([
  "schema_version",
  "id",
  "title",
  "synthetic",
  "synthetic_rationale",
  "violated_contracts",
  "transformation",
  "detectors",
  "expected_detection",
  "equivalence_review",
  "isolation",
  "notes",
  "linked_issue",
]);

const TRANSFORMATION_KEYS = new Set([
  "target_path",
  "patch_path",
  "source_fingerprint",
  "expected_mutated_fingerprint",
]);

const DETECTOR_KEYS = new Set([
  "id",
  "package",
  "test_run",
  "build_tags",
  "timeout_seconds",
  "evidence_kind",
]);

// What a detector's kill actually says about the mutation:
//   golden-text   the detector compares emitted text (a TXTAR fixture's
//                 sql/args/chplan sections) against a stored golden, so it
//                 fires on ANY change to the emitted SQL — a
//                 semantics-preserving refactor as readily as a wrong
//                 answer. A kill here proves the text moved, not that a
//                 semantic verifier would have caught the bug.
//   execution     the detector executes the mutated code (a property
//                 test, a chDB round trip, a behavioural unit test) and
//                 asserts on values; a kill here is a wrong answer.
// The cohort report keeps the two kill rates apart (lib/semantic-mutation-
// report.mjs), which is only possible if every detector says which it is.
export const DETECTOR_EVIDENCE_KINDS = Object.freeze(["golden-text", "execution"]);
const DETECTOR_EVIDENCE_KIND_SET = new Set(DETECTOR_EVIDENCE_KINDS);

// Every head's TestLower (internal/<head>/lower_test.go) calls spec.Match
// over the golden sections BEFORE spec.RunRoundTripSQL, and spec.Match
// fails the subtest on a text mismatch — so a detector selecting a
// TestLower fixture can only ever kill on the golden text; the round trip
// is never reached for a mutant that changed the SQL. Declaring such a
// detector "execution" would be false, and is rejected.
const GOLDEN_TEXT_ONLY_TEST_RUN_RE = /^\^TestLower\$/;

const EQUIVALENCE_REVIEW_KEYS = new Set([
  "reviewer",
  "reviewed_at",
  "source_fingerprint",
  "rationale",
]);

const ISOLATION_KEYS = new Set(["requires_chdb", "memory_max", "memory_hold"]);

function booleanValue(value, path, problems) {
  if (typeof value !== "boolean") {
    fail(problems, "schema", `${path} must be a boolean; got ${JSON.stringify(value)}`);
    return false;
  }
  return true;
}

function validateTransformation(raw, at, problems, opts) {
  if (!exactObject(raw, TRANSFORMATION_KEYS, at, problems)) return;
  existingPathValue(raw.target_path, `${at}.target_path`, problems, opts);
  existingPathValue(raw.patch_path, `${at}.patch_path`, problems, opts);
  stringValue(raw.source_fingerprint, `${at}.source_fingerprint`, problems, { pattern: SHA256_HEX_RE });
  stringValue(raw.expected_mutated_fingerprint, `${at}.expected_mutated_fingerprint`, problems, {
    pattern: SHA256_HEX_RE,
  });
}

function validateDetector(raw, at, problems, seenIds) {
  if (!exactObject(raw, DETECTOR_KEYS, at, problems)) return;
  if (stringValue(raw.id, `${at}.id`, problems)) {
    if (seenIds.has(raw.id)) fail(problems, "schema", `${at}.id is a duplicate within this record: ${raw.id}`);
    seenIds.add(raw.id);
  }
  stringValue(raw.package, `${at}.package`, problems, { pattern: /^\.\// });
  stringValue(raw.test_run, `${at}.test_run`, problems);
  stringArray(raw.build_tags, `${at}.build_tags`, problems, { allowEmpty: true });
  positiveIntegerValue(raw.timeout_seconds, `${at}.timeout_seconds`, problems);
  if (
    enumValue(raw.evidence_kind, DETECTOR_EVIDENCE_KIND_SET, `${at}.evidence_kind`, problems) &&
    raw.evidence_kind !== "golden-text" &&
    typeof raw.test_run === "string" &&
    GOLDEN_TEXT_ONLY_TEST_RUN_RE.test(raw.test_run)
  ) {
    fail(
      problems,
      "schema",
      `${at}.evidence_kind must be "golden-text" for a TestLower fixture detector (${raw.test_run}): ` +
        "spec.Match compares the golden sections before the chDB round trip is reached, so its kill is a text mismatch",
    );
  }
}

function validateIsolation(raw, at, problems) {
  if (!exactObject(raw, ISOLATION_KEYS, at, problems)) return;
  booleanValue(raw.requires_chdb, `${at}.requires_chdb`, problems);
  if (stringValue(raw.memory_max, `${at}.memory_max`, problems)) {
    try {
      byteSize(`${at}.memory_max`, raw.memory_max);
    } catch (cause) {
      fail(problems, "schema", cause.message);
    }
  }
  if (stringValue(raw.memory_hold, `${at}.memory_hold`, problems)) {
    try {
      goDurationSeconds(`${at}.memory_hold`, raw.memory_hold);
    } catch (cause) {
      fail(problems, "schema", cause.message);
    }
  }
}

function validateEquivalenceReview(raw, at, problems, { expectedSourceFingerprint }) {
  if (raw === null) return;
  if (!exactObject(raw, EQUIVALENCE_REVIEW_KEYS, at, problems)) return;
  stringValue(raw.reviewer, `${at}.reviewer`, problems);
  stringValue(raw.reviewed_at, `${at}.reviewed_at`, problems, { pattern: OBSERVED_AT_RE });
  if (stringValue(raw.source_fingerprint, `${at}.source_fingerprint`, problems, { pattern: SHA256_HEX_RE })) {
    if (
      typeof expectedSourceFingerprint === "string" &&
      raw.source_fingerprint !== expectedSourceFingerprint
    ) {
      fail(
        problems,
        "schema",
        `${at}.source_fingerprint (${raw.source_fingerprint}) disagrees with transformation.source_fingerprint ` +
          `(${expectedSourceFingerprint}) — an equivalence review is tied to the exact source it reviewed`,
      );
    }
  }
  stringValue(raw.rationale, `${at}.rationale`, problems);
}

// validateMutantRecord validates one already-parsed record. `opts.root`
// resolves target_path/patch_path; `opts.contractIds`, when given, is the
// live contracts.json ID set — a non-synthetic record's violated_contracts
// entries are cross-checked against it, the same discipline
// lib/semantic-counterexamples.mjs applies to contract_id. A synthetic
// record's entries are never cross-checked against the real model at all:
// they name a placeholder, not standing evidence, and must carry the
// SYNTHETIC- prefix precisely so they can never be mistaken for one.
export function validateMutantRecord(raw, at, problems, opts = {}) {
  const root = opts.root ?? process.cwd();
  if (!exactObject(raw, MUTANT_KEYS, at, problems)) return null;
  if (raw.schema_version !== MUTANT_SCHEMA_VERSION) {
    fail(
      problems,
      "schema",
      `${at}.schema_version must be ${MUTANT_SCHEMA_VERSION}; got ${JSON.stringify(raw.schema_version)}`,
    );
  }

  let id = null;
  if (stringValue(raw.id, `${at}.id`, problems, { pattern: MUTANT_ID_RE })) id = raw.id;
  if (id !== null && at !== `${id}.json`) {
    fail(problems, "schema", `${at}: filename must be <id>.json (id is ${id})`);
  }

  stringValue(raw.title, `${at}.title`, problems);

  const synthetic = booleanValue(raw.synthetic, `${at}.synthetic`, problems) ? raw.synthetic : null;
  if (synthetic === true) {
    if (raw.synthetic_rationale === null) {
      fail(problems, "schema", `${at}.synthetic_rationale is required (non-null) when synthetic is true`);
    } else {
      nullableStringValue(raw.synthetic_rationale, `${at}.synthetic_rationale`, problems);
    }
  } else if (synthetic === false && raw.synthetic_rationale !== null) {
    fail(problems, "schema", `${at}.synthetic_rationale must be null when synthetic is false`);
  }

  if (
    stringArray(raw.violated_contracts, `${at}.violated_contracts`, problems, { allowEmpty: false })
  ) {
    for (let i = 0; i < raw.violated_contracts.length; i += 1) {
      const contractId = raw.violated_contracts[i];
      const path = `${at}.violated_contracts[${i}]`;
      if (synthetic === true) {
        if (!SYNTHETIC_CONTRACT_RE.test(contractId)) {
          fail(
            problems,
            "schema",
            `${path} must start with SYNTHETIC- when synthetic is true (never a real contract ID): ${contractId}`,
          );
        }
      } else {
        if (!CONTRACT_ID_RE.test(contractId)) {
          fail(problems, "schema", `${path} is not a well-formed contract ID: ${contractId}`);
        } else if (opts.contractIds && !opts.contractIds.has(contractId)) {
          fail(problems, "reference", `${path} references unknown contract ${contractId}`);
        }
      }
    }
  }

  let expectedSourceFingerprint = null;
  if (isObject(raw.transformation)) {
    validateTransformation(raw.transformation, `${at}.transformation`, problems, { root });
    if (typeof raw.transformation.source_fingerprint === "string") {
      expectedSourceFingerprint = raw.transformation.source_fingerprint;
    }
  } else {
    fail(problems, "schema", `${at}.transformation must be an object`);
  }

  if (!Array.isArray(raw.detectors)) {
    fail(problems, "schema", `${at}.detectors must be an array`);
  } else if (raw.detectors.length === 0) {
    fail(problems, "schema", `${at}.detectors must not be empty`);
  } else {
    const seenIds = new Set();
    raw.detectors.forEach((d, i) => validateDetector(d, `${at}.detectors[${i}]`, problems, seenIds));
  }

  enumValue(raw.expected_detection, CLASSIFICATION_SET, `${at}.expected_detection`, problems);
  if (
    synthetic !== true &&
    CLASSIFICATION_SET.has(raw.expected_detection) &&
    !NON_SYNTHETIC_CLASSIFICATIONS.has(raw.expected_detection)
  ) {
    fail(
      problems,
      "schema",
      `${at}.expected_detection is "${raw.expected_detection}", but a non-synthetic record may only declare ` +
        `${[...NON_SYNTHETIC_CLASSIFICATIONS].join(" or ")} — a bare non-killed outcome on real evidence is an ` +
        "expected-failure/tolerance-list entry, forbidden by repo invariant 7",
    );
  }
  if (raw.expected_detection === "equivalent-reviewed" && raw.equivalence_review === null) {
    fail(
      problems,
      "schema",
      `${at}.equivalence_review is required (non-null) when expected_detection is "equivalent-reviewed"`,
    );
  } else if (raw.expected_detection !== "equivalent-reviewed" && raw.equivalence_review !== null) {
    fail(
      problems,
      "schema",
      `${at}.equivalence_review must be null unless expected_detection is "equivalent-reviewed"`,
    );
  } else if (isObject(raw.equivalence_review)) {
    validateEquivalenceReview(raw.equivalence_review, `${at}.equivalence_review`, problems, {
      expectedSourceFingerprint,
    });
  }

  if (isObject(raw.isolation)) {
    validateIsolation(raw.isolation, `${at}.isolation`, problems);
  } else {
    fail(problems, "schema", `${at}.isolation must be an object`);
  }

  nullableStringValue(raw.notes, `${at}.notes`, problems);

  // linked_issue is the traceability seam issue #3452's own acceptance
  // criteria requires — but it can only ever be a DECLARATION-TIME
  // annotation, never a declaration-time REQUIREMENT: #3520/#3532 already
  // forbid a non-synthetic record from declaring expected_detection
  // "survived" at all (NON_SYNTHETIC_CLASSIFICATIONS above), so "this
  // record's own declared status is survived" can never be true for a
  // record that loads at all — a schema rule keyed off it would be
  // unreachable dead code. The real traceability case #3452 cares about is
  // a REAL regression an actual execution OBSERVED (test/semantic/
  // mutant-executions.json, via resolveDisposition in
  // lib/semantic-mutation-report.mjs) even though this record still
  // declares "killed"/"equivalent-reviewed" — a fact only knowable at
  // report-build time, never at record-authoring time, so lib/semantic-
  // mutation-report.mjs's own unresolvedSurvivors (keyed off the RESOLVED
  // disposition, not this field's declared expected_detection) is where
  // that requirement actually lives; see its own header. This function only
  // validates the field's SHAPE: null on a synthetic record (nothing real
  // to link), otherwise either null (not yet linked) or a positive integer
  // — a non-synthetic record's author may set it ahead of any observation,
  // to pre-acknowledge a known regression, or after one via the report's
  // own unresolved_survivors list telling them which record needs it.
  if (synthetic === true && raw.linked_issue !== null) {
    fail(problems, "schema", `${at}.linked_issue must be null on a synthetic record`);
  } else if (raw.linked_issue !== null) {
    positiveIntegerValue(raw.linked_issue, `${at}.linked_issue`, problems);
  }

  return id;
}

// loadMutants reads every *.json file directly under `dir` (default
// test/semantic/mutants; fixtures/ and patches/ are subdirectories, never
// matched by this non-recursive listing) as one mutant record each. Throws
// SemanticModelError, across every file, on any problem — never fail-fast on
// the first bad file, mirroring loadCounterexamples.
export function loadMutants(dir = DEFAULT_MUTANTS_DIR, { root = process.cwd(), contractIds } = {}) {
  const base = resolve(root, dir);
  const problems = [];
  const records = new Map();

  let filenames;
  try {
    filenames = readdirSyncJSON(base);
  } catch (error) {
    throw new SemanticModelError("invalid semantic mutant records", [
      `[schema] cannot read ${base}: ${error.message}`,
    ]);
  }

  for (const filename of filenames) {
    const path = join(base, filename);
    const raw = parseJSONFile(path, filename);
    const id = validateMutantRecord(raw, filename, problems, { root, contractIds });
    if (id === null) continue;
    if (records.has(id)) {
      fail(problems, "schema", `${filename}: id is a duplicate across mutant records: ${id}`);
      continue;
    }
    records.set(id, { ...raw, at: filename });
  }

  if (problems.length > 0) {
    throw new SemanticModelError("invalid semantic mutant records", problems);
  }
  return records;
}

function readdirSyncJSON(base) {
  return readdirSync(base)
    .filter((name) => name.endsWith(".json"))
    .sort();
}

export function renderMutantsSummary(records) {
  const byExpectation = Object.fromEntries(CLASSIFICATIONS.map((c) => [c, 0]));
  for (const [, record] of records) byExpectation[record.expected_detection] += 1;
  const parts = CLASSIFICATIONS.filter((c) => byExpectation[c] > 0).map(
    (c) => `${c}: ${byExpectation[c]}`,
  );
  return [`- mutants: **${records.size}** (${parts.join(", ")})`].join("\n");
}

// ---------------------------------------------------------------------------
// Execution ledger (test/semantic/mutant-executions.json)
// ---------------------------------------------------------------------------
//
// A mutant record's `expected_detection` (above) is a DECLARATION, authored
// once and re-verified by every CI run of `semantic-mutation-corpus.mjs`
// (required, `ci.check`) — but a declaration is not itself an observation,
// exactly the distinction lib/semantic-report.mjs already draws between a
// contract's BOUND evidence and its OBSERVED evidence (executions.json).
// This ledger is that same split applied to the mutation pilot: one
// revision-bound, hand-reviewed record of what a REAL `runMutant` execution
// actually reported, independent of what the record declares. Hand-authored
// and reviewed like executions.json itself (see semantic-execution-
// adapter.mjs's own header) — this module never writes it.
//
// WHY THIS MATTERS FOR STALENESS: an `equivalent-reviewed` mutant's audited
// review is tied to a source fingerprint (validateEquivalenceReview above)
// and runMutant() already falls back to a bare `survived` the moment that
// fingerprint drifts from the live target file — but a report built only
// from the STATIC record would never see that fallback happen; it would
// keep reading the record's own unchanged `expected_detection:
// "equivalent-reviewed"` forever. A ledger entry whose own `status` reports
// the ACTUAL post-fallback classification (e.g. "survived") is what lets a
// report built from this ledger reflect that expiry rather than silently
// keep crediting a stale adjudication.

export const DEFAULT_MUTANT_EXECUTIONS_PATH = "test/semantic/mutant-executions.json";
export const MUTANT_EXECUTION_SCHEMA_VERSION = 1;

const COMMIT_SHA_RE = /^[0-9a-f]{40}$/;
const MUTANT_EXECUTION_ID_RE = /^MUTEXEC-[A-Z][A-Z0-9-]*$/;

const MUTANT_EXECUTION_KEYS = new Set([
  "id",
  "mutant",
  "observed_at",
  "status",
  "run_ref",
  "source_sha",
  "detectors",
]);

const MUTANT_EXECUTION_DETECTOR_KEYS = new Set(["id", "classification", "duration_ms"]);

// duration_ms is the runtime/cost metadata issue #3452's own acceptance
// criteria names ("exact detector outcomes and runtime/cost metadata") —
// runGoTest's own durationMs, milliseconds wall-clock for that ONE detector
// run (never a sum across detectors, and never the clean control's own
// duration, which this ledger does not separately carry). Nullable: a
// hand-authored or pre-#3452 entry may not have timed anything.
function validateMutantExecutionDetector(raw, at, problems) {
  if (!exactObject(raw, MUTANT_EXECUTION_DETECTOR_KEYS, at, problems)) return;
  stringValue(raw.id, `${at}.id`, problems);
  enumValue(raw.classification, CLASSIFICATION_SET, `${at}.classification`, problems);
  if (raw.duration_ms !== null) positiveIntegerValue(raw.duration_ms, `${at}.duration_ms`, problems);
}

function validateMutantExecution(raw, at, problems, { mutantIds } = {}) {
  if (!exactObject(raw, MUTANT_EXECUTION_KEYS, at, problems)) return null;
  let id = null;
  if (stringValue(raw.id, `${at}.id`, problems, { pattern: MUTANT_EXECUTION_ID_RE })) id = raw.id;
  if (stringValue(raw.mutant, `${at}.mutant`, problems, { pattern: MUTANT_ID_RE })) {
    if (mutantIds && !mutantIds.has(raw.mutant)) {
      fail(problems, "reference", `${at}.mutant references unknown mutant record ${raw.mutant}`);
    }
  }
  stringValue(raw.observed_at, `${at}.observed_at`, problems, { pattern: OBSERVED_AT_RE });
  enumValue(raw.status, CLASSIFICATION_SET, `${at}.status`, problems);
  stringValue(raw.run_ref, `${at}.run_ref`, problems);
  nullableStringValue(raw.source_sha, `${at}.source_sha`, problems, { pattern: COMMIT_SHA_RE });
  if (Array.isArray(raw.detectors)) {
    raw.detectors.forEach((d, i) =>
      validateMutantExecutionDetector(d, `${at}.detectors[${i}]`, problems),
    );
  } else {
    fail(problems, "schema", `${at}.detectors must be an array (possibly empty)`);
  }
  return id;
}

// loadMutantExecutions reads test/semantic/mutant-executions.json (default
// path), the hand-authored, revision-bound observation ledger for the
// mutation pilot — never generated, exactly like test/semantic/
// executions.json. Missing file is NOT an error: it returns an empty Map,
// so a report built from it falls back to each record's own declared
// expected_detection (the same "absent observation reports unknown, never
// an assumed pass" discipline lib/semantic-report.mjs already applies to
// contract bindings) rather than refusing to run at all.
export function loadMutantExecutions(
  path = DEFAULT_MUTANT_EXECUTIONS_PATH,
  { root = process.cwd(), mutantIds } = {},
) {
  const absolute = resolve(root, path);
  if (!existsSync(absolute)) return new Map();
  const raw = parseJSONFile(absolute, path);

  const problems = [];
  if (raw?.schema_version !== MUTANT_EXECUTION_SCHEMA_VERSION) {
    fail(
      problems,
      "schema",
      `${path}.schema_version must be ${MUTANT_EXECUTION_SCHEMA_VERSION}; got ${JSON.stringify(raw?.schema_version)}`,
    );
  }
  if (!Array.isArray(raw?.executions)) {
    fail(problems, "schema", `${path}.executions must be an array`);
  }
  if (problems.length > 0) throw new SemanticModelError("invalid mutant execution ledger", problems);

  const seenIds = new Set();
  const records = [];
  raw.executions.forEach((entry, i) => {
    const at = `${path}.executions[${i}]`;
    const id = validateMutantExecution(entry, at, problems, { mutantIds });
    if (id !== null) {
      if (seenIds.has(id)) fail(problems, "schema", `${at}.id is a duplicate across execution records: ${id}`);
      seenIds.add(id);
    }
    records.push(entry);
  });
  if (problems.length > 0) throw new SemanticModelError("invalid mutant execution ledger", problems);

  // Most-recent-observed-first per mutant: ties on observed_at break on id
  // descending, mirroring lib/semantic-report.mjs's latestExecution() — an
  // arbitrary but deterministic tiebreak, since real data never collides.
  const byMutant = new Map();
  for (const record of records) {
    const current = byMutant.get(record.mutant);
    if (
      !current ||
      record.observed_at > current.observed_at ||
      (record.observed_at === current.observed_at && record.id > current.id)
    ) {
      byMutant.set(record.mutant, record);
    }
  }
  return byMutant;
}

// ---------------------------------------------------------------------------
// Execution engine
// ---------------------------------------------------------------------------

// GO_TEST_TIMEOUT_PANIC_RE matches the exact signature Go's own `-timeout`
// watchdog prints when it fires: `panic: test timed out after <duration>`,
// immediately followed by every goroutine's stack — the signature
// mutant-memory-guard.mjs's own header already documents for the sibling
// gremlins lane. runGoTest always passes `-timeout` (see below), so this is
// the FIRST mechanism a stuck detector should ever trip, ahead of this
// runner's own wall-clock backstop.
const GO_TEST_TIMEOUT_PANIC_RE = /^panic: test timed out after /m;

// classifyGoTestOutput is the pure verdict reader: given exactly what a `go
// test` child produced, it returns one of killed/survived/build-failed/
// timeout/infrastructure-error — never invalid-transform or
// equivalent-reviewed, which are decided outside the go test invocation
// entirely (before it, and as a post-processing override, respectively).
//
// Ordering is deliberate and load-bearing:
//   1. timedOut (this runner's own wall-clock SIGKILL, the backstop — see
//      runGoTest) is checked first, since a killed-by-us child's exit
//      code/signal say nothing about the mutation.
//   2. Go's own `-timeout` panic signature is checked next: this is the
//      mechanism that should normally catch a hang, since (unlike this
//      runner's SIGKILL) it dumps every goroutine's stack before the
//      process exits on its own.
//   3. an external signal (OOM, SIGSEGV — anything neither this runner nor
//      Go's own watchdog sent) is infrastructure-error: no test harness
//      ever reported.
//   4. `[build failed]` is checked before either PASS or FAIL, because a
//      failed compile always exits non-zero and its FAIL line would
//      otherwise be misread as a real assertion failure. (Go's compiler
//      diagnostics themselves land on stderr, not stdout — this check
//      deliberately reads only the `[build failed]` trailer `go test`'s own
//      driver writes to stdout, never a heuristic prefix match that could
//      false-positive on a passing test's own printed output.)
//   5. exit 0 with a bare `PASS` is survived; exit 0 with anything else
//      (should not happen for a well-behaved go test, but is not assumed) is
//      infrastructure-error rather than guessed at.
//   6. non-zero exit with at least one well-formed `--- FAIL: TestName` line
//      is killed — a panic Go's OWN test harness caught (inside the failing
//      test's own goroutine) and reported this way counts (the spike's own
//      finding: a caught panic reports explicitly, just like an assertion
//      failure). A panic on a goroutine the testing package is NOT
//      supervising never produces a `--- FAIL:` line at all — the Go
//      runtime tears the whole process down instead — so that shape
//      classifies as infrastructure-error, a known limitation stated
//      plainly in this file's header rather than a silent wrong answer — it
//      fails loudly today, just under a different bucket than `killed`, if
//      a real mutation from #3449-#3451 ever takes that exact shape. The
//      package-level
//      `FAIL\t<pkg>\t<duration>` trailer is NOT sufficient on its own: `go
//      test`'s driver prints that whenever the test BINARY exits non-zero
//      for any reason, including a direct os.Exit() call that bypasses the
//      testing package's own per-test reporting entirely — so only the
//      per-test `--- FAIL:` line, which only testing's own harness ever
//      prints, counts as an adjudication. Anything non-zero without one is
//      infrastructure-error: the process crashed in a way no test harness
//      ever adjudicated, and a crash is never a semantic kill.
export function classifyGoTestOutput({ exitCode, signal, stdout, timedOut }) {
  if (timedOut) return "timeout";
  const text = stdout ?? "";
  if (GO_TEST_TIMEOUT_PANIC_RE.test(text)) return "timeout";
  if (signal !== null && signal !== undefined) return "infrastructure-error";
  if (/\[build failed\]/.test(text)) return "build-failed";
  if (exitCode === 0) {
    return /^PASS$/m.test(text) ? "survived" : "infrastructure-error";
  }
  return /^--- FAIL: /m.test(text) ? "killed" : "infrastructure-error";
}

// The three outcomes in which a detector's harness never adjudicated the
// mutation at all. A record carrying one of these on ANY detector is an
// incomplete measurement, whatever its other detectors said.
const HARNESS_OUTCOMES = Object.freeze(["build-failed", "timeout", "infrastructure-error"]);

// aggregateClassifications folds one mutant's per-detector verdicts into one
// overall verdict, in this precedence:
//   1. a harness outcome (build-failed / timeout / infrastructure-error,
//      in CLASSIFICATIONS order) on ANY detector wins — an unadjudicated
//      detector is a broken measurement, and folding it under a sibling's
//      `survived` (which a non-null equivalence_review then promotes to
//      equivalent-reviewed, exit 0) or `killed` would hide a detector that
//      never compiled or never finished behind a green run;
//   2. `killed` from any detector — one detector catching the mutation is
//      enough to call it caught, once every detector actually ran;
//   3. the first remaining verdict in CLASSIFICATIONS order.
// An all-equal set collapses to that one value trivially under the same
// rule.
export function aggregateClassifications(verdicts) {
  if (verdicts.length === 0) throw new Error("aggregateClassifications: no verdicts");
  for (const c of HARNESS_OUTCOMES) {
    if (verdicts.includes(c)) return c;
  }
  if (verdicts.includes("killed")) return "killed";
  for (const c of CLASSIFICATIONS) {
    if (verdicts.includes(c)) return c;
  }
  throw new Error(`aggregateClassifications: unrecognised verdict(s) ${JSON.stringify(verdicts)}`);
}

export function selectDetectors(record, detectorId) {
  const all = record.detectors;
  const selected = detectorId ? all.filter((d) => d.id === detectorId) : all;
  if (selected.length === 0) {
    throw new Error(
      detectorId
        ? `no detector named "${detectorId}" on mutant ${record.id} (has: ${all.map((d) => d.id).join(", ")})`
        : `mutant ${record.id} declares zero detectors — refusing to run a measurement with nothing to detect`,
    );
  }
  return selected;
}

// applyTransformation materializes the mutant's overlay file inside
// `scratchDir`, without ever writing to the real target. Returns
// { ok:true, mutatedAbsPath, targetAbsPath, observedSourceFingerprint,
// observedMutatedFingerprint } or { ok:false, reason, detail, ... } — a
// `reason` of "source-fingerprint-mismatch" is checked and can fail BEFORE
// git apply ever runs, which is what makes the fail-closed guarantee hold
// even for a patch that would otherwise still (perhaps fuzzily) apply.
export function applyTransformation({ record, root, scratchDir, spawnSyncFn = spawnSync }) {
  const targetAbsPath = resolve(root, record.transformation.target_path);
  const patchAbsPath = resolve(root, record.transformation.patch_path);

  const originalBytes = readFileSync(targetAbsPath);
  const observedSourceFingerprint = sha256Hex(originalBytes);
  if (observedSourceFingerprint !== record.transformation.source_fingerprint) {
    return {
      ok: false,
      reason: "source-fingerprint-mismatch",
      detail:
        `declared source_fingerprint ${record.transformation.source_fingerprint} disagrees with the ` +
        `live target file (observed ${observedSourceFingerprint}) — refusing to test a stale mutant`,
      observedSourceFingerprint,
    };
  }

  const scratchTargetPath = join(scratchDir, record.transformation.target_path);
  mkdirSync(dirname(scratchTargetPath), { recursive: true });
  copyFileSync(targetAbsPath, scratchTargetPath);

  const applyArgs = ["apply", "--unsafe-paths", `--directory=${scratchDir}`, patchAbsPath];
  const checkResult = spawnSyncFn("git", ["apply", "--check", ...applyArgs.slice(1)], {
    cwd: root,
    encoding: "utf8",
  });
  if (checkResult.status !== 0) {
    return {
      ok: false,
      reason: "patch-check-failed",
      detail: (checkResult.stderr || checkResult.stdout || "").trim(),
      observedSourceFingerprint,
    };
  }

  const applyResult = spawnSyncFn("git", applyArgs, { cwd: root, encoding: "utf8" });
  if (applyResult.status !== 0) {
    return {
      ok: false,
      reason: "patch-apply-failed",
      detail: (applyResult.stderr || applyResult.stdout || "").trim(),
      observedSourceFingerprint,
    };
  }

  const mutatedBytes = readFileSync(scratchTargetPath);
  const observedMutatedFingerprint = sha256Hex(mutatedBytes);
  if (observedMutatedFingerprint !== record.transformation.expected_mutated_fingerprint) {
    return {
      ok: false,
      reason: "mutated-fingerprint-mismatch",
      detail:
        `applied patch produced ${observedMutatedFingerprint}, expected ` +
        `${record.transformation.expected_mutated_fingerprint} — the patch applied somewhere unexpected`,
      observedSourceFingerprint,
      observedMutatedFingerprint,
    };
  }

  return {
    ok: true,
    mutatedAbsPath: scratchTargetPath,
    targetAbsPath,
    observedSourceFingerprint,
    observedMutatedFingerprint,
  };
}

export function buildOverlay(targetAbsPath, mutatedAbsPath) {
  return { Replace: { [targetAbsPath]: mutatedAbsPath } };
}

// readMemoryBreaches reads mutant-memory-guard.mjs's own ledger (one JSON
// object per line, appended on every breach — see that script's header) and
// returns the parsed breach records, or [] when the file was never written
// (the common case: no breach occurred). This MUST run before the scratch
// directory is removed — runMutant calls it immediately after each detector
// run, while the CLI still owns cleanup for later — because a memory breach
// is otherwise real evidence that gets collapsed into a bare `timeout`
// (this runner's own wall-clock kill reaping the whole tree) with no trace
// of WHY the detector actually hung.
export function readMemoryBreaches(ledgerPath) {
  let text;
  try {
    text = readFileSync(ledgerPath, "utf8");
  } catch {
    return [];
  }
  return text
    .split("\n")
    .filter((line) => line.trim() !== "")
    .map((line) => JSON.parse(line));
}

// runGoTestPollIntervalMs is how often the wall-clock timeout enforcer polls
// elapsed time before SIGKILLing a stuck detector. Coarse on purpose —
// nothing observes it but the deadline itself.
const runGoTestPollIntervalMs = 100;

// outerTimeoutBackstopSeconds is the headroom this runner's own wall-clock
// kill adds ON TOP of a detector's declared timeout_seconds, which is passed
// to Go's own `-timeout` flag. Go's watchdog is meant to fire FIRST — it
// dumps every goroutine's stack before the process exits on its own, which a
// runner-level SIGKILL cannot produce (this repo's own documented timeout
// doctrine: just/test.just's own comment, pinned by
// test/regression/go_test_timeout_budget_test.go). This runner's SIGKILL
// exists only as a backstop for the case where Go's watchdog somehow does
// not fire — it is deliberately never the first mechanism to act.
const outerTimeoutBackstopSeconds = 5;

// runGoTest spawns exactly one `go test` invocation — a clean control when
// `overlayPath` is null, a mutant run when it names an overlay JSON file —
// wrapped by mutant-memory-guard.mjs via `-exec` exactly as its own header
// documents. `spawnFn` defaults to node:child_process's real spawn and is
// injectable so callers can test the orchestration above this function
// without compiling or running any Go code.
export function runGoTest({
  root,
  pkg,
  testRun,
  buildTags,
  overlayPath,
  timeoutSeconds,
  memoryMax,
  memoryHold,
  memoryLedgerPath,
  memoryGuardPath,
  spawnFn = spawn,
}) {
  // -v is required, not cosmetic: go test's default (non-verbose) success
  // output is a bare `ok  <pkg>  <duration>` line with no literal `PASS`,
  // while a FAILURE prints `--- FAIL:`/`FAIL\t` either way. Without -v,
  // classifyGoTestOutput's PASS check would misread every real pass as
  // infrastructure-error (no interpretable success marker) — verbose output
  // is what makes PASS and FAIL symmetric enough to classify from text alone.
  //
  // -timeout is Go's OWN watchdog (see outerTimeoutBackstopSeconds above) —
  // set to the detector's declared bound exactly, so it is what normally
  // ends a hang; this runner's SIGKILL below is only the backstop.
  //
  // -exec's value is double-quoted because go test -exec is whitespace-split
  // with no shell involved, so an unquoted path containing a space (a
  // worktree directory name, say) would silently truncate at the first one
  // — confirmed empirically: unquoted, `node` receives only the text before
  // the space as its entry script and fails with MODULE_NOT_FOUND; quoted,
  // go's own splitter keeps the quoted text as one field.
  const args = ["test", "-v", `-run=${testRun}`, `-timeout=${timeoutSeconds}s`];
  if (buildTags.length > 0) args.push(`-tags=${buildTags.join(",")}`);
  args.push(`-exec=node "${memoryGuardPath}"`);
  if (overlayPath) args.push(`-overlay=${overlayPath}`);
  args.push(pkg);

  const startedAt = Date.now();
  return new Promise((resolvePromise) => {
    // detached so the child becomes its own process-group leader: `go test`
    // spawns the test binary as a descendant via `-exec`, and killing only
    // the top-level `go` PID (the default, non-detached shape) leaves that
    // descendant — and mutant-memory-guard.mjs's own hold loop, should it be
    // mid-breach — running orphaned for up to Go's own timeout. Killing the
    // whole group (the negative-pid form below) reaps all of it at once.
    const child = spawnFn("go", args, {
      cwd: root,
      detached: true,
      env: {
        ...process.env,
        MUTANT_MEMORY_MAX: memoryMax,
        MUTANT_MEMORY_HOLD: memoryHold,
        MUTANT_MEMORY_LEDGER: memoryLedgerPath,
      },
    });

    let stdout = "";
    let stderr = "";
    let timedOut = false;
    child.stdout?.on("data", (chunk) => {
      stdout += chunk;
    });
    child.stderr?.on("data", (chunk) => {
      stderr += chunk;
    });

    const deadline = startedAt + (timeoutSeconds + outerTimeoutBackstopSeconds) * 1000;
    const poller = setInterval(() => {
      // child.exitCode is set (non-null) the instant the child has already
      // exited on its own — Go's own -timeout watchdog firing first is
      // exactly that case. Without this guard a poll landing in the ~100ms
      // window right after a clean exit could still flag a healthy run as
      // timedOut.
      if (Date.now() < deadline || child.exitCode !== null) return;
      timedOut = true;
      clearInterval(poller);
      try {
        process.kill(-child.pid, "SIGKILL");
      } catch {
        // The group may already be gone (a race with a natural exit right at
        // the deadline) — nothing left to kill is not a failure here.
      }
    }, runGoTestPollIntervalMs);

    child.on("error", (cause) => {
      clearInterval(poller);
      resolvePromise({
        exitCode: null,
        signal: null,
        stdout,
        stderr: `${stderr}\ncannot execute go: ${cause.message}`,
        timedOut: false,
        durationMs: Date.now() - startedAt,
      });
    });

    child.on("close", (exitCode, signal) => {
      clearInterval(poller);
      resolvePromise({
        exitCode,
        signal,
        stdout,
        stderr,
        timedOut,
        durationMs: Date.now() - startedAt,
      });
    });
  });
}

// runMutant executes the full protocol for one mutant record against its
// selected detector(s): clean control(s) first — any failure aborts the
// whole measurement as infrastructure-error before the mutant is ever
// attempted — then transformation materialization/verification, then the
// mutant run(s), classified and (for a bare `survived`) checked against an
// audited equivalence_review tied to the observed source fingerprint.
//
// `scratchDir` must already exist (see createScratchDir) and is never
// created or removed by this function — the CALLER owns its lifecycle, so
// it can register a signal handler that cleans it up on SIGINT/SIGTERM
// before this function's own control flow ever gets a chance to (see the
// CLI's cleanup wiring). This function only ever writes inside it.
export async function runMutant({
  record,
  root,
  scratchDir,
  detectorId,
  memoryGuardPath = resolve(root, DEFAULT_MEMORY_GUARD),
  spawnSyncFn = spawnSync,
  runGoTestFn = runGoTest,
}) {
  const detectors = selectDetectors(record, detectorId); // throws on zero-match — a hard usage error, never a classification
  const startedAt = new Date().toISOString();

  const cleanControls = [];
  for (const detector of detectors) {
    // eslint-disable-next-line no-await-in-loop -- detectors run sequentially by design: a clean-control failure must abort before any further work, and per-detector wall-clock budgets already assume no contention with a sibling detector's own run.
    const result = await runGoTestFn({
      root,
      pkg: detector.package,
      testRun: detector.test_run,
      buildTags: detector.build_tags,
      overlayPath: null,
      timeoutSeconds: detector.timeout_seconds,
      memoryMax: record.isolation.memory_max,
      memoryHold: record.isolation.memory_hold,
      memoryLedgerPath: join(scratchDir, `${detector.id}-clean-memory-ledger.jsonl`),
      memoryGuardPath,
    });
    const classification = classifyGoTestOutput(result);
    const memoryBreaches = readMemoryBreaches(join(scratchDir, `${detector.id}-clean-memory-ledger.jsonl`));
    cleanControls.push({ detector: detector.id, ...result, classification, memory_breaches: memoryBreaches });
    if (classification !== "survived") {
      return {
        mutant_id: record.id,
        status: "infrastructure-error",
        reason: "clean-control-failed",
        detail:
          `clean control for detector "${detector.id}" did not PASS (classified ${classification}) — ` +
          "the harness could not establish a baseline, so no mutant was ever attempted",
        target_path: record.transformation.target_path,
        patch_path: record.transformation.patch_path,
        detectors: detectors.map((d) => d.id),
        clean_controls: cleanControls,
        scratch_dir: scratchDir,
        started_at: startedAt,
        finished_at: new Date().toISOString(),
      };
    }
  }

  const transformed = applyTransformation({ record, root, scratchDir, spawnSyncFn });
  if (!transformed.ok) {
    return {
      mutant_id: record.id,
      status: "invalid-transform",
      reason: transformed.reason,
      detail: transformed.detail,
      target_path: record.transformation.target_path,
      patch_path: record.transformation.patch_path,
      source_fingerprint: {
        expected: record.transformation.source_fingerprint,
        observed: transformed.observedSourceFingerprint ?? null,
      },
      mutated_fingerprint: {
        expected: record.transformation.expected_mutated_fingerprint,
        observed: transformed.observedMutatedFingerprint ?? null,
      },
      detectors: detectors.map((d) => d.id),
      clean_controls: cleanControls,
      scratch_dir: scratchDir,
      started_at: startedAt,
      finished_at: new Date().toISOString(),
    };
  }

  const overlay = buildOverlay(transformed.targetAbsPath, transformed.mutatedAbsPath);
  const overlayPath = join(scratchDir, "overlay.json");
  writeFileSync(overlayPath, JSON.stringify(overlay));

  const mutantRuns = [];
  for (const detector of detectors) {
    // eslint-disable-next-line no-await-in-loop -- sequential by design, see the clean-control loop above.
    const result = await runGoTestFn({
      root,
      pkg: detector.package,
      testRun: detector.test_run,
      buildTags: detector.build_tags,
      overlayPath,
      timeoutSeconds: detector.timeout_seconds,
      memoryMax: record.isolation.memory_max,
      memoryHold: record.isolation.memory_hold,
      memoryLedgerPath: join(scratchDir, `${detector.id}-mutant-memory-ledger.jsonl`),
      memoryGuardPath,
    });
    const classification = classifyGoTestOutput(result);
    const memoryBreaches = readMemoryBreaches(join(scratchDir, `${detector.id}-mutant-memory-ledger.jsonl`));
    mutantRuns.push({ detector: detector.id, ...result, classification, memory_breaches: memoryBreaches });
  }

  let status = aggregateClassifications(mutantRuns.map((r) => r.classification));
  if (status === "survived" && record.equivalence_review !== null) {
    const review = record.equivalence_review;
    if (review.source_fingerprint === transformed.observedSourceFingerprint) {
      status = "equivalent-reviewed";
    }
    // A stale review (fingerprint disagrees with the live source) is
    // deliberately NOT applied — status stays "survived", which is the
    // fail-closed reading: re-review is required, never assumed.
  }

  return {
    mutant_id: record.id,
    status,
    target_path: record.transformation.target_path,
    patch_path: record.transformation.patch_path,
    source_fingerprint: {
      expected: record.transformation.source_fingerprint,
      observed: transformed.observedSourceFingerprint,
    },
    mutated_fingerprint: {
      expected: record.transformation.expected_mutated_fingerprint,
      observed: transformed.observedMutatedFingerprint,
    },
    detectors: detectors.map((d) => d.id),
    clean_controls: cleanControls,
    mutant_runs: mutantRuns,
    overlay_path: overlayPath,
    scratch_dir: scratchDir,
    started_at: startedAt,
    finished_at: new Date().toISOString(),
  };
}

// scratchRootFor picks the mkdtemp() base: RUNNER_TEMP (GitHub Actions' own
// per-job scratch disk) when set, else the OS temp dir. Either way mkdtemp's
// own random suffix is what guarantees concurrent runs never collide — this
// function never itself derives a name from anything caller-specific.
export function scratchRootFor(env = process.env) {
  return env.RUNNER_TEMP && env.RUNNER_TEMP.trim() !== "" ? env.RUNNER_TEMP : tmpdir();
}

// createScratchDir makes one fresh, uniquely-named scratch directory under
// `scratchRoot` (see scratchRootFor). The CALLER owns removing it — this
// split is what lets the CLI register a SIGINT/SIGTERM handler that can
// clean up the exact path even if a run is interrupted before runMutant's
// own return value (and any cleanup logic wrapped around it) is reached.
export function createScratchDir(scratchRoot) {
  return mkdtempSync(join(scratchRoot, "semantic-mutation-"));
}
