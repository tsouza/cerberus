// semantic-mutation-report.test.mjs — node --test guard for the semantic
// mutation cohort report (lib/semantic-mutation-report.mjs, cerberus issue
// #3452). Small in-memory fixtures drive the acceptance-criteria-shaped
// cases directly (empty denominator, an incomplete run, correlated
// detectors collapsing to one record, a stale equivalence review moving the
// denominator) — never a real `go test -overlay` invocation, mirroring
// semantic-mutation.test.mjs's own discipline. A handful of end-to-end
// checks run the real committed test/semantic/mutants/ corpus.

import assert from "node:assert/strict";
import { test } from "node:test";

import { CLASSIFICATIONS, loadMutants } from "./lib/semantic-mutation.mjs";
import { DEFAULT_SEMANTIC_MODEL_DIR, loadSemanticModel } from "./lib/semantic-model.mjs";
import {
  CANONICAL_HEADS,
  SMALL_COHORT_DENOMINATOR_FLOOR,
  buildMutationCohortReport,
  cohortFingerprint,
  dispositionBucket,
  dispositionRates,
  mutantHeads,
  renderMutationCohortMarkdown,
  resolveDisposition,
} from "./lib/semantic-mutation-report.mjs";

// --- Fixture builders --------------------------------------------------

function mutantRecord(overrides = {}) {
  return {
    schema_version: 1,
    id: "MUTANT-FIXTURE-EXAMPLE",
    title: "fixture mutant",
    synthetic: false,
    synthetic_rationale: null,
    violated_contracts: ["FIXTURE-CONTRACT"],
    transformation: {
      target_path: "internal/fixture/example.go",
      patch_path: "test/semantic/mutants/patches/fixture.patch",
      source_fingerprint: "a".repeat(64),
      expected_mutated_fingerprint: "b".repeat(64),
    },
    detectors: [
      {
        id: "example-detector",
        package: "./internal/fixture",
        test_run: "^TestExample$",
        build_tags: [],
        timeout_seconds: 30,
      },
    ],
    expected_detection: "killed",
    equivalence_review: null,
    isolation: { requires_chdb: false, memory_max: "1GiB", memory_hold: "1s" },
    notes: null,
    linked_issue: null,
    ...overrides,
  };
}

function recordsMap(records) {
  return new Map(records.map((r) => [r.id, r]));
}

function contractsMap(entries) {
  // entries: [id, { scope, applicableHeads: [...] }]
  return new Map(entries.map(([id, c]) => [id, { ...c, applicableHeads: new Set(c.applicableHeads) }]));
}

// --- dispositionBucket ---------------------------------------------------

test("dispositionBucket: the seven classifications map to exactly four buckets", () => {
  assert.equal(dispositionBucket("killed"), "denominator");
  assert.equal(dispositionBucket("survived"), "denominator");
  assert.equal(dispositionBucket("equivalent-reviewed"), "equivalent");
  assert.equal(dispositionBucket("invalid-transform"), "invalid");
  assert.equal(dispositionBucket("build-failed"), "incomplete");
  assert.equal(dispositionBucket("timeout"), "incomplete");
  assert.equal(dispositionBucket("infrastructure-error"), "incomplete");
});

// --- Empty denominator ---------------------------------------------------

test("dispositionRates: an empty denominator reports null rates, never 0/0 or NaN", () => {
  const rates = dispositionRates([{ status: "equivalent-reviewed" }, { status: "invalid-transform" }]);
  assert.equal(rates.denominator, 0);
  assert.equal(rates.escape_rate, null);
  assert.equal(rates.kill_rate, null);
  assert.equal(rates.small_sample, false, "a zero denominator is not itself flagged small_sample");
});

test("buildMutationCohortReport: a semantic cohort with no killed/survived records reports a null rate, not zero", () => {
  const records = recordsMap([
    mutantRecord({ id: "MUTANT-A", expected_detection: "invalid-transform" }),
    mutantRecord({ id: "MUTANT-B", expected_detection: "build-failed" }),
  ]);
  const report = buildMutationCohortReport(records, { contracts: new Map() });
  assert.equal(report.semantic_cohort.rates.denominator, 0);
  assert.equal(report.semantic_cohort.rates.escape_rate, null);
  assert.equal(report.semantic_cohort.rates.kill_rate, null);
  // Both records still visible, just excluded from the rate.
  assert.equal(report.semantic_cohort.rates.by_bucket.invalid, 1);
  assert.equal(report.semantic_cohort.rates.by_bucket.incomplete, 1);
});

