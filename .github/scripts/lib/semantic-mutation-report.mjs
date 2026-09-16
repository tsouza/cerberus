// semantic-mutation-report.mjs — extends the semantic conformance report
// (lib/semantic-report.mjs, issue #3435) with a versioned, per-head/per-
// contract/per-detector cohort report over the hand-authored domain-semantic
// mutation pilot (test/semantic/mutants/*.json, issues #3448-#3451). It never
// replaces semantic-report.mjs's own contract/binding/lane report — this
// module's buildMutationCohortReport() output is embedded as one additional
// top-level section (see semantic-report.mjs's own wiring), exactly the
// "extends, doesn't replace" scope issue #3452 sets for itself.
//
// WHY A UNIVERSAL KILL PERCENTAGE IS NOT COMPUTED HERE. A single number over
// all 13 committed records would silently blend two incomparable cohorts:
// seven SYNTHETIC records that exist only to exercise the runner's own seven
// classification paths (lib/semantic-mutation.mjs's own header) — one is
// EXPECTED to survive, one is EXPECTED to time out — against six REAL,
// per-head domain mutations that actually probe verifier sensitivity. This
// module keeps the two cohorts in permanently separate buckets
// (synthetic_cohort vs semantic_cohort below); the escape/kill rate is
// computed ONLY over the semantic cohort, never blended with the harness
// self-test.
//
// DENOMINATOR DISCIPLINE (issue #3452's own "Proposed change" #2). A mutant
// record's classification is a CLOSED, ORDERED set of seven outcomes
// (semantic-mutation.mjs's CLASSIFICATIONS). Exactly two of them —
// "killed" and "survived" — are outcomes where a real detector actually ran
// against a validly-applied, non-equivalent mutation with a passing clean
// control (runMutant's own contract: any clean-control failure aborts to
// infrastructure-error BEFORE a mutant is ever attempted, so every "killed"/
// "survived" record already implies a passing control — see
// lib/semantic-mutation.mjs's runMutant). Every other outcome is EXCLUDED
// from the denominator, in its own visible bucket, never folded into either
// side of the rate:
//   - "equivalent-reviewed"   an audited exemption, not a clean/kill result
//   - "invalid-transform"     the mutation was never validly applied at all
//   - "build-failed"/"timeout"/"infrastructure-error"   the measurement
//     itself never completed — INCOMPLETE, not evidence either way
// dispositionBucket() below is the one function that draws this line; every
// rate this module computes calls it, so there is exactly one place the
// denominator rule can drift.
//
// BOUND (declared) vs OBSERVED, mirrored from lib/semantic-report.mjs. A
// mutant record's own `expected_detection` is a DECLARATION, continuously
// re-verified by CI's required `ci.check` lane (semantic-mutation-corpus.mjs
// runs every committed record for real on every PR) — but a declaration is
// not the same claim as "this exact run, on this exact revision, produced
// this classification". resolveDisposition() below prefers a REAL, revision-
// bound observation from the hand-authored ledger
// (test/semantic/mutant-executions.json, ledger loader in
// lib/semantic-mutation.mjs) over the static declaration whenever the ledger
// has one — which is what lets a report reflect an equivalence review going
// stale (lib/semantic-mutation.mjs's runMutant falls back to a bare
// "survived" the moment the review's own source_fingerprint drifts from the
// live target) even though the STATIC record still names
// "equivalent-reviewed". Absent an observation, the declaration is reported
// as such (`source: "declared"`), never silently upgraded to "observed".
//
// DETERMINISM. Every function here is pure: no Date.now(), no environment
// read, no random iteration order (every emitted array is explicitly sorted
// by ID). buildMutationCohortReport() is a pure function of its inputs,
// exactly like lib/semantic-report.mjs's own buildReport().

import { CLASSIFICATIONS, sha256Hex } from "./semantic-mutation.mjs";
import { CANONICAL_HEAD_IDS } from "./semantic-model.mjs";

export const MUTATION_COHORT_SCHEMA_VERSION = 1;

// Below this many semantic-cohort records, a percentage's own granularity
// overstates its precision — one record flipping status moves the rate by
// more than ten percentage points. Named so a reader of a report entry never
// has to reverse-engineer "why is this one flagged small_sample" from a bare
// number.
export const SMALL_COHORT_DENOMINATOR_FLOOR = 10;

