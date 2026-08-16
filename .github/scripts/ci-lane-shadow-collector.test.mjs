// Negative controls for the native Actions shadow collector. Network access is
// replaced by a URL-aware in-memory HTTP function in every test.

import assert from "node:assert/strict";
import {
  mkdtempSync,
  readFileSync,
  rmSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import { deriveQualificationCorrelationNonce } from "./ci-lane-contract.mjs";

import {
  bindSelectionToEvent,
  collectShadowObservation,
  jobObservationState,
  shadowObservationSettled,
  trustedEventIdentity,
  writeShadowObservation,
} from "./ci-lane-shadow-collector.mjs";

const sha = (character) => character.repeat(40);
const REPOSITORY = "example/project";
const BASE_SHA = sha("a");
const EVENT_HEAD_SHA = sha("b");
const PROJECTED_SHA = sha("c");
const PROJECTED_TREE = sha("d");
const COLLECTED_AT = "2026-08-16T12:00:00Z";

function lane({
  id,
  workflow,
  context,
  protectedContext = false,
  mergePosture = "impact",
  packageGlobs,
  match = "exact",
}) {
  return {
    id,
    owner: { workflow },
    executions: ["default"],
    context: {
      match,
      name: context,
      protected: protectedContext,
    },
    merge_posture: mergePosture,
    main_posture: "always",
    release_posture: "advisory",
    package_globs: packageGlobs,
    applicability: { source: true, artifact: false },
  };
}

function registryFixture() {
  return {
    schema_version: 1,
    selection_schema_version: 1,
    report_schema_version: 1,
    impact_selection: { known_nonimpact_globs: ["docs/**", "**/*.md"] },
    lanes: [
      lane({
        id: "current.protected",
        workflow: ".github/workflows/protected.yml",
        context: "protected-context",
        protectedContext: true,
        packageGlobs: ["deploy/**"],
      }),
      lane({
        id: "optional.omitted",
        workflow: ".github/workflows/optional.yml",
        context: "optional-context",
        packageGlobs: ["optional/**"],
      }),
      lane({
        id: "proposed.selected",
        workflow: ".github/workflows/selected.yml",
        context: "selected-context",
        packageGlobs: ["internal/**"],
      }),
    ],
  };
}

function selectionFixture({ sourceSHA = PROJECTED_SHA, baseSHA = BASE_SHA } = {}) {
  const source = { sha: sourceSHA, tree: PROJECTED_TREE };
  return {
    schema_version: 1,
    registry_schema_version: 1,
    report_schema_version: 1,
    posture: "merge",
    source,
    candidate_digest: null,
    correlation_nonce: deriveQualificationCorrelationNonce({
      posture: "merge",
      source,
    }),
    run: { id: "900", attempt: 2 },
    selector: {
      conclusion: "success",
      base_sha: baseSHA,
      head_sha: sourceSHA,
      changed_paths: ["internal/change.go"],
      unknown_paths: [],
    },
    lanes: [
      {
        lane_id: "current.protected",
        disposition: "omitted",
        executions: [],
        reason: "not_impacted",
      },
      {
        lane_id: "optional.omitted",
        disposition: "omitted",
        executions: [],
        reason: "not_impacted",
      },
      {
        lane_id: "proposed.selected",
        disposition: "selected",
        executions: ["default"],
        reason: null,
      },
    ],
  };
}

function pullRequestPayload() {
  return {
    repository: { full_name: REPOSITORY },
    pull_request: {
      number: 42,
      head: {
        sha: EVENT_HEAD_SHA,
        ref: "feature/native-evidence",
        repo: { full_name: "fork/project" },
      },
      base: {
        sha: BASE_SHA,
        ref: "main",
        repo: { full_name: REPOSITORY },
      },
    },
  };
}

function pullRequestIdentity() {
  return trustedEventIdentity({
    eventName: "pull_request",
    repository: REPOSITORY,
    projectedSHA: PROJECTED_SHA,
    payload: pullRequestPayload(),
  });
}

function workflowRun({
  id,
  workflow = ".github/workflows/selected.yml",
  status = "completed",
  conclusion = "success",
  createdAt = "2026-08-16T11:00:00Z",
  updatedAt = "2026-08-16T11:05:00Z",
  attempt = 1,
  repository = REPOSITORY,
  event = "pull_request",
  headSHA = EVENT_HEAD_SHA,
  headBranch = "feature/native-evidence",
  prNumber = 42,
  prHeadSHA = EVENT_HEAD_SHA,
  prBaseSHA = BASE_SHA,
  prHeadRef = "feature/native-evidence",
  prBaseRef = "main",
} = {}) {
  return {
    id,
    run_attempt: attempt,
    path: workflow,
    repository: { full_name: repository },
    event,
    head_sha: headSHA,
    head_branch: headBranch,
    status,
    conclusion,
    created_at: createdAt,
    updated_at: updatedAt,
    pull_requests: [
      {
        number: prNumber,
        head: { sha: prHeadSHA, ref: prHeadRef },
        base: { sha: prBaseSHA, ref: prBaseRef },
      },
    ],
  };
}

function workflowJob({
  id,
  name = "selected-context",
  status = "completed",
  conclusion = "success",
  startedAt = "2026-08-16T11:00:00Z",
  completedAt = "2026-08-16T11:02:30Z",
} = {}) {
  return {
    id,
    name,
    status,
    conclusion,
    started_at: startedAt,
    completed_at: completedAt,
  };
}

function mockGitHub({ runs, jobsByRun = {} }) {
  const calls = [];
  const requestJSON = async (urlText) => {
    calls.push(urlText);
    const url = new URL(urlText);
    const jobMatch = url.pathname.match(
      /\/actions\/runs\/([1-9][0-9]*)\/attempts\/([1-9][0-9]*)\/jobs$/,
    );
    if (jobMatch) {
      const key = `${jobMatch[1]}:${jobMatch[2]}`;
      const jobs = jobsByRun[key] ?? [];
      return { total_count: jobs.length, jobs };
    }
    assert.equal(url.pathname, "/repos/example/project/actions/runs");
    assert.equal(url.searchParams.get("event"), "pull_request");
    assert.equal(url.searchParams.get("head_sha"), EVENT_HEAD_SHA);
    return { total_count: runs.length, workflow_runs: runs };
  };
  return { requestJSON, calls };
}

async function collect({ runs, jobsByRun, registry, selection, identity } = {}) {
  const github = mockGitHub({ runs: runs ?? [], jobsByRun });
  const document = await collectShadowObservation({
    registry: registry ?? registryFixture(),
    selection: selection ?? selectionFixture(),
    identity: identity ?? pullRequestIdentity(),
    requestJSON: github.requestJSON,
    apiBase: "https://api.example.invalid",
    collectedAt: COLLECTED_AT,
  });
  return { document, calls: github.calls };
}

function observation(document, laneID) {
  return document.lane_observations.find(
    (candidate) => candidate.lane_id === laneID,
  );
}

test("PR collection binds REST head_sha to the PR head, not projected SHA", async () => {
  const run = workflowRun({ id: 101 });
  const { document, calls } = await collect({
    runs: [run],
    jobsByRun: { "101:1": [workflowJob({ id: 1001 })] },
  });

  assert.notEqual(PROJECTED_SHA, EVENT_HEAD_SHA);
  assert.equal(document.event.projected_sha, PROJECTED_SHA);
  assert.equal(document.event.event_head_sha, EVENT_HEAD_SHA);
  assert.equal(document.workflow_runs[0].head_sha, EVENT_HEAD_SHA);
  assert.equal(
    observation(document, "proposed.selected").state,
    "success",
  );
  assert.ok(
    calls.some((url) =>
      url.includes(`head_sha=${encodeURIComponent(EVENT_HEAD_SHA)}`),
    ),
  );

  const projectedRun = workflowRun({ id: 102, headSHA: PROJECTED_SHA });
  const wrong = await collect({
    runs: [projectedRun],
    jobsByRun: { "102:1": [workflowJob({ id: 1002 })] },
  });
  assert.equal(wrong.document.workflow_runs.length, 0);
  assert.equal(
    observation(wrong.document, "proposed.selected").reason,
    "workflow_run_missing",
  );
});

test("wrong repository, event, PR, head, or base cannot bind a run", async (t) => {
  const cases = [
    ["repository", { repository: "elsewhere/project" }],
    ["event", { event: "push" }],
    ["PR number", { prNumber: 43 }],
    ["event head", { headSHA: sha("e") }],
    ["associated head", { prHeadSHA: sha("e") }],
    ["associated base", { prBaseSHA: sha("e") }],
    ["head ref", { headBranch: "another-branch" }],
    ["associated base ref", { prBaseRef: "another-base" }],
  ];
  for (const [name, change] of cases) {
    await t.test(name, async () => {
      const run = workflowRun({ id: 200, ...change });
      const { document, calls } = await collect({
        runs: [run],
        jobsByRun: { "200:1": [workflowJob({ id: 2001 })] },
      });
      assert.equal(document.workflow_runs.length, 0);
      assert.equal(
        observation(document, "proposed.selected").state,
        "missing",
      );
      assert.equal(
        calls.some((url) => url.includes("/attempts/1/jobs")),
        false,
      );
    });
  }
});

test("newest run and attempt wins even when an older run is green", async () => {
  const oldGreen = workflowRun({
    id: 300,
    createdAt: "2026-08-16T10:00:00Z",
  });
  const newRed = workflowRun({
    id: 301,
    createdAt: "2026-08-16T11:00:00Z",
    conclusion: "failure",
  });
  const oldAttempt = workflowRun({
    id: 302,
    attempt: 1,
    createdAt: "2026-08-16T12:00:00Z",
  });
  const newAttempt = workflowRun({
    id: 302,
    attempt: 2,
    createdAt: "2026-08-16T12:00:00Z",
    conclusion: "failure",
  });
  const { document, calls } = await collect({
    runs: [oldGreen, newAttempt, newRed, oldAttempt],
    jobsByRun: {
      "300:1": [workflowJob({ id: 3001 })],
      "301:1": [workflowJob({ id: 3011, conclusion: "failure" })],
      "302:1": [workflowJob({ id: 3021 })],
      "302:2": [workflowJob({ id: 3022, conclusion: "failure" })],
    },
  });
  const selected = observation(document, "proposed.selected");
  assert.deepEqual(selected.run, { id: "302", attempt: 2 });
  assert.equal(selected.state, "red");
  assert.equal(selected.terminal_success, false);
  assert.deepEqual(
    document.workflow_runs.map((run) => `${run.run_id}:${run.run_attempt}`),
    ["302:2", "302:1", "301:1", "300:1"],
  );
  for (const ref of ["300:1", "301:1", "302:1", "302:2"]) {
    const [id, attempt] = ref.split(":");
    assert.ok(calls.some((url) => url.includes(`/runs/${id}/attempts/${attempt}/jobs`)));
  }
});

test("missing and duplicate stable contexts fail closed", async (t) => {
  const run = workflowRun({ id: 400 });
  await t.test("missing", async () => {
    const { document } = await collect({ runs: [run] });
    const selected = observation(document, "proposed.selected");
    assert.equal(selected.state, "missing");
    assert.equal(selected.reason, "context_missing");
    assert.equal(selected.terminal_success, false);
  });
  await t.test("duplicate", async () => {
    const { document } = await collect({
      runs: [run],
      jobsByRun: {
        "400:1": [
          workflowJob({ id: 4001 }),
          workflowJob({ id: 4002 }),
        ],
      },
    });
    const selected = observation(document, "proposed.selected");
    assert.equal(selected.state, "duplicate");
    assert.equal(selected.reason, "context_duplicate");
    assert.deepEqual(selected.job_ids, ["4001", "4002"]);
    assert.equal(selected.terminal_success, false);
  });
});

test("registered prefix contexts match suffix-bearing native job names", async () => {
  const registry = registryFixture();
  registry.lanes.find(
    (candidate) => candidate.id === "proposed.selected",
  ).context.match = "prefix";
  const run = workflowRun({ id: 450 });
  const { document } = await collect({
    registry,
    runs: [run],
    jobsByRun: {
      "450:1": [
        workflowJob({ id: 4501, name: "selected-context (shard-one)" }),
      ],
    },
  });
  assert.equal(
    observation(document, "proposed.selected").state,
    "success",
  );
});

test("observations cover selected union protected while optional runs remain telemetry", async () => {
  const selectedRun = workflowRun({ id: 500 });
  const protectedRun = workflowRun({
    id: 501,
    workflow: ".github/workflows/protected.yml",
  });
  const optionalRun = workflowRun({
    id: 502,
    workflow: ".github/workflows/optional.yml",
  });
  const { document } = await collect({
    runs: [optionalRun, selectedRun, protectedRun],
    jobsByRun: {
      "500:1": [workflowJob({ id: 5001 })],
      "501:1": [workflowJob({ id: 5011, name: "protected-context" })],
      "502:1": [workflowJob({ id: 5021, name: "optional-context" })],
    },
  });
  assert.deepEqual(
    document.workflow_runs.map((run) => run.workflow),
    [
      ".github/workflows/optional.yml",
      ".github/workflows/protected.yml",
      ".github/workflows/selected.yml",
    ],
  );
  assert.deepEqual(
    document.lane_observations.map((item) => item.lane_id),
    ["current.protected", "proposed.selected"],
  );
  assert.equal(
    observation(document, "current.protected").selection_disposition,
    "omitted",
  );
  assert.deepEqual(
    observation(document, "current.protected").observation_sources,
    ["protected"],
  );
  assert.deepEqual(
    observation(document, "proposed.selected").observation_sources,
    ["selected"],
  );
  assert.equal(shadowObservationSettled(document), true);
});

test("terminal states remain distinct and only success qualifies", () => {
  const cases = [
    ["success", "success", true],
    ["failure", "red", false],
    ["action_required", "red", false],
    ["skipped", "skipped", false],
    ["neutral", "neutral", false],
    ["cancelled", "cancelled", false],
    ["timed_out", "timed_out", false],
  ];
  for (const [conclusion, state, terminalSuccess] of cases) {
    const outcome = jobObservationState({
      status: "completed",
      conclusion,
    });
    assert.equal(outcome.state, state, conclusion);
    assert.equal(outcome.state === "success", terminalSuccess, conclusion);
  }
  assert.deepEqual(
    jobObservationState({ status: "in_progress", conclusion: null }),
    { state: "pending", reason: "job_not_terminal" },
  );
});

test("job rosters are canonical and durations come from API timestamps", async () => {
  const run = workflowRun({ id: 600 });
  const { document } = await collect({
    runs: [run],
    jobsByRun: {
      "600:1": [
        workflowJob({
          id: 6002,
          name: "supporting-job",
          startedAt: "2026-08-16T11:00:30Z",
          completedAt: "2026-08-16T11:01:00Z",
        }),
        workflowJob({ id: 6001 }),
      ],
    },
  });
  assert.deepEqual(
    document.workflow_runs[0].jobs.map((job) => job.id),
    ["6001", "6002"],
  );
  assert.equal(document.workflow_runs[0].jobs[0].duration_ms, 150_000);
  assert.equal(document.workflow_runs[0].jobs[1].duration_ms, 30_000);
  assert.equal(
    observation(document, "proposed.selected").duration_ms,
    150_000,
  );
});

test("synthetic reversed timestamps on skipped jobs normalize to zero", async () => {
  const run = workflowRun({ id: 650 });
  const completedAt = "2026-08-16T11:01:00Z";
  const { document } = await collect({
    runs: [run],
    jobsByRun: {
      "650:1": [
        workflowJob({
          id: 6501,
          conclusion: "skipped",
          startedAt: "2026-08-16T11:02:00Z",
          completedAt,
        }),
      ],
    },
  });
  const job = document.workflow_runs[0].jobs[0];
  assert.equal(job.started_at, completedAt);
  assert.equal(job.completed_at, completedAt);
  assert.equal(job.duration_ms, 0);
  assert.equal(job.conclusion, "skipped");
});

test("malformed or reversed job timestamps are rejected", async (t) => {
  const run = workflowRun({ id: 700 });
  for (const [name, timestamps] of [
    ["malformed", { startedAt: "not-a-timestamp" }],
    [
      "reversed",
      {
        startedAt: "2026-08-16T11:02:00Z",
        completedAt: "2026-08-16T11:01:00Z",
      },
    ],
  ]) {
    await t.test(name, async () => {
      await assert.rejects(
        collect({
          runs: [run],
          jobsByRun: {
            "700:1": [workflowJob({ id: 7001, ...timestamps })],
          },
        }),
        /workflow job (started_at has invalid value|completion precedes its start)/,
      );
    });
  }
});

test("selection binding rejects stale run identity and the wrong event base", () => {
  const registry = registryFixture();
  const identity = pullRequestIdentity();
  assert.equal(
    bindSelectionToEvent(selectionFixture(), registry, identity, {
      runID: "900",
      runAttempt: 2,
    }).source.sha,
    PROJECTED_SHA,
  );
  const replayed = selectionFixture();
  replayed.correlation_nonce = "f".repeat(64);
  assert.throws(
    () => bindSelectionToEvent(replayed, registry, identity),
    /correlation_nonce does not match its projected Git objects/,
  );
  assert.throws(
    () =>
      bindSelectionToEvent(selectionFixture(), registry, identity, {
        runID: "901",
        runAttempt: 2,
      }),
    /selection\.run\.id/,
  );
  assert.throws(
    () =>
      bindSelectionToEvent(
        selectionFixture({ baseSHA: sha("e") }),
        registry,
        identity,
      ),
    /selection\.selector\.base_sha/,
  );
});

test("merge groups bind projected head and queue ref without PR association", async () => {
  const mergeIdentity = trustedEventIdentity({
    eventName: "merge_group",
    repository: REPOSITORY,
    projectedSHA: EVENT_HEAD_SHA,
    payload: {
      repository: { full_name: REPOSITORY },
      merge_group: {
        head_sha: EVENT_HEAD_SHA,
        base_sha: BASE_SHA,
        head_ref: "refs/heads/gh-readonly-queue/main/pr-42",
        base_ref: "refs/heads/main",
      },
    },
  });
  const selection = selectionFixture({ sourceSHA: EVENT_HEAD_SHA });
  const mergeRun = workflowRun({
    id: 800,
    event: "merge_group",
    headBranch: "gh-readonly-queue/main/pr-42",
  });
  mergeRun.pull_requests = [];
  const mergeRequest = async (urlText) => {
    const url = new URL(urlText);
    if (url.pathname.endsWith("/jobs")) {
      return { total_count: 1, jobs: [workflowJob({ id: 8001 })] };
    }
    assert.equal(url.searchParams.get("event"), "merge_group");
    return { total_count: 1, workflow_runs: [mergeRun] };
  };
  const document = await collectShadowObservation({
    registry: registryFixture(),
    selection,
    identity: mergeIdentity,
    requestJSON: mergeRequest,
    apiBase: "https://api.example.invalid",
    collectedAt: COLLECTED_AT,
  });
  assert.equal(
    observation(document, "proposed.selected").state,
    "success",
  );
  assert.equal(document.event.pr_number, null);
});

test("manifest writing is canonical, atomic, and refuses stale output", (t) => {
  const root = mkdtempSync(join(tmpdir(), "ci-lane-shadow-write-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const output = join(root, "observation.json");
  const document = {
    schema_version: 1,
    workflow_runs: [],
    lane_observations: [],
  };
  const written = writeShadowObservation(output, document);
  assert.equal(written.body, `${JSON.stringify(document, null, 2)}\n`);
  assert.equal(readFileSync(output, "utf8"), written.body);
  assert.match(written.sha256, /^[0-9a-f]{64}$/);
  assert.throws(
    () => writeShadowObservation(output, document),
    /refusing stale reuse/,
  );
});