// --- Incomplete run --------------------------------------------------------

test("dispositionRates: a timeout/build-failed/infrastructure-error run is neither a kill nor a clean escape", () => {
  const rates = dispositionRates([
    { status: "killed" },
    { status: "survived" },
    { status: "timeout" },
    { status: "build-failed" },
    { status: "infrastructure-error" },
  ]);
  assert.equal(rates.denominator, 2, "only killed+survived count toward the denominator");
  assert.equal(rates.kill_rate, 0.5);
  assert.equal(rates.escape_rate, 0.5);
  assert.equal(rates.by_bucket.incomplete, 3);
  assert.equal(rates.total, 5, "the incomplete records stay visible in the total");
});

// --- Correlated detectors --------------------------------------------------

test("buildMutationCohortReport: a mutant with N correlated detectors still contributes exactly ONE record to the denominator", () => {
  const records = recordsMap([
    mutantRecord({
      id: "MUTANT-MULTI-DETECTOR",
      expected_detection: "killed",
      detectors: [
        { id: "d1", package: "./internal/fixture", test_run: "^Test1$", build_tags: [], timeout_seconds: 30 },
        { id: "d2", package: "./internal/fixture", test_run: "^Test2$", build_tags: [], timeout_seconds: 30 },
        { id: "d3", package: "./internal/fixture", test_run: "^Test3$", build_tags: [], timeout_seconds: 30 },
      ],
    }),
  ]);
  const report = buildMutationCohortReport(records, { contracts: new Map() });
  assert.equal(report.semantic_cohort.rates.denominator, 1, "three detectors on one record is one attempt, not three");
  assert.equal(report.semantic_cohort.rates.killed, 1);
  assert.equal(report.records[0].detectors.length, 3, "the individual detectors stay visible on the record");
});

test("buildMutationCohortReport: two multi-detector records (correlated within each) sum to their own record count, not detector count", () => {
  const twoDetectors = [
    { id: "d1", package: "./internal/fixture", test_run: "^Test1$", build_tags: [], timeout_seconds: 30 },
    { id: "d2", package: "./internal/fixture", test_run: "^Test2$", build_tags: [], timeout_seconds: 30 },
  ];
  const records = recordsMap([
    mutantRecord({ id: "MUTANT-KILLED-PAIR", expected_detection: "killed", detectors: twoDetectors }),
    mutantRecord({ id: "MUTANT-SURVIVED-PAIR", expected_detection: "survived", detectors: twoDetectors }),
  ]);
  const report = buildMutationCohortReport(records, { contracts: new Map() });
  assert.equal(report.semantic_cohort.rates.denominator, 2, "two records, four detectors — denominator is 2");
  assert.equal(report.semantic_cohort.rates.killed, 1);
  assert.equal(report.semantic_cohort.rates.survived, 1);
});

// --- Stale equivalence review (observed overrides declared) ---------------

test("resolveDisposition: an actual observation overrides the record's own declared expected_detection", () => {
  const record = mutantRecord({ id: "MUTANT-STALE-REVIEW", expected_detection: "equivalent-reviewed" });
  const executions = new Map([
    [
      "MUTANT-STALE-REVIEW",
      {
        status: "survived",
        observed_at: "2026-09-16T00:00:00Z",
        source_sha: "f".repeat(40),
        run_ref: "https://example.invalid/run/1",
        detectors: [{ id: "example-detector", classification: "survived" }],
      },
    ],
  ]);
  const resolved = resolveDisposition(record, executions);
  assert.equal(resolved.status, "survived", "a fresh observation must win over a stale declared adjudication");
  assert.equal(resolved.source, "observed");
});

test("resolveDisposition: with no ledger entry, the declared expected_detection is used and labelled as such", () => {
  const record = mutantRecord({ expected_detection: "equivalent-reviewed" });
  const resolved = resolveDisposition(record, new Map());
  assert.equal(resolved.status, "equivalent-reviewed");
  assert.equal(resolved.source, "declared");
  assert.equal(resolved.observed_at, null);
});

