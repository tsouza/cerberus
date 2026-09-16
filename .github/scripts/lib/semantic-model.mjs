// semantic-model.mjs — loader and validator for Cerberus's semantic contract
// metadata model (test/semantic/). Node builtins only; no YAML parsing, no
// production Go dependency (see cerberus issue #3426 and epic #3421).
//
// The model separates four kinds of record that today get conflated in
// ad-hoc comments and test names: a semantic CONTRACT (an obligation a head
// owes its users), a VERIFIER (a technique that can produce evidence for a
// contract, with its own detection blind spots), a BINDING (a concrete test
// that offers a verifier's evidence for one contract), and an EXECUTION (an
// observed run of a binding). Two more record kinds anchor the whole thing:
// HEAD (the three peer query languages) and CAP, a capability owned by one
// or more heads.
//
// Every record carries a stable ID prefixed by its scope:
//   HEAD-*     one of the three peer query heads (closed set, never grows)
//   CAP-*      a capability, explicitly attached to the heads that own it
//   PROMQL-*   a contract scoped to exactly the PromQL head
//   LOGQL-*    a contract scoped to exactly the LogQL head
//   TRACEQL-*  a contract scoped to exactly the TraceQL head
//   SIGNAL-*   a contract scoped to a signal concept spanning >=1 heads
//   ARCH-*     a contract scoped to the shared architecture (always universal)
// VERIFIER-*, BINDING-* and EXEC-* are record-kind prefixes for the three
// record kinds that carry no applicability scope of their own — they inherit
// scope transitively through the contract they reference.
//
// IDs are never recycled: a record that stops applying is marked
// `status: "superseded"` with `replaced_by` naming its successor (and the
// successor names it back via `replaces`), not deleted. Assurance is
// deliberately a SET of required evidence classes plus independence groups,
// not a numeric score — `computeAssurance()` returns which contracts are
// fully assured, which are legitimately excluded (draft / superseded /
// explicit_deficit), and which are active but incomplete (a schema error,
// not a quiet gap).
//
// Env (semantic-model.mjs, the CLI wrapper, reads these — this module takes
// plain arguments and touches no env itself):
//   SEMANTIC_MODEL_DIR   directory holding the six JSON files (default
//                         test/semantic)
//
// Every exported validation entry point throws SemanticModelError, whose
// `.problems` is an array of strings each tagged with its failure class in
// a leading `[tag]`: `[schema]` for structural/vocabulary defects,
// `[reference]` for a dangling or invalid cross-reference, `[cycle]` for a
// cyclic inheritance or replacement chain, `[assurance]` for a status that
// is active/explicit_deficit but whose required evidence is missing or
// malformed. The tag is what lets a caller (or a test) tell "malformed
// metadata" apart from "missing required evidence" per issue #3426's own
// acceptance criteria — they are different failure classes even though both
// fail the same command.

import { readFileSync } from "node:fs";
import { join, resolve } from "node:path";

// Bumped 1 -> 2 by issue #3459: executions.json's EXECUTION_KEYS gained ten
// new revision-binding fields (source_sha and friends, see their own
// comment below). exactObject() requires every declared key on every
// record — optionality here is expressed as an explicit `null` value,
// never by omitting the key (see exactObject's own comment) — so every
// existing execution record needed the ten keys added (nulled out except
// `selection: "executed"`, since every one of them already recorded a
// real observed pass). The other five documents carry no new keys; their
// schema_version bumped in lock-step only because loadSemanticModel reads
// one shared constant, not because their own shape changed.
export const SEMANTIC_MODEL_SCHEMA_VERSION = 2;
export const DEFAULT_SEMANTIC_MODEL_DIR = "test/semantic";

// The three peer heads are a closed set: the whole point of this model is
// that a head can never silently disappear from the catalog (issue #3426
// acceptance criterion "negative tests cover removal of a head"). Adding a
// fourth head is a deliberate, larger change than this validator owns.
export const CANONICAL_HEAD_IDS = Object.freeze([
  "HEAD-LOGQL",
  "HEAD-PROMQL",
  "HEAD-TRACEQL",
]);

const HEAD_SCOPE_PREFIXES = Object.freeze({
  PROMQL: "HEAD-PROMQL",
  LOGQL: "HEAD-LOGQL",
  TRACEQL: "HEAD-TRACEQL",
});

const CONTRACT_SCOPES = Object.freeze({
  PROMQL: "head",
  LOGQL: "head",
  TRACEQL: "head",
  SIGNAL: "signal",
  ARCH: "architecture",
});

const HEAD_ID_RE = /^HEAD-[A-Z][A-Z0-9]*$/;
const CAP_ID_RE = /^CAP-[A-Z][A-Z0-9]*(?:-[A-Z0-9]+)*$/;
const CONTRACT_ID_RE =
  /^(PROMQL|LOGQL|TRACEQL|SIGNAL|ARCH)-[A-Z][A-Z0-9]*(?:-[A-Z0-9]+)*$/;
