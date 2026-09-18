// semantic-execution-adapter.mjs — normalizes EXISTING Go test / compat
// harness report artifacts and GitHub Actions run identity into
// REVISION-BOUND execution observations for test/semantic/executions.json
// (cerberus issue #3459, epic #3421's "developer guidance" milestone).
//
// WHY THIS EXISTS. An executions.json record's original five fields
// (id/binding/observed_at/result/run_ref) say a verifier produced SOME
// verdict SOMETIME — they say nothing about WHICH candidate revision it
// ran against. A stale report from three commits ago, or a report from a
// job that never actually executed (a no-op short-circuit, a `-run` filter
// that matched nothing), can silently masquerade as fresh evidence. This
// module is the classification and normalization step that closes that
// gap — it never writes test/semantic/executions.json itself (the file
// stays hand-authored/reviewed, like every other file under
// test/semantic/, per .github/scripts/README.md's own "Semantic contract
// model" section); it prints normalized observation records a human
// reviews and appends. property.yml and compatibility.yml (issue #3499)
// are the CI steps that run this against real go-test-json / compat-cases
// artifacts and upload the printed records as a build artifact for that
// review — see .github/scripts/README.md's "Semantic execution adapter"
// section for the wiring.
//
// THE CORE JOIN: classifyRevisionBinding(candidate, observation).
// `candidate` is what THIS classification run asserts the evidence must
// be about (the commit actually being assessed, and — only when the
// caller chooses to assert them — the reference backend version and
// dataset/corpus fingerprint expected). `observation` is the raw facts
// read off an existing artifact (a go test -json stream, or a compat
// harness's compat-cases.json). The five non-evidence classes named in
// issue #3459's own acceptance criteria (wrong SHA, wrong reference, a
// stale corpus fingerprint, zero selected tests, an aggregate no-op) each
// resolve to one of the four NON-"executed" selection states
// lib/semantic-model.mjs's EXECUTION_SELECTIONS declares
// ("selected_not_run" / "no_op" / "stale" / "unavailable") — every one of
// which the model schema FORCES to carry result: "error", so the report's
// existing classifyExecutionObservation / classifyObservedEvidence rule
// (only a real pass/fail counts as observed evidence) already treats every
// one of them as non-evidence with ZERO changes to that rule. This module
// never invents a second "is this evidence" rule; it only ever produces
// records the EXISTING rule already classifies correctly.
//
// SCOPE, per the issue's own "Proposed change"/"Scope" sections: read and
// normalize what a driver ALREADY emits (compatibility/internal/score's
// CaseSet — the one shape all three compat heads already funnel through —
// and Go's own `go test -json` stream, which every `go test` invocation
// can already produce with no source change) plus GitHub Actions run
// identity (GITHUB_SHA / GITHUB_RUN_ID / GITHUB_RUN_ATTEMPT / GITHUB_JOB /
// GITHUB_EVENT_NAME, which every workflow step already receives with no
// explicit `env:` wiring). Dataset fingerprinting is the one place a fact
// genuinely does not exist anywhere in this repository yet (confirmed: no
// corpus/dataset content hash exists in compatibility/ or test/ today) —
// this module computes it directly by hashing the corpus file from the
// checked-out working tree, needing no change to any Go driver.
//
// NON-GOALS this module holds itself to (issue #3459's own): no external
// evidence database (the only persistence is the hand-authored
// executions.json a human/CI reviews and appends to); no automatic
// reruns; no inferring "pass" from a missing or unreadable report (a
// missing report simply cannot be classified — see parseCaseSet /
// parseGoTestJSONShapeResults below, which throw rather than guess); no
// new freshness/time constants (dataset "staleness" here means a CONTENT
// mismatch against the candidate's own fingerprint, never a clock); no
// change to release-preflight.mjs's publication gate.

import { readFileSync, readdirSync, statSync } from "node:fs";
import { join, relative } from "node:path";
import { createHash } from "node:crypto";

import {
  sha256Hex,
  stampedRecordId, EXECUTION_EVENTS, EXECUTION_SELECTIONS } from "./semantic-model.mjs";

const EXECUTION_EVENTS_SET = new Set(EXECUTION_EVENTS);
const EXECUTION_SELECTIONS_SET = new Set(EXECUTION_SELECTIONS);
const REAL_RESULTS = new Set(["pass", "fail"]);

export class SemanticExecutionAdapterError extends Error {
  constructor(problems) {
    super(`semantic execution adapter:\n${problems.map((p) => `- ${p}`).join("\n")}`);
    this.name = "SemanticExecutionAdapterError";
    this.problems = problems;
  }
}

