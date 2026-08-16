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
  REPORT_SCHEMA_VERSION,
  RELEASE_QUALIFICATION_SLO_MINUTES,
  deriveQualificationCorrelationNonce,
  loadRegistry,
  nativeArtifactName,
  parseExpectedRunAttempt,
  qualificationExpectations,
  renderSummary,
  selectionManifestSHA256,
  validateProducerManifest,
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
const MERGE_CORRELATION_NONCE = deriveQualificationCorrelationNonce({
  posture: "merge",
  source: { sha: SOURCE_SHA, tree: SOURCE_TREE },
});
const RELEASE_CORRELATION_NONCE = "9".repeat(64);
const RUN = Object.freeze({ id: "321", attempt: 2 });
const REPOSITORY = "example/project";
const PRODUCER_RUNS = Object.freeze({
  ".github/workflows/ci.yml": Object.freeze({ id: "801", attempt: 3 }),
  ".github/workflows/e2e.yml": Object.freeze({ id: "802", attempt: 4 }),
  ".github/workflows/release.yml": Object.freeze({ id: "803", attempt: 5 }),
});
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
    report_schema_version: REPORT_SCHEMA_VERSION,
  };
}

function registryFixture() {
  return {
    schema_version: 1,
    selection_schema_version: 1,
    report_schema_version: REPORT_SCHEMA_VERSION,
    rollout: "shadow",
    impact_selection: { known_nonimpact_globs: ["**/*.md", "docs/**"] },
    native_evidence: {
      schema_version: 1,
      parts: [
        {
          id: "always-go",
          lane_id: "always",
          execution_id: "default",
          invocation_mode: "source_tree",
          producer_job: "lint",
          parser: "go-test-json-v1",
          entry: "go-test.json",
        },
        {
          id: "impact-tap",
          lane_id: "impact",
          execution_id: "shard-a",
          invocation_mode: "source_tree",
          producer_job: "shard",
          parser: "node-tap-v1",
          entry: "node.tap",
        },
      ],
    },
    layers: CANONICAL_LAYER_IDS.map((id) => ({ id, name: `Layer ${id}` })),
    lanes: [
      lane({
        id: "always",
        workflow: ".github/workflows/ci.yml",
        jobs: ["check", "lint"],
        producerJobs: ["lint"],
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
    report_schema_version: REPORT_SCHEMA_VERSION,
    posture: "merge",
    source: { sha: SOURCE_SHA, tree: SOURCE_TREE },
    candidate_digest: null,
    correlation_nonce: MERGE_CORRELATION_NONCE,
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
            reason:
              changedPaths.length > 0 &&
              changedPaths.every(
                (path) => path.endsWith(".md") || path.startsWith("docs/"),
              )
                ? "docs_only"
                : "not_impacted",
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
    report_schema_version: REPORT_SCHEMA_VERSION,
    posture: "release",
    source: { sha: SOURCE_SHA, tree: SOURCE_TREE },
    candidate_digest: CANDIDATE_DIGEST,
    correlation_nonce: RELEASE_CORRELATION_NONCE,
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

function eventIdentityFor(selection) {
  if (selection.posture === "merge") {
    return {
      kind: "pull_request",
      pr_number: 17,
      event_head_sha: OTHER_SHA,
      event_base_sha: BASE_SHA,
      projected_sha: selection.source.sha,
    };
  }
  return {
    kind: selection.posture === "main" ? "push" : "workflow_dispatch",
    pr_number: null,
    event_head_sha: selection.source.sha,
    event_base_sha: null,
    projected_sha: selection.source.sha,
  };
}

function producerRunFor(workflow) {
  return PRODUCER_RUNS[workflow];
}

function nativeArtifactFor(
  workflow,
  laneID,
  executionID,
  invocationMode,
  run,
) {
  const ordinal = {
    ".github/workflows/ci.yml": 901,
    ".github/workflows/e2e.yml": 902,
    ".github/workflows/release.yml": 903,
  }[workflow];
  const digestByte = {
    always: "1",
    impact: "2",
    release: "3",
    "oracle-property": "4",
    "oracle-reference": "5",
  }[laneID];
  return {
    id: String(ordinal),
    name: nativeArtifactName(workflow, run),
    sha256: String(ordinal % 10).repeat(64),
    entry: `${laneID}/${executionID}/${invocationMode}`,
    entry_sha256: digestByte.repeat(64),
  };
}

function selectionRefFor(selection) {
  return {
    run: { ...selection.run },
    manifest_sha256: selectionManifestSHA256(selection),
  };
}

function reportFor(
  registry,
  selection,
  laneID,
  executionID = "default",
  invocationMode,
) {
  const registered = registry.lanes.find(
    (candidate) => candidate.id === laneID,
  );
  const mode =
    invocationMode ??
    (selection.posture === "release" &&
    registered.applicability.artifact &&
    !registered.applicability.source
      ? "candidate_artifact"
      : "source_tree");
  const candidateArtifact = mode === "candidate_artifact";
  const producerRun = producerRunFor(registered.owner.workflow);
  return {
    schema_version: REPORT_SCHEMA_VERSION,
    registry_schema_version: 1,
    lane_id: laneID,
    execution_id: executionID,
    posture: selection.posture,
    source: { ...selection.source },
    candidate_digest: candidateArtifact ? selection.candidate_digest : null,
    correlation_nonce: selection.correlation_nonce,
    selection_ref: selectionRefFor(selection),
    producer: {
      workflow: registered.owner.workflow,
      job: registered.owner.context_job,
      run: { ...producerRun },
      artifact: nativeArtifactFor(
        registered.owner.workflow,
        laneID,
        executionID,
        mode,
        producerRun,
      ),
    },
    invocation: {
      mode,
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

function producerManifestFor(registry, selection) {
  const producers = new Map();
  for (const item of selection.lanes) {
    if (item.disposition !== "selected") continue;
    const registered = registry.lanes.find((lane) => lane.id === item.lane_id);
    const workflow = registered.owner.workflow;
    const run = producerRunFor(workflow);
    if (!producers.has(workflow)) {
      producers.set(workflow, {
        workflow,
        run: { ...run },
        source: { ...selection.source },
        correlation_nonce: selection.correlation_nonce,
        event: eventIdentityFor(selection),
        repository: REPOSITORY,
        conclusion: "success",
        jobs: [],
        artifacts: [],
      });
    }
    const producer = producers.get(workflow);
    const artifactMode = registered.applicability.source
      ? "source_tree"
      : "candidate_artifact";
    if (!producer.jobs.some((job) => job.job === registered.owner.context_job))
      producer.jobs.push({
        job: registered.owner.context_job,
        name: registered.context.name,
        database_id: String(701 + producer.jobs.length),
        conclusion: "success",
      });
    if (producer.artifacts.length === 0) {
      producer.artifacts.push({
        id: nativeArtifactFor(
          workflow,
          item.lane_id,
          item.executions[0],
          artifactMode,
          run,
        ).id,
        name: nativeArtifactName(workflow, run),
        sha256: nativeArtifactFor(
          workflow,
          item.lane_id,
          item.executions[0],
          artifactMode,
          run,
        ).sha256,
        entries: [],
      });
    }
    for (const execution of item.executions) {
      const modes = [];
      if (registered.applicability.source) modes.push("source_tree");
      if (selection.posture === "release" && registered.applicability.artifact) {
        modes.push("candidate_artifact");
      }
      for (const mode of modes) {
        const reference = nativeArtifactFor(
          workflow,
          item.lane_id,
          execution,
          mode,
          run,
        );
        producer.artifacts[0].entries.push({
          lane_id: item.lane_id,
          execution_id: execution,
          invocation_mode: mode,
          sha256: reference.entry_sha256,
        });
      }
    }
  }
  return {
    schema_version: REPORT_SCHEMA_VERSION,
    correlation_nonce: selection.correlation_nonce,
    selection_ref: selectionRefFor(selection),
    producers: [...producers.values()],
  };
}

function qualificationExpected(selection) {
  return {
    posture: selection.posture,
    sha: selection.source.sha,
    tree: selection.source.tree,
    correlationNonce: selection.correlation_nonce,
    candidateDigest:
      selection.posture === "release" ? selection.candidate_digest : undefined,
    runID: selection.run.id,
    runAttempt: selection.run.attempt,
    selectionManifestSHA256: selectionManifestSHA256(selection),
    eventIdentity: eventIdentityFor(selection),
    repository: REPOSITORY,
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

test("registry producer jobs are unique owner jobs with an exact native-part roster", () => {
  const cases = [
    {
      producerJobs: ["shard", "shard"],
      pattern: /producer_jobs contains duplicate "shard"/,
    },
    {
      producerJobs: ["ghost"],
      pattern: /producer_jobs contains ghost, which is not in owner\.jobs/,
    },
  ];
  for (const { producerJobs, pattern } of cases) {
    const document = registryFixture();
    document.lanes[1].owner.producer_jobs = producerJobs;
    expectContractError(() => validateRegistry(document, { root }), pattern);
  }

  const directContextProducer = registryFixture();
  directContextProducer.lanes[1].owner.producer_jobs = ["aggregate"];
  directContextProducer.native_evidence.parts[1].producer_job = "aggregate";
  assert.equal(
    validateRegistry(directContextProducer, { root }),
    directContextProducer,
  );
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

test("selection correlation is mandatory and bound to projected Git objects", () => {
  const registry = registryFixture();
  const missing = mergeSelection();
  delete missing.correlation_nonce;
  expectContractError(
    () => validateSelection(missing, registry),
    /selection\.correlation_nonce is required/,
  );

  const mismatched = mergeSelection();
  mismatched.correlation_nonce = "8".repeat(64);
  expectContractError(
    () => validateSelection(mismatched, registry),
    /merge selection\.correlation_nonce does not match its projected Git objects/,
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

test("trusted producer manifest rejects unknown, missing, duplicate, and invalid provenance", () => {
  const registry = registryFixture();
  const selection = mergeSelection();
  const valid = producerManifestFor(registry, selection);
  assert.equal(validateProducerManifest(valid, registry), valid);

  const unknown = structuredClone(valid);
  unknown.producers[0].workflow = ".github/workflows/ghost.yml";
  expectContractError(
    () => validateProducerManifest(unknown, registry),
    /workflow is not registered/,
  );

  const missing = structuredClone(valid);
  delete missing.producers[0].run.attempt;
  expectContractError(
    () => validateProducerManifest(missing, registry),
    /run\.attempt is required/,
  );

  for (const value of [-1, 1.5]) {
    const invalid = structuredClone(valid);
    invalid.producers[0].run.attempt = value;
    expectContractError(
      () => validateProducerManifest(invalid, registry),
      /run\.attempt must be an integer >= 1/,
    );
  }

  const invalidConclusion = structuredClone(valid);
  invalidConclusion.producers[0].conclusion = "running";
  expectContractError(
    () => validateProducerManifest(invalidConclusion, registry),
    /conclusion must be one of/,
  );

  const superseded = structuredClone(valid);
  superseded.producers.push(structuredClone(superseded.producers[0]));
  superseded.producers[1].run.attempt += 1;
  superseded.producers[1].conclusion = "failure";
  expectContractError(
    () => validateProducerManifest(superseded, registry),
    /collector must choose exactly one newest run\/attempt/,
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
      producerManifest: producerManifestFor(registry, selection),
    }),
    { expected: 1, received: 1 },
  );
});

test("report set rejects missing, unexpected, and duplicate reports", () => {
  const registry = registryFixture();
  const selection = mergeSelection();
  const report = reportFor(registry, selection, "always");
  const results = producerManifestFor(registry, selection);

  expectContractError(
    () =>
      validateReportSet({
        registry,
        selection,
        reports: [],
        producerManifest: results,
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
        producerManifest: results,
      }),
    /unexpected report impact\/shard-a/,
  );

  expectContractError(
    () =>
      validateReportSet({
        registry,
        selection,
        reports: [report, structuredClone(report)],
        producerManifest: results,
      }),
    /duplicate report always\/default/,
  );
});

test("report set rejects hollow, skipped, and every non-success outcome", () => {
  const registry = registryFixture();
  const selection = mergeSelection();
  const results = producerManifestFor(registry, selection);
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
          producerManifest: results,
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
          producerManifest: results,
        }),
      new RegExp(`conclusion ${conclusion} is not success`),
    );
  }
});

test("report set binds source and selection_ref while allowing an independent producer run", () => {
  const registry = registryFixture();
  const selection = mergeSelection();
  const results = producerManifestFor(registry, selection);
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
        report.selection_ref.run.id = "999";
      },
      pattern: /selection_ref does not match the current selection/,
    },
    {
      mutate: (report) => {
        report.selection_ref.run.attempt = 9;
      },
      pattern: /selection_ref does not match the current selection/,
    },
    {
      mutate: (report) => {
        report.selection_ref.manifest_sha256 = "9".repeat(64);
      },
      pattern: /selection_ref does not match the current selection/,
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
          producerManifest: results,
        }),
      pattern,
    );
  }

  assert.notEqual(reportFor(registry, selection, "always").producer.run.id, selection.run.id);
});

test("pull-request API head identity is distinct from projected checkout identity", () => {
  const registry = registryFixture();
  const selection = mergeSelection();
  const report = reportFor(registry, selection, "always");
  const manifest = producerManifestFor(registry, selection);
  assert.notEqual(
    manifest.producers[0].event.event_head_sha,
    manifest.producers[0].event.projected_sha,
  );
  assert.deepEqual(
    validateReportSet({
      registry,
      selection,
      reports: [report],
      producerManifest: manifest,
    }),
    { expected: 1, received: 1 },
  );

  const wrongPR = producerManifestFor(registry, selection);
  wrongPR.producers[0].event.pr_number += 1;
  expectContractError(
    () =>
      validateReportSet({
        registry,
        selection,
        reports: [report],
        producerManifest: wrongPR,
      }),
    /trusted producer event identity does not match the qualification event/,
  );

  const wrongRepository = producerManifestFor(registry, selection);
  wrongRepository.producers[0].repository = "example/other";
  expectContractError(
    () =>
      validateReportSet({
        registry,
        selection,
        reports: [report],
        producerManifest: wrongRepository,
      }),
    /trusted producer repository does not match the qualification repository/,
  );

  const headAsCheckout = producerManifestFor(registry, selection);
  headAsCheckout.producers[0].source.sha =
    headAsCheckout.producers[0].event.event_head_sha;
  expectContractError(
    () =>
      validateReportSet({
        registry,
        selection,
        reports: [report],
        producerManifest: headAsCheckout,
      }),
    /trusted producer projected SHA\/tree does not match the selection/,
  );
});

test("merge-group API head must equal the projected checkout", () => {
  const registry = registryFixture();
  const selection = mergeSelection();
  const manifest = producerManifestFor(registry, selection);
  manifest.producers[0].event = {
    kind: "merge_group",
    pr_number: null,
    event_head_sha: OTHER_SHA,
    event_base_sha: BASE_SHA,
    projected_sha: SOURCE_SHA,
  };
  expectContractError(
    () => validateProducerManifest(manifest, registry),
    /event_head_sha must equal projected_sha for merge_group/,
  );
});

test("report set binds the exact selection manifest digest", () => {
  const registry = registryFixture();
  const selection = mergeSelection();
  const expected = qualificationExpected(selection);
  expected.selectionManifestSHA256 = "9".repeat(64);
  expectContractError(
    () =>
      validateReportSet({
        registry,
        selection,
        reports: [reportFor(registry, selection, "always")],
        producerManifest: producerManifestFor(registry, selection),
        expected,
      }),
    /selection manifest SHA-256 does not match the trusted expected digest/,
  );
});

test("report set binds artifact ID, attempt-qualified name, and SHA-256", () => {
  const registry = registryFixture();
  const selection = mergeSelection();
  const report = reportFor(registry, selection, "always");

  const missing = producerManifestFor(registry, selection);
  missing.producers[0].artifacts = [];
  expectContractError(
    () =>
      validateReportSet({
        registry,
        selection,
        reports: [report],
        producerManifest: missing,
      }),
    /artifacts must contain exactly one native bundle/,
  );

  const wrongDigest = producerManifestFor(registry, selection);
  wrongDigest.producers[0].artifacts[0].sha256 = "9".repeat(64);
  expectContractError(
    () =>
      validateReportSet({
        registry,
        selection,
        reports: [report],
        producerManifest: wrongDigest,
      }),
    /artifact name or SHA-256 does not match trusted API provenance/,
  );

  const priorAttempt = reportFor(registry, selection, "always");
  priorAttempt.producer.artifact.name = nativeArtifactName(
    priorAttempt.producer.workflow,
    {
      id: priorAttempt.producer.run.id,
      attempt: priorAttempt.producer.run.attempt - 1,
    },
  );
  expectContractError(
    () => validateReport(priorAttempt, registry),
    /producer\.artifact\.name must be ci-native-ci-801-3/,
  );

  const duplicate = producerManifestFor(registry, selection);
  duplicate.producers[0].artifacts.push(
    structuredClone(duplicate.producers[0].artifacts[0]),
  );
  expectContractError(
    () => validateProducerManifest(duplicate, registry),
    /artifacts must contain exactly one native bundle/,
  );
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
        producerManifest: producerManifestFor(registry, selection),
      }),
    /release\/artifact\/candidate_artifact: candidate digest does not match the selection/,
  );
});

test("a stale report plus matching stale manifest cannot qualify a new selection", () => {
  const registry = registryFixture();
  const selection = mergeSelection();
  const report = reportFor(registry, selection, "always");
  report.selection_ref = {
    run: { id: "999", attempt: 9 },
    manifest_sha256: "9".repeat(64),
  };
  const manifest = producerManifestFor(registry, selection);
  manifest.selection_ref = structuredClone(report.selection_ref);
  expectContractError(
    () =>
      validateReportSet({
        registry,
        selection,
        reports: [report],
        producerManifest: manifest,
      }),
    /producer manifest selection_ref does not match the current selection/,
    /selection_ref does not match the current selection/,
  );
});

test("release evidence from another qualification nonce cannot be replayed", () => {
  const registry = registryFixture();
  const prior = releaseSelection();
  const report = reportFor(registry, prior, "always");
  const manifest = producerManifestFor(registry, prior);

  const current = releaseSelection();
  current.correlation_nonce = "7".repeat(64);
  const currentRef = selectionRefFor(current);
  report.selection_ref = structuredClone(currentRef);
  manifest.selection_ref = structuredClone(currentRef);

  expectContractError(
    () =>
      validateReportSet({
        registry,
        selection: current,
        reports: [report],
        producerManifest: manifest,
      }),
    /producer manifest correlation_nonce does not match the current qualification/,
    /correlation_nonce does not match the current qualification/,
  );
});

test("report producer run must match API provenance, not the selection run", () => {
  const registry = registryFixture();
  const selection = mergeSelection();
  const report = reportFor(registry, selection, "always");
  for (const [field, value] of [
    ["id", "999"],
    ["attempt", 9],
  ]) {
    const manifest = producerManifestFor(registry, selection);
    report.producer.run[field] = value;
    report.producer.artifact.name = nativeArtifactName(
      report.producer.workflow,
      report.producer.run,
    );
    expectContractError(
      () =>
        validateReportSet({
          registry,
          selection,
          reports: [report],
          producerManifest: manifest,
        }),
      /producer run identity does not match the trusted workflow run/,
    );
  }
});

test("report set rejects missing and non-success trusted context jobs", () => {
  const registry = registryFixture();
  const selection = mergeSelection();
  const report = reportFor(registry, selection, "always");

  const missing = producerManifestFor(registry, selection);
  missing.producers[0].jobs = [];
  expectContractError(
    () =>
      validateReportSet({
        registry,
        selection,
        reports: [report],
        producerManifest: missing,
      }),
    /jobs must be a non-empty array/,
  );

  const wrongName = producerManifestFor(registry, selection);
  wrongName.producers[0].jobs[0].name = "another-check";
  expectContractError(
    () =>
      validateReportSet({
        registry,
        selection,
        reports: [report],
        producerManifest: wrongName,
      }),
    /trusted context check name another-check does not match always-context/,
  );

  for (const conclusion of NON_SUCCESS_CONCLUSIONS) {
    const failed = producerManifestFor(registry, selection);
    failed.producers[0].jobs[0].conclusion = conclusion;
    expectContractError(
      () =>
        validateReportSet({
          registry,
          selection,
          reports: [report],
          producerManifest: failed,
        }),
      new RegExp(`trusted context job check is ${conclusion}`),
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
      producerManifest: producerManifestFor(registry, selection),
    }),
    { expected: 4, received: 4 },
  );
});

