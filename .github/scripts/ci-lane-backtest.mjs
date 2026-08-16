// ci-lane-backtest.mjs — retrospective backtest of the merge-fence lane
// selector (ci-lane-selection.mjs) against real historical pull-request CI
// data, in place of the literal 7-day / 50-SHA live shadow soak issue #2230's
// acceptance checklist asks for.
//
// Why a backtest stands in for the soak: the selector is a pure function of
// (base SHA, head SHA, registry) -> lane-selection manifest. It needs no live
// checkout to evaluate — `selectionPolicy()` only needs the changed-path set,
// which GitHub's own compare view already computed for every historical PR.
// The Actions REST API keeps run/job history far longer than 7 days (22,958+
// completed runs observed on `main` on 2026-08-16), so replaying the new
// selector against N historical PRs and diffing its hypothetical choices
// against what ACTUALLY ran is strictly more evidence than waiting for N new
// PRs to trickle in, over a wider time span than 7 days.
//
// What this script checks, per issue #2230's acceptance line:
//   "require p95 <=20 minutes, runner p50 <=95 and p95 <=160 minutes, all
//    historical signatures caught, and no old-only catches."
//   - required-ready wall time: for each PR, the span from the earliest start
//     to the latest completion among the lane jobs the NEW selector would
//     have selected (a parallel-execution proxy — see renderSummary()'s
//     header note on the assumption this makes).
//   - runner-minutes: the sum of actual job durations for the NEW selector's
//     selected lanes (the real GitHub-billed cost the new system would have
//     spent).
//   - "historical signatures caught" / "no old-only catches" are the same
//     computational check from two angles: a lane the OLD (always-run) system
//     observed failing (red or timed_out) on a PR whose diff the NEW selector
//     would have omitted that lane for is a coverage gap. Zero such gaps means
//     both phrasings hold.
//
// What this script deliberately does NOT check (see the PR description for
// the follow-up): whether the new artifact-bundling / evidence-collection
// plumbing (ci-lane-shadow-collector.mjs, ci-lane-native-evidence.mjs)
// actually works end-to-end inside a live GitHub Actions run — permissions,
// cross-job artifact download, etc. That needs one live run; a backtest
// cannot exercise the runner sandbox.
//
// This script intentionally does NOT reuse ci-lane-shadow-collector.mjs's
// discoverWorkflowRuns()/hydrateWorkflowRuns(): those bind a live event's
// trusted pull_requests[] association for security (a spoofed workflow_run
// must not be trusted as evidence for THIS PR). A retrospective replay has no
// live event to spoof — the (workflow path, event=pull_request, head SHA)
// tuple already uniquely identifies the historical run, so this script uses a
// simpler, more lenient fetch. It does reuse contextMatches() and
// jobObservationState() from that module so "which job backs this lane" and
// "was this job green" can never drift between the live collector and this
// backtest.
//
// Required environment:
//   GITHUB_REPOSITORY   owner/repo to backtest (default: parsed from the
//                        local `git remote get-url origin`).
//
// Optional environment:
//   CI_LANE_BACKTEST_SHA_COUNT     minimum non-documentation final SHAs to
//                                  analyze (default 50; the issue's floor).
//   CI_LANE_BACKTEST_MAX_PRS       hard cap on merged PRs walked while
//                                  looking for CI_LANE_BACKTEST_SHA_COUNT
//                                  non-documentation SHAs (default 20x the
//                                  target, so a doc-heavy stretch can't spin
//                                  forever).
//   CI_LANE_BACKTEST_OUTPUT        JSON report path (default
//                                  build/ci-lane-backtest/report.json).
//   CI_LANE_REGISTRY               registry path (default .github/ci-lanes.json).
//   GITHUB_API_URL                 API origin (default https://api.github.com).
//   GITHUB_TOKEN                   token with `actions: read` + `pull-requests:
//                                  read`. Falls back to `gh auth token` when
//                                  unset, for interactive local runs.
//
// Node builtins only (plus the `gh` CLI as a token fallback, invoked once,
// never for data).