function fail(problems) {
  throw new SemanticExecutionAdapterError(problems);
}

// --- Hashing (dataset/corpus fingerprint) -----------------------------------

/** sha256 hex digest of a file's current on-disk content. */
export function hashFile(path) {
  return sha256Hex(readFileSync(path));
}

/**
 * sha256 hex digest of ONE corpus path — a single file, or a whole
 * directory walked recursively with every file's repo-relative path hashed
 * ALONGSIDE its content (not content alone), so adding, removing, or
 * renaming a query file changes the fingerprint even when every surviving
 * file's bytes are untouched. Entries are sorted by relative path first, so
 * the result is independent of directory-listing order.
 */
function hashOneCorpusPath(path) {
  const st = statSync(path);
  if (st.isFile()) return hashFile(path);
  if (!st.isDirectory()) {
    fail([`corpus path ${JSON.stringify(path)} is neither a file nor a directory`]);
  }
  const files = [];
  const walk = (dir) => {
    for (const entry of readdirSync(dir, { withFileTypes: true }).sort((a, b) => (a.name < b.name ? -1 : 1))) {
      const abs = join(dir, entry.name);
      if (entry.isDirectory()) walk(abs);
      else if (entry.isFile()) files.push(abs);
    }
  };
  walk(path);
  const hash = createHash("sha256");
  for (const abs of files) {
    hash.update(relative(path, abs).replaceAll("\\", "/"));
    hash.update("\0");
    hash.update(readFileSync(abs));
    hash.update("\0");
  }
  return hash.digest("hex");
}

/**
 * sha256 hex digest of a corpus (`hashOneCorpusPath`, single file/directory
 * form — unchanged, byte-identical to before this array form existed) OR of
 * SEVERAL corpus roots folded into one digest (an array), for the case a
 * single harness run draws its queries from more than one root — loki's
 * `-corpus`/`-cerberus-queries` flags each name a DIFFERENT directory
 * (`upstream/loki-bench/queries` and `cerberus-queries`), and hashing only
 * one of them leaves the fingerprint blind to drift in the other. Each
 * root's own digest is combined with that root's own path string (so two
 * corpora can never collide into the same combined digest merely by having
 * matching content), sorted by path first so the result is independent of
 * argument order — the same order-independence guarantee a single
 * directory's own file listing already gets.
 */
export function hashCorpus(path) {
  if (!Array.isArray(path)) return hashOneCorpusPath(path);
  if (path.length === 0) {
    fail(["hashCorpus: an array of corpus paths must not be empty"]);
  }
  const hash = createHash("sha256");
  for (const p of [...path].sort()) {
    hash.update(p);
    hash.update("\0");
    hash.update(hashOneCorpusPath(p));
    hash.update("\0");
  }
  return hash.digest("hex");
}

// --- GitHub Actions run identity --------------------------------------------

/**
 * Reads the run-identity fields GitHub Actions sets automatically on every
 * step's shell environment (no explicit workflow `env:` wiring needed —
 * confirmed absent from every relevant step in property.yml and
 * compatibility.yml today, so this is genuinely free). `event` is narrowed
 * to the model's own EXECUTION_EVENTS vocabulary; an event outside it
 * (there is no sixth trigger this repo's lanes use) reads as null rather
 * than smuggling an unvalidated string into a record the model would
 * reject anyway.
 */
export function githubRunContext(env = process.env) {
  const event = env.GITHUB_EVENT_NAME;
  return {
    sourceSha: env.GITHUB_SHA || null,
    runId: env.GITHUB_RUN_ID || null,
    runAttempt: env.GITHUB_RUN_ATTEMPT || null,
    job: env.GITHUB_JOB || null,
    event: EXECUTION_EVENTS_SET.has(event) ? event : null,
  };
}

// --- Run/context helpers shared by the CLI and CI orchestration scripts ----
//
// A candidate/run-identity resolution a caller reaches for regardless of
// which artifact it is normalizing: the CLI's three MODEs need it, and so
// does a script that fans ONE CI run's evidence out over SEVERAL bindings
// (compat-execution-report.mjs, issue #3499) instead of the CLI's one
// caller-named BINDING. Shared here so CANDIDATE_SHA/RUN_REF/OBSERVED_AT
// resolution and the executions.json id scheme can never drift between the
// CLI and an orchestrator built on top of it.

/**
 * The run_ref a record should carry when RUN_REF is not set explicitly: a
 * constructed Actions run URL when the three GITHUB_* run-identity vars are
 * present, else null (never a placeholder string — sharedContext below is
 * the one caller that turns a null run_ref into a readable fallback for a
 * human).
 */