test("buildMutationCohortReport: a stale equivalence review's fresher 'survived' observation moves the denominator, not just the escape count", () => {
  const records = recordsMap([mutantRecord({ id: "MUTANT-STALE-REVIEW", expected_detection: "equivalent-reviewed" })]);

  const beforeReport = buildMutationCohortReport(records, { contracts: new Map() });
  assert.equal(beforeReport.semantic_cohort.rates.denominator, 0, "an equivalent-reviewed record is excluded");
  assert.equal(beforeReport.semantic_cohort.rates.by_bucket.equivalent, 1);

  const executions = new Map([
    [
      "MUTANT-STALE-REVIEW",
      {
        status: "survived",
        observed_at: "2026-09-16T00:00:00Z",
        source_sha: "f".repeat(40),
        run_ref: "https://example.invalid/run/1",
        detectors: [],
      },
    ],
  ]);
  const afterReport = buildMutationCohortReport(records, { contracts: new Map(), executions });
  assert.equal(afterReport.semantic_cohort.rates.denominator, 1, "the fresh observation must enter the denominator");
  assert.equal(afterReport.semantic_cohort.rates.by_bucket.equivalent, 0, "no longer excluded as equivalent");
  assert.equal(afterReport.semantic_cohort.rates.escape_rate, 1, "a real escape must be visible, not silently absorbed");
});

// --- Cohort revision / fingerprint ------------------------------------------
//
// cohortFingerprint takes the already-RESOLVED report records (each
// {id, synthetic, violated_contracts, disposition: {status}}), never raw
// mutant records — these fixtures build that shape directly rather than
// going through buildMutationCohortReport, so the fingerprint's own
// sensitivity is pinned independent of everything else buildReport does.

function reportRecord({ id = "MUTANT-A", synthetic = false, violatedContracts = ["FIXTURE-CONTRACT"], status = "killed" } = {}) {
  return { id, synthetic, violated_contracts: violatedContracts, disposition: { status } };
}

test("cohortFingerprint: changing one record's RESOLVED status changes the fingerprint even when the total count is unchanged", () => {
  const before = [reportRecord({ status: "killed" })];
  const after = [reportRecord({ status: "survived" })];
  assert.notEqual(cohortFingerprint(before), cohortFingerprint(after));
});

test("cohortFingerprint: identical cohorts fingerprint identically (deterministic, order-independent)", () => {
  const a = [reportRecord({ id: "MUTANT-A" }), reportRecord({ id: "MUTANT-B" })];
  const b = [...a].reverse();
  assert.equal(cohortFingerprint(a), cohortFingerprint(b));
});

test("cohortFingerprint: a ledger observation overriding a record's DECLARATION moves the fingerprint even though the declared JSON is untouched", () => {
  // The exact case this fingerprint exists to catch (per its own header):
  // fingerprinting only the declared expected_detection would leave
  // cohort_revision unchanged here, while the published rate moves.
  const records = recordsMap([mutantRecord({ id: "MUTANT-STALE", expected_detection: "equivalent-reviewed" })]);
  const before = buildMutationCohortReport(records, { contracts: new Map() });
  const executions = new Map([
    ["MUTANT-STALE", { status: "survived", observed_at: "2026-09-16T00:00:00Z", source_sha: null, run_ref: "r", detectors: [] }],
  ]);
  const after = buildMutationCohortReport(records, { contracts: new Map(), executions });
  assert.notEqual(before.cohort_revision, after.cohort_revision);
});

// --- Head / scope attribution -----------------------------------------------

test("mutantHeads: a synthetic record resolves to no heads regardless of its placeholder violated_contracts", () => {
  const record = mutantRecord({ synthetic: true, synthetic_rationale: "x", violated_contracts: ["SYNTHETIC-X"] });
  const { heads } = mutantHeads(record, new Map());
  assert.deepEqual(heads, []);
});

test("mutantHeads: a cross-head contract attributes the mutant to every applicable head, disclosed not hidden", () => {
  const contracts = contractsMap([
    ["SIGNAL-CONTRACT", { scope: "signal", applicableHeads: ["HEAD-PROMQL", "HEAD-LOGQL"] }],
  ]);
  const record = mutantRecord({ violated_contracts: ["SIGNAL-CONTRACT"] });
  const { heads, contract_scopes } = mutantHeads(record, contracts);
  assert.deepEqual(heads, ["HEAD-LOGQL", "HEAD-PROMQL"]);
  assert.equal(contract_scopes["SIGNAL-CONTRACT"], "signal");
});

test("buildMutationCohortReport: a cross-head record appears under every applicable head's own tally", () => {
  const contracts = contractsMap([
    ["SIGNAL-CONTRACT", { scope: "signal", applicableHeads: ["HEAD-PROMQL", "HEAD-LOGQL"] }],
  ]);
  const records = recordsMap([mutantRecord({ id: "MUTANT-CROSS", violated_contracts: ["SIGNAL-CONTRACT"] })]);
  const report = buildMutationCohortReport(records, { contracts });
  assert.ok(report.by_head["HEAD-PROMQL"].record_ids.includes("MUTANT-CROSS"));
  assert.ok(report.by_head["HEAD-LOGQL"].record_ids.includes("MUTANT-CROSS"));
  assert.deepEqual(report.cross_head_record_ids, ["MUTANT-CROSS"]);
});

