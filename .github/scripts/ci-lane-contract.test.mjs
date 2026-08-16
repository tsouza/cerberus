// ci-lane-contract.test.mjs — dependency-free negative controls for the
// machine-readable test-fence contract. The suite deliberately constructs a
// tiny repository instead of trusting the production registry as its only
// fixture: a broken validator must not be able to make its own test data green.

import { after, test } from "node:test";
import assert from "node:assert/strict";
import {
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import {
  CANONICAL_HEAD_ORACLE_FLOORS,
  ContractError,
  MERGE_P95_SLO_MINUTES,
  RELEASE_QUALIFICATION_SLO_MINUTES,
  loadRegistry,
  parseExpectedRunAttempt,
  qualificationExpectations,
  renderSummary,
  validateJobResults,
  validateRegistry,
  validateReport,
  validateReportSet as validateReportSetContract,
  validateSelection,
} from "./ci-lane-contract.mjs";

const CANONICAL_LAYER_IDS = [
  "1",
  "2a",
  "2b",
  "3",
  "4",
  "5",
  "6a",
  "6b",
  "6c",
  "6d",
  "6e",
  "6f",
  "7",
  "7b",
  "8",
  "9",
  "10",
  "11",
  "12",
  "13",
  "14",
];

const SOURCE_SHA = "a".repeat(40);
const SOURCE_TREE = "b".repeat(40);
const OTHER_SHA = "c".repeat(40);
const CANDIDATE_DIGEST = `sha256:${"d".repeat(64)}`;
const OTHER_DIGEST = `sha256:${"e".repeat(64)}`;
const BASE_SHA = "f".repeat(40);
const RUN = Object.freeze({ id: "321", attempt: 2 });
const NON_SUCCESS_CONCLUSIONS = [
  "action_required",
  "cancelled",
  "failure",
  "neutral",
  "skipped",
  "stale",
  "startup_failure",
  "timed_out",
];

const root = mkdtempSync(join(tmpdir(), "cerberus-ci-lane-contract-"));
mkdirSync(join(root, ".github", "workflows"), { recursive: true });
writeFileSync(
  join(root, "Justfile"),
  [
    'TEST_VARIABLE := "this is not a recipe"',
    'set shell := ["bash", "-cu"]',
    "",
    "test:",
    "  @node fixture-tools/execution-oracle.mjs",
    "",
    "reference-oracle:",
    "  @./fixture-tools/reference-oracle",
    "",
    "e2e-down:",
    "  @true",
    "",
    // This exact dependency form exposed the production parser defect.
    "e2e-up: e2e-down",
    "  @true",
    "",
  ].join("\n"),
);
const workflowFixtures = {
  "ci.yml": [
    "name: ci",
    "jobs:",
    "  check:",
    "    steps:",
    "      - run: just test",
    "  lint:",
    "    steps:",
    "      - run: true",
    "  oracle-property:",
    "    steps:",
    "      - run: true",
    "  oracle-reference:",
    "    steps:",
    "      - run: ./fixture-tools/reference-oracle",
    "",
  ].join("\n"),
  "e2e.yml": [
    "name: e2e",
    "jobs:",
    "  shard:",
    "    steps:",
    "      - run: true",
    "  aggregate:",
    "    steps:",
    "      - run: true",
    "",
  ].join("\n"),
  "release.yml": [
    "name: release",
    "jobs:",
    "  CodeQL:",
    "    steps:",
    "      - run: true",
    "",
  ].join("\n"),
};
for (const [name, contents] of Object.entries(workflowFixtures)) {
  writeFileSync(join(root, ".github", "workflows", name), contents);
}
for (const directory of [
  "always",
  "docs",
  "impact",
  "oracle-property",
  "oracle-reference",
  "release",
]) {
  mkdirSync(join(root, directory), { recursive: true });
}
mkdirSync(join(root, "fixture-tools"), { recursive: true });
writeFileSync(join(root, "fixture-tools", "execution-oracle.mjs"), "");
writeFileSync(join(root, "fixture-tools", "reference-oracle"), "");
const canonicalSourceFiles = [
  ["test", "spec", "promql", "fixture.txtar"],
  ["test", "spec", "logql", "fixture.txtar"],
  ["test", "spec", "traceql", "fixture.txtar"],
  ["compatibility", "prometheus", "oracle.test"],
  ["compatibility", "loki", "oracle.test"],
  ["compatibility", "tempo", "oracle.test"],
];
for (const parts of canonicalSourceFiles) {
  mkdirSync(join(root, ...parts.slice(0, -1)), { recursive: true });
  writeFileSync(join(root, ...parts), "oracle fixture\n");
}
after(() => rmSync(root, { recursive: true, force: true }));

function lane({
  id,
  workflow,
  jobs,
  producerJobs = [],
  contextJob,
  executions = ["default"],
  layers = [],
  recipes = [],
  mergePosture,
  mainPosture,
  releasePosture,
  determinism = "deterministic",
  source = true,
  artifact = false,
  purpose = "correctness",
  oracleClass = "execution",
  riskDomains = [`${id}-risk`],
  substrate = "runner",
  timeoutMinutes = 30,
  sloMinutes = 20,
  command = `run-${id}`,
  packageGlobs = [`${id}/**`],
}) {
  return {
    id,
    description: `${id} lane`,
    owner: {
      workflow,
      jobs,
      producer_jobs: producerJobs,
      context_job: contextJob,
    },
    executions,
    context: {
      match: "exact",
      name: `${id}-context`,
      protected: id === "always",
    },
    purpose,
    layers,
    oracle_class: oracleClass,
    recipes,
    command,
    build_tags: [],
    package_globs: packageGlobs,
    substrate,
    risk_domains: riskDomains,
    merge_posture: mergePosture,
    main_posture: mainPosture,
    release_posture: releasePosture,
    determinism,
    applicability: { source, artifact },
    selector: { unknown_paths: "full", failure: "full" },
    timeout_minutes: timeoutMinutes,
    slo_minutes: sloMinutes,
    accountable_owner: "quality",
    report_schema_version: 1,
  };
}

function registryFixture() {
  return {
    schema_version: 1,
    selection_schema_version: 1,
    report_schema_version: 1,
    rollout: "shadow",
    impact_selection: { known_nonimpact_globs: ["**/*.md", "docs/**"] },
    layers: CANONICAL_LAYER_IDS.map((id) => ({ id, name: `Layer ${id}` })),
    lanes: [
      lane({
        id: "always",
        workflow: ".github/workflows/ci.yml",
        jobs: ["check", "lint"],
        contextJob: "check",
        layers: CANONICAL_LAYER_IDS,
        recipes: ["test"],
        command: "just test",
        packageGlobs: [
          "test/spec/logql/**",
          "test/spec/promql/**",
          "test/spec/traceql/**",
        ],
        riskDomains: ["logql", "promql", "traceql"],
        mergePosture: "always",
        mainPosture: "always",
        releasePosture: "required",
      }),
      lane({
        id: "impact",
        workflow: ".github/workflows/e2e.yml",
        jobs: ["shard", "aggregate"],
        producerJobs: ["shard"],
        contextJob: "aggregate",
        executions: ["shard-a"],
        recipes: ["e2e-up"],
        mergePosture: "impact",
        mainPosture: "coalesced",
        releasePosture: "advisory",
        determinism: "seeded",
        timeoutMinutes: 60,
      }),
      lane({
        id: "oracle-property",
        workflow: ".github/workflows/ci.yml",
        jobs: ["oracle-property"],
        contextJob: "oracle-property",
        layers: ["6a", "6b", "6c"],
        mergePosture: "never",
        mainPosture: "always",
        releasePosture: "required",
        oracleClass: "property",
        riskDomains: ["logql", "promql", "traceql"],
      }),
      lane({
        id: "oracle-reference",
        workflow: ".github/workflows/ci.yml",
        jobs: ["oracle-reference"],
        contextJob: "oracle-reference",
        layers: ["6a", "6b", "6c"],
        mergePosture: "never",
        mainPosture: "always",
        releasePosture: "required",
        oracleClass: "reference",
        recipes: ["reference-oracle"],
        command: "reference compatibility via just reference-oracle",
        packageGlobs: [
          "compatibility/loki/**",
          "compatibility/prometheus/**",
          "compatibility/tempo/**",
        ],
        riskDomains: ["logql", "promql", "traceql"],
      }),
      lane({
        id: "release",
        workflow: ".github/workflows/release.yml",
        // GitHub job/check IDs are case-sensitive; CodeQL is a live registry ID.
        jobs: ["CodeQL"],
        contextJob: "CodeQL",
        executions: ["artifact"],
        mergePosture: "never",
        mainPosture: "never",
        releasePosture: "required",
        source: false,
        artifact: true,
        purpose: "release",
        oracleClass: "packaging",
        timeoutMinutes: 180,
        sloMinutes: 120,
      }),
    ],
    non_lane_workflows: [],
  };
}

function mergeSelection({
  selectImpact = false,
  selectorConclusion = "success",
  changedPaths = selectImpact ? ["impact/change.go"] : ["docs/readme.md"],
  unknownPaths = [],
} = {}) {
  return {
    schema_version: 1,
    registry_schema_version: 1,
    report_schema_version: 1,
    posture: "merge",
    source: { sha: SOURCE_SHA, tree: SOURCE_TREE },
    candidate_digest: null,
    run: { ...RUN },
    selector: {
      conclusion: selectorConclusion,
      base_sha: BASE_SHA,
      head_sha: SOURCE_SHA,
      changed_paths: changedPaths,
      unknown_paths: unknownPaths,
    },
    lanes: [
      {
        lane_id: "always",
        disposition: "selected",
        executions: ["default"],
        reason: null,
      },
      selectImpact
        ? {
            lane_id: "impact",
            disposition: "selected",
            executions: ["shard-a"],
            reason: null,
          }
        : {
            lane_id: "impact",
            disposition: "omitted",
            executions: [],
            reason: "not_impacted",
          },
      {
        lane_id: "oracle-property",
        disposition: "omitted",
        executions: [],
        reason: "posture_excluded",
      },
      {
        lane_id: "oracle-reference",
        disposition: "omitted",
        executions: [],
        reason: "posture_excluded",
      },
      {
        lane_id: "release",
        disposition: "omitted",
        executions: [],
        reason: "posture_excluded",
      },
    ],
  };
}

function addNonDocumentationLane(
  registry,
  selection,
  { selected = false, reason = "docs_only" } = {},
) {
  const quickstart = lane({
    id: "quickstart",
    workflow: ".github/workflows/e2e.yml",
    jobs: ["quickstart"],
    contextJob: "quickstart",
    mergePosture: "non_documentation",
    mainPosture: "always",
    releasePosture: "required",
  });
  quickstart.package_globs = ["README.md", "runtime/**"];
  registry.lanes.push(quickstart);
  registry.lanes.sort((left, right) => left.id.localeCompare(right.id));
  selection.lanes.push({
    lane_id: "quickstart",
    disposition: selected ? "selected" : "omitted",
    executions: selected ? ["default"] : [],
    reason: selected ? null : reason,
  });
  selection.lanes.sort((left, right) =>
    left.lane_id.localeCompare(right.lane_id),
  );
}

function releaseSelection() {
  return {
    schema_version: 1,
    registry_schema_version: 1,
    report_schema_version: 1,
    posture: "release",
    source: { sha: SOURCE_SHA, tree: SOURCE_TREE },
    candidate_digest: CANDIDATE_DIGEST,
    run: { ...RUN },
    selector: {
      conclusion: "success",
      base_sha: null,
      head_sha: null,
      changed_paths: [],
      unknown_paths: [],
    },
    lanes: [
      {
        lane_id: "always",
        disposition: "selected",
        executions: ["default"],
        reason: null,
      },
      {
        lane_id: "impact",
        disposition: "omitted",
        executions: [],
        reason: "advisory",
      },
      {
        lane_id: "oracle-property",
        disposition: "selected",
        executions: ["default"],
        reason: null,
      },
      {
        lane_id: "oracle-reference",
        disposition: "selected",
        executions: ["default"],
        reason: null,
      },
      {
        lane_id: "release",
        disposition: "selected",
        executions: ["artifact"],
        reason: null,
      },
    ],
  };
}

function reportFor(registry, selection, laneID, executionID = "default") {
  const registered = registry.lanes.find(
    (candidate) => candidate.id === laneID,
  );
  const candidateArtifact =
    selection.posture === "release" && registered.applicability.artifact;
  return {
    schema_version: 1,
    registry_schema_version: 1,
    lane_id: laneID,
    execution_id: executionID,
    posture: selection.posture,
    source: { ...selection.source },
    candidate_digest: candidateArtifact ? selection.candidate_digest : null,
    run: { ...selection.run },
    producer: {
      workflow: registered.owner.workflow,
      job: registered.owner.context_job,
    },
    invocation: {
      mode: candidateArtifact ? "candidate_artifact" : "source_tree",
      recipe: registered.recipes[0] ?? null,
      command: registered.command,
      build_tags: [...registered.build_tags],
      selected_domains: [registered.risk_domains[0]],
    },
    evidence: {
      executed: 3,
      passed: 3,
      failed: 0,
      skipped: 0,
      duration_ms: 25,
      seed: registered.determinism === "seeded" ? "seed-42" : null,
      corpus_id: `${laneID}-corpus-v1`,
    },
    conclusion: "success",
  };
}

function jobResultsFor(registry, selection) {
  const selected = new Set(
    selection.lanes
      .filter((item) => item.disposition === "selected")
      .map((item) => item.lane_id),
  );
  const results = {};
  for (const registered of registry.lanes) {
    if (!selected.has(registered.id)) continue;
    for (const job of registered.owner.jobs) {
      results[`${registered.owner.workflow}#${job}`] = {
        conclusion: "success",
        run_id: selection.run.id,
        run_attempt: selection.run.attempt,
      };
    }
  }
  return results;
}

function qualificationExpected(selection) {
  return {
    posture: selection.posture,
    sha: selection.source.sha,
    tree: selection.source.tree,
    candidateDigest:
      selection.posture === "release" ? selection.candidate_digest : undefined,
    runID: selection.run.id,
    runAttempt: selection.run.attempt,
    baseSHA:
      selection.posture === "merge" ? selection.selector.base_sha : undefined,
    changedPaths:
      selection.posture === "merge"
        ? [...selection.selector.changed_paths]
        : undefined,
  };
}

function validateReportSet(args) {
  return validateReportSetContract({
    ...args,
    expected: args.expected ?? qualificationExpected(args.selection),
  });
}

function expectContractError(fn, ...patterns) {
  let caught;
  try {
    fn();
  } catch (error) {
    caught = error;
  }
  assert.ok(
    caught instanceof ContractError,
    `expected ContractError, got ${caught}`,
  );
  for (const pattern of patterns) assert.match(caught.message, pattern);
  return caught;
}

test("registry accepts dependency recipes, uppercase workflow job IDs, and valid paths", () => {
  const document = registryFixture();
  assert.equal(validateRegistry(document, { root }), document);

  writeFileSync(join(root, "registry.json"), `${JSON.stringify(document)}\n`);
  assert.deepEqual(loadRegistry("registry.json", { root }), document);
  assert.deepEqual(
    loadRegistry(join(root, "registry.json"), { root }),
    document,
  );
});

test("registry keeps independent execution, property, and reference oracles per query head", () => {
  assert.deepEqual(CANONICAL_HEAD_ORACLE_FLOORS, {
    promql: { layer: "6a", classes: ["execution", "property", "reference"] },
    logql: { layer: "6b", classes: ["execution", "property", "reference"] },
    traceql: { layer: "6c", classes: ["execution", "property", "reference"] },
  });

  const controls = [
    {
      mutate(document) {
        document.lanes = document.lanes.filter(
          (candidate) => candidate.id !== "oracle-reference",
        );
      },
    },
    {
      mutate(document) {
        document.lanes.find(
          (candidate) => candidate.id === "oracle-reference",
        ).oracle_class = "execution";
      },
    },
    {
      mutate(document) {
        const provider = document.lanes.find(
          (candidate) => candidate.id === "oracle-reference",
        );
        provider.risk_domains = provider.risk_domains.filter(
          (domain) => domain !== "promql",
        );
      },
    },
    {
      mutate(document) {
        const provider = document.lanes.find(
          (candidate) => candidate.id === "oracle-reference",
        );
        provider.layers = provider.layers.filter((layer) => layer !== "6a");
      },
    },
    {
      mutate(document) {
        document.lanes.find(
          (candidate) => candidate.id === "oracle-reference",
        ).release_posture = "advisory";
      },
    },
    {
      mutate(document) {
        document.lanes.find(
          (candidate) => candidate.id === "oracle-reference",
        ).main_posture = "never";
      },
    },
    {
      mutate(document) {
        document.lanes.find(
          (candidate) => candidate.id === "oracle-reference",
        ).applicability.source = false;
      },
    },
  ];
  for (const control of controls) {
    const document = registryFixture();
    control.mutate(document);
    expectContractError(
      () => validateRegistry(document, { root }),
      /canonical head promql layer 6a requires a source-applicable reference oracle/,
    );
  }

  const renamed = registryFixture();
  renamed.lanes.find(
    (candidate) => candidate.id === "oracle-reference",
  ).id = "oracle-upstream";
  const advisory = structuredClone(renamed.lanes[3]);
  advisory.id = "oracle-zshadow";
  advisory.release_posture = "advisory";
  renamed.lanes.splice(4, 0, advisory);
  assert.equal(validateRegistry(renamed, { root }), renamed);

  const coalesced = registryFixture();
  coalesced.lanes.find(
    (candidate) => candidate.id === "oracle-reference",
  ).main_posture = "coalesced";
  assert.equal(validateRegistry(coalesced, { root }), coalesced);
});

test("canonical providers fail when their workflow command is removed", () => {
  const workflow = join(root, ".github", "workflows", "ci.yml");
  const original = readFileSync(workflow, "utf8");
  const controls = [
    {
      command: "      - run: just test",
      pattern:
        /canonical head promql execution provider always has no command evidence in its declared workflow jobs/,
    },
    {
      command: "      - run: ./fixture-tools/reference-oracle",
      pattern:
        /canonical head promql reference provider oracle-reference has no command evidence in its declared workflow jobs/,
    },
  ];
  for (const control of controls) {
    const withoutCommand = original.replace(control.command, "      - run: true");
    assert.notEqual(withoutCommand, original);
    writeFileSync(workflow, withoutCommand);
    try {
      expectContractError(
        () => validateRegistry(registryFixture(), { root }),
        control.pattern,
      );
    } finally {
      writeFileSync(workflow, original);
    }
  }
});

test("canonical providers fail when their real oracle source is removed", () => {
  const controls = [
    {
      parts: ["test", "spec", "promql", "fixture.txtar"],
      pattern:
        /canonical head promql execution provider always has no real oracle\/test source under test\/spec\/promql covered by package_globs/,
    },
    {
      parts: ["compatibility", "prometheus", "oracle.test"],
      pattern:
        /canonical head promql reference provider oracle-reference has no real oracle\/test source under compatibility\/prometheus covered by package_globs/,
    },
  ];
  for (const control of controls) {
    const source = join(root, ...control.parts);
    const original = readFileSync(source, "utf8");
    rmSync(source);
    try {
      expectContractError(
        () => validateRegistry(registryFixture(), { root }),
        control.pattern,
      );
    } finally {
      writeFileSync(source, original);
    }
  }
});

test("registry rejects unknown and missing schema fields", () => {
  const document = registryFixture();
  delete document.rollout;
  document.surprise = true;
  expectContractError(
    () => validateRegistry(document, { root }),
    /registry\.rollout is required/,
    /registry\.surprise is unknown/,
  );
});

test("schema v1 rejects an unsafe enforced rollout state", () => {
  const document = registryFixture();
  document.rollout = "enforced";
  expectContractError(
    () => validateRegistry(document, { root }),
    /registry\.rollout must be shadow in schema v1/,
  );
});

test("registry rejects duplicate lane IDs", () => {
  const document = registryFixture();
  document.lanes.splice(1, 0, structuredClone(document.lanes[0]));
  expectContractError(
    () => validateRegistry(document, { root }),
    /id duplicates lane always/,
  );
});

test("registry rejects missing, absolute, and escaping workflow paths", () => {
  for (const [workflow, pattern] of [
    [".github/workflows/missing.yml", /does not exist/],
    ["/tmp/workflow.yml", /must be repository-relative/],
    ["../workflow.yml", /escapes the repository/],
  ]) {
    const document = registryFixture();
    document.lanes[1].owner.workflow = workflow;
    expectContractError(() => validateRegistry(document, { root }), pattern);
  }
});

test("registry rejects invalid enums and invalid workflow job IDs", () => {
  for (const [field, value, pattern] of [
    ["purpose", "maybe", /purpose must be one of/],
    ["oracle_class", "guess", /oracle_class must be one of/],
    ["substrate", "laptop", /substrate must be one of/],
  ]) {
    const document = registryFixture();
    document.lanes[0][field] = value;
    expectContractError(() => validateRegistry(document, { root }), pattern);
  }

  for (const job of ["1job", "job.name", "bad/job"]) {
    const invalidJob = registryFixture();
    invalidJob.lanes[0].owner.jobs[0] = job;
    expectContractError(
      () => validateRegistry(invalidJob, { root }),
      /owner\.jobs\[0\] has invalid value/,
    );
  }
});

test("registry producer jobs are unique owner jobs and exclude the terminal context", () => {
  const cases = [
    {
      producerJobs: ["shard", "shard"],
      pattern: /producer_jobs contains duplicate "shard"/,
    },
    {
      producerJobs: ["ghost"],
      pattern: /producer_jobs contains ghost, which is not in owner\.jobs/,
    },
    {
      producerJobs: ["aggregate"],
      pattern: /producer_jobs must exclude the context_job aggregate/,
    },
  ];
  for (const { producerJobs, pattern } of cases) {
    const document = registryFixture();
    document.lanes[1].owner.producer_jobs = producerJobs;
    expectContractError(() => validateRegistry(document, { root }), pattern);
  }
});

test("registry discovers and rejects an unclassified .yaml workflow", () => {
  const path = join(root, ".github", "workflows", "unclassified.yaml");
  writeFileSync(path, "name: unclassified\n");
  try {
    expectContractError(
      () => validateRegistry(registryFixture(), { root }),
      /\.github\/workflows\/unclassified\.yaml is neither owned by a lane nor classified as non-lane/,
    );
  } finally {
    rmSync(path, { force: true });
  }
});

test("registry rejects unsafe and stale package globs", () => {
  for (const [glob, pattern] of [
    ["../../outside/**", /not a normalized repository-relative path/],
    ["misspelled-dir/**", /stale static directory prefix misspelled-dir/],
    ["always\\**", /must use forward slashes/],
    ["/tmp/**", /must be repository-relative/],
    ["missing.file", /does not exist: missing\.file/],
  ]) {
    const document = registryFixture();
    document.lanes[0].package_globs = [glob];
    expectContractError(() => validateRegistry(document, { root }), pattern);
  }
});

test("Just assignments are not accepted as recipes", () => {
  const document = registryFixture();
  document.lanes[0].recipes = ["TEST_VARIABLE"];
  expectContractError(
    () => validateRegistry(document, { root }),
    /recipes names missing Just recipe TEST_VARIABLE/,
  );
});

test("registry enforces merge and release qualification SLO ceilings", () => {
  assert.equal(MERGE_P95_SLO_MINUTES, 20);
  assert.equal(RELEASE_QUALIFICATION_SLO_MINUTES, 120);

  const slowMerge = registryFixture();
  slowMerge.lanes[0].slo_minutes = MERGE_P95_SLO_MINUTES + 1;
  expectContractError(
    () => validateRegistry(slowMerge, { root }),
    /slo_minutes must be <= 20 for a merge lane/,
  );

  const slowRelease = registryFixture();
  const releaseLane = slowRelease.lanes.find(
    (candidate) => candidate.id === "release",
  );
  releaseLane.timeout_minutes = RELEASE_QUALIFICATION_SLO_MINUTES + 10;
  releaseLane.slo_minutes = RELEASE_QUALIFICATION_SLO_MINUTES + 1;
  expectContractError(
    () => validateRegistry(slowRelease, { root }),
    /slo_minutes must be <= 120 for a release-required lane/,
  );
});

test("selection accepts a legitimate merge-impact omission", () => {
  const registry = registryFixture();
  const selection = mergeSelection();
  assert.equal(validateSelection(selection, registry), selection);
});

test("non-documentation posture selects the canary even when another impact lane owns the path", () => {
  const registry = registryFixture();
  const omitted = mergeSelection({
    selectImpact: true,
    changedPaths: ["impact/change.go"],
  });
  addNonDocumentationLane(registry, omitted);
  expectContractError(
    () => validateSelection(omitted, registry),
    /selector success must select non-documentation lane quickstart/,
  );

  Object.assign(
    omitted.lanes.find((item) => item.lane_id === "quickstart"),
    {
      disposition: "selected",
      executions: ["default"],
      reason: null,
    },
  );
  assert.equal(validateSelection(omitted, registry), omitted);
});

test("non-documentation posture permits docs-only omission but direct contract docs still select", () => {
  const registry = registryFixture();
  const docsOnly = mergeSelection({ changedPaths: ["docs/readme.md"] });
  addNonDocumentationLane(registry, docsOnly);
  assert.equal(validateSelection(docsOnly, registry), docsOnly);

  const contractDoc = mergeSelection({ changedPaths: ["README.md"] });
  const contractRegistry = registryFixture();
  addNonDocumentationLane(contractRegistry, contractDoc);
  expectContractError(
    () => validateSelection(contractDoc, contractRegistry),
    /selector success must select non-documentation lane quickstart/,
  );

  docsOnly.lanes.find(
    (item) => item.lane_id === "quickstart",
  ).reason = "not_impacted";
  expectContractError(
    () => validateSelection(docsOnly, registry),
    /merge-non-documentation and may omit only as docs_only/,
  );
});

test("fallback-full includes the non-documentation canary", () => {
  const registry = registryFixture();
  const selection = mergeSelection({
    selectImpact: true,
    selectorConclusion: "fallback_full",
    changedPaths: ["unknown/file.txt"],
    unknownPaths: ["unknown/file.txt"],
  });
  addNonDocumentationLane(registry, selection);
  expectContractError(
    () => validateSelection(selection, registry),
    /fallback_full must select quickstart/,
  );
});

test("selection rejects unknown and missing schema fields", () => {
  const registry = registryFixture();
  const selection = mergeSelection();
  delete selection.run;
  selection.surprise = true;
  expectContractError(
    () => validateSelection(selection, registry),
    /selection\.run is required/,
    /selection\.surprise is unknown/,
  );
});

test("selector failure and unknown paths fail closed to fallback_full", () => {
  const registry = registryFixture();

  const selectorFailure = mergeSelection();
  selectorFailure.selector.conclusion = "failure";
  expectContractError(
    () => validateSelection(selectorFailure, registry),
    /selector\.conclusion must be one of success, fallback_full/,
  );

  const unknownWithoutFallback = mergeSelection({
    changedPaths: ["unknown/file.txt"],
    unknownPaths: ["unknown/file.txt"],
  });
  expectContractError(
    () => validateSelection(unknownWithoutFallback, registry),
    /empty or unmatched merge paths require selector conclusion fallback_full/,
  );

  const emptyWithoutFallback = mergeSelection({ changedPaths: [] });
  expectContractError(
    () => validateSelection(emptyWithoutFallback, registry),
    /empty or unmatched merge paths require selector conclusion fallback_full/,
  );

  const incompleteFallback = mergeSelection({
    selectorConclusion: "fallback_full",
    changedPaths: ["unknown/file.txt"],
    unknownPaths: ["unknown/file.txt"],
  });
  expectContractError(
    () => validateSelection(incompleteFallback, registry),
    /fallback_full must select impact/,
  );

  const fullFallback = mergeSelection({
    selectImpact: true,
    selectorConclusion: "fallback_full",
    changedPaths: ["unknown/file.txt"],
    unknownPaths: ["unknown/file.txt"],
  });
  assert.equal(validateSelection(fullFallback, registry), fullFallback);
});

test("merge impact routing is recomputed from trusted changed paths", () => {
  const registry = registryFixture();
  const omittedImpact = mergeSelection({ changedPaths: ["impact/change.go"] });
  expectContractError(
    () => validateSelection(omittedImpact, registry),
    /selector success must select impacted lane impact/,
  );

  const falseUnknownClaim = mergeSelection({
    selectImpact: true,
    selectorConclusion: "fallback_full",
    changedPaths: ["unknown/file.txt"],
  });
  expectContractError(
    () => validateSelection(falseUnknownClaim, registry),
    /unknown_paths must exactly equal computed unknown paths \["unknown\/file\.txt"\]/,
  );
});

test("selection rejects invented and missing execution roster entries", () => {
  const registry = registryFixture();
  const invented = mergeSelection({ selectImpact: true });
  invented.lanes[1].executions = ["invented"];
  expectContractError(
    () => validateSelection(invented, registry),
    /impact selected executions must exactly equal registry roster \["shard-a"\]/,
  );

  const missing = mergeSelection({ selectImpact: true });
  registry.lanes[1].executions = ["shard-a", "shard-b"];
  expectContractError(
    () => validateSelection(missing, registry),
    /impact selected executions must exactly equal registry roster \["shard-a","shard-b"\]/,
  );
});

test("selection rejects release-required omission", () => {
  const registry = registryFixture();
  const selection = releaseSelection();
  selection.lanes[selection.lanes.findIndex((lane) => lane.lane_id === "release")] = {
    lane_id: "release",
    disposition: "omitted",
    executions: [],
    reason: "posture_excluded",
  };
  expectContractError(
    () => validateSelection(selection, registry),
    /release is release-required and must be selected/,
  );
});

test("selection binds expected SHA, tree, digest, run ID, and run attempt", () => {
  const registry = registryFixture();
  const selection = releaseSelection();
  assert.equal(
    validateSelection(selection, registry, {
      posture: "release",
      sha: SOURCE_SHA,
      tree: SOURCE_TREE,
      candidateDigest: CANDIDATE_DIGEST,
      runID: RUN.id,
      runAttempt: RUN.attempt,
    }),
    selection,
  );
  expectContractError(
    () =>
      validateSelection(selection, registry, {
        sha: OTHER_SHA,
        tree: OTHER_SHA,
        candidateDigest: OTHER_DIGEST,
        runID: "999",
        runAttempt: 9,
      }),
    /selection\.source\.sha/,
    /selection\.source\.tree/,
    /selection\.candidate_digest/,
    /selection\.run\.id/,
    /selection\.run\.attempt/,
  );
});

test("merge selection binds trusted base SHA and changed paths", () => {
  const registry = registryFixture();
  const selection = mergeSelection();
  const expected = qualificationExpected(selection);
  assert.equal(validateSelection(selection, registry, expected), selection);

  expectContractError(
    () =>
      validateSelection(selection, registry, {
        ...expected,
        baseSHA: OTHER_SHA,
      }),
    /selection\.selector\.base_sha is .+, want/,
  );
  expectContractError(
    () =>
      validateSelection(selection, registry, {
        ...expected,
        changedPaths: ["impact/change.go"],
      }),
    /selection\.selector\.changed_paths does not match trusted expected paths/,
  );
});

test("report accepts valid deterministic, seeded, and artifact evidence", () => {
  const registry = registryFixture();
  const merge = mergeSelection({ selectImpact: true });
  const release = releaseSelection();
  assert.equal(
    validateReport(reportFor(registry, merge, "always"), registry).lane_id,
    "always",
  );
  assert.equal(
    validateReport(reportFor(registry, merge, "impact", "shard-a"), registry)
      .evidence.seed,
    "seed-42",
  );
  assert.equal(
    validateReport(
      reportFor(registry, release, "release", "artifact"),
      registry,
    ).candidate_digest,
    CANDIDATE_DIGEST,
  );
});

test("report rejects unknown, missing, and mismatched producer fields", () => {
  const registry = registryFixture();
  const selection = mergeSelection();
  const malformed = reportFor(registry, selection, "always");
  delete malformed.invocation;
  malformed.surprise = true;
  expectContractError(
    () => validateReport(malformed, registry),
    /report\.invocation is required/,
    /report\.surprise is unknown/,
  );

  const wrongProducer = reportFor(registry, selection, "always");
  wrongProducer.producer.job = "lint";
  expectContractError(
    () => validateReport(wrongProducer, registry),
    /producer\.job must be the registry context_job/,
  );
});

test("report rejects negative, noninteger, inconsistent, and dishonest evidence", () => {
  const registry = registryFixture();
  const selection = mergeSelection();
  const cases = [
    {
      mutate: (report) => {
        report.evidence.executed = -1;
      },
      pattern: /evidence\.executed must be an integer >= 0/,
    },
    {
      mutate: (report) => {
        report.evidence.duration_ms = 1.5;
      },
      pattern: /evidence\.duration_ms must be an integer >= 0/,
    },
    {
      mutate: (report) => {
        report.evidence.passed = 2;
      },
      pattern: /executed must equal passed \+ failed \+ skipped/,
    },
    {
      mutate: (report) => {
        report.evidence.passed = 2;
        report.evidence.failed = 1;
      },
      pattern: /successful report cannot carry failed evidence/,
    },
  ];
  for (const { mutate, pattern } of cases) {
    const report = reportFor(registry, selection, "always");
    mutate(report);
    expectContractError(() => validateReport(report, registry), pattern);
  }
});

test("trusted job-result schema rejects unknown, missing, invalid, and noninteger values", () => {
  const registry = registryFixture();
  const selection = mergeSelection();
  const valid = jobResultsFor(registry, selection);
  assert.equal(validateJobResults(valid, registry), valid);

  const unknown = structuredClone(valid);
  unknown[".github/workflows/ci.yml#ghost"] = {
    conclusion: "success",
    run_id: RUN.id,
    run_attempt: RUN.attempt,
  };
  expectContractError(
    () => validateJobResults(unknown, registry),
    /does not name a registered owner job/,
  );

  const missing = structuredClone(valid);
  delete missing[".github/workflows/ci.yml#check"].run_attempt;
  expectContractError(
    () => validateJobResults(missing, registry),
    /run_attempt is required/,
  );

  for (const value of [-1, 1.5]) {
    const invalid = structuredClone(valid);
    invalid[".github/workflows/ci.yml#check"].run_attempt = value;
    expectContractError(
      () => validateJobResults(invalid, registry),
      /run_attempt must be an integer >= 1/,
    );
  }

  const invalidConclusion = structuredClone(valid);
  invalidConclusion[".github/workflows/ci.yml#check"].conclusion = "running";
  expectContractError(
    () => validateJobResults(invalidConclusion, registry),
    /conclusion must be one of/,
  );
});

test("report set accepts total green evidence and legitimate impact omission", () => {
  const registry = registryFixture();
  const selection = mergeSelection();
  const report = reportFor(registry, selection, "always");
  assert.deepEqual(
    validateReportSet({
      registry,
      selection,
      reports: [report],
      jobResults: jobResultsFor(registry, selection),
    }),
    { expected: 1, received: 1 },
  );
});

test("report set rejects missing, unexpected, and duplicate reports", () => {
  const registry = registryFixture();
  const selection = mergeSelection();
  const report = reportFor(registry, selection, "always");
  const results = jobResultsFor(registry, selection);

  expectContractError(
    () =>
      validateReportSet({
        registry,
        selection,
        reports: [],
        jobResults: results,
      }),
    /missing report always\/default/,
  );

  const unexpected = reportFor(registry, selection, "impact", "shard-a");
  expectContractError(
    () =>
      validateReportSet({
        registry,
        selection,
        reports: [report, unexpected],
        jobResults: results,
      }),
    /unexpected report impact\/shard-a/,
  );

  expectContractError(
    () =>
      validateReportSet({
        registry,
        selection,
        reports: [report, structuredClone(report)],
        jobResults: results,
      }),
    /duplicate report always\/default/,
  );
});

test("report set rejects hollow, skipped, and every non-success outcome", () => {
  const registry = registryFixture();
  const selection = mergeSelection();
  const results = jobResultsFor(registry, selection);
  const cases = [
    {
      mutate: (report) => {
        report.evidence.executed = 0;
        report.evidence.passed = 0;
      },
      pattern: /zero tests\/checks executed/,
    },
    {
      mutate: (report) => {
        report.evidence.passed = 2;
        report.evidence.skipped = 1;
      },
      pattern: /evidence is not an all-executed, all-passed result/,
    },
  ];
  for (const { mutate, pattern } of cases) {
    const report = reportFor(registry, selection, "always");
    mutate(report);
    expectContractError(
      () =>
        validateReportSet({
          registry,
          selection,
          reports: [report],
          jobResults: results,
        }),
      pattern,
    );
  }
  for (const conclusion of NON_SUCCESS_CONCLUSIONS) {
    const report = reportFor(registry, selection, "always");
    report.conclusion = conclusion;
    expectContractError(
      () =>
        validateReportSet({
          registry,
          selection,
          reports: [report],
          jobResults: results,
        }),
      new RegExp(`conclusion ${conclusion} is not success`),
    );
  }
});

test("report set binds report SHA, tree, run ID, and run attempt to selection", () => {
  const registry = registryFixture();
  const selection = mergeSelection();
  const results = jobResultsFor(registry, selection);
  const cases = [
    {
      mutate: (report) => {
        report.source.sha = OTHER_SHA;
      },
      pattern: /source SHA\/tree does not match the selection/,
    },
    {
      mutate: (report) => {
        report.source.tree = OTHER_SHA;
      },
      pattern: /source SHA\/tree does not match the selection/,
    },
    {
      mutate: (report) => {
        report.run.id = "999";
      },
      pattern: /report run id does not match the selection/,
    },
    {
      mutate: (report) => {
        report.run.attempt = 9;
      },
      pattern: /report run attempt does not match the selection/,
    },
  ];
  for (const { mutate, pattern } of cases) {
    const report = reportFor(registry, selection, "always");
    mutate(report);
    expectContractError(
      () =>
        validateReportSet({
          registry,
          selection,
          reports: [report],
          jobResults: results,
        }),
      pattern,
    );
  }
});

test("report set rejects artifact digest mismatch", () => {
  const registry = registryFixture();
  const selection = releaseSelection();
  const reports = [
    reportFor(registry, selection, "always"),
    reportFor(registry, selection, "release", "artifact"),
  ];
  reports[1].candidate_digest = OTHER_DIGEST;
  expectContractError(
    () =>
      validateReportSet({
        registry,
        selection,
        reports,
        jobResults: jobResultsFor(registry, selection),
      }),
    /release\/artifact: candidate digest does not match the selection/,
  );
});

test("a stale report plus matching stale trusted results cannot qualify a new selection run", () => {
  // This is the critical negative control: before the run-binding checks, the
  // report and context job could agree with each other while both belonged to
  // an older workflow run than the selection.
  const registry = registryFixture();
  const selection = mergeSelection();
  const report = reportFor(registry, selection, "always");
  report.run = { id: "999", attempt: 9 };
  const results = jobResultsFor(registry, selection);
  for (const result of Object.values(results)) {
    result.run_id = "999";
    result.run_attempt = 9;
  }
  expectContractError(
    () =>
      validateReportSet({
        registry,
        selection,
        reports: [report],
        jobResults: results,
      }),
    /report run id does not match the selection/,
    /report run attempt does not match the selection/,
    /trusted result for \.github\/workflows\/ci\.yml#check does not match the selection run/,
    /trusted result for \.github\/workflows\/ci\.yml#lint does not match the selection run/,
  );
});

test("every trusted owner job binds the selection run, not only the context job", () => {
  const registry = registryFixture();
  const selection = mergeSelection();
  const report = reportFor(registry, selection, "always");

  for (const [field, value] of [
    ["run_id", "999"],
    ["run_attempt", 9],
  ]) {
    const results = jobResultsFor(registry, selection);
    results[".github/workflows/ci.yml#lint"][field] = value;
    expectContractError(
      () =>
        validateReportSet({
          registry,
          selection,
          reports: [report],
          jobResults: results,
        }),
      /trusted result for \.github\/workflows\/ci\.yml#lint does not match the selection run/,
    );
  }
});

test("report set rejects missing and non-success trusted owner jobs", () => {
  const registry = registryFixture();
  const selection = mergeSelection();
  const report = reportFor(registry, selection, "always");

  const missing = jobResultsFor(registry, selection);
  delete missing[".github/workflows/ci.yml#lint"];
  expectContractError(
    () =>
      validateReportSet({
        registry,
        selection,
        reports: [report],
        jobResults: missing,
      }),
    /trusted result for \.github\/workflows\/ci\.yml#lint is missing/,
  );

  for (const conclusion of NON_SUCCESS_CONCLUSIONS) {
    const failed = jobResultsFor(registry, selection);
    failed[".github/workflows/ci.yml#lint"].conclusion = conclusion;
    expectContractError(
      () =>
        validateReportSet({
          registry,
          selection,
          reports: [report],
          jobResults: failed,
        }),
      new RegExp(
        `trusted result for \\.github/workflows/ci\\.yml#lint is ${conclusion}`,
      ),
    );
  }
});

test("release report set qualifies all release-required source and artifact lanes", () => {
  const registry = registryFixture();
  const selection = releaseSelection();
  const reports = [
    reportFor(registry, selection, "always"),
    reportFor(registry, selection, "oracle-property"),
    reportFor(registry, selection, "oracle-reference"),
    reportFor(registry, selection, "release", "artifact"),
  ];
  assert.deepEqual(
    validateReportSet({
      registry,
      selection,
      reports,
      jobResults: jobResultsFor(registry, selection),
    }),
    { expected: 4, received: 4 },
  );
});

test("CI_EXPECT_RUN_ATTEMPT uses strict canonical integer parsing", () => {
  assert.equal(parseExpectedRunAttempt(undefined), undefined);
  assert.equal(parseExpectedRunAttempt("2"), 2);
  for (const value of ["", "0", "-1", "1.5", "1e2", "1junk"]) {
    expectContractError(
      () => parseExpectedRunAttempt(value),
      /CI_EXPECT_RUN_ATTEMPT must be a canonical positive integer/,
    );
  }
  expectContractError(
    () => parseExpectedRunAttempt("999999999999999999999"),
    /exceeds JavaScript's safe integer range/,
  );
});

test("reports-mode expectations wire strict run-attempt parsing into qualification", () => {
  const valid = qualificationExpectations({
    CI_LANE_POSTURE: "merge",
    CI_EXPECT_SHA: SOURCE_SHA,
    CI_EXPECT_TREE: SOURCE_TREE,
    CI_EXPECT_RUN_ID: RUN.id,
    CI_EXPECT_RUN_ATTEMPT: String(RUN.attempt),
    CI_EXPECT_BASE_SHA: BASE_SHA,
    CI_EXPECT_CHANGED_PATHS_JSON: "[]",
  });
  assert.deepEqual(valid, {
    posture: "merge",
    sha: SOURCE_SHA,
    tree: SOURCE_TREE,
    candidateDigest: undefined,
    runID: RUN.id,
    runAttempt: RUN.attempt,
    baseSHA: BASE_SHA,
    changedPaths: [],
  });
  expectContractError(
    () =>
      qualificationExpectations({
        CI_LANE_POSTURE: "merge",
        CI_EXPECT_SHA: SOURCE_SHA,
        CI_EXPECT_TREE: SOURCE_TREE,
        CI_EXPECT_RUN_ID: RUN.id,
        CI_EXPECT_RUN_ATTEMPT: String(RUN.attempt),
      }),
    /CI_EXPECT_BASE_SHA is required for merge qualification/,
    /CI_EXPECT_CHANGED_PATHS_JSON is required for merge qualification/,
  );
  expectContractError(
    () =>
      qualificationExpectations({
        CI_LANE_POSTURE: "release",
        CI_EXPECT_SHA: SOURCE_SHA,
        CI_EXPECT_TREE: SOURCE_TREE,
        CI_EXPECT_RUN_ID: RUN.id,
        CI_EXPECT_RUN_ATTEMPT: String(RUN.attempt),
      }),
    /CI_EXPECT_CANDIDATE_DIGEST is required for release qualification/,
  );
  expectContractError(
    () => qualificationExpectations({ CI_EXPECT_RUN_ATTEMPT: "2junk" }),
    /CI_EXPECT_RUN_ATTEMPT must be a canonical positive integer/,
  );
});

test("summary reports registry and qualification cardinality", () => {
  const body = renderSummary(registryFixture(), { expected: 4, received: 4 });
  assert.match(body, /logical lanes: \*\*5\*\*/);
  assert.match(body, /release-required lanes represented: \*\*4\*\*/);
  assert.match(body, /qualified reports: \*\*4\/4\*\*/);
});