export function defaultRunRef(env) {
  if (!env.GITHUB_SERVER_URL || !env.GITHUB_REPOSITORY || !env.GITHUB_RUN_ID) return null;
  return `${env.GITHUB_SERVER_URL}/${env.GITHUB_REPOSITORY}/actions/runs/${env.GITHUB_RUN_ID}`;
}

/**
 * Resolves the candidate SHA, run_ref, and observed_at every mode needs,
 * plus the raw githubRunContext() fields. `env.CANDIDATE_SHA` wins over
 * `GITHUB_SHA` when both are set (a developer overriding a local run).
 */
export function sharedContext(env = process.env) {
  const run = githubRunContext(env);
  const candidateSha = env.CANDIDATE_SHA || run.sourceSha;
  if (!candidateSha) {
    fail(["CANDIDATE_SHA is required (or GITHUB_SHA, when run as a workflow step)"]);
  }
  return {
    candidateSha,
    runRef: env.RUN_REF || defaultRunRef(env) || "(no run_ref available)",
    observedAt: env.OBSERVED_AT || new Date().toISOString(),
    run,
  };
}

/** Deterministic execution id: EXEC-<binding, BINDING- prefix stripped>-<YYYYMMDDTHHMMSS>. */
export function execIdFor(bindingId, observedAt) {
  return stampedRecordId("EXEC", bindingId.replace(/^BINDING-/, ""), observedAt);
}

// --- Revision-binding classification (acceptance criterion 1) --------------

/**
 * The single classification rule this module offers. `candidate` states
 * what this run asserts the evidence must be about:
 *   sourceSha            required — the commit actually being assessed
 *   referenceVersion      optional — asserted ONLY when the caller wants a
 *                         reference-version mismatch to flag; omitted
 *                         (undefined/null) means "not asserted, don't check"
 *   datasetFingerprint    optional, same asserted-or-skipped rule
 * `observation` is the raw facts read off an artifact (see
 * parseGoTestJSONShapeResults / parseCaseSet below, which produce this
 * shape's selectedCount/ranCount/result fields directly):
 *   sourceSha, referenceVersion, datasetFingerprint  (nullable — absence
 *     from the raw artifact itself is a legitimate "not available", never
 *     coerced into a false match)
 *   selectedCount         how many cases/subtests the run actually selected
 *   ranCount               how many of those produced a real terminal verdict
 *   aggregateNoOp          true when the WHOLE run short-circuited (e.g.
 *     property.yml's "No-op (non-release PR)" branch, or a harness that
 *     never started) — checked FIRST, since a no-op run's sourceSha can
 *     legitimately still match the candidate; it just did no work
 *   result                 "pass" | "fail" | anything else (a raw non-verdict)
 *
 * Returns { selection, evidence, result, reason }: `evidence: true` only
 * for selection "executed" with a real pass/fail; every other branch sets
 * result: "error" and a non-null reason naming exactly which of the five
 * acceptance-criterion conditions fired, in priority order:
 *   aggregate no-op > wrong SHA > wrong reference > stale corpus
 *   fingerprint > zero selected > (selected but none ran) > malformed result
 */
export function classifyRevisionBinding(candidate, observation) {
  if (!candidate || typeof candidate.sourceSha !== "string" || candidate.sourceSha === "") {
    fail(["candidate.sourceSha is required (the commit this classification is FOR)"]);
  }
  if (!observation || typeof observation !== "object") {
    fail(["observation is required"]);
  }

  const nonEvidence = (selection, reason) => ({ selection, evidence: false, result: "error", reason });

  if (observation.aggregateNoOp) {
    return nonEvidence(
      "no_op",
      "the run short-circuited to a no-op — no verifier work was attempted at all",
    );
  }
  if (!observation.sourceSha || observation.sourceSha !== candidate.sourceSha) {
    return nonEvidence(
      "unavailable",
      `observation source_sha ${JSON.stringify(observation.sourceSha ?? null)} does not match candidate ${JSON.stringify(candidate.sourceSha)} — this observation is not evidence about the requested revision`,
    );
  }
  if (
    candidate.referenceVersion != null &&
    observation.referenceVersion !== candidate.referenceVersion
  ) {
    return nonEvidence(
      "unavailable",
      `observation reference_version ${JSON.stringify(observation.referenceVersion ?? null)} does not match the candidate's asserted ${JSON.stringify(candidate.referenceVersion)}`,
    );
  }
  if (
    candidate.datasetFingerprint != null &&
    observation.datasetFingerprint !== candidate.datasetFingerprint
  ) {
    return nonEvidence(
      "stale",
      `observation dataset_fingerprint ${JSON.stringify(observation.datasetFingerprint ?? null)} does not match the candidate's current corpus fingerprint ${JSON.stringify(candidate.datasetFingerprint)} — the corpus has changed since this observation was produced`,
    );
  }
  if (!(observation.selectedCount > 0)) {
    return nonEvidence(
      "selected_not_run",
      `test selection matched zero cases (selectedCount=${observation.selectedCount ?? 0})`,
    );
  }
  if (!(observation.ranCount > 0)) {
    return nonEvidence(
      "unavailable",
      `${observation.selectedCount} case(s) were selected but none produced a recorded terminal verdict`,
    );
  }
  if (!REAL_RESULTS.has(observation.result)) {
    return nonEvidence(
      "unavailable",
      `observation result ${JSON.stringify(observation.result)} is not a real pass/fail verdict`,
    );
  }
  return { selection: "executed", evidence: true, result: observation.result, reason: null };
}