import { createHash } from "node:crypto";
import { existsSync, mkdirSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";

import {
  ContractError,
  DEFAULT_REGISTRY_PATH,
  MERGE_P95_SLO_MINUTES,
  loadRegistry,
  matchesGlob,
} from "./ci-lane-contract.mjs";
import { selectionPolicy } from "./ci-lane-selection.mjs";
import {
  contextMatches,
  jobObservationState,
} from "./ci-lane-shadow-collector.mjs";
import { capture, error, notice, warning } from "./lib/gh.mjs";

export const DEFAULT_SHA_COUNT = 50;
export const DEFAULT_MAX_PRS_MULTIPLIER = 20;
export const DEFAULT_API_URL = "https://api.github.com";
export const DEFAULT_OUTPUT_PATH = "build/ci-lane-backtest/report.json";

// The issue's own targets. MERGE_P95_SLO_MINUTES (20) is the registry's own
// constant, reused rather than duplicated. The runner-minute targets have no
// other home in the source tree; they are the literal numbers issue #2230's
// acceptance checklist states.
export const RUNNER_P50_TARGET_MINUTES = 95;
export const RUNNER_P95_TARGET_MINUTES = 160;

const FAILING_STATES = new Set(["red", "timed_out"]);

function contract(problem) {
  throw new ContractError("CI lane backtest", [problem]);
}

// --- pure comparison / aggregation logic (unit-tested with fixtures) ------

// Nearest-rank percentile over an ALREADY-sorted-ascending array of numbers.
// Chosen over interpolation because it never invents a value the sample
// didn't produce, which matters when reporting a real PR's minutes as "the
// p95".
export function percentile(sortedAscending, p) {
  if (sortedAscending.length === 0) return null;
  const rank = Math.ceil((p / 100) * sortedAscending.length);
  const index = Math.min(Math.max(rank - 1, 0), sortedAscending.length - 1);
  return sortedAscending[index];
}

// The raw Actions REST job object carries `started_at`/`completed_at` but no
// `duration_ms` field — this repository's ci-lane-shadow-collector.mjs
// derives it (see normalizeJob() there); this backtest replay derives it the
// same way rather than trusting an absent field as zero cost. A skipped job
// with a reversed timestamp pair (documented Actions API shape for jobs
// skipped before runner allocation) canonicalizes to zero duration, matching
// the live collector.
function jobDurationMS(job) {
  if (typeof job.started_at !== "string" || typeof job.completed_at !== "string") {
    return 0;
  }
  const delta = Date.parse(job.completed_at) - Date.parse(job.started_at);
  // Clamp rather than throw: a skipped-before-allocation job's reversed pair
  // is a documented API shape, not a data error worth failing an analytics
  // replay over (unlike the live collector, which must fail closed on it).
  return Number.isFinite(delta) && delta > 0 ? delta : 0;
}

function isDocsOnly(registry, changedPaths) {
  const globs = registry.impact_selection?.known_nonimpact_globs ?? [];
  return (
    changedPaths.length > 0 &&
    changedPaths.every((path) => globs.some((glob) => matchesGlob(path, glob)))
  );
}

// jobsByWorkflow: Map<workflowPath, Array<{name,status,conclusion,
//   started_at,completed_at,duration_ms}>> — every job this PR's final-SHA
// workflow runs reported, across every lane-owning workflow that ran.
export function analyzePullRequest({
  registry,
  number,
  headSHA,
  baseSHA,
  mergeSHA,
  mergedAt,
  changedPaths,
  truncatedFiles = false,
  jobsByWorkflow,
}) {
  const policy = selectionPolicy(
    registry,
    changedPaths,
    truncatedFiles ? "unknown_paths" : "none",
  );
  const laneByID = new Map(policy.lanes.map((l) => [l.lane_id, l]));

  const laneRows = [];
  for (const lane of registry.lanes) {
    if (lane.merge_posture === "never") continue; // outside merge-fence scope
    const laneSelection = laneByID.get(lane.id);
    const jobs = jobsByWorkflow.get(lane.owner.workflow) ?? [];
    const matches = jobs.filter((job) => contextMatches(lane.context, job.name));
    let state = "missing";
    let conclusion = null;
    let durationMS = 0;
    let startedAt = null;
    let completedAt = null;
    if (matches.length === 1) {
      const [job] = matches;
      if (job.status === "completed") {
        state = jobObservationState(job).state;
        conclusion = job.conclusion;
        durationMS = typeof job.duration_ms === "number" ? job.duration_ms : jobDurationMS(job);
        startedAt = job.started_at;
        completedAt = job.completed_at;
      } else {
        state = "pending";
      }
    } else if (matches.length > 1) {
      state = "duplicate";
    }
    laneRows.push({
      lane_id: lane.id,
      merge_posture: lane.merge_posture,
      workflow: lane.owner.workflow,
      new_disposition: laneSelection.disposition,
      new_reason: laneSelection.reason,
      observed_state: state,
      observed_conclusion: conclusion,
      observed_duration_ms: durationMS,
      observed_started_at: startedAt,
      observed_completed_at: completedAt,
    });
  }

  const coverageGaps = laneRows
    .filter(
      (row) =>
        row.new_disposition === "omitted" && FAILING_STATES.has(row.observed_state),
    )
    .map((row) => ({
      lane_id: row.lane_id,
      new_reason: row.new_reason,
      observed_state: row.observed_state,
      observed_conclusion: row.observed_conclusion,
    }));

  const selectedRows = laneRows.filter((row) => row.new_disposition === "selected");
  const selectedDurationMinutes =
    selectedRows.reduce((sum, row) => sum + row.observed_duration_ms, 0) / 60000;

  const starts = selectedRows
    .map((row) => row.observed_started_at)
    .filter((value) => typeof value === "string")
    .map((value) => Date.parse(value));
  const completions = selectedRows
    .map((row) => row.observed_completed_at)
    .filter((value) => typeof value === "string")
    .map((value) => Date.parse(value));
  const wallClockMinutes =
    starts.length > 0 && completions.length > 0
      ? (Math.max(...completions) - Math.min(...starts)) / 60000
      : null;

  return {
    number,
    headSHA,
    baseSHA,
    mergeSHA,
    mergedAt,
    docsOnly: isDocsOnly(registry, changedPaths),
    selectorConclusion: policy.conclusion,
    fallbackReason: policy.fallbackReason,
    changedPathCount: changedPaths.length,
    // Paths that matched no conditional lane's package_globs and no
    // known_nonimpact_glob — the reason a "none" fallbackReason PR can still
    // fall back (selectionPolicy promotes an empty unknownPaths list's
    // absence to "unknown_paths"). Surfaced so a registry glob-coverage gap
    // (a real path class no conditional lane claims) is visible in the
    // report rather than only showing up as an unexplained fallback rate.
    unknownPaths: policy.unknownPaths,
    laneRows,
    coverageGaps,
    selectedDurationMinutes,
    wallClockMinutes,
  };
}

export function aggregateBacktest(analyses, { minSHACount = DEFAULT_SHA_COUNT } = {}) {
  const nonDoc = analyses.filter((a) => !a.docsOnly);
  const docOnlyCount = analyses.length - nonDoc.length;
  const settled = nonDoc.filter((a) => a.wallClockMinutes !== null);
  const wallClock = settled.map((a) => a.wallClockMinutes).sort((a, b) => a - b);
  const runnerMinutes = settled
    .map((a) => a.selectedDurationMinutes)
    .sort((a, b) => a - b);
  const coverageGaps = nonDoc.flatMap((a) =>
    a.coverageGaps.map((gap) => ({
      pr: a.number,
      head_sha: a.headSHA,
      merged_at: a.mergedAt,
      ...gap,
    })),
  );

  const fallbackFullCount = nonDoc.filter(
    (a) => a.selectorConclusion === "fallback_full",
  ).length;
  // Registry glob-coverage diagnostic: which repository-relative directories
  // most often appear in an "unknown_paths" fallback. A path only lands here
  // when it matched NEITHER a conditional lane's package_globs NOR a
  // known_nonimpact_glob — i.e. no lane in the registry claims it, so the
  // selector fails closed. A directory dominating this list is a real
  // registry coverage gap, not a backtest artifact: it means impact selection
  // can never narrow the fence for that class of change.
  const unknownPathPrefixCounts = new Map();
  for (const analysis of nonDoc) {
    for (const path of analysis.unknownPaths ?? []) {
      const prefix = path.split("/").slice(0, 2).join("/");
      unknownPathPrefixCounts.set(prefix, (unknownPathPrefixCounts.get(prefix) ?? 0) + 1);
    }
  }
  const topUnknownPathPrefixes = [...unknownPathPrefixCounts.entries()]
    .sort(([, a], [, b]) => b - a)
    .slice(0, 10)
    .map(([prefix, count]) => ({ prefix, count }));

  const requiredReadyP50 = percentile(wallClock, 50);
  const requiredReadyP95 = percentile(wallClock, 95);
  const runnerP50 = percentile(runnerMinutes, 50);
  const runnerP95 = percentile(runnerMinutes, 95);

  const verdict = {
    sha_count_ok: nonDoc.length >= minSHACount,
    required_ready_p95_ok:
      requiredReadyP95 !== null && requiredReadyP95 <= MERGE_P95_SLO_MINUTES,
    runner_p50_ok: runnerP50 !== null && runnerP50 <= RUNNER_P50_TARGET_MINUTES,
    runner_p95_ok: runnerP95 !== null && runnerP95 <= RUNNER_P95_TARGET_MINUTES,
    zero_old_only_catches: coverageGaps.length === 0,
  };
  const pass = Object.values(verdict).every(Boolean);

  const mergedAtValues = nonDoc.map((a) => a.mergedAt).filter(Boolean).sort();
  const spanDays =
    mergedAtValues.length >= 2
      ? (Date.parse(mergedAtValues[mergedAtValues.length - 1]) -
          Date.parse(mergedAtValues[0])) /
        (24 * 60 * 60 * 1000)
      : 0;

  return {
    totalPRs: analyses.length,
    nonDocCount: nonDoc.length,
    docOnlyCount,
    settledCount: settled.length,
    unsettledCount: nonDoc.length - settled.length,
    spanDays,
    fallbackFullCount,
    topUnknownPathPrefixes,
    requiredReadyP50,
    requiredReadyP95,
    runnerP50,
    runnerP95,
    coverageGaps,
    verdict: { ...verdict, pass },
  };
}

function fmtMinutes(value) {
  return value === null ? "n/a" : value.toFixed(1);
}

export function renderSummary(report) {
  const lines = [];
  lines.push("# CI lane-selection backtest");
  lines.push("");
  lines.push(
    "Retrospective replay of the new merge-fence selector against real historical " +
      "pull-request CI data (see issue #2230's shadow-soak acceptance line). " +
      "The wall-clock \"required-ready\" figure below assumes every selected lane " +
      "could start immediately and run in parallel — an optimistic proxy for the " +
      "real merge-fence critical path, since it ignores runner-availability queuing.",
  );
  lines.push("");
  lines.push(
    `- Merged PRs walked: ${report.totalPRs} (${report.docOnlyCount} documentation-only, excluded from the gate)`,
  );
  lines.push(
    `- Non-documentation final SHAs analyzed: ${report.nonDocCount} (target >= ${DEFAULT_SHA_COUNT}), spanning ${report.spanDays.toFixed(1)} days`,
  );
  if (report.unsettledCount > 0) {
    lines.push(
      `- ${report.unsettledCount} non-documentation SHA(s) had no measurable selected-lane job timing and were excluded from the percentile stats`,
    );
  }
  lines.push(
    `- Selector fell back to the full lane set on ${report.fallbackFullCount}/${report.nonDocCount} SHAs (an unknown-path or empty-diff fallback selects every conditional lane, same as the old always-run system)`,
  );
  if (report.topUnknownPathPrefixes.length > 0) {
    lines.push("");
    lines.push(
      "Directories most often driving that fallback (no conditional lane's package_globs, and no known_nonimpact_glob, claims them — a registry glob-coverage gap, not a backtest artifact):",
    );
    for (const { prefix, count } of report.topUnknownPathPrefixes) {
      lines.push(`  - \`${prefix}\` — ${count} occurrence(s)`);
    }
  }
  lines.push("");
  lines.push("| Metric | p50 | p95 | Target | Status |");
  lines.push("| --- | --- | --- | --- | --- |");
  lines.push(
    `| Required-ready wall time (min) | ${fmtMinutes(report.requiredReadyP50)} | ${fmtMinutes(report.requiredReadyP95)} | p95 <= ${MERGE_P95_SLO_MINUTES} | ${report.verdict.required_ready_p95_ok ? "PASS" : "FAIL"} |`,
  );
  lines.push(
    `| Runner-minutes | ${fmtMinutes(report.runnerP50)} | ${fmtMinutes(report.runnerP95)} | p50 <= ${RUNNER_P50_TARGET_MINUTES}, p95 <= ${RUNNER_P95_TARGET_MINUTES} | ${report.verdict.runner_p50_ok && report.verdict.runner_p95_ok ? "PASS" : "FAIL"} |`,
  );
  lines.push("");
  lines.push(
    `Coverage (historical signatures caught / no old-only catches): ${report.verdict.zero_old_only_catches ? "PASS" : `FAIL — ${report.coverageGaps.length} gap(s)`}`,
  );
  if (report.coverageGaps.length > 0) {
    lines.push("");
    lines.push("| PR | head SHA | lane | omission reason | observed |");
    lines.push("| --- | --- | --- | --- | --- |");
    for (const gap of report.coverageGaps) {
      lines.push(
        `| #${gap.pr} | ${gap.head_sha.slice(0, 12)} | ${gap.lane_id} | ${gap.new_reason} | ${gap.observed_state}/${gap.observed_conclusion} |`,
      );
    }
  }
  lines.push("");
  lines.push(`Overall: ${report.verdict.pass ? "PASS" : "FAIL"}`);
  return lines.join("\n");
}

// --- network layer (thin; not unit-tested directly — see the .test.mjs for
// the injected-requestJSON coverage of the orchestration these call into) ---

function resolveRepository(env) {
  const fromEnv = String(env.GITHUB_REPOSITORY ?? "").trim();
  if (fromEnv !== "") return fromEnv;
  const remote = capture("git", ["remote", "get-url", "origin"]);
  if (remote.status !== 0) {
    contract(
      "GITHUB_REPOSITORY is unset and `git remote get-url origin` failed: " +
        remote.stderr.trim(),
    );
  }
  const url = remote.stdout.trim();
  // scp-like syntax ([user@]host:owner/repo(.git)?) — host may be a local SSH
  // config alias (e.g. `github.com-tsouza`), not literally `github.com`, so
  // this does not require or validate the host at all: the owner/repo pair is
  // always the last colon-delimited path segment.
  const scpLike = /^[^/]+:([^/]+\/[^/]+?)(?:\.git)?$/.exec(url);
  // scheme://[user@]host/owner/repo(.git)?
  const urlLike = /^[a-z][a-z0-9+.-]*:\/\/[^/]+\/([^/]+\/[^/]+?)(?:\.git)?$/.exec(url);
  const match = scpLike ?? urlLike;
  if (!match) {
    contract(
      `GITHUB_REPOSITORY is unset and the origin remote URL could not be parsed: ${url}`,
    );
  }
  return match[1];
}

function resolveToken(env) {
  const fromEnv = String(env.GITHUB_TOKEN ?? "").trim();
  if (fromEnv !== "") return fromEnv;
  const fromGH = capture("gh", ["auth", "token"]);
  if (fromGH.status !== 0 || fromGH.stdout.trim() === "") {
    contract(
      "GITHUB_TOKEN is unset and `gh auth token` failed; set GITHUB_TOKEN or run `gh auth login`",
    );
  }
  return fromGH.stdout.trim();
}

function githubRequest({ token, apiBase }) {
  return async (url) => {
    const response = await fetch(url, {
      headers: {
        Accept: "application/vnd.github+json",
        Authorization: `Bearer ${token}`,
        "X-GitHub-Api-Version": "2022-11-28",
      },
    });
    if (!response.ok) {
      const remaining = response.headers.get("x-ratelimit-remaining");
      contract(
        `GitHub API ${response.status} for ${new URL(url).pathname} (rate-limit remaining: ${remaining ?? "unknown"})`,
      );
    }
    return response.json();
  };
}

function pagedURL(apiBase, pathname, query, page, perPage = 100) {
  const url = new URL(pathname, `${apiBase.replace(/\/$/, "")}/`);
  for (const [name, value] of Object.entries(query)) {
    url.searchParams.set(name, String(value));
  }
  url.searchParams.set("per_page", String(perPage));
  url.searchParams.set("page", String(page));
  return url.toString();
}

// Paginate a GitHub REST list endpoint. `field` is null for the endpoints
// that return a bare JSON array (`/pulls`, `/pulls/{n}/files`); the Actions
// endpoints (`/actions/runs`, `/actions/runs/{id}/attempts/{n}/jobs`) instead
// wrap the array in `{ total_count, <field>: [...] }`.
async function paginatedArray({
  requestJSON,
  buildURL,
  field = null,
  perPage = 100,
  maxPages = 30,
}) {
  const rows = [];
  let truncated = false;
  for (let page = 1; page <= maxPages; page += 1) {
    const document = await requestJSON(buildURL(page));
    const batch = field === null ? document : document?.[field];
    if (!Array.isArray(batch)) {
      contract(`expected a JSON array response${field ? ` at field "${field}"` : ""}`);
    }
    rows.push(...batch);
    if (batch.length < perPage) return { rows, truncated };
  }
  truncated = true;
  return { rows, truncated };
}

async function fetchMergedPullRequests({ requestJSON, apiBase, repository, page }) {
  const url = pagedURL(
    apiBase,
    `/repos/${repository}/pulls`,
    { base: "main", state: "closed", sort: "updated", direction: "desc" },
    page,
  );
  const batch = await requestJSON(url);
  if (!Array.isArray(batch)) contract("expected a JSON array response for /pulls");
  return { merged: batch.filter((pr) => pr.merged_at != null), rawLength: batch.length };
}

async function fetchChangedPaths({ requestJSON, apiBase, repository, number }) {
  const { rows, truncated } = await paginatedArray({
    requestJSON,
    buildURL: (page) =>
      pagedURL(apiBase, `/repos/${repository}/pulls/${number}/files`, {}, page),
    maxPages: 30,
  });
  const paths = new Set();
  for (const entry of rows) {
    if (typeof entry.filename === "string") paths.add(entry.filename);
    if (typeof entry.previous_filename === "string") paths.add(entry.previous_filename);
  }
  return { changedPaths: [...paths].sort(), truncatedFiles: truncated };
}

async function fetchJobsByWorkflow({
  requestJSON,
  apiBase,
  repository,
  headSHA,
  workflowPaths,
}) {
  const { rows: candidateRuns } = await paginatedArray({
    requestJSON,
    field: "workflow_runs",
    buildURL: (page) =>
      pagedURL(
        apiBase,
        `/repos/${repository}/actions/runs`,
        { event: "pull_request", head_sha: headSHA },
        page,
      ),
    maxPages: 10,
  });
  const wanted = new Set(workflowPaths);
  const newestByWorkflow = new Map();
  for (const run of candidateRuns) {
    if (!wanted.has(run.path) || run.head_sha !== headSHA) continue;
    const previous = newestByWorkflow.get(run.path);
    if (!previous || run.run_attempt > previous.run_attempt) {
      newestByWorkflow.set(run.path, run);
    }
  }
  const jobsByWorkflow = new Map();
  for (const [workflow, run] of newestByWorkflow) {
    const { rows: jobs } = await paginatedArray({
      requestJSON,
      field: "jobs",
      buildURL: (page) =>
        pagedURL(
          apiBase,
          `/repos/${repository}/actions/runs/${run.id}/attempts/${run.run_attempt}/jobs`,
          { filter: "all" },
          page,
        ),
      maxPages: 10,
    });
    jobsByWorkflow.set(workflow, jobs);
  }
  return jobsByWorkflow;
}

export async function runBacktest({
  registry,
  requestJSON,
  apiBase,
  repository,
  shaCount,
  maxPRs,
}) {
  const workflowPaths = [...new Set(registry.lanes.map((lane) => lane.owner.workflow))];
  const analyses = [];
  let nonDocCount = 0;
  let apiPage = 1;
  let scanned = 0;
  for (;;) {
    if (nonDocCount >= shaCount || scanned >= maxPRs) break;
    const { merged, rawLength } = await fetchMergedPullRequests({
      requestJSON,
      apiBase,
      repository,
      page: apiPage,
    });
    apiPage += 1;
    for (const pr of merged) {
      if (scanned >= maxPRs || nonDocCount >= shaCount) break;
      scanned += 1;
      const { changedPaths, truncatedFiles } = await fetchChangedPaths({
        requestJSON,
        apiBase,
        repository,
        number: pr.number,
      });
      const jobsByWorkflow = await fetchJobsByWorkflow({
        requestJSON,
        apiBase,
        repository,
        headSHA: pr.head.sha,
        workflowPaths,
      });
      const analysis = analyzePullRequest({
        registry,
        number: pr.number,
        headSHA: pr.head.sha,
        baseSHA: pr.base.sha,
        mergeSHA: pr.merge_commit_sha,
        mergedAt: pr.merged_at,
        changedPaths,
        truncatedFiles,
        jobsByWorkflow,
      });
      analyses.push(analysis);
      if (!analysis.docsOnly) nonDocCount += 1;
    }
    if (rawLength < 100) break; // exhausted the repository's closed-PR list
  }
  return analyses;
}

async function main(env = process.env) {
  const root = resolve(process.cwd());
  const registry = loadRegistry(env.CI_LANE_REGISTRY || DEFAULT_REGISTRY_PATH, {
    root,
  });
  const repository = resolveRepository(env);
  const token = resolveToken(env);
  const apiBase = env.GITHUB_API_URL || DEFAULT_API_URL;
  const requestJSON = githubRequest({ token, apiBase });
  const shaCount = Number(env.CI_LANE_BACKTEST_SHA_COUNT || DEFAULT_SHA_COUNT);
  const maxPRs = Number(
    env.CI_LANE_BACKTEST_MAX_PRS || shaCount * DEFAULT_MAX_PRS_MULTIPLIER,
  );
  if (!Number.isSafeInteger(shaCount) || shaCount < 1) {
    contract("CI_LANE_BACKTEST_SHA_COUNT must be a positive integer");
  }
  if (!Number.isSafeInteger(maxPRs) || maxPRs < shaCount) {
    contract("CI_LANE_BACKTEST_MAX_PRS must be a positive integer >= the SHA count");
  }

  notice(
    `Backtesting the merge-fence selector against ${repository} — target ${shaCount} non-documentation final SHAs (cap ${maxPRs} PRs walked)`,
  );
  const analyses = await runBacktest({
    registry,
    requestJSON,
    apiBase,
    repository,
    shaCount,
    maxPRs,
  });
  const report = aggregateBacktest(analyses, { minSHACount: shaCount });
  const summary = renderSummary(report);

  const outputPath = resolve(root, env.CI_LANE_BACKTEST_OUTPUT || DEFAULT_OUTPUT_PATH);
  const parent = dirname(outputPath);
  if (!existsSync(parent)) mkdirSync(parent, { recursive: true });
  const document = { ...report, analyses, generated_at: new Date().toISOString() };
  const body = `${JSON.stringify(document, null, 2)}\n`;
  writeFileSync(outputPath, body, "utf8");
  notice(
    `Wrote ${outputPath} (sha256 ${createHash("sha256").update(body).digest("hex").slice(0, 12)})`,
  );

  process.stdout.write(`${summary}\n`);
  if (!report.verdict.sha_count_ok) {
    warning(
      `Only found ${report.nonDocCount} non-documentation final SHAs (target ${shaCount}); ` +
        "widen CI_LANE_BACKTEST_MAX_PRS or accept a smaller sample",
    );
  }
  if (report.verdict.pass) {
    notice("CI lane-selection backtest: PASS against every issue #2230 acceptance target");
  } else {
    error(
      `CI lane-selection backtest: FAIL — ${JSON.stringify(report.verdict)}`,
      { title: "merge-fence backtest did not clear the shadow-soak bar" },
    );
    process.exitCode = 1;
  }
}

const invokedDirectly =
  process.argv[1] && fileURLToPath(import.meta.url) === resolve(process.argv[1]);
if (invokedDirectly) {
  try {
    await main();
  } catch (backtestError) {
    error(
      backtestError instanceof Error ? backtestError.message : String(backtestError),
      { title: "merge-fence backtest failed to run" },
    );
    process.exit(1);
  }
}