// --- Synthetic vs semantic separation ---------------------------------------

test("buildMutationCohortReport: a synthetic record never enters the semantic cohort's rate", () => {
  const records = recordsMap([
    mutantRecord({ id: "MUTANT-SYNTH-X", synthetic: true, synthetic_rationale: "x", expected_detection: "survived" }),
  ]);
  const report = buildMutationCohortReport(records, { contracts: new Map() });
  assert.equal(report.semantic_cohort.rates.total, 0);
  assert.equal(report.synthetic_cohort.rates.total, 1);
  assert.equal(report.synthetic_cohort.rates.survived, 1);
});

// --- Small-sample disclosure -------------------------------------------------

test("dispositionRates: small_sample flags a denominator below the documented floor, clears above it", () => {
  const below = dispositionRates(
    Array.from({ length: SMALL_COHORT_DENOMINATOR_FLOOR - 1 }, () => ({ status: "killed" })),
  );
  assert.equal(below.small_sample, true);
  const atFloor = dispositionRates(Array.from({ length: SMALL_COHORT_DENOMINATOR_FLOOR }, () => ({ status: "killed" })));
  assert.equal(atFloor.small_sample, false);
});

// --- Unresolved survivors ----------------------------------------------------

// A non-synthetic record can never DECLARE expected_detection "survived" —
// #3520/#3532 (lib/semantic-mutation.mjs's NON_SYNTHETIC_CLASSIFICATIONS)
// forbid it at the schema level, since a bare non-killed outcome on real
// evidence would be an expected-failure/tolerance-list entry (invariant 7).
// So the only way a non-synthetic record's RESOLVED disposition is ever
// "survived" is a ledger observation overriding its (valid) declared
// "killed"/"equivalent-reviewed" — every fixture below reflects exactly
// that, never a declared "survived" a real record could never carry.

test("buildMutationCohortReport: an observed survivor WITH a linked_issue is not flagged as unresolved", () => {
  const records = recordsMap([
    mutantRecord({ id: "MUTANT-REAL-SURVIVOR", expected_detection: "killed", linked_issue: 9999 }),
  ]);
  const executions = new Map([
    [
      "MUTANT-REAL-SURVIVOR",
      { status: "survived", observed_at: "2026-09-16T00:00:00Z", source_sha: null, run_ref: "r", detectors: [] },
    ],
  ]);
  const report = buildMutationCohortReport(records, { contracts: new Map(), executions });
  assert.equal(report.records[0].disposition.status, "survived");
  assert.deepEqual(report.unresolved_survivors, []);
});

test("buildMutationCohortReport: an observed survivor with NO linked_issue is flagged as unresolved", () => {
  const records = recordsMap([
    mutantRecord({ id: "MUTANT-REAL-SURVIVOR", expected_detection: "killed", linked_issue: null }),
  ]);
  const executions = new Map([
    [
      "MUTANT-REAL-SURVIVOR",
      { status: "survived", observed_at: "2026-09-16T00:00:00Z", source_sha: null, run_ref: "r", detectors: [] },
    ],
  ]);
  const report = buildMutationCohortReport(records, { contracts: new Map(), executions });
  assert.deepEqual(report.unresolved_survivors, ["MUTANT-REAL-SURVIVOR"]);
});

test("buildMutationCohortReport: an observed (not declared) survivor with no linked_issue is flagged even though the record declares 'killed'", () => {
  const records = recordsMap([mutantRecord({ id: "MUTANT-DRIFTED", expected_detection: "killed", linked_issue: null })]);
  const executions = new Map([
    [
      "MUTANT-DRIFTED",
      { status: "survived", observed_at: "2026-09-16T00:00:00Z", source_sha: null, run_ref: "r", detectors: [] },
    ],
  ]);
  const report = buildMutationCohortReport(records, { contracts: new Map(), executions });
  assert.deepEqual(report.unresolved_survivors, ["MUTANT-DRIFTED"]);
});

// --- Disposition disagreements (declared vs. observed) ----------------------