const VERIFIER_ID_RE = /^VERIFIER-[A-Z][A-Z0-9]*(?:-[A-Z0-9]+)*$/;
const BINDING_ID_RE = /^BINDING-[A-Z][A-Z0-9]*(?:-[A-Z0-9]+)*$/;
const EXEC_ID_RE = /^EXEC-[A-Z][A-Z0-9]*(?:-[A-Z0-9]+)*$/;
const OWNER_RE = /^[a-z0-9][a-z0-9-]*$/;
const OBSERVED_AT_RE =
  /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z$/;

const CATALOG_STATUSES = new Set(["draft", "active", "superseded"]);
const CONTRACT_STATUSES = new Set([
  "draft",
  "active",
  "superseded",
  "explicit_deficit",
]);
const BINDING_STATUSES = new Set(["draft", "active"]);
const EVIDENCE_CLASSES = new Set([
  "execution",
  "property",
  "reference",
  "static-analysis",
  "manual-review",
]);
const AUTHORITIES = new Set([
  "specification",
  "reference-implementation",
  "design-decision",
  "operational-invariant",
]);
const VERIFIER_SUBSTRATES = new Set([
  "chdb",
  "real-clickhouse",
  "reference-stack",
  "static",
  "runner",
]);
const RELATIVE_COSTS = new Set(["low", "medium", "high"]);
const EXECUTION_RESULTS = new Set(["pass", "fail", "error"]);

// Revision-binding fields (cerberus issue #3459). An execution record's
// core five fields (id/binding/observed_at/result/run_ref) say THAT a
// verifier produced SOME verdict SOMETIME; they say nothing about WHICH
// candidate revision it was produced against, so a stale or mismatched
// report can silently masquerade as fresh evidence for today's commit. The
// fields below close that gap — every one of them nullable, since a
// pre-#3459 or hand-authored record legitimately cannot state what an
// automated adapter would have captured live.
//
//   source_sha           the commit the verifier actually ran against
//   run_id/run_attempt/job  GitHub Actions run identity, together
//                         sufficient to tell two RERUNS of the same
//                         workflow apart (a bare source_sha cannot: a
//                         re-triggered rerun keeps the same commit)
//   event                the triggering GitHub event (push/pull_request/…)
//   substrate             which execution path actually ran (reuses the
//                         verifier substrate vocabulary, so a chdb-only
//                         run can never silently stand in for a
//                         reference-stack claim)
//   reference_version     the reference backend's version/build, when the
//                         verifier's authority is reference-implementation
//   dataset_fingerprint   a content hash of the corpus/dataset actually
//                         exercised — lets a later reader detect the
//                         corpus has since changed under the pinned record
//   selection             classifies whether this record is real executed
//                         evidence at all: "executed" (the normal case —
//                         result carries a real pass/fail/error verdict)
//                         or one of four NON-EVIDENCE classes an adapter
//                         emits instead of silently omitting the
//                         observation: "selected_not_run" (the test
//                         selection matched zero cases), "no_op" (the
//                         job itself short-circuited, e.g. a non-release
//                         PR's no-op branch), "stale" (a dataset/reference
//                         mismatch against the candidate this record
//                         claims to be about), "unavailable" (the report
//                         this record would summarize could not be
//                         produced or resolved at all — including a
//                         source_sha mismatch). A non-"executed" selection
//                         must carry result "error" (schema-enforced
//                         below) and a non-null selection_reason — the
//                         existing classifyExecutionObservation /
//                         classifyObservedEvidence rule (only "pass"/"fail"
//                         count as observed evidence) therefore already
//                         treats every one of these four classes as
//                         non-evidence with ZERO changes to that rule.
// Exported (not just module-local) so lib/semantic-execution-adapter.mjs —
// the producer of these fields — validates against the SAME vocabulary the
// loader enforces, rather than a second hand-copied list that could drift.
export const EXECUTION_EVENTS = Object.freeze([
  "push",
  "pull_request",
  "merge_group",
  "schedule",
  "workflow_dispatch",
]);
export const EXECUTION_SELECTIONS = Object.freeze([
  "executed",
  "selected_not_run",
  "no_op",
  "stale",
  "unavailable",
]);
export const SOURCE_SHA_PATTERN = /^[0-9a-f]{7,40}$/;
export const RUN_IDENTITY_PATTERN = /^\d+$/;
export const DATASET_FINGERPRINT_PATTERN = /^[0-9a-f]{64}$/;

const EXECUTION_EVENTS_SET = new Set(EXECUTION_EVENTS);
const EXECUTION_SELECTIONS_SET = new Set(EXECUTION_SELECTIONS);

export class SemanticModelError extends Error {
  constructor(label, problems) {
    super(`${label}:\n${problems.map((p) => `- ${p}`).join("\n")}`);
    this.name = "SemanticModelError";
    this.problems = problems;
  }
}