// Re-exported under this module's own name for API stability — the
// canonical three-head set itself is lib/semantic-model.mjs's own
// CANONICAL_HEAD_IDS (the model's closed-set invariant, issue #3426), never
// re-declared here. lib/semantic-report.mjs's renderMarkdown hardcodes the
// same three IDs as literal renderHeadSection() calls rather than looping
// this constant — a pre-existing choice this module does not change — but
// this module's own two uses below both go through the one shared source.
export const CANONICAL_HEADS = CANONICAL_HEAD_IDS;

const DENOMINATOR_STATUSES = new Set(["killed", "survived"]);
const INCOMPLETE_STATUSES = new Set(["build-failed", "timeout", "infrastructure-error"]);

/**
 * Classifies one resolved status into the bucket its rate math belongs in.
 * The ONE place the denominator rule (see this module's header) is drawn —
 * every aggregate below calls this rather than re-deriving the rule.
 */
export function dispositionBucket(status) {
  if (DENOMINATOR_STATUSES.has(status)) return "denominator";
  if (status === "equivalent-reviewed") return "equivalent";
  if (INCOMPLETE_STATUSES.has(status)) return "incomplete";
  if (status === "invalid-transform") return "invalid";
  throw new Error(`dispositionBucket: unrecognised status ${JSON.stringify(status)}`);
}

/**
 * Resolves ONE mutant record's disposition: an actual observation from the
 * ledger (Map<mutantId, executionRecord>, from lib/semantic-mutation.mjs's
 * loadMutantExecutions) when one exists, else the record's own declared
 * expected_detection. Never blends the two — `source` says which.
 */
export function resolveDisposition(record, executionsByMutant = new Map()) {
  const observed = executionsByMutant.get(record.id);
  if (observed) {
    return {
      status: observed.status,
      source: "observed",
      observed_at: observed.observed_at,
      source_sha: observed.source_sha,
      run_ref: observed.run_ref,
      detectors: observed.detectors ?? [],
    };
  }
  return {
    status: record.expected_detection,
    source: "declared",
    observed_at: null,
    source_sha: null,
    run_ref: null,
    detectors: null,
  };
}

/**
 * Resolves a non-synthetic record's `violated_contracts` to the union of
 * canonical heads they apply to (via the semantic contract model's own
 * `applicableHeads`, computed once by lib/semantic-model.mjs — never
 * re-derived here) plus each contract's own scope, so a cross-head or
 * architecture-scoped mutation is disclosed rather than silently dropped or
 * mis-filed under one head. A synthetic record (whose violated_contracts
 * name SYNTHETIC- placeholders, never real contract IDs) always resolves to
 * no heads — it is not a per-head query mutation, by its own
 * synthetic_rationale.
 */
export function mutantHeads(record, contracts = new Map()) {
  if (record.synthetic) return { heads: [], contract_scopes: {}, unresolved: [] };
  const heads = new Set();
  const contractScopes = {};
  const unresolved = [];
  for (const contractId of record.violated_contracts) {
    const contract = contracts.get(contractId);
    if (!contract) {
      unresolved.push(contractId);
      continue;
    }
    contractScopes[contractId] = contract.scope;
    for (const headId of contract.applicableHeads ?? []) heads.add(headId);
  }
  return { heads: [...heads].sort(), contract_scopes: contractScopes, unresolved };
}

/**
 * A content fingerprint over the cohort's own decision-relevant facts (id,
 * synthetic, RESOLVED disposition status, violated contracts) — sorted and
 * joined deterministically, hashed with the same sha256Hex
 * lib/semantic-mutation.mjs already uses for fingerprinting mutated source.
 * Two cohorts with the same fingerprint are provably identical in every fact
 * a published rate depends on; changing a single record's disposition
 * changes this fingerprint even when a rounding coincidence would otherwise
 * leave a published rate unchanged — the cohort REVISION moves whenever the
 * cohort's own content does, independent of whether the currently-published
 * rate happens to move too.
 *
 * DELIBERATELY the RESOLVED status (record.disposition.status — a fresh
 * ledger observation when one exists, else the declared expected_detection;
 * see resolveDisposition), never the bare declared expected_detection alone.
 * A ledger observation overriding a record's declaration (the exact
 * mechanism this module exists to surface — an equivalent-reviewed
 * adjudication going stale, say) changes what the report PUBLISHES without
 * touching the record's own committed JSON at all; fingerprinting only the
 * declaration would let that exact case move a published rate while leaving
 * cohort_revision unchanged, precisely the "changing a record's
 * classification cannot silently improve an unchanged score" failure this
 * revision exists to rule out.
 *
 * Takes the already-built, per-record report entries (this module's own
 * `records` array — each `{id, synthetic, violated_contracts, disposition}`
 * shape) rather than raw mutant records, so the resolution work done to
 * build them is never repeated.
 */
