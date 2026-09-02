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
  renderSummary,
  validateRegistry,
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
  // The head query-path packages a reference lane must seed on, so the
  // affected-path set derived from its globs reaches the shared pipeline
  // (#2902). These are real files because the coverage check asks whether any
  // file under the root matches a glob, not whether the directory exists.
  ["internal", "promql", "lower.go"],
  ["internal", "logql", "lower.go"],
  ["internal", "traceql", "lower.go"],
  ["internal", "api", "prom", "handler.go"],
  ["internal", "api", "loki", "handler.go"],
  ["internal", "api", "tempo", "handler.go"],
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
    timeout_minutes: timeoutMinutes,
    slo_minutes: sloMinutes,
    accountable_owner: "quality",
  };
}

function registryFixture() {
  return {
    schema_version: 1,
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
          "internal/api/loki/**",
          "internal/api/prom/**",
          "internal/api/tempo/**",
          "internal/logql/**",
          "internal/promql/**",
          "internal/traceql/**",
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

test("a reference lane that stops naming its own head query path is rejected", () => {
  // #2902's seed rule. The derived affected-path set is the dependency closure
  // of the packages a lane's globs name, so dropping the head's API package or
  // its query language narrows the lane back to seeing only its own harness —
  // which is exactly how PR #2824 moved the shared pipeline with no reference
  // lane considering itself touched. Both roots are controlled independently:
  // covering one must not excuse the other.
  for (const dropped of ["internal/api/loki/**", "internal/logql/**"]) {
    const narrowed = registryFixture();
    const provider = narrowed.lanes.find(
      (candidate) => candidate.id === "oracle-reference",
    );
    provider.package_globs = provider.package_globs.filter(
      (glob) => glob !== dropped,
    );
    expectContractError(
      () => validateRegistry(narrowed, { root }),
      new RegExp(
        `canonical head logql reference provider oracle-reference does not cover ` +
          `${dropped.replace("/**", "").replace(/\//g, "\\/")} in package_globs`,
      ),
    );
  }
});

test("the head query-path rule does not apply to an execution provider", () => {
  // An execution lane lowers and executes SQL without entering the HTTP head,
  // so requiring the API package of one would assert a dependency it has not
  // got. The `always` lane declares only `test/spec/<head>/**` and stays valid.
  const document = registryFixture();
  const execution = document.lanes.find((candidate) => candidate.id === "always");
  assert.ok(!execution.package_globs.some((glob) => glob.startsWith("internal/api/")));
  assert.equal(validateRegistry(document, { root }), document);
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
  delete document.schema_version;
  document.surprise = true;
  expectContractError(
    () => validateRegistry(document, { root }),
    /registry\.schema_version is required/,
    /registry\.surprise is unknown/,
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

test("summary reports registry cardinality", () => {
  const body = renderSummary(registryFixture());
  assert.match(body, /logical lanes: \*\*5\*\*/);
  assert.match(body, /release-required lanes represented: \*\*4\*\*/);
});
