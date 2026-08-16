// Negative controls for the CI lane-selection backtest's comparison and
// aggregation logic. Entirely offline — synthetic registry/job fixtures only,
// no network access, matching this repo's ".mjs" test convention (node --test,
// no external framework).

import assert from "node:assert/strict";
import test from "node:test";

import { MERGE_P95_SLO_MINUTES } from "./ci-lane-contract.mjs";
import {
  DEFAULT_SHA_COUNT,
  RUNNER_P50_TARGET_MINUTES,
  RUNNER_P95_TARGET_MINUTES,
  aggregateBacktest,
  analyzePullRequest,
  percentile,
  renderSummary,
} from "./ci-lane-backtest.mjs";

const sha = (character) => character.repeat(40);

function lane({ id, mergePosture, workflow, contextName, packageGlobs }) {
  return {
    id,
    merge_posture: mergePosture,
    executions: ["default"],
    owner: { workflow },
    context: { match: "exact", name: contextName, protected: false },
    package_globs: packageGlobs,
  };
}

function registryFixture() {
  return {
    impact_selection: { known_nonimpact_globs: ["**/*.md", "docs/**"] },
    lanes: [
      lane({
        id: "core.always",
        mergePosture: "always",
        workflow: ".github/workflows/ci.yml",
        contextName: "check",
        packageGlobs: ["**"],
      }),
      lane({
        id: "quality.mutation",
        mergePosture: "impact",
        workflow: ".github/workflows/mutation.yml",
        contextName: "mutation",
        packageGlobs: ["internal/mutation/**"],
      }),
      lane({
        id: "schema.ddl",
        mergePosture: "impact",
        workflow: ".github/workflows/schema-integration.yml",
        contextName: "schema-ddl",
        packageGlobs: ["internal/schema/**"],
      }),
      lane({
        id: "e2e.full",
        mergePosture: "never",
        workflow: ".github/workflows/e2e.yml",
        contextName: "e2e",
        packageGlobs: ["**"],
      }),
    ],
  };
}

function job({
  name,
  status = "completed",
  conclusion = "success",
  startedAt,
  completedAt,
  durationMS,
}) {
  return {
    name,
    status,
    conclusion,
    started_at: startedAt,
    completed_at: completedAt,
    duration_ms: durationMS,
  };
}

test("analyzePullRequest: a lane the new selector selects is never a coverage gap even when it failed", () => {
  const registry = registryFixture();
  const analysis = analyzePullRequest({
    registry,
    number: 100,
    headSHA: sha("a"),
    baseSHA: sha("b"),
    mergeSHA: sha("c"),
    mergedAt: "2026-06-01T00:00:00Z",
    changedPaths: ["internal/mutation/gremlin.go"],
    jobsByWorkflow: new Map([
      [
        ".github/workflows/ci.yml",
        [job({ name: "check", startedAt: "2026-06-01T00:00:00Z", completedAt: "2026-06-01T00:05:00Z", durationMS: 300000 })],
      ],
      [
        ".github/workflows/mutation.yml",
        [
          job({
            name: "mutation",
            conclusion: "failure",
            startedAt: "2026-06-01T00:00:00Z",
            completedAt: "2026-06-01T00:10:00Z",
            durationMS: 600000,
          }),
        ],
      ],
    ]),
  });
  assert.deepEqual(analysis.coverageGaps, []);
  assert.equal(analysis.selectedDurationMinutes, 15);
  assert.equal(analysis.wallClockMinutes, 10);
  assert.equal(analysis.docsOnly, false);
  const mutationRow = analysis.laneRows.find((row) => row.lane_id === "quality.mutation");
  assert.equal(mutationRow.new_disposition, "selected");
  assert.equal(mutationRow.observed_state, "red");
});