test("buildMutationCohortReport: an observation that disagrees with the declaration is surfaced in disposition_disagreements", () => {
  const records = recordsMap([mutantRecord({ id: "MUTANT-DRIFTED", expected_detection: "killed" })]);
  const executions = new Map([
    ["MUTANT-DRIFTED", { status: "survived", observed_at: "2026-09-16T00:00:00Z", source_sha: null, run_ref: "r", detectors: [] }],
  ]);
  const report = buildMutationCohortReport(records, { contracts: new Map(), executions });
  assert.deepEqual(report.disposition_disagreements, [
    { id: "MUTANT-DRIFTED", declared_status: "killed", observed_status: "survived" },
  ]);
});

test("buildMutationCohortReport: an observation that CONFIRMS the declaration never appears in disposition_disagreements", () => {
  const records = recordsMap([mutantRecord({ id: "MUTANT-CONFIRMED", expected_detection: "killed" })]);
  const executions = new Map([
    ["MUTANT-CONFIRMED", { status: "killed", observed_at: "2026-09-16T00:00:00Z", source_sha: null, run_ref: "r", detectors: [] }],
  ]);
  const report = buildMutationCohortReport(records, { contracts: new Map(), executions });
  assert.deepEqual(report.disposition_disagreements, []);
});

test("buildMutationCohortReport: with no ledger entry at all, a record is never reported as a disagreement", () => {
  const records = recordsMap([mutantRecord({ id: "MUTANT-NO-LEDGER", expected_detection: "killed" })]);
  const report = buildMutationCohortReport(records, { contracts: new Map() });
  assert.deepEqual(report.disposition_disagreements, []);
});

// --- Determinism / rendering --------------------------------------------------

test("buildMutationCohortReport + renderMutationCohortMarkdown: byte-identical across two independent builds", () => {
  const records = recordsMap([mutantRecord({ id: "MUTANT-A" }), mutantRecord({ id: "MUTANT-B", expected_detection: "survived", linked_issue: 1 })]);
  const reportA = buildMutationCohortReport(records, { contracts: new Map() });
  const reportB = buildMutationCohortReport(records, { contracts: new Map() });
  assert.deepEqual(reportA, reportB);
  assert.equal(renderMutationCohortMarkdown(reportA), renderMutationCohortMarkdown(reportB));
});

// --- Real committed corpus (end to end) ----------------------------------

test("real corpus: every canonical head has real, non-empty membership, including the cross-head record", () => {
  const records = loadMutants();
  const model = loadSemanticModel(DEFAULT_SEMANTIC_MODEL_DIR);
  const report = buildMutationCohortReport(records, { contracts: model.contracts });
  assert.equal(report.records.length, 13);

  // Real membership, not just "the key exists" — each canonical head must
  // actually have at least one real record attributed to it, and the one
  // mutation whose violated_contracts span two heads (the LogQL anchoring
  // mutant, which also violates a PromQL contract via the shared
  // anchoredRegexPattern emission site) must appear under BOTH.
  for (const headId of CANONICAL_HEADS) {
    assert.ok(report.by_head[headId].record_ids.length > 0, `${headId} has no real record attributed to it`);
  }
  assert.ok(report.by_head["HEAD-LOGQL"].record_ids.includes("MUTANT-LOGQL-LABEL-MATCHER-UNANCHORED-1741"));
  assert.ok(report.by_head["HEAD-PROMQL"].record_ids.includes("MUTANT-LOGQL-LABEL-MATCHER-UNANCHORED-1741"));
  assert.deepEqual(report.cross_head_record_ids, ["MUTANT-LOGQL-LABEL-MATCHER-UNANCHORED-1741"]);

  assert.equal(report.synthetic_cohort.rates.total, 7);
  assert.equal(report.semantic_cohort.rates.total, 6);
});

test("real corpus: the synthetic cohort's seven records exercise every one of the seven CLASSIFICATIONS at least once", () => {
  // This is what makes the synthetic cohort a genuine calibration set for
  // the runner's own outcome vocabulary, not just a smoke test — a gap here
  // would mean some classification path has zero committed coverage.
  const records = loadMutants();
  const report = buildMutationCohortReport(records, { contracts: new Map() });
  for (const classification of CLASSIFICATIONS) {
    assert.ok(
      report.synthetic_cohort.rates.by_status[classification] >= 1,
      `synthetic cohort has zero records classified ${classification}`,
    );
  }
});

test("real corpus: renders without throwing", () => {
  const records = loadMutants();
  const report = buildMutationCohortReport(records, { contracts: new Map() });
  const markdown = renderMutationCohortMarkdown(report);
  assert.ok(markdown.includes("Semantic mutation pilot"));
  assert.ok(markdown.includes(report.cohort_revision));
});