// --- Normalization to the executions.json record shape ----------------------

/**
 * Renders one classifyRevisionBinding() result as an executions.json-shaped
 * record (lib/semantic-model.mjs's EXECUTION_KEYS — every key present,
 * absence expressed as an explicit `null`, never omission, matching
 * exactObject's own documented discipline). Never writes the file itself —
 * the caller (a CLI, or a human) decides whether/where to append it.
 */
export function toExecutionRecord({ id, binding, observedAt, runRef, classification, context = {} }) {
  if (!EXECUTION_SELECTIONS_SET.has(classification.selection)) {
    fail([`classification.selection ${JSON.stringify(classification.selection)} is not a known selection`]);
  }
  return {
    id,
    binding,
    observed_at: observedAt,
    result: classification.result,
    run_ref: runRef,
    selection: classification.selection,
    selection_reason: classification.evidence ? null : classification.reason,
    source_sha: context.sourceSha ?? null,
    run_id: context.runId ?? null,
    run_attempt: context.runAttempt ?? null,
    job: context.job ?? null,
    event: context.event ?? null,
    substrate: context.substrate ?? null,
    reference_version: context.referenceVersion ?? null,
    dataset_fingerprint: context.datasetFingerprint ?? null,
  };
}

// --- go test -json parsing (property lane) -----------------------------------
//
// `go test -json` is a standard Go toolchain capability — no source change
// to test/property or property-fanout.mjs is needed to produce it; any
// caller (a developer, or a future CI step) runs
// `go test -tags chdb,agpl_oracle,chdb_agpl_oracle -json ./test/property/...`
// and feeds the output here. test/property/framework.go's RunShapeExamples
// / RunShapeCases call `t.Run(string(shapeID), ...)` directly, so a
// shape's own subtest name is EXACTLY its ShapeID string with no
// sanitization needed (cerberus ShapeIDs are dotted lowercase identifiers,
// none of the characters go test's testName sanitizer rewrites) — the
// LAST "/"-separated segment of a terminal event's Test field is compared
// against the shape roster directly, with no separate translation table.

const TERMINAL_ACTIONS = new Set(["pass", "fail", "skip"]);

/**
 * Parses an NDJSON `go test -json` stream into a Map from each terminal
 * subtest's last "/"-segment to its terminal action ("pass"/"fail"/"skip").
 * A later terminal event for the same Test name overwrites an earlier one,
 * matching go test's own one-terminal-event-per-test contract. Lines that
 * are not valid JSON (build output interleaved on some Go versions) are
 * skipped rather than failing the whole parse — a truncated/garbled stream
 * still yields whatever real terminal events it does carry, and a shape ID
 * absent from the result reads as "not selected" via
 * classifyRevisionBinding, never as a hard error.
 */
export function parseGoTestJSONShapeResults(text) {
  const bySegment = new Map();
  for (const line of text.split("\n")) {
    const trimmed = line.trim();
    if (!trimmed) continue;
    let event;
    try {
      event = JSON.parse(trimmed);
    } catch {
      continue;
    }
    if (!event || typeof event.Test !== "string" || !TERMINAL_ACTIONS.has(event.Action)) continue;
    const segments = event.Test.split("/");
    bySegment.set(segments[segments.length - 1], event.Action);
  }
  return bySegment;
}

