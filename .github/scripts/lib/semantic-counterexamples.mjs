// semantic-counterexamples.mjs — loader and validator for
// test/semantic/counterexamples/*.json (cerberus issue #3445).
//
// The six files lib/semantic-model.mjs owns (heads/capabilities/contracts/
// verifiers/bindings/executions) record what a head OWES its users and what
// evidence currently backs that promise. Nothing in that model joins a
// specific, already-fixed bug to the contract it violated, how the bug was
// originally FOUND, and which test would catch it coming back — that triple
// lives today only in a closed GitHub issue and a scattered regression test,
// never in one record. A COUNTEREXAMPLE record is that join, for a bounded,
// hand-curated cohort of real historical bugs — never a bulk mining of GitHub
// history, and never a re-derivation of a bug's minimized input (the input
// lives in the fixture/test the record POINTS TO, not copied into the JSON).
//
// A counterexample record is NOT a seventh kind of evidence for
// computeAssurance() in lib/semantic-model.mjs, and this module never touches
// that computation. Pointing a `contracts[].related_bindings` entry at an
// existing, already-active BINDING-* only cites standing evidence that
// already counts; it is not new evidence conjured by giving the bug an ID
// (see the acceptance criterion this guards: a record must not be countable
// as independent oracle evidence merely by existing).
//
// Provenance discipline: `seed_provenance` distinguishes three genuinely
// different situations a historical bug's original repro can be in today —
// "verified" (the cited locator/replay files are real, current, and
// reproduce the discriminating case as described), "reconstructed" (the
// ORIGINAL repro — e.g. a Rapid failfile under test/property/testdata/rapid/
// — has since expired/rotated out of the tree, so this record binds to a
// permanent regression test that reproduces the same minimal shape rather
// than the original seed itself), and "not-replayed" (no committed artifact
// reproduces the original discovery method at all; only a narrower permanent
// guard exists). A record never claims "verified" for a repro it cannot
// point at on disk.
//
// `mutation_class` is this module's own small taxonomy of "what shape of
// code defect this bug was", not gremlins' own eleven mutator operator IDs
// (`.gremlins.yaml`) — none of gremlins' arithmetic/conditional/bitwise/
// loop-control operators models a call-omission, a wrong sentinel constant,
// or two independently-declared config values silently disagreeing, which is
// what these three seed bugs actually were, and two of the three root causes
// (a property-test generator, a spec-lane oracle) sit in test-only code
// gremlins' phased rollout never mutates in the first place (see each
// record's own `root_cause_subsystem`). Forcing a gremlins operator ID onto a
// bug it cannot express would be a false, uncheckable claim, not evidence.
//
// Env:
//   SEMANTIC_COUNTEREXAMPLES_DIR   directory holding the *.json records
//                                   (default test/semantic/counterexamples)
//
// Node builtins only. Throws SemanticModelError (imported, not
// re-implemented) on any schema/reference/provenance violation, tagged the
// same way lib/semantic-model.mjs tags its own problems.

import { existsSync, readdirSync } from "node:fs";
import { join, resolve } from "node:path";

import {
  BINDING_ID_RE,
  CONTRACT_ID_RE,
  EVIDENCE_CLASSES,
  OBSERVED_AT_RE,
  SemanticModelError,
  enumValue,
  exactObject,
  fail,
  isObject,
  nullableStringValue,
  parseJSONFile,
  stringArray,
  stringValue,
} from "./semantic-model.mjs";

export const COUNTEREXAMPLE_SCHEMA_VERSION = 1;
export const DEFAULT_COUNTEREXAMPLES_DIR = "test/semantic/counterexamples";

const COUNTEREXAMPLE_ID_RE = /^CTREX-[0-9]+$/;
const GITHUB_ISSUE_URL_RE =
  /^https:\/\/github\.com\/tsouza\/cerberus\/issues\/([0-9]+)$/;