export function cohortFingerprint(records) {
  const lines = [...records]
    .map((r) => `${r.id}|${r.synthetic}|${r.disposition.status}|${[...r.violated_contracts].sort().join(",")}`)
    .sort();
  return sha256Hex(Buffer.from(lines.join("\n"), "utf8"));
}

function emptyBucketCounts() {
  return Object.fromEntries(CLASSIFICATIONS.map((c) => [c, 0]));
}

/**
 * Aggregates a list of resolved dispositions ({status, ...}) into the rate
 * report shape: raw per-status counts (every CLASSIFICATIONS value, always
 * present even at zero — see the acceptance criterion this satisfies:
 * equivalent/invalid/incomplete cases stay VISIBLE, never dropped from the
 * shape just because a cohort happens to have none), the denominator-only
 * escape/kill rate, and an explicit small_sample flag. An empty `dispositions`
 * array (denominator 0) reports both rates as `null` — never 0/0, never
 * NaN, and never silently omitted — so "no data" is never misread as "no
 * escapes".
 */
export function dispositionRates(dispositions) {
  const byStatus = emptyBucketCounts();
  const byBucket = { denominator: 0, equivalent: 0, incomplete: 0, invalid: 0 };
  for (const d of dispositions) {
    byStatus[d.status] += 1;
    byBucket[dispositionBucket(d.status)] += 1;
  }
  const denominator = byBucket.denominator;
  const killed = byStatus.killed;
  const survived = byStatus.survived;
  return {
    total: dispositions.length,
    by_status: byStatus,
    by_bucket: byBucket,
    denominator,
    killed,
    survived,
    escape_rate: denominator > 0 ? survived / denominator : null,
    kill_rate: denominator > 0 ? killed / denominator : null,
    small_sample: denominator > 0 && denominator < SMALL_COHORT_DENOMINATOR_FLOOR,
  };
}

/**
 * Builds the full, deterministic mutation-cohort report object embedded
 * into docs/semantic-conformance.{md,json} as `.mutation_cohort`.
 *
 * `mutantRecords` is loadMutants()'s own Map<id, record>. `contracts` is the
 * semantic model's contracts Map (model.contracts from loadSemanticModel),
 * used only for head/scope attribution — this module never re-validates or
 * re-derives assurance from it. `executions` is
 * loadMutantExecutions()'s own Map<mutantId, executionRecord>, optional
 * (defaults empty — an absent ledger falls back to every record's own
 * declared expected_detection, never a hard failure).
 */
