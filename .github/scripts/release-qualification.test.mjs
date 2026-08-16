import assert from "node:assert/strict";
import { test } from "node:test";

import {
  nativeArtifactName,
  selectionManifestSHA256,
} from "./ci-lane-contract.mjs";
import {
  QualificationError,
  QUICKSTART_IDENTITY,
  RELEASE_QUALIFICATION_INVOCATIONS,
  RELEASE_QUALIFICATION_LIMIT_MS,
  createPlan,
  createReleaseSelection,
  evaluateQualification,
  releaseLaneRoster,
  validateAttestation,
} from "./release-qualification.mjs";

const SHA = "a".repeat(40);
const TREE = "b".repeat(40);
const CANDIDATE = `sha256:${"c".repeat(64)}`;
const IMAGE = `sha256:${"d".repeat(64)}`;
const NONCE = "e".repeat(64);
const REPOSITORY = "example/project";
const WORKFLOW = ".github/workflows/qualification-fixture.yml";
const SELECTION_RUN = Object.freeze({ id: "321", attempt: 2 });
const PRODUCER_RUN = Object.freeze({ id: "801", attempt: 3 });
const EVENT = Object.freeze({
  kind: "workflow_dispatch",
  pr_number: null,
  event_head_sha: SHA,
  event_base_sha: null,
  projected_sha: SHA,
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

function fixtureLane(
  id,
  ordinal,
  { artifact = false, seeded = false, advisory = false, prefix = false } = {},
) {
  const token = String(ordinal).padStart(2, "0");
  const job = `job_${token}`;
  return {
    id,
    executions: ["default"],
    owner: {
      workflow: WORKFLOW,
      jobs: [job],
      context_job: job,
    },
    context: {
      match: prefix ? "prefix" : "exact",
      name: `qualification-${token}`,
      protected: id === "e2e.quickstart",
    },
    recipes: ["fixture"],
    command: `run-${id}`,
    build_tags: [],
    risk_domains: ["release"],
    determinism: seeded ? "seeded" : "deterministic",
    release_posture: advisory ? "advisory" : "required",
    applicability: { source: true, artifact },
  };
}

function registryFixture() {
  const lanes = [fixtureLane("e2e.quickstart", 0)];
  for (let index = 0; index < 42; index += 1) {
    lanes.push(
      fixtureLane(`quality.lane${String(index).padStart(2, "0")}`, index + 1, {
        seeded: index === 7,
        advisory: index === 11,
      }),
    );
  }
  lanes.push(
    fixtureLane("release.migration", 43, { artifact: true, prefix: true }),
  );
  lanes.sort((left, right) => left.id.localeCompare(right.id));
  return {
    schema_version: 1,
    selection_schema_version: 1,
    report_schema_version: 2,
    lanes,
  };
}

function entryDigest(index) {
  return ((index % 15) + 1).toString(16).repeat(64);
}

function fixture() {
  const registry = registryFixture();
  const startedAtMs = 1_000_000;
  const selection = createReleaseSelection({
    registry,
    source: { sha: SHA, tree: TREE },
    candidateDigest: CANDIDATE,
    correlationNonce: NONCE,
    run: SELECTION_RUN,
  });
  const plan = createPlan({
    registry,
    selection,
    eventIdentity: EVENT,
    repository: REPOSITORY,
    startedAtMs,
  });
  const roster = releaseLaneRoster(registry);
  const artifact = {
    id: "901",
    name: nativeArtifactName(WORKFLOW, PRODUCER_RUN),
    sha256: "f".repeat(64),
    entries: roster.map((item, index) => ({
      lane_id: item.lane_id,
      execution_id: item.execution_id,
      invocation_mode: item.invocation_mode,
      sha256: entryDigest(index),
    })),
  };
  const jobs = registry.lanes.map((lane, index) => ({
    job: lane.owner.context_job,
    name:
      lane.context.match === "prefix"
        ? `${lane.context.name} / default`
        : lane.context.name,
    database_id: String(1001 + index),
    conclusion: "success",
  }));
  const reports = roster.map((item, index) => {
    const lane = registry.lanes.find((candidate) => candidate.id === item.lane_id);
    const nativeEntry = artifact.entries[index];
    return {
      schema_version: registry.report_schema_version,
      registry_schema_version: registry.schema_version,
      lane_id: item.lane_id,
      execution_id: item.execution_id,
      posture: "release",
      source: { sha: SHA, tree: TREE },
      candidate_digest:
        item.invocation_mode === "candidate_artifact" ? CANDIDATE : null,
      correlation_nonce: NONCE,
      selection_ref: {
        run: { ...SELECTION_RUN },
        manifest_sha256: selectionManifestSHA256(selection),
      },
      producer: {
        workflow: WORKFLOW,
        job: lane.owner.context_job,
        run: { ...PRODUCER_RUN },
        artifact: {
          id: artifact.id,
          name: artifact.name,
          sha256: artifact.sha256,
          entry: `${item.lane_id}/${item.execution_id}/${item.invocation_mode}`,
          entry_sha256: nativeEntry.sha256,
        },
      },
      invocation: {
        mode: item.invocation_mode,
        recipe: lane.recipes[0],
        command: lane.command,
        build_tags: [...lane.build_tags],
        selected_domains: [...lane.risk_domains],
      },
      evidence: {
        executed: 3,
        passed: 3,
        failed: 0,
        skipped: 0,
        duration_ms: 250,
        seed: lane.determinism === "seeded" ? "release-seed" : null,
        corpus_id: `${item.lane_id}/${item.execution_id}/${item.invocation_mode}`,
      },
      conclusion: "success",
    };
  });
  return {
    registry,
    selection,
    plan,
    reports,
    producerManifest: {
      schema_version: registry.report_schema_version,
      correlation_nonce: NONCE,
      selection_ref: {
        run: { ...SELECTION_RUN },
        manifest_sha256: selectionManifestSHA256(selection),
      },
      producers: [
        {
          workflow: WORKFLOW,
          run: { ...PRODUCER_RUN },
          source: { sha: SHA, tree: TREE },
          correlation_nonce: NONCE,
          event: { ...EVENT },
          repository: REPOSITORY,
          conclusion: "success",
          jobs,
          artifacts: [artifact],
        },
      ],
    },
    candidateManifest: {
      image: { digest: IMAGE },
      files: [
        {
          path: "app/image/index.json",
          size: 5,
          sha256: `sha256:${"1".repeat(64)}`,
        },
      ],
    },
    nowMs: startedAtMs + 2000,
  };
}

function evaluate(value) {
  return evaluateQualification({
    registry: value.registry,
    plan: value.plan,
    selection: value.selection,
    reports: value.reports,
    producerManifest: value.producerManifest,
    candidateManifest: value.candidateManifest,
    nowMs: value.nowMs,
  });
}

function expectQualificationError(fn, ...patterns) {
  let caught;
  try {
    fn();
  } catch (error) {
    caught = error;
  }
  assert.ok(
    caught instanceof QualificationError,
    `expected QualificationError, got ${caught}`,
  );
  for (const pattern of patterns) assert.match(caught.message, pattern);
  return caught;
}

function reportByIdentity(value, identity) {
  return value.reports.find(
    (report) =>
      `${report.lane_id}/${report.execution_id}/${report.invocation.mode}` ===
      identity,
  );
}

function expectedAttestation(value) {
  return {
    sha: SHA,
    tree: TREE,
    candidateDigest: CANDIDATE,
    imageDigest: IMAGE,
    correlationNonce: NONCE,
    selectionManifestSHA256: value.plan.selection_ref.manifest_sha256,
    eventIdentity: EVENT,
    repository: REPOSITORY,
  };
}

test("release selection expands 44 finite lanes into exactly 45 mode-bound invocations", () => {
  const value = fixture();
  const roster = releaseLaneRoster(value.registry);
  assert.equal(
    value.selection.lanes.filter((lane) => lane.disposition === "selected").length,
    44,
  );
  assert.equal(roster.length, RELEASE_QUALIFICATION_INVOCATIONS);
  assert(roster.some((item) => `${item.lane_id}/${item.execution_id}/${item.invocation_mode}` === QUICKSTART_IDENTITY));
  assert.deepEqual(
    roster
      .filter((item) => item.lane_id === "release.migration")
      .map((item) => item.invocation_mode),
    ["candidate_artifact", "source_tree"],
  );
  assert.equal(
    value.plan.selection_ref.manifest_sha256,
    selectionManifestSHA256(value.selection),
  );
});

test("post-publish and observational lanes stay outside the exact finite roster", () => {
  const value = fixture();
  value.registry.lanes.push({
    ...fixtureLane("monitor.public", 90, { artifact: true }),
    applicability: { source: false, artifact: true },
    release_posture: "post_publish",
  });
  value.registry.lanes.push({
    ...fixtureLane("observation.advisory", 91, { advisory: true }),
    determinism: "observational",
  });
  value.registry.lanes.sort((left, right) => left.id.localeCompare(right.id));
  const selection = createReleaseSelection({
    registry: value.registry,
    source: { sha: SHA, tree: TREE },
    candidateDigest: CANDIDATE,
    correlationNonce: NONCE,
    run: SELECTION_RUN,
  });
  assert.equal(releaseLaneRoster(value.registry).length, 45);
  assert.equal(
    selection.lanes.find((lane) => lane.lane_id === "monitor.public").reason,
    "post_publish",
  );
  assert.equal(
    selection.lanes.find((lane) => lane.lane_id === "observation.advisory").reason,
    "advisory",
  );
});

test("successful join pins candidate, selection, event, jobs, artifact entries, and quickstart", () => {
  const value = fixture();
  const attestation = evaluate(value);
  assert.equal(attestation.result, "success");
  assert.equal(attestation.candidate.digest, CANDIDATE);
  assert.equal(attestation.candidate.image_digest, IMAGE);
  assert.equal(attestation.qualification.invocation_count, 45);
  assert.deepEqual(attestation.qualification.selection_ref, value.plan.selection_ref);
  assert.deepEqual(attestation.qualification.event, EVENT);
  assert.equal(attestation.qualification.repository, REPOSITORY);
  assert.equal(attestation.lanes.length, 45);
  const quickstart = attestation.lanes.find(
    (lane) => lane.identity === QUICKSTART_IDENTITY,
  );
  assert(quickstart);
  assert.equal(quickstart.producer.job_name, "qualification-00");
  assert.equal(quickstart.producer.job_database_id, "1001");
  assert.equal(quickstart.report_artifact.name, nativeArtifactName(WORKFLOW, PRODUCER_RUN));
  assert.equal(quickstart.report_artifact.entry, QUICKSTART_IDENTITY);
  assert.equal(quickstart.report_artifact.entry_sha256, entryDigest(0));
  assert.equal(
    validateAttestation(attestation, expectedAttestation(value), value.registry),
    attestation,
  );
});

test("missing, duplicate, and mode-collapsed reports fail closed", () => {
  {
    const value = fixture();
    value.reports = value.reports.filter(
      (report) =>
        `${report.lane_id}/${report.execution_id}/${report.invocation.mode}` !==
        QUICKSTART_IDENTITY,
    );
    expectQualificationError(
      () => evaluate(value),
      /missing report e2e\.quickstart\/default\/source_tree/,
    );
  }
  {
    const value = fixture();
    value.reports.push(structuredClone(value.reports[0]));
    expectQualificationError(() => evaluate(value), /duplicate report/);
  }
  {
    const value = fixture();
    value.reports = value.reports.filter(
      (report) =>
        !(
          report.lane_id === "release.migration" &&
          report.invocation.mode === "candidate_artifact"
        ),
    );
    expectQualificationError(
      () => evaluate(value),
      /missing report release\.migration\/default\/candidate_artifact/,
    );
  }
  {
    const value = fixture();
    const artifact = reportByIdentity(
      value,
      "release.migration/default/candidate_artifact",
    );
    artifact.invocation.mode = "source_tree";
    expectQualificationError(
      () => evaluate(value),
      /source_tree report must not carry candidate_digest|duplicate report/,
    );
  }
});

test("trusted job display names, ambiguity, database IDs, and conclusions fail closed", () => {
  {
    const value = fixture();
    value.producerManifest.producers[0].jobs[0].name = "wrong-check";
    expectQualificationError(
      () => evaluate(value),
      /trusted context check name wrong-check does not match qualification-00/,
    );
  }
  {
    const value = fixture();
    const lane = value.registry.lanes.find(
      (candidate) => candidate.id === "release.migration",
    );
    value.producerManifest.producers[0].jobs.push({
      job: lane.owner.context_job,
      name: `${lane.context.name} / second`,
      database_id: "9999",
      conclusion: "success",
    });
    expectQualificationError(
      () => evaluate(value),
      /trusted context job job_43 is ambiguous/,
    );
  }
  {
    const value = fixture();
    value.producerManifest.producers[0].jobs[1].database_id =
      value.producerManifest.producers[0].jobs[0].database_id;
    expectQualificationError(() => evaluate(value), /database_id is duplicated/);
  }
  for (const conclusion of NON_SUCCESS_CONCLUSIONS) {
    const value = fixture();
    value.producerManifest.producers[0].jobs[0].conclusion = conclusion;
    expectQualificationError(
      () => evaluate(value),
      new RegExp(`trusted context job job_00 is ${conclusion}`),
    );
  }
});

test("selection digest, selection run, source, nonce, and candidate digest cannot drift", () => {
  const cases = [
    {
      mutate(value) {
        value.plan.selection_ref.manifest_sha256 = "0".repeat(64);
      },
      pattern: /selection manifest SHA-256 does not match/,
    },
    {
      mutate(value) {
        value.selection.run.attempt += 1;
      },
      pattern: /selection.*run\.attempt|selection run does not match/,
    },
    {
      mutate(value) {
        value.reports[0].source.tree = "0".repeat(40);
      },
      pattern: /source SHA\/tree does not match the selection/,
    },
    {
      mutate(value) {
        value.producerManifest.producers[0].source.sha = "0".repeat(40);
      },
      pattern: /projected SHA\/tree does not match the selection/,
    },
    {
      mutate(value) {
        value.reports[0].correlation_nonce = "0".repeat(64);
      },
      pattern: /correlation_nonce does not match the current qualification/,
    },
    {
      mutate(value) {
        value.producerManifest.correlation_nonce = "0".repeat(64);
        value.producerManifest.producers[0].correlation_nonce = "0".repeat(64);
      },
      pattern: /producer manifest correlation_nonce does not match/,
    },
    {
      mutate(value) {
        reportByIdentity(
          value,
          "release.migration/default/candidate_artifact",
        ).candidate_digest = `sha256:${"0".repeat(64)}`;
      },
      pattern: /candidate digest does not match the selection/,
    },
  ];
  for (const { mutate, pattern } of cases) {
    const value = fixture();
    mutate(value);
    expectQualificationError(() => evaluate(value), pattern);
  }
});

test("trusted event and repository identity cannot be substituted", () => {
  {
    const value = fixture();
    value.producerManifest.producers[0].event.kind = "push";
    expectQualificationError(
      () => evaluate(value),
      /trusted producer event identity does not match the qualification event/,
    );
  }
  {
    const value = fixture();
    value.producerManifest.producers[0].event.projected_sha = "0".repeat(40);
    expectQualificationError(
      () => evaluate(value),
      /trusted producer event identity does not match the qualification event/,
    );
  }
  {
    const value = fixture();
    value.producerManifest.producers[0].repository = "example/other";
    expectQualificationError(
      () => evaluate(value),
      /trusted producer repository does not match the qualification repository/,
    );
  }
});

test("producer run, artifact identity, bundle name, bundle digest, and entry digest are bound", () => {
  const cases = [
    {
      mutate(value) {
        value.reports[0].producer.run.id = "999";
        value.reports[0].producer.artifact.name = nativeArtifactName(
          WORKFLOW,
          value.reports[0].producer.run,
        );
      },
      pattern: /producer run identity does not match the trusted workflow run/,
    },
    {
      mutate(value) {
        value.reports[0].producer.artifact.id = "999";
      },
      pattern: /trusted artifact 999 is missing/,
    },
    {
      mutate(value) {
        value.reports[0].producer.artifact.name = "ci-native-wrong-801-3";
      },
      pattern: /producer\.artifact\.name must be/,
    },
    {
      mutate(value) {
        value.reports[0].producer.artifact.sha256 = "0".repeat(64);
      },
      pattern: /artifact name or SHA-256 does not match trusted API provenance/,
    },
    {
      mutate(value) {
        value.reports[0].producer.artifact.entry_sha256 = "0".repeat(64);
      },
      pattern: /entry SHA-256 does not match trusted provenance/,
    },
    {
      mutate(value) {
        value.producerManifest.producers[0].artifacts[0].entries.shift();
      },
      pattern: /trusted native bundle entry is missing/,
    },
  ];
  for (const { mutate, pattern } of cases) {
    const value = fixture();
    mutate(value);
    expectQualificationError(() => evaluate(value), pattern);
  }
});

test("zero, skipped, failed, and every non-success native report conclusion block", () => {
  for (const evidence of [
    { executed: 0, passed: 0, failed: 0, skipped: 0 },
    { executed: 3, passed: 2, failed: 0, skipped: 1 },
    { executed: 3, passed: 2, failed: 1, skipped: 0 },
  ]) {
    const value = fixture();
    Object.assign(value.reports[0].evidence, evidence);
    expectQualificationError(
      () => evaluate(value),
      /zero tests\/checks executed|all-executed, all-passed result|successful report cannot carry failed evidence/,
    );
  }
  for (const conclusion of NON_SUCCESS_CONCLUSIONS) {
    const value = fixture();
    value.reports[0].conclusion = conclusion;
    expectQualificationError(
      () => evaluate(value),
      new RegExp(`conclusion ${conclusion} is not success`),
    );
  }
});

test("stale selection, nonce, producer attempt, and artifact-entry replays block", () => {
  const cases = [
    {
      mutate(value) {
        value.reports[0].selection_ref.run.id = "999";
      },
      pattern: /selection_ref does not match the current selection/,
    },
    {
      mutate(value) {
        value.reports[0].selection_ref.manifest_sha256 = "0".repeat(64);
      },
      pattern: /selection_ref does not match the current selection/,
    },
    {
      mutate(value) {
        value.reports[0].correlation_nonce = "0".repeat(64);
      },
      pattern: /correlation_nonce does not match the current qualification/,
    },
    {
      mutate(value) {
        value.reports[0].producer.run.attempt = 2;
        value.reports[0].producer.artifact.name = nativeArtifactName(
          WORKFLOW,
          value.reports[0].producer.run,
        );
      },
      pattern: /producer run identity does not match the trusted workflow run/,
    },
    {
      mutate(value) {
        value.reports[0].producer.artifact.entry_sha256 = "0".repeat(64);
      },
      pattern: /entry SHA-256 does not match trusted provenance/,
    },
  ];
  for (const { mutate, pattern } of cases) {
    const value = fixture();
    mutate(value);
    expectQualificationError(() => evaluate(value), pattern);
  }
});

test("the hard qualification window rejects early and late joins", () => {
  {
    const value = fixture();
    value.nowMs = value.plan.started_at_ms - 1;
    expectQualificationError(
      () => evaluate(value),
      /completed before it started/,
    );
  }
  {
    const value = fixture();
    value.nowMs = value.plan.started_at_ms + RELEASE_QUALIFICATION_LIMIT_MS + 1;
    expectQualificationError(() => evaluate(value), /qualification timed out/);
  }
});

test("attestation verification rejects provenance, roster, candidate, and deadline tampering", () => {
  const value = fixture();
  const baseline = evaluate(value);
  const cases = [
    {
      mutate(attestation) {
        attestation.candidate.image_digest = `sha256:${"0".repeat(64)}`;
      },
      pattern: /candidate\.image_digest/,
    },
    {
      mutate(attestation) {
        attestation.qualification.selection_ref.manifest_sha256 = "0".repeat(64);
      },
      pattern: /selection_ref\.manifest_sha256/,
    },
    {
      mutate(attestation) {
        attestation.qualification.event.kind = "push";
      },
      pattern: /qualification event does not match expected event/,
    },
    {
      mutate(attestation) {
        attestation.qualification.repository = "example/other";
      },
      pattern: /qualification\.repository/,
    },
    {
      mutate(attestation) {
        attestation.qualification.completed_at_ms =
          attestation.qualification.deadline_at_ms + 1;
      },
      pattern: /completion is outside its qualification window/,
    },
    {
      mutate(attestation) {
        attestation.lanes.shift();
      },
      pattern: /exactly 45 identities|missing release lane|quickstart proof/,
    },
    {
      mutate(attestation) {
        attestation.lanes[0].producer.job_database_id = "bad";
      },
      pattern: /job_database_id has invalid value/,
    },
    {
      mutate(attestation) {
        attestation.lanes[0].report_artifact.entry_sha256 = "bad";
      },
      pattern: /entry_sha256 has invalid value/,
    },
    {
      mutate(attestation) {
        attestation.lanes[0].evidence.executed = 0;
        attestation.lanes[0].evidence.passed = 0;
      },
      pattern: /positive all-passed native result/,
    },
  ];
  for (const { mutate, pattern } of cases) {
    const attestation = structuredClone(baseline);
    mutate(attestation);
    expectQualificationError(
      () =>
        validateAttestation(
          attestation,
          expectedAttestation(value),
          value.registry,
        ),
      pattern,
    );
  }
});

test("finite registry growth fails until the explicit 45-invocation contract is reviewed", () => {
  const value = fixture();
  value.registry.lanes.push(fixtureLane("quality.newlane", 90));
  value.registry.lanes.sort((left, right) => left.id.localeCompare(right.id));
  expectQualificationError(
    () => releaseLaneRoster(value.registry),
    /must contain exactly 45 invocations; registry produced 46/,
  );
});