const GITHUB_PR_URL_RE =
  /^https:\/\/github\.com\/tsouza\/cerberus\/pull\/([0-9]+)$/;
const COMMIT_SHA_RE = /^[0-9a-f]{40}$/;

const SEED_PROVENANCES = new Set(["verified", "reconstructed", "not-replayed"]);

// This module's own taxonomy — see the file header for why it is not
// gremlins' operator vocabulary. Kept to exactly the shapes the current
// cohort instantiates; extending it for a future record is an ordinary,
// additive schema change, not a reason to force-fit an existing bug into a
// neighboring bucket.
const MUTATION_CLASSES = new Set([
  // A comparison's FULL-match/CONTAINS-match (or inclusive/exclusive,
  // before/after) boundary is drawn in the wrong place.
  "boundary-condition",
  // A wrong literal/sentinel value stands in for the correct one (e.g. a
  // marker byte string used where an empty value was the real convention).
  "constant-substitution",
  // Two configurations meant to represent the SAME thing (an engine flag,
  // an oracle default) are declared independently and silently disagree.
  "configuration-divergence",
]);

const COUNTEREXAMPLE_KEYS = new Set([
  "schema_version",
  "id",
  "source_issue",
  "source_issue_url",
  "title",
  "summary",
  "root_cause_subsystem",
  "fix_pr",
  "fix_pr_url",
  "fix_commit",
  "fix_merged_at",
  "discovery_evidence_class",
  "discovery_narrative",
  "seed_provenance",
  "seed_provenance_note",
  "mutation_class",
  "mutation_class_rationale",
  "contracts",
]);

const CONTRACT_ENTRY_KEYS = new Set([
  "contract_id",
  "related_bindings",
  "locator_path",
  "locator_detail",
  "replay_test_path",
  "replay_test_name",
  "replay_command",
]);

function positiveIntegerValue(value, path, problems) {
  if (!Number.isInteger(value) || value <= 0) {
    fail(problems, "schema", `${path} must be a positive integer; got ${JSON.stringify(value)}`);
    return false;
  }
  return true;
}

// A path field is real evidence only if it resolves on disk TODAY — the
// acceptance criterion this enforces ("no record depends solely on an
// expiring CI URL / missing artifacts are explicit") is exactly what makes a
// dangling `locator_path`/`replay_test_path` a validation failure rather than
// a silently stale string.
function existingPathValue(value, path, problems, { root }) {
  if (!stringValue(value, path, problems)) return false;
  if (!existsSync(resolve(root, value))) {
    fail(problems, "reference", `${path} does not exist on disk: ${value}`);
    return false;
  }
  return true;
}

function validateContractEntry(entry, at, model, problems, opts) {
  if (!exactObject(entry, CONTRACT_ENTRY_KEYS, at, problems)) return;
  let contractId = null;
  if (stringValue(entry.contract_id, `${at}.contract_id`, problems, { pattern: CONTRACT_ID_RE })) {
    contractId = entry.contract_id;
    if (!model.contracts.has(contractId)) {
      fail(problems, "reference", `${at}.contract_id references unknown contract ${contractId}`);
      contractId = null;
    }
  }
  const bindings = stringArray(entry.related_bindings, `${at}.related_bindings`, problems, {
    allowEmpty: false,
    pattern: BINDING_ID_RE,
  })
    ? entry.related_bindings
    : [];
  for (let i = 0; i < bindings.length; i += 1) {
    const bindingId = bindings[i];
    const binding = model.bindings.get(bindingId);
    if (!binding) {
      fail(problems, "reference", `${at}.related_bindings[${i}] references unknown binding ${bindingId}`);
      continue;
    }
    if (contractId !== null && binding.contract !== contractId) {
      fail(
        problems,
        "reference",
        `${at}.related_bindings[${i}] (${bindingId}) binds ${binding.contract}, not ${at}'s own contract_id ${contractId}`,
      );
    }
  }
  existingPathValue(entry.locator_path, `${at}.locator_path`, problems, opts);
  stringValue(entry.locator_detail, `${at}.locator_detail`, problems);
  existingPathValue(entry.replay_test_path, `${at}.replay_test_path`, problems, opts);
  stringValue(entry.replay_test_name, `${at}.replay_test_name`, problems);
  stringValue(entry.replay_command, `${at}.replay_command`, problems);
}