test("analyzePullRequest: an omitted lane that historically failed is an old-only catch", () => {
  const registry = registryFixture();
  const analysis = analyzePullRequest({
    registry,
    number: 101,
    headSHA: sha("d"),
    baseSHA: sha("e"),
    mergeSHA: sha("f"),
    mergedAt: "2026-07-01T00:00:00Z",
    // Touches only schema.ddl's surface — quality.mutation's glob does not
    // match, and the path is still "known" via schema.ddl's conditional
    // glob, so the selector omits mutation cleanly rather than falling back.
    changedPaths: ["internal/schema/columns.go"],
    jobsByWorkflow: new Map([
      [
        ".github/workflows/ci.yml",
        [job({ name: "check", startedAt: "2026-07-01T00:00:00Z", completedAt: "2026-07-01T00:03:00Z", durationMS: 180000 })],
      ],
      [
        ".github/workflows/schema-integration.yml",
        [job({ name: "schema-ddl", startedAt: "2026-07-01T00:00:00Z", completedAt: "2026-07-01T00:02:00Z", durationMS: 120000 })],
      ],
      [
        ".github/workflows/mutation.yml",
        [
          job({
            name: "mutation",
            conclusion: "failure",
            startedAt: "2026-07-01T00:00:00Z",
            completedAt: "2026-07-01T00:20:00Z",
            durationMS: 1200000,
          }),
        ],
      ],
    ]),
  });
  assert.equal(analysis.coverageGaps.length, 1);
  assert.equal(analysis.coverageGaps[0].lane_id, "quality.mutation");
  assert.equal(analysis.coverageGaps[0].observed_state, "red");
  // The old-only-caught lane's cost must NOT count toward the new system's
  // hypothetical runner-minutes — it would not have run.
  assert.equal(analysis.selectedDurationMinutes, 5);
});

test("analyzePullRequest: a lane the old system skipped is not a coverage gap", () => {
  const registry = registryFixture();
  const analysis = analyzePullRequest({
    registry,
    number: 102,
    headSHA: sha("1"),
    baseSHA: sha("2"),
    mergeSHA: sha("3"),
    mergedAt: "2026-07-02T00:00:00Z",
    changedPaths: ["internal/schema/columns.go"],
    jobsByWorkflow: new Map([
      [
        ".github/workflows/ci.yml",
        [job({ name: "check", startedAt: "2026-07-02T00:00:00Z", completedAt: "2026-07-02T00:03:00Z", durationMS: 180000 })],
      ],
      [
        ".github/workflows/schema-integration.yml",
        [job({ name: "schema-ddl", startedAt: "2026-07-02T00:00:00Z", completedAt: "2026-07-02T00:02:00Z", durationMS: 120000 })],
      ],
      [
        ".github/workflows/mutation.yml",
        [job({ name: "mutation", status: "completed", conclusion: "skipped", startedAt: "2026-07-02T00:00:00Z", completedAt: "2026-07-02T00:00:00Z", durationMS: 0 })],
      ],
    ]),
  });
  assert.deepEqual(analysis.coverageGaps, []);
});

test("analyzePullRequest: an unknown path falls back to selecting every conditional lane", () => {
  const registry = registryFixture();
  const analysis = analyzePullRequest({
    registry,
    number: 103,
    headSHA: sha("4"),
    baseSHA: sha("5"),
    mergeSHA: sha("6"),
    mergedAt: "2026-07-03T00:00:00Z",
    changedPaths: ["internal/unclassified/thing.go"],
    jobsByWorkflow: new Map(),
  });
  assert.equal(analysis.selectorConclusion, "fallback_full");
  assert.equal(analysis.fallbackReason, "unknown_paths");
  assert.deepEqual(analysis.unknownPaths, ["internal/unclassified/thing.go"]);
  const rows = new Map(analysis.laneRows.map((row) => [row.lane_id, row]));
  assert.equal(rows.get("quality.mutation").new_disposition, "selected");
  assert.equal(rows.get("schema.ddl").new_disposition, "selected");
});

test("analyzePullRequest: a docs-only diff is flagged and never a coverage gap", () => {
  const registry = registryFixture();
  const analysis = analyzePullRequest({
    registry,
    number: 104,
    headSHA: sha("7"),
    baseSHA: sha("8"),
    mergeSHA: sha("9"),
    mergedAt: "2026-07-04T00:00:00Z",
    changedPaths: ["docs/readme.md"],
    jobsByWorkflow: new Map(),
  });
  assert.equal(analysis.docsOnly, true);
  assert.deepEqual(analysis.coverageGaps, []);
});