function isObject(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function fail(problems, tag, message) {
  problems.push(`[${tag}] ${message}`);
}

// Every declared key is mandatory (absence is a schema defect, not an
// optional field) and no undeclared key is tolerated — the same discipline
// `ci-lane-contract.mjs` applies to `.github/ci-lanes.json`. Optionality is
// expressed inside the value (an explicit `null`, or an explicit `[]`), never
// by omitting the key, because omission is exactly the "inferred, not
// explicit" shape acceptance criterion #2 forbids for capability membership.
function exactObject(value, keys, path, problems) {
  if (!isObject(value)) {
    fail(problems, "schema", `${path} must be an object`);
    return false;
  }
  for (const key of keys) {
    if (!Object.hasOwn(value, key))
      fail(problems, "schema", `${path}.${key} is required`);
  }
  for (const key of Object.keys(value)) {
    if (!keys.has(key)) fail(problems, "schema", `${path}.${key} is unknown`);
  }
  return true;
}

function stringValue(value, path, problems, { pattern, allowEmpty = false } = {}) {
  if (typeof value !== "string" || (!allowEmpty && value.trim() === "")) {
    fail(problems, "schema", `${path} must be a non-empty string`);
    return false;
  }
  if (pattern && !pattern.test(value)) {
    fail(problems, "schema", `${path} has invalid value ${JSON.stringify(value)}`);
    return false;
  }
  return true;
}

function nullableStringValue(value, path, problems, opts = {}) {
  if (value === null) return true;
  return stringValue(value, path, problems, opts);
}

function enumValue(value, allowed, path, problems) {
  if (!allowed.has(value)) {
    fail(
      problems,
      "schema",
      `${path} must be one of ${[...allowed].join(", ")}; got ${JSON.stringify(value)}`,
    );
    return false;
  }
  return true;
}

function nullableEnumValue(value, allowed, path, problems) {
  if (value === null) return true;
  return enumValue(value, allowed, path, problems);
}

function stringArray(value, path, problems, { allowEmpty = true, pattern } = {}) {
  if (!Array.isArray(value)) {
    fail(problems, "schema", `${path} must be an array`);
    return false;
  }
  if (!allowEmpty && value.length === 0)
    fail(problems, "schema", `${path} must not be empty`);
  const seen = new Set();
  for (let i = 0; i < value.length; i += 1) {
    if (!stringValue(value[i], `${path}[${i}]`, problems, { pattern })) continue;
    if (seen.has(value[i]))
      fail(problems, "schema", `${path} contains duplicate ${JSON.stringify(value[i])}`);
    seen.add(value[i]);
  }
  return true;
}

// applicable_heads is either the literal string "universal" (expands equally
// to every canonical head) or an explicit array of head IDs. Nothing here
// ever infers head membership from an ID's own scope prefix or from syntax —
// acceptance criterion #2 requires it stated, every time.
function expandApplicableHeads(value, path, problems) {
  if (value === "universal") return new Set(CANONICAL_HEAD_IDS);
  if (!Array.isArray(value)) {
    fail(
      problems,
      "schema",
      `${path} must be "universal" or an array of head IDs`,
    );
    return new Set();
  }
  if (value.length === 0) {
    fail(problems, "schema", `${path} must not be an empty array`);
    return new Set();
  }
  const heads = new Set();
  for (let i = 0; i < value.length; i += 1) {
    const head = value[i];
    if (!stringValue(head, `${path}[${i}]`, problems)) continue;
    if (!CANONICAL_HEAD_IDS.includes(head)) {
      fail(problems, "reference", `${path}[${i}] is not a canonical head: ${head}`);
      continue;
    }
    if (heads.has(head))
      fail(problems, "schema", `${path} contains duplicate ${head}`);
    heads.add(head);
  }
  return heads;
}

function parseJSONFile(path, label) {
  let body;
  try {
    body = readFileSync(path, "utf8");
  } catch (error) {
    throw new SemanticModelError(label, [`[schema] cannot read ${path}: ${error.message}`]);
  }
  try {
    return JSON.parse(body);
  } catch (error) {
    throw new SemanticModelError(label, [`[schema] ${path} is not valid JSON: ${error.message}`]);
  }
}

// --- Per-file loaders -------------------------------------------------

function requireDocument(raw, kindKey, label, problems) {
  if (!isObject(raw)) {
    fail(problems, "schema", `${label} must be a JSON object`);
    return [];
  }
  if (raw.schema_version !== SEMANTIC_MODEL_SCHEMA_VERSION) {
    fail(
      problems,
      "schema",
      `${label}.schema_version must be ${SEMANTIC_MODEL_SCHEMA_VERSION}; got ${JSON.stringify(raw.schema_version)}`,
    );
  }
  if (!Array.isArray(raw[kindKey])) {
    fail(problems, "schema", `${label}.${kindKey} must be an array`);
    return [];
  }
  const extraKeys = Object.keys(raw).filter(
    (key) => key !== "schema_version" && key !== kindKey,
  );
  for (const key of extraKeys)
    fail(problems, "schema", `${label}.${key} is unknown`);
  return raw[kindKey];
}

const HEAD_KEYS = new Set([
  "id",
  "name",
  "description",
  "status",
  "owner",
  "replaces",
  "replaced_by",
]);

function validateHeads(raw, problems) {
  const records = requireDocument(raw, "heads", "heads.json", problems);
  const heads = new Map();
  for (let i = 0; i < records.length; i += 1) {
    const at = `heads.json.heads[${i}]`;
    const record = records[i];
    if (!exactObject(record, HEAD_KEYS, at, problems)) continue;
    if (!stringValue(record.id, `${at}.id`, problems, { pattern: HEAD_ID_RE })) continue;
    stringValue(record.name, `${at}.name`, problems);
    stringValue(record.description, `${at}.description`, problems);
    enumValue(record.status, CATALOG_STATUSES, `${at}.status`, problems);
    stringValue(record.owner, `${at}.owner`, problems, { pattern: OWNER_RE });
    nullableStringValue(record.replaces, `${at}.replaces`, problems, { pattern: HEAD_ID_RE });
    nullableStringValue(record.replaced_by, `${at}.replaced_by`, problems, { pattern: HEAD_ID_RE });
    if (heads.has(record.id))
      fail(problems, "schema", `${at}.id is a duplicate: ${record.id}`);
    heads.set(record.id, { ...record, kind: "head", at });
  }
  const actualIds = [...heads.keys()].sort();
  const expectedIds = [...CANONICAL_HEAD_IDS].sort();
  if (JSON.stringify(actualIds) !== JSON.stringify(expectedIds)) {
    fail(
      problems,
      "reference",
      `heads.json must declare exactly the canonical heads ${expectedIds.join(", ")}; got ${actualIds.join(", ") || "(none)"}`,
    );
  }
  return heads;
}

const CAP_KEYS = new Set([
  "id",
  "name",
  "description",
  "status",
  "applicable_heads",
  "owner",
  "replaces",
  "replaced_by",
]);

function validateCapabilities(raw, problems) {
  const records = requireDocument(raw, "capabilities", "capabilities.json", problems);
  const caps = new Map();
  for (let i = 0; i < records.length; i += 1) {
    const at = `capabilities.json.capabilities[${i}]`;
    const record = records[i];
    if (!exactObject(record, CAP_KEYS, at, problems)) continue;
    if (!stringValue(record.id, `${at}.id`, problems, { pattern: CAP_ID_RE })) continue;
    stringValue(record.name, `${at}.name`, problems);
    stringValue(record.description, `${at}.description`, problems);
    enumValue(record.status, CATALOG_STATUSES, `${at}.status`, problems);
    const applicableHeads = expandApplicableHeads(
      record.applicable_heads,
      `${at}.applicable_heads`,
      problems,
    );
    stringValue(record.owner, `${at}.owner`, problems, { pattern: OWNER_RE });
    nullableStringValue(record.replaces, `${at}.replaces`, problems, { pattern: CAP_ID_RE });
    nullableStringValue(record.replaced_by, `${at}.replaced_by`, problems, { pattern: CAP_ID_RE });
    if (caps.has(record.id))
      fail(problems, "schema", `${at}.id is a duplicate: ${record.id}`);
    caps.set(record.id, { ...record, applicableHeads, kind: "capability", at });
  }
  return caps;
}

const CONTRACT_KEYS = new Set([
  "id",
  "statement",
  "scope",
  "applicable_heads",
  "applicable_capabilities",
  "authority",
  "required_evidence_classes",
  "required_independence_groups",
  "blind_spots",
  "related_contracts",
  "inherits_from",
  "status",
  "deficit_reason",
  "owner",
  "replaces",
  "replaced_by",
]);

function validateContracts(raw, capabilityIds, problems) {
  const records = requireDocument(raw, "contracts", "contracts.json", problems);
  const contracts = new Map();
  for (let i = 0; i < records.length; i += 1) {
    const at = `contracts.json.contracts[${i}]`;
    const record = records[i];
    if (!exactObject(record, CONTRACT_KEYS, at, problems)) continue;
    if (!stringValue(record.id, `${at}.id`, problems, { pattern: CONTRACT_ID_RE })) continue;
    const prefix = record.id.split("-")[0];
    const expectedScope = CONTRACT_SCOPES[prefix];
    stringValue(record.statement, `${at}.statement`, problems);
    if (!enumValue(record.scope, new Set(Object.values(CONTRACT_SCOPES)), `${at}.scope`, problems)) {
      // fall through — still validated below against the prefix
    } else if (expectedScope && record.scope !== expectedScope) {
      fail(
        problems,
        "schema",
        `${at}.scope must be "${expectedScope}" for a ${prefix}-scoped ID; got "${record.scope}"`,
      );
    }
    const applicableHeads = expandApplicableHeads(
      record.applicable_heads,
      `${at}.applicable_heads`,
      problems,
    );
    if (prefix in HEAD_SCOPE_PREFIXES) {
      const wantOnly = HEAD_SCOPE_PREFIXES[prefix];
      if (applicableHeads.size !== 1 || !applicableHeads.has(wantOnly)) {
        fail(
          problems,
          "schema",
          `${at}.applicable_heads must be exactly ["${wantOnly}"] for a ${prefix}-scoped contract`,
        );
      }
    } else if (prefix === "ARCH" && record.applicable_heads !== "universal") {
      fail(problems, "schema", `${at}.applicable_heads must be "universal" for an ARCH-scoped contract`);
    }
    stringArray(record.applicable_capabilities, `${at}.applicable_capabilities`, problems, {
      allowEmpty: true,
      pattern: CAP_ID_RE,
    });
    for (const cap of Array.isArray(record.applicable_capabilities) ? record.applicable_capabilities : []) {
      if (typeof cap === "string" && !capabilityIds.has(cap))
        fail(problems, "reference", `${at}.applicable_capabilities references unknown capability ${cap}`);
    }
    enumValue(record.authority, AUTHORITIES, `${at}.authority`, problems);
    stringArray(record.required_evidence_classes, `${at}.required_evidence_classes`, problems, {
      allowEmpty: false,
    });
    for (const [j, cls] of (record.required_evidence_classes ?? []).entries()) {
      if (typeof cls === "string" && !EVIDENCE_CLASSES.has(cls))
        fail(
          problems,
          "schema",
          `${at}.required_evidence_classes[${j}] must be one of ${[...EVIDENCE_CLASSES].join(", ")}; got ${JSON.stringify(cls)}`,
        );
    }
    stringArray(record.required_independence_groups, `${at}.required_independence_groups`, problems, {
      allowEmpty: true,
    });
    stringArray(record.blind_spots, `${at}.blind_spots`, problems, { allowEmpty: true });
    stringArray(record.related_contracts, `${at}.related_contracts`, problems, {
      allowEmpty: true,
      pattern: CONTRACT_ID_RE,
    });
    nullableStringValue(record.inherits_from, `${at}.inherits_from`, problems, { pattern: CONTRACT_ID_RE });
    if (record.inherits_from === record.id)
      fail(problems, "reference", `${at}.inherits_from cannot reference itself`);
    enumValue(record.status, CONTRACT_STATUSES, `${at}.status`, problems);
    if (record.status === "explicit_deficit") {
      stringValue(record.deficit_reason, `${at}.deficit_reason`, problems);
    } else if (record.deficit_reason !== null) {
      fail(
        problems,
        "schema",
        `${at}.deficit_reason must be null unless status is explicit_deficit`,
      );
    }
    stringValue(record.owner, `${at}.owner`, problems, { pattern: OWNER_RE });
    nullableStringValue(record.replaces, `${at}.replaces`, problems, { pattern: CONTRACT_ID_RE });
    nullableStringValue(record.replaced_by, `${at}.replaced_by`, problems, { pattern: CONTRACT_ID_RE });
    if (record.status === "superseded" && record.replaced_by === null) {
      fail(problems, "schema", `${at}: a superseded contract must set replaced_by`);
    }
    if (contracts.has(record.id))
      fail(problems, "schema", `${at}.id is a duplicate: ${record.id}`);
    contracts.set(record.id, { ...record, applicableHeads, kind: "contract", at });
  }
  return contracts;
}

const VERIFIER_KEYS = new Set([
  "id",
  "name",
  "description",
  "detects",
  "cannot_detect",
  "complemented_by",
  "substrate",
  "relative_cost",
  "status",
  "replaces",
  "replaced_by",
]);

function validateVerifiers(raw, problems) {
  const records = requireDocument(raw, "verifiers", "verifiers.json", problems);
  const verifiers = new Map();
  for (let i = 0; i < records.length; i += 1) {
    const at = `verifiers.json.verifiers[${i}]`;
    const record = records[i];
    if (!exactObject(record, VERIFIER_KEYS, at, problems)) continue;
    if (!stringValue(record.id, `${at}.id`, problems, { pattern: VERIFIER_ID_RE })) continue;
    stringValue(record.name, `${at}.name`, problems);
    stringValue(record.description, `${at}.description`, problems);
    stringArray(record.detects, `${at}.detects`, problems, { allowEmpty: false });
    stringArray(record.cannot_detect, `${at}.cannot_detect`, problems, { allowEmpty: true });
    stringArray(record.complemented_by, `${at}.complemented_by`, problems, {
      allowEmpty: true,
      pattern: VERIFIER_ID_RE,
    });
    if (Array.isArray(record.complemented_by) && record.complemented_by.includes(record.id))
      fail(problems, "reference", `${at}.complemented_by cannot reference itself`);
    enumValue(record.substrate, VERIFIER_SUBSTRATES, `${at}.substrate`, problems);
    enumValue(record.relative_cost, RELATIVE_COSTS, `${at}.relative_cost`, problems);
    enumValue(record.status, CATALOG_STATUSES, `${at}.status`, problems);
    nullableStringValue(record.replaces, `${at}.replaces`, problems, { pattern: VERIFIER_ID_RE });
    nullableStringValue(record.replaced_by, `${at}.replaced_by`, problems, { pattern: VERIFIER_ID_RE });
    if (record.status === "superseded" && record.replaced_by === null) {
      fail(problems, "schema", `${at}: a superseded verifier must set replaced_by`);
    }
    if (verifiers.has(record.id))
      fail(problems, "schema", `${at}.id is a duplicate: ${record.id}`);
    verifiers.set(record.id, { ...record, kind: "verifier", at });
  }
  return verifiers;
}

const BINDING_KEYS = new Set([
  "id",
  "contract",
  "verifier",
  "evidence_class",
  "independence_group",
  "test_ref",
  "status",
]);

function validateBindings(raw, contractIds, verifierIds, problems) {
  const records = requireDocument(raw, "bindings", "bindings.json", problems);
  const bindings = new Map();
  for (let i = 0; i < records.length; i += 1) {
    const at = `bindings.json.bindings[${i}]`;
    const record = records[i];
    if (!exactObject(record, BINDING_KEYS, at, problems)) continue;
    if (!stringValue(record.id, `${at}.id`, problems, { pattern: BINDING_ID_RE })) continue;
    if (stringValue(record.contract, `${at}.contract`, problems, { pattern: CONTRACT_ID_RE })) {
      if (!contractIds.has(record.contract))
        fail(problems, "reference", `${at}.contract references unknown contract ${record.contract}`);
    }
    if (stringValue(record.verifier, `${at}.verifier`, problems, { pattern: VERIFIER_ID_RE })) {
      if (!verifierIds.has(record.verifier))
        fail(problems, "reference", `${at}.verifier references unknown verifier ${record.verifier}`);
    }
    if (
      typeof record.evidence_class === "string" &&
      !EVIDENCE_CLASSES.has(record.evidence_class)
    ) {
      fail(
        problems,
        "schema",
        `${at}.evidence_class must be one of ${[...EVIDENCE_CLASSES].join(", ")}; got ${JSON.stringify(record.evidence_class)}`,
      );
    } else {
      stringValue(record.evidence_class, `${at}.evidence_class`, problems);
    }
    stringValue(record.independence_group, `${at}.independence_group`, problems);
    stringValue(record.test_ref, `${at}.test_ref`, problems);
    enumValue(record.status, BINDING_STATUSES, `${at}.status`, problems);
    if (bindings.has(record.id))
      fail(problems, "schema", `${at}.id is a duplicate: ${record.id}`);
    bindings.set(record.id, { ...record, kind: "binding", at });
  }
  return bindings;
}

const EXECUTION_KEYS = new Set([
  "id",
  "binding",
  "observed_at",
  "result",
  "run_ref",
  "selection",
  "selection_reason",
  "source_sha",
  "run_id",
  "run_attempt",
  "job",
  "event",
  "substrate",
  "reference_version",
  "dataset_fingerprint",
]);

function validateExecutions(raw, bindingIds, problems) {
  const records = requireDocument(raw, "executions", "executions.json", problems);
  const executions = new Map();
  for (let i = 0; i < records.length; i += 1) {
    const at = `executions.json.executions[${i}]`;
    const record = records[i];
    if (!exactObject(record, EXECUTION_KEYS, at, problems)) continue;
    if (!stringValue(record.id, `${at}.id`, problems, { pattern: EXEC_ID_RE })) continue;
    if (stringValue(record.binding, `${at}.binding`, problems, { pattern: BINDING_ID_RE })) {
      if (!bindingIds.has(record.binding))
        fail(problems, "reference", `${at}.binding references unknown binding ${record.binding}`);
    }
    stringValue(record.observed_at, `${at}.observed_at`, problems, { pattern: OBSERVED_AT_RE });
    enumValue(record.result, EXECUTION_RESULTS, `${at}.result`, problems);
    stringValue(record.run_ref, `${at}.run_ref`, problems);

    const selectionOk = enumValue(record.selection, EXECUTION_SELECTIONS_SET, `${at}.selection`, problems);
    nullableStringValue(record.selection_reason, `${at}.selection_reason`, problems);
    // A non-"executed" selection is, by definition, NOT a real observed
    // pass/fail — result must say so explicitly (never silently disagree),
    // and selection_reason must say WHY, so a reader is never left to
    // guess between five distinct non-evidence causes (acceptance
    // criterion: wrong SHA/reference, stale corpus, zero selected, and
    // aggregate no-op all yield non-evidence, each nameable).
    if (selectionOk && record.selection !== "executed") {
      if (record.result !== "error") {
        fail(
          problems,
          "schema",
          `${at}.result must be "error" when selection is ${JSON.stringify(record.selection)} (a non-executed selection cannot claim a real pass/fail verdict)`,
        );
      }
      if (record.selection_reason === null) {
        fail(
          problems,
          "schema",
          `${at}.selection_reason is required (non-null) when selection is ${JSON.stringify(record.selection)}`,
        );
      }
    }

    nullableStringValue(record.source_sha, `${at}.source_sha`, problems, { pattern: SOURCE_SHA_PATTERN });
    nullableStringValue(record.run_id, `${at}.run_id`, problems, { pattern: RUN_IDENTITY_PATTERN });
    nullableStringValue(record.run_attempt, `${at}.run_attempt`, problems, { pattern: RUN_IDENTITY_PATTERN });
    nullableStringValue(record.job, `${at}.job`, problems);
    nullableEnumValue(record.event, EXECUTION_EVENTS_SET, `${at}.event`, problems);
    nullableEnumValue(record.substrate, VERIFIER_SUBSTRATES, `${at}.substrate`, problems);
    nullableStringValue(record.reference_version, `${at}.reference_version`, problems);
    nullableStringValue(record.dataset_fingerprint, `${at}.dataset_fingerprint`, problems, {
      pattern: DATASET_FINGERPRINT_PATTERN,
    });

    if (executions.has(record.id))
      fail(problems, "schema", `${at}.id is a duplicate: ${record.id}`);
    executions.set(record.id, { ...record, kind: "execution", at });
  }
  return executions;
}

// --- Cross-cutting checks ----------------------------------------------

// Duplicate-ID detection lives per-kind, inline in each validateXxx loop
// above (`if (heads.has(record.id)) fail(...)`, and its five siblings) —
// not in one global pass here. That is a deliberate, not an accidental,
// split: catching a duplicate requires seeing it before the second record
// overwrites the first in the kind's own Map, so the check has to run
// during that Map's own construction. A SEPARATE cross-kind uniqueness pass
// over the six finished Maps would only ever inspect their post-dedup
// contents — and because every kind's ID pattern (HEAD-/CAP-/PROMQL-/
// LOGQL-/TRACEQL-/SIGNAL-/ARCH-/VERIFIER-/BINDING-/EXEC-) is a disjoint
// prefix, no two kinds can ever produce the same ID string in the first
// place. A pass that can never fire is not a safety net, just unreachable
// code — so there isn't one.

// A `replaced_by` link is only real history if the record it names says
// `replaces` right back — otherwise one side of the story silently vanishes
// the moment either file is edited without the other.
function checkReplacementMutuality(map, kindLabel, problems) {
  for (const [id, record] of map) {
    if (record.replaced_by !== null) {
      const successor = map.get(record.replaced_by);
      if (!successor) {
        fail(problems, "reference", `${record.at}.replaced_by references unknown ${kindLabel} ${record.replaced_by}`);
      } else if (successor.replaces !== id) {
        fail(
          problems,
          "reference",
          `${record.at}.replaced_by names ${record.replaced_by}, but that ${kindLabel}'s replaces does not name it back`,
        );
      }
    }
    if (record.replaces !== null) {
      const predecessor = map.get(record.replaces);
      if (!predecessor) {
        fail(problems, "reference", `${record.at}.replaces references unknown ${kindLabel} ${record.replaces}`);
      } else if (predecessor.replaced_by !== id) {
        fail(
          problems,
          "reference",
          `${record.at}.replaces names ${record.replaces}, but that ${kindLabel}'s replaced_by does not name it back`,
        );
      }
    }
  }
}

// Detects a cycle in a directed edge relation given as `id -> Set(next ids)`.
// Returns the first cycle found, as an ordered array of IDs, or null.
function findCycle(edges) {
  const WHITE = 0;
  const GRAY = 1;
  const BLACK = 2;
  const color = new Map();
  const stack = [];

  function visit(id) {
    color.set(id, GRAY);
    stack.push(id);
    for (const next of edges.get(id) ?? []) {
      const state = color.get(next) ?? WHITE;
      if (state === WHITE) {
        const found = visit(next);
        if (found) return found;
      } else if (state === GRAY) {
        const start = stack.indexOf(next);
        return [...stack.slice(start), next];
      }
    }
    stack.pop();
    color.set(id, BLACK);
    return null;
  }

  for (const id of edges.keys()) {
    if ((color.get(id) ?? WHITE) === WHITE) {
      const found = visit(id);
      if (found) return found;
    }
  }
  return null;
}

function checkReplacementCycles(catalogs, problems) {
  const edges = new Map();
  for (const map of catalogs) {
    for (const [id, record] of map) {
      if (!edges.has(id)) edges.set(id, new Set());
      if (record.replaced_by !== null && map.has(record.replaced_by)) {
        edges.get(id).add(record.replaced_by);
      }
    }
  }
  const cycle = findCycle(edges);
  if (cycle) fail(problems, "cycle", `cyclic replacement chain: ${cycle.join(" -> ")}`);
}

function checkInheritanceCycles(contracts, problems) {
  const edges = new Map();
  for (const [id, record] of contracts) {
    edges.set(id, record.inherits_from && contracts.has(record.inherits_from)
      ? new Set([record.inherits_from])
      : new Set());
  }
  const cycle = findCycle(edges);
  if (cycle) fail(problems, "cycle", `cyclic contract inheritance: ${cycle.join(" -> ")}`);
}

function checkContractReferences(contracts, problems) {
  for (const [, record] of contracts) {
    for (const relatedId of record.related_contracts ?? []) {
      if (typeof relatedId === "string" && !contracts.has(relatedId)) {
        fail(problems, "reference", `${record.at}.related_contracts references unknown contract ${relatedId}`);
      }
    }
    if (
      record.inherits_from !== null &&
      typeof record.inherits_from === "string" &&
      !contracts.has(record.inherits_from)
    ) {
      fail(problems, "reference", `${record.at}.inherits_from references unknown contract ${record.inherits_from}`);
    }
  }
}

// --- Assurance ------------------------------------------------------------

// Assurance is a SET of required evidence classes plus independence groups —
// deliberately not a numeric ladder. A contract is "assured" only when every
// required evidence class has >=1 ACTIVE binding covering it AND every
// required independence group has >=1 ACTIVE binding tagged with it.
// Several bindings sharing one independence group satisfy that group once,
// never more — which is exactly what stops correlated tests from
// masquerading as independent evidence (issue #3426 acceptance criterion).
//
// `perContract` carries the same coverage computation this function already
// does for its own pass/fail verdict, keyed by contract ID, for every
// contract regardless of status — not just the active ones `assured`/
// `excluded` summarize. It exists so a consumer that needs the DETAIL (which
// classes/groups are covered, by which active bindings) — semantic-
// report.mjs (issue #3435) is the first one — reads it from here instead of
// re-walking model.bindings itself, which would drift the moment this
// function's own coverage rule changed and the second copy did not.
export function computeAssurance(model, problems) {
  const bindingsByContract = new Map();
  for (const [, binding] of model.bindings) {
    if (binding.status !== "active") continue;
    if (!bindingsByContract.has(binding.contract))
      bindingsByContract.set(binding.contract, []);
    bindingsByContract.get(binding.contract).push(binding);
  }

  const assured = [];
  const excluded = { draft: [], superseded: [], explicit_deficit: [] };
  const perContract = new Map();

  for (const [id, contract] of model.contracts) {
    const bindings = bindingsByContract.get(id) ?? [];
    const coveredClasses = new Set(bindings.map((b) => b.evidence_class));
    const coveredGroups = new Set(bindings.map((b) => b.independence_group));
    const missingClasses = (contract.required_evidence_classes ?? []).filter(
      (cls) => !coveredClasses.has(cls),
    );
    const missingGroups = (contract.required_independence_groups ?? []).filter(
      (group) => !coveredGroups.has(group),
    );
    perContract.set(id, {
      activeBindings: bindings,
      coveredClasses,
      coveredGroups,
      missingClasses,
      missingGroups,
    });

    if (contract.status !== "active") {
      if (contract.status in excluded) excluded[contract.status].push(id);
      continue;
    }
    if (missingClasses.length > 0) {
      fail(
        problems,
        "assurance",
        `${contract.at} (${id}) is active but has no active binding for required evidence class(es): ${missingClasses.join(", ")}`,
      );
    }
    if (missingGroups.length > 0) {
      fail(
        problems,
        "assurance",
        `${contract.at} (${id}) is active but has no active binding for required independence group(s): ${missingGroups.join(", ")}`,
      );
    }
    if (missingClasses.length === 0 && missingGroups.length === 0) assured.push(id);
  }

  return { assured, excluded, perContract };
}

// --- Top-level entry points -------------------------------------------

// Validates six already-parsed JSON documents (as plain objects, one per
// record kind) and returns the merged model plus its assurance summary.
// Exposed directly (rather than only through loadSemanticModel) so tests can
// build in-memory fixtures without touching disk.
export function validateSemanticModel(documents) {
  const problems = [];

  const heads = validateHeads(documents.heads, problems);
  const capabilities = validateCapabilities(documents.capabilities, problems);
  const contracts = validateContracts(documents.contracts, new Set(capabilities.keys()), problems);
  const verifiers = validateVerifiers(documents.verifiers, problems);
  const bindings = validateBindings(
    documents.bindings,
    new Set(contracts.keys()),
    new Set(verifiers.keys()),
    problems,
  );
  const executions = validateExecutions(documents.executions, new Set(bindings.keys()), problems);

  checkReplacementMutuality(heads, "head", problems);
  checkReplacementMutuality(capabilities, "capability", problems);
  checkReplacementMutuality(contracts, "contract", problems);
  checkReplacementMutuality(verifiers, "verifier", problems);
  checkReplacementCycles([heads, capabilities, contracts, verifiers], problems);
  checkContractReferences(contracts, problems);
  checkInheritanceCycles(contracts, problems);

  const model = { heads, capabilities, contracts, verifiers, bindings, executions };
  const assurance = computeAssurance(model, problems);

  if (problems.length > 0) {
    throw new SemanticModelError("invalid semantic contract model", problems);
  }

  return { ...model, assurance };
}

// Loads and validates the six JSON files from `dir` (default test/semantic,
// resolved against `root`; an absolute `dir` overrides `root` entirely, the
// same convention `ci-lane-contract.mjs`'s `loadRegistry` follows for its
// own CI_LANE_REGISTRY override).
export function loadSemanticModel(dir = DEFAULT_SEMANTIC_MODEL_DIR, { root = process.cwd() } = {}) {
  const base = resolve(root, dir);
  const documents = {
    heads: parseJSONFile(join(base, "heads.json"), "heads.json"),
    capabilities: parseJSONFile(join(base, "capabilities.json"), "capabilities.json"),
    contracts: parseJSONFile(join(base, "contracts.json"), "contracts.json"),
    verifiers: parseJSONFile(join(base, "verifiers.json"), "verifiers.json"),
    bindings: parseJSONFile(join(base, "bindings.json"), "bindings.json"),
    executions: parseJSONFile(join(base, "executions.json"), "executions.json"),
  };
  return validateSemanticModel(documents);
}

export function renderSummary(model) {
  const lines = [
    "## Semantic contract model",
    "",
    `- heads: **${model.heads.size}**`,
    `- capabilities: **${model.capabilities.size}**`,
    `- contracts: **${model.contracts.size}** (assured: ${model.assurance.assured.length}, draft: ${model.assurance.excluded.draft.length}, superseded: ${model.assurance.excluded.superseded.length}, explicit deficit: ${model.assurance.excluded.explicit_deficit.length})`,
    `- verifiers: **${model.verifiers.size}**`,
    `- bindings: **${model.bindings.size}**`,
    `- executions: **${model.executions.size}**`,
  ];
  return `${lines.join("\n")}\n`;
}