export function buildMutationCohortReport(mutantRecords, { contracts = new Map(), executions = new Map() } = {}) {
  const sortedIds = [...mutantRecords.keys()].sort();

  const records = sortedIds.map((id) => {
    const record = mutantRecords.get(id);
    const disposition = resolveDisposition(record, executions);
    const { heads, contract_scopes, unresolved } = mutantHeads(record, contracts);
    return {
      id: record.id,
      title: record.title,
      synthetic: record.synthetic,
      synthetic_rationale: record.synthetic_rationale,
      violated_contracts: [...record.violated_contracts].sort(),
      heads,
      contract_scopes,
      unresolved_contracts: unresolved,
      target_path: record.transformation.target_path,
      patch_path: record.transformation.patch_path,
      detectors: record.detectors.map((d) => ({
        id: d.id,
        package: d.package,
        test_run: d.test_run,
        build_tags: [...d.build_tags],
        timeout_seconds: d.timeout_seconds,
        requires_chdb: record.isolation.requires_chdb,
      })),
      isolation: { ...record.isolation },
      declared_status: record.expected_detection,
      disposition,
      bucket: dispositionBucket(disposition.status),
      equivalence_review: record.equivalence_review,
      linked_issue: record.linked_issue,
      notes: record.notes,
    };
  });

  const syntheticRecords = records.filter((r) => r.synthetic);
  const semanticRecords = records.filter((r) => !r.synthetic);

  const byHead = {};
  for (const headId of CANONICAL_HEADS) {
    const inHead = semanticRecords.filter((r) => r.heads.includes(headId));
    byHead[headId] = {
      record_ids: inHead.map((r) => r.id),
      rates: dispositionRates(inHead.map((r) => r.disposition)),
    };
  }

  const byContract = {};
  for (const record of semanticRecords) {
    for (const contractId of record.violated_contracts) {
      (byContract[contractId] ??= []).push(record.id);
    }
  }
  for (const contractId of Object.keys(byContract)) byContract[contractId].sort();

  const crossHeadRecordIds = semanticRecords.filter((r) => r.heads.length > 1).map((r) => r.id).sort();

  const unresolvedSurvivors = semanticRecords
    .filter((r) => r.disposition.status === "survived" && r.linked_issue === null)
    .map((r) => r.id)
    .sort();

  // disposition_disagreements — a record whose LATEST ledger observation
  // disagrees with what it still declares. test/semantic/mutant-
  // executions.json is hand-authored and never cross-checked by CI against
  // expected_detection (unlike the required ci.check corpus step, which
  // re-verifies the DECLARATION on every PR but never touches this ledger),
  // so a drift here would otherwise be invisible — this is the one place it
  // surfaces. A record whose observation simply CONFIRMS its declaration
  // never appears here, regardless of source.
  const dispositionDisagreements = records
    .filter((r) => r.disposition.source === "observed" && r.disposition.status !== r.declared_status)
    .map((r) => ({ id: r.id, declared_status: r.declared_status, observed_status: r.disposition.status }))
    .sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));

  return {
    schema_version: MUTATION_COHORT_SCHEMA_VERSION,
    cohort_revision: cohortFingerprint(records),
    generated_from:
      "test/semantic/mutants/*.json (+ test/semantic/mutant-executions.json when an observation exists)",
    records,
    synthetic_cohort: {
      description:
        "Harness self-test records (lib/semantic-mutation.mjs's own seven classification paths) — " +
        "never a per-head query mutation, and never folded into semantic_cohort's rate below.",
      record_ids: syntheticRecords.map((r) => r.id),
      rates: dispositionRates(syntheticRecords.map((r) => r.disposition)),
    },
    semantic_cohort: {
      description:
        "Real, per-head domain-semantic mutations (issues #3449-#3451) — the ONLY cohort " +
        "escape_rate/kill_rate below is computed over.",
      record_ids: semanticRecords.map((r) => r.id),
      rates: dispositionRates(semanticRecords.map((r) => r.disposition)),
    },
    by_head: byHead,
    by_contract: byContract,
    cross_head_record_ids: crossHeadRecordIds,
    unresolved_survivors: unresolvedSurvivors,
    disposition_disagreements: dispositionDisagreements,
  };
}

// --- Rendering ---------------------------------------------------------------

function pct(rate) {
  return rate === null ? "n/a (zero denominator)" : `${(rate * 100).toFixed(1)}%`;
}

function mdEscape(text) {
  return String(text).replaceAll("|", "\\|");
}

const MUTATION_DENOMINATOR_DISCLAIMER =
  "Escape/kill rate is computed ONLY over records in the `denominator` bucket " +
  "(`killed` + `survived` — a real detector ran, against a validly-applied, " +
  "non-equivalent mutation, with a passing clean control). `equivalent` " +
  "(an audited exemption), `invalid` (the mutation never validly applied), " +
  "and `incomplete` (build-failed/timeout/infrastructure-error — the " +
  "measurement itself never finished) are each reported separately and " +
  "never folded into either side of the rate — an incomplete run is neither " +
  "a kill nor a clean escape.";

const MUTATION_SYNTHETIC_DISCLAIMER =
  "The synthetic cohort exists only to exercise the runner's own seven " +
  "classification paths (one record is DESIGNED to survive, one to time " +
  "out, …) — it is never a per-head query mutation and its rate below is " +
  "never blended with the semantic cohort's.";

function renderRatesTable(rates) {
  const lines = [];
  lines.push("| status | count |");
  lines.push("| --- | --- |");
  for (const status of CLASSIFICATIONS) lines.push(`| ${status} | ${rates.by_status[status]} |`);
  lines.push(`| **total** | **${rates.total}** |`);
  lines.push("");
  lines.push(
    `denominator (killed+survived): **${rates.denominator}**` +
      `${rates.small_sample ? " — **SMALL SAMPLE**, treat the percentages below as illustrative, not conclusive" : ""}`,
  );
  lines.push(`- kill rate: **${pct(rates.kill_rate)}** (${rates.killed}/${rates.denominator})`);
  lines.push(`- escape rate: **${pct(rates.escape_rate)}** (${rates.survived}/${rates.denominator})`);
  lines.push(
    `- excluded: equivalent **${rates.by_bucket.equivalent}**, invalid **${rates.by_bucket.invalid}**, incomplete **${rates.by_bucket.incomplete}**`,
  );
  return lines.join("\n");
}