test("analyzePullRequest: a truncated file list (API's 3000-file cap) treats paths as unknown", () => {
  const registry = registryFixture();
  const analysis = analyzePullRequest({
    registry,
    number: 105,
    headSHA: sha("a1"),
    baseSHA: sha("b1"),
    mergeSHA: sha("c1"),
    mergedAt: "2026-07-05T00:00:00Z",
    changedPaths: ["internal/schema/columns.go"],
    truncatedFiles: true,
    jobsByWorkflow: new Map(),
  });
  assert.equal(analysis.selectorConclusion, "fallback_full");
  assert.equal(analysis.fallbackReason, "unknown_paths");
});

test("analyzePullRequest: 'never' merge-posture lanes are outside merge-fence scope", () => {
  const registry = registryFixture();
  const analysis = analyzePullRequest({
    registry,
    number: 106,
    headSHA: sha("d1"),
    baseSHA: sha("e1"),
    mergeSHA: sha("f1"),
    mergedAt: "2026-07-06T00:00:00Z",
    changedPaths: ["internal/schema/columns.go"],
    jobsByWorkflow: new Map(),
  });
  assert.ok(!analysis.laneRows.some((row) => row.lane_id === "e2e.full"));
});

test("percentile: nearest-rank over sorted input, and null on an empty sample", () => {
  assert.equal(percentile([], 95), null);
  assert.equal(percentile([10], 50), 10);
  assert.equal(percentile([10], 95), 10);
  const sorted = [1, 2, 3, 4, 5, 6, 7, 8, 9, 10];
  assert.equal(percentile(sorted, 50), 5);
  assert.equal(percentile(sorted, 95), 10);
});

function baseAnalysis(overrides) {
  return {
    number: 1,
    headSHA: sha("1"),
    mergedAt: "2026-06-01T00:00:00Z",
    docsOnly: false,
    coverageGaps: [],
    selectedDurationMinutes: 10,
    wallClockMinutes: 5,
    ...overrides,
  };
}

test("aggregateBacktest: PASS when every target clears and there are zero gaps", () => {
  const analyses = [
    baseAnalysis({ number: 1, mergedAt: "2026-06-01T00:00:00Z", selectedDurationMinutes: 50, wallClockMinutes: 10 }),
    baseAnalysis({ number: 2, mergedAt: "2026-07-01T00:00:00Z", selectedDurationMinutes: 60, wallClockMinutes: 12 }),
  ];
  const report = aggregateBacktest(analyses, { minSHACount: 2 });
  assert.equal(report.verdict.sha_count_ok, true);
  assert.equal(report.verdict.required_ready_p95_ok, true);
  assert.equal(report.verdict.runner_p50_ok, true);
  assert.equal(report.verdict.runner_p95_ok, true);
  assert.equal(report.verdict.zero_old_only_catches, true);
  assert.equal(report.verdict.pass, true);
  assert.equal(report.docOnlyCount, 0);
  assert.ok(report.spanDays > 0);
});

test("aggregateBacktest: FAIL when a coverage gap exists, independent of performance", () => {
  const analyses = [
    baseAnalysis({
      number: 1,
      coverageGaps: [{ lane_id: "quality.mutation", new_reason: "not_impacted", observed_state: "red", observed_conclusion: "failure" }],
    }),
  ];
  const report = aggregateBacktest(analyses, { minSHACount: 1 });
  assert.equal(report.verdict.zero_old_only_catches, false);
  assert.equal(report.verdict.pass, false);
  assert.equal(report.coverageGaps.length, 1);
  assert.equal(report.coverageGaps[0].pr, 1);
});

test("aggregateBacktest: FAIL when runner-minutes p95 exceeds its target", () => {
  const analyses = Array.from({ length: 20 }, (_, i) =>
    baseAnalysis({ number: i, selectedDurationMinutes: 200, wallClockMinutes: 5 }),
  );
  const report = aggregateBacktest(analyses, { minSHACount: 20 });
  assert.equal(report.verdict.runner_p50_ok, false);
  assert.equal(report.verdict.runner_p95_ok, false);
  assert.equal(report.verdict.pass, false);
});