// Cross-checks a URL field's embedded number against the record's own
// integer field, so the two can never quietly drift apart (the same failure
// class as a PR title that no longer matches its squash-merged subject).
function checkUrlNumberAgrees(urlValue, urlPath, pattern, expectedNumber, expectedPath, problems) {
  const match = typeof urlValue === "string" ? urlValue.match(pattern) : null;
  if (match && Number(match[1]) !== expectedNumber) {
    fail(
      problems,
      "schema",
      `${urlPath} names #${match[1]}, which disagrees with ${expectedPath} (${expectedNumber})`,
    );
  }
}

// Validates one already-parsed counterexample record against the already-
// validated semantic model (`model.contracts`, `model.bindings`). `opts.root`
// is the directory `locator_path`/`replay_test_path` resolve against
// (default process.cwd()).
export function validateCounterexampleRecord(raw, at, model, problems, opts = {}) {
  const root = opts.root ?? process.cwd();
  if (!exactObject(raw, COUNTEREXAMPLE_KEYS, at, problems)) return null;
  if (raw.schema_version !== COUNTEREXAMPLE_SCHEMA_VERSION) {
    fail(
      problems,
      "schema",
      `${at}.schema_version must be ${COUNTEREXAMPLE_SCHEMA_VERSION}; got ${JSON.stringify(raw.schema_version)}`,
    );
  }
  let id = null;
  if (stringValue(raw.id, `${at}.id`, problems, { pattern: COUNTEREXAMPLE_ID_RE })) id = raw.id;

  if (positiveIntegerValue(raw.source_issue, `${at}.source_issue`, problems)) {
    checkUrlNumberAgrees(
      raw.source_issue_url,
      `${at}.source_issue_url`,
      GITHUB_ISSUE_URL_RE,
      raw.source_issue,
      `${at}.source_issue`,
      problems,
    );
  }
  stringValue(raw.source_issue_url, `${at}.source_issue_url`, problems, { pattern: GITHUB_ISSUE_URL_RE });
  if (id !== null && raw.id !== `CTREX-${raw.source_issue}`) {
    fail(problems, "schema", `${at}.id must be CTREX-<source_issue>; got ${raw.id} for source_issue ${raw.source_issue}`);
  }

  stringValue(raw.title, `${at}.title`, problems);
  stringValue(raw.summary, `${at}.summary`, problems);
  stringValue(raw.root_cause_subsystem, `${at}.root_cause_subsystem`, problems);

  if (positiveIntegerValue(raw.fix_pr, `${at}.fix_pr`, problems)) {
    checkUrlNumberAgrees(
      raw.fix_pr_url,
      `${at}.fix_pr_url`,
      GITHUB_PR_URL_RE,
      raw.fix_pr,
      `${at}.fix_pr`,
      problems,
    );
  }
  stringValue(raw.fix_pr_url, `${at}.fix_pr_url`, problems, { pattern: GITHUB_PR_URL_RE });
  stringValue(raw.fix_commit, `${at}.fix_commit`, problems, { pattern: COMMIT_SHA_RE });
  stringValue(raw.fix_merged_at, `${at}.fix_merged_at`, problems, { pattern: OBSERVED_AT_RE });

  if (typeof raw.discovery_evidence_class === "string" && !EVIDENCE_CLASSES.has(raw.discovery_evidence_class)) {
    fail(
      problems,
      "schema",
      `${at}.discovery_evidence_class must be one of ${[...EVIDENCE_CLASSES].join(", ")}; got ${JSON.stringify(raw.discovery_evidence_class)}`,
    );
  } else {
    stringValue(raw.discovery_evidence_class, `${at}.discovery_evidence_class`, problems);
  }
  stringValue(raw.discovery_narrative, `${at}.discovery_narrative`, problems);

  enumValue(raw.seed_provenance, SEED_PROVENANCES, `${at}.seed_provenance`, problems);
  if (raw.seed_provenance === "verified") {
    if (raw.seed_provenance_note !== null) {
      fail(problems, "schema", `${at}.seed_provenance_note must be null when seed_provenance is "verified"`);
    }
  } else if (raw.seed_provenance !== undefined) {
    nullableStringValue(raw.seed_provenance_note, `${at}.seed_provenance_note`, problems);
    if (raw.seed_provenance_note === null) {
      fail(problems, "schema", `${at}.seed_provenance_note is required (non-null) unless seed_provenance is "verified"`);
    }
  }

  enumValue(raw.mutation_class, MUTATION_CLASSES, `${at}.mutation_class`, problems);
  stringValue(raw.mutation_class_rationale, `${at}.mutation_class_rationale`, problems);

  if (!Array.isArray(raw.contracts)) {
    fail(problems, "schema", `${at}.contracts must be an array`);
  } else if (raw.contracts.length === 0) {
    fail(problems, "schema", `${at}.contracts must not be empty`);
  } else {
    const seenContractIds = new Set();
    for (let i = 0; i < raw.contracts.length; i += 1) {
      const entryAt = `${at}.contracts[${i}]`;
      validateContractEntry(raw.contracts[i], entryAt, model, problems, { root });
      const cid = isObject(raw.contracts[i]) ? raw.contracts[i].contract_id : undefined;
      if (typeof cid === "string") {
        if (seenContractIds.has(cid)) fail(problems, "schema", `${entryAt}.contract_id is a duplicate within this record: ${cid}`);
        seenContractIds.add(cid);
      }
    }
  }

  return id;
}