/**
 * The per-shape observation input classifyRevisionBinding expects, derived
 * from a parseGoTestJSONShapeResults() map. `selectedCount`/`ranCount` are
 * scoped to THIS ONE shape (1 when its subtest appears at all, 0
 * otherwise) — deliberately not the whole file's subtest count, so a shape
 * a `-run` filter excluded reports "selected_not_run" for ITSELF even
 * though sibling shapes in the same file did run. `skip` (t.Skip — never
 * used by this repo's own property tests, invariant 6 forbids it, but
 * handled defensively) maps to a non-real result, which
 * classifyRevisionBinding folds into "unavailable" via its final
 * malformed-result check.
 */
export function propertyShapeObservation(bySegment, shapeID) {
  const action = bySegment.get(shapeID);
  const seen = action !== undefined;
  return {
    selectedCount: seen ? 1 : 0,
    ranCount: seen ? 1 : 0,
    result: action === "pass" || action === "fail" ? action : "error",
  };
}

// --- Compat harness CaseSet parsing (compatibility/internal/score) ---------
//
// compatibility/internal/score.CaseSet ({head, cases:[{id,passed}]}) is the
// one report shape all three compat drivers (tempo diff.go + grpc_diff.go,
// loki-compliance-tester, the prometheus scorer) already funnel through —
// see cases.go's own WriteCases. A semantic binding for a compat verifier
// is scoped to the WHOLE driver invocation (e.g.
// BINDING-TRACEQL-TRANSPORT-ARM-HTTP's test_ref names diff.go itself, not
// one case ID), so the observation this module derives is an AGGREGATE
// over every case in the set: selectedCount is the case count, and result
// is "pass" only when every case agreed — one regressed case is real
// non-evidence-of-full-agreement, exactly like a `go test` subtest failure
// failing its parent.

/**
 * Parses a compat-cases.json document (score.CaseSet's JSON shape) into
 * the observation input classifyRevisionBinding expects. Throws on a
 * structurally malformed document (missing head/cases) rather than
 * guessing — an unreadable report is never silently treated as zero cases,
 * since those are two different non-evidence reasons a caller must be able
 * to tell apart (a hard read/parse failure belongs to the CLI's own
 * unavailable-report handling, not to this classifier).
 */
export function parseCaseSet(raw) {
  if (!raw || typeof raw.head !== "string" || raw.head === "") {
    fail(["compat case set must have a non-empty string \"head\""]);
  }
  if (!Array.isArray(raw.cases)) {
    fail(["compat case set must have a \"cases\" array"]);
  }
  const selectedCount = raw.cases.length;
  const allPassed = raw.cases.every((c) => c && c.passed === true);
  return {
    head: raw.head,
    selectedCount,
    ranCount: selectedCount,
    result: selectedCount > 0 ? (allPassed ? "pass" : "fail") : "error",
  };
}

// --- Shared compat record composition (CLI MODE=compat + compat-execution-report.mjs) ---
//
// Both callers classify ONE binding against ONE already-parsed CaseSet and
// render it as an executions.json-shaped record — the same
// classifyRevisionBinding + toExecutionRecord composition, with the same
// literal aggregateNoOp: false (a compat CaseSet, unlike a go-test-json
// stream, never represents a short-circuited no-op run — see parseCaseSet's
// own header) and substrate: "reference-stack". Extracted so the two
// callers (semantic-execution-adapter.mjs's runCompat and
// compat-execution-report.mjs's per-binding fan-out) can never drift apart
// on this composition again (cerberus issue #3510).

/**
 * Classifies `binding` against `caseSet` for `candidate` and renders the
 * executions.json-shaped record. `candidate` is classifyRevisionBinding's
 * own candidate shape ({sourceSha, referenceVersion?}); `run` is
 * githubRunContext()'s shape (runId/runAttempt/job/event).
 */
export function compatExecutionRecord({
  binding,
  candidate,
  caseSet,
  observedAt,
  runRef,
  run,
  referenceVersion = null,
  datasetFingerprint = null,
}) {
  const classification = classifyRevisionBinding(candidate, {
    sourceSha: candidate.sourceSha,
    referenceVersion,
    datasetFingerprint,
    selectedCount: caseSet.selectedCount,
    ranCount: caseSet.ranCount,
    aggregateNoOp: false,
    result: caseSet.result,
  });
  return toExecutionRecord({
    id: execIdFor(binding.id, observedAt),
    binding: binding.id,
    observedAt,
    runRef,
    classification,
    context: {
      sourceSha: candidate.sourceSha,
      runId: run.runId,
      runAttempt: run.runAttempt,
      job: run.job,
      event: run.event,
      substrate: "reference-stack",
      referenceVersion,
      datasetFingerprint,
    },
  });
}