test("aggregateBacktest: FAIL when required-ready wall time p95 exceeds its target", () => {
  const analyses = Array.from({ length: 20 }, (_, i) =>
    baseAnalysis({ number: i, selectedDurationMinutes: 10, wallClockMinutes: 30 }),
  );
  const report = aggregateBacktest(analyses, { minSHACount: 20 });
  assert.equal(report.verdict.required_ready_p95_ok, false);
  assert.equal(report.verdict.pass, false);
});

test("aggregateBacktest: FAIL when fewer than the target non-documentation SHAs were found", () => {
  const analyses = [baseAnalysis({ number: 1 })];
  const report = aggregateBacktest(analyses, { minSHACount: 50 });
  assert.equal(report.verdict.sha_count_ok, false);
  assert.equal(report.verdict.pass, false);
  assert.equal(report.nonDocCount, 1);
});

test("aggregateBacktest: documentation-only SHAs neither count toward the target nor pollute the percentiles", () => {
  const analyses = [
    baseAnalysis({ number: 1, docsOnly: true, selectedDurationMinutes: 99999, wallClockMinutes: 99999 }),
    baseAnalysis({ number: 2, selectedDurationMinutes: 10, wallClockMinutes: 5 }),
  ];
  const report = aggregateBacktest(analyses, { minSHACount: 1 });
  assert.equal(report.nonDocCount, 1);
  assert.equal(report.docOnlyCount, 1);
  assert.equal(report.runnerP95, 10);
  assert.equal(report.requiredReadyP95, 5);
});

test("aggregateBacktest: a SHA with no measurable job timing is excluded from percentiles, not treated as zero", () => {
  const analyses = [
    baseAnalysis({ number: 1, selectedDurationMinutes: 0, wallClockMinutes: null }),
    baseAnalysis({ number: 2, selectedDurationMinutes: 10, wallClockMinutes: 5 }),
  ];
  const report = aggregateBacktest(analyses, { minSHACount: 2 });
  assert.equal(report.settledCount, 1);
  assert.equal(report.unsettledCount, 1);
  assert.equal(report.requiredReadyP50, 5);
});

test("aggregateBacktest: tallies the fallback rate and the directories driving unknown-path fallbacks", () => {
  const analyses = [
    baseAnalysis({
      number: 1,
      selectorConclusion: "fallback_full",
      unknownPaths: [".github/scripts/foo.mjs", ".github/scripts/bar.mjs"],
    }),
    baseAnalysis({
      number: 2,
      selectorConclusion: "fallback_full",
      unknownPaths: [".github/scripts/baz.mjs", ".github/workflows/qux.yml"],
    }),
    baseAnalysis({ number: 3, selectorConclusion: "success", unknownPaths: [] }),
  ];
  const report = aggregateBacktest(analyses, { minSHACount: 3 });
  assert.equal(report.fallbackFullCount, 2);
  assert.deepEqual(report.topUnknownPathPrefixes[0], { prefix: ".github/scripts", count: 3 });
  assert.deepEqual(report.topUnknownPathPrefixes[1], { prefix: ".github/workflows", count: 1 });
});

test("renderSummary: names every target, the overall verdict, and every coverage gap", () => {
  const analyses = [
    baseAnalysis({
      number: 42,
      headSHA: sha("a"),
      coverageGaps: [{ lane_id: "quality.mutation", new_reason: "not_impacted", observed_state: "red", observed_conclusion: "failure" }],
    }),
  ];
  const report = aggregateBacktest(analyses, { minSHACount: 1 });
  const summary = renderSummary(report);
  assert.match(summary, /Overall: FAIL/);
  assert.match(summary, /#42/);
  assert.match(summary, /quality\.mutation/);
  assert.match(summary, new RegExp(`p95 <= ${MERGE_P95_SLO_MINUTES}`));
  assert.match(summary, new RegExp(`p50 <= ${RUNNER_P50_TARGET_MINUTES}, p95 <= ${RUNNER_P95_TARGET_MINUTES}`));
});

test("DEFAULT_SHA_COUNT matches issue #2230's acceptance floor", () => {
  assert.equal(DEFAULT_SHA_COUNT, 50);
});