test("canonical pre-publication qualification validates exactly 45 invocations", () => {
  const registry = JSON.parse(readFileSync(".github/ci-lanes.json", "utf8"));
  const selection = {
    schema_version: registry.selection_schema_version,
    registry_schema_version: registry.schema_version,
    report_schema_version: registry.report_schema_version,
    posture: "release",
    source: { sha: SOURCE_SHA, tree: SOURCE_TREE },
    candidate_digest: CANDIDATE_DIGEST,
    correlation_nonce: RELEASE_CORRELATION_NONCE,
    run: { ...RUN },
    selector: {
      conclusion: "success",
      base_sha: null,
      head_sha: null,
      changed_paths: [],
      unknown_paths: [],
    },
    lanes: registry.lanes.map((registered) => {
      if (registered.release_posture === "post_publish") {
        return {
          lane_id: registered.id,
          disposition: "omitted",
          executions: [],
          reason: "post_publish",
        };
      }
      if (registered.determinism === "observational") {
        return {
          lane_id: registered.id,
          disposition: "omitted",
          executions: [],
          reason: "advisory",
        };
      }
      return {
        lane_id: registered.id,
        disposition: "selected",
        executions: [...registered.executions],
        reason: null,
      };
    }),
  };
  const selected = selection.lanes.filter(
    (item) => item.disposition === "selected",
  );
  const workflows = [
    ...new Set(
      selected.map(
        (item) =>
          registry.lanes.find((lane) => lane.id === item.lane_id).owner
            .workflow,
      ),
    ),
  ].sort();
  const producerRuns = new Map(
    workflows.map((workflow, index) => [
      workflow,
      { id: String(5001 + index), attempt: 1 },
    ]),
  );
  const producers = new Map(
    workflows.map((workflow, index) => {
      const run = producerRuns.get(workflow);
      const digestByte = ((index % 15) + 1).toString(16);
      return [
        workflow,
        {
          workflow,
          run,
          source: { ...selection.source },
          correlation_nonce: selection.correlation_nonce,
          event: eventIdentityFor(selection),
          repository: REPOSITORY,
          conclusion: "success",
          jobs: [],
          artifacts: [
            {
              id: String(6001 + index),
              name: nativeArtifactName(workflow, run),
              sha256: digestByte.repeat(64),
              entries: [],
            },
          ],
        },
      ];
    }),
  );
  const reports = [];
  for (const item of selected) {
    const registered = registry.lanes.find(
      (lane) => lane.id === item.lane_id,
    );
    const producer = producers.get(registered.owner.workflow);
    if (
      !producer.jobs.some(
        (job) =>
          job.job === registered.owner.context_job &&
          job.name === registered.context.name,
      )
    ) {
      producer.jobs.push({
        job: registered.owner.context_job,
        name: registered.context.name,
        database_id: String(
          700001 +
            workflows.indexOf(registered.owner.workflow) * 100 +
            producer.jobs.length,
        ),
        conclusion: "success",
      });
    }
    const modes = [];
    if (registered.applicability.source) modes.push("source_tree");
    if (registered.applicability.artifact) {
      modes.push("candidate_artifact");
    }
    for (const executionID of item.executions) {
      for (const mode of modes) {
        const entrySHA256 = ((reports.length % 15) + 1)
          .toString(16)
          .repeat(64);
        producer.artifacts[0].entries.push({
          lane_id: registered.id,
          execution_id: executionID,
          invocation_mode: mode,
          sha256: entrySHA256,
        });
        reports.push({
          schema_version: registry.report_schema_version,
          registry_schema_version: registry.schema_version,
          lane_id: registered.id,
          execution_id: executionID,
          posture: selection.posture,
          source: { ...selection.source },
          candidate_digest:
            mode === "candidate_artifact"
              ? selection.candidate_digest
              : null,
          correlation_nonce: selection.correlation_nonce,
          selection_ref: selectionRefFor(selection),
          producer: {
            workflow: registered.owner.workflow,
            job: registered.owner.context_job,
            run: { ...producer.run },
            artifact: {
              id: producer.artifacts[0].id,
              name: producer.artifacts[0].name,
              sha256: producer.artifacts[0].sha256,
              entry: `${registered.id}/${executionID}/${mode}`,
              entry_sha256: entrySHA256,
            },
          },
          invocation: {
            mode,
            recipe: registered.recipes[0] ?? null,
            command: registered.command,
            build_tags: [...registered.build_tags],
            selected_domains: [registered.risk_domains[0]],
          },
          evidence: {
            executed: 1,
            passed: 1,
            failed: 0,
            skipped: 0,
            duration_ms: 1,
            seed:
              registered.determinism === "seeded"
                ? "canonical-release-seed"
                : null,
            corpus_id: `canonical-${registered.id}-${executionID}-${mode}`,
          },
          conclusion: "success",
        });
      }
    }
  }
  const producerManifest = {
    schema_version: registry.report_schema_version,
    correlation_nonce: selection.correlation_nonce,
    selection_ref: selectionRefFor(selection),
    producers: [...producers.values()],
  };

  assert.equal(selected.length, 44);
  assert.equal(reports.length, 45);
  assert.deepEqual(
    validateReportSet({
      registry,
      selection,
      reports,
      producerManifest,
    }),
    { expected: 45, received: 45 },
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
  const selection = mergeSelection({ changedPaths: [] });
  const valid = qualificationExpectations({
    CI_LANE_POSTURE: "merge",
    CI_EXPECT_SHA: SOURCE_SHA,
    CI_EXPECT_TREE: SOURCE_TREE,
    CI_EXPECT_CORRELATION_NONCE: selection.correlation_nonce,
    CI_EXPECT_RUN_ID: RUN.id,
    CI_EXPECT_RUN_ATTEMPT: String(RUN.attempt),
    CI_EXPECT_SELECTION_SHA256: selectionManifestSHA256(selection),
    CI_EXPECT_EVENT_KIND: "pull_request",
    CI_EXPECT_PR_NUMBER: "17",
    CI_EXPECT_EVENT_HEAD_SHA: OTHER_SHA,
    CI_EXPECT_EVENT_BASE_SHA: BASE_SHA,
    CI_EXPECT_PROJECTED_SHA: SOURCE_SHA,
    CI_EXPECT_REPOSITORY: REPOSITORY,
    CI_EXPECT_BASE_SHA: BASE_SHA,
    CI_EXPECT_CHANGED_PATHS_JSON: "[]",
  });
  assert.deepEqual(valid, {
    posture: "merge",
    sha: SOURCE_SHA,
    tree: SOURCE_TREE,
    correlationNonce: selection.correlation_nonce,
    candidateDigest: undefined,
    runID: RUN.id,
    runAttempt: RUN.attempt,
    selectionManifestSHA256: selectionManifestSHA256(selection),
    eventIdentity: {
      kind: "pull_request",
      pr_number: 17,
      event_head_sha: OTHER_SHA,
      event_base_sha: BASE_SHA,
      projected_sha: SOURCE_SHA,
    },
    repository: REPOSITORY,
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