// Loads every *.json file directly under `dir` (default
// test/semantic/counterexamples, resolved against `root`) as ONE
// counterexample record each — a glob of small, independently reviewable
// files, unlike the six single-file catalogs lib/semantic-model.mjs owns —
// and validates each against the already-validated `model`. Returns a Map
// keyed by record id. Throws SemanticModelError on any problem, across every
// file (not fail-fast on the first bad file), so a CI run reports the whole
// picture in one pass.
export function loadCounterexamples(model, dir = DEFAULT_COUNTEREXAMPLES_DIR, { root = process.cwd() } = {}) {
  const base = resolve(root, dir);
  const problems = [];
  const records = new Map();

  let filenames;
  try {
    filenames = readdirSync(base).filter((name) => name.endsWith(".json")).sort();
  } catch (error) {
    throw new SemanticModelError("invalid semantic counterexample records", [
      `[schema] cannot read ${base}: ${error.message}`,
    ]);
  }

  for (const filename of filenames) {
    const path = join(base, filename);
    const raw = parseJSONFile(path, filename);
    const id = validateCounterexampleRecord(raw, filename, model, problems, { root });
    if (id === null) continue;
    if (records.has(id)) {
      fail(problems, "schema", `${filename}: id is a duplicate across counterexample records: ${id}`);
      continue;
    }
    records.set(id, { ...raw, at: filename });
  }

  if (problems.length > 0) {
    throw new SemanticModelError("invalid semantic counterexample records", problems);
  }

  return records;
}

export function renderCounterexamplesSummary(records) {
  const bySeedProvenance = { verified: 0, reconstructed: 0, "not-replayed": 0 };
  for (const [, record] of records) bySeedProvenance[record.seed_provenance] += 1;
  return [
    `- counterexamples: **${records.size}** (verified: ${bySeedProvenance.verified}, reconstructed: ${bySeedProvenance.reconstructed}, not-replayed: ${bySeedProvenance["not-replayed"]})`,
  ].join("\n");
}