function renderMutantRow(record) {
  const detectorIds = record.detectors.map((d) => d.id).join(", ");
  const observed =
    record.disposition.source === "observed"
      ? `observed (${record.disposition.observed_at ?? "?"}${record.disposition.source_sha ? `, \`${record.disposition.source_sha.slice(0, 12)}\`` : ""})`
      : "declared (no ledger observation)";
  return (
    `| ${record.id} | ${record.disposition.status} | ${record.bucket} | ${observed} | ` +
    `\`${mdEscape(detectorIds)}\` | \`${mdEscape(record.target_path)}\` |`
  );
}

function renderMutantSection(title, records) {
  if (records.length === 0) return `${title}\n\n(none)\n`;
  const lines = [title, ""];
  lines.push("| id | disposition | bucket | source | detector(s) | target |");
  lines.push("| --- | --- | --- | --- | --- | --- |");
  for (const record of records) lines.push(renderMutantRow(record));
  return lines.join("\n");
}

/**
 * Renders the mutation-cohort section embedded into
 * docs/semantic-conformance.md, from a buildMutationCohortReport() result.
 * Pure function of its input, like every other renderer in this repo's
 * semantic-report family.
 */
export function renderMutationCohortMarkdown(report) {
  const parts = [];
  parts.push("## Semantic mutation pilot\n");
  parts.push(
    "A bounded, hand-authored cohort of contract-linked domain-semantic " +
      "mutations (issues #3448-#3452), isolated from the developer checkout " +
      "via `go test -overlay` (lib/semantic-mutation.mjs) — distinct from, " +
      "and never a substitute for, the required traditional `mutation` " +
      "(gremlins) lane. Cohort revision (content fingerprint over every " +
      "record's id/synthetic/violated_contracts and its RESOLVED disposition " +
      "— the latest ledger observation when one exists, not merely the " +
      "declared expected_detection — so a classification change is visible " +
      "even when a published rate's own digits do not move): `" +
      `${report.cohort_revision}` + "`.\n",
  );
  parts.push(`${MUTATION_DENOMINATOR_DISCLAIMER}\n`);

  parts.push("### Semantic cohort (real per-head domain mutations)\n");
  parts.push(`${report.semantic_cohort.description}\n`);
  parts.push(renderRatesTable(report.semantic_cohort.rates));
  parts.push("");

  for (const headId of CANONICAL_HEADS) {
    const head = report.by_head[headId];
    const headRecords = report.records.filter((r) => head.record_ids.includes(r.id));
    parts.push(renderMutantSection(`#### ${headId}`, headRecords));
    parts.push("");
    parts.push(renderRatesTable(head.rates));
    parts.push("");
  }

  if (report.cross_head_record_ids.length > 0) {
    parts.push(
      `**Cross-head mutations** (counted under every head their violated contracts apply to, ` +
        `never double-hidden): ${report.cross_head_record_ids.map((id) => `\`${id}\``).join(", ")}.\n`,
    );
  }

  parts.push("### Survivors requiring a linked issue\n");
  parts.push(
    "A non-synthetic record can never DECLARE `expected_detection: \"survived\"` — #3520/#3532 forbid it " +
      "at the schema level (a bare non-killed outcome on real evidence is an expected-failure/tolerance-list " +
      "entry, invariant 7). The only way a real record's RESOLVED disposition is ever `survived` is a fresh " +
      "`test/semantic/mutant-executions.json` observation overriding its own valid declaration — an actual " +
      "regression an execution caught. Listed below when that has happened and the record's own `linked_issue` " +
      "is still null.\n",
  );
  if (report.unresolved_survivors.length === 0) {
    parts.push("None on record.\n");
  } else {
    parts.push(
      report.unresolved_survivors.map((id) => `- \`${id}\` — reproduce via \`just semantic-mutate ${id}\`.`).join("\n") + "\n",
    );
  }

  parts.push("### Disposition disagreements (declared vs. observed)\n");
  parts.push(
    "A record whose latest ledger observation disagrees with what it still declares — the ledger is " +
      "hand-authored and never cross-checked by CI against `expected_detection`, so this is the one place " +
      "such a drift becomes visible.\n",
  );
  if (report.disposition_disagreements.length === 0) {
    parts.push("None on record.\n");
  } else {
    parts.push(
      report.disposition_disagreements
        .map((d) => `- \`${d.id}\` — declared \`${d.declared_status}\`, observed \`${d.observed_status}\`.`)
        .join("\n") + "\n",
    );
  }

  parts.push("### Synthetic self-test cohort\n");
  parts.push(`${MUTATION_SYNTHETIC_DISCLAIMER}\n`);
  parts.push(renderRatesTable(report.synthetic_cohort.rates));
  parts.push("");

  return parts.join("\n");
}
